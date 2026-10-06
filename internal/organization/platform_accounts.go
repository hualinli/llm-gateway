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

// ListPlatformOrganizations returns all accounts using a stable id cursor. A
// nil type filter includes both personal and enterprise accounts.
func (s *Service) ListPlatformOrganizations(ctx context.Context, typeFilter string, limit int, cursor int64) ([]PlatformOrganization, string, error) {
	limit = normalizeLimit(limit)
	rows, err := s.db.QueryContext(ctx, `
		SELECT o.id, o.type, o.name, o.status, o.legacy_tenant_id, o.created_at,
		       o.owner_user_id, u.email, o.model_allowlist
		FROM organizations AS o
		JOIN users AS u ON u.id = o.owner_user_id
		WHERE o.id > $1 AND ($2 = '' OR o.type = $2)
		ORDER BY o.id ASC
		LIMIT $3`, cursor, typeFilter, limit+1)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()

	items := make([]PlatformOrganization, 0, limit)
	for rows.Next() {
		var item PlatformOrganization
		var legacy sql.NullString
		var rawAllowlist sql.NullString
		if err := rows.Scan(
			&item.ID, &item.Type, &item.Name, &item.Status, &legacy, &item.CreatedAt,
			&item.OwnerUserID, &item.OwnerEmail, &rawAllowlist,
		); err != nil {
			return nil, "", err
		}
		if legacy.Valid {
			value := legacy.String
			item.LegacyTenantID = &value
		}
		if rawAllowlist.Valid {
			var allowlist []string
			if err := json.Unmarshal([]byte(rawAllowlist.String), &allowlist); err != nil {
				return nil, "", err
			}
			item.ModelAllowlist = &allowlist
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
func (s *Service) SetOrganizationStatus(ctx context.Context, orgID int64, status string) (Organization, error) {
	if status != statusActive && status != statusSuspended {
		return Organization{}, ErrOrganizationStatusInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Organization{}, err
	}
	defer func() { _ = tx.Rollback() }()

	var lockedID int64
	err = tx.QueryRowContext(ctx, queryLockOrganization, orgID).Scan(&lockedID)
	if errors.Is(err, sql.ErrNoRows) {
		return Organization{}, ErrNotFound
	}
	if err != nil {
		return Organization{}, err
	}

	org, err := scanOrganization(tx.QueryRowContext(ctx, queryGetOrganization, orgID).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return Organization{}, ErrNotFound
	}
	if err != nil {
		return Organization{}, err
	}
	if org.Status == status {
		if err := tx.Commit(); err != nil {
			return Organization{}, err
		}
		return org, nil
	}

	org, err = scanOrganization(tx.QueryRowContext(ctx, `
		UPDATE organizations
		SET status = $2, updated_at = $3
		WHERE id = $1
		RETURNING id, type, name, status, legacy_tenant_id, created_at`, orgID, status, time.Now().UTC()).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return Organization{}, ErrNotFound
	}
	if err != nil {
		return Organization{}, err
	}
	if err := tx.Commit(); err != nil {
		return Organization{}, err
	}
	return org, nil
}
