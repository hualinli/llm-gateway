package organization

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	activationCodePrefix = "gwe_"
	activationCodeBytes  = 32
	activationCodeMaxLen = 128
)

// CreateActivationCode stores only a SHA-256 digest of a high-entropy random
// code. The raw value is returned to the caller once and is never queryable.
func (s *Service) CreateActivationCode(ctx context.Context, createdBy int64, enterpriseName string, expiresAt *time.Time) (ActivationCode, string, error) {
	name, err := normalizeOrgName(enterpriseName)
	if err != nil {
		return ActivationCode{}, "", err
	}
	now := time.Now().UTC()
	if expiresAt != nil {
		value := expiresAt.UTC()
		if !value.After(now) {
			return ActivationCode{}, "", ErrActivationExpiryInvalid
		}
		expiresAt = &value
	}

	for attempt := 0; attempt < 3; attempt++ {
		rawCode, codeHash, err := generateActivationCode()
		if err != nil {
			return ActivationCode{}, "", err
		}
		code, err := scanActivationCode(s.db.QueryRowContext(ctx, queryInsertActivationCode,
			codeHash, name, expiresAt, createdBy, now).Scan)
		if isUniqueViolation(err) {
			continue
		}
		if err != nil {
			return ActivationCode{}, "", err
		}
		return code, rawCode, nil
	}
	return ActivationCode{}, "", errors.New("organization: unable to allocate unique activation code")
}

