package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"cloud.google.com/go/iam"
	"cloud.google.com/go/iam/apiv1/iampb"
	"cloud.google.com/go/kms/apiv1/kmspb"
	"cloud.google.com/go/storage"
	"google.golang.org/api/googleapi"
	storagev1 "google.golang.org/api/storage/v1"
	"google.golang.org/grpc/status"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/secrets"
	"github.com/cloudburrow/cloudburrow/internal/service/tasks"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// permissionsCase is one resource with a Permissions tab, and how its service
// reads the policy back and words a stale etag's refusal.
type permissionsCase struct {
	service, project string
	path             []string
	// readBack is the policy as the service's own GetIamPolicy returns it.
	readBack func(t *testing.T) *iampb.Policy
	// refusal is the message the console should show for the stale etag:
	// the service's own refusal of it, as the console words any error.
	refusal func() string
	// write is a change made outside the console, through the service.
	write func(t *testing.T, add *iampb.Binding)
}

// grpcRefusal is how the console words a gRPC error: its code and message.
func grpcRefusal(err error) string {
	st, _ := status.FromError(err)
	return st.Code().String() + ": " + st.Message()
}

type permissionsPage struct {
	Unavailable string
	Sections    []struct {
		ID, Kind, Unavailable string
		Permissions           *console.PermissionsView
	}
}

func readPermissionsTab(t *testing.T, srv *httptest.Server, c permissionsCase) *console.PermissionsView {
	t.Helper()
	q := url.Values{"project": {c.project}, "name": c.path}
	resp, err := http.Get(srv.URL + "/api/detail/" + c.service + "?" + q.Encode())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var page permissionsPage
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		t.Fatal(err)
	}
	for _, s := range page.Sections {
		if s.ID == "permissions" {
			if s.Kind != string(console.KindPermissions) || s.Unavailable != "" || s.Permissions == nil {
				t.Fatalf("%v: the Permissions tab is %+v", c.path, s)
			}
			return s.Permissions
		}
	}
	t.Fatalf("%v has no Permissions tab (unavailable %q)", c.path, page.Unavailable)
	return nil
}

func hasPermissionsTab(t *testing.T, srv *httptest.Server, service, project string, path ...string) bool {
	t.Helper()
	q := url.Values{"project": {project}, "name": path}
	resp, err := http.Get(srv.URL + "/api/detail/" + service + "?" + q.Encode())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var page permissionsPage
	_ = json.NewDecoder(resp.Body).Decode(&page)
	for _, s := range page.Sections {
		if s.ID == "permissions" {
			return true
		}
	}
	return false
}

