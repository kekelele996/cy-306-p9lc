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

// Create 在线报名：有名额直接占坑，名额已满则进入候补队列（按提交先后排队）。
func (s *RegistrationService) Create(activityID, userID uint64, name, phone, remark string) (*model.Registration, error) {
	s.logger.Info(constants.LogRegistrationCreateStart, "activity_id", activityID, "user_id", userID)
	reg := &model.Registration{}
	err := s.db.Transaction(func(tx *gorm.DB) error {
		// 锁定活动行作为名额串行点，保证并发提交下同一名额不会被重复占用。
		act, occupied, err := s.activitySvc.LockActivityForSignupTx(tx, activityID)
		if err != nil {
			return err
		}
		if _, err := s.repo.FindByActivityAndUserTx(tx, activityID, userID); err == nil {
			return util.NewAppError(constants.CodeDuplicateSignup, constants.MsgDuplicateSignup)
		} else if !errNotFound(err) {
			return err
		}
		reg.ActivityID = activityID
		reg.UserID = userID
		reg.Name = name
		reg.Phone = phone
		reg.Remark = remark
		reg.VoucherNo = util.GenerateVoucherNo()
		reg.ReviewStatus = constants.ReviewStatusPending
		if HasFreeCapacity(act, occupied) {
			reg.Status = constants.RegistrationStatusRegistered
		} else {
			reg.Status = constants.RegistrationStatusWaitlisted
		}
		if err := s.repo.CreateTx(tx, reg); err != nil {
			s.logger.Error(constants.LogRegistrationCreateFailed, "error", err)
			return util.Wrap(err, "Registration[activity_id=%d,user_id=%d] create failed", activityID, userID)
		}
		if reg.Status == constants.RegistrationStatusWaitlisted {
			if err := s.activitySvc.CreateSignupNotificationTx(tx, userID, constants.NotificationWaitlistJoined,
				"已进入候补队列", "活动名额已满，您已进入候补队列，有名额释放时将按提交顺序自动转正并通知您"); err != nil {
				return err
			}
			s.logger.Info(constants.LogRegistrationWaitlistJoin, "activity_id", activityID, "user_id", userID, "registration_id", reg.ID)
		} else {
			if err := s.activitySvc.CreateSignupNotificationTx(tx, userID, constants.NotificationSignupSuccess,
				"报名成功", "您已成功报名活动，凭证号："+reg.VoucherNo); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.logger.Info(constants.LogRegistrationCreateSuccess, "registration_id", reg.ID, "voucher_no", reg.VoucherNo, "status", reg.Status)
	return reg, nil
}

// Cancel 取消报名：正式报名取消后在同一事务内把最早候补者转正；候补取消仅退出候补队列。
func (s *RegistrationService) Cancel(id, operatorID uint64, operatorRole string) (*model.Registration, error) {
	reg := &model.Registration{}
	err := s.db.Transaction(func(tx *gorm.DB) error {
		cur, err := s.repo.FindByID(id)
		if err != nil {
			return util.Wrap(err, "Registration[id=%d] cancel find failed", id)
		}
		if operatorRole != constants.RoleAdmin && cur.UserID != operatorID {
			return util.NewAppError(constants.CodeForbidden, "Registration[id="+itoa(id)+"] cancel forbidden: not owner")
		}
		// 先锁活动行再锁报名行，与报名/审核事务保持相同加锁顺序，避免并发死锁。
		act, err := s.activitySvc.LockActivityForUpdateTx(tx, cur.ActivityID)
		if err != nil {
			return util.Wrap(err, "Registration[id=%d] cancel lock activity failed", id)
		}
		cur, err = s.repo.FindByIDForUpdate(tx, id)
		if err != nil {
			return util.Wrap(err, "Registration[id=%d] cancel lock failed", id)
		}
		if cur.Status != constants.RegistrationStatusRegistered && cur.Status != constants.RegistrationStatusWaitlisted {
			return util.NewAppError(constants.CodeCancelConflict, constants.MsgCancelConflict)
		}
		releasedSlot := cur.Status == constants.RegistrationStatusRegistered
		cur.Status = constants.RegistrationStatusCancelled
		if err := s.repo.UpdateTx(tx, cur); err != nil {
			return util.Wrap(err, "Registration[id=%d] cancel save failed", id)
		}
		// 释放一个正式名额：驳回/取消与转正严格等量，本事务最多转正一人。
		if releasedSlot {
			if err := s.promoteEarliestWaitlistedTx(tx, act); err != nil {
				return err
			}
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

// promoteEarliestWaitlistedTx 在同一事务内把活动最早的候补者转为正式报名并通知本人。
// 调用方必须已持有该活动行的 FOR UPDATE 锁；无候补或名额未真正释放时不产生任何变更。
func (s *RegistrationService) promoteEarliestWaitlistedTx(tx *gorm.DB, act *model.Activity) error {
	occupied, err := s.activitySvc.CountOccupiedTx(tx, act.ID)
	if err != nil {
		return err
	}
	if !HasFreeCapacity(act, occupied) {
		return nil
	}
	next, err := s.repo.FindEarliestWaitlistedTx(tx, act.ID)
	if err != nil {
		if errNotFound(err) {
			return nil
		}
		return err
	}
	next.Status = constants.RegistrationStatusRegistered
	if err := s.repo.UpdateTx(tx, next); err != nil {
		return util.Wrap(err, "Registration[id=%d] waitlist promote save failed", next.ID)
	}
	if err := s.activitySvc.CreateSignupNotificationTx(tx, next.UserID, constants.NotificationWaitlistPromoted,
		constants.MsgWaitlistPromoted, "有名额释放，您已按候补顺序自动转为正式报名，凭证号："+next.VoucherNo); err != nil {
		return err
	}
	s.logger.Info(constants.LogRegistrationWaitlistPromote, "activity_id", act.ID, "registration_id", next.ID, "user_id", next.UserID)
	return nil
}

// Review 审核报名（pending -> approved/rejected）。
// 驳回正式报名会释放名额，并在同一事务内把最早候补者转正；驳回候补报名仅退出候补，不涉及名额。
func (s *RegistrationService) Review(id, operatorID uint64, operatorRole string, reviewStatus string) (*model.Registration, error) {
	var reg *model.Registration
	err := s.db.Transaction(func(tx *gorm.DB) error {
		cur, err := s.repo.FindByID(id)
		if err != nil {
			return util.Wrap(err, "Registration[id=%d] review find failed", id)
		}
		// 先锁活动行，再锁报名行，与取消/报名事务保持一致的加锁顺序。
		act, err := s.activitySvc.LockActivityForUpdateTx(tx, cur.ActivityID)
		if err != nil {
			return util.Wrap(err, "Registration[id=%d] review lock activity failed", id)
		}
		cur, err = s.repo.FindByIDForUpdate(tx, id)
		if err != nil {
			return util.Wrap(err, "Registration[id=%d] review lock failed", id)
		}
		if !IsOrganizer(operatorID, operatorRole, act.OrganizerID) {
			return util.NewAppError(constants.CodeForbidden, "Registration[id="+itoa(id)+"] review forbidden: organizer not match")
		}
		if cur.ReviewStatus != constants.ReviewStatusPending {
			return util.NewAppError(constants.CodeReviewConflict, constants.MsgReviewConflict)
		}
		if !constants.IsValidReviewStatus(reviewStatus) {
			return util.NewAppError(constants.CodeValidationFailed, "Registration[id="+itoa(id)+"] review invalid status="+reviewStatus)
		}
		// 驳回：审核态置 rejected；
		// - registered：报名态置 cancelled 释放名额；
		// - checked_in：保留签到记录（status 维持 checked_in），但 rejected 不再计入占用，名额同样释放；
		// - waitlisted：置 cancelled 退出候补队列，不涉及名额。
		// 凡释放一个名额都在本事务内等量转正一人。
		releasedSlot := false
		if reviewStatus == constants.ReviewStatusRejected {
			if cur.Status == constants.RegistrationStatusRegistered || cur.Status == constants.RegistrationStatusCheckedIn {
				releasedSlot = true
			}
			if cur.Status == constants.RegistrationStatusRegistered || cur.Status == constants.RegistrationStatusWaitlisted {
				cur.Status = constants.RegistrationStatusCancelled
			}
		}
		cur.ReviewStatus = reviewStatus
		if err := s.repo.UpdateTx(tx, cur); err != nil {
			s.logger.Error(constants.LogRegistrationReviewFailed, "error", err)
			return util.Wrap(err, "Registration[id=%d] review save failed", id)
		}
		if releasedSlot {
			if err := s.promoteEarliestWaitlistedTx(tx, act); err != nil {
				return err
			}
		}
		title := "审核结果"
		content := "您的报名审核已" + util.ReviewStatusText(reviewStatus)
		if reviewStatus == constants.ReviewStatusApproved {
			content = "您的报名已通过审核，凭证号：" + cur.VoucherNo
		}
		if err := s.activitySvc.CreateSignupNotificationTx(tx, cur.UserID, constants.NotificationReviewResult, title, content); err != nil {
			return err
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

// OfflineCreate 线下补录报名：有名额才补录，名额已满直接报错，不进入候补队列。
func (s *RegistrationService) OfflineCreate(activityID, operatorID uint64, name, phone, remark string) (*model.Registration, error) {
	reg := &model.Registration{}
	err := s.db.Transaction(func(tx *gorm.DB) error {
		act, occupied, err := s.activitySvc.LockActivityForSignupTx(tx, activityID)
		if err != nil {
			return err
		}
		if !HasFreeCapacity(act, occupied) {
			return util.NewAppError(constants.CodeActivityFull, constants.MsgActivityFull)
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

// List 分页查询报名（组织者）。
func (s *RegistrationService) List(page, pageSize int, activityID uint64, status, reviewStatus string) ([]model.Registration, int64, error) {
	return s.repo.List(page, pageSize, activityID, status, reviewStatus)
}

// ListMine 查询我的报名。
func (s *RegistrationService) ListMine(userID uint64, page, pageSize int) ([]model.Registration, int64, error) {
	return s.repo.ListByUser(userID, page, pageSize)
}

// Get 查询报名详情。
func (s *RegistrationService) Get(id uint64) (*model.Registration, error) {
	return s.repo.FindByID(id)
}

// ExportCSV 导出报名名单 CSV。
func (s *RegistrationService) ExportCSV(activityID, operatorID uint64, operatorRole string) (string, string, error) {
	a, _, _, err := s.activitySvc.Get(activityID)
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
