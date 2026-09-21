package secrets

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	secretmanagerpb "cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/identity-wael/cloudburrow/internal/transport/rest"
)

func newGRPC(t *testing.T) (*GRPCServer, *Store) {
	t.Helper()
	s := newTestStore(t)
	return NewGRPCServer(s), s
}

func wantGRPCCode(t *testing.T, err error, want codes.Code) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected %s, got no error", want)
	}
	if got := status.Code(err); got != want {
		t.Errorf("gRPC code = %s, want %s (%v)", got, want, err)
	}
}

// --- gRPC --------------------------------------------------------------

func TestGRPCSecretAndVersionLifecycle(t *testing.T) {
	t.Parallel()
	g, _ := newGRPC(t)
	ctx := context.Background()

	sec, err := g.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{
		Parent:   "projects/demo",
		SecretId: "api-key",
		Secret: &secretmanagerpb.Secret{
			Replication: &secretmanagerpb.Replication{
				Replication: &secretmanagerpb.Replication_Automatic_{
					Automatic: &secretmanagerpb.Replication_Automatic{},
				},
			},
			Labels: map[string]string{"env": "dev"},
		},
	})
	if err != nil {
		t.Fatalf("CreateSecret: %v", err)
	}
	if sec.GetName() != "projects/demo/secrets/api-key" {
		t.Errorf("name = %q", sec.GetName())
	}
	// Replication is a required field of the resource, so a response without
	// one would not round-trip through a client.
	if sec.GetReplication().GetAutomatic() == nil {
		t.Error("the created secret reports no replication")
	}

	added, err := g.AddSecretVersion(ctx, &secretmanagerpb.AddSecretVersionRequest{
		Parent:  sec.GetName(),
		Payload: &secretmanagerpb.SecretPayload{Data: []byte("s3cret")},
	})
	if err != nil {
		t.Fatalf("AddSecretVersion: %v", err)
	}
	if added.GetState() != secretmanagerpb.SecretVersion_ENABLED {
		t.Errorf("state = %v", added.GetState())
	}

	accessed, err := g.AccessSecretVersion(ctx, &secretmanagerpb.AccessSecretVersionRequest{
		Name: sec.GetName() + "/versions/latest",
	})
	if err != nil {
		t.Fatalf("AccessSecretVersion: %v", err)
	}
	if string(accessed.GetPayload().GetData()) != "s3cret" {
		t.Errorf("payload = %q", accessed.GetPayload().GetData())
	}
	// The concrete name is returned even for "latest", so a client can record
	// which bytes it actually got.
	if accessed.GetName() != added.GetName() {
		t.Errorf("access returned name %q, want the concrete %q", accessed.GetName(), added.GetName())
	}

	list, err := g.ListSecrets(ctx, &secretmanagerpb.ListSecretsRequest{Parent: "projects/demo"})
	if err != nil {
		t.Fatalf("ListSecrets: %v", err)
	}
	if len(list.GetSecrets()) != 1 || list.GetTotalSize() != 1 {
		t.Errorf("list = %d secrets, total %d", len(list.GetSecrets()), list.GetTotalSize())
	}

	if _, err := g.DeleteSecret(ctx, &secretmanagerpb.DeleteSecretRequest{Name: sec.GetName()}); err != nil {
		t.Fatalf("DeleteSecret: %v", err)
	}
	_, err = g.GetSecret(ctx, &secretmanagerpb.GetSecretRequest{Name: sec.GetName()})
	wantGRPCCode(t, err, codes.NotFound)
}

