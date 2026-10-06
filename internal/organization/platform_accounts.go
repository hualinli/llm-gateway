package organization

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"time"
)

var ErrOrganizationStatusInvalid = errors.New("organization: status must be active or suspended")

// PlatformOrganization is the support view used by platform account management.
// It intentionally contains the owner and account model scope but no member or
// API-key data.
type PlatformOrganization struct {
	Organization
	OwnerUserID    int64
	OwnerEmail     string
	ModelAllowlist *[]string
}

const queryGetPlatformOrganization = `
	SELECT o.id, o.type, o.name, o.status, o.legacy_tenant_id, o.created_at,
	       o.owner_user_id, u.email, o.model_allowlist
	FROM organizations AS o
	JOIN users AS u ON u.id = o.owner_user_id
	WHERE o.id = $1`

// ListPlatformOrganizations returns all accounts using a stable id cursor. A
// nil type filter includes both personal and enterprise accounts.
func (s *Service) ListPlatformOrganizations(ctx context.Context, typeFilter string, limit int, cursor int64) ([]PlatformOrganization, string, error) {
	limit = normalizeLimit(limit)
	rows, err := s.db.QueryContext(ctx, queryListPlatformOrganizations, cursor, typeFilter, limit+1)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()

	items := make([]PlatformOrganization, 0, limit)
	for rows.Next() {
		item, err := scanPlatformOrganization(rows.Scan)
		if err != nil {
			return nil, "", err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	next := ""
	if len(items) > limit {
		items = items[:limit]
		next = strconv.FormatInt(items[len(items)-1].ID, 10)
	}
	return items, next, nil
}

// SetOrganizationStatus changes only the organization state. Membership rows
// are preserved so a later resume restores the same account relationships.
func (s *Service) SetOrganizationStatus(ctx context.Context, orgID int64, status string) (PlatformOrganization, error) {
	if status != statusActive && status != statusSuspended {
		return PlatformOrganization{}, ErrOrganizationStatusInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PlatformOrganization{}, err
	}
	defer func() { _ = tx.Rollback() }()

	var lockedID int64
	err = tx.QueryRowContext(ctx, queryLockOrganization, orgID).Scan(&lockedID)
	if errors.Is(err, sql.ErrNoRows) {
		return PlatformOrganization{}, ErrNotFound
	}
	if err != nil {
		return PlatformOrganization{}, err
	}

	org, err := scanPlatformOrganization(tx.QueryRowContext(ctx, queryGetPlatformOrganization, orgID).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return PlatformOrganization{}, ErrNotFound
	}
	if err != nil {
		return PlatformOrganization{}, err
	}
	if org.Organization.Status == status {
		if err := tx.Commit(); err != nil {
			return PlatformOrganization{}, err
		}
		return org, nil
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE organizations
		SET status = $2, updated_at = $3
		WHERE id = $1`, orgID, status, time.Now().UTC()); err != nil {
		return PlatformOrganization{}, err
	}
	org, err = scanPlatformOrganization(tx.QueryRowContext(ctx, queryGetPlatformOrganization, orgID).Scan)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return PlatformOrganization{}, ErrNotFound
		}
		return PlatformOrganization{}, err
	}
	if err := tx.Commit(); err != nil {
		return PlatformOrganization{}, err
	}
	return org, nil
}

const queryListPlatformOrganizations = `
	SELECT o.id, o.type, o.name, o.status, o.legacy_tenant_id, o.created_at,
	       o.owner_user_id, u.email, o.model_allowlist
	FROM organizations AS o
	JOIN users AS u ON u.id = o.owner_user_id
	WHERE o.id > $1 AND ($2 = '' OR o.type = $2)
	ORDER BY o.id ASC
	LIMIT $3`

func scanPlatformOrganization(scan func(dest ...any) error) (PlatformOrganization, error) {
	var (
		item         PlatformOrganization
		legacy       sql.NullString
		rawAllowlist sql.NullString
	)
	if err := scan(
		&item.ID, &item.Type, &item.Name, &item.Status, &legacy, &item.CreatedAt,
		&item.OwnerUserID, &item.OwnerEmail, &rawAllowlist,
	); err != nil {
		return PlatformOrganization{}, err
	}
	if legacy.Valid {
		value := legacy.String
		item.LegacyTenantID = &value
	}
	if rawAllowlist.Valid {
		var allowlist []string
		if err := json.Unmarshal([]byte(rawAllowlist.String), &allowlist); err != nil {
			return PlatformOrganization{}, err
		}
		item.ModelAllowlist = &allowlist
	}
	return item, nil
}
