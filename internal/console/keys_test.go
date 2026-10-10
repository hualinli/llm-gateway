package console

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"llm-gateway/internal/controlclient"
)

// T03 key-management HTTP tests: the real Hertz engine, the real
// ControlKeyService (so the permission matrix really runs) and a fake Control
// AdminService. Every assertion is made on what reached Control.

type keyPageBody struct {
	Items []struct {
		KeyID          string    `json:"key_id"`
		Name           string    `json:"name"`
		Prefix         string    `json:"prefix"`
		OrganizationID string    `json:"organization_id"`
		OwnerUserID    *string   `json:"owner_user_id"`
		Status         string    `json:"status"`
		ModelAllowlist *[]string `json:"model_allowlist"`
		TenantID       string    `json:"tenant_id"`
		CreatedAt      string    `json:"created_at"`
	} `json:"items"`
	NextCursor *string `json:"next_cursor"`
	Replayed   bool    `json:"replayed"`
}

type createKeyBody struct {
	Key struct {
		KeyID          string    `json:"key_id"`
		Name           string    `json:"name"`
		OwnerUserID    *string   `json:"owner_user_id"`
		ModelAllowlist *[]string `json:"model_allowlist"`
		Status         string    `json:"status"`
	} `json:"key"`
	Secret             string `json:"secret"`
	SecretReturnedOnce bool   `json:"secret_returned_once"`
	Replayed           bool   `json:"replayed"`
	Notice             string `json:"notice"`
}

func (h *harness) keyOrgFixture() {
	h.orgs.memberships = twoEnterpriseMemberships()
}

func TestKeyRoutesRequireSessionAndCSRF(t *testing.T) {
	h := newHarness(t, nil)
	h.keyOrgFixture()

	withoutSession := []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodGet, "/api/v1/orgs/2/keys", ""},
		{http.MethodPost, "/api/v1/orgs/2/keys", `{}`},
		{http.MethodPost, "/api/v1/orgs/2/keys/key_1/revoke", ""},
	}
	for _, route := range withoutSession {
		rec := h.serve(route.method, route.path, jsonBody(route.body))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without a session = %d, want 401", route.method, route.path, rec.Code)
		}
	}

	fx := h.t03Session(11, "admin@example.com")
	for _, route := range []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodPost, "/api/v1/orgs/2/keys", `{}`},
		{http.MethodPost, "/api/v1/orgs/2/keys/key_1/revoke", ""},
	} {
		rec := h.orgReq(route.method, route.path, route.body, fx, false)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s %s without CSRF = %d, want 403 (body=%s)", route.method, route.path, rec.Code, rec.Body.String())
		}
	}
	creates, revokes, lists, operations := h.control.calls()
	if creates+revokes+lists+operations != 0 {
		t.Fatalf("a rejected request reached Control: %d/%d/%d/%d", creates, revokes, lists, operations)
	}
}

