// Package api contains the HTTP surface: router, middleware chain, error
// envelope, and handlers.
//
// The error envelope is a stable API contract — error codes are
// machine-readable so AI agents can branch on them without parsing prose.
// See ALL_DESIGN_IMPLEMENTATION/http-api-layer-plan.md §1.
package api

import (
	"encoding/json"
	"net/http"

	"github.com/rs/zerolog/log"
)

// APIError is the top-level JSON error envelope written on every error
// response. Shape: {"error": {"code": "...", "message": "..."}}.
type APIError struct {
	Error ErrorBody `json:"error"`
}

// ErrorBody carries the machine-readable code and human-readable message.
type ErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Standard error codes. This list is a subset of the codes defined in
// http-api-layer-plan.md — Phase 1 handlers extend it as needed. Keeping
// them as named constants (not string literals) prevents typos across the
// codebase.
const (
	// Universal
	CodeInternalError        = "internal_error"
	CodeMethodNotAllowed     = "method_not_allowed"
	CodeUnsupportedMediaType = "unsupported_media_type"
	CodeRequestTooLarge      = "request_too_large"
	CodeContentTooLarge      = "content_too_large"
	CodeInvalidUTF8          = "invalid_utf8"
	CodeInvalidJSON          = "invalid_json"

	// Agent auth
	CodeMissingAgentID = "missing_agent_id"
	CodeInvalidAgentID = "invalid_agent_id"
	CodeAgentNotFound  = "agent_not_found"

	// Conversation path
	CodeInvalidConversationID = "invalid_conversation_id"
	CodeConversationNotFound  = "conversation_not_found"
	CodeNotMember             = "not_member"

	// Message send
	CodeMissingMessageID = "missing_message_id"
	CodeInvalidMessageID = "invalid_message_id"
	CodeEmptyContent     = "empty_content"
	CodeAlreadyProcessed = "already_processed"
	CodeAlreadyAborted   = "already_aborted"
	CodeSlowWriter       = "slow_writer"
	CodeLineTooLarge     = "line_too_large"

	// Conversation ops
	CodeInviteeNotFound       = "invitee_not_found"
	CodeLastMember            = "last_member"
	CodeResidentUnavailable   = "resident_agent_unavailable"

	// Generic request body
	CodeMissingField = "missing_field"
	CodeInvalidField = "invalid_field"
	CodeUnknownField = "unknown_field"
)

// WriteError serializes an APIError at the given HTTP status. It never
// returns — any marshal/write failure is logged and dropped (by the time
// we're writing a JSON error envelope, the request is already over).
func WriteError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(APIError{Error: ErrorBody{Code: code, Message: message}}); err != nil {
		log.Ctx(r.Context()).Warn().Err(err).Msg("api: write error response failed")
	}
}

// WriteJSON serializes body at the given HTTP status. Shared helper so every
// handler writes the same content-type and the same encoding settings.
func WriteJSON(w http.ResponseWriter, r *http.Request, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		log.Ctx(r.Context()).Warn().Err(err).Msg("api: write JSON response failed")
	}
}
