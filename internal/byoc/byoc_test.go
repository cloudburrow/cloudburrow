package byoc_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/byoc"
)

func TestParseCloudAndPreset(t *testing.T) {
	for in, want := range map[string]byoc.Cloud{"aws": byoc.AWS, "AWS": byoc.AWS, " azure ": byoc.Azure} {
		if got, err := byoc.ParseCloud(in); err != nil || got != want {
			t.Errorf("ParseCloud(%q) = %q, %v", in, got, err)
		}
	}
	if _, err := byoc.ParseCloud("gcp"); err == nil {
		t.Error("gcp was accepted as a cloud to provision in")
	}
	if p, err := byoc.ParsePreset("GPU"); err != nil || p != byoc.GPU || p.Title() != "GPU" {
		t.Errorf("ParsePreset(GPU) = %q, %v", p, err)
	}
	if _, err := byoc.ParsePreset("xl"); err == nil {
		t.Error("an unknown preset was accepted")
	}
}

func TestErrorsReadInPlainWords(t *testing.T) {
	pe := &byoc.PermissionError{Missing: []byoc.Permission{{Action: "ec2:RunInstances", Why: "to start the VM"}}}
	if !strings.Contains(pe.Error(), "ec2:RunInstances (to start the VM)") || byoc.ErrorKind(pe) != "permission" {
		t.Errorf("%v / %s", pe, byoc.ErrorKind(pe))
	}
	le := &byoc.LeftoverError{Resources: []byoc.Resource{{Type: "ec2:vpc", ID: "vpc-1"}}}
	if !strings.Contains(le.Error(), "ec2:vpc vpc-1") || byoc.ErrorKind(le) != "leftover" {
		t.Errorf("%v", le)
	}
	if byoc.ErrorKind(&byoc.QuotaError{}) != "quota" || byoc.ErrorKind(&byoc.AuthError{}) != "auth" || byoc.ErrorKind(errors.New("x")) != "" {
		t.Error("ErrorKind misclassifies")
	}
	if r := (byoc.PermissionReport{Checked: false, Note: "iam:SimulatePrincipalPolicy was refused"}); !strings.Contains(r.Summary(), "could not be checked") {
		t.Errorf("unchecked summary %q", r.Summary())
	}
}

func TestCheckIP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/bad" {
			_, _ = w.Write([]byte("<html>"))
			return
		}
		_, _ = w.Write([]byte("203.0.113.5\n"))
	}))
	defer srv.Close()
	ip, err := byoc.CheckIP(srv.URL, srv.Client())(context.Background())
	if err != nil || ip != "203.0.113.5" {
		t.Fatalf("CheckIP = %q, %v", ip, err)
	}
	if _, err := byoc.CheckIP(srv.URL+"/bad", srv.Client())(context.Background()); err == nil {
		t.Fatal("a page that is not an address was accepted")
	}
}

func TestNoRemoteEngineSaysSo(t *testing.T) {
	var e byoc.RemoteEngine = byoc.NoRemoteEngine{}
	if err := e.Connect(context.Background(), byoc.RemoteTarget{}); !errors.Is(err, byoc.ErrNoRemoteEngine) {
		t.Fatal(err)
	}
	st, _ := e.Status(context.Background(), "x")
	if st.Connected || !strings.Contains(st.Message, "phase 1") {
		t.Fatalf("%+v", st)
	}
	if _, open := <-e.Events("x"); open {
		t.Fatal("events of no engine are open")
	}
}

func TestOpenURLRefusesNonHTTPS(t *testing.T) {
	for _, u := range []string{"file:///etc/passwd", "javascript:alert(1)", "http://example.com/"} {
		if err := byoc.OpenURL(u); err == nil {
			t.Errorf("OpenURL(%q) was accepted", u)
		}
	}
}