func TestMemberIsScopedToOwnKeys(t *testing.T) {
	h := newHarness(t, nil)
	h.keyOrgFixture()
	member := h.t03Session(10, "member@example.com")

	// List: the member filter is forced, not taken from the query.
	h.control.listFn = func(_ context.Context, req controlclient.ListApiKeysRequest) (controlclient.ListApiKeysResult, error) {
		if req.OwnerUserID == nil || *req.OwnerUserID != 10 {
			t.Errorf("member list reached Control with owner=%v, want 10", req.OwnerUserID)
		}
		return controlclient.ListApiKeysResult{Keys: []controlclient.KeyMetadata{sampleKey("key_mine", 2, 10)}}, nil
	}
	rec := h.orgReq(http.MethodGet, "/api/v1/orgs/2/keys", "", member, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("member list = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	var page keyPageBody
	decodeInto(t, rec, &page)
	if len(page.Items) != 1 || page.Items[0].KeyID != "key_mine" {
		t.Fatalf("unexpected page %+v", page)
	}
	if page.Replayed {
		t.Fatal("a normal list must not claim replayed")
	}
	if got := h.control.lastList(t); got.OwnerUserID == nil || *got.OwnerUserID != 10 || got.OrgID != 2 || got.ActorUserID != 10 {
		t.Fatalf("control list request lost its scope: %+v", got)
	}

	// Asking for somebody else's keys uses the same 404 as an unknown owner.
	rec = h.orgReq(http.MethodGet, "/api/v1/orgs/2/keys?owner_user_id=11", "", member, false)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("member list of another owner = %d, want 404", rec.Code)
	}
	if _, _, lists, _ := h.control.calls(); lists != 1 {
		t.Fatalf("list calls=%d, want 1 (the refused request must not reach Control)", lists)
	}

	// Create: owner is forced to the caller's user id.
	secret := "sk-member-secret"
	h.control.createFn = func(_ context.Context, req controlclient.CreateApiKeyRequest) (controlclient.CreateApiKeyResult, error) {
		return controlclient.CreateApiKeyResult{
			Key:              sampleKey("key_new", 2, 10),
			Secret:           secret,
			ResultingVersion: 9,
		}, nil
	}
	rec = h.orgReq(http.MethodPost, "/api/v1/orgs/2/keys", `{"name":"My key"}`, member, true)
	if rec.Code != http.StatusCreated {
		t.Fatalf("member create = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}
	var created createKeyBody
	decodeInto(t, rec, &created)
	if created.Secret != secret || !created.SecretReturnedOnce || created.Replayed || created.Key.Name != "Example key" {
		t.Fatalf("create response must return the secret exactly once: %+v", created)
	}
	if !strings.Contains(strings.ToLower(created.Notice), "only once") {
		t.Fatalf("create response must say the secret is shown once, got %q", created.Notice)
	}
	if created.Key.OwnerUserID == nil || *created.Key.OwnerUserID != "10" {
		t.Fatalf("member-created key owner = %v, want 10", created.Key.OwnerUserID)
	}
	wire := h.control.lastCreate(t)
	if wire.OwnerUserID == nil || *wire.OwnerUserID != 10 {
		t.Fatalf("control create owner = %v, want 10", wire.OwnerUserID)
	}
	if wire.ActorUserID != 10 || wire.ActorOrgID != 2 || wire.OrgID != 2 {
		t.Fatalf("control create lost the actor triple: %+v", wire)
	}
	if wire.MembershipID != 210 || wire.Name != "My key" {
		t.Fatalf("Control request lost the name/membership binding: %+v", wire)
	}
	if wire.OperationID == "" {
		t.Fatal("control create must carry a generated operation id")
	}

	// Creating a key for somebody else is indistinguishable from a missing key.
	rec = h.orgReq(http.MethodPost, "/api/v1/orgs/2/keys", `{"name":"Other key","owner_user_id":"11"}`, member, true)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("member create for another owner = %d, want 404", rec.Code)
	}
	if creates, _, _, _ := h.control.calls(); creates != 1 {
		t.Fatalf("create calls=%d, want 1", creates)
	}

	// Revoke: Control atomically verifies the actor and membership incarnation.
	h.control.revokeFn = func(_ context.Context, req controlclient.RevokeApiKeyRequest) (controlclient.RevokeApiKeyResult, error) {
		if req.KeyID == "key_someone_else" {
			return controlclient.RevokeApiKeyResult{}, controlclient.ErrNotFound
		}
		key := sampleKey(req.KeyID, 2, 10)
		key.Status = "revoked"
		return controlclient.RevokeApiKeyResult{Key: key, ResultingVersion: 10}, nil
	}
	rec = h.orgReq(http.MethodPost, "/api/v1/orgs/2/keys/key_mine/revoke", "", member, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("member revoke of own key = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if got := h.control.lastRevoke(t); got.KeyID != "key_mine" || got.OrgID != 2 || got.ActorUserID != 10 {
		t.Fatalf("control revoke request wrong: %+v", got)
	}

	// Revoking somebody else's key is 404: the key is invisible to this member.
	h.control.listFn = func(context.Context, controlclient.ListApiKeysRequest) (controlclient.ListApiKeysResult, error) {
		return controlclient.ListApiKeysResult{}, nil
	}
	rec = h.orgReq(http.MethodPost, "/api/v1/orgs/2/keys/key_someone_else/revoke", "", member, true)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("member revoke of another user's key = %d, want 404 (body=%s)", rec.Code, rec.Body.String())
	}
	if _, revokes, _, _ := h.control.calls(); revokes != 2 {
		t.Fatalf("revoke calls=%d, want 2: an unknown/foreign Key is checked by Control", revokes)
	}
}

func TestAdminAndOwnerManageOnlyOwnKeys(t *testing.T) {
	for _, actor := range []struct {
		name string
		id   int64
	}{{"admin", 11}, {"owner", 13}} {
		t.Run(actor.name, func(t *testing.T) {
			h := newHarness(t, nil)
			h.keyOrgFixture()
			fx := h.t03Session(actor.id, actor.name+"@example.com")

			h.control.listFn = func(_ context.Context, req controlclient.ListApiKeysRequest) (controlclient.ListApiKeysResult, error) {
				if req.OwnerUserID == nil || *req.OwnerUserID != actor.id {
					t.Errorf("admin/owner list was not scoped to actor %d: %+v", actor.id, req)
				}
				return controlclient.ListApiKeysResult{
					Keys:          []controlclient.KeyMetadata{sampleKey("key_self", 2, actor.id)},
					NextPageToken: "next-token",
				}, nil
			}
			rec := h.orgReq(http.MethodGet, "/api/v1/orgs/2/keys?limit=50", "", fx, false)
			if rec.Code != http.StatusOK {
				t.Fatalf("org list = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
			}
			wire := h.control.lastList(t)
			if wire.OwnerUserID == nil || *wire.OwnerUserID != actor.id {
				t.Fatalf("an admin list must be owner-filtered to self, got %v", wire.OwnerUserID)
			}
			if wire.PageSize != 50 {
				t.Fatalf("page size=%d, want 50", wire.PageSize)
			}
			var page keyPageBody
			decodeInto(t, rec, &page)
			if len(page.Items) != 1 || page.Items[0].OwnerUserID == nil || *page.Items[0].OwnerUserID != formatID(actor.id) || page.NextCursor == nil || *page.NextCursor != "next-token" {
				t.Fatalf("unexpected org page %+v", page)
			}

			// Admins and owners also create only their own Keys.
			h.control.createFn = func(_ context.Context, req controlclient.CreateApiKeyRequest) (controlclient.CreateApiKeyResult, error) {
				owner := int64(0)
				if req.OwnerUserID != nil {
					owner = *req.OwnerUserID
				}
				return controlclient.CreateApiKeyResult{Key: sampleKey("key_self", 2, owner), Secret: "sk-for-self"}, nil
			}
			rec = h.orgReq(http.MethodPost, "/api/v1/orgs/2/keys", `{"name":"My admin key"}`, fx, true)
			if rec.Code != http.StatusCreated {
				t.Fatalf("admin create self key = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
			}
			if got := h.control.lastCreate(t); got.OwnerUserID == nil || *got.OwnerUserID != actor.id || got.MembershipID != 200+actor.id || got.Name != "My admin key" {
				t.Fatalf("self ownership/membership/name=%+v", got)
			}

			// Naming a colleague is indistinguishable from a missing target.
			rec = h.orgReq(http.MethodPost, "/api/v1/orgs/2/keys", `{"name":"Other key","owner_user_id":"10"}`, fx, true)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("admin create for another member = %d, want 404 (body=%s)", rec.Code, rec.Body.String())
			}

			// Revoke needs no list scan and is scoped to the current member.
			h.control.revokeFn = func(_ context.Context, req controlclient.RevokeApiKeyRequest) (controlclient.RevokeApiKeyResult, error) {
				key := sampleKey(req.KeyID, 2, actor.id)
				key.Status = "revoked"
				return controlclient.RevokeApiKeyResult{Key: key}, nil
			}
			_, revokesBefore, listsBefore, _ := h.control.calls()
			rec = h.orgReq(http.MethodPost, "/api/v1/orgs/2/keys/key_self/revoke", "", fx, true)
			if rec.Code != http.StatusOK {
				t.Fatalf("admin revoke = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
			}
			_, revokesAfter, listsAfter, _ := h.control.calls()
			if revokesAfter != revokesBefore+1 || listsAfter != listsBefore {
				t.Fatalf("admin revoke must not scan the key list (revokes %d->%d, lists %d->%d)",
					revokesBefore, revokesAfter, listsBefore, listsAfter)
			}
		})
	}
}

func TestBillingViewerCannotManageKeys(t *testing.T) {
	h := newHarness(t, nil)
	h.keyOrgFixture()
	viewer := h.t03Session(12, "billing@example.com")

	rec := h.orgReq(http.MethodGet, "/api/v1/orgs/2/keys", "", viewer, false)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("billing_viewer list = %d, want 403 (body=%s)", rec.Code, rec.Body.String())
	}
	if body := decodeError(t, rec); body.Error.Code != "forbidden" {
		t.Fatalf("error code=%q, want forbidden", body.Error.Code)
	}
	rec = h.orgReq(http.MethodPost, "/api/v1/orgs/2/keys", `{}`, viewer, true)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("billing_viewer create = %d, want 403", rec.Code)
	}
	rec = h.orgReq(http.MethodPost, "/api/v1/orgs/2/keys/key_1/revoke", "", viewer, true)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("billing_viewer revoke = %d, want 403", rec.Code)
	}
	rec = h.orgReq(http.MethodGet, "/api/v1/orgs/2/keys?operation_id=op-1", "", viewer, false)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("billing_viewer recovery = %d, want 403", rec.Code)
	}
	if creates, revokes, lists, operations := h.control.calls(); creates+revokes+lists+operations != 0 {
		t.Fatalf("a billing_viewer request reached Control: %d/%d/%d/%d", creates, revokes, lists, operations)
	}
}

