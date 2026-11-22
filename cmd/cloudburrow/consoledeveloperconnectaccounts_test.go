package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"

	runadapter "github.com/cloudburrow/cloudburrow/internal/adapter/run"
)

// TestConsoleDeveloperConnectAccountConnectors (#2105): Create connector
// makes a GOOGLE connector, which the list shows; its page shows its
// scopes and oauth start URI, and Connect your account is a link to that
// URI; Edit changes the scopes; Delete removes it.
func TestConsoleDeveloperConnectAccountConnectors(t *testing.T) {
	if err := runadapter.UseSourceManagerRoot(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	s := runadapter.NewServer(&runadapter.Knative{}, "t", time.Minute)
	var srv *httptest.Server
	ssm := s.SecureSourceManager(func() string { return srv.URL })
	srv = httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)
	g := grpc.NewServer()
	s.DeveloperConnect().Register(g)
	ssm.Register(g)
	runadapter.NewOperationsServer(s).Register(g)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = g.Serve(ln) }()
	t.Cleanup(g.Stop)
	p := devConnectAccountsProvider{ssm: sourceManagerProvider{b: runBuildsProvider{defaultProject: "console-dcac", runAddr: func() string { return ln.Addr().String() }}}}

	if _, err := p.Create(ctx, "", map[string]string{"name": "gh", "region": "us-central1", "provider": "GITHUB", "scopes": "repo"}); err == nil {
		t.Error("Create connector made a GITHUB connector")
	}
	if _, f := p.CreateForm(); len(f) != 5 || f[3].Name != "provider" || len(f[3].Options) != 1 || f[3].Options[0] != "GOOGLE" {
		t.Errorf("Create connector's fields = %+v", f)
	}
	if name, err := p.Create(ctx, "", map[string]string{"name": "docs", "region": "us-central1", "provider": "GOOGLE",
		"scopes": strings.Join(dcGoogleScopes, "\n")}); err != nil || name != "docs" {
		t.Fatalf("Create = %q, %v", name, err)
	}
	l, err := p.List(ctx, "")
	if err != nil || l.Unavailable != "" || len(l.Items) != 1 || l.Items[0].Name != "docs" || l.Items[0].Fields["Provider"] != "Google" {
		t.Fatalf("the list = %+v, %v", l, err)
	}
	d, err := p.Detail(ctx, "", []string{"docs"})
	if err != nil || d.Unavailable != "" {
		t.Fatalf("the page = %+v, %v", d, err)
	}
	props := map[string]string{}
	for _, pr := range d.Summary {
		props[pr.Label] = pr.Value
	}
	if !strings.Contains(props["Scopes"], "drive.readonly") || !strings.HasPrefix(props["OAuth start URI"], srv.URL+runadapter.DeveloperConnectOAuthStartPath) {
		t.Errorf("the page's properties = %v", props)
	}
	if len(d.Sections) != 1 || d.Sections[0].ID != "users" || len(d.Sections[0].Listing.Items) != 0 {
		t.Errorf("the page's sections = %+v", d.Sections)
	}
	acts := p.DetailActions(ctx, "", []string{"docs"})
	if len(acts) != 2 || acts[0].Label != "Connect your account" || acts[0].Href != props["OAuth start URI"] || acts[1].ID != "edit" {
		t.Fatalf("the page's actions = %+v", acts)
	}
	if err := p.ActAt(ctx, "", []string{"docs"}, "edit", map[string]string{"scopes": "openid"}); err != nil {
		t.Fatal(err)
	}
	if d, _ := p.Detail(ctx, "", []string{"docs"}); d.Summary[3].Value != "openid" {
		t.Errorf("after Edit, Scopes = %q", d.Summary[3].Value)
	}
	if err := p.Delete(ctx, "", "docs"); err != nil {
		t.Fatal(err)
	}
	if l, _ := p.List(ctx, ""); len(l.Items) != 0 {
		t.Errorf("after Delete, the list = %+v", l.Items)
	}
}