func changePermissions(t *testing.T, srv *httptest.Server, c permissionsCase, body map[string]any) (int, string) {
	t.Helper()
	body["Path"] = c.path
	b, _ := json.Marshal(body)
	resp, err := http.Post(srv.URL+"/api/permissions/"+c.service+"?project="+url.QueryEscape(c.project),
		"application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	var e struct{ Error string }
	_ = json.Unmarshal(out, &e)
	if e.Error != "" {
		return resp.StatusCode, e.Error
	}
	return resp.StatusCode, string(out)
}

func membersOf(p *iampb.Policy, role string) []string {
	for _, b := range p.GetBindings() {
		if b.GetRole() == role {
			return b.GetMembers()
		}
	}
	return nil
}

// exercisePermissions is the tab's contract, the same on every service: the
// note, a grant the service reads back, a stale etag refused in the
// service's words, a removal, and a refusal of what cannot be sent.
func exercisePermissions(t *testing.T, p console.Provider, c permissionsCase) {
	t.Helper()
	srv := httptest.NewServer(console.New("127.0.0.1:0", nil, p).Handler())
	t.Cleanup(srv.Close)
	const role = "roles/viewer"
	ada, svc := "user:ada@example.com", "serviceAccount:svc@example.iam.gserviceaccount.com"

	view := readPermissionsTab(t, srv, c)
	if view.Note != console.PermissionsNote || !strings.Contains(view.Note, "does not enforce") {
		t.Errorf("the tab's note is %q", view.Note)
	}
	if !strings.HasPrefix(view.Link, compatibilityDoc) || view.RoleHelp == "" || view.PrincipalHelp == "" {
		t.Errorf("the tab links %q with role help %q and principal help %q", view.Link, view.RoleHelp, view.PrincipalHelp)
	}
	if len(view.Bindings) != 0 {
		t.Errorf("a new resource's tab lists %v", view.Bindings)
	}

	// Grant access: what the service's GetIamPolicy then reads.
	if code, body := changePermissions(t, srv, c, map[string]any{"Etag": view.Etag,
		"Grant": map[string]any{"Members": []string{" " + ada + " ", svc, ada}, "Role": role}}); code != http.StatusOK {
		t.Fatalf("Grant access = %d: %s", code, body)
	}
	if got := membersOf(c.readBack(t), role); !slices.Equal(got, []string{ada, svc}) {
		t.Errorf("after Grant access the service reads %s held by %v", role, got)
	}
	granted := readPermissionsTab(t, srv, c)
	if granted.Etag == view.Etag || len(granted.Bindings) != 1 {
		t.Errorf("after Grant access the tab reads %+v (etag was %q)", granted.Policy, view.Etag)
	}

	// A change made elsewhere since the page read the policy: the page's
	// etag is stale, and the service refuses it, in its own words.
	c.write(t, &iampb.Binding{Role: "roles/editor", Members: []string{"group:other@example.com"}})
	code, body := changePermissions(t, srv, c, map[string]any{"Etag": granted.Etag,
		"Grant": map[string]any{"Members": []string{"user:late@example.com"}, "Role": role}})
	if code != http.StatusBadRequest || body == "" {
		t.Fatalf("a grant with a stale etag = %d %s; want it refused", code, body)
	}
	if want := c.refusal(); body != want {
		t.Errorf("a grant with a stale etag was refused with %q; want the service's %q", body, want)
	}
	after := c.readBack(t)
	if slices.Contains(membersOf(after, role), "user:late@example.com") || len(membersOf(after, "roles/editor")) != 1 {
		t.Errorf("a refused grant changed the policy: %v", after.GetBindings())
	}
	code, body = changePermissions(t, srv, c, map[string]any{"Etag": granted.Etag,
		"Remove": map[string]any{"Member": ada, "Role": role}})
	if want := c.refusal(); code != http.StatusBadRequest || body != want {
		t.Errorf("a removal with a stale etag = %d %q; want the service's %q", code, body, want)
	}

	// Remove principal, with the current etag.
	current := readPermissionsTab(t, srv, c)
	if code, body := changePermissions(t, srv, c, map[string]any{"Etag": current.Etag,
		"Remove": map[string]any{"Member": ada, "Role": role}}); code != http.StatusOK {
		t.Fatalf("Remove principal = %d: %s", code, body)
	}
	if got := membersOf(c.readBack(t), role); !slices.Equal(got, []string{svc}) {
		t.Errorf("after removing %s the service reads %s held by %v", ada, role, got)
	}
	current = readPermissionsTab(t, srv, c)
	if code, body := changePermissions(t, srv, c, map[string]any{"Etag": current.Etag,
		"Remove": map[string]any{"Member": svc, "Role": role}}); code != http.StatusOK {
		t.Fatalf("removing the last principal = %d: %s", code, body)
	}
	if got := membersOf(c.readBack(t), role); got != nil {
		t.Errorf("removing the last principal left %s held by %v", role, got)
	}

	// Refused before the service: no principal, no etag, both changes, a
	// principal who holds no such role.
	current = readPermissionsTab(t, srv, c)
	for what, body := range map[string]map[string]any{
		"no principal": {"Etag": current.Etag, "Grant": map[string]any{"Members": []string{" "}, "Role": role}},
		"no etag":      {"Grant": map[string]any{"Members": []string{ada}, "Role": role}},
		"both":         {"Etag": current.Etag, "Grant": map[string]any{"Members": []string{ada}, "Role": role}, "Remove": map[string]any{"Member": ada, "Role": role}},
		"not held":     {"Etag": current.Etag, "Remove": map[string]any{"Member": ada, "Role": role}},
	} {
		if code, msg := changePermissions(t, srv, c, body); code != http.StatusBadRequest {
			t.Errorf("a change with %s = %d %s; want 400", what, code, msg)
		}
	}
	// An empty role is the service's to refuse, and it does.
	if code, msg := changePermissions(t, srv, c, map[string]any{"Etag": current.Etag,
		"Grant": map[string]any{"Members": []string{ada}, "Role": ""}}); code != http.StatusBadRequest || !strings.Contains(msg, "role") {
		t.Errorf("a grant with no role = %d %q; want the service's refusal naming the role", code, msg)
	}
}

func TestTasksPermissionsThroughGetAndSetIamPolicy(t *testing.T) {
	t.Parallel()
	p := tasksProvider{svc: &tasksService{store: tasks.NewStore(store.NewMemory())}}
	ctx := context.Background()
	name, err := p.Create(ctx, "iam-proj", map[string]string{"name": "iam-q", "location": "us-central1"})
	if err != nil {
		t.Fatal(err)
	}
	api := tasks.NewGRPCServer(p.svc.Store())
	var lastStale error
	c := permissionsCase{service: "tasks", project: "iam-proj", path: []string{name},
		readBack: func(t *testing.T) *iampb.Policy {
			pol, err := api.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: name})
			if err != nil {
				t.Fatal(err)
			}
			return pol
		},
		write: func(t *testing.T, add *iampb.Binding) {
			pol, _ := api.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: name})
			stale := pol.GetEtag()
			pol.Bindings = append(pol.Bindings, add)
			if _, err := api.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: name, Policy: pol}); err != nil {
				t.Fatal(err)
			}
			pol.Etag = stale
			_, lastStale = api.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: name, Policy: pol})
		},
		refusal: func() string { return grpcRefusal(lastStale) },
	}
	exercisePermissions(t, p, c)

	srv := httptest.NewServer(console.New("127.0.0.1:0", nil, p).Handler())
	t.Cleanup(srv.Close)
	if hasPermissionsTab(t, srv, "tasks", "iam-proj", name, "some-task") {
		t.Error("a task's page has a Permissions tab; only a queue has a policy")
	}
	if (tasksProvider{}).PolicyOn(context.Background(), []string{name, "t"}) != nil {
		t.Error("PolicyOn a task")
	}
	if _, err := p.GetPolicy(ctx, "other-proj", []string{name}); err == nil {
		t.Error("another project's queue policy was read")
	}
}

