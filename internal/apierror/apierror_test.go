package apierror

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/anypb"
)

// One cause must produce a consistent gRPC code and HTTP status. If these could
// disagree, a gRPC client and a JSON client would be told different things
// about the same failure.
func TestCodeAndHTTPStatusAgree(t *testing.T) {
	t.Parallel()
	tests := []struct {
		err      *Error
		wantCode codes.Code
		wantHTTP int
		reason   string
	}{
		{NotFound("no such bucket"), codes.NotFound, http.StatusNotFound, "notFound"},
		{AlreadyExists("bucket exists"), codes.AlreadyExists, http.StatusConflict, "alreadyExists"},
		{InvalidArgument("bad name"), codes.InvalidArgument, http.StatusBadRequest, "invalid"},
		{FailedPrecondition("generation mismatch"), codes.FailedPrecondition, http.StatusBadRequest, "conditionNotMet"},
		{Unimplemented("not yet"), codes.Unimplemented, http.StatusNotImplemented, "notImplemented"},
		{Internal(errors.New("boom"), "internal"), codes.Internal, http.StatusInternalServerError, "internalError"},
	}
	for _, tt := range tests {
		t.Run(tt.reason, func(t *testing.T) {
			t.Parallel()
			if tt.err.Code != tt.wantCode {
				t.Errorf("Code = %v, want %v", tt.err.Code, tt.wantCode)
			}
			if got := tt.err.HTTPStatus(); got != tt.wantHTTP {
				t.Errorf("HTTPStatus() = %d, want %d", got, tt.wantHTTP)
			}
			if tt.err.Reason != tt.reason {
				t.Errorf("Reason = %q, want %q", tt.err.Reason, tt.reason)
			}
			// The gRPC runtime must see the same code.
			if got := status.Code(tt.err); got != tt.wantCode {
				t.Errorf("status.Code() = %v, want %v", got, tt.wantCode)
			}
		})
	}
}

// WriteJSON is Google's JSON mapping of google.rpc.Status: the HTTP code, the
// message and the canonical code's name in status, with no Cloud Storage
// errors[]. Every code a REST client could confuse by its HTTP code alone
// (FAILED_PRECONDITION, OUT_OF_RANGE and INVALID_ARGUMENT are all 400;
// ABORTED and ALREADY_EXISTS both 409) is told apart by status (#1470).
func TestWriteJSONCarriesStatus(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		err    error
		http   int
		status string
	}{
		{NotFound("No such secret"), 404, "NOT_FOUND"},
		{InvalidArgument("bad name"), 400, "INVALID_ARGUMENT"},
		{FailedPrecondition("version is disabled"), 400, "FAILED_PRECONDITION"},
		{status.Error(codes.OutOfRange, "past the end"), 400, "OUT_OF_RANGE"},
		{AlreadyExists("secret exists"), 409, "ALREADY_EXISTS"},
		{Aborted("stale etag"), 409, "ABORTED"},
		{status.Error(codes.PermissionDenied, "denied"), 403, "PERMISSION_DENIED"},
		{status.Error(codes.ResourceExhausted, "quota"), 429, "RESOURCE_EXHAUSTED"},
		{status.Error(codes.Canceled, "gone"), 499, "CANCELLED"},
		{Unimplemented("not yet"), 501, "UNIMPLEMENTED"},
		{errors.New("boom"), 500, "INTERNAL"},
	} {
		t.Run(tt.status, func(t *testing.T) {
			t.Parallel()
			rec := httptest.NewRecorder()
			WriteJSON(rec, tt.err)
			if rec.Code != tt.http {
				t.Errorf("HTTP status = %d, want %d", rec.Code, tt.http)
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/json; charset=UTF-8" {
				t.Errorf("Content-Type = %q", ct)
			}
			var raw map[string]map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
				t.Fatalf("body %s: %v", rec.Body, err)
			}
			e := raw["error"]
			if e["code"] != float64(tt.http) || e["status"] != tt.status || e["message"] == "" {
				t.Errorf("body = %s; want code %d, status %s and a message", rec.Body, tt.http, tt.status)
			}
			// Google's body for these APIs has no errors[], and no details
			// when the status has none.
			if _, ok := e["errors"]; ok {
				t.Errorf("body = %s; want no errors[]", rec.Body)
			}
			if _, ok := e["details"]; ok {
				t.Errorf("body = %s; want no details", rec.Body)
			}
		})
	}
}

