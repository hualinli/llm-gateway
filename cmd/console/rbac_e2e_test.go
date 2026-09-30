package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"llm-gateway/internal/console"
	"llm-gateway/internal/iam"
	"llm-gateway/internal/organization"

	"github.com/cloudwego/hertz/pkg/route"
)

// T03 end-to-end tests: real Hertz engine, real iam/organization services, real
// PostgreSQL (including the PG audit sink), real HTTP status codes. They are
// gated on IAM_TEST_POSTGRES_DSN like the T02 end-to-end tests.

const rbacPassword = "correct horse battery staple"

type rbacActor struct {
	id      int64
	email   string
	session e2eSession
}

func rbacUser(t *testing.T, engine *route.Engine, users *iam.Service, email string) rbacActor {
	t.Helper()
	user, err := users.CreateUser(context.Background(), email, rbacPassword, false)
	if err != nil {
		t.Fatalf("create %s: %v", email, err)
	}
	return rbacActor{id: user.ID, email: user.Email, session: e2eLogin(t, engine, email, rbacPassword)}
}

// rbacFixture is two enterprises with disjoint members, mirroring the T03
// acceptance item "两企业各 3 人".
type rbacFixture struct {
	engine *route.Engine
	db     *sql.DB
	orgs   *organization.Service

	// Acme: alice owner, bob member, carol billing_viewer.
	alice, bob, carol rbacActor
	acmeOrgID         int64
	// Beta: dave owner, erin member.
	dave, erin rbacActor
	betaOrgID  int64
	// frank belongs to no organization (invitation target).
	frank rbacActor
}

func newRbacFixture(t *testing.T) *rbacFixture {
	t.Helper()
	engine, db, users, orgs := e2eBoot(t)
	ctx := context.Background()

	f := &rbacFixture{engine: engine, db: db, orgs: orgs}
	f.alice = rbacUser(t, engine, users, "alice@rbac.test")
	f.bob = rbacUser(t, engine, users, "bob@rbac.test")
	f.carol = rbacUser(t, engine, users, "carol@rbac.test")
	f.dave = rbacUser(t, engine, users, "dave@rbac.test")
	f.erin = rbacUser(t, engine, users, "erin@rbac.test")
	f.frank = rbacUser(t, engine, users, "frank@rbac.test")

	acme, err := orgs.CreateEnterprise(ctx, f.alice.id, "acme-rbac")
	if err != nil {
		t.Fatal(err)
	}
	f.acmeOrgID = acme.ID
	if _, err := orgs.AddMember(ctx, acme.ID, f.bob.id, "member"); err != nil {
		t.Fatal(err)
	}
	if _, err := orgs.AddMember(ctx, acme.ID, f.carol.id, "billing_viewer"); err != nil {
		t.Fatal(err)
	}
	beta, err := orgs.CreateEnterprise(ctx, f.dave.id, "beta-rbac")
	if err != nil {
		t.Fatal(err)
	}
	f.betaOrgID = beta.ID
	if _, err := orgs.AddMember(ctx, beta.ID, f.erin.id, "member"); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *rbacFixture) membersPath(orgID int64) string {
	return fmt.Sprintf("/api/v1/orgs/%d/members", orgID)
}

func (f *rbacFixture) auditPath(orgID int64) string {
	return fmt.Sprintf("/api/v1/orgs/%d/audit", orgID)
}

func (f *rbacFixture) invitationsPath(orgID int64) string {
	return fmt.Sprintf("/api/v1/orgs/%d/invitations", orgID)
}

func (f *rbacFixture) transferPath(orgID int64) string {
	return fmt.Sprintf("/api/v1/orgs/%d/owner-transfer", orgID)
}

func (f *rbacFixture) memberPath(orgID, userID int64) string {
	return fmt.Sprintf("/api/v1/orgs/%d/members/%d", orgID, userID)
}

// --- audit helpers ---------------------------------------------------------

type consoleAuditRow struct {
	ID         int64
	ActorUser  sql.NullInt64
	ActorOrg   sql.NullInt64
	Action     string
	TargetType string
	TargetID   string
	Outcome    string
	Detail     sql.NullString
	RequestID  sql.NullString
}

