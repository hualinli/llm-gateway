package console

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"llm-gateway/internal/controlclient"
)

type providerAdminTestKeys struct {
	KeyService
	client *fakeProviderAdminControl
}

func (s *providerAdminTestKeys) providerAdmin() providerAdminControl { return s.client }

type fakeProviderAdminControl struct {
	listProvidersResult controlclient.ProviderListResult
	upsertResult        controlclient.ProviderMutationResult
	upsertErr           error
	deleteErr           error
	routesResult        controlclient.ProviderRoutesResult
	routesErr           error
	credentialsResult   controlclient.ProviderCredentialPage
	credentialResult    controlclient.ProviderCredentialMetadata
	credentialErr       error
	secretReceived      string
	listProvidersCalls  int
	upsertCalls         int
	deleteCalls         int
	routeCalls          int
	listCredentialCalls int
	createCredCalls     int
	revokeCredCalls     int
}

func (f *fakeProviderAdminControl) ListProviders(context.Context, int64, int64, string) (controlclient.ProviderListResult, error) {
	f.listProvidersCalls++
	return f.listProvidersResult, nil
}

func (f *fakeProviderAdminControl) UpsertProvider(_ context.Context, _, _ int64, _ string, provider controlclient.ProviderConfig, _ bool) (controlclient.ProviderMutationResult, error) {
	f.upsertCalls++
	if f.upsertErr != nil {
		return controlclient.ProviderMutationResult{}, f.upsertErr
	}
	result := f.upsertResult
	if result.Provider.ID == "" {
		result.Provider = provider
	}
	return result, nil
}

func (f *fakeProviderAdminControl) DeleteProvider(context.Context, int64, int64, string, string) (int64, error) {
	f.deleteCalls++
	return 3, f.deleteErr
}

func (f *fakeProviderAdminControl) GetRoutes(context.Context, int64, int64, string) (controlclient.ProviderRoutesResult, error) {
	return f.routesResult, nil
}

func (f *fakeProviderAdminControl) ReplaceRoutes(_ context.Context, _, _ int64, _ string, routes controlclient.ProviderRouteMap, version int64) (controlclient.ProviderRoutesResult, error) {
	f.routeCalls++
	if f.routesErr != nil {
		return controlclient.ProviderRoutesResult{}, f.routesErr
	}
	return controlclient.ProviderRoutesResult{Routes: routes, Version: version + 1}, nil
}

func (f *fakeProviderAdminControl) ListProviderCredentials(context.Context, int64, int64, string, string) (controlclient.ProviderCredentialPage, error) {
	f.listCredentialCalls++
	return f.credentialsResult, nil
}

func (f *fakeProviderAdminControl) CreateProviderCredential(_ context.Context, _, _ int64, _, providerID string, secret []byte) (controlclient.ProviderCredentialMetadata, error) {
	f.createCredCalls++
	f.secretReceived = string(secret)
	result := f.credentialResult
	result.ProviderID = providerID
	return result, f.credentialErr
}

func (f *fakeProviderAdminControl) RevokeProviderCredential(context.Context, int64, int64, string, string) (controlclient.ProviderCredentialMetadata, error) {
	f.revokeCredCalls++
	return f.credentialResult, f.credentialErr
}

