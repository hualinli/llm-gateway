package organization

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"
)

const maxCatalogModelName = 128

var ErrModelCatalogInvalid = errors.New("organization: model catalog contains an invalid or duplicate model")

// PlatformModel is one globally known model and its platform-wide admission
// switch. Disabled rows remain visible to platform admins for later re-enable.
type PlatformModel struct {
	Model   string
	Enabled bool
}

// ListPlatformModelCatalog returns the explicitly configured global catalog in
// stable lexical order. An empty result means every model is closed.
func (s *Service) ListPlatformModelCatalog(ctx context.Context) ([]PlatformModel, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT model, enabled FROM platform_model_catalog ORDER BY model`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	items := make([]PlatformModel, 0)
	for rows.Next() {
		var item PlatformModel
		if err := rows.Scan(&item.Model, &item.Enabled); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return items, nil
}

// ReplacePlatformModelCatalog replaces the complete configured catalog in one
// transaction. The advisory lock serializes concurrent full-replacement writes
// while normal admission reads remain non-blocking.
func (s *Service) ReplacePlatformModelCatalog(ctx context.Context, actorUserID int64, requested []PlatformModel) ([]PlatformModel, error) {
	items, err := canonicalPlatformModels(requested)
	if err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(1668506979, 16)`); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM platform_model_catalog`); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	for _, item := range items {
		if _, err := tx.ExecContext(ctx, `INSERT INTO platform_model_catalog (model, enabled, updated_by, updated_at)
			VALUES ($1, $2, $3, $4)`, item.Model, item.Enabled, nullableUserID(actorUserID), now); err != nil {
			return nil, err
		}
	}
	result, err := listPlatformModelCatalog(ctx, tx)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return result, nil
}

// GetOrganizationModelAllowlist returns nil for inherited platform access, an
// empty non-nil slice for deny-all, or the explicit model names.
func (s *Service) GetOrganizationModelAllowlist(ctx context.Context, orgID int64) (*[]string, error) {
	var raw []byte
	err := s.db.QueryRowContext(ctx, `SELECT model_allowlist FROM organizations WHERE id = $1`, orgID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return decodeModelAllowlist(raw)
}

// SetOrganizationModelAllowlist persists the tri-state scope on the account.
// It locks the organization row with NO KEY UPDATE, which composes with FK
// KEY SHARE locks taken by Billing reservations and keeps the status and model
// policy mutations serialized with other organization state changes.
func (s *Service) SetOrganizationModelAllowlist(ctx context.Context, orgID int64, requested *[]string) (*[]string, error) {
	var value any
	if requested != nil {
		canonical, err := canonicalModelNames(*requested)
		if err != nil {
			return nil, err
		}
		encoded, err := json.Marshal(canonical)
		if err != nil {
			return nil, ErrModelCatalogInvalid
		}
		value = encoded
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	var lockedID int64
	err = tx.QueryRowContext(ctx, queryLockOrganization, orgID).Scan(&lockedID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE organizations SET model_allowlist = $2::jsonb, updated_at = $3 WHERE id = $1`, orgID, value, time.Now().UTC()); err != nil {
		return nil, err
	}
	var raw []byte
	if err := tx.QueryRowContext(ctx, `SELECT model_allowlist FROM organizations WHERE id = $1`, orgID).Scan(&raw); err != nil {
		return nil, err
	}
	result, err := decodeModelAllowlist(raw)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return result, nil
}

func listPlatformModelCatalog(ctx context.Context, tx *sql.Tx) ([]PlatformModel, error) {
	rows, err := tx.QueryContext(ctx, `SELECT model, enabled FROM platform_model_catalog ORDER BY model`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]PlatformModel, 0)
	for rows.Next() {
		var item PlatformModel
		if err := rows.Scan(&item.Model, &item.Enabled); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return items, nil
}

func canonicalPlatformModels(requested []PlatformModel) ([]PlatformModel, error) {
	items := make([]PlatformModel, 0, len(requested))
	seen := make(map[string]struct{}, len(requested))
	for _, item := range requested {
		model := strings.TrimSpace(item.Model)
		if model == "" || len(model) > maxCatalogModelName {
			return nil, ErrModelCatalogInvalid
		}
		if _, exists := seen[model]; exists {
			return nil, ErrModelCatalogInvalid
		}
		seen[model] = struct{}{}
		items = append(items, PlatformModel{Model: model, Enabled: item.Enabled})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Model < items[j].Model })
	return items, nil
}

func canonicalModelNames(requested []string) ([]string, error) {
	items := make([]PlatformModel, 0, len(requested))
	for _, model := range requested {
		items = append(items, PlatformModel{Model: model})
	}
	canonical, err := canonicalPlatformModels(items)
	if err != nil {
		return nil, err
	}
	models := make([]string, 0, len(canonical))
	for _, item := range canonical {
		models = append(models, item.Model)
	}
	return models, nil
}

func decodeModelAllowlist(raw []byte) (*[]string, error) {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return nil, nil
	}
	var models []string
	if err := json.Unmarshal(raw, &models); err != nil {
		return nil, err
	}
	if models == nil {
		models = []string{}
	}
	canonical, err := canonicalModelNames(models)
	if err != nil {
		return nil, err
	}
	return &canonical, nil
}

func nullableUserID(userID int64) any {
	if userID <= 0 {
		return nil
	}
	return userID
}
