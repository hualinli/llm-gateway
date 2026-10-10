package gatewayguard

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"llm-gateway/internal/auth"
	"llm-gateway/internal/httperr"
	"llm-gateway/internal/observability"
)

func models(values ...string) *[]string {
	out := append([]string(nil), values...)
	return &out
}

func TestModelPolicyTriStateIntersection(t *testing.T) {
	cases := []struct {
		name   string
		global *[]string
		org    *[]string
		member *[]string
		key    *[]string
		model  string
		want   bool
	}{
		{name: "all inherit allows everything", model: "gpt-a", want: true},
		{name: "global catalog allows configured model", global: models("gpt-a"), model: "gpt-a", want: true},
		{name: "global catalog denies unconfigured model", global: models("gpt-a"), model: "gpt-b", want: false},
		{name: "org list allows member", org: models("gpt-a", "gpt-b"), model: "gpt-a", want: true},
		{name: "org list denies other", org: models("gpt-a"), model: "gpt-b", want: false},
		{name: "explicit empty org list denies all", org: models(), model: "gpt-a", want: false},
		{name: "key list narrows org", org: models("gpt-a", "gpt-b"), key: models("gpt-b"), model: "gpt-a", want: false},
		{name: "key list intersect org allows", org: models("gpt-a", "gpt-b"), key: models("gpt-b"), model: "gpt-b", want: true},
		{name: "explicit empty key list denies all", key: models(), model: "gpt-a", want: false},
		{name: "global and org are intersected", global: models("gpt-a"), org: models("gpt-b"), model: "gpt-b", want: false},
		{name: "member list narrows org", org: models("gpt-a", "gpt-b"), member: models("gpt-a"), model: "gpt-b", want: false},
		{name: "member inherit does not widen key", member: nil, key: models("gpt-a"), model: "gpt-b", want: false},
		{name: "explicit empty member list denies all", org: models("gpt-a"), member: models(), model: "gpt-a", want: false},
		{name: "three-way intersection allows", org: models("gpt-a", "gpt-b"), member: models("gpt-a", "gpt-b"), key: models("gpt-a"), model: "gpt-a", want: true},
		{name: "three-way intersection denies", org: models("gpt-a", "gpt-b"), member: models("gpt-b"), key: models("gpt-a"), model: "gpt-a", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			policy := ModelPolicy{Global: tc.global, Org: tc.org, Member: tc.member, Key: tc.key}
			if got := policy.Allows(tc.model); got != tc.want {
				t.Fatalf("Allows(%q) = %v, want %v (org=%v member=%v key=%v)",
					tc.model, got, tc.want, deref(tc.org), deref(tc.member), deref(tc.key))
			}
			restricted := tc.global != nil || tc.org != nil || tc.member != nil || tc.key != nil
			if policy.Restricted() != restricted {
				t.Fatalf("Restricted() = %v, want %v", policy.Restricted(), restricted)
			}
		})
	}
}

func deref(list *[]string) any {
	if list == nil {
		return "NULL"
	}
	return *list
}

func TestIntersectAllowlistsTriState(t *testing.T) {
	cases := []struct {
		name string
		a    *[]string
		b    *[]string
		want string
	}{
		{name: "both inherit", want: "NULL"},
		{name: "left inherits", a: nil, b: models("gpt-a"), want: "[gpt-a]"},
		{name: "right inherits", a: models("gpt-a"), b: nil, want: "[gpt-a]"},
		{name: "empty wins over list", a: models(), b: models("gpt-a"), want: "[]"},
		{name: "list loses to empty", a: models("gpt-a"), b: models(), want: "[]"},
		{name: "intersection narrows", a: models("gpt-a", "gpt-b"), b: models("gpt-b", "gpt-c"), want: "[gpt-b]"},
		{name: "disjoint becomes deny-all", a: models("gpt-a"), b: models("gpt-b"), want: "[]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := intersectAllowlists(tc.a, tc.b)
			switch tc.want {
			case "NULL":
				if got != nil {
					t.Fatalf("intersection = %v, want nil (inherit)", *got)
				}
			case "[]":
				if got == nil || len(*got) != 0 {
					t.Fatalf("intersection = %v, want explicit deny-all", deref(got))
				}
			default:
				if got == nil || fmt.Sprint(*got) != tc.want {
					t.Fatalf("intersection = %v, want %s", deref(got), tc.want)
				}
			}
		})
	}
}

