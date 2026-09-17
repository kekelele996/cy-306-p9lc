package service

import (
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"gbevent/internal/constants"
	"gbevent/internal/model"
	"gbevent/internal/repository"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newWaitlistTestDB 构建内存外的临时 SQLite 库（共享缓存，支持 FOR UPDATE 被忽略，靠活动行锁语义足够验证逻辑）。
func newWaitlistTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "waitlist.db")), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(
		&model.User{}, &model.Activity{}, &model.Registration{},
		&model.Notification{}, &model.CheckInRecord{}, &model.Comment{},
		&model.Favorite{}, &model.AuditLog{},
	); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func setupWaitlistSvc(t *testing.T, capacity int) (*gorm.DB, *RegistrationService) {
	db := newWaitlistTestDB(t)
	logger := testLogger()
	actRepo := repository.NewActivityRepository(db)
	regRepo := repository.NewRegistrationRepository(db)
	notifyRepo := repository.NewNotificationRepository(db)
	checkinRepo := repository.NewCheckInRecordRepository(db)
	activitySvc := NewActivityService(db, actRepo, regRepo, notifyRepo, checkinRepo, logger)
	regSvc := NewRegistrationService(db, regRepo, activitySvc, notifyRepo, logger)

	organizer := &model.User{ID: 1, Username: "org", Role: constants.RoleOrganizer}
	db.Create(organizer)
	deadline := time.Now().Add(24 * time.Hour)
	act := &model.Activity{
		ID: 1, Title: "cap-act", ActivityType: constants.ActivityTypeLecture,
		StartTime: deadline, EndTime: deadline.Add(time.Hour), SignupDeadline: deadline,
		Capacity: capacity, Status: constants.ActivityStatusPublished, OrganizerID: 1,
	}
	db.Create(act)
	// 普通用户 10..39
	for i := uint64(10); i < 40; i++ {
		db.Create(&model.User{ID: i, Username: "u" + itoa(i), Role: constants.RoleUser})
	}
	return db, regSvc
}

// TestWaitlistFillsThenQueues 名额满后进入候补，且按提交先后排队。
func TestWaitlistFillsThenQueues(t *testing.T) {
	db, svc := setupWaitlistSvc(t, 2)

	// 前 2 人正式报名
	for _, uid := range []uint64{10, 11} {
		reg, err := svc.Create(1, uid, "n", "p", "")
		if err != nil {
			t.Fatalf("user %d signup: %v", uid, err)
		}
		if reg.Status != constants.RegistrationStatusRegistered {
			t.Fatalf("user %d want registered, got %s", uid, reg.Status)
		}
	}
	// 第 3、4 人进入候补，顺序 3->1, 4->2
	for _, uid := range []uint64{12, 13} {
		reg, err := svc.Create(1, uid, "n", "p", "")
		if err != nil {
			t.Fatalf("user %d waitlist: %v", uid, err)
		}
		if reg.Status != constants.RegistrationStatusWaitlisted {
			t.Fatalf("user %d want waitlisted, got %s", uid, reg.Status)
		}
	}
	// 校验候补位次
	w1 := mustFindReg(t, db, 1, 12)
	if pos, err := svc.repo.WaitlistPosition(w1); err != nil || pos != 1 {
		t.Fatalf("user 12 waitlist position = %d, err=%v, want 1", pos, err)
	}
	w2 := mustFindReg(t, db, 1, 13)
	if pos, _ := svc.repo.WaitlistPosition(w2); pos != 2 {
		t.Fatalf("user 13 waitlist position = %d, want 2", pos)
	}
	assertCounts(t, db, 1, 2, 2)
}

