// 报名状态枚举（与后端 backend/internal/constants/registration.go 保持一致）
export const RegistrationStatus = {
  REGISTERED: 'registered',
  CANCELLED: 'cancelled',
  CHECKED_IN: 'checked_in',
  WAITLISTED: 'waitlisted',
} as const

export const RegistrationStatusText: Record<string, string> = {
  [RegistrationStatus.REGISTERED]: '已报名',
  [RegistrationStatus.CANCELLED]: '已取消',
  [RegistrationStatus.CHECKED_IN]: '已签到',
  [RegistrationStatus.WAITLISTED]: '候补中',
}

export const ReviewStatus = {
  PENDING: 'pending',
  APPROVED: 'approved',
  REJECTED: 'rejected',
} as const

export const ReviewStatusText: Record<string, string> = {
  [ReviewStatus.PENDING]: '待审核',
  [ReviewStatus.APPROVED]: '已通过',
  [ReviewStatus.REJECTED]: '已拒绝',
}

export const CheckInMethodText: Record<string, string> = {
  voucher: '凭证签到',
  scan: '扫码签到',
}

// 综合展示：审核驳回会释放名额（status=cancelled + review_status=rejected），
// 名单中需显示为“已驳回”，与用户主动取消区分开。
export function displayRegistrationText(row: { status: string; review_status: string }): string {
  if (row.status === RegistrationStatus.CANCELLED && row.review_status === ReviewStatus.REJECTED) {
    return '已驳回'
  }
  return RegistrationStatusText[row.status] ?? row.status
}

export function displayRegistrationTag(row: { status: string; review_status: string }): string {
  if (row.status === RegistrationStatus.CANCELLED && row.review_status === ReviewStatus.REJECTED) {
    return 'danger'
  }
  if (row.status === RegistrationStatus.CHECKED_IN) return 'success'
  if (row.status === RegistrationStatus.CANCELLED) return 'info'
  if (row.status === RegistrationStatus.WAITLISTED) return 'warning'
  return 'primary'
}

// 可取消的报名：正式报名与候补中；已取消/已签到不可取消。
export function isCancellable(status: string): boolean {
  return status === RegistrationStatus.REGISTERED || status === RegistrationStatus.WAITLISTED
}
