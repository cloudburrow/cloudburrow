// Package apierror maps internal failures to Google-style errors, producing a
// consistent gRPC status and JSON error body from a single cause.
//
// One cause, two renderings: a caller using gRPC and a caller using the JSON
// API must be told the same thing. Deriving both from one value is what stops
// them drifting apart.
package apierror

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Error is a Google-style API error.
type Error struct {
	// Code is the canonical gRPC code. The HTTP status is derived from it, not
	// stored separately, so the two cannot disagree.
	Code codes.Code
	// Message is the human-readable message.
	Message string
	// Reason is a short, stable, machine-readable token such as "notFound".
	Reason string
	// Cause is the underlying error, if any.
	Cause error
}

func (e *Error) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.Cause)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *Error) Unwrap() error { return e.Cause }

// GRPCStatus lets errors.As and the gRPC runtime pick up the code directly.
func (e *Error) GRPCStatus() *status.Status { return status.New(e.Code, e.Message) }

// HTTPStatus returns the HTTP status for this error.
//
// The mapping is the standard one from google.rpc.Code to HTTP.
func (e *Error) HTTPStatus() int { return httpStatus(e.Code) }

func httpStatus(c codes.Code) int {
	switch c {
	case codes.OK:
		return http.StatusOK
	case codes.InvalidArgument, codes.FailedPrecondition, codes.OutOfRange:
		return http.StatusBadRequest
	case codes.Unauthenticated:
		return http.StatusUnauthorized
	case codes.PermissionDenied:
		return http.StatusForbidden
	case codes.NotFound:
		return http.StatusNotFound
	case codes.AlreadyExists, codes.Aborted:
		return http.StatusConflict
	case codes.ResourceExhausted:
		return http.StatusTooManyRequests
	case codes.Canceled:
		return 499 // client closed request
	case codes.Unimplemented:
		return http.StatusNotImplemented
	case codes.Unavailable:
		return http.StatusServiceUnavailable
	case codes.DeadlineExceeded:
		return http.StatusGatewayTimeout
	default:
		return http.StatusInternalServerError
	}
}

// Constructors for the cases services actually produce.

func NotFound(format string, a ...any) *Error {
	return &Error{Code: codes.NotFound, Message: fmt.Sprintf(format, a...), Reason: "notFound"}
}

func AlreadyExists(format string, a ...any) *Error {
	return &Error{Code: codes.AlreadyExists, Message: fmt.Sprintf(format, a...), Reason: "alreadyExists"}
}

func InvalidArgument(format string, a ...any) *Error {
	return &Error{Code: codes.InvalidArgument, Message: fmt.Sprintf(format, a...), Reason: "invalid"}
}

func FailedPrecondition(format string, a ...any) *Error {
	return &Error{Code: codes.FailedPrecondition, Message: fmt.Sprintf(format, a...), Reason: "conditionNotMet"}
}

func Unimplemented(format string, a ...any) *Error {
	return &Error{Code: codes.Unimplemented, Message: fmt.Sprintf(format, a...), Reason: "notImplemented"}
}

func Internal(cause error, format string, a ...any) *Error {
	return &Error{Code: codes.Internal, Message: fmt.Sprintf(format, a...), Reason: "internalError", Cause: cause}
}

// From converts any error into an *Error, preserving one that already is.
//
// An unrecognised error becomes Internal rather than something more specific:
// guessing a friendlier code would misreport a bug as a client mistake.
func From(err error) *Error {
	if err == nil {
		return nil
	}
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return Internal(err, "internal error")
}

// jsonError is the Cloud Storage JSON API error envelope.
type jsonError struct {
	Error struct {
		Code    int          `json:"code"`
		Message string       `json:"message"`
		Errors  []jsonDetail `json:"errors,omitempty"`
	} `json:"error"`
}

type jsonDetail struct {
	Message string `json:"message"`
	Domain  string `json:"domain"`
	Reason  string `json:"reason"`
}

// WriteJSON renders the error in the Google JSON API envelope.
func WriteJSON(w http.ResponseWriter, err error) {
	e := From(err)
	var body jsonError
	body.Error.Code = e.HTTPStatus()
	body.Error.Message = e.Message
	body.Error.Errors = []jsonDetail{{Message: e.Message, Domain: "global", Reason: e.Reason}}

	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(e.HTTPStatus())
	_ = json.NewEncoder(w).Encode(body)
}

// DecodeJSON reads a Google JSON API error envelope. Used by tests to confirm
// what a client would actually see.
func DecodeJSON(r io.Reader) (code int, message, reason string, err error) {
	var body jsonError
	if err := json.NewDecoder(r).Decode(&body); err != nil {
		return 0, "", "", err
	}
	if len(body.Error.Errors) > 0 {
		reason = body.Error.Errors[0].Reason
	}
	return body.Error.Code, body.Error.Message, reason, nil
}