// A gRPC status's details reach the JSON body in their JSON form, each with
// its @type, which is what gax's REST error parsing reads ErrorInfo from: the
// body must unmarshal into google.rpc.Status's JSON mapping exactly, since an
// unknown field (such as errors[]) makes it drop every detail.
func TestWriteJSONCarriesDetails(t *testing.T) {
	t.Parallel()
	st, err := status.New(codes.FailedPrecondition, "the API is disabled").WithDetails(
		&errdetails.ErrorInfo{Reason: "SERVICE_DISABLED", Domain: "googleapis.com", Metadata: map[string]string{"service": "secretmanager.googleapis.com"}},
		&errdetails.PreconditionFailure{Violations: []*errdetails.PreconditionFailure_Violation{{Type: "TOS", Subject: "p", Description: "d"}}},
	)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	WriteJSON(rec, st.Err())
	if rec.Code != http.StatusBadRequest {
		t.Errorf("HTTP status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"@type":"type.googleapis.com/google.rpc.ErrorInfo"`) {
		t.Errorf("body = %s; want an ErrorInfo detail with its @type", rec.Body)
	}

	// gax's own schema: google.rpc.Status with the HTTP code in code and the
	// canonical code in status.
	var got struct {
		Error struct {
			Code    int               `json:"code"`
			Message string            `json:"message"`
			Status  string            `json:"status"`
			Details []json.RawMessage `json:"details"`
		} `json:"error"`
	}
	dec := json.NewDecoder(bytes.NewReader(rec.Body.Bytes()))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&got); err != nil {
		t.Fatalf("body %s is not Google's status mapping: %v", rec.Body, err)
	}
	if got.Error.Code != 400 || got.Error.Status != "FAILED_PRECONDITION" || got.Error.Message != "the API is disabled" || len(got.Error.Details) != 2 {
		t.Fatalf("body = %+v", got.Error)
	}
	var info anypb.Any
	if err := protojson.Unmarshal(got.Error.Details[0], &info); err != nil {
		t.Fatalf("detail %s: %v", got.Error.Details[0], err)
	}
	ei := &errdetails.ErrorInfo{}
	if err := info.UnmarshalTo(ei); err != nil || ei.GetReason() != "SERVICE_DISABLED" || ei.GetMetadata()["service"] != "secretmanager.googleapis.com" {
		t.Errorf("ErrorInfo = %v, %v", ei, err)
	}

	// The same details on the gRPC side of the same cause.
	if d := From(st.Err()).GRPCStatus().Details(); len(d) != 2 {
		t.Errorf("GRPCStatus().Details() = %v; want both details", d)
	}
	code, message, gotStatus, details, err := DecodeStatusJSON(bytes.NewReader(rec.Body.Bytes()))
	if err != nil || code != 400 || message != "the API is disabled" || gotStatus != "FAILED_PRECONDITION" || len(details) != 2 {
		t.Errorf("DecodeStatusJSON = %d, %q, %q, %d details, %v", code, message, gotStatus, len(details), err)
	}
}

