package console

import "github.com/cloudwego/hertz/pkg/route"

const (
	PathPlatformOrganizations      = "/api/v1/admin/organizations"
	PathPlatformOrganizationStatus = "/api/v1/admin/organizations/:org/status"
)

// registerPlatformOrganizationReadRoutes installs platform account listing
// and state-management routes. Enterprise creation is registered separately.
func (s *server) registerPlatformOrganizationReadRoutes(engine *route.Engine) {
	engine.GET(PathPlatformOrganizations, s.withSession, s.withPlatformAdmin, s.listAdminOrganizations)
	engine.POST(PathPlatformOrganizationStatus, s.withSession, s.withCSRF, s.withPlatformAdmin, s.setAdminOrganizationStatus)
}
