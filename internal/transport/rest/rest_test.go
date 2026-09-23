package rest

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// An unknown method must return a protocol-correct 404, never a fabricated
// success that lets a client proceed as though the call worked.
func TestUnknownMethodReturnsNotFound(t *testing.T) {
	t.Parallel()
	r := NewRouter()
	r.Handle("GET /v2/known", func(w http.ResponseWriter, _ *http.Request) error {
		return WriteJSON(w, http.StatusOK, map[string]string{"ok": "yes"})
	})

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v2/unknown", nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	code, message, reason, err := apierror.DecodeJSON(rec.Body)
	if err != nil {
		t.Fatalf("response was not a Google error envelope: %v", err)
	}
	if code != 404 || reason != "notFound" {
		t.Errorf("code=%d reason=%q, want 404/notFound", code, reason)
	}
	if !strings.Contains(message, "/v2/unknown") {
		t.Errorf("message should name the method, got %q", message)
	}
}

// A method registered for one verb must not answer another.
func TestWrongMethodIsNotRouted(t *testing.T) {
	t.Parallel()
	r := NewRouter()
	r.Handle("POST /v2/queues", func(w http.ResponseWriter, _ *http.Request) error {
		return WriteJSON(w, http.StatusOK, nil)
	})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/v2/queues", nil))
	if rec.Code == http.StatusOK {
		t.Error("DELETE was answered by a POST-only route")
	}
}

func TestHandlerErrorsRenderAsGoogleErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		err      error
		wantCode int
		reason   string
	}{
		{"not found", apierror.NotFound("no such queue"), 404, "notFound"},
		{"already exists", apierror.AlreadyExists("queue exists"), 409, "alreadyExists"},
		{"invalid", apierror.InvalidArgument("bad name"), 400, "invalid"},
		{"unimplemented", apierror.Unimplemented("not yet"), 501, "notImplemented"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := NewRouter()
			r.Handle("GET /x", func(http.ResponseWriter, *http.Request) error { return tt.err })
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
			if rec.Code != tt.wantCode {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantCode)
			}
			_, _, reason, err := apierror.DecodeJSON(rec.Body)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if reason != tt.reason {
				t.Errorf("reason = %q, want %q", reason, tt.reason)
			}
		})
	}
}

// A field we silently dropped would leave the caller believing it took effect.
func TestDecodeJSONRejectsUnknownFields(t *testing.T) {
	t.Parallel()
	var dst struct {
		Name string `json:"name"`
	}
	req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(`{"name":"a","surprise":1}`))
	err := DecodeJSON(req, &dst)
	if err == nil {
		t.Fatal("DecodeJSON accepted an unknown field")
	}
	if !strings.Contains(err.Error(), "surprise") {
		t.Errorf("error should name the offending field, got: %v", err)
	}
}

func TestDecodeJSONRejectsTrailingContent(t *testing.T) {
	t.Parallel()
	var dst struct {
		Name string `json:"name"`
	}
	req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(`{"name":"a"}{"name":"b"}`))
	if err := DecodeJSON(req, &dst); err == nil {
		t.Error("DecodeJSON accepted a second object that would have been ignored")
	}
}

func TestDecodeJSONAcceptsValidBody(t *testing.T) {
	t.Parallel()
	var dst struct {
		Name string `json:"name"`
	}
	req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(`{"name":"queue-1"}`))
	if err := DecodeJSON(req, &dst); err != nil {
		t.Fatalf("DecodeJSON: %v", err)
	}
	if dst.Name != "queue-1" {
		t.Errorf("Name = %q", dst.Name)
	}
}

// An oversized body must be refused rather than decoded into memory.
func TestRequestBodyIsBounded(t *testing.T) {
	t.Parallel()
	r := NewRouter()
	r.Handle("POST /x", func(w http.ResponseWriter, req *http.Request) error {
		var dst struct {
			Data string `json:"data"`
		}
		if err := DecodeJSON(req, &dst); err != nil {
			return err
		}
		return WriteJSON(w, http.StatusOK, nil)
	})

	huge := `{"data":"` + strings.Repeat("a", MaxRequestBytes+1024) + `"}`
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(huge)))
	if rec.Code == http.StatusOK {
		t.Error("an oversized body was accepted")
	}
}

func TestPathValuesAndResourceName(t *testing.T) {
	t.Parallel()
	r := NewRouter()
	var got string
	r.Handle("GET /v2/projects/{project}/locations/{location}/queues/{queues}", func(w http.ResponseWriter, req *http.Request) error {
		name, err := ResourceName(req, "queues")
		if err != nil {
			return err
		}
		got = name
		return WriteJSON(w, http.StatusOK, nil)
	})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v2/projects/p1/locations/us-central1/queues/q1", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if want := "projects/p1/locations/us-central1/queues/q1"; got != want {
		t.Errorf("ResourceName = %q, want %q", got, want)
	}
}

func TestQueryInt(t *testing.T) {
	t.Parallel()
	req := httptest.NewRequest(http.MethodGet, "/x?pageSize=25", nil)
	n, err := QueryInt(req, "pageSize", 100)
	if err != nil || n != 25 {
		t.Errorf("QueryInt = %d, %v", n, err)
	}
	if n, err := QueryInt(req, "absent", 7); err != nil || n != 7 {
		t.Errorf("default = %d, %v", n, err)
	}
	bad := httptest.NewRequest(http.MethodGet, "/x?pageSize=lots", nil)
	if _, err := QueryInt(bad, "pageSize", 1); err == nil {
		t.Error("QueryInt accepted a non-integer")
	}
}

func TestRoutesAreDiscoverable(t *testing.T) {
	t.Parallel()
	r := NewRouter()
	r.Handle("GET /a", func(http.ResponseWriter, *http.Request) error { return nil })
	r.Handle("POST /b", func(http.ResponseWriter, *http.Request) error { return nil })
	routes := r.Routes()
	if len(routes) != 2 || routes[0] != "GET /a" || routes[1] != "POST /b" {
		t.Errorf("Routes() = %v", routes)
	}
}