// ListActivationCodes returns redacted code records ordered by their durable
// id. Expiry is derived by the HTTP serializer; the stored status never
// changes merely because time passed.
func (s *Service) ListActivationCodes(ctx context.Context, limit int, cursor int64) ([]ActivationCode, string, error) {
	if cursor < 0 {
		return nil, "", errors.New("organization: invalid activation-code cursor")
	}
	limit = normalizeLimit(limit)
	rows, err := s.db.QueryContext(ctx, queryListActivationCodes, cursor, limit+1)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()

	items := make([]ActivationCode, 0, limit)
	for rows.Next() {
		item, err := scanActivationCode(rows.Scan)
		if err != nil {
			return nil, "", err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	if len(items) > limit {
		items = items[:limit]
		return items, fmt.Sprint(items[limit-1].ID), nil
	}
	return items, "", nil
}

// RedeemActivationCode atomically consumes a code, creates a new enterprise,
// creates its owner membership incarnation, and provisions a zero-balance
// wallet. A row lock makes concurrent redemptions linearizable.
func (s *Service) RedeemActivationCode(ctx context.Context, userID int64, rawCode string) (MemberOrg, error) {
	rawCode = strings.TrimSpace(rawCode)
	if len(rawCode) < len(activationCodePrefix)+32 || len(rawCode) > activationCodeMaxLen || !strings.HasPrefix(rawCode, activationCodePrefix) {
		return MemberOrg{}, ErrActivationCodeUnavailable
	}
	codeHash := activationCodeHash(rawCode)
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return MemberOrg{}, err
	}
	defer func() { _ = tx.Rollback() }()

	code, err := scanActivationCode(tx.QueryRowContext(ctx, queryLockActivationCode, codeHash).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return MemberOrg{}, ErrActivationCodeUnavailable
	}
	if err != nil {
		return MemberOrg{}, err
	}
	if code.Status != "active" || (code.ExpiresAt != nil && !code.ExpiresAt.After(now)) {
		return MemberOrg{}, ErrActivationCodeUnavailable
	}
	if err := lockActiveUser(ctx, tx, userID, false); err != nil {
		return MemberOrg{}, err
	}

	org, err := scanOrganization(tx.QueryRowContext(ctx, queryInsertOrganization,
		typeEnterprise, code.EnterpriseName, userID, now).Scan)
	if isForeignKeyViolation(err) {
		return MemberOrg{}, ErrNotFound
	}
	if err != nil {
		return MemberOrg{}, err
	}
	var owner Membership
	if err := scanMembership(tx.QueryRowContext(ctx, queryInsertActivationOwnerMembership,
		org.ID, userID, roleOwner, nil, now).Scan, &owner); err != nil {
		return MemberOrg{}, err
	}
	if err := ensureZeroBalanceWallet(ctx, tx, org.ID, now); err != nil {
		return MemberOrg{}, err
	}
	result, err := tx.ExecContext(ctx, queryMarkActivationCodeUsed, code.ID, userID, now, org.ID)
	if err != nil {
		return MemberOrg{}, err
	}
	if affected, err := result.RowsAffected(); err != nil {
		return MemberOrg{}, err
	} else if affected != 1 {
		return MemberOrg{}, ErrActivationCodeUnavailable
	}
	if err := tx.Commit(); err != nil {
		return MemberOrg{}, err
	}
	return MemberOrg{
		Organization:      org,
		MembershipID:      owner.ID,
		Role:              owner.Role,
		Status:            owner.Status,
		MonthlyLimitMicro: cloneLimit(owner.MonthlyLimitMicro),
	}, nil
}

// CreateJoinRequest records a user's request to join an active enterprise.
// Organization locking serializes the current-membership check against
// invitation acceptance and other approval transactions; the partial unique
// index remains the final guard against two pending requests.
func (s *Service) CreateJoinRequest(ctx context.Context, userID, orgID int64, displayName string) (JoinRequest, error) {
	name := strings.TrimSpace(displayName)
	if countRunes(name) == 0 || countRunes(name) > maxApplicationNameRunes {
		return JoinRequest{}, ErrApplicationNameInvalid
	}
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return JoinRequest{}, err
	}
	defer func() { _ = tx.Rollback() }()

	if err := lockOrganization(ctx, tx, orgID); err != nil {
		return JoinRequest{}, err
	}
	if err := ensureEnterpriseInTx(ctx, tx, orgID); err != nil {
		if errors.Is(err, ErrPersonalTeam) || errors.Is(err, ErrOrganizationSuspended) {
			return JoinRequest{}, ErrNotFound
		}
		return JoinRequest{}, err
	}
	if err := lockActiveUser(ctx, tx, userID, false); err != nil {
		return JoinRequest{}, err
	}
	var currentStatus string
	err = tx.QueryRowContext(ctx, queryCheckCurrentMembership, orgID, userID).Scan(&currentStatus)
	if err == nil {
		return JoinRequest{}, ErrApplicationAlreadyMember
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return JoinRequest{}, err
	}

	var id int64
	if err := tx.QueryRowContext(ctx, queryInsertMembershipApplication, orgID, userID, name, now).Scan(&id); err != nil {
		if isUniqueViolation(err) {
			return JoinRequest{}, ErrApplicationPending
		}
		if isForeignKeyViolation(err) {
			return JoinRequest{}, ErrNotFound
		}
		return JoinRequest{}, err
	}
	request, err := scanJoinRequest(tx.QueryRowContext(ctx, queryGetMembershipApplication, orgID, id).Scan)
	if err != nil {
		return JoinRequest{}, err
	}
	if err := tx.Commit(); err != nil {
		return JoinRequest{}, err
	}
	return request, nil
}

// ListMyJoinRequests scopes the result by the authenticated user id; callers
// cannot supply an account id or user id through the HTTP contract.
func (s *Service) ListMyJoinRequests(ctx context.Context, userID int64, limit int, cursor int64) ([]JoinRequest, string, error) {
	if cursor < 0 {
		return nil, "", errors.New("organization: invalid application cursor")
	}
	limit = normalizeLimit(limit)
	return listJoinRequests(ctx, s.db, queryListMyMembershipApplications, userID, cursor, limit)
}