func TestParseAllowlistJSONTriState(t *testing.T) {
	cases := []struct {
		raw     string
		want    string
		wantErr bool
	}{
		{raw: `["gpt-a","gpt-b"]`, want: "[gpt-a gpt-b]"},
		{raw: `[]`, want: "[]"},
		{raw: ` null `, want: "NULL"},
		{raw: ``, want: "NULL"},
		{raw: `{"not":"a list"}`, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			got, err := parseAllowlistJSON([]byte(tc.raw))
			if tc.wantErr {
				if err == nil {
					t.Fatal("malformed allowlist was accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tc.want == "NULL" {
				if got != nil {
					t.Fatalf("allowlist = %v, want nil (inherit)", *got)
				}
				return
			}
			if got == nil || fmt.Sprint(*got) != tc.want {
				t.Fatalf("allowlist = %v, want %s", deref(got), tc.want)
			}
		})
	}
}

func TestSnapshotGuardEnforcesSnapshotOnlyPolicy(t *testing.T) {
	policies := NewPolicySet()
	policies.Replace(map[int64]OrgPolicy{
		7: {OrgID: 7, ModelAllowlist: models("gpt-a", "gpt-b")},
	})
	guard := SnapshotGuard{Policies: policies}
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

	t.Run("active key without owner is admitted", func(t *testing.T) {
		permit, denial := guard.Check(context.Background(), auth.KeyRecord{
			ID: "key-1", Hash: "h", TenantID: "org:7", Status: "active", OrgID: 7,
		}, now)
		if denial.Denied() {
			t.Fatalf("denied: %+v", denial)
		}
		if permit.OrgID != 7 || permit.UserID != 0 || permit.KeyID != "key-1" {
			t.Fatalf("permit = %+v", permit)
		}
		if !permit.Model.Allows("gpt-a") || permit.Model.Allows("gpt-c") {
			t.Fatalf("org allowlist not applied: %+v", permit.Model)
		}
	})

	t.Run("revoked snapshot key is 401", func(t *testing.T) {
		_, denial := guard.Check(context.Background(), auth.KeyRecord{
			ID: "key-2", Hash: "h", TenantID: "org:7", Status: "revoked", OrgID: 7,
		}, now)
		if denial.Status != http.StatusUnauthorized || denial.Code != httperr.CodeUnauthorized {
			t.Fatalf("denial = %+v", denial)
		}
	})

	t.Run("expired snapshot key is 401", func(t *testing.T) {
		expired := now.Add(-time.Second)
		_, denial := guard.Check(context.Background(), auth.KeyRecord{
			ID: "key-3", Hash: "h", TenantID: "org:7", Status: "active", OrgID: 7, ExpiresAt: &expired,
		}, now)
		if denial.Status != http.StatusUnauthorized {
			t.Fatalf("denial = %+v", denial)
		}
	})

	t.Run("key expiring exactly now is expired", func(t *testing.T) {
		_, denial := guard.Check(context.Background(), auth.KeyRecord{
			ID: "key-4", Hash: "h", TenantID: "org:7", Status: "active", OrgID: 7, ExpiresAt: &now,
		}, now)
		if denial.Status != http.StatusUnauthorized {
			t.Fatalf("denial = %+v", denial)
		}
	})

	t.Run("future expiry is admitted and key allowlist applies", func(t *testing.T) {
		future := now.Add(time.Hour)
		permit, denial := guard.Check(context.Background(), auth.KeyRecord{
			ID: "key-5", Hash: "h", TenantID: "org:7", Status: "active", OrgID: 7,
			ExpiresAt: &future, OwnerUserID: ptr(int64(42)), ModelAllowlist: models("gpt-b"),
		}, now)
		if denial.Denied() {
			t.Fatalf("denied: %+v", denial)
		}
		if permit.UserID != 42 {
			t.Fatalf("user attribution lost: %+v", permit)
		}
		if permit.Model.Allows("gpt-a") || !permit.Model.Allows("gpt-b") {
			t.Fatalf("key allowlist not applied: %+v", permit.Model)
		}
	})
}

func ptr[T any](value T) *T { return &value }

// TestSnapshotGuardNeverTouchesPostgres proves the disabled path needs no
// database: the guard has no pool at all and still admits an active key.
func TestSnapshotGuardNeverTouchesPostgres(t *testing.T) {
	guard := SnapshotGuard{}
	permit, denial := guard.Check(context.Background(), auth.KeyRecord{
		ID: "legacy-1", Hash: "h", TenantID: "tenant-demo", Status: "active",
	}, time.Now())
	if denial.Denied() {
		t.Fatalf("disabled guard denied an active legacy key: %+v", denial)
	}
	if permit.OrgID != 0 || permit.Model.Restricted() {
		t.Fatalf("permit = %+v", permit)
	}
}

func TestMayGenerateMatchesPermissionMatrix(t *testing.T) {
	for _, role := range KnownGeneratingRoles() {
		if !MayGenerate(role) {
			t.Fatalf("role %q must generate", role)
		}
	}
	for _, role := range []string{"billing_viewer", "removed", "disabled", "platform_admin", "", "custom"} {
		if MayGenerate(role) {
			t.Fatalf("role %q must not generate", role)
		}
	}
}

func TestPolicySetOrgRate(t *testing.T) {
	set := NewPolicySet()
	rpm, burst := 600, 120
	set.Replace(map[int64]OrgPolicy{
		1: {OrgID: 1, RequestsPerMinute: &rpm, Burst: &burst},
		2: {OrgID: 2, RequestsPerMinute: &rpm},
		3: {OrgID: 3},
	})
	if gotRPM, gotBurst, ok := set.OrgRate(1); !ok || gotRPM != 600 || gotBurst != 120 {
		t.Fatalf("org 1 rate = %d/%d/%v", gotRPM, gotBurst, ok)
	}
	if gotRPM, gotBurst, ok := set.OrgRate(2); !ok || gotRPM != 600 || gotBurst != 600 {
		t.Fatalf("absent burst must default to one minute of tokens: %d/%d/%v", gotRPM, gotBurst, ok)
	}
	if _, _, ok := set.OrgRate(3); ok {
		t.Fatal("org without a published rpm must fall back to the default bucket")
	}
	if _, _, ok := set.OrgRate(99); ok {
		t.Fatal("unknown org must fall back to the default bucket")
	}
	// Lease expiry / empty snapshot revokes published policy.
	set.Replace(nil)
	if _, _, ok := set.OrgRate(1); ok {
		t.Fatal("cleared policy set still reports an org rate")
	}
	if set.ModelAllowlistFor(1) != nil {
		t.Fatal("cleared policy set still reports an org allowlist")
	}
}

func TestPolicySetReplaceCopiesInput(t *testing.T) {
	set := NewPolicySet()
	input := map[int64]OrgPolicy{1: {OrgID: 1}}
	set.Replace(input)
	delete(input, 1)
	if _, ok := set.Lookup(1); !ok {
		t.Fatal("policy set aliased the caller's map")
	}
}

// --- middleware ---

type fakeGuard struct {
	permit Permit
	denial Denial
	calls  int
	last   auth.KeyRecord
}

func (f *fakeGuard) Check(_ context.Context, key auth.KeyRecord, _ time.Time) (Permit, Denial) {
	f.calls++
	f.last = key
	return f.permit, f.denial
}

func decodeEnvelope(t *testing.T, rec *httptest.ResponseRecorder) (code, message string) {
	t.Helper()
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid error envelope %q: %v", rec.Body.String(), err)
	}
	return body.Error.Code, body.Error.Message
}