func TestProviderAdminRoutesRequirePlatformAdminAndRedactCredential(t *testing.T) {
	control := &fakeProviderAdminControl{
		listProvidersResult: controlclient.ProviderListResult{Version: 7, Items: []controlclient.ProviderConfig{{
			ID: "primary", Protocol: "openai-compatible", URL: "https://upstream.example/v1", Model: "gpt-test",
			Timeouts: &controlclient.ProviderTimeouts{AttemptTimeoutMS: 5000}, ModelMapping: map[string]string{"public-model": "upstream-model"},
		}}},
		credentialsResult: controlclient.ProviderCredentialPage{Items: []controlclient.ProviderCredentialMetadata{{
			CredentialID: "pcred_0123456789abcdef", CredentialRef: "control://provider-credentials/pcred_0123456789abcdef",
			ProviderID: "primary", Fingerprint: "sha256:123456789abc", Status: "active", CreatedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		}}},
		credentialResult: controlclient.ProviderCredentialMetadata{
			CredentialID: "pcred_0123456789abcdef", CredentialRef: "control://provider-credentials/pcred_0123456789abcdef",
			Fingerprint: "sha256:123456789abc", Status: "active", CreatedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		},
	}
	var keys *providerAdminTestKeys
	h := newHarness(t, func(opts *Options) {
		keys = &providerAdminTestKeys{KeyService: opts.Keys, client: control}
		opts.Keys = keys
	})
	admin := defaultFixture()
	companyAdmin := h.t03Session(91, "company-admin@example.com")
	companyAdmin.user.PlatformAdmin = false
	allowBillingReportSessions(h, admin, companyAdmin)

	list := h.orgReq(http.MethodGet, PathAdminProviders, "", admin, false)
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), `"version":7`) || strings.Contains(list.Body.String(), "secret") {
		t.Fatalf("provider list status=%d body=%s", list.Code, list.Body.String())
	}
	if control.listProvidersCalls != 1 {
		t.Fatalf("provider list calls=%d", control.listProvidersCalls)
	}
	if denied := h.orgReq(http.MethodGet, PathAdminProviders, "", companyAdmin, false); denied.Code != http.StatusForbidden {
		t.Fatalf("company administrator provider access status=%d body=%s, want 403", denied.Code, denied.Body.String())
	}
	if control.listProvidersCalls != 1 {
		t.Fatal("non-platform organization admin reached provider management")
	}

	missingCSRF := h.orgReq(http.MethodPost, PathAdminProviders, `{"id":"primary","protocol":"openai-compatible","url":"https://upstream.example"}`, admin, false)
	if missingCSRF.Code != http.StatusForbidden || control.upsertCalls != 0 {
		t.Fatalf("provider write without CSRF status=%d calls=%d", missingCSRF.Code, control.upsertCalls)
	}

	created := h.orgReq(http.MethodPost, PathAdminProviders, `{"id":"secondary","protocol":"openai-responses","url":"https://upstream.example/v1","timeouts":{"attempt_timeout_ms":0},"mock_delay_ms":0}`, admin, true)
	if created.Code != http.StatusCreated || control.upsertCalls != 1 {
		t.Fatalf("provider create status=%d body=%s calls=%d", created.Code, created.Body.String(), control.upsertCalls)
	}
	if !strings.Contains(created.Body.String(), `"provider"`) || !strings.Contains(created.Body.String(), `"version"`) {
		t.Fatalf("provider create DTO=%s", created.Body.String())
	}

	mismatch := h.orgReq(http.MethodPut, "/api/v1/admin/providers/primary", `{"id":"secondary","protocol":"openai-compatible","url":"https://upstream.example/v1"}`, admin, true)
	if mismatch.Code != http.StatusBadRequest || control.upsertCalls != 1 {
		t.Fatalf("path/body mismatch status=%d calls=%d body=%s", mismatch.Code, control.upsertCalls, mismatch.Body.String())
	}

	credentialList := h.orgReq(http.MethodGet, PathAdminProviderCredentials, "", admin, false)
	if credentialList.Code != http.StatusOK || strings.Contains(credentialList.Body.String(), "secret") || !strings.Contains(credentialList.Body.String(), "fingerprint") {
		t.Fatalf("credential list status=%d body=%s", credentialList.Code, credentialList.Body.String())
	}
	const secret = "upstream-secret-that-must-not-be-echoed"
	credentialCreate := h.orgReq(http.MethodPost, PathAdminProviderCredentials, `{"provider_id":"primary","secret":"`+secret+`"}`, admin, true)
	if credentialCreate.Code != http.StatusCreated || strings.Contains(credentialCreate.Body.String(), secret) || control.secretReceived != secret {
		t.Fatalf("credential create status=%d leaked=%v body=%s", credentialCreate.Code, strings.Contains(credentialCreate.Body.String(), secret), credentialCreate.Body.String())
	}

	control.routesErr = controlclient.ErrPreconditionFailed
	staleRoutes := h.orgReq(http.MethodPut, PathAdminRoutes, `{"routes":{"public-model":["primary"]},"expected_version":6}`, admin, true)
	if staleRoutes.Code != http.StatusConflict || control.routeCalls != 1 {
		t.Fatalf("stale routes status=%d calls=%d body=%s", staleRoutes.Code, control.routeCalls, staleRoutes.Body.String())
	}
	control.routesErr = nil
	disabled := h.orgReq(http.MethodPut, PathAdminRoutes, `{"routes":{"disabled-model":[]},"expected_version":7}`, admin, true)
	if disabled.Code != http.StatusOK || !strings.Contains(disabled.Body.String(), `"disabled-model":[]`) {
		t.Fatalf("empty route should disable the model and round trip as []: status=%d body=%s", disabled.Code, disabled.Body.String())
	}
	if len(h.logs.Bytes()) > 0 && strings.Contains(h.logs.String(), secret) {
		t.Fatal("provider secret was written to Console logs")
	}
}

