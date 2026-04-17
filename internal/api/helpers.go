package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"agentmail/internal/store"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// decodeJSON is the shared request-body parser for every JSON endpoint. It
// applies a MaxBytesReader cap, rejects unknown fields, and maps Go's decoder
// errors onto the stable error-code registry defined in errors.go.
//
// On failure it writes an envelope and returns (_, false); callers must return
// immediately. On success (_, true).
func decodeJSON[T any](w http.ResponseWriter, r *http.Request, maxBytes int64) (T, bool) {
	var v T
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)

	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	if err := dec.Decode(&v); err != nil {
		var syntaxErr *json.SyntaxError
		var typeErr *json.UnmarshalTypeError
		var maxBytesErr *http.MaxBytesError

		switch {
		case errors.As(err, &syntaxErr):
			WriteError(w, r, http.StatusBadRequest, CodeInvalidJSON,
				fmt.Sprintf("malformed JSON at position %d", syntaxErr.Offset))
		case errors.As(err, &typeErr):
			WriteError(w, r, http.StatusBadRequest, CodeInvalidField,
				fmt.Sprintf("field %q has wrong type: expected %s", typeErr.Field, typeErr.Type))
		case errors.As(err, &maxBytesErr):
			WriteError(w, r, http.StatusRequestEntityTooLarge, CodeRequestTooLarge,
				fmt.Sprintf("request body exceeds %d bytes", maxBytes))
		case strings.HasPrefix(err.Error(), "json: unknown field"):
			field := strings.TrimPrefix(err.Error(), "json: unknown field ")
			WriteError(w, r, http.StatusBadRequest, CodeUnknownField,
				fmt.Sprintf("unknown field: %s", field))
		case errors.Is(err, io.EOF):
			WriteError(w, r, http.StatusBadRequest, CodeEmptyBody, "request body is empty")
		default:
			WriteError(w, r, http.StatusBadRequest, CodeInvalidJSON,
				fmt.Sprintf("invalid JSON: %s", err.Error()))
		}
		return v, false
	}

	if dec.More() {
		WriteError(w, r, http.StatusBadRequest, CodeInvalidJSON, "unexpected content after JSON object")
		return v, false
	}

	return v, true
}

// validateContentType matches the incoming Content-Type (stripped of params)
// against expected. Returns false and writes a 415 envelope on mismatch.
func validateContentType(w http.ResponseWriter, r *http.Request, expected string) bool {
	ct := r.Header.Get("Content-Type")
	if ct == "" {
		WriteError(w, r, http.StatusUnsupportedMediaType, CodeUnsupportedMediaType,
			"Content-Type header is required")
		return false
	}
	mt, _, err := mime.ParseMediaType(ct)
	if err != nil {
		WriteError(w, r, http.StatusUnsupportedMediaType, CodeUnsupportedMediaType,
			"malformed Content-Type header")
		return false
	}
	if mt != expected {
		WriteError(w, r, http.StatusUnsupportedMediaType, CodeUnsupportedMediaType,
			fmt.Sprintf("expected %s, got %s", expected, mt))
		return false
	}
	return true
}

// validateNDJSONContentType accepts either application/x-ndjson (IANA) or
// application/ndjson (widespread in the wild).
func validateNDJSONContentType(w http.ResponseWriter, r *http.Request) bool {
	ct := r.Header.Get("Content-Type")
	mt, _, _ := mime.ParseMediaType(ct)
	if mt != "application/x-ndjson" && mt != "application/ndjson" {
		WriteError(w, r, http.StatusUnsupportedMediaType, CodeUnsupportedMediaType,
			fmt.Sprintf("expected application/x-ndjson, got %q; "+
				"for complete messages, use POST /conversations/{cid}/messages with application/json", mt))
		return false
	}
	return true
}