func TestCrossOrgKeyRoutesAreNotFound(t *testing.T) {
	h := newHarness(t, nil)
	h.keyOrgFixture()
	outsider := h.t03Session(99, "outsider@example.com")

	for _, route := range []struct {
		method string
		path   string
		body   string
		csrf   bool
	}{
		{http.MethodGet, "/api/v1/orgs/2/keys", "", false},
		{http.MethodGet, "/api/v1/orgs/2/keys?operation_id=op-1", "", false},
		{http.MethodPost, "/api/v1/orgs/2/keys", `{}`, true},
		{http.MethodPost, "/api/v1/orgs/2/keys/key_a/revoke", "", true},
	} {
		rec := h.orgReq(route.method, route.path, route.body, outsider, route.csrf)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s from another organization = %d, want 404 (body=%s)", route.method, route.path, rec.Code, rec.Body.String())
		}
	}
	if creates, revokes, lists, operations := h.control.calls(); creates+revokes+lists+operations != 0 {
		t.Fatalf("a cross-org request reached Control: %d/%d/%d/%d", creates, revokes, lists, operations)
	}
}

func TestKeyOperationRecoveryReturnsMetadataWithoutSecret(t *testing.T) {
	h := newHarness(t, nil)
	h.keyOrgFixture()
	member := h.t03Session(10, "member@example.com")

	h.control.getOpFn = func(_ context.Context, req controlclient.GetOperationRequest) (controlclient.OperationResult, error) {
		if req.OperationID != "op-lost" {
			return controlclient.OperationResult{}, controlclient.ErrNotFound
		}
		key := sampleKey("key_recovered", 2, 10)
		return controlclient.OperationResult{
			OperationID:      "op-lost",
			Kind:             "create_api_key",
			Key:              &key,
			ResultingVersion: 12,
		}, nil
	}

	rec := h.orgReq(http.MethodGet, "/api/v1/orgs/2/keys?operation_id=op-lost", "", member, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("recovery = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	var page keyPageBody
	decodeInto(t, rec, &page)
	if !page.Replayed {
		t.Fatal("a recovery response must set replayed=true")
	}
	if len(page.Items) != 1 || page.Items[0].KeyID != "key_recovered" {
		t.Fatalf("unexpected recovery page %+v", page)
	}
	var raw map[string]json.RawMessage
	decodeInto(t, rec, &raw)
	if _, present := raw["secret"]; present {
		t.Fatal("a recovery response must not carry a secret field at all")
	}
	if wire := h.control.getOpCalls[len(h.control.getOpCalls)-1]; wire.ActorUserID != 10 || wire.ActorOrgID != 2 {
		t.Fatalf("recovery lost the actor triple: %+v", wire)
	}

	// An operation of another organization is not found.
	h.control.getOpFn = func(context.Context, controlclient.GetOperationRequest) (controlclient.OperationResult, error) {
		key := sampleKey("key_other_org", 3, 99)
		return controlclient.OperationResult{OperationID: "op-other", Key: &key}, nil
	}
	rec = h.orgReq(http.MethodGet, "/api/v1/orgs/2/keys?operation_id=op-other", "", member, false)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-org recovery = %d, want 404", rec.Code)
	}

	// Another user's operation is not found for a member.
	h.control.getOpFn = func(context.Context, controlclient.GetOperationRequest) (controlclient.OperationResult, error) {
		key := sampleKey("key_colleague", 2, 11)
		return controlclient.OperationResult{OperationID: "op-colleague", Key: &key}, nil
	}
	rec = h.orgReq(http.MethodGet, "/api/v1/orgs/2/keys?operation_id=op-colleague", "", member, false)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("another user's operation = %d, want 404", rec.Code)
	}

	// An unknown operation id is not found (Control answers NOT_FOUND).
	rec = h.orgReq(http.MethodGet, "/api/v1/orgs/2/keys?operation_id=op-unknown", "", member, false)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown operation = %d, want 404", rec.Code)
	}
}

