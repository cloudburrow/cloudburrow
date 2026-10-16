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

	rpccode "google.golang.org/genproto/googleapis/rpc/code"
	spb "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/anypb"
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
	// Details are the google.rpc.Status details (ErrorInfo, BadRequest and
	// the like) a gRPC status carried, kept so the JSON body carries them as
	// Google's does.
	Details []*anypb.Any
}

func (e *Error) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.Cause)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *Error) Unwrap() error { return e.Cause }

// GRPCStatus lets errors.As and the gRPC runtime pick up the code directly.
func (e *Error) GRPCStatus() *status.Status {
	if len(e.Details) == 0 {
		return status.New(e.Code, e.Message)
	}
	return status.FromProto(&spb.Status{Code: int32(e.Code), Message: e.Message, Details: e.Details})
}

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

// Aborted is a concurrency conflict, such as a stale etag, that the caller
// resolves by reading again and retrying.
func Aborted(format string, a ...any) *Error {
	return &Error{Code: codes.Aborted, Message: fmt.Sprintf(format, a...), Reason: "aborted"}
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
	// A gRPC status keeps its code, so a REST transcoding of a gRPC method
	// reports UNIMPLEMENTED as 501 rather than as an internal error.
	if st, ok := status.FromError(err); ok && st.Code() != codes.Unknown {
		return &Error{Code: st.Code(), Message: st.Message(), Reason: statusReasons[st.Code()], Details: st.Proto().GetDetails()}
	}
	return Internal(err, "internal error")
}

// statusReasons are the JSON "reason" values for codes a gRPC status carries.
var statusReasons = map[codes.Code]string{
	codes.NotFound: "notFound", codes.AlreadyExists: "alreadyExists", codes.InvalidArgument: "invalid",
	codes.FailedPrecondition: "conditionNotMet", codes.Unimplemented: "notImplemented", codes.Aborted: "aborted",
	codes.PermissionDenied: "forbidden", codes.Unauthenticated: "unauthorized", codes.Unavailable: "backendError",
}

// statusError is the error body of Google's JSON/HTTP mapping of
// google.rpc.Status (https://cloud.google.com/apis/design/errors#http_mapping,
// AIP-193): the HTTP code, the message, the canonical code's name in status,
// and the status's details, each an Any in its JSON form with its "@type".
// Every API but Cloud Storage's JSON API and BigQuery's v2 API answers so, and
// the REST clients read status for the canonical code (gcloud prints it, the
// Node and Java REST transports map it back) and details for ErrorInfo.
type statusError struct {
	Error struct {
		Code    int               `json:"code"`
		Message string            `json:"message"`
		Status  string            `json:"status"`
		Details []json.RawMessage `json:"details,omitempty"`
	} `json:"error"`
}

// storageError is the Cloud Storage JSON API error envelope, which has no
// status and carries errors[] with a reason instead.
type storageError struct {
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

// WriteJSON renders the error as Google's JSON mapping of google.rpc.Status:
// {"error": {"code", "message", "status", "details"}}. Cloud Storage's JSON
// API answers in its own envelope instead: WriteStorageJSON.
func WriteJSON(w http.ResponseWriter, err error) {
	e := From(err)
	var body statusError
	body.Error.Code = e.HTTPStatus()
	body.Error.Message = e.Message
	body.Error.Status = rpccode.Code(e.Code).String()
	for _, d := range e.Details {
		// A detail whose type this binary does not link cannot be rendered as
		// JSON; it is left out rather than failing the whole error.
		if b, err := protojson.Marshal(d); err == nil {
			body.Error.Details = append(body.Error.Details, b)
		}
	}
	write(w, e.HTTPStatus(), body)
}

// WriteStorageJSON renders the error in the Cloud Storage JSON API envelope,
// {"error": {"code", "message", "errors": [{"message", "domain", "reason"}]}},
// which is Google's for that API: it carries no status.
func WriteStorageJSON(w http.ResponseWriter, err error) {
	e := From(err)
	var body storageError
	body.Error.Code = e.HTTPStatus()
	body.Error.Message = e.Message
	body.Error.Errors = []jsonDetail{{Message: e.Message, Domain: "global", Reason: e.Reason}}
	write(w, e.HTTPStatus(), body)
}

func write(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

// DecodeJSON reads a Google JSON API error body, of either envelope. reason
// is errors[0].reason, which only the Cloud Storage envelope carries. Used by
// tests to confirm what a client would actually see.
func DecodeJSON(r io.Reader) (code int, message, reason string, err error) {
	var body storageError
	if err := json.NewDecoder(r).Decode(&body); err != nil {
		return 0, "", "", err
	}
	if len(body.Error.Errors) > 0 {
		reason = body.Error.Errors[0].Reason
	}
	return body.Error.Code, body.Error.Message, reason, nil
}

// DecodeStatusJSON reads Google's JSON mapping of google.rpc.Status, as
// WriteJSON writes it: the HTTP code, the message, the canonical code's name
// and the raw details.
func DecodeStatusJSON(r io.Reader) (code int, message, status string, details []json.RawMessage, err error) {
	var body statusError
	if err := json.NewDecoder(r).Decode(&body); err != nil {
		return 0, "", "", nil, err
	}
	return body.Error.Code, body.Error.Message, body.Error.Status, body.Error.Details, nil
}

// Wrap converts an error for return through an `error` interface.
//
// Prefer this over From at a return site. From returns *Error, and a nil
// *Error assigned to an error interface is NOT nil — so `return From(x)` makes
// every success look like a failure. Wrap returns an untyped nil instead.
func Wrap(err error) error {
	if err == nil {
		return nil
	}
	return From(err)
}