func TestGRPCVersionStateTransitions(t *testing.T) {
	t.Parallel()
	g, s := newGRPC(t)
	ctx := context.Background()
	seed(t, s, "demo", "k", "one")
	name := "projects/demo/secrets/k/versions/1"

	disabled, err := g.DisableSecretVersion(ctx, &secretmanagerpb.DisableSecretVersionRequest{Name: name})
	if err != nil {
		t.Fatalf("DisableSecretVersion: %v", err)
	}
	if disabled.GetState() != secretmanagerpb.SecretVersion_DISABLED {
		t.Errorf("state = %v", disabled.GetState())
	}
	_, err = g.AccessSecretVersion(ctx, &secretmanagerpb.AccessSecretVersionRequest{Name: name})
	wantGRPCCode(t, err, codes.FailedPrecondition)

	if _, err := g.EnableSecretVersion(ctx, &secretmanagerpb.EnableSecretVersionRequest{Name: name}); err != nil {
		t.Fatalf("EnableSecretVersion: %v", err)
	}
	if _, err := g.AccessSecretVersion(ctx, &secretmanagerpb.AccessSecretVersionRequest{Name: name}); err != nil {
		t.Errorf("a re-enabled version is inaccessible: %v", err)
	}

	destroyed, err := g.DestroySecretVersion(ctx, &secretmanagerpb.DestroySecretVersionRequest{Name: name})
	if err != nil {
		t.Fatalf("DestroySecretVersion: %v", err)
	}
	if destroyed.GetState() != secretmanagerpb.SecretVersion_DESTROYED {
		t.Errorf("state = %v", destroyed.GetState())
	}
	if destroyed.GetDestroyTime() == nil {
		t.Error("no destroy time was reported")
	}
	_, err = g.EnableSecretVersion(ctx, &secretmanagerpb.EnableSecretVersionRequest{Name: name})
	wantGRPCCode(t, err, codes.FailedPrecondition)
}

// An empty mask would otherwise silently clear every label.
func TestGRPCUpdateSecretRequiresAMask(t *testing.T) {
	t.Parallel()
	g, s := newGRPC(t)
	ctx := context.Background()
	seed(t, s, "demo", "k")

	_, err := g.UpdateSecret(ctx, &secretmanagerpb.UpdateSecretRequest{
		Secret: &secretmanagerpb.Secret{Name: "projects/demo/secrets/k"},
	})
	wantGRPCCode(t, err, codes.InvalidArgument)

	// A field that is not mutable must be refused rather than ignored: a
	// client believing it renamed a secret is worse than an error.
	_, err = g.UpdateSecret(ctx, &secretmanagerpb.UpdateSecretRequest{
		Secret:     &secretmanagerpb.Secret{Name: "projects/demo/secrets/k"},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"name"}},
	})
	wantGRPCCode(t, err, codes.InvalidArgument)

	updated, err := g.UpdateSecret(ctx, &secretmanagerpb.UpdateSecretRequest{
		Secret: &secretmanagerpb.Secret{
			Name:   "projects/demo/secrets/k",
			Labels: map[string]string{"env": "prod"},
		},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"labels"}},
	})
	if err != nil {
		t.Fatalf("UpdateSecret: %v", err)
	}
	if updated.GetLabels()["env"] != "prod" {
		t.Errorf("labels = %v", updated.GetLabels())
	}
}

func TestGRPCListVersionsIsNewestFirstAndPagesWithoutGaps(t *testing.T) {
	t.Parallel()
	g, s := newGRPC(t)
	ctx := context.Background()

	if _, err := s.CreateSecret("demo", "k", nil, nil, ""); err != nil {
		t.Fatal(err)
	}
	const total = 12
	for i := 0; i < total; i++ {
		if _, err := s.AddVersion("demo", "k", []byte("v")); err != nil {
			t.Fatal(err)
		}
	}

	var seen []string
	token := ""
	for {
		resp, err := g.ListSecretVersions(ctx, &secretmanagerpb.ListSecretVersionsRequest{
			Parent: "projects/demo/secrets/k", PageSize: 5, PageToken: token,
		})
		if err != nil {
			t.Fatalf("ListSecretVersions: %v", err)
		}
		for _, v := range resp.GetVersions() {
			seen = append(seen, v.GetName())
		}
		token = resp.GetNextPageToken()
		if token == "" {
			break
		}
		if len(seen) > total {
			t.Fatal("paging did not terminate")
		}
	}

	if len(seen) != total {
		t.Fatalf("a full walk saw %d versions, want %d", len(seen), total)
	}
	unique := map[string]bool{}
	for _, n := range seen {
		if unique[n] {
			t.Fatalf("paging returned %s twice", n)
		}
		unique[n] = true
	}
	// Newest first, across page boundaries.
	if !strings.HasSuffix(seen[0], "/versions/12") {
		t.Errorf("first version = %q, want the newest", seen[0])
	}
	if !strings.HasSuffix(seen[len(seen)-1], "/versions/1") {
		t.Errorf("last version = %q, want the oldest", seen[len(seen)-1])
	}
}