func TestKeyCreateIsIdempotentByOperationID(t *testing.T) {
	h := newHarness(t, nil)
	h.keyOrgFixture()
	member := h.t03Session(10, "member@example.com")

	// Control replays: stored metadata, no secret, replayed=true.
	h.control.createFn = func(_ context.Context, req controlclient.CreateApiKeyRequest) (controlclient.CreateApiKeyResult, error) {
		if req.OperationID != "browser-retry-1" {
			return controlclient.CreateApiKeyResult{}, fmt.Errorf("unexpected operation id %q", req.OperationID)
		}
		return controlclient.CreateApiKeyResult{
			Key:              sampleKey("key_once", 2, 10),
			Replayed:         true,
			ResultingVersion: 9,
		}, nil
	}
	rec := h.orgReq(http.MethodPost, "/api/v1/orgs/2/keys", `{"name":"Retry key","operation_id":"browser-retry-1"}`, member, true)
	if rec.Code != http.StatusCreated {
		t.Fatalf("replayed create = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}
	var created createKeyBody
	decodeInto(t, rec, &created)
	if !created.Replayed || created.Secret != "" || created.SecretReturnedOnce {
		t.Fatalf("a replay must not return a secret: %+v", created)
	}
	if !strings.Contains(created.Notice, "operation_id") {
		t.Fatalf("a replayed create must point at the recovery route, got %q", created.Notice)
	}
	var raw map[string]json.RawMessage
	decodeInto(t, rec, &raw)
	if _, present := raw["secret"]; present {
		t.Fatal("a replayed create must omit the secret field")
	}

	// A malformed client operation id is refused before reaching Control.
	rec = h.orgReq(http.MethodPost, "/api/v1/orgs/2/keys",
		`{"name":"Retry key","operation_id":"`+strings.Repeat("x", maxKeyOperationIDLength+1)+`"}`, member, true)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("oversized operation_id = %d, want 400", rec.Code)
	}
}

func TestKeyCreateValidatesAllowlistAndExpiry(t *testing.T) {
	h := newHarness(t, nil)
	h.keyOrgFixture()
	admin := h.t03Session(11, "admin@example.com")
	h.control.createFn = func(_ context.Context, req controlclient.CreateApiKeyRequest) (controlclient.CreateApiKeyResult, error) {
		return controlclient.CreateApiKeyResult{Key: sampleKey("key_new", 2, 11), Secret: "sk-new"}, nil
	}

	// Tri-state: explicit allowlists are canonicalized (trimmed, sorted).
	rec := h.orgReq(http.MethodPost, "/api/v1/orgs/2/keys", `{"name":"Model key","model_allowlist":[" gpt-4o ","claude","gpt-4o"]}`, admin, true)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create with allowlist = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}
	wire := h.control.lastCreate(t)
	if wire.ModelAllowlist == nil || len(*wire.ModelAllowlist) != 2 ||
		(*wire.ModelAllowlist)[0] != "claude" || (*wire.ModelAllowlist)[1] != "gpt-4o" {
		t.Fatalf("allowlist=%v, want the canonical [claude gpt-4o]", wire.ModelAllowlist)
	}

	// An explicit empty list is deny-all, not "inherit".
	if rec := h.orgReq(http.MethodPost, "/api/v1/orgs/2/keys", `{"name":"Deny all","model_allowlist":[]}`, admin, true); rec.Code != http.StatusCreated {
		t.Fatalf("create with deny-all = %d, want 201", rec.Code)
	}
	wire = h.control.lastCreate(t)
	if wire.ModelAllowlist == nil || len(*wire.ModelAllowlist) != 0 {
		t.Fatalf("allowlist=%v, want a present empty (deny-all) set", wire.ModelAllowlist)
	}

	// Absent stays absent (inherit).
	if rec := h.orgReq(http.MethodPost, "/api/v1/orgs/2/keys", `{"name":"Inherited key"}`, admin, true); rec.Code != http.StatusCreated {
		t.Fatalf("create without allowlist = %d, want 201", rec.Code)
	}
	if wire = h.control.lastCreate(t); wire.ModelAllowlist != nil {
		t.Fatalf("allowlist=%v, want nil (inherit)", wire.ModelAllowlist)
	}

	// Malformed values never reach Control.
	before, _, _, _ := h.control.calls()
	invalid := []string{
		`{"name":"Test key","model_allowlist":[""]}`,
		`{"name":"Test key","model_allowlist":["` + strings.Repeat("m", maxModelNameLength+1) + `"]}`,
		`{"name":"Test key","expires_at":"2020-01-01T00:00:00Z"}`,
		`{"name":"Test key","owner_user_id":"0"}`,
		`{"name":"Test key","owner_user_id":"not-a-number"}`,
		`{"name":"   "}`,
		`{"name":"` + strings.Repeat("x", 121) + `"}`,
	}
	for _, body := range invalid {
		rec := h.orgReq(http.MethodPost, "/api/v1/orgs/2/keys", body, admin, true)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body %s = %d, want 400 (body=%s)", body, rec.Code, rec.Body.String())
		}
	}
	rec = h.orgReq(http.MethodPost, "/api/v1/orgs/2/keys", `{"name":"Expiry key","expires_at":"not-a-time"}`, admin, true)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("malformed expires_at = %d, want 400", rec.Code)
	}
	after, _, _, _ := h.control.calls()
	if after != before {
		t.Fatalf("invalid requests reached Control: %d -> %d", before, after)
	}

	// A valid future expiry is forwarded.
	rec = h.orgReq(http.MethodPost, "/api/v1/orgs/2/keys", `{"name":"Expiry key","expires_at":"2027-01-01T00:00:00Z"}`, admin, true)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create with a future expiry = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}
	wire = h.control.lastCreate(t)
	if wire.ExpiresAt == nil || !wire.ExpiresAt.Equal(time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("expires_at=%v, want 2027-01-01T00:00:00Z", wire.ExpiresAt)
	}
}

