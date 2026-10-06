package console

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"llm-gateway/internal/organization"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/route"
)

const PathAdminOrganizations = "/api/v1/admin/organizations"

type enterpriseOpeningService interface {
	CreateEnterpriseForEmail(context.Context, string, string) (organization.MemberOrg, error)
}

type createEnterpriseForEmailRequest struct {
	Name       string `json:"name"`
	OwnerEmail string `json:"owner_email"`
}

func (s *server) registerPlatformAccountRoutes(engine *route.Engine) {
	engine.POST(PathAdminOrganizations, s.withSession, s.withCSRF, s.withPlatformAdmin, s.createAdminOrganization)
}

func (s *server) createAdminOrganization(ctx context.Context, c *app.RequestContext) {
	if _, ok := authFrom(c); !ok {
		s.writeError(c, http.StatusUnauthorized, "", "authentication required")
		return
	}
	var req createEnterpriseForEmailRequest
	if !s.decodeJSON(c, &req) {
		return
	}
	name, ownerEmail := strings.TrimSpace(req.Name), strings.TrimSpace(req.OwnerEmail)
	if name == "" {
		s.writeError(c, http.StatusBadRequest, "", "name is required")
		return
	}
	if ownerEmail == "" {
		s.writeError(c, http.StatusBadRequest, "", "owner_email is required")
		return
	}
	service, ok := s.orgs.(enterpriseOpeningService)
	if !ok {
		s.writeError(c, http.StatusServiceUnavailable, "", "enterprise account opening is not configured")
		return
	}

	created, err := service.CreateEnterpriseForEmail(ctx, ownerEmail, name)
	if err != nil {
		event := s.auditFrom(c, "admin.organization.create", "failure", err.Error())
		event.TargetType = "organization"
		event.TargetEmail = strings.ToLower(ownerEmail)
		event.Detail = map[string]any{"name": name, "owner_email": strings.ToLower(ownerEmail)}
		s.audit.Record(ctx, event)
		s.writeEnterpriseOpeningError(c, "create enterprise organization", err)
		return
	}
	event := s.auditFrom(c, "admin.organization.create", "success", "")
	event.TargetType = "organization"
	event.TargetID = formatID(created.Organization.ID)
	event.TargetEmail = strings.ToLower(ownerEmail)
	event.Detail = map[string]any{"name": created.Organization.Name, "owner_email": strings.ToLower(ownerEmail)}
	s.audit.Record(ctx, event)
	writeJSON(c, http.StatusCreated, toMembershipDTO(created))
}

func (s *server) writeEnterpriseOpeningError(c *app.RequestContext, operation string, err error) {
	switch {
	case errors.Is(err, organization.ErrNameInvalid), errors.Is(err, organization.ErrEnterpriseOwnerEmailInvalid):
		s.writeError(c, http.StatusBadRequest, "", err.Error())
	case errors.Is(err, organization.ErrNotFound):
		s.writeError(c, http.StatusNotFound, "", "owner account not found")
	case errors.Is(err, organization.ErrEnterpriseOwnerInactive):
		s.writeError(c, http.StatusConflict, "", "owner account must be active")
	default:
		s.writeInternal(c, operation, err)
	}
}