func guardRequest(t *testing.T, middleware func(http.Handler) http.Handler, record *auth.KeyRecord, body string) (*httptest.ResponseRecorder, *http.Request) {
	t.Helper()
	var captured *http.Request
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = r
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	ctx := req.Context()
	if record != nil {
		ctx = auth.WithKeyRecord(ctx, *record)
	}
	rec := httptest.NewRecorder()
	middleware(next).ServeHTTP(rec, req.WithContext(ctx))
	return rec, captured
}

func TestMiddlewareRequiresSnapshotIdentity(t *testing.T) {
	guard := &fakeGuard{permit: Permit{TenantID: "t"}}
	rec, _ := guardRequest(t, Middleware(guard, Options{}), nil, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d, want 401", rec.Code)
	}
	if guard.calls != 0 {
		t.Fatal("guard ran without a snapshot-resolved key")
	}
}

func TestMiddlewareMapsDenialsToEnvelope(t *testing.T) {
	cases := []struct {
		name    string
		denial  Denial
		status  int
		code    string
		message string
	}{
		{
			name:   "revoked or expired key",
			denial: Denial{Status: http.StatusUnauthorized, Code: httperr.CodeUnauthorized, Message: "invalid api key", Reason: "revoked"},
			status: http.StatusUnauthorized, code: httperr.CodeUnauthorized, message: "invalid api key",
		},
		{
			name:   "role or org denial",
			denial: Denial{Status: http.StatusForbidden, Code: httperr.CodeForbidden, Message: "access forbidden", Reason: "billing_viewer"},
			status: http.StatusForbidden, code: httperr.CodeForbidden, message: "access forbidden",
		},
		{
			name:   "guard unavailable",
			denial: Denial{Status: http.StatusServiceUnavailable, Code: httperr.CodeUnavailable, Message: "access guard unavailable", Reason: "timeout"},
			status: http.StatusServiceUnavailable, code: httperr.CodeUnavailable, message: "access guard unavailable",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			guard := &fakeGuard{denial: tc.denial}
			record := auth.KeyRecord{ID: "key-1", Hash: "h", TenantID: "t", Status: "active"}
			rec, captured := guardRequest(t, Middleware(guard, Options{}), &record, `{"model":"m","stream":true}`)
			if rec.Code != tc.status {
				t.Fatalf("status=%d, want %d", rec.Code, tc.status)
			}
			code, message := decodeEnvelope(t, rec)
			if code != tc.code || message != tc.message {
				t.Fatalf("envelope = %q/%q, want %q/%q", code, message, tc.code, tc.message)
			}
			if captured != nil {
				t.Fatal("denied request reached the next handler")
			}
			if strings.Contains(rec.Body.String(), tc.denial.Reason) && tc.denial.Reason != "" {
				t.Fatalf("operator-only reason leaked to the client: %s", rec.Body.String())
			}
		})
	}
}