// TestCancelPromotesEarliest 取消正式报名后，最早候补者在同一事务中转正。
func TestCancelPromotesEarliest(t *testing.T) {
	db, svc := setupWaitlistSvc(t, 2)
	regs := make([]*model.Registration, 0, 4)
	for _, uid := range []uint64{10, 11, 12, 13} {
		r, err := svc.Create(1, uid, "n", "p", "")
		if err != nil {
			t.Fatal(err)
		}
		regs = append(regs, r)
	}

	if _, err := svc.Cancel(regs[0].ID, 10, constants.RoleUser); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	// 最早候补 user 12 转正
	if got := mustFindReg(t, db, 1, 12); got.Status != constants.RegistrationStatusRegistered {
		t.Fatalf("user 12 after cancel = %s, want registered", got.Status)
	}
	// 第二位候补仍是候补，且现居第 1
	w := mustFindReg(t, db, 1, 13)
	if w.Status != constants.RegistrationStatusWaitlisted {
		t.Fatalf("user 13 = %s, want waitlisted", w.Status)
	}
	if pos, _ := svc.repo.WaitlistPosition(w); pos != 1 {
		t.Fatalf("user 13 position = %d, want 1", pos)
	}
	assertCounts(t, db, 1, 2, 1)

	// 转正者收到通知
	var notifCount int64
	db.Model(&model.Notification{}).Where("user_id = ? AND title = ?", 12, "候补名额已转正").Count(&notifCount)
	if notifCount != 1 {
		t.Fatalf("promote notifications = %d, want 1", notifCount)
	}
}

// TestPromotedThenCancelRollsToNext 转正者再取消，顺延给下一位候补。
func TestPromotedThenCancelRollsToNext(t *testing.T) {
	db, svc := setupWaitlistSvc(t, 1)
	for _, uid := range []uint64{10, 11, 12} {
		if _, err := svc.Create(1, uid, "n", "p", ""); err != nil {
			t.Fatal(err)
		}
	}
	// user10 取消 -> user11 转正
	first := mustFindReg(t, db, 1, 10)
	if _, err := svc.Cancel(first.ID, 10, constants.RoleUser); err != nil {
		t.Fatalf("cancel first: %v", err)
	}
	if got := mustFindReg(t, db, 1, 11); got.Status != constants.RegistrationStatusRegistered {
		t.Fatalf("user 11 = %s, want registered", got.Status)
	}
	// 转正的 user11 再取消 -> user12 转正
	p := mustFindReg(t, db, 1, 11)
	if _, err := svc.Cancel(p.ID, 11, constants.RoleUser); err != nil {
		t.Fatalf("cancel promoted: %v", err)
	}
	if got := mustFindReg(t, db, 1, 12); got.Status != constants.RegistrationStatusRegistered {
		t.Fatalf("user 12 after second cancel = %s, want registered", got.Status)
	}
	assertCounts(t, db, 1, 1, 0)
}

// TestRejectFreesAndPromotes 驳回正式报名释放一个名额并转正一名候补；驳回候补不顺延。
func TestRejectFreesAndPromotes(t *testing.T) {
	db, svc := setupWaitlistSvc(t, 2)
	var r10, r13 *model.Registration
	for _, uid := range []uint64{10, 11, 12, 13} {
		r, err := svc.Create(1, uid, "n", "p", "")
		if err != nil {
			t.Fatal(err)
		}
		if uid == 10 {
			r10 = r
		}
		if uid == 13 {
			r13 = r
		}
	}

	// 驳回正式报名 user10 -> user12 转正
	if _, err := svc.Review(r10.ID, 1, constants.RoleOrganizer, constants.ReviewStatusRejected); err != nil {
		t.Fatalf("reject r10: %v", err)
	}
	if got := mustFindReg(t, db, 1, 10); got.Status != constants.RegistrationStatusCancelled || got.ReviewStatus != constants.ReviewStatusRejected {
		t.Fatalf("user 10 = %s/%s, want cancelled/rejected", got.Status, got.ReviewStatus)
	}
	if got := mustFindReg(t, db, 1, 12); got.Status != constants.RegistrationStatusRegistered {
		t.Fatalf("user 12 = %s, want registered", got.Status)
	}
	assertCounts(t, db, 1, 2, 1)

	// 驳回候补 user13：退出候补但不顺延（user11 正式 + user12 转正，仍满 2）
	if _, err := svc.Review(r13.ID, 1, constants.RoleOrganizer, constants.ReviewStatusRejected); err != nil {
		t.Fatalf("reject waitlist r13: %v", err)
	}
	if got := mustFindReg(t, db, 1, 13); got.Status != constants.RegistrationStatusCancelled {
		t.Fatalf("user 13 = %s, want cancelled", got.Status)
	}
	if got := mustFindReg(t, db, 1, 11); got.Status != constants.RegistrationStatusRegistered {
		t.Fatalf("user 11 = %s, want still registered", got.Status)
	}
	assertCounts(t, db, 1, 2, 0)
}