func TestSecretPermissionsThroughGetAndSetIamPolicy(t *testing.T) {
	t.Parallel()
	p := secretsFixture(t)
	ctx := context.Background()
	name, err := p.Create(ctx, "iam-proj", map[string]string{"secretId": "iam-s", "payload": "v"})
	if err != nil {
		t.Fatal(err)
	}
	api := secrets.NewGRPCServer(p.svc.Store())
	resource := "projects/iam-proj/secrets/iam-s"
	var lastStale error
	c := permissionsCase{service: "secrets", project: "iam-proj", path: []string{name},
		readBack: func(t *testing.T) *iampb.Policy {
			pol, err := api.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: resource})
			if err != nil {
				t.Fatal(err)
			}
			return pol
		},
		write: func(t *testing.T, add *iampb.Binding) {
			pol, _ := api.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: resource})
			stale := pol.GetEtag()
			pol.Bindings = append(pol.Bindings, add)
			if _, err := api.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: resource, Policy: pol}); err != nil {
				t.Fatal(err)
			}
			pol.Etag = stale
			_, lastStale = api.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: resource, Policy: pol})
		},
		refusal: func() string { return grpcRefusal(lastStale) },
	}
	exercisePermissions(t, p, c)

	srv := httptest.NewServer(console.New("127.0.0.1:0", nil, p).Handler())
	t.Cleanup(srv.Close)
	if hasPermissionsTab(t, srv, "secrets", "iam-proj", name, "1") {
		t.Error("a secret version's page has a Permissions tab; only the secret has a policy")
	}
}