func TestProviderDTOValidationAndRoutesConflict(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"uppercase id", `{"id":"Primary","protocol":"openai-compatible","url":"https://upstream.example"}`},
		{"unsupported protocol", `{"id":"primary","protocol":"anthropic","url":"https://upstream.example"}`},
		{"bad url scheme", `{"id":"primary","protocol":"openai-compatible","url":"ftp://upstream.example"}`},
		{"url query secret", `{"id":"primary","protocol":"openai-compatible","url":"https://upstream.example/v1?api_key=secret"}`},
		{"url userinfo secret", `{"id":"primary","protocol":"openai-compatible","url":"https://user:secret@upstream.example/v1"}`},
		{"negative timeout", `{"id":"primary","protocol":"openai-compatible","url":"https://upstream.example","timeouts":{"attempt_timeout_ms":-1}}`},
		{"oversized event bytes", `{"id":"primary","protocol":"openai-compatible","url":"https://upstream.example","timeouts":{"max_event_bytes":2147483648}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			control := &fakeProviderAdminControl{}
			h := newHarness(t, func(opts *Options) { opts.Keys = &providerAdminTestKeys{KeyService: opts.Keys, client: control} })
			platform := h.t03Session(42, "platform-admin@example.com")
			platform.user.PlatformAdmin = true
			rec := h.orgReq(http.MethodPost, PathAdminProviders, tc.body, platform, true)
			if rec.Code != http.StatusBadRequest || control.upsertCalls != 0 {
				t.Fatalf("status=%d calls=%d body=%s", rec.Code, control.upsertCalls, rec.Body.String())
			}
		})
	}

	control := &fakeProviderAdminControl{routesErr: errors.New("unexpected")}
	h := newHarness(t, func(opts *Options) { opts.Keys = &providerAdminTestKeys{KeyService: opts.Keys, client: control} })
	platform := h.t03Session(42, "platform-admin@example.com")
	platform.user.PlatformAdmin = true
	rec := h.orgReq(http.MethodPut, PathAdminRoutes, `{"routes":{},"expected_version":0}`, platform, true)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("unexpected route backend error status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestProviderAdminUnavailableWithoutControl(t *testing.T) {
	h := newHarness(t, func(opts *Options) { opts.Keys = nil })
	platform := h.t03Session(42, "platform-admin@example.com")
	platform.user.PlatformAdmin = true
	rec := h.orgReq(http.MethodGet, PathAdminProviders, "", platform, false)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("provider management without Control status=%d body=%s", rec.Code, rec.Body.String())
	}
}

var _ providerAdminControl = (*fakeProviderAdminControl)(nil)