// TestWaitlistCancelDoesNotPromote 候补主动退出不顺延、不占名额。
func TestWaitlistCancelDoesNotPromote(t *testing.T) {
	db, svc := setupWaitlistSvc(t, 1)
	var w2 *model.Registration
	for _, uid := range []uint64{10, 11, 12} {
		r, err := svc.Create(1, uid, "n", "p", "")
		if err != nil {
			t.Fatal(err)
		}
		if uid == 12 {
			w2 = r
		}
	}
	if _, err := svc.Cancel(w2.ID, 12, constants.RoleUser); err != nil {
		t.Fatal(err)
	}
	if got := mustFindReg(t, db, 1, 11); got.Status != constants.RegistrationStatusWaitlisted {
		t.Fatalf("user 11 = %s, want still waitlisted", got.Status)
	}
	assertCounts(t, db, 1, 1, 1)
}

// TestConcurrentSignupNoOverbook 并发提交下正式报名数严格等于名额，其余全部进入候补。
func TestConcurrentSignupNoOverbook(t *testing.T) {
	db, svc := setupWaitlistSvc(t, 5)
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1) // SQLite 写串行化；MySQL 生产环境由活动行 FOR UPDATE 锁保证

	const n = 20
	var wg sync.WaitGroup
	errCh := make(chan error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		uid := uint64(10 + i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, err := svc.Create(1, uid, "n", "p", ""); err != nil {
				errCh <- err
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("concurrent signup error: %v", err)
	}
	assertCounts(t, db, 1, 5, n-5)

	// 候补顺序必须严格按提交先后（报名 ID 升序）
	var waits []model.Registration
	db.Where("activity_id = ? AND status = ?", 1, constants.RegistrationStatusWaitlisted).Order("id ASC").Find(&waits)
	if len(waits) != n-5 {
		t.Fatalf("waitlist size = %d, want %d", len(waits), n-5)
	}
	for i := 1; i < len(waits); i++ {
		if waits[i].ID <= waits[i-1].ID {
			t.Fatalf("候补顺序必须与提交先后一致：第 %d 条 id=%d 应大于前一条 id=%d", i, waits[i].ID, waits[i-1].ID)
		}
	}
}

// TestWaitlistApproveRejected 候补转正前不能审核通过，只能拒绝退出队列。
func TestWaitlistApproveConflict(t *testing.T) {
	db, svc := setupWaitlistSvc(t, 1)
	var w *model.Registration
	for _, uid := range []uint64{10, 11} {
		r, err := svc.Create(1, uid, "n", "p", "")
		if err != nil {
			t.Fatal(err)
		}
		if uid == 11 {
			w = r
		}
	}
	if _, err := svc.Review(w.ID, 1, constants.RoleOrganizer, constants.ReviewStatusApproved); err == nil {
		t.Fatal("候补报名审核通过应当失败")
	}
	// 候补仍在，且正式名额未受影响
	if got := mustFindReg(t, db, 1, 11); got.Status != constants.RegistrationStatusWaitlisted {
		t.Fatalf("user 11 = %s, want still waitlisted", got.Status)
	}
	assertCounts(t, db, 1, 1, 1)
}

func mustFindReg(t *testing.T, db *gorm.DB, activityID, userID uint64) *model.Registration {
	t.Helper()
	var r model.Registration
	if err := db.Where("activity_id = ? AND user_id = ?", activityID, userID).First(&r).Error; err != nil {
		t.Fatalf("find reg activity=%d user=%d: %v", activityID, userID, err)
	}
	return &r
}

func assertCounts(t *testing.T, db *gorm.DB, activityID uint64, wantOccupied, wantWaitlisted int) {
	t.Helper()
	var occupied, waitlisted int64
	db.Model(&model.Registration{}).
		Where("activity_id = ? AND status IN ?", activityID, constants.OccupiedRegistrationStatuses).Count(&occupied)
	db.Model(&model.Registration{}).
		Where("activity_id = ? AND status = ?", activityID, constants.RegistrationStatusWaitlisted).Count(&waitlisted)
	if int(occupied) != wantOccupied || int(waitlisted) != wantWaitlisted {
		t.Fatalf("counts: occupied=%d(want %d) waitlisted=%d(want %d)", occupied, wantOccupied, waitlisted, wantWaitlisted)
	}
}