// ListJoinRequests authorizes from the caller's active organization membership.
// A platform-admin flag never substitutes for owner/admin membership.
func (s *Service) ListJoinRequests(ctx context.Context, actorUserID, orgID int64, status string, limit int, cursor int64) ([]JoinRequest, string, error) {
	if cursor < 0 {
		return nil, "", errors.New("organization: invalid application cursor")
	}
	if status != "" && status != "pending" && status != "approved" && status != "rejected" && status != "cancelled" {
		return nil, "", ErrApplicationStatusInvalid
	}
	actor, err := s.Membership(ctx, actorUserID, orgID)
	if err != nil {
		return nil, "", err
	}
	if err := ensureEnterprise(ctx, s.db, orgID); err != nil {
		if errors.Is(err, ErrPersonalTeam) || errors.Is(err, ErrOrganizationSuspended) {
			return nil, "", ErrNotFound
		}
		return nil, "", err
	}
	if !Allowed(actor.Role, ActionManageMembers) {
		return nil, "", ErrForbidden
	}
	limit = normalizeLimit(limit)
	return listOrgJoinRequests(ctx, s.db, orgID, status, cursor, limit)
}

// DecideJoinRequest changes one pending application. Approval always creates a
// fresh ordinary-member incarnation with an explicit cap, and commits the
// application decision, org revision, and policy publication in one
// transaction. Repeated/concurrent decisions observe a stable 409-style error.
func (s *Service) DecideJoinRequest(ctx context.Context, actorUserID, orgID, applicationID int64, decision string, monthlyLimit *int64, monthlyLimitProvided bool) (JoinRequest, error) {
	if decision != "approved" && decision != "rejected" {
		return JoinRequest{}, ErrApplicationDecisionInvalid
	}
	if monthlyLimit != nil && *monthlyLimit < 0 {
		return JoinRequest{}, ErrApplicationLimitInvalid
	}
	if decision == "approved" && !monthlyLimitProvided {
		return JoinRequest{}, ErrLimitRequired
	}
	if decision == "rejected" {
		monthlyLimit = nil
	}

	now := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return JoinRequest{}, err
	}
	defer func() { _ = tx.Rollback() }()

	if err := lockOrganization(ctx, tx, orgID); err != nil {
		return JoinRequest{}, err
	}
	actor, err := actorMembershipInTx(ctx, tx, orgID, actorUserID)
	if err != nil {
		return JoinRequest{}, err
	}
	if err := ensureEnterpriseInTx(ctx, tx, orgID); err != nil {
		if errors.Is(err, ErrPersonalTeam) || errors.Is(err, ErrOrganizationSuspended) {
			return JoinRequest{}, ErrNotFound
		}
		return JoinRequest{}, err
	}
	if !Allowed(actor.Role, ActionManageMembers) {
		return JoinRequest{}, ErrForbidden
	}

	request, err := scanJoinRequest(tx.QueryRowContext(ctx, queryGetMembershipApplication+" FOR UPDATE OF a", orgID, applicationID).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return JoinRequest{}, ErrApplicationNotFound
	}
	if err != nil {
		return JoinRequest{}, err
	}
	if request.Status != "pending" {
		return JoinRequest{}, ErrApplicationDecided
	}

	if decision == "approved" {
		if err := lockActiveUser(ctx, tx, request.UserID, true); err != nil {
			return JoinRequest{}, err
		}
		var currentStatus string
		err := tx.QueryRowContext(ctx, queryCheckCurrentMembership, orgID, request.UserID).Scan(&currentStatus)
		if err == nil {
			return JoinRequest{}, ErrApplicationAlreadyMember
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return JoinRequest{}, err
		}
		var member Membership
		if err := scanMembership(tx.QueryRowContext(ctx, queryInsertApplicationMembership,
			orgID, request.UserID, monthlyLimit, now).Scan, &member); err != nil {
			if isUniqueViolation(err) {
				return JoinRequest{}, ErrApplicationAlreadyMember
			}
			if isForeignKeyViolation(err) {
				return JoinRequest{}, ErrApplicationNotFound
			}
			return JoinRequest{}, err
		}
		var revision int64
		if err := tx.QueryRowContext(ctx, queryBumpOrgRevision, orgID, now).Scan(&revision); err != nil {
			return JoinRequest{}, err
		}
		if _, err := tx.ExecContext(ctx, queryInsertPolicyPublication, orgID, revision, now); err != nil {
			return JoinRequest{}, err
		}
	}

	if _, err := tx.ExecContext(ctx, queryDecideMembershipApplication,
		orgID, applicationID, decision, monthlyLimit, actorUserID, now); err != nil {
		return JoinRequest{}, err
	}
	request, err = scanJoinRequest(tx.QueryRowContext(ctx, queryGetMembershipApplication, orgID, applicationID).Scan)
	if err != nil {
		return JoinRequest{}, err
	}
	if err := tx.Commit(); err != nil {
		return JoinRequest{}, err
	}
	return request, nil
}

