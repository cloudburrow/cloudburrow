package apierror

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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

// The JSON envelope must match what a Google client expects to parse.
func TestWriteJSONEnvelope(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	WriteJSON(rec, NotFound("No such object: bucket/obj"))

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json; charset=UTF-8" {
		t.Errorf("Content-Type = %q", ct)
	}
	code, message, reason, err := DecodeJSON(rec.Body)
	if err != nil {
		t.Fatalf("DecodeJSON: %v", err)
	}
	if code != 404 || reason != "notFound" {
		t.Errorf("decoded code=%d reason=%q, want 404/notFound", code, reason)
	}
	if message != "No such object: bucket/obj" {
		t.Errorf("message = %q", message)
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