// parseConversationID extracts and validates the {cid} path parameter.
func parseConversationID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	s := chi.URLParam(r, "cid")
	cid, err := uuid.Parse(s)
	if err != nil {
		WriteError(w, r, http.StatusBadRequest, CodeInvalidConversationID,
			"conversation_id must be a valid UUID")
		return uuid.Nil, false
	}
	return cid, true
}

// requireMembership is the shared gate for every authenticated conversation
// endpoint. It parses {cid}, confirms the conversation exists, and verifies
// the caller is a current member. On failure it writes the envelope and
// returns (_, _, false).
func (h *Handler) requireMembership(w http.ResponseWriter, r *http.Request) (uuid.UUID, uuid.UUID, bool) {
	agentID := AgentIDFromContext(r.Context())
	convID, ok := parseConversationID(w, r)
	if !ok {
		return uuid.Nil, uuid.Nil, false
	}

	exists, err := h.meta.ConversationExists(r.Context(), convID)
	if err != nil {
		WriteError(w, r, http.StatusInternalServerError, CodeInternalError, "internal server error")
		return uuid.Nil, uuid.Nil, false
	}
	if !exists {
		WriteError(w, r, http.StatusNotFound, CodeConversationNotFound,
			fmt.Sprintf("conversation %s not found", convID))
		return uuid.Nil, uuid.Nil, false
	}

	isMember, err := h.meta.IsMember(r.Context(), agentID, convID)
	if err != nil {
		WriteError(w, r, http.StatusInternalServerError, CodeInternalError, "internal server error")
		return uuid.Nil, uuid.Nil, false
	}
	if !isMember {
		WriteError(w, r, http.StatusForbidden, CodeNotMember,
			fmt.Sprintf("agent %s is not a member of conversation %s", agentID, convID))
		return uuid.Nil, uuid.Nil, false
	}

	return agentID, convID, true
}

// parseUint64Query parses a uint64 query parameter. If absent returns
// (0, true, true). If present and valid returns (v, true, true). If present
// and malformed writes a 400 with badCode and returns (_, _, false).
func parseUint64Query(w http.ResponseWriter, r *http.Request, name, badCode string) (value uint64, present bool, ok bool) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return 0, false, true
	}
	v, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		WriteError(w, r, http.StatusBadRequest, badCode,
			fmt.Sprintf("query parameter %q must be a non-negative integer", name))
		return 0, true, false
	}
	return v, true, true
}

// parseLimitQuery parses ?limit=N, clamping to [1, max]. If absent returns
// def. On malformed / out-of-range writes 400 CodeInvalidLimit.
func parseLimitQuery(w http.ResponseWriter, r *http.Request, def, max int) (int, bool) {
	raw := r.URL.Query().Get("limit")
	if raw == "" {
		return def, true
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v < 1 || v > max {
		WriteError(w, r, http.StatusBadRequest, CodeInvalidLimit,
			fmt.Sprintf("limit must be an integer in [1, %d]", max))
		return 0, false
	}
	return v, true
}

// isUUIDv7 reports whether id is a UUIDv7 (version 7 per the 4-bit version
// field). Used on the message_id write path — clients MUST generate v7 so
// seq ordering tracks wall-clock and retries reuse the same id.
func isUUIDv7(id uuid.UUID) bool {
	return id.Version() == 7
}

// mapStoreErr translates common store sentinels into HTTP envelopes. Returns
// true if a response was written. Handlers call this for the shared sentinels
// only; specific errors (ErrLastMember, ErrAlreadyAborted) are handled inline
// where the mapping differs by endpoint.
func mapStoreErr(w http.ResponseWriter, r *http.Request, err error) bool {
	switch {
	case errors.Is(err, store.ErrConversationNotFound):
		WriteError(w, r, http.StatusNotFound, CodeConversationNotFound, "conversation not found")
	case errors.Is(err, store.ErrAgentNotFound):
		WriteError(w, r, http.StatusNotFound, CodeAgentNotFound, "agent not found")
	default:
		return false
	}
	return true
}