func TestGRPCRejectsMalformedNames(t *testing.T) {
	t.Parallel()
	g, _ := newGRPC(t)
	ctx := context.Background()

	_, err := g.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{Parent: "demo", SecretId: "k"})
	wantGRPCCode(t, err, codes.InvalidArgument)

	_, err = g.GetSecret(ctx, &secretmanagerpb.GetSecretRequest{Name: "projects/demo/secrets"})
	wantGRPCCode(t, err, codes.InvalidArgument)

	_, err = g.AccessSecretVersion(ctx, &secretmanagerpb.AccessSecretVersionRequest{
		Name: "projects/demo/secrets/k/versions/newest",
	})
	wantGRPCCode(t, err, codes.InvalidArgument)
}

// --- REST --------------------------------------------------------------

func newREST(t *testing.T) (*httptest.Server, *Store) {
	t.Helper()
	s := newTestStore(t)
	r := rest.NewRouter()
	NewRESTServer(s).Routes(r)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv, s
}

func do(t *testing.T, srv *httptest.Server, method, path, body string) (int, string) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, srv.URL+path, rdr)
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(out)
}

// The REST surface must reach the same store as gRPC, or the two would drift
// into two implementations of one contract.
func TestRESTLifecycleSharesTheStoreWithGRPC(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	r := rest.NewRouter()
	NewRESTServer(s).Routes(r)
	srv := httptest.NewServer(r)
	defer srv.Close()
	g := NewGRPCServer(s)

	code, body := do(t, srv, http.MethodPost, "/v1/projects/demo/secrets?secretId=api-key",
		`{"replication":{"automatic":{}}}`)
	if code != http.StatusOK {
		t.Fatalf("create = %d: %s", code, body)
	}

	payload := base64.StdEncoding.EncodeToString([]byte("s3cret"))
	code, body = do(t, srv, http.MethodPost, "/v1/projects/demo/secrets/api-key:addVersion",
		`{"payload":{"data":"`+payload+`"}}`)
	if code != http.StatusOK {
		t.Fatalf("addVersion = %d: %s", code, body)
	}

	// Written over REST, read over gRPC.
	accessed, err := g.AccessSecretVersion(context.Background(),
		&secretmanagerpb.AccessSecretVersionRequest{Name: "projects/demo/secrets/api-key/versions/latest"})
	if err != nil {
		t.Fatalf("AccessSecretVersion over gRPC: %v", err)
	}
	if string(accessed.GetPayload().GetData()) != "s3cret" {
		t.Errorf("gRPC read back %q", accessed.GetPayload().GetData())
	}

	code, body = do(t, srv, http.MethodGet, "/v1/projects/demo/secrets/api-key/versions/latest:access", "")
	if code != http.StatusOK {
		t.Fatalf("access = %d: %s", code, body)
	}
	var access struct {
		Name    string `json:"name"`
		Payload struct {
			Data string `json:"data"`
		} `json:"payload"`
	}
	if err := json.Unmarshal([]byte(body), &access); err != nil {
		t.Fatalf("decode access: %v\n%s", err, body)
	}
	raw, err := base64.StdEncoding.DecodeString(access.Payload.Data)
	if err != nil {
		t.Fatalf("payload is not base64: %v", err)
	}
	if string(raw) != "s3cret" {
		t.Errorf("payload = %q", raw)
	}
	if !strings.HasSuffix(access.Name, "/versions/1") {
		t.Errorf("access returned name %q, want the concrete version", access.Name)
	}
}

// The JSON API carries bytes as base64, so raw text must be refused rather
// than stored as the text of the client's own encoding mistake.
func TestRESTAddVersionRequiresBase64(t *testing.T) {
	t.Parallel()
	srv, s := newREST(t)
	seed(t, s, "demo", "k")

	code, body := do(t, srv, http.MethodPost, "/v1/projects/demo/secrets/k:addVersion",
		`{"payload":{"data":"not base64!!"}}`)
	if code != http.StatusBadRequest {
		t.Errorf("addVersion with raw text = %d, want 400: %s", code, body)
	}
}

