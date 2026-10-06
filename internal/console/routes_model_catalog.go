package console

import "github.com/cloudwego/hertz/pkg/route"

const (
	PathAdminModels             = "/api/v1/admin/models"
	PathAdminOrganizationModels = "/api/v1/admin/organizations/:org/models"
	PathOrganizationModels      = "/api/v1/orgs/:org/models"
)

func (s *server) registerModelCatalogRoutes(engine *route.Engine) {
	engine.GET(PathAdminModels, s.withSession, s.withPlatformAdmin, s.listAdminModels)
	engine.PUT(PathAdminModels, s.withSession, s.withCSRF, s.withPlatformAdmin, s.replaceAdminModels)
	engine.GET(PathAdminOrganizationModels, s.withSession, s.withPlatformAdmin, s.getAdminOrganizationModels)
	engine.PUT(PathAdminOrganizationModels, s.withSession, s.withCSRF, s.withPlatformAdmin, s.setAdminOrganizationModels)
	engine.GET(PathOrganizationModels, s.withSession, s.getOrganizationModels)
}