func TestKeyListPagingAndLimitClamping(t *testing.T) {
	h := newHarness(t, nil)
	h.keyOrgFixture()
	admin := h.t03Session(11, "admin@example.com")

	for _, testCase := range []struct {
		query string
		want  int
	}{
		{"", DefaultListLimit},
		{"?limit=1000", MaxListLimit},
		{"?limit=7", 7},
	} {
		h.control.listFn = nil
		rec := h.orgReq(http.MethodGet, "/api/v1/orgs/2/keys"+testCase.query, "", admin, false)
		if rec.Code != http.StatusOK {
			t.Fatalf("list %q = %d, want 200", testCase.query, rec.Code)
		}
		if got := h.control.lastList(t).PageSize; got != testCase.want {
			t.Errorf("list %q page size=%d, want %d", testCase.query, got, testCase.want)
		}
	}

	// The Control page token is passed through opaquely.
	rec := h.orgReq(http.MethodGet, "/api/v1/orgs/2/keys?cursor=next-token", "", admin, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("cursor list = %d, want 200", rec.Code)
	}
	if got := h.control.lastList(t).PageToken; got != "next-token" {
		t.Fatalf("page token=%q, want next-token", got)
	}

	// A malformed limit and an oversized cursor are 400, not a silent default.
	for _, query := range []string{"?limit=0", "?limit=abc", "?cursor=" + strings.Repeat("c", maxKeyCursorBytes+1)} {
		rec := h.orgReq(http.MethodGet, "/api/v1/orgs/2/keys"+query, "", admin, false)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("list %q = %d, want 400", query, rec.Code)
		}
	}
}