func TestRESTErrorsUseGoogleStatusCodes(t *testing.T) {
	t.Parallel()
	srv, s := newREST(t)
	seed(t, s, "demo", "k", "one")

	for _, tc := range []struct {
		name, method, path, body string
		want                     int
	}{
		{"missing secret", http.MethodGet, "/v1/projects/demo/secrets/nope", "", http.StatusNotFound},
		{"duplicate", http.MethodPost, "/v1/projects/demo/secrets?secretId=k", "", http.StatusConflict},
		{"no secretId", http.MethodPost, "/v1/projects/demo/secrets", "", http.StatusBadRequest},
		{"bad version", http.MethodGet, "/v1/projects/demo/secrets/k/versions/zero", "", http.StatusBadRequest},
		{"missing version", http.MethodGet, "/v1/projects/demo/secrets/k/versions/9", "", http.StatusNotFound},
	} {
		code, body := do(t, srv, tc.method, tc.path, tc.body)
		if code != tc.want {
			t.Errorf("%s: %d, want %d: %s", tc.name, code, tc.want, body)
		}
	}

	// A disabled version is 400 FAILED_PRECONDITION, not 404: the version
	// exists.
	if _, err := s.SetVersionState("demo", "k", "1", StateDisabled); err != nil {
		t.Fatal(err)
	}
	code, body := do(t, srv, http.MethodGet, "/v1/projects/demo/secrets/k/versions/1:access", "")
	if code == http.StatusNotFound {
		t.Errorf("a disabled version reported 404, which sends a caller looking for a creation bug: %s", body)
	}
}

func TestRESTVersionStateEndpoints(t *testing.T) {
	t.Parallel()
	srv, s := newREST(t)
	seed(t, s, "demo", "k", "one")

	for _, action := range []string{"disable", "enable", "destroy"} {
		code, body := do(t, srv, http.MethodPost,
			"/v1/projects/demo/secrets/k/versions/1:"+action, "")
		if code != http.StatusOK {
			t.Fatalf("%s = %d: %s", action, code, body)
		}
	}
	v, err := s.GetVersion("demo", "k", "1")
	if err != nil {
		t.Fatal(err)
	}
	if v.State != StateDestroyed {
		t.Errorf("state = %s, want DESTROYED", v.State)
	}
}

func TestRESTListsSecretsAndVersions(t *testing.T) {
	t.Parallel()
	srv, s := newREST(t)
	seed(t, s, "demo", "k", "a", "b")

	code, body := do(t, srv, http.MethodGet, "/v1/projects/demo/secrets", "")
	if code != http.StatusOK {
		t.Fatalf("list secrets = %d: %s", code, body)
	}
	if !strings.Contains(body, "projects/demo/secrets/k") {
		t.Errorf("listing omitted the secret: %s", body)
	}

	code, body = do(t, srv, http.MethodGet, "/v1/projects/demo/secrets/k/versions", "")
	if code != http.StatusOK {
		t.Fatalf("list versions = %d: %s", code, body)
	}
	var list struct {
		Versions []struct {
			Name  string `json:"name"`
			State string `json:"state"`
		} `json:"versions"`
		TotalSize int `json:"totalSize"`
	}
	if err := json.Unmarshal([]byte(body), &list); err != nil {
		t.Fatalf("decode: %v\n%s", err, body)
	}
	if list.TotalSize != 2 || len(list.Versions) != 2 {
		t.Fatalf("list = %+v", list)
	}
	if !strings.HasSuffix(list.Versions[0].Name, "/versions/2") {
		t.Errorf("versions are not newest first: %+v", list.Versions)
	}
}

func TestRESTDeleteRemovesTheSecret(t *testing.T) {
	t.Parallel()
	srv, s := newREST(t)
	seed(t, s, "demo", "k", "a")

	if code, body := do(t, srv, http.MethodDelete, "/v1/projects/demo/secrets/k", ""); code != http.StatusOK {
		t.Fatalf("delete = %d: %s", code, body)
	}
	if code, _ := do(t, srv, http.MethodGet, "/v1/projects/demo/secrets/k", ""); code != http.StatusNotFound {
		t.Errorf("the secret survived deletion: %d", code)
	}
}