func TestKMSPermissionsOnARingAndAKey(t *testing.T) {
	ctx := context.Background()
	p, client := kmsEditFixture(t)
	ring, err := client.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: "projects/iam-proj/locations/global", KeyRingId: "r"})
	if err != nil {
		t.Fatal(err)
	}
	key, err := client.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "k",
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}})
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []struct {
		resource string
		path     []string
	}{
		{ring.GetName(), []string{ring.GetName()}},
		{key.GetName(), []string{ring.GetName(), "k"}},
	} {
		// The official client's IAM handle, against the service's port.
		h := client.ResourceIAM(target.resource)
		var lastStale error
		c := permissionsCase{service: "kms", project: "iam-proj", path: target.path,
			readBack: func(t *testing.T) *iampb.Policy {
				pol, err := h.Policy(ctx)
				if err != nil {
					t.Fatal(err)
				}
				return pol.InternalProto
			},
			write: func(t *testing.T, add *iampb.Binding) {
				pol, _ := h.Policy(ctx)
				stale := pol.InternalProto.GetEtag()
				pol.InternalProto.Bindings = append(pol.InternalProto.Bindings, add)
				if err := h.SetPolicy(ctx, pol); err != nil {
					t.Fatal(err)
				}
				pol.InternalProto.Etag = stale
				lastStale = h.SetPolicy(ctx, pol)
			},
			refusal: func() string { return grpcRefusal(lastStale) },
		}
		exercisePermissions(t, p, c)
	}
	srv := httptest.NewServer(console.New("127.0.0.1:0", nil, p).Handler())
	t.Cleanup(srv.Close)
	if hasPermissionsTab(t, srv, "kms", "iam-proj", ring.GetName(), "k", "1") {
		t.Error("a key version's page has a Permissions tab; a version has no policy")
	}
	if _, err := p.GetPolicy(ctx, "other-proj", []string{ring.GetName()}); err == nil {
		t.Error("another project's key ring policy was read")
	}
}

func TestStoragePermissionsThroughBucketIAM(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p, client := newObjectOpsProvider(t)
	putObject(t, client, "ops", "dir/a.txt", "a", nil)
	h := client.Bucket("ops").IAM()
	var lastStale error
	c := permissionsCase{service: "storage", project: "p", path: []string{"ops"},
		readBack: func(t *testing.T) *iampb.Policy {
			pol, err := h.Policy(ctx)
			if err != nil {
				t.Fatal(err)
			}
			return pol.InternalProto
		},
		write: func(t *testing.T, add *iampb.Binding) {
			pol, _ := h.Policy(ctx)
			stale := pol.InternalProto.GetEtag()
			pol.InternalProto.Bindings = append(pol.InternalProto.Bindings, add)
			if err := h.SetPolicy(ctx, pol); err != nil {
				t.Fatal(err)
			}
			lastStale = h.SetPolicy(ctx, &iam.Policy{InternalProto: &iampb.Policy{Bindings: pol.InternalProto.Bindings, Etag: stale}})
		},
		// The client's error carries a gRPC status, which is how the
		// console words it: FailedPrecondition and the JSON API's 412.
		refusal: func() string {
			if lastStale == nil {
				return "a stale etag was accepted"
			}
			return grpcRefusal(lastStale)
		},
	}
	exercisePermissions(t, p, c)
	if lastStale == nil || !strings.Contains(lastStale.Error(), "412") {
		t.Errorf("the official client's setIamPolicy with a stale etag = %v; want 412", lastStale)
	}

	srv := httptest.NewServer(console.New("127.0.0.1:0", nil, p).Handler())
	t.Cleanup(srv.Close)
	if hasPermissionsTab(t, srv, "storage", "p", "ops", "dir") {
		t.Error("a folder's page has a Permissions tab; only a bucket has a policy")
	}
	if hasPermissionsTab(t, srv, "storage", "p", objectPath("ops", "dir/a.txt")...) {
		t.Error("an object's page has a Permissions tab; object IAM is not implemented")
	}
}

