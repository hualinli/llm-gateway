package console

import "github.com/cloudwego/hertz/pkg/route"

const (
	PathAdminProviders           = "/api/v1/admin/providers"
	PathAdminProvider            = "/api/v1/admin/providers/:provider_id"
	PathAdminProviderCredentials = "/api/v1/admin/provider-credentials"
	PathAdminProviderCredential  = "/api/v1/admin/provider-credentials/:credential_id"
	PathAdminRoutes              = "/api/v1/admin/routes"
)

func (s *server) registerProviderAdminRoutes(engine *route.Engine) {
	engine.GET(PathAdminProviders, s.withSession, s.withPlatformAdmin, s.listAdminProviders)
	engine.POST(PathAdminProviders, s.withSession, s.withCSRF, s.withPlatformAdmin, s.createAdminProvider)
	engine.PUT(PathAdminProvider, s.withSession, s.withCSRF, s.withPlatformAdmin, s.updateAdminProvider)
	engine.DELETE(PathAdminProvider, s.withSession, s.withCSRF, s.withPlatformAdmin, s.deleteAdminProvider)
	engine.GET(PathAdminProviderCredentials, s.withSession, s.withPlatformAdmin, s.listAdminProviderCredentials)
	engine.POST(PathAdminProviderCredentials, s.withSession, s.withCSRF, s.withPlatformAdmin, s.createAdminProviderCredential)
	engine.DELETE(PathAdminProviderCredential, s.withSession, s.withCSRF, s.withPlatformAdmin, s.revokeAdminProviderCredential)
	engine.GET(PathAdminRoutes, s.withSession, s.withPlatformAdmin, s.getAdminRoutes)
	engine.PUT(PathAdminRoutes, s.withSession, s.withCSRF, s.withPlatformAdmin, s.replaceAdminRoutes)
}
