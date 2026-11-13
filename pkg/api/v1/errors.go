package v1

import (
	"errors"
	"fmt"
	"net/http"
)

// ErrorCode is a stable, machine-readable error identifier. Codes are part of
// the public contract: agents and clients branch on them.
type ErrorCode string

const (
	// Admission refusals (HTTP 409).
	CodeInsufficientMemory ErrorCode = "insufficient_memory"
	CodeInsufficientDisk   ErrorCode = "insufficient_disk"
	CodeMaxLeases          ErrorCode = "max_leases"
	CodeDraining           ErrorCode = "draining"

	CodeBadRequest      ErrorCode = "bad_request"       // 400
	CodeNotFound        ErrorCode = "not_found"         // 404
	CodeLeaseGone       ErrorCode = "lease_gone"        // 410: expired/released; client must mark it lost
	CodeConflict        ErrorCode = "conflict"          // 409: state conflict (e.g. op on releasing lease)
	CodeNotOwned        ErrorCode = "not_owned"         // 403: resource lacks our labels / other lease
	CodeVersionMismatch ErrorCode = "version_mismatch"  // 400: incompatible client/node versions
	CodeInternal        ErrorCode = "internal"          // 500
	CodeUnavailable     ErrorCode = "unavailable"       // 503 (e.g. docker daemon down on node)
	CodeUnreachable     ErrorCode = "unreachable"       // client-side: ssh / dial failure
	CodeTimeout         ErrorCode = "timeout"           // client-side: deadline exceeded
	CodeKindFailed      ErrorCode = "kind_failed"       // 500 from kind operations
	CodeLeaseLost       ErrorCode = "lease_lost"        // client-side: heartbeat got 404/410
	CodeNoPlacement     ErrorCode = "no_placement"      // client-side: fallback=never and all nodes failed
	CodeLocalDockerDown ErrorCode = "local_docker_down" // client-side: local fallback impossible
)

// HTTPStatus maps a code to the HTTP status used by the node API.
func (c ErrorCode) HTTPStatus() int {
	switch c {
	case CodeInsufficientMemory, CodeInsufficientDisk, CodeMaxLeases, CodeDraining, CodeConflict:
		return http.StatusConflict
	case CodeBadRequest, CodeVersionMismatch:
		return http.StatusBadRequest
	case CodeNotFound:
		return http.StatusNotFound
	case CodeLeaseGone:
		return http.StatusGone
	case CodeNotOwned:
		return http.StatusForbidden
	case CodeUnavailable:
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

// IsRefusal reports whether the code is a capacity/admission refusal, i.e.
// the node is healthy but declined the work. Clients fall back on refusals.
func (c ErrorCode) IsRefusal() bool {
	switch c {
	case CodeInsufficientMemory, CodeInsufficientDisk, CodeMaxLeases, CodeDraining:
		return true
	}
	return false
}

// Error is the body of every error. It implements the error interface so it
// can travel through Go code unchanged.
//
// Message says what happened and why; Hint says what to do next (brief §0.6).
type Error struct {
	Code    ErrorCode      `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
	Hint    string         `json:"hint,omitempty"`
}

func (e *Error) Error() string {
	s := string(e.Code) + ": " + e.Message
	if e.Hint != "" {
		s += " (hint: " + e.Hint + ")"
	}
	return s
}

// ErrorResponse is the JSON envelope for all non-2xx node API responses.
type ErrorResponse struct {
	Error *Error `json:"error"`
}

// Errorf builds an *Error.
func Errorf(code ErrorCode, hint string, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...), Hint: hint}
}

// WithDetails returns e with details merged in.
func (e *Error) WithDetails(kv map[string]any) *Error {
	if e.Details == nil {
		e.Details = map[string]any{}
	}
	for k, v := range kv {
		e.Details[k] = v
	}
	return e
}

// AsError extracts an *Error from err, if any.
func AsError(err error) (*Error, bool) {
	var e *Error
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}

// CodeOf returns the code of err, or CodeInternal when err is not an *Error.
func CodeOf(err error) ErrorCode {
	if e, ok := AsError(err); ok {
		return e.Code
	}
	return CodeInternal
}