func TestKeyErrorMappingFromControl(t *testing.T) {
	cases := []struct {
		name       string
		controlErr error
		wantStatus int
		wantCode   string
	}{
		{"conflict", controlclient.ErrConflict, http.StatusConflict, "conflict"},
		{"precondition", &controlclient.PreconditionError{Message: "stale", CurrentVersion: versionPtr(7)}, http.StatusConflict, "conflict"},
		{"upstream auth", controlclient.ErrUpstreamAuth, http.StatusBadGateway, "upstream_unavailable"},
		{"unavailable", controlclient.ErrUnavailable, http.StatusBadGateway, "upstream_unavailable"},
		{"not found", controlclient.ErrNotFound, http.StatusNotFound, "not_found"},
		{"invalid argument", controlclient.ErrInvalidArgument, http.StatusBadRequest, "invalid_request"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			h := newHarness(t, nil)
			h.keyOrgFixture()
			admin := h.t03Session(11, "admin@example.com")
			h.control.createFn = func(context.Context, controlclient.CreateApiKeyRequest) (controlclient.CreateApiKeyResult, error) {
				return controlclient.CreateApiKeyResult{}, testCase.controlErr
			}
			rec := h.orgReq(http.MethodPost, "/api/v1/orgs/2/keys", `{"name":"Error mapping key"}`, admin, true)
			if rec.Code != testCase.wantStatus {
				t.Fatalf("status=%d, want %d (body=%s)", rec.Code, testCase.wantStatus, rec.Body.String())
			}
			body := decodeError(t, rec)
			if body.Error.Code != testCase.wantCode {
				t.Fatalf("code=%q, want %q", body.Error.Code, testCase.wantCode)
			}
		})
	}

	t.Run("no control channel answers 503", func(t *testing.T) {
		h := newHarness(t, func(opts *Options) { opts.Keys = nil })
		h.keyOrgFixture()
		admin := h.t03Session(11, "admin@example.com")
		rec := h.orgReq(http.MethodGet, "/api/v1/orgs/2/keys", "", admin, false)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status=%d, want 503 (body=%s)", rec.Code, rec.Body.String())
		}
		if body := decodeError(t, rec); body.Error.Code != "unavailable" {
			t.Fatalf("code=%q, want unavailable", body.Error.Code)
		}
	})
}

