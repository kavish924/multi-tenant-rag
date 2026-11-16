// Package httpx holds HTTP helpers shared by handlers: the JSON error
// envelope and strict JSON request decoding.
package httpx

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// MaxBodyBytes bounds JSON request bodies.
const MaxBodyBytes = 1 << 20

// ErrorBody is the envelope {"error": {"code", "message"}}.
type ErrorBody struct {
	Error ErrorDetail `json:"error"`
}

// ErrorDetail is the inner error object.
type ErrorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// JSON writes v with the given status.
func JSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// Error writes the JSON error envelope.
func Error(w http.ResponseWriter, status int, code, message string) {
	JSON(w, status, ErrorBody{Error: ErrorDetail{Code: code, Message: message}})
}

// Common error responses.
func Unauthorized(w http.ResponseWriter) {
	Error(w, http.StatusUnauthorized, "unauthenticated", "missing, invalid or expired token")
}

// NotFound hides whether a resource exists (TEN-AC-003).
func NotFound(w http.ResponseWriter) {
	Error(w, http.StatusNotFound, "not_found", "not found")
}

// Forbidden is returned when the caller's role is too low (TEN-AC-004).
func Forbidden(w http.ResponseWriter) {
	Error(w, http.StatusForbidden, "forbidden", "insufficient role")
}

// Internal hides error details from clients; callers log them.
func Internal(w http.ResponseWriter) {
	Error(w, http.StatusInternalServerError, "internal", "internal error")
}

// Decode reads exactly one JSON object into v, rejecting unknown fields and
// bodies over MaxBodyBytes. On failure it writes a 400 and returns false.
func Decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, MaxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			Error(w, http.StatusRequestEntityTooLarge, "body_too_large", "request body too large")
			return false
		}
		Error(w, http.StatusBadRequest, "invalid_json", fmt.Sprintf("invalid request body: %v", err))
		return false
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		Error(w, http.StatusBadRequest, "invalid_json", "request body must be a single JSON object")
		return false
	}
	return true
}