// A detail of a type this binary does not link is left out; the rest of the
// error is still written.
func TestWriteJSONSkipsUnrenderableDetails(t *testing.T) {
	t.Parallel()
	e := &Error{Code: codes.Aborted, Message: "retry", Details: []*anypb.Any{{TypeUrl: "type.googleapis.com/no.such.Type", Value: []byte{1}}}}
	rec := httptest.NewRecorder()
	WriteJSON(rec, e)
	code, _, gotStatus, details, err := DecodeStatusJSON(rec.Body)
	if err != nil || code != 409 || gotStatus != "ABORTED" || len(details) != 0 {
		t.Errorf("DecodeStatusJSON = %d, %q, %d details, %v", code, gotStatus, len(details), err)
	}
}

// WriteStorageJSON is the Cloud Storage JSON API's envelope, exactly as
// Google's: code, message and errors[] with a reason, and no status.
func TestWriteStorageJSONEnvelope(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	WriteStorageJSON(rec, NotFound("No such object: bucket/obj"))

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json; charset=UTF-8" {
		t.Errorf("Content-Type = %q", ct)
	}
	const want = `{"error":{"code":404,"message":"No such object: bucket/obj","errors":[{"message":"No such object: bucket/obj","domain":"global","reason":"notFound"}]}}`
	if got := strings.TrimSpace(rec.Body.String()); got != want {
		t.Errorf("body = %s\nwant   %s", got, want)
	}
	code, message, reason, err := DecodeJSON(strings.NewReader(want))
	if err != nil {
		t.Fatalf("DecodeJSON: %v", err)
	}
	if code != 404 || reason != "notFound" || message != "No such object: bucket/obj" {
		t.Errorf("decoded code=%d reason=%q message=%q", code, reason, message)
	}
}

// An unrecognised error must become Internal. Guessing a friendlier code would
// misreport one of our bugs as the caller's mistake.
func TestFromUnknownErrorIsInternal(t *testing.T) {
	t.Parallel()
	e := From(errors.New("something went wrong"))
	if e.Code != codes.Internal {
		t.Errorf("Code = %v, want Internal", e.Code)
	}
	if !errors.Is(e, e.Cause) {
		t.Error("cause was not preserved")
	}
}

func TestFromPreservesExistingError(t *testing.T) {
	t.Parallel()
	orig := NotFound("gone")
	if got := From(orig); got != orig {
		t.Error("From() replaced an existing *Error")
	}
	// Also through a wrap.
	wrapped := errors.Join(errors.New("context"), orig)
	if got := From(wrapped); got.Code != codes.NotFound {
		t.Errorf("From(wrapped) = %v, want NotFound", got.Code)
	}
}

func TestFromNil(t *testing.T) {
	t.Parallel()
	if From(nil) != nil {
		t.Error("From(nil) should be nil")
	}
}

func TestUnwrap(t *testing.T) {
	t.Parallel()
	cause := errors.New("disk full")
	e := Internal(cause, "write failed")
	if !errors.Is(e, cause) {
		t.Error("errors.Is could not reach the cause")
	}
}

// A nil *Error assigned to an error interface is not nil. Wrap exists to stop
// that turning every successful call into a reported failure, which it did
// once in internal/service/tasks before this was added.
func TestWrapReturnsUntypedNil(t *testing.T) {
	t.Parallel()
	if err := Wrap(nil); err != nil {
		t.Fatalf("Wrap(nil) = %v (%T), want an untyped nil", err, err)
	}

	// Demonstrate the hazard Wrap avoids, so the reason is not lost.
	var typed error = From(nil)
	if typed == nil {
		t.Skip("From(nil) no longer produces a typed nil; Wrap may be unnecessary")
	}
	if Wrap(nil) != nil {
		t.Error("Wrap did not fix the typed-nil case")
	}
}

func TestWrapPreservesRealErrors(t *testing.T) {
	t.Parallel()
	orig := NotFound("gone")
	if got := Wrap(orig); got == nil {
		t.Fatal("Wrap dropped a real error")
	}
	if status := From(Wrap(orig)).HTTPStatus(); status != http.StatusNotFound {
		t.Errorf("status = %d, want 404", status)
	}
}
