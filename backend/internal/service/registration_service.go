package service

import (
	"encoding/csv"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"gbevent/internal/constants"
	"gbevent/internal/model"
	"gbevent/internal/repository"
	"gbevent/internal/util"

	"gorm.io/gorm"
)

// RegistrationService 报名业务逻辑。
type RegistrationService struct {
	db          *gorm.DB
	repo        *repository.RegistrationRepository
	activitySvc *ActivityService
	notifyRepo  *repository.NotificationRepository
	logger      *slog.Logger
}

// NewRegistrationService 构造报名服务。
func NewRegistrationService(db *gorm.DB, repo *repository.RegistrationRepository, activitySvc *ActivityService,
	notifyRepo *repository.NotificationRepository, logger *slog.Logger) *RegistrationService {
	return &RegistrationService{db: db, repo: repo, activitySvc: activitySvc, notifyRepo: notifyRepo, logger: logger}
}

// Create 在线报名：名额未满则成为正式报名；名额已满则按提交先后进入候补队列。
// 活动行加锁，保证并发提交下名额不被重复占用、候补顺序与提交先后一致。
func (s *RegistrationService) Create(activityID, userID uint64, name, phone, remark string) (*model.Registration, error) {
	s.logger.Info(constants.LogRegistrationCreateStart, "activity_id", activityID, "user_id", userID)
	reg := &model.Registration{}
	waitlisted := false
	err := s.db.Transaction(func(tx *gorm.DB) error {
		act, err := s.activitySvc.CheckSignupEligibilityTx(tx, activityID)
		if err != nil {
			return err
		}
		if _, err := s.repo.FindByActivityAndUserTx(tx, activityID, userID); err == nil {
			return util.NewAppError(constants.CodeDuplicateSignup, constants.MsgDuplicateSignup)
		} else if !errNotFound(err) {
			return err
		}
		occupied, err := s.activitySvc.CountRegisteredTx(tx, activityID)
		if err != nil {
			return err
		}
		reg.ActivityID = activityID
		reg.UserID = userID
		reg.Name = name
		reg.Phone = phone
		reg.Remark = remark
		reg.VoucherNo = util.GenerateVoucherNo()
		if act.Capacity > 0 && occupied >= int64(act.Capacity) {
			// 名额已满：进入候补队列，不占名额
			waitlisted = true
			reg.Status = constants.RegistrationStatusWaitlisted
			reg.ReviewStatus = constants.ReviewStatusPending
			if err := s.repo.CreateTx(tx, reg); err != nil {
				s.logger.Error(constants.LogRegistrationCreateFailed, "error", err)
				return util.Wrap(err, "Registration[activity_id=%d,user_id=%d] waitlist create failed", activityID, userID)
			}
			if err := s.activitySvc.CreateSignupNotificationTx(tx, userID, constants.NotificationSignupSuccess,
				"已进入候补队列", "活动名额已满，您已进入候补队列，有人退出时将按顺序自动转正并通知您"); err != nil {
				return err
			}
			return nil
		}
		reg.Status = constants.RegistrationStatusRegistered
		reg.ReviewStatus = constants.ReviewStatusPending
		if err := s.repo.CreateTx(tx, reg); err != nil {
			s.logger.Error(constants.LogRegistrationCreateFailed, "error", err)
			return util.Wrap(err, "Registration[activity_id=%d,user_id=%d] create failed", activityID, userID)
		}
		if err := s.activitySvc.CreateSignupNotificationTx(tx, userID, constants.NotificationSignupSuccess,
			"报名成功", "您已成功报名活动，凭证号："+reg.VoucherNo); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if waitlisted {
		s.logger.Info(constants.LogRegistrationWaitlistJoin, "registration_id", reg.ID, "activity_id", activityID)
	} else {
		s.logger.Info(constants.LogRegistrationCreateSuccess, "registration_id", reg.ID, "voucher_no", reg.VoucherNo)
	}
	return reg, nil
}

// Cancel 取消报名。
// 正式报名（registered/checked_in）取消后在同一事务内释放名额，
// 并将候补队列中最早的报名转为正式报名（顺延），同时通知本人；候补报名取消仅退出队列。
func (s *RegistrationService) Cancel(id, operatorID uint64, operatorRole string) (*model.Registration, error) {
	var reg *model.Registration
	err := s.db.Transaction(func(tx *gorm.DB) error {
		cur, err := s.repo.FindByIDForUpdate(tx, id)
		if err != nil {
			return util.Wrap(err, "Registration[id=%d] cancel find failed", id)
		}
		if operatorRole != constants.RoleAdmin && cur.UserID != operatorID {
			return util.NewAppError(constants.CodeForbidden, "Registration[id="+itoa(id)+"] cancel forbidden: not owner")
		}
		switch cur.Status {
		case constants.RegistrationStatusRegistered, constants.RegistrationStatusCheckedIn:
			// 占用名额：取消并释放一个名额，顺延给最早候补者
			cur.Status = constants.RegistrationStatusCancelled
			if err := s.repo.UpdateTx(tx, cur); err != nil {
				return util.Wrap(err, "Registration[id=%d] cancel save failed", id)
			}
			if err := s.promoteEarliestWaitlistTx(tx, cur.ActivityID); err != nil {
				return err
			}
		case constants.RegistrationStatusWaitlisted:
			// 候补退出：不占名额，不触发顺延
			cur.Status = constants.RegistrationStatusCancelled
			if err := s.repo.UpdateTx(tx, cur); err != nil {
				return util.Wrap(err, "Registration[id=%d] waitlist cancel save failed", id)
			}
		default:
			return util.NewAppError(constants.CodeCancelConflict, constants.MsgCancelConflict)
		}
		reg = cur
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.logger.Info(constants.LogRegistrationCancelSuccess, "registration_id", reg.ID)
	return reg, nil
}

// promoteEarliestWaitlistTx 在同一事务内将候补队列中最早的报名转为正式报名并通知本人。
// 转正前再次校验名额未满，保证释放的名额与转正占用的名额等量、同一名额不被重复占用。
func (s *RegistrationService) promoteEarliestWaitlistTx(tx *gorm.DB, activityID uint64) error {
	act, err := s.activitySvc.FindActivityForUpdateTx(tx, activityID)
	if err != nil {
		return err
	}
	candidate, err := s.repo.FindEarliestWaitlistedForUpdateTx(tx, activityID)
	if err != nil {
		if errNotFound(err) {
			return nil
		}
		return util.Wrap(err, "Activity[id=%d] promote waitlist find failed", activityID)
	}
	occupied, err := s.activitySvc.CountRegisteredTx(tx, activityID)
	if err != nil {
		return err
	}
	if act.Capacity > 0 && occupied >= int64(act.Capacity) {
		// 名额仍满（理论上不会发生：取消恰好释放一个名额），放弃本次顺延
		s.logger.Warn(constants.LogRegistrationWaitlistPromoteFailed,
			"activity_id", activityID, "occupied", occupied, "capacity", act.Capacity)
		return nil
	}
	candidate.Status = constants.RegistrationStatusRegistered
	candidate.ReviewStatus = constants.ReviewStatusPending
	if err := s.repo.UpdateTx(tx, candidate); err != nil {
		return util.Wrap(err, "Registration[id=%d] waitlist promote save failed", candidate.ID)
	}
	if err := s.activitySvc.CreateSignupNotificationTx(tx, candidate.UserID, constants.NotificationSignupSuccess,
		"候补名额已转正", "您候补的活动已有名额释放，您已自动转为正式报名，请等待审核。凭证号："+candidate.VoucherNo); err != nil {
		return err
	}
	s.logger.Info(constants.LogRegistrationWaitlistPromote, "registration_id", candidate.ID, "activity_id", activityID)
	return nil
}

// Review 审核报名（pending -> approved/rejected）。
// 驳回占用名额的正式报名时，在同一事务内释放名额并把最早候补者转为正式报名；候补报名尚未占名额，驳回不顺延。
func (s *RegistrationService) Review(id, operatorID uint64, operatorRole string, reviewStatus string) (*model.Registration, error) {
	var reg *model.Registration
	err := s.db.Transaction(func(tx *gorm.DB) error {
		cur, err := s.repo.FindByIDForUpdate(tx, id)
		if err != nil {
			return util.Wrap(err, "Registration[id=%d] review find failed", id)
		}
		act, err := s.activitySvc.FindActivityForUpdateTx(tx, cur.ActivityID)
		if err != nil {
			return err
		}
		if !IsOrganizer(operatorID, operatorRole, act.OrganizerID) {
			return util.NewAppError(constants.CodeForbidden, "Registration[id="+itoa(id)+"] review forbidden: organizer not match")
		}
		if cur.ReviewStatus != constants.ReviewStatusPending {
			return util.NewAppError(constants.CodeReviewConflict, constants.MsgReviewConflict)
		}
		if cur.Status != constants.RegistrationStatusRegistered && cur.Status != constants.RegistrationStatusWaitlisted {
			return util.NewAppError(constants.CodeReviewConflict, "Registration[id="+itoa(id)+"] review conflict: status="+cur.Status)
		}
		if !constants.IsValidReviewStatus(reviewStatus) {
			return util.NewAppError(constants.CodeValidationFailed, "Registration[id="+itoa(id)+"] review invalid status="+reviewStatus)
		}
		if cur.Status == constants.RegistrationStatusWaitlisted && reviewStatus == constants.ReviewStatusApproved {
			// 候补尚未占名额，转正前不允许审核通过；转正后会重新进入待审核
			return util.NewAppError(constants.CodeWaitlistConflict, "Registration[id="+itoa(id)+"] approve conflict: registration still on waitlist")
		}
		// 驳回前记录该报名是否占用名额：仅正式报名被驳回才释放名额并顺延，候补驳回不顺延。
		wasOccupied := constants.IsOccupiedRegistrationStatus(cur.Status)
		cur.ReviewStatus = reviewStatus
		title := "审核结果"
		content := "您的报名审核已" + util.ReviewStatusText(reviewStatus)
		if reviewStatus == constants.ReviewStatusApproved {
			content = "您的报名已通过审核，凭证号：" + cur.VoucherNo
		}
		if reviewStatus == constants.ReviewStatusRejected {
			cur.Status = constants.RegistrationStatusCancelled
		}
		if err := s.repo.UpdateTx(tx, cur); err != nil {
			s.logger.Error(constants.LogRegistrationReviewFailed, "error", err)
			return util.Wrap(err, "Registration[id=%d] review save failed", id)
		}
		if err := s.activitySvc.CreateSignupNotificationTx(tx, cur.UserID, constants.NotificationReviewResult, title, content); err != nil {
			return err
		}
		if reviewStatus == constants.ReviewStatusRejected && wasOccupied {
			// 释放的名额与转正占用的名额等量：恰好顺延一名候补者
			if err := s.promoteEarliestWaitlistTx(tx, cur.ActivityID); err != nil {
				return err
			}
		}
		reg = cur
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.logger.Info(constants.LogRegistrationReviewSuccess, "registration_id", reg.ID, "review_status", reviewStatus)
	return reg, nil
}

// OfflineCreate 线下补录报名（活动行加锁，避免与在线报名/候补转正竞争同一名额）。
func (s *RegistrationService) OfflineCreate(activityID, operatorID uint64, name, phone, remark string) (*model.Registration, error) {
	reg := &model.Registration{}
	err := s.db.Transaction(func(tx *gorm.DB) error {
		if err := s.activitySvc.CheckRegistrationLimitTx(tx, activityID); err != nil {
			return err
		}
		reg.ActivityID = activityID
		reg.UserID = operatorID
		reg.Name = name
		reg.Phone = phone
		reg.Remark = remark
		reg.VoucherNo = util.GenerateVoucherNo()
		reg.Status = constants.RegistrationStatusRegistered
		reg.ReviewStatus = constants.ReviewStatusApproved
		if err := s.repo.CreateTx(tx, reg); err != nil {
			return util.Wrap(err, "Registration[activity_id=%d] offline create failed", activityID)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.logger.Info(constants.LogRegistrationCreateSuccess, "registration_id", reg.ID, "source", "offline")
	return reg, nil
}

// registrationResponse 报名响应：附带候补位次，供个人中心区分候补/正式。
type registrationResponse struct {
	model.Registration
	WaitlistPosition int `json:"waitlist_position"`
}

// toResponse 组装单个报名响应，候补报名附带其在队列中的位次。
func (s *RegistrationService) toResponse(r model.Registration) registrationResponse {
	pos := 0
	if r.Status == constants.RegistrationStatusWaitlisted {
		if p, err := s.repo.WaitlistPosition(&r); err == nil {
			pos = p
		}
	}
	return registrationResponse{Registration: r, WaitlistPosition: pos}
}

func (s *RegistrationService) toResponseList(list []model.Registration) []registrationResponse {
	out := make([]registrationResponse, 0, len(list))
	for _, r := range list {
		out = append(out, s.toResponse(r))
	}
	return out
}

// List 分页查询报名（组织者）。
func (s *RegistrationService) List(page, pageSize int, activityID uint64, status, reviewStatus string) ([]registrationResponse, int64, error) {
	list, total, err := s.repo.List(page, pageSize, activityID, status, reviewStatus)
	if err != nil {
		return nil, 0, err
	}
	return s.toResponseList(list), total, nil
}

// ListMine 查询我的报名。
func (s *RegistrationService) ListMine(userID uint64, page, pageSize int) ([]registrationResponse, int64, error) {
	list, total, err := s.repo.ListByUser(userID, page, pageSize)
	if err != nil {
		return nil, 0, err
	}
	return s.toResponseList(list), total, nil
}

// Get 查询报名详情。
func (s *RegistrationService) Get(id uint64) (*model.Registration, error) {
	return s.repo.FindByID(id)
}

// ExportCSV 导出报名名单 CSV。
func (s *RegistrationService) ExportCSV(activityID, operatorID uint64, operatorRole string) (string, string, error) {
	a, _, err := s.activitySvc.Get(activityID)
	if err != nil {
		return "", "", err
	}
	if !IsOrganizer(operatorID, operatorRole, a.OrganizerID) {
		return "", "", util.NewAppError(constants.CodeForbidden, "Registration[activity_id="+itoa(activityID)+"] export forbidden: organizer not match")
	}
	list, err := s.repo.ListByActivity(activityID)
	if err != nil {
		return "", "", err
	}
	var buf strings.Builder
	w := csv.NewWriter(&buf)
	_ = w.Write([]string{"ID", "活动ID", "报名人", "手机号", "凭证号", "状态", "审核状态", "备注", "报名时间"})
	for _, r := range list {
		_ = w.Write([]string{
			strconv.FormatUint(r.ID, 10),
			strconv.FormatUint(r.ActivityID, 10),
			r.Name, r.Phone, r.VoucherNo,
			util.RegistrationStatusText(r.Status),
			util.ReviewStatusText(r.ReviewStatus),
			r.Remark,
			r.CreatedAt.Format("2006-01-02 15:04:05"),
		})
	}
	w.Flush()
	filename := "registrations_" + strconv.FormatUint(activityID, 10) + "_" + time.Now().Format("20060102150405") + ".csv"
	return filename, buf.String(), nil
}
