package main

import (
	"context"
	"testing"
	"time"

	"cloud.google.com/go/iam/apiv1/iampb"
	"google.golang.org/genproto/googleapis/type/expr"

	"github.com/cloudburrow/cloudburrow/internal/iameval"
	"github.com/cloudburrow/cloudburrow/internal/service/compute"
	"github.com/cloudburrow/cloudburrow/internal/service/iap"
)

// TestIAPGateAllowed (#2160): roles/iap.httpsResourceAccessor on the
// backend service's IAP resource, or a level above it, grants access;
// roles/owner does not, as on Google; a request.path condition is read.
func TestIAPGateAllowed(t *testing.T) {
	ia, err := iap.New(iap.Options{})
	if err != nil {
		t.Fatal(err)
	}
	svc := &computeService{iap: ia, iapEval: &iameval.Evaluator{}}
	gate := computeIAPGate{svc: svc}
	res := "projects/p/iap_web/compute/services/web"
	set := func(resource string, b ...*iampb.Binding) {
		if _, err := ia.SetIamPolicy(context.Background(), &iampb.SetIamPolicyRequest{Resource: resource, Policy: &iampb.Policy{Version: 3, Bindings: b}}); err != nil {
			t.Fatal(err)
		}
	}
	req := compute.IAPRequest{Host: "example.com", Path: "/", Time: time.Now()}
	set(res, &iampb.Binding{Role: "roles/owner", Members: []string{"user:owner@example.com"}},
		&iampb.Binding{Role: "roles/iap.httpsResourceAccessor", Members: []string{"user:a@example.com"}})
	set("projects/p/iap_web", &iampb.Binding{Role: "roles/iap.httpsResourceAccessor", Members: []string{"user:b@example.com"},
		Condition: &expr.Expr{Title: "admin only", Expression: `request.path.startsWith("/admin")`}})
	for _, c := range []struct {
		email, path string
		want        bool
	}{{"a@example.com", "/", true}, {"A@example.com", "/x", true}, {"owner@example.com", "/", false}, {"c@example.com", "/", false},
		{"b@example.com", "/", false}, {"b@example.com", "/admin/x", true}} {
		req.Path = c.path
		if got := gate.Allowed(context.Background(), res, c.email, req); got != c.want {
			t.Errorf("Allowed(%s, %s) = %v", c.email, c.path, got)
		}
	}
	if gate.ProjectNumber("p") == "" {
		t.Error("ProjectNumber")
	}
}

// TestIAPTunnelAllowed (#2160): roles/iap.tunnelResourceAccessor on an
// instance's tunnel resource or the zone above grants the tunnel, the
// project's owner role does not, and a destination.port condition is read.
func TestIAPTunnelAllowed(t *testing.T) {
	ia, err := iap.New(iap.Options{})
	if err != nil {
		t.Fatal(err)
	}
	svc := &computeService{iap: ia, iapEval: &iameval.Evaluator{}}
	set := func(resource string, b ...*iampb.Binding) {
		if _, err := ia.SetIamPolicy(context.Background(), &iampb.SetIamPolicyRequest{Resource: resource, Policy: &iampb.Policy{Version: 3, Bindings: b}}); err != nil {
			t.Fatal(err)
		}
	}
	vm := "projects/p/iap_tunnel/zones/z/instances/vm"
	set(vm, &iampb.Binding{Role: "roles/owner", Members: []string{"user:owner@example.com"}},
		&iampb.Binding{Role: "roles/iap.tunnelResourceAccessor", Members: []string{"user:a@example.com"}})
	set("projects/p/iap_tunnel/zones/z", &iampb.Binding{Role: "roles/iap.tunnelResourceAccessor", Members: []string{"user:b@example.com"},
		Condition: &expr.Expr{Title: "ssh only", Expression: `destination.port == 22`}})
	const perm = "iap.tunnelInstances.accessViaIAP"
	for _, c := range []struct {
		email string
		port  int
		want  bool
	}{{"a@example.com", 8080, true}, {"owner@example.com", 22, false}, {"b@example.com", 22, true}, {"b@example.com", 8080, false}, {"c@example.com", 22, false}} {
		if got := svc.tunnelAllowed(context.Background(), perm, vm, c.email, "10.128.0.2", c.port); got != c.want {
			t.Errorf("tunnelAllowed(%s, %d) = %v", c.email, c.port, got)
		}
	}
}
