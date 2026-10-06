package console

import "github.com/cloudwego/hertz/pkg/route"

const (
	PathOrgQuota                = "/api/v1/orgs/:org/quota"
	PathOrgMemberQuota          = "/api/v1/orgs/:org/members/:user/quota"
	PathOrgMemberBillingSummary = "/api/v1/orgs/:org/billing/member-summary"
	PathAdminBillingSummary     = "/api/v1/admin/billing/summary"
	PathAdminBillingAccount     = "/api/v1/admin/billing/accounts/:org"
	PathAdminBillingEntries     = "/api/v1/admin/billing/accounts/:org/entries"
	PathAdminBillingExceptions  = "/api/v1/admin/billing/accounts/:org/exceptions"
	PathAdminBillingException   = "/api/v1/admin/billing/accounts/:org/exceptions/:exception"
)

// registerBillingReportRoutes installs the quota and formal billing-report
// endpoints. Own-history access is enforced by each handler; platform totals
// additionally require a platform-admin session.
func (s *server) registerBillingReportRoutes(engine *route.Engine) {
	engine.GET(PathOrgQuota, s.withSession, s.getOrgQuota)
	engine.PATCH(PathOrgMemberQuota, s.withSession, s.withCSRF, s.changeMemberQuota)
	engine.GET(PathOrgMemberBillingSummary, s.withSession, s.listOrgMemberBillingSummary)
	engine.GET(PathAdminBillingSummary, s.withSession, s.withPlatformAdmin, s.getAdminBillingSummary)
	engine.GET(PathAdminBillingAccount, s.withSession, s.withPlatformAdmin, s.getAdminBillingAccount)
	engine.GET(PathAdminBillingEntries, s.withSession, s.withPlatformAdmin, s.listAdminBillingEntries)
	engine.GET(PathAdminBillingExceptions, s.withSession, s.withPlatformAdmin, s.listAdminBillingExceptions)
	engine.GET(PathAdminBillingException, s.withSession, s.withPlatformAdmin, s.getAdminBillingException)
}
