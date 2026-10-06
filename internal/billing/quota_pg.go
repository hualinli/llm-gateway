package billing

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Embed IANA zone data so Asia/Shanghai remains available in minimal service
// images. Beijing currently has a fixed UTC+8 offset; quota.go's fallback is
// only used if this data cannot be loaded.
type admissionMembership struct {
	LimitMicro *int64
}

type memberUsageCounter struct {
	ActualMicro   int64
	ReservedMicro int64
}

const queryLockAdmissionMembership = `SELECT m.monthly_limit_micro, m.status, o.status, u.status
FROM memberships m
JOIN organizations o ON o.id = m.org_id
JOIN users u ON u.id = m.user_id
WHERE m.id = $1 AND m.org_id = $2 AND m.user_id = $3
FOR UPDATE OF m, u`

const queryEnsureMemberUsageCounter = `INSERT INTO billing_member_monthly_usage
  (org_id, user_id, quota_month, actual_micro, reserved_micro, updated_at)
VALUES ($1, $2, $3::date, 0, 0, $4)
ON CONFLICT (org_id, user_id, quota_month) DO NOTHING`

const queryLockMemberUsageCounter = `SELECT actual_micro, reserved_micro
FROM billing_member_monthly_usage
WHERE org_id = $1 AND user_id = $2 AND quota_month = $3::date
FOR UPDATE`

// lockAdmissionMembershipTx is the last business authorization check before
// the money is reserved. Gateway policy and the Key snapshot are checked
// earlier, but this row lock makes cap/status changes serialize with Reserve.
func lockAdmissionMembershipTx(ctx context.Context, tx txExecutor, params ReserveParams) (admissionMembership, error) {
	if params.MembershipID <= 0 || params.UserID <= 0 {
		return admissionMembership{}, fmt.Errorf("%w: a verified key membership and user are required", ErrMembershipNotFound)
	}
	var (
		limit       sql.NullInt64
		memberState string
		orgState    string
		userState   string
	)
	err := tx.QueryRowContext(ctx, queryLockAdmissionMembership,
		params.MembershipID, params.OrgID, params.UserID).Scan(&limit, &memberState, &orgState, &userState)
	if errors.Is(err, sql.ErrNoRows) {
		return admissionMembership{}, fmt.Errorf("%w: membership_id=%d", ErrMembershipNotFound, params.MembershipID)
	}
	if err != nil {
		return admissionMembership{}, err
	}
	if memberState != "active" {
		return admissionMembership{}, fmt.Errorf("%w: membership_id=%d status=%s", ErrMembershipInactive, params.MembershipID, memberState)
	}
	if orgState != "active" {
		return admissionMembership{}, fmt.Errorf("%w: org_id=%d status=%s", ErrAccountSuspended, params.OrgID, orgState)
	}
	if userState != "active" {
		return admissionMembership{}, fmt.Errorf("%w: user_id=%d status=%s", ErrMembershipInactive, params.UserID, userState)
	}
	member := admissionMembership{}
	if limit.Valid {
		value := limit.Int64
		member.LimitMicro = &value
	}
	return member, nil
}

func lockMemberQuotaTx(ctx context.Context, tx txExecutor, orgID, userID int64, monthDate string, now time.Time) (memberUsageCounter, error) {
	if _, err := tx.ExecContext(ctx, queryEnsureMemberUsageCounter, orgID, userID, monthDate, now); err != nil {
		return memberUsageCounter{}, err
	}
	var usage memberUsageCounter
	if err := tx.QueryRowContext(ctx, queryLockMemberUsageCounter, orgID, userID, monthDate).
		Scan(&usage.ActualMicro, &usage.ReservedMicro); err != nil {
		return memberUsageCounter{}, err
	}
	return usage, nil
}

