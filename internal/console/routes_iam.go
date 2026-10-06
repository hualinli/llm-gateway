package console

import (
	"github.com/cloudwego/hertz/pkg/route"
)

const (
	PathRegister   = "/api/v1/auth/register"
	PathMePassword = "/api/v1/me/password"
)

// registerIAMRoutes keeps self-service identity routes isolated from the
// organization and onboarding route tables.
func registerIAMRoutes(engine *route.Engine, s *server) {
	engine.POST(PathRegister, s.register)
	engine.POST(PathMePassword, s.withSession, s.withCSRF, s.changePassword)
}