func TestForeignKeyRevokeFailureDoesNotLeakControlDetails(t *testing.T) {
	h := newHarness(t, nil)
	h.keyOrgFixture()
	admin := h.t03Session(11, "admin@example.com")
	const foreignDetails = "key_id=key_hidden name=Colleague secret key prefix=sk-hidden"
	h.control.revokeFn = func(context.Context, controlclient.RevokeApiKeyRequest) (controlclient.RevokeApiKeyResult, error) {
		return controlclient.RevokeApiKeyResult{}, fmt.Errorf("%w: %s", controlclient.ErrNotFound, foreignDetails)
	}

	rec := h.orgReq(http.MethodPost, "/api/v1/orgs/2/keys/key_hidden/revoke", "", admin, true)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("foreign key revoke = %d, want 404 (body=%s)", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), foreignDetails) || strings.Contains(rec.Body.String(), "key_hidden") {
		t.Fatalf("404 response exposed Control's foreign-key detail: %s", rec.Body.String())
	}
	events := h.audit.all()
	if len(events) != 1 {
		t.Fatalf("audit events = %+v, want one refusal", events)
	}
	event := events[0]
	detail, err := json.Marshal(event.Detail)
	if err != nil {
		t.Fatal(err)
	}
	if event.Outcome != "failure" || event.TargetID != "" || event.Error != "key_revoke_failed" ||
		strings.Contains(string(detail), "key_hidden") || strings.Contains(string(detail), "Colleague") || strings.Contains(event.Error, "prefix") {
		t.Fatalf("foreign-key details leaked into refusal audit: %+v detail=%s", event, detail)
	}
}

