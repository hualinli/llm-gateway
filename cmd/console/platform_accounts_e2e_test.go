package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"testing"

	"llm-gateway/internal/console"
)

func TestPlatformEnterpriseOpeningRequiresAdminAndCSRF(t *testing.T) {
	engine, db, users, orgs := e2eBoot(t)
	ctx := context.Background()

	admin, err := users.CreateUser(ctx, "enterprise-platform-admin@example.com", e2eAdminPassword, true)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := users.CreateUser(ctx, "enterprise-owner@example.com", e2eVictimPass, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := orgs.EnsurePersonalOrganization(ctx, owner.ID, "Enterprise Owner"); err != nil {
		t.Fatal(err)
	}
	nonAdmin, err := users.CreateUser(ctx, "enterprise-non-admin@example.com", e2eVictimPass, false)
	if err != nil {
		t.Fatal(err)
	}
	disabledOwner, err := users.CreateUser(ctx, "enterprise-disabled-owner@example.com", e2eVictimPass, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := users.SetUserStatus(ctx, disabledOwner.ID, "disabled"); err != nil {
		t.Fatal(err)
	}

	adminSession := e2eLogin(t, engine, admin.Email, e2eAdminPassword)
	nonAdminSession := e2eLogin(t, engine, nonAdmin.Email, e2eVictimPass)

	requestBody := `{"name":"Acme","owner_email":"enterprise-owner@example.com"}`
	if rec := e2eRequest(engine, http.MethodPost, console.PathAdminOrganizations, requestBody); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous create status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := e2eRequest(engine, http.MethodPost, console.PathAdminOrganizations, requestBody,
		adminSession.cookieHeader()); rec.Code != http.StatusForbidden {
		t.Fatalf("admin create without CSRF status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := e2eRequest(engine, http.MethodPost, console.PathAdminOrganizations, requestBody,
		nonAdminSession.cookieHeader(), nonAdminSession.csrfHeader()); rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin create status=%d body=%s", rec.Code, rec.Body.String())
	}

	missingOwner := e2eRequest(engine, http.MethodPost, console.PathAdminOrganizations,
		`{"name":"Missing","owner_email":"missing-enterprise-owner@example.com"}`,
		adminSession.cookieHeader(), adminSession.csrfHeader())
	if missingOwner.Code != http.StatusNotFound {
		t.Fatalf("missing owner status=%d body=%s", missingOwner.Code, missingOwner.Body.String())
	}
	disabled := e2eRequest(engine, http.MethodPost, console.PathAdminOrganizations,
		fmt.Sprintf(`{"name":"Disabled","owner_email":%q}`, disabledOwner.Email),
		adminSession.cookieHeader(), adminSession.csrfHeader())
	if disabled.Code != http.StatusConflict {
		t.Fatalf("disabled owner status=%d body=%s", disabled.Code, disabled.Body.String())
	}
	var disabledEnterprises int
	if err := db.QueryRow(`SELECT count(*) FROM organizations WHERE type='enterprise' AND owner_user_id=$1`, disabledOwner.ID).Scan(&disabledEnterprises); err != nil {
		t.Fatal(err)
	}
	if disabledEnterprises != 0 {
		t.Fatalf("disabled owner created %d enterprises", disabledEnterprises)
	}

	created := e2eRequest(engine, http.MethodPost, console.PathAdminOrganizations,
		`{"name":"  Acme  ","owner_email":" ENTERPRISE-OWNER@EXAMPLE.COM "}`,
		adminSession.cookieHeader(), adminSession.csrfHeader())
	if created.Code != http.StatusCreated {
		t.Fatalf("create enterprise status=%d body=%s", created.Code, created.Body.String())
	}
	var response struct {
		Organization struct {
			ID     string `json:"id"`
			Type   string `json:"type"`
			Name   string `json:"name"`
			Status string `json:"status"`
		} `json:"organization"`
		MembershipID      string  `json:"membership_id"`
		Role              string  `json:"role"`
		Status            string  `json:"status"`
		MonthlyLimitMicro *string `json:"monthly_limit_micro"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Organization.ID == "" || response.Organization.Type != "enterprise" ||
		response.Organization.Name != "Acme" || response.Organization.Status != "active" ||
		response.MembershipID == "" || response.Role != "owner" || response.Status != "active" ||
		response.MonthlyLimitMicro != nil {
		t.Fatalf("created membership response=%+v", response)
	}
	orgID, err := strconv.ParseInt(response.Organization.ID, 10, 64)
	if err != nil {
		t.Fatalf("organization id %q: %v", response.Organization.ID, err)
	}

	var ownerID int64
	var orgType, orgName, orgStatus, role, membershipStatus, currency, walletStatus string
	var monthlyLimit sql.NullInt64
	var posted, reserved, version int64
	err = db.QueryRow(`SELECT o.owner_user_id, o.type, o.name, o.status, m.role, m.status, m.monthly_limit_micro,
		b.currency, b.posted_balance_micro, b.reserved_micro, b.status, b.version
		FROM organizations o JOIN memberships m ON m.org_id=o.id JOIN billing_accounts b ON b.org_id=o.id
		WHERE o.id=$1 AND m.user_id=$2`, orgID, owner.ID).Scan(&ownerID, &orgType, &orgName, &orgStatus,
		&role, &membershipStatus, &monthlyLimit, &currency, &posted, &reserved, &walletStatus, &version)
	if err != nil {
		t.Fatal(err)
	}
	if ownerID != owner.ID || orgType != "enterprise" || orgName != "Acme" || orgStatus != "active" ||
		role != "owner" || membershipStatus != "active" || monthlyLimit.Valid ||
		currency != "CNY" || posted != 0 || reserved != 0 || walletStatus != "active" || version != 1 {
		t.Fatalf("enterprise owner/wallet row=%d %s/%s %s/%s cap=%+v wallet=%s/%d/%d/%s/v%d",
			ownerID, orgType, orgName, role, membershipStatus, monthlyLimit, currency, posted, reserved, walletStatus, version)
	}

	var personalAccounts, adminMemberships int
	if err := db.QueryRow(`SELECT count(*) FROM organizations WHERE type='personal' AND owner_user_id=$1`, owner.ID).Scan(&personalAccounts); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM memberships WHERE org_id=$1 AND user_id=$2`, orgID, admin.ID).Scan(&adminMemberships); err != nil {
		t.Fatal(err)
	}
	if personalAccounts != 1 || adminMemberships != 0 {
		t.Fatalf("personal accounts=%d, platform admin enterprise memberships=%d; want 1/0", personalAccounts, adminMemberships)
	}
	var auditRows int
	if err := db.QueryRow(`SELECT count(*) FROM console_audit_events WHERE actor_user_id=$1 AND action='admin.organization.create' AND target_type='organization' AND target_id=$2 AND outcome='success'`, admin.ID, response.Organization.ID).Scan(&auditRows); err != nil {
		t.Fatal(err)
	}
	if auditRows != 1 {
		t.Fatalf("enterprise creation audit rows=%d, want 1", auditRows)
	}
}
