// Package apperror defines basic application errors and their HTTP rendering.
package apperror

import (
	"encoding/json"
	"errors"
	"net/http"
)

// Error is an error with a stable machine-readable code and an HTTP status.
type Error struct {
	Status  int
	Code    string
	Message string
	Err     error
}

func (e *Error) Error() string {
	if e.Err != nil {
		return e.Code + ": " + e.Message + ": " + e.Err.Error()
	}
	return e.Code + ": " + e.Message
}

// Unwrap returns the underlying cause.
func (e *Error) Unwrap() error { return e.Err }

// New creates an Error.
func New(status int, code, message string) *Error {
	return &Error{Status: status, Code: code, Message: message}
}

// Wrap attaches a cause to an Error.
func Wrap(status int, code, message string, cause error) *Error {
	return &Error{Status: status, Code: code, Message: message, Err: cause}
}

type body struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// Write renders err as a JSON error response. Unknown errors become a generic
// 500 so internal details are never leaked to clients.
func Write(w http.ResponseWriter, err error) {
	var ae *Error
	if !errors.As(err, &ae) {
		ae = New(http.StatusInternalServerError, "internal_error", "internal server error")
	}
	var b body
	b.Error.Code = ae.Code
	b.Error.Message = ae.Message
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(ae.Status)
	_ = json.NewEncoder(w).Encode(b)
}