func reserveMemberQuotaTx(ctx context.Context, tx txExecutor, orgID, userID int64, monthDate string, amount int64, now time.Time) error {
	usage, err := lockMemberQuotaTx(ctx, tx, orgID, userID, monthDate, now)
	if err != nil {
		return err
	}
	reserved, err := addMicro(usage.ReservedMicro, amount)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE billing_member_monthly_usage
SET reserved_micro = $4, updated_at = $5
WHERE org_id = $1 AND user_id = $2 AND quota_month = $3::date`, orgID, userID, monthDate, reserved, now)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return fmt.Errorf("%w: member quota counter disappeared during reservation", ErrQuotaProjectionMismatch)
	}
	return nil
}

// settleMemberQuotaTx transfers the reservation to actual spend (or adds a
// known actual amount for a legacy execution) in the same transaction as the
// debit and release. A missing/underflowed projection fails closed so the
// accounting fact and wallet cannot diverge.
func settleMemberQuotaTx(ctx context.Context, tx txExecutor, execution Execution, releaseMicro, chargeMicro int64, now time.Time) error {
	if execution.MembershipID == nil || execution.QuotaMonth == nil || execution.UserID == nil {
		return nil
	}
	return updateMemberQuotaTx(ctx, tx, execution, releaseMicro, chargeMicro, now)
}

func releaseMemberQuotaTx(ctx context.Context, tx txExecutor, execution Execution, releaseMicro, chargeMicro int64, now time.Time) error {
	if execution.MembershipID == nil || execution.QuotaMonth == nil || execution.UserID == nil {
		return nil
	}
	return updateMemberQuotaTx(ctx, tx, execution, releaseMicro, chargeMicro, now)
}

func updateMemberQuotaTx(ctx context.Context, tx txExecutor, execution Execution, releaseMicro, chargeMicro int64, now time.Time) error {
	if releaseMicro < 0 || chargeMicro < 0 {
		return fmt.Errorf("%w: quota settlement amounts must be non-negative", ErrInvalidArgument)
	}
	monthDate := *execution.QuotaMonth + "-01"
	result, err := tx.ExecContext(ctx, `UPDATE billing_member_monthly_usage
SET actual_micro = actual_micro + $5,
    reserved_micro = reserved_micro - $4,
    updated_at = $6
WHERE org_id = $1 AND user_id = $2 AND quota_month = $3::date
  AND reserved_micro >= $4`, execution.OrgID, *execution.UserID, monthDate,
		releaseMicro, chargeMicro, now)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return fmt.Errorf("%w: missing or insufficient member quota reservation for execution %s",
			ErrQuotaProjectionMismatch, execution.ExecutionID)
	}
	return nil
}

// GetMemberQuota returns one active member's quota and usage projection for a
// Beijing month. It never creates a counter as a side effect of a read.
func (s *PGAccountStore) GetMemberQuota(ctx context.Context, orgID, userID, membershipID int64, month BillingMonth) (MemberQuota, error) {
	if s == nil || s.DB == nil {
		return MemberQuota{}, errors.New("billing: account store is not configured")
	}
	if orgID <= 0 || userID <= 0 || membershipID <= 0 || month.Label == "" || month.StartDate == "" {
		return MemberQuota{}, fmt.Errorf("%w: org, user, membership and month are required", ErrInvalidArgument)
	}
	ctx, cancel := s.withTimeout(ctx)
	defer cancel()
	var (
		limit  sql.NullInt64
		status string
	)
	err := s.DB.QueryRowContext(ctx, `SELECT monthly_limit_micro, status
FROM memberships WHERE id = $1 AND org_id = $2 AND user_id = $3`, membershipID, orgID, userID).
		Scan(&limit, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return MemberQuota{}, fmt.Errorf("%w: membership_id=%d", ErrMembershipNotFound, membershipID)
	}
	if err != nil {
		return MemberQuota{}, err
	}
	if status != "active" {
		return MemberQuota{}, ErrMembershipInactive
	}
	quota := MemberQuota{OrgID: orgID, UserID: userID, MembershipID: membershipID, Month: month}
	if limit.Valid {
		value := limit.Int64
		quota.LimitMicro = &value
	}
	err = s.DB.QueryRowContext(ctx, `SELECT actual_micro, reserved_micro
FROM billing_member_monthly_usage
WHERE org_id = $1 AND user_id = $2 AND quota_month = $3::date`, orgID, userID, month.StartDate).
		Scan(&quota.UsedMicro, &quota.ReservedMicro)
	if errors.Is(err, sql.ErrNoRows) {
		quota.UsedMicro, quota.ReservedMicro = 0, 0
	} else if err != nil {
		return MemberQuota{}, err
	}
	quota.RemainingMicro, err = quotaRemaining(quota.LimitMicro, quota.UsedMicro, quota.ReservedMicro)
	if err != nil {
		return MemberQuota{}, err
	}
	return quota, nil
}