func TestMiddlewareInjectsAttributionAndExecutionID(t *testing.T) {
	guard := &fakeGuard{permit: Permit{TenantID: "org:7", OrgID: 7, UserID: 42, MembershipID: 27, KeyID: "key-1"}}
	record := auth.KeyRecord{ID: "key-1", Hash: "h", TenantID: "org:7", Status: "active", OrgID: 7}
	rec, captured := guardRequest(t, Middleware(guard, Options{}), &record, `{"model":"m","stream":true}`)
	if rec.Code != http.StatusOK || captured == nil {
		t.Fatalf("status=%d captured=%v", rec.Code, captured != nil)
	}
	attribution, ok := AttributionFromContext(captured.Context())
	if !ok {
		t.Fatal("no attribution in context")
	}
	if attribution.OrgID != 7 || attribution.UserID != 42 || attribution.MembershipID != 27 || attribution.KeyID != "key-1" || attribution.TenantID != "org:7" {
		t.Fatalf("attribution = %+v", attribution)
	}
	if !strings.HasPrefix(attribution.ExecutionID, "exec_") || len(attribution.ExecutionID) != len("exec_")+32 {
		t.Fatalf("execution id = %q", attribution.ExecutionID)
	}
	if got := ExecutionIDFromContext(captured.Context()); got != attribution.ExecutionID {
		t.Fatalf("execution id mismatch: %q vs %q", got, attribution.ExecutionID)
	}
}

func TestMiddlewareModelPolicy(t *testing.T) {
	known := func(model string) bool { return model == "known" }
	knownDefault := func(model string) bool { return model == "known" || model == "default" }

	cases := []struct {
		name       string
		policy     ModelPolicy
		predicate  func(string) bool
		body       string
		wantStatus int
	}{
		{name: "unrestricted model passes", body: `{"model":"anything","stream":true}`, wantStatus: http.StatusOK},
		{name: "allowed model passes", policy: ModelPolicy{Key: models("known")}, body: `{"model":"known","stream":true}`, wantStatus: http.StatusOK},
		{name: "known but not allowed is 403", policy: ModelPolicy{Key: models("other")}, body: `{"model":"known","stream":true}`, wantStatus: http.StatusForbidden},
		{name: "unknown model under restriction is 404", policy: ModelPolicy{Key: models("known")}, body: `{"model":"mystery","stream":true}`, wantStatus: http.StatusNotFound},
		{name: "empty allowlist denies every known model", policy: ModelPolicy{Org: models()}, body: `{"model":"known","stream":true}`, wantStatus: http.StatusForbidden},
		{name: "empty allowlist keeps unknown model at 404", policy: ModelPolicy{Org: models()}, body: `{"model":"mystery","stream":true}`, wantStatus: http.StatusNotFound},
		{name: "missing model normalizes to default and is checked", policy: ModelPolicy{Key: models("known")}, predicate: knownDefault, body: `{"stream":true}`, wantStatus: http.StatusForbidden},
		{name: "missing model is 404 when default is unknown", policy: ModelPolicy{Key: models("known")}, body: `{"stream":true}`, wantStatus: http.StatusNotFound},
		{name: "non-streaming falls through to the proxy 400", policy: ModelPolicy{Key: models("other")}, body: `{"model":"known","stream":false}`, wantStatus: http.StatusOK},
		{name: "malformed json falls through to the proxy 400", policy: ModelPolicy{Key: models("other")}, body: `{`, wantStatus: http.StatusOK},
		{name: "oversized model name falls through to the proxy 400", policy: ModelPolicy{Key: models("other")}, body: `{"model":"` + strings.Repeat("m", maxModelBytes+1) + `","stream":true}`, wantStatus: http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			guard := &fakeGuard{permit: Permit{TenantID: "t", Model: tc.policy}}
			record := auth.KeyRecord{ID: "key-1", Hash: "h", TenantID: "t", Status: "active"}
			predicate := tc.predicate
			if predicate == nil {
				predicate = known
			}
			rec, captured := guardRequest(t, Middleware(guard, Options{KnownModel: predicate}), &record, tc.body)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status=%d, want %d (body=%s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if tc.wantStatus == http.StatusOK {
				if captured == nil {
					t.Fatal("allowed request did not reach the next handler")
				}
				// The guard must always restore the body for the proxy.
				body, err := io.ReadAll(captured.Body)
				if err != nil || string(body) != tc.body {
					t.Fatalf("body not restored: %q err=%v", body, err)
				}
			}
		})
	}
}

