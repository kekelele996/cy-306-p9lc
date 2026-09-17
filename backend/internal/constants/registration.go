package constants

// RegistrationStatus 报名状态枚举。
const (
	RegistrationStatusRegistered = "registered"
	RegistrationStatusWaitlisted = "waitlisted"
	RegistrationStatusCancelled  = "cancelled"
	RegistrationStatusCheckedIn  = "checked_in"
)

// RegistrationStatusValues 全部报名状态值。
var RegistrationStatusValues = []string{
	RegistrationStatusRegistered, RegistrationStatusWaitlisted,
	RegistrationStatusCancelled, RegistrationStatusCheckedIn,
}

// OccupiedRegistrationStatuses 占用活动名额的报名状态（候补/已取消不占名额）。
var OccupiedRegistrationStatuses = []string{
	RegistrationStatusRegistered, RegistrationStatusCheckedIn,
}

// ReviewStatus 报名审核状态枚举。
const (
	ReviewStatusPending  = "pending"
	ReviewStatusApproved = "approved"
	ReviewStatusRejected = "rejected"
)

// ReviewStatusValues 全部审核状态值。
var ReviewStatusValues = []string{ReviewStatusPending, ReviewStatusApproved, ReviewStatusRejected}

// CheckInMethod 签到方式枚举。
const (
	CheckInMethodVoucher = "voucher"
	CheckInMethodScan    = "scan"
)

// IsValidRegistrationStatus 校验报名状态。
func IsValidRegistrationStatus(s string) bool {
	for _, v := range RegistrationStatusValues {
		if v == s {
			return true
		}
	}
	return false
}

// IsOccupiedRegistrationStatus 判断报名状态是否占用活动名额。
func IsOccupiedRegistrationStatus(s string) bool {
	for _, v := range OccupiedRegistrationStatuses {
		if v == s {
			return true
		}
	}
	return false
}

// IsValidReviewStatus 校验审核状态。
func IsValidReviewStatus(s string) bool {
	for _, v := range ReviewStatusValues {
		if v == s {
			return true
		}
	}
	return false
}
