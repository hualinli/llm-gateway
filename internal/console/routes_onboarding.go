package console

import "github.com/cloudwego/hertz/pkg/route"

const (
	PathOrganizationsActivate  = "/api/v1/organizations/activate"
	PathAdminActivationCodes   = "/api/v1/admin/activation-codes"
	PathMeJoinRequests         = "/api/v1/me/join-requests"
	PathOrgJoinRequests        = "/api/v1/orgs/:org/join-requests"
	PathOrgJoinRequestDecision = "/api/v1/orgs/:org/join-requests/:request/decision"
)

// registerOnboardingRoutes keeps the onboarding endpoints together. The
// organization's concrete implementation is an optional extension to the
// older OrganizationService interface, so downstream test fakes and unrelated
// consumers do not need to implement the new operations.
func (s *server) registerOnboardingRoutes(engine *route.Engine) {
	engine.GET(PathAdminActivationCodes, s.withSession, s.withPlatformAdmin, s.listActivationCodes)
	engine.POST(PathAdminActivationCodes, s.withSession, s.withCSRF, s.withPlatformAdmin, s.createActivationCode)
	engine.POST(PathOrganizationsActivate, s.withSession, s.withCSRF, s.redeemActivationCode)
	engine.GET(PathMeJoinRequests, s.withSession, s.listMyJoinRequests)
	engine.POST(PathOrgJoinRequests, s.withSession, s.withCSRF, s.createJoinRequest)
	engine.GET(PathOrgJoinRequests, s.withSession, s.listOrgJoinRequests)
	engine.POST(PathOrgJoinRequestDecision, s.withSession, s.withCSRF, s.decideJoinRequest)
}