func TestMiddlewareGlobalCatalogDeniesBeforeNextHandler(t *testing.T) {
	guard := &fakeGuard{permit: Permit{TenantID: "t", Model: ModelPolicy{Global: models()}}}
	record := auth.KeyRecord{ID: "key-1", Hash: "h", TenantID: "t", Status: "active"}
	rec, captured := guardRequest(t, Middleware(guard, Options{KnownModel: func(model string) bool { return model == "known" }}), &record, `{"model":"known","stream":true}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("global-disabled known model status=%d body=%s, want 403", rec.Code, rec.Body.String())
	}
	if captured != nil {
		t.Fatal("globally disabled model reached downstream proxy/provider handler")
	}
}

func TestMiddlewareNilKnownModelStillDenies(t *testing.T) {
	guard := &fakeGuard{permit: Permit{TenantID: "t", Model: ModelPolicy{Key: models("a")}}}
	record := auth.KeyRecord{ID: "key-1", Hash: "h", TenantID: "t", Status: "active"}
	rec, _ := guardRequest(t, Middleware(guard, Options{}), &record, `{"model":"b","stream":true}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d, want 403", rec.Code)
	}
}

// --- PG error path without a live server ---

func TestPGGuardFailsClosedWhenPoolIsGone(t *testing.T) {
	db, err := sql.Open("pgx", "postgres://127.0.0.1:1/none")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	guard := &PGGuard{db: db, options: PGOptions{}.withDefaults(), policies: NewPolicySet()}
	permit, denial := guard.Check(context.Background(), auth.KeyRecord{
		ID: "key-1", Hash: "h", TenantID: "org:7", Status: "active", OrgID: 7,
	}, time.Now())
	if permit.OrgID != 0 {
		t.Fatalf("denied request produced a permit: %+v", permit)
	}
	if denial.Status != http.StatusServiceUnavailable || denial.Code != httperr.CodeUnavailable {
		t.Fatalf("denial = %+v", denial)
	}
}

func TestPGGuardDeniesKeyWithoutOrgAttribution(t *testing.T) {
	guard := &PGGuard{options: PGOptions{}.withDefaults(), policies: NewPolicySet()}
	_, denial := guard.Check(context.Background(), auth.KeyRecord{
		ID: "bootstrap-1", Hash: "h", TenantID: "tenant-demo", Status: "active",
	}, time.Now())
	if denial.Status != http.StatusUnauthorized {
		t.Fatalf("denial = %+v", denial)
	}
}