// A managed folder's page, the folder page of its prefix, has a Permissions
// tab that reads and writes its own policy through managedFolders.getIamPolicy
// and setIamPolicy on the JSON API, leaving the bucket's alone; a plain folder
// beside it has none, and neither route accepts a change to one (#847).
func TestStoragePermissionsOnAManagedFolder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p, client := newObjectOpsProvider(t)
	if _, err := client.Bucket("ops").Update(ctx, storage.BucketAttrsToUpdate{
		UniformBucketLevelAccess: &storage.UniformBucketLevelAccess{Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	if err := p.ActAt(ctx, "p", []string{"ops"}, "createmanagedfolder", map[string]string{"name": "team/reports"}); err != nil {
		t.Fatal(err)
	}
	putObject(t, client, "ops", "plain/a.txt", "a", nil)
	s, err := jsonAPI(ctx, p.endpoint)
	if err != nil {
		t.Fatal(err)
	}
	const folder = "team/reports/"
	var lastStale error
	c := permissionsCase{service: "storage", project: "p", path: []string{"ops", "team", "reports"},
		readBack: func(t *testing.T) *iampb.Policy {
			pol, err := s.ManagedFolders.GetIamPolicy("ops", folder).Context(ctx).Do()
			if err != nil {
				t.Fatal(err)
			}
			out := &iampb.Policy{Etag: []byte(pol.Etag)}
			for _, b := range pol.Bindings {
				out.Bindings = append(out.Bindings, &iampb.Binding{Role: b.Role, Members: b.Members})
			}
			return out
		},
		write: func(t *testing.T, add *iampb.Binding) {
			pol, err := s.ManagedFolders.GetIamPolicy("ops", folder).Context(ctx).Do()
			if err != nil {
				t.Fatal(err)
			}
			stale := pol.Etag
			pol.Bindings = append(pol.Bindings, &storagev1.PolicyBindings{Role: add.GetRole(), Members: add.GetMembers()})
			if _, err := s.ManagedFolders.SetIamPolicy("ops", folder, pol).Context(ctx).Do(); err != nil {
				t.Fatal(err)
			}
			pol.Etag = stale
			_, lastStale = s.ManagedFolders.SetIamPolicy("ops", folder, pol).Context(ctx).Do()
		},
		refusal: func() string {
			if lastStale == nil {
				return "a stale etag was accepted"
			}
			// The JSON API client's error carries a gRPC status too:
			// FailedPrecondition and the 412.
			return grpcRefusal(lastStale)
		},
	}
	exercisePermissions(t, p, c)
	if httpCode := func() int {
		var e *googleapi.Error
		if errors.As(lastStale, &e) {
			return e.Code
		}
		return 0
	}(); httpCode != http.StatusPreconditionFailed {
		t.Errorf("the JSON API client's setIamPolicy with a stale etag = %v; want 412", lastStale)
	}
	if bp, err := client.Bucket("ops").IAM().Policy(ctx); err != nil || len(bp.Roles()) != 0 {
		t.Errorf("the bucket's policy took the managed folder's bindings: %v, %v", bp.Roles(), err)
	}

	d, err := p.Detail(ctx, "p", c.path)
	if err != nil || !slices.Contains(d.Summary, console.Property{Label: "Type", Value: "Managed folder"}) {
		t.Errorf("the managed folder's page summary is %+v, %v", d.Summary, err)
	}
	srv := httptest.NewServer(console.New("127.0.0.1:0", nil, p).Handler())
	t.Cleanup(srv.Close)
	for _, path := range [][]string{{"ops", "team"}, {"ops", "plain"}, {"ops", "absent"}} {
		if hasPermissionsTab(t, srv, "storage", "p", path...) {
			t.Errorf("%v, not a managed folder, has a Permissions tab", path)
		}
		if code, _ := changePermissions(t, srv, permissionsCase{service: "storage", project: "p", path: path},
			map[string]any{"Etag": "CAE=", "Grant": map[string]any{"Members": []string{"user:a@example.com"}, "Role": "roles/viewer"}}); code != http.StatusBadRequest {
			t.Errorf("a grant on %v = %d; want it refused", path, code)
		}
	}
	d, err = p.Detail(ctx, "p", []string{"ops", "plain"})
	if err != nil || slices.Contains(d.Summary, console.Property{Label: "Type", Value: "Managed folder"}) {
		t.Errorf("a plain folder's summary is %+v, %v", d.Summary, err)
	}
}

// A condition cannot be written back, since no service here stores one; the
// conversion refuses rather than drop it.
func TestPermissionsNeverDropACondition(t *testing.T) {
	_, err := policyToProto(console.Policy{Bindings: []console.Binding{{Role: "roles/viewer", Members: []string{"user:a@example.com"}, Condition: "t: true"}}}, nil)
	if err == nil {
		t.Error("a binding with a condition was converted without it")
	}
	if _, err := protoEtag(console.Policy{Etag: "not base64!"}); err == nil {
		t.Error("an etag the console did not read was decoded")
	}
}
