package console

import (
	"encoding/json"
	"time"

	"llm-gateway/internal/billing"
	"llm-gateway/internal/organization"
)

type createActivationCodeRequest struct {
	EnterpriseName string  `json:"enterprise_name"`
	ExpiresAt      *string `json:"expires_at"`
}

type activationCodeDTO struct {
	ID             string  `json:"id"`
	EnterpriseName string  `json:"enterprise_name"`
	Status         string  `json:"status"`
	ExpiresAt      *string `json:"expires_at"`
	CreatedAt      string  `json:"created_at"`
}

type activationCodeCreatedDTO struct {
	activationCodeDTO
	Code string `json:"code"`
}

type activationCodeListDTO struct {
	Items      []activationCodeDTO `json:"items"`
	NextCursor *string             `json:"next_cursor"`
}

type createJoinRequestRequest struct {
	DisplayName string `json:"display_name"`
}

type decideJoinRequestRequest struct {
	Status            string          `json:"status"`
	MonthlyLimitMicro json.RawMessage `json:"monthly_limit_micro"`
}

type joinRequestDTO struct {
	ID                string  `json:"id"`
	OrganizationID    string  `json:"organization_id"`
	UserID            string  `json:"user_id"`
	Email             string  `json:"email"`
	DisplayName       string  `json:"display_name"`
	Status            string  `json:"status"`
	MonthlyLimitMicro *string `json:"monthly_limit_micro"`
	CreatedAt         string  `json:"created_at"`
	UpdatedAt         string  `json:"updated_at"`
	ReviewedBy        *string `json:"reviewed_by"`
	ReviewedAt        *string `json:"reviewed_at"`
}

type joinRequestListDTO struct {
	Items      []joinRequestDTO `json:"items"`
	NextCursor *string          `json:"next_cursor"`
}

func toActivationCodeDTO(code organization.ActivationCode, now time.Time) activationCodeDTO {
	var expiresAt *string
	if code.ExpiresAt != nil {
		value := formatTime(*code.ExpiresAt)
		expiresAt = &value
	}
	status := code.Status
	switch code.Status {
	case "used":
		status = "redeemed"
	case "active":
		if code.ExpiresAt != nil && !code.ExpiresAt.After(now) {
			status = "expired"
		}
	}
	return activationCodeDTO{
		ID:             formatID(code.ID),
		EnterpriseName: code.EnterpriseName,
		Status:         status,
		ExpiresAt:      expiresAt,
		CreatedAt:      formatTime(code.CreatedAt),
	}
}

func toJoinRequestDTO(request organization.JoinRequest) joinRequestDTO {
	var cap *string
	if request.MonthlyLimitMicro != nil {
		value := billing.FormatMicro(*request.MonthlyLimitMicro)
		cap = &value
	}
	var reviewer *string
	if request.ReviewedBy != nil {
		value := formatID(*request.ReviewedBy)
		reviewer = &value
	}
	var reviewedAt *string
	if request.ReviewedAt != nil {
		value := formatTime(*request.ReviewedAt)
		reviewedAt = &value
	}
	return joinRequestDTO{
		ID:                formatID(request.ID),
		OrganizationID:    formatID(request.OrganizationID),
		UserID:            formatID(request.UserID),
		Email:             request.Email,
		DisplayName:       request.DisplayName,
		Status:            request.Status,
		MonthlyLimitMicro: cap,
		CreatedAt:         formatTime(request.CreatedAt),
		UpdatedAt:         formatTime(request.UpdatedAt),
		ReviewedBy:        reviewer,
		ReviewedAt:        reviewedAt,
	}
}

func toJoinRequestDTOs(items []organization.JoinRequest) []joinRequestDTO {
	out := make([]joinRequestDTO, 0, len(items))
	for _, item := range items {
		out = append(out, toJoinRequestDTO(item))
	}
	return out
}