func TestPGGuardEvaluateDecisionTable(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	expired := now.Add(-time.Minute)
	future := now.Add(time.Hour)
	guard := &PGGuard{policies: NewPolicySet()}

	base := func() Facts {
		return Facts{
			Found: true, KeyID: "key-1", KeyStatus: "active", OrgID: 7, OrgStatus: "active",
			UserID: 42, UserKnown: true, UserStatus: "active", MembershipID: 27, MemberRole: "member", MemberStatus: "active",
		}
	}
	cases := []struct {
		name       string
		mutate     func(*Facts)
		record     auth.KeyRecord
		wantStatus int
		wantCode   string
	}{
		{name: "active member is admitted", mutate: func(*Facts) {}},
		{name: "key row missing is 401", mutate: func(f *Facts) { f.Found = false }, wantStatus: 401, wantCode: httperr.CodeUnauthorized},
		{name: "missing membership incarnation is 403", mutate: func(f *Facts) { f.MembershipID = 0 }, wantStatus: 403, wantCode: httperr.CodeForbidden},
		{name: "key revoked in pg is 401", mutate: func(f *Facts) { f.KeyStatus = "revoked" }, wantStatus: 401, wantCode: httperr.CodeUnauthorized},
		{name: "key expired in pg is 401", mutate: func(f *Facts) { f.KeyExpiresAt = &expired }, wantStatus: 401, wantCode: httperr.CodeUnauthorized},
		{name: "key expiring later is admitted", mutate: func(f *Facts) { f.KeyExpiresAt = &future }},
		{name: "suspended org is 403", mutate: func(f *Facts) { f.OrgStatus = "suspended" }, wantStatus: 403, wantCode: httperr.CodeForbidden},
		{name: "disabled user is 403", mutate: func(f *Facts) { f.UserStatus = "disabled" }, wantStatus: 403, wantCode: httperr.CodeForbidden},
		{name: "removed membership is 403", mutate: func(f *Facts) { f.MemberStatus = "removed" }, wantStatus: 403, wantCode: httperr.CodeForbidden},
		{name: "billing viewer is 403", mutate: func(f *Facts) { f.MemberRole = "billing_viewer" }, wantStatus: 403, wantCode: httperr.CodeForbidden},
		{name: "unknown role is 403", mutate: func(f *Facts) { f.MemberRole = "platform_admin" }, wantStatus: 403, wantCode: httperr.CodeForbidden},
		{name: "org admin is admitted", mutate: func(f *Facts) { f.MemberRole = "admin" }},
		{name: "org owner is admitted", mutate: func(f *Facts) { f.MemberRole = "owner" }},
		{name: "key without owner is admitted", mutate: func(f *Facts) { f.UserID, f.UserKnown, f.MemberRole, f.MemberStatus = 0, false, "", "" }},
		{name: "owner removed from org is 403", mutate: func(f *Facts) { f.UserKnown, f.MemberRole, f.MemberStatus = false, "", "" }, wantStatus: 403, wantCode: httperr.CodeForbidden},
		{name: "snapshot revoked is 401", mutate: func(*Facts) {}, record: auth.KeyRecord{Status: "revoked"}, wantStatus: 401, wantCode: httperr.CodeUnauthorized},
		{name: "snapshot expired is 401", mutate: func(*Facts) {}, record: auth.KeyRecord{Status: "active", ExpiresAt: &expired}, wantStatus: 401, wantCode: httperr.CodeUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			facts := base()
			tc.mutate(&facts)
			record := tc.record
			record.ID = "key-1"
			record.Hash = "h"
			record.OrgID = 7
			if record.Status == "" {
				record.Status = "active"
			}
			permit, denial := guard.evaluate(record, facts, now)
			if tc.wantStatus == 0 {
				if denial.Denied() {
					t.Fatalf("unexpected denial %+v", denial)
				}
				if permit.OrgID != 7 || permit.MembershipID != 27 || permit.KeyID != "key-1" {
					t.Fatalf("permit = %+v", permit)
				}
				return
			}
			if denial.Status != tc.wantStatus || denial.Code != tc.wantCode {
				t.Fatalf("denial = %+v, want %d/%s", denial, tc.wantStatus, tc.wantCode)
			}
		})
	}
}

func TestMiddlewareUsesObservabilityRequestIDInEnvelope(t *testing.T) {
	guard := &fakeGuard{denial: Denial{Status: http.StatusForbidden, Code: httperr.CodeForbidden, Message: "access forbidden"}}
	record := auth.KeyRecord{ID: "key-1", Hash: "h", TenantID: "t", Status: "active"}
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("denied request reached the proxy") })
	handler := observability.Middleware(Middleware(guard, Options{})(next))
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{}"))
	req = req.WithContext(auth.WithKeyRecord(req.Context(), record))
	req.Header.Set("X-Request-ID", "req-guard-1")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d", rec.Code)
	}
	var body struct {
		Error struct {
			Code      string `json:"code"`
			RequestID string `json:"request_id"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Code != httperr.CodeForbidden || body.Error.RequestID != "req-guard-1" {
		t.Fatalf("envelope = %+v", body.Error)
	}
}
