package usersignup

import (
	"time"

	"github.com/pocket-id/pocket-id/backend/internal/model"
	datatype "github.com/pocket-id/pocket-id/backend/internal/model/types"
)

// SignupToken is a single- or limited-use token that grants the ability to self-register
type SignupToken struct {
	model.Base

	Token      string            `json:"token"`
	ExpiresAt  datatype.DateTime `json:"expiresAt" sortable:"true"`
	UsageLimit int               `json:"usageLimit" sortable:"true"`
	UsageCount int               `json:"usageCount" sortable:"true"`
	UserGroups []model.UserGroup `json:"userGroups"`
}

// signupAttemptStatus is the lifecycle stage of one accepted token usage
type signupAttemptStatus string

const (
	// signupAttemptPending means the token use is occupied but no committed user has been confirmed yet
	signupAttemptPending signupAttemptStatus = "pending"
	// signupAttemptCompleted means the user created by this attempt has been confirmed in the database
	signupAttemptCompleted signupAttemptStatus = "completed"
)

// signupTokenAttempt is one accepted usage of a signup token.
// Its ID is the stable attempt identifier and doubles as the ID of the user this attempt creates, so the occupied quota slot is tied to exactly one database user.
type signupTokenAttempt struct {
	ID        string              `json:"id"`
	Status    signupAttemptStatus `json:"status"`
	CreatedAt time.Time           `json:"createdAt"`
}
