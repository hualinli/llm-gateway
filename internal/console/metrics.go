package console

import (
	"context"
	"time"

	"llm-gateway/internal/httperr"
	"llm-gateway/internal/metrics"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
)

// route label vocabulary for console_entry_requests_total. It is derived from
// the matched route pattern (RequestContext.FullPath), never from the raw path:
// a raw path carries unbounded user input and would explode Prometheus
// cardinality. Unknown or unmatched paths collapse into RouteOther.
const (
	RouteAuthLogin             = "auth_login"
	RouteAuthLogout            = "auth_logout"
	RouteMe                    = "me"
	RouteOrganizationsList     = "organizations_list"
	RouteOrganizationsGet      = "organizations_get"
	RouteOrganizationsCreate   = "organizations_create"
	RouteAdminUsersCreate      = "admin_users_create"
	RouteAdminUsersSetStatus   = "admin_users_status"
	RouteOrgMembersList        = "org_members_list"
	RouteOrgMemberUpdate       = "org_member_update"
	RouteOrgMemberDelete       = "org_member_delete"
	RouteOrgInvitationsList    = "org_invitations_list"
	RouteOrgInvitationsNew     = "org_invitations_create"
	RouteInvitationsAccept     = "invitations_accept"
	RouteOrgOwnerTransfer      = "org_owner_transfer"
	RouteOrgAuditList          = "org_audit_list"
	RouteOrgBillingStatement   = "org_billing_statement"
	RouteOrgBillingExceptions  = "org_billing_exceptions"
	RouteOrgBillingException   = "org_billing_exception"
	RouteAdminExceptionResolve = "admin_billing_exception_resolution"
	RouteHealth                = "health"
	RouteMetrics               = "metrics"
	RouteOther                 = "other"
)

// RouteClass maps a matched Hertz route pattern plus the request method to the
// bounded `route` label.
//
// The method is part of the mapping because two different operations share one
// path pattern: GET /api/v1/organizations lists, POST /api/v1/organizations
// creates. Collapsing them would make the route label lie about which handler
// produced the result class.
func RouteClass(method, fullPath string) string {
	switch fullPath {
	case PathLogin:
		return RouteAuthLogin
	case PathLogout:
		return RouteAuthLogout
	case PathMe:
		return RouteMe
	case PathOrganizations:
		if method == string(consts.MethodPost) {
			return RouteOrganizationsCreate
		}
		return RouteOrganizationsList
	case PathOrganization:
		return RouteOrganizationsGet
	case PathAdminUsers:
		return RouteAdminUsersCreate
	case PathAdminUserStatus:
		return RouteAdminUsersSetStatus
	case PathOrgMembers:
		return RouteOrgMembersList
	case PathOrgMember:
		if method == string(consts.MethodPatch) {
			return RouteOrgMemberUpdate
		}
		return RouteOrgMemberDelete
	case PathOrgInvitations:
		if method == string(consts.MethodPost) {
			return RouteOrgInvitationsNew
		}
		return RouteOrgInvitationsList
	case PathInvitationAccept:
		return RouteInvitationsAccept
	case PathOrgOwnerTransfer:
		return RouteOrgOwnerTransfer
	case PathOrgAudit:
		return RouteOrgAuditList
	case PathOrgBillingStatements:
		return RouteOrgBillingStatement
	case PathOrgBillingExceptions:
		return RouteOrgBillingExceptions
	case PathOrgBillingException:
		return RouteOrgBillingException
	case PathAdminBillingExceptionResolution:
		return RouteAdminExceptionResolve
	case PathLivez, PathReadyz:
		return RouteHealth
	case PathMetrics:
		return RouteMetrics
	default:
		return RouteOther
	}
}

// accessMetrics records exactly one console_entry_requests_total sample per
// request that reaches the entry chain, plus one access log line.
//
// The sample is taken in a defer so a panic is still counted, mirroring
// observability.EntryMetrics: the class comes from httperr.Class(status), a
// panicking handler is forced to internal_error, and the panic is re-thrown for
// the recovery middleware to render.
func (s *server) accessMetrics() app.HandlerFunc {
	return func(ctx context.Context, c *app.RequestContext) {
		start := time.Now()
		defer func() {
			rec := recover()
			route := RouteClass(string(c.Method()), c.FullPath())
			status := c.Response.StatusCode()
			class := httperr.Class(status)
			if rec != nil {
				class = httperr.ClassInternalError
			}
			metrics.ConsoleEntryRequestsTotal.WithLabelValues(route, class).Inc()
			s.logger.Printf("console request method=%s route=%s status=%d class=%s duration_ms=%.2f bytes=%d request_id=%s",
				string(c.Method()), route, status, class,
				float64(time.Since(start).Microseconds())/1000.0, len(c.Response.Body()), requestIDFrom(c))
			if rec != nil {
				panic(rec)
			}
		}()
		c.Next(ctx)
	}
}
