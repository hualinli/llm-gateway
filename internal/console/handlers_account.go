package console

import (
	"context"
	"net/http"
	"strings"

	"github.com/cloudwego/hertz/pkg/app"
)

type registerRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type changePasswordRequest struct {
	OldPassword string `json:"old_password"`
	NewPassword string `json:"new_password"`
}

// register implements POST /api/v1/auth/register. IAM provisions the personal
// organization, owner membership, wallet, and first session atomically.
func (s *server) register(ctx context.Context, c *app.RequestContext) {
	var req registerRequest
	if !s.decodeJSON(c, &req) {
		return
	}
	email := strings.TrimSpace(req.Email)
	if email == "" || req.Password == "" {
		s.writeError(c, http.StatusBadRequest, "", "email and password are required")
		return
	}

	user, issued, err := s.iam.Register(ctx, email, req.Password, c.ClientIP())
	if err != nil {
		s.writeIAMError(c, "register", err)
		return
	}
	orgs, _, err := s.orgs.ListForUser(ctx, user.ID, s.loginOrgPageSize, 0)
	if err != nil {
		s.revokeAfterFailedLogin(ctx, issued.Token)
		s.writeInternal(c, "list organizations after registration", err)
		return
	}
	s.setSessionCookies(c, issued.Token, issued.CSRFToken)
	writeJSON(c, http.StatusCreated, loginResponse{
		User:          toUserDTO(user),
		Organizations: toMembershipDTOs(orgs),
		CSRFToken:     issued.CSRFToken,
	})
}

// changePassword implements POST /api/v1/me/password. The service changes the
// password and revokes every browser session in one transaction; this request's
// cookies are cleared after that transaction commits.
func (s *server) changePassword(ctx context.Context, c *app.RequestContext) {
	auth, ok := authFrom(c)
	if !ok {
		s.writeError(c, http.StatusUnauthorized, "", "authentication required")
		return
	}
	var req changePasswordRequest
	if !s.decodeJSON(c, &req) {
		return
	}
	if req.OldPassword == "" || req.NewPassword == "" {
		s.writeError(c, http.StatusBadRequest, "", "old_password and new_password are required")
		return
	}
	if req.OldPassword == req.NewPassword {
		s.writeError(c, http.StatusBadRequest, "", "new password must differ from old password")
		return
	}
	if _, err := s.iam.ChangePassword(ctx, auth.User.ID, req.OldPassword, req.NewPassword); err != nil {
		s.writeIAMError(c, "change password", err)
		return
	}
	s.clearSessionCookies(c)
	c.SetStatusCode(http.StatusNoContent)
}
