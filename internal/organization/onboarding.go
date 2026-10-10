package organization

import (
	"errors"
	"time"
)

// ActivationCode is the redacted, durable part of a single-use enterprise
// opening code. The raw code is returned only from CreateActivationCode.
type ActivationCode struct {
	ID             int64
	EnterpriseName string
	Status         string // active, used, revoked; expiry is derived from ExpiresAt
	ExpiresAt      *time.Time
	CreatedAt      time.Time
}

// JoinRequest is a public view of an application. It never contains key or
// billing data; reviewed_by/reviewed_at are the durable decision record.
type JoinRequest struct {
	ID                int64
	OrganizationID    int64
	UserID            int64
	Email             string
	DisplayName       string
	Status            string
	MonthlyLimitMicro *int64
	CreatedAt         time.Time
	UpdatedAt         time.Time
	ReviewedBy        *int64
	ReviewedAt        *time.Time
}

var (
	ErrActivationCodeUnavailable  = errors.New("organization: activation code is invalid, expired, or already used")
	ErrActivationExpiryInvalid    = errors.New("organization: activation code expiry must be in the future")
	ErrApplicationNameInvalid     = errors.New("organization: application display name is invalid")
	ErrApplicationDecisionInvalid = errors.New("organization: application decision must be approved or rejected")
	ErrApplicationLimitInvalid    = errors.New("organization: application monthly limit must not be negative")
	ErrApplicationPending         = errors.New("organization: an application is already pending")
	ErrApplicationAlreadyMember   = errors.New("organization: user is already a member")
	ErrApplicationDecided         = errors.New("organization: application has already been decided")
	ErrApplicationNotFound        = errors.New("organization: application not found")
	ErrApplicationStatusInvalid   = errors.New("organization: invalid application status filter")
	ErrOnboardingActorInactive    = errors.New("organization: account is not active")
	ErrApplicantInactive          = errors.New("organization: applicant account is not active")
)

const maxApplicationNameRunes = 80