// TestKeySecretNeverLeaksIntoLogsOrAudit is the raw-secret containment test:
// the secret appears exactly once, in the create response body, and in no log
// line and no audit row.
func TestKeySecretNeverLeaksIntoLogsOrAudit(t *testing.T) {
	h := newHarness(t, nil)
	h.keyOrgFixture()
	member := h.t03Session(10, "member@example.com")
	const secret = "sk-0123456789abcdefghijklmnop"
	h.control.createFn = func(context.Context, controlclient.CreateApiKeyRequest) (controlclient.CreateApiKeyResult, error) {
		return controlclient.CreateApiKeyResult{
			Key:              sampleKey("key_secret", 2, 10),
			Secret:           secret,
			ResultingVersion: 4,
		}, nil
	}
	rec := h.orgReq(http.MethodPost, "/api/v1/orgs/2/keys", `{"name":"Secret key"}`, member, true)
	if rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), secret) {
		t.Fatalf("the create response must carry the secret once (status=%d)", rec.Code)
	}
	assertNoSecrets(t, h.logs.String(), secret)

	events := h.audit.all()
	if len(events) == 0 {
		t.Fatal("key creation must be audited")
	}
	for _, event := range events {
		detail, err := json.Marshal(event.Detail)
		if err != nil {
			t.Fatalf("marshal audit detail: %v", err)
		}
		if strings.Contains(string(detail), secret) || strings.Contains(event.Error, secret) {
			t.Fatalf("the secret reached the audit trail: %s", detail)
		}
	}
	if events[0].Action != ActionKeyCreate || events[0].TargetType != TargetAPIKey {
		t.Fatalf("unexpected audit event %+v", events[0])
	}
	if events[0].TargetID != "key_secret" {
		t.Fatalf("successful audit target=%q, want the key id", events[0].TargetID)
	}

	// A failed create is audited too, without the request body.
	h.control.createFn = func(context.Context, controlclient.CreateApiKeyRequest) (controlclient.CreateApiKeyResult, error) {
		return controlclient.CreateApiKeyResult{}, controlclient.ErrConflict
	}
	if rec := h.orgReq(http.MethodPost, "/api/v1/orgs/2/keys", `{"name":"Secret key"}`, member, true); rec.Code != http.StatusConflict {
		t.Fatalf("conflicting create = %d, want 409", rec.Code)
	}
	failures := h.audit.all()
	last := failures[len(failures)-1]
	if last.Outcome != "failure" || last.Action != ActionKeyCreate {
		t.Fatalf("failed create not audited: %+v", last)
	}
	if last.TargetID != "" || last.Error != "key_create_failed" {
		t.Fatalf("failed create audit leaked a target or raw error: %+v", last)
	}
}

// versionPtr is a small helper for the precondition-mapping case.
func versionPtr(value int64) *int64 { return &value }

func TestKeyOwnerAndOperationIDHelpers(t *testing.T) {
	if _, err := parseOptionalUserIDValue(" 7 "); err != nil {
		t.Fatalf("a trimmed positive id must parse: %v", err)
	}
	for _, raw := range []string{"0", "-1", "abc", "1.5"} {
		if _, err := parseOptionalUserIDValue(raw); err == nil {
			t.Errorf("parseOptionalUserIDValue(%q) succeeded, want an error", raw)
		}
	}
	if value, err := parseOptionalUserIDValue(""); err != nil || value != nil {
		t.Fatalf("an empty string must mean absent, got %v/%v", value, err)
	}

	// canonicalKeyAllowlist keeps the tri-state distinction.
	empty := []string{}
	canonical, err := canonicalKeyAllowlist(&empty)
	if err != nil || canonical == nil || len(*canonical) != 0 {
		t.Fatalf("empty allowlist must stay a present empty set: %v/%v", canonical, err)
	}
	duplicates := []string{"b", "a", "b"}
	canonical, err = canonicalKeyAllowlist(&duplicates)
	if err != nil || canonical == nil || strings.Join(*canonical, ",") != "a,b" {
		t.Fatalf("canonical=%v err=%v, want a,b", canonical, err)
	}
	if canonical, err := canonicalKeyAllowlist(nil); err != nil || canonical != nil {
		t.Fatalf("nil must stay nil: %v/%v", canonical, err)
	}

	// A generated operation id is bounded and prefixed.
	service := NewControlKeyService(&fakeOrgs{}, &fakeControl{}, ControlKeyServiceOptions{})
	operationID, err := service.resolveOperationID("", operationIDPrefixCreate)
	if err != nil || !strings.HasPrefix(operationID, operationIDPrefixCreate+"-") {
		t.Fatalf("operation id=%q err=%v", operationID, err)
	}
	if _, err := service.resolveOperationID("with\nnewline", operationIDPrefixCreate); !errors.Is(err, ErrKeyRequestInvalid) {
		t.Fatalf("a control character in operation_id must be refused, got %v", err)
	}
}