func lockOrganization(ctx context.Context, tx *sql.Tx, orgID int64) error {
	var lockedID int64
	err := tx.QueryRowContext(ctx, queryLockOrganization, orgID).Scan(&lockedID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

func lockActiveUser(ctx context.Context, tx *sql.Tx, userID int64, applicant bool) error {
	var status string
	err := tx.QueryRowContext(ctx, queryLockActiveUser, userID).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if status == "active" {
		return nil
	}
	if applicant {
		return ErrApplicantInactive
	}
	return ErrOnboardingActorInactive
}

func listJoinRequests(ctx context.Context, db *sql.DB, query string, userID, cursor int64, limit int) ([]JoinRequest, string, error) {
	rows, err := db.QueryContext(ctx, query, userID, cursor, limit+1)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	return scanJoinRequestPage(rows, limit)
}

func listOrgJoinRequests(ctx context.Context, db *sql.DB, orgID int64, status string, cursor int64, limit int) ([]JoinRequest, string, error) {
	rows, err := db.QueryContext(ctx, queryListOrgMembershipApplications, orgID, status, cursor, limit+1)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	return scanJoinRequestPage(rows, limit)
}

func scanJoinRequestPage(rows *sql.Rows, limit int) ([]JoinRequest, string, error) {
	items := make([]JoinRequest, 0, limit)
	for rows.Next() {
		item, err := scanJoinRequest(rows.Scan)
		if err != nil {
			return nil, "", err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	if len(items) > limit {
		items = items[:limit]
		return items, fmt.Sprint(items[limit-1].ID), nil
	}
	return items, "", nil
}

func scanActivationCode(scan func(dest ...any) error) (ActivationCode, error) {
	var (
		code    ActivationCode
		expires sql.NullTime
	)
	if err := scan(&code.ID, &code.EnterpriseName, &code.Status, &expires, &code.CreatedAt); err != nil {
		return ActivationCode{}, err
	}
	if expires.Valid {
		value := expires.Time.UTC()
		code.ExpiresAt = &value
	}
	code.CreatedAt = code.CreatedAt.UTC()
	return code, nil
}

func scanJoinRequest(scan func(dest ...any) error) (JoinRequest, error) {
	var (
		request  JoinRequest
		limit    sql.NullInt64
		reviewer sql.NullInt64
		reviewed sql.NullTime
	)
	if err := scan(&request.ID, &request.OrganizationID, &request.UserID, &request.Email,
		&request.DisplayName, &request.Status, &limit, &request.CreatedAt, &request.UpdatedAt,
		&reviewer, &reviewed); err != nil {
		return JoinRequest{}, err
	}
	if limit.Valid {
		value := limit.Int64
		request.MonthlyLimitMicro = &value
	}
	if reviewer.Valid {
		value := reviewer.Int64
		request.ReviewedBy = &value
	}
	if reviewed.Valid {
		value := reviewed.Time.UTC()
		request.ReviewedAt = &value
	}
	request.CreatedAt = request.CreatedAt.UTC()
	request.UpdatedAt = request.UpdatedAt.UTC()
	return request, nil
}

func generateActivationCode() (raw, digest string, err error) {
	secret := make([]byte, activationCodeBytes)
	if _, err := rand.Read(secret); err != nil {
		return "", "", err
	}
	raw = activationCodePrefix + base64.RawURLEncoding.EncodeToString(secret)
	return raw, activationCodeHash(raw), nil
}

func activationCodeHash(raw string) string {
	digest := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(digest[:])
}

func countRunes(value string) int { return len([]rune(value)) }
