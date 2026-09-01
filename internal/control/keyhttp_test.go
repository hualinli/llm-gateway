package control

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCredentialHTTPFacadeAdminLifecycle(t *testing.T) {
	srv, err := NewServer(NewMemoryStore())
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewCredentialService(srv)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewCredentialHTTPHandler(service)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := NewIdentityVerifier("admin-token", "")
	if err != nil {
		t.Fatal(err)
	}
	authenticated := IdentityAuth(handler, verifier, true)

	createRequest := httptest.NewRequest(http.MethodPost, "/keys", bytes.NewBufferString(`{"tenant_id":"tenant-a"}`))
	createRequest.Header.Set("Authorization", "Bearer admin-token")
	createRequest.Header.Set("Content-Type", "application/json")
	createResponse := httptest.NewRecorder()
	authenticated.ServeHTTP(createResponse, createRequest)
	if createResponse.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", createResponse.Code, createResponse.Body.String())
	}
	var created createdKeyResponse
	if err := json.Unmarshal(createResponse.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.RawKey == "" || created.Key.KeyID == "" || created.Key.TenantID != "tenant-a" {
		t.Fatalf("unexpected create response: %+v", created)
	}

	getRequest := httptest.NewRequest(http.MethodGet, "/keys?tenant_id=tenant-a", nil)
	getRequest.Header.Set("Authorization", "Bearer admin-token")
	getResponse := httptest.NewRecorder()
	authenticated.ServeHTTP(getResponse, getRequest)
	if getResponse.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", getResponse.Code, getResponse.Body.String())
	}
	if strings.Contains(getResponse.Body.String(), created.RawKey) || strings.Contains(getResponse.Body.String(), srv.CurrentConfig().KeyRecords[0].KeyHash) {
		t.Fatal("list response contains credential material")
	}

	deleteRequest := httptest.NewRequest(http.MethodDelete, "/keys/"+created.Key.KeyID, nil)
	deleteRequest.Header.Set("Authorization", "Bearer admin-token")
	deleteResponse := httptest.NewRecorder()
	authenticated.ServeHTTP(deleteResponse, deleteRequest)
	if deleteResponse.Code != http.StatusNoContent {
		t.Fatalf("delete status=%d body=%s", deleteResponse.Code, deleteResponse.Body.String())
	}
	if srv.CurrentConfig().KeyRecords[0].Status != "revoked" {
		t.Fatalf("status=%q, want revoked", srv.CurrentConfig().KeyRecords[0].Status)
	}
}

func TestCredentialHTTPFacadeTenantIsolation(t *testing.T) {
	srv, err := NewServer(NewMemoryStore())
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewCredentialService(srv)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewCredentialHTTPHandler(service)
	if err != nil {
		t.Fatal(err)
	}
	secret := strings.Repeat("s", 32)
	verifier, err := NewIdentityVerifier("admin-token", secret)
	if err != nil {
		t.Fatal(err)
	}
	userToken, err := signIdentityToken(secret, Principal{Subject: "user-a", Role: RoleTenantUser, TenantID: "tenant-a"}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	authenticated := IdentityAuth(handler, verifier, true)

	crossTenant := httptest.NewRequest(http.MethodPost, "/keys", bytes.NewBufferString(`{"tenant_id":"tenant-b"}`))
	crossTenant.Header.Set("Authorization", "Bearer "+userToken)
	crossResponse := httptest.NewRecorder()
	authenticated.ServeHTTP(crossResponse, crossTenant)
	if crossResponse.Code != http.StatusForbidden {
		t.Fatalf("cross-tenant create status=%d", crossResponse.Code)
	}

	ownTenant := httptest.NewRequest(http.MethodPost, "/keys", bytes.NewBufferString(`{}`))
	ownTenant.Header.Set("Authorization", "Bearer "+userToken)
	ownResponse := httptest.NewRecorder()
	authenticated.ServeHTTP(ownResponse, ownTenant)
	if ownResponse.Code != http.StatusCreated {
		t.Fatalf("own-tenant create status=%d body=%s", ownResponse.Code, ownResponse.Body.String())
	}
	crossList := httptest.NewRequest(http.MethodGet, "/keys?tenant_id=tenant-b", nil)
	crossList.Header.Set("Authorization", "Bearer "+userToken)
	crossListResponse := httptest.NewRecorder()
	authenticated.ServeHTTP(crossListResponse, crossList)
	if crossListResponse.Code != http.StatusForbidden {
		t.Fatalf("cross-tenant list status=%d", crossListResponse.Code)
	}
}

func TestCredentialHTTPFacadeRejectsMalformedRequests(t *testing.T) {
	srv, err := NewServer(NewMemoryStore())
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewCredentialService(srv)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewCredentialHTTPHandler(service)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := NewIdentityVerifier("admin-token", "")
	if err != nil {
		t.Fatal(err)
	}
	authenticated := IdentityAuth(handler, verifier, true)
	request := httptest.NewRequest(http.MethodPost, "/keys", bytes.NewBufferString(`{"tenant_id":"tenant-a","raw_key":"must-not-be-accepted"}`))
	request.Header.Set("Authorization", "Bearer admin-token")
	response := httptest.NewRecorder()
	authenticated.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("malformed request status=%d", response.Code)
	}

	missingAuth := httptest.NewRecorder()
	authenticated.ServeHTTP(missingAuth, httptest.NewRequest(http.MethodGet, "/keys", nil))
	if missingAuth.Code != http.StatusUnauthorized {
		t.Fatalf("missing auth status=%d", missingAuth.Code)
	}
}