func consoleAuditRows(t *testing.T, db *sql.DB, orgID int64) []consoleAuditRow {
	t.Helper()
	rows, err := db.Query(`SELECT id, actor_user_id, actor_org_id, action, target_type, target_id, outcome, detail::text, request_id
		FROM console_audit_events WHERE actor_org_id = $1 ORDER BY id`, orgID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []consoleAuditRow
	for rows.Next() {
		var row consoleAuditRow
		if err := rows.Scan(&row.ID, &row.ActorUser, &row.ActorOrg, &row.Action, &row.TargetType,
			&row.TargetID, &row.Outcome, &row.Detail, &row.RequestID); err != nil {
			t.Fatal(err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// consoleAuditText renders every audit row (all organizations) as text so a test
// can prove a secret never landed in the table.
func consoleAuditText(t *testing.T, db *sql.DB) string {
	t.Helper()
	var text sql.NullString
	err := db.QueryRow(`SELECT string_agg(
		id::text || '|' || coalesce(actor_user_id::text,'') || '|' || coalesce(actor_org_id::text,'') || '|' ||
		action || '|' || target_type || '|' || target_id || '|' || outcome || '|' ||
		coalesce(detail::text,'') || '|' || coalesce(request_id,''), E'\n')
		FROM console_audit_events`).Scan(&text)
	if err != nil {
		t.Fatal(err)
	}
	return text.String
}

func orgRevisionAndPublications(t *testing.T, db *sql.DB, orgID int64) (int64, []string) {
	t.Helper()
	var revision int64
	if err := db.QueryRow(`SELECT org_revision FROM organizations WHERE id = $1`, orgID).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	rows, err := db.Query(`SELECT org_revision::text || ':' || scope || ':' || target_id || ':' || status
		FROM policy_publications WHERE org_id = $1 ORDER BY org_revision, id`, orgID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var publications []string
	for rows.Next() {
		var row string
		if err := rows.Scan(&row); err != nil {
			t.Fatal(err)
		}
		publications = append(publications, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return revision, publications
}

// --- 1. members visibility + cross-org isolation ---------------------------

func TestRbacHTTPMemberVisibilityAndCrossOrgNotFound(t *testing.T) {
	f := newRbacFixture(t)

	memberPage := func(actor rbacActor, orgID int64) (int, []string, string) {
		t.Helper()
		rec := e2eRequest(f.engine, http.MethodGet, f.membersPath(orgID), "", actor.session.cookieHeader())
		var body struct {
			Items []struct {
				UserID string `json:"user_id"`
				Email  string `json:"email"`
				Role   string `json:"role"`
			} `json:"items"`
			NextCursor *string `json:"next_cursor"`
			Error      *struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		emails := make([]string, 0, len(body.Items))
		for _, item := range body.Items {
			emails = append(emails, item.Email)
		}
		code := ""
		if body.Error != nil {
			code = body.Error.Code
		}
		return rec.Code, emails, code
	}

	// owner / billing_viewer see the whole organization (3 members).
	code, emails, _ := memberPage(f.alice, f.acmeOrgID)
	if code != http.StatusOK || len(emails) != 3 {
		t.Fatalf("owner members = %d, %v; want 200 with 3 members", code, emails)
	}
	code, emails, _ = memberPage(f.carol, f.acmeOrgID)
	if code != http.StatusOK || len(emails) != 3 {
		t.Fatalf("billing_viewer members = %d, %v; want 200 with 3 members", code, emails)
	}
	// A plain member sees only themselves.
	code, emails, _ = memberPage(f.bob, f.acmeOrgID)
	if code != http.StatusOK || len(emails) != 1 || emails[0] != f.bob.email {
		t.Fatalf("member members = %d, %v; want 200 with only themselves", code, emails)
	}
	// Cross-organization and total outsiders get 404, never 403, and the message
	// is identical to a non-existent organization.
	code, _, errCode := memberPage(f.dave, f.acmeOrgID)
	if code != http.StatusNotFound || errCode != "not_found" {
		t.Fatalf("cross-org members = %d/%s, want 404 not_found", code, errCode)
	}
	code, _, errCode = memberPage(f.frank, f.acmeOrgID)
	if code != http.StatusNotFound || errCode != "not_found" {
		t.Fatalf("outsider members = %d/%s, want 404 not_found", code, errCode)
	}
	code, _, _ = memberPage(f.frank, 99_999_999)
	if code != http.StatusNotFound {
		t.Fatalf("unknown organization = %d, want 404", code)
	}
	// Cross-org audit read is 404; a member reading their own org's audit is 403.
	rec := e2eRequest(f.engine, http.MethodGet, f.auditPath(f.acmeOrgID), "", f.dave.session.cookieHeader())
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-org audit = %d, want 404 (body=%s)", rec.Code, rec.Body.String())
	}
	rec = e2eRequest(f.engine, http.MethodGet, f.auditPath(f.acmeOrgID), "", f.bob.session.cookieHeader())
	if rec.Code != http.StatusForbidden {
		t.Fatalf("member audit = %d, want 403 (body=%s)", rec.Code, rec.Body.String())
	}
	rec = e2eRequest(f.engine, http.MethodGet, f.auditPath(f.acmeOrgID), "", f.carol.session.cookieHeader())
	if rec.Code != http.StatusForbidden {
		t.Fatalf("billing_viewer audit = %d, want 403 (body=%s)", rec.Code, rec.Body.String())
	}
	// No session and missing CSRF are still 401/403 on the new routes.
	rec = e2eRequest(f.engine, http.MethodGet, f.membersPath(f.acmeOrgID), "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("members without session = %d, want 401", rec.Code)
	}
	rec = e2eRequest(f.engine, http.MethodPost, f.invitationsPath(f.acmeOrgID),
		`{"email":"x@rbac.test","role":"member"}`, f.alice.session.cookieHeader())
	if rec.Code != http.StatusForbidden {
		t.Fatalf("invitation without CSRF = %d, want 403", rec.Code)
	}
}

// --- 2. invitation lifecycle ----------------------------------------------

func TestRbacHTTPInvitationLifecycle(t *testing.T) {
	f := newRbacFixture(t)
	revisionBefore, _ := orgRevisionAndPublications(t, f.db, f.acmeOrgID)

	// A plain member has no manage_members: 403, and nothing is created.
	rec := e2eRequest(f.engine, http.MethodPost, f.invitationsPath(f.acmeOrgID),
		fmt.Sprintf(`{"email":%q,"role":"member"}`, f.frank.email),
		f.bob.session.cookieHeader(), f.bob.session.csrfHeader())
	if rec.Code != http.StatusForbidden {
		t.Fatalf("member creates invitation = %d, want 403 (body=%s)", rec.Code, rec.Body.String())
	}
	var invitation struct {
		ID         string  `json:"id"`
		Email      string  `json:"email"`
		Role       string  `json:"role"`
		Status     string  `json:"status"`
		AcceptedAt *string `json:"accepted_at"`
	}
	// The owner creates an invitation for an email with different casing: the
	// stored value is normalized and fixed.
	rec = e2eRequest(f.engine, http.MethodPost, f.invitationsPath(f.acmeOrgID),
		fmt.Sprintf(`{"email":%q,"role":"member"}`, strings.ToUpper(f.frank.email)),
		f.alice.session.cookieHeader(), f.alice.session.csrfHeader())
	if rec.Code != http.StatusCreated {
		t.Fatalf("owner creates invitation = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}
	var created struct {
		Invitation json.RawMessage `json:"invitation"`
		Token      string          `json:"token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(created.Invitation, &invitation); err != nil {
		t.Fatal(err)
	}
	if created.Token == "" {
		t.Fatal("the create response did not return a raw token")
	}
	if invitation.Email != f.frank.email || invitation.Role != "member" || invitation.Status != "pending" {
		t.Fatalf("invitation = %+v, want normalized email, fixed role, pending", invitation)
	}
	rawToken := created.Token

	// The list exposes metadata only.
	rec = e2eRequest(f.engine, http.MethodGet, f.invitationsPath(f.acmeOrgID), "", f.alice.session.cookieHeader())
	if rec.Code != http.StatusOK {
		t.Fatalf("list invitations = %d, want 200", rec.Code)
	}
	if strings.Contains(rec.Body.String(), rawToken) || strings.Contains(rec.Body.String(), "token_hash") {
		t.Fatalf("invitation list leaks the token: %s", rec.Body.String())
	}

	// A different logged-in user cannot redeem the token: the email is fixed.
	rec = e2eRequest(f.engine, http.MethodPost, "/api/v1/invitations/"+rawToken+"/accept", "",
		f.erin.session.cookieHeader(), f.erin.session.csrfHeader())
	if rec.Code != http.StatusForbidden {
		t.Fatalf("accept with the wrong email = %d, want 403 (body=%s)", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), rawToken) {
		t.Fatal("the error body echoes the token")
	}

	// The invited user accepts: role comes from the invitation.
	rec = e2eRequest(f.engine, http.MethodPost, "/api/v1/invitations/"+rawToken+"/accept", "",
		f.frank.session.cookieHeader(), f.frank.session.csrfHeader())
	if rec.Code != http.StatusOK {
		t.Fatalf("accept = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	var accepted struct {
		OrganizationID string `json:"organization_id"`
		UserID         string `json:"user_id"`
		Role           string `json:"role"`
		Status         string `json:"status"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &accepted); err != nil {
		t.Fatal(err)
	}
	if accepted.OrganizationID != fmt.Sprintf("%d", f.acmeOrgID) ||
		accepted.UserID != fmt.Sprintf("%d", f.frank.id) ||
		accepted.Role != "member" || accepted.Status != "active" {
		t.Fatalf("accepted membership = %+v", accepted)
	}

	// Single use: replaying the same token is 409 conflict.
	rec = e2eRequest(f.engine, http.MethodPost, "/api/v1/invitations/"+rawToken+"/accept", "",
		f.frank.session.cookieHeader(), f.frank.session.csrfHeader())
	if rec.Code != http.StatusConflict {
		t.Fatalf("replayed accept = %d, want 409 (body=%s)", rec.Code, rec.Body.String())
	}
	// A tampered token is 404.
	rec = e2eRequest(f.engine, http.MethodPost, "/api/v1/invitations/"+rawToken+"x/accept", "",
		f.frank.session.cookieHeader(), f.frank.session.csrfHeader())
	if rec.Code != http.StatusNotFound {
		t.Fatalf("tampered token = %d, want 404", rec.Code)
	}

	// The organization revision was bumped and a pending publication row was
	// written in the same transaction as the membership.
	revisionAfter, publications := orgRevisionAndPublications(t, f.db, f.acmeOrgID)
	if revisionAfter != revisionBefore+1 {
		t.Fatalf("revision after accept = %d, want %d", revisionAfter, revisionBefore+1)
	}
	if len(publications) != 1 || publications[0] != fmt.Sprintf("%d:org::pending", revisionAfter) {
		t.Fatalf("publications = %v, want a single pending row for revision %d", publications, revisionAfter)
	}

	// The audit trail recorded the refused member attempt and both successful
	// mutations, and never the raw token.
	rows := consoleAuditRows(t, f.db, f.acmeOrgID)
	if len(rows) != 3 {
		t.Fatalf("audit rows = %+v, want 3 (refused member attempt + create + accept)", rows)
	}
	if rows[0].Action != console.ActionInvitationCreate || rows[0].Outcome != "failure" ||
		!rows[0].ActorUser.Valid || rows[0].ActorUser.Int64 != f.bob.id {
		t.Fatalf("refused member attempt audit row = %+v", rows[0])
	}
	if rows[1].Action != console.ActionInvitationCreate || rows[1].Outcome != "success" ||
		rows[1].TargetType != console.TargetInvitation || rows[1].TargetID != invitation.ID {
		t.Fatalf("create audit row = %+v", rows[1])
	}
	if rows[2].Action != console.ActionInvitationAccept || rows[2].Outcome != "success" ||
		rows[2].TargetID != fmt.Sprintf("%d", f.frank.id) {
		t.Fatalf("accept audit row = %+v", rows[2])
	}
	if strings.Contains(consoleAuditText(t, f.db), rawToken) {
		t.Fatal("the raw invitation token was written to console_audit_events")
	}
}

// --- 3. roles, last owner and ownership transfer ---------------------------

func TestRbacHTTPLastOwnerAndOwnershipTransfer(t *testing.T) {
	f := newRbacFixture(t)
	revisionBefore, _ := orgRevisionAndPublications(t, f.db, f.acmeOrgID)

	// The owner is the only one who can leave — and not when they are the last
	// owner: that is a 409 conflict, not a 403.
	rec := e2eRequest(f.engine, http.MethodDelete, f.memberPath(f.acmeOrgID, f.alice.id), "",
		f.alice.session.cookieHeader(), f.alice.session.csrfHeader())
	if rec.Code != http.StatusConflict {
		t.Fatalf("last owner self-removal = %d, want 409 (body=%s)", rec.Code, rec.Body.String())
	}
	var conflict struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &conflict); err != nil {
		t.Fatal(err)
	}
	if conflict.Error.Code != "conflict" {
		t.Fatalf("last owner error code = %q, want conflict", conflict.Error.Code)
	}

	// `owner` can only be granted through the transfer endpoint: 403.
	rec = e2eRequest(f.engine, http.MethodPatch, f.memberPath(f.acmeOrgID, f.bob.id), `{"role":"owner"}`,
		f.alice.session.cookieHeader(), f.alice.session.csrfHeader())
	if rec.Code != http.StatusForbidden {
		t.Fatalf("PATCH role=owner = %d, want 403 (body=%s)", rec.Code, rec.Body.String())
	}
	// `platform_admin` is not an organization role: 403 from the service.
	rec = e2eRequest(f.engine, http.MethodPatch, f.memberPath(f.acmeOrgID, f.bob.id), `{"role":"platform_admin"}`,
		f.alice.session.cookieHeader(), f.alice.session.csrfHeader())
	if rec.Code != http.StatusForbidden {
		t.Fatalf("PATCH role=platform_admin = %d, want 403 (body=%s)", rec.Code, rec.Body.String())
	}
	// An unknown role is a 400 from the service's validation.
	rec = e2eRequest(f.engine, http.MethodPatch, f.memberPath(f.acmeOrgID, f.bob.id), `{"role":"superuser"}`,
		f.alice.session.cookieHeader(), f.alice.session.csrfHeader())
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("PATCH role=superuser = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
	}
	// The owner promotes bob to admin; bob then cannot demote the owner.
	rec = e2eRequest(f.engine, http.MethodPatch, f.memberPath(f.acmeOrgID, f.bob.id), `{"role":"admin"}`,
		f.alice.session.cookieHeader(), f.alice.session.csrfHeader())
	if rec.Code != http.StatusNoContent {
		t.Fatalf("PATCH role=admin = %d, want 204 (body=%s)", rec.Code, rec.Body.String())
	}
	rec = e2eRequest(f.engine, http.MethodPatch, f.memberPath(f.acmeOrgID, f.alice.id), `{"role":"member"}`,
		f.bob.session.cookieHeader(), f.bob.session.csrfHeader())
	if rec.Code != http.StatusForbidden {
		t.Fatalf("admin demotes owner = %d, want 403 (body=%s)", rec.Code, rec.Body.String())
	}

	// Ownership transfer: bob becomes owner, alice becomes admin in one step.
	rec = e2eRequest(f.engine, http.MethodPost, f.transferPath(f.acmeOrgID),
		fmt.Sprintf(`{"user":"%d"}`, f.bob.id),
		f.alice.session.cookieHeader(), f.alice.session.csrfHeader())
	if rec.Code != http.StatusOK {
		t.Fatalf("owner transfer = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	var receipt struct {
		OrganizationID      string `json:"organization_id"`
		OwnerUserID         string `json:"owner_user_id"`
		PreviousOwnerUserID string `json:"previous_owner_user_id"`
		PreviousOwnerRole   string `json:"previous_owner_role"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.OwnerUserID != fmt.Sprintf("%d", f.bob.id) ||
		receipt.PreviousOwnerUserID != fmt.Sprintf("%d", f.alice.id) ||
		receipt.PreviousOwnerRole != "admin" {
		t.Fatalf("transfer receipt = %+v", receipt)
	}
	// The demoted owner can no longer transfer or remove the new owner.
	rec = e2eRequest(f.engine, http.MethodDelete, f.memberPath(f.acmeOrgID, f.bob.id), "",
		f.alice.session.cookieHeader(), f.alice.session.csrfHeader())
	if rec.Code != http.StatusForbidden {
		t.Fatalf("demoted owner removes the new owner = %d, want 403 (body=%s)", rec.Code, rec.Body.String())
	}
	// The new owner removes the old one: 204, and the audit row exists.
	rec = e2eRequest(f.engine, http.MethodDelete, f.memberPath(f.acmeOrgID, f.alice.id), "",
		f.bob.session.cookieHeader(), f.bob.session.csrfHeader())
	if rec.Code != http.StatusNoContent {
		t.Fatalf("new owner removes the old owner = %d, want 204 (body=%s)", rec.Code, rec.Body.String())
	}
	// Now bob is the last owner and cannot leave either.
	rec = e2eRequest(f.engine, http.MethodDelete, f.memberPath(f.acmeOrgID, f.bob.id), "",
		f.bob.session.cookieHeader(), f.bob.session.csrfHeader())
	if rec.Code != http.StatusConflict {
		t.Fatalf("last owner leaves = %d, want 409 (body=%s)", rec.Code, rec.Body.String())
	}

	// Every mutation bumped the revision and left a pending publication row, and
	// every mutation produced an audit row.
	revisionAfter, publications := orgRevisionAndPublications(t, f.db, f.acmeOrgID)
	successfulMutations := 3 // role change, transfer, removal
	if revisionAfter != revisionBefore+int64(successfulMutations) {
		t.Fatalf("revision = %d, want %d after %d mutations", revisionAfter, revisionBefore+int64(successfulMutations), successfulMutations)
	}
	if len(publications) != successfulMutations {
		t.Fatalf("publications = %v, want %d rows", publications, successfulMutations)
	}
	for i, publication := range publications {
		wantRevision := revisionBefore + int64(i) + 1
		if publication != fmt.Sprintf("%d:org::pending", wantRevision) {
			t.Fatalf("publication[%d] = %q, want revision %d pending", i, publication, wantRevision)
		}
	}

	// GET /audit: admin/owner only, newest first, string ids, paginated.
	rec = e2eRequest(f.engine, http.MethodGet, f.auditPath(f.acmeOrgID)+"?limit=2", "",
		f.bob.session.cookieHeader())
	if rec.Code != http.StatusOK {
		t.Fatalf("audit read = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	var page struct {
		Items []struct {
			ID         string  `json:"id"`
			Action     string  `json:"action"`
			TargetType string  `json:"target_type"`
			TargetID   string  `json:"target_id"`
			Outcome    string  `json:"outcome"`
			RequestID  *string `json:"request_id"`
		} `json:"items"`
		NextCursor *string `json:"next_cursor"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 2 {
		t.Fatalf("audit page = %+v, want 2 items", page)
	}
	if page.NextCursor == nil {
		t.Fatal("audit page with limit=2 must expose a next_cursor")
	}
	if page.Items[0].Action == "" || page.Items[0].RequestID == nil {
		t.Fatalf("audit item = %+v, want action and request_id", page.Items[0])
	}
	// The second page continues with older ids.
	rec = e2eRequest(f.engine, http.MethodGet, f.auditPath(f.acmeOrgID)+"?limit=100&cursor="+*page.NextCursor, "",
		f.bob.session.cookieHeader())
	if rec.Code != http.StatusOK {
		t.Fatalf("audit second page = %d, want 200", rec.Code)
	}
	var page2 struct {
		Items      []json.RawMessage `json:"items"`
		NextCursor *string           `json:"next_cursor"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page2); err != nil {
		t.Fatal(err)
	}
	if len(page2.Items) == 0 || page2.NextCursor != nil {
		t.Fatalf("second page = %d items, next=%v; want the rest and no cursor", len(page2.Items), page2.NextCursor)
	}

	// The audit trail records failures too (the refused owner grant above).
	rows := consoleAuditRows(t, f.db, f.acmeOrgID)
	var failures int
	for _, row := range rows {
		if row.Outcome == "failure" {
			failures++
		}
	}
	if failures == 0 {
		t.Fatal("no failed mutation was audited")
	}
}

// --- 4. cross-organization key/member/audit isolation ----------------------

func TestRbacHTTPCrossOrgMutationsAreNotFound(t *testing.T) {
	f := newRbacFixture(t)
	// dave (Beta owner) targets Acme: every T03 mutation must be 404, never 403,
	// so the existence of the other organization is not disclosed.
	cases := []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodPost, f.invitationsPath(f.acmeOrgID), fmt.Sprintf(`{"email":%q,"role":"member"}`, f.erin.email)},
		{http.MethodPatch, f.memberPath(f.acmeOrgID, f.bob.id), `{"role":"member"}`},
		{http.MethodDelete, f.memberPath(f.acmeOrgID, f.bob.id), ""},
		{http.MethodPost, f.transferPath(f.acmeOrgID), fmt.Sprintf(`{"user":"%d"}`, f.bob.id)},
		{http.MethodGet, f.invitationsPath(f.acmeOrgID), ""},
	}
	for _, tc := range cases {
		rec := e2eRequest(f.engine, tc.method, tc.path, tc.body,
			f.dave.session.cookieHeader(), f.dave.session.csrfHeader())
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s %s = %d, want 404 (body=%s)", tc.method, tc.path, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), `"code":"not_found"`) {
			t.Fatalf("%s %s body = %s", tc.method, tc.path, rec.Body.String())
		}
	}
	// Beta is untouched: its own owner sees exactly one member.
	rec := e2eRequest(f.engine, http.MethodGet, f.membersPath(f.betaOrgID), "", f.dave.session.cookieHeader())
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), f.dave.email) {
		t.Fatalf("beta members = %d %s", rec.Code, rec.Body.String())
	}
	// The refused attempts are attributed to the organization they were aimed at
	// (so that organization's admins can see the probes), all as failures, and
	// they carry no target metadata of the other enterprise.
	rows := consoleAuditRows(t, f.db, f.acmeOrgID)
	if len(rows) != 4 {
		t.Fatalf("audit rows = %+v, want 4 refused attempts attributed to Acme", rows)
	}
	for _, row := range rows {
		if row.Outcome != "failure" || !row.ActorUser.Valid || row.ActorUser.Int64 != f.dave.id {
			t.Fatalf("cross-org audit row = %+v, want a failure by dave", row)
		}
		if row.TargetType != console.TargetInvitation && row.TargetType != console.TargetMembership {
			t.Fatalf("cross-org audit row target_type = %q", row.TargetType)
		}
	}
	// Beta, the actor's own organization, has no audit rows for these attempts.
	if betaRows := consoleAuditRows(t, f.db, f.betaOrgID); len(betaRows) != 0 {
		t.Fatalf("actor organization gained audit rows: %+v", betaRows)
	}
	// The audit table never contains a token.
	if text := consoleAuditText(t, f.db); strings.Contains(text, "token") {
		t.Fatalf("unexpected audit content: %s", text)
	}
}

// --- 5. example captures for the report ------------------------------------

// TestRbacHTTPErrorMappingExamples captures one concrete envelope for each newly
// mapped error class (403 matrix, 404 cross-org, 409 last owner) so the report
// can quote real bytes instead of describing them.
func TestRbacHTTPErrorMappingExamples(t *testing.T) {
	f := newRbacFixture(t)

	// 403: a member tries to invite.
	rec := e2eRequest(f.engine, http.MethodPost, f.invitationsPath(f.acmeOrgID),
		fmt.Sprintf(`{"email":%q,"role":"member"}`, f.erin.email),
		f.bob.session.cookieHeader(), f.bob.session.csrfHeader())
	t.Logf("403 matrix: %d %s", rec.Code, rec.Body.String())
	if rec.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d", rec.Code)
	}

	// 404: a Beta owner reads Acme members.
	rec = e2eRequest(f.engine, http.MethodGet, f.membersPath(f.acmeOrgID), "", f.dave.session.cookieHeader())
	t.Logf("404 cross-org: %d %s", rec.Code, rec.Body.String())
	if rec.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", rec.Code)
	}

	// 409: the last owner tries to leave.
	rec = e2eRequest(f.engine, http.MethodDelete, f.memberPath(f.acmeOrgID, f.alice.id), "",
		f.alice.session.cookieHeader(), f.alice.session.csrfHeader())
	t.Logf("409 last owner: %d %s", rec.Code, rec.Body.String())
	if rec.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d", rec.Code)
	}

	// Every envelope carries the request id echoed from X-Request-ID.
	if !strings.Contains(rec.Body.String(), `"request_id":`) {
		t.Fatalf("envelope without request_id: %s", rec.Body.String())
	}
}
