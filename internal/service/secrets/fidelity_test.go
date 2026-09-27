package secrets

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"

	secretmanagerpb "cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func automatic() *secretmanagerpb.Replication {
	return &secretmanagerpb.Replication{Replication: &secretmanagerpb.Replication_Automatic_{Automatic: &secretmanagerpb.Replication_Automatic{}}}
}

// Fields Google honours and this server would drop are refused by name, and
// nothing is stored (#580).
func TestCreateSecretRefusesFieldsItWouldDrop(t *testing.T) {
	t.Parallel()
	g, s := newGRPC(t)
	ctx := context.Background()
	cases := map[string]*secretmanagerpb.Secret{
		"ttl":                 {Replication: automatic(), Expiration: &secretmanagerpb.Secret_Ttl{Ttl: durationpb.New(3600e9)}},
		"expire_time":         {Replication: automatic(), Expiration: &secretmanagerpb.Secret_ExpireTime{ExpireTime: timestamppb.Now()}},
		"rotation":            {Replication: automatic(), Rotation: &secretmanagerpb.Rotation{RotationPeriod: durationpb.New(86400e9)}},
		"topics":              {Replication: automatic(), Topics: []*secretmanagerpb.Topic{{Name: "projects/demo/topics/t"}}},
		"version_aliases":     {Replication: automatic(), VersionAliases: map[string]int64{"current": 1}},
		"version_destroy_ttl": {Replication: automatic(), VersionDestroyTtl: durationpb.New(86400e9)},
	}
	for field, sec := range cases {
		_, err := g.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{Parent: "projects/demo", SecretId: "s-" + strings.ReplaceAll(field, "_", "-"), Secret: sec})
		wantGRPCCode(t, err, codes.Unimplemented)
		if err != nil && !strings.Contains(err.Error(), field) {
			t.Errorf("%s: the refusal does not name the field: %v", field, err)
		}
	}
	_, err := g.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{Parent: "projects/demo", SecretId: "no-replicas", Secret: &secretmanagerpb.Secret{
		Replication: &secretmanagerpb.Replication{Replication: &secretmanagerpb.Replication_UserManaged_{UserManaged: &secretmanagerpb.Replication_UserManaged{}}}}})
	wantGRPCCode(t, err, codes.InvalidArgument)
	if all, _ := s.ListSecrets("demo"); len(all) != 0 {
		t.Errorf("a refused create stored %d secrets", len(all))
	}
}

// The same body gets the same answer over REST as over gRPC; before, REST
// said 400 for an unknown field and gRPC stored the secret without it.
func TestRESTCreateAgreesWithGRPC(t *testing.T) {
	t.Parallel()
	srv, _ := newREST(t)
	code, body := do(t, srv, "POST", "/v1/projects/demo/secrets?secretId=ttl", `{"replication":{"automatic":{}},"expireTime":"2030-01-01T00:00:00Z"}`)
	if code != http.StatusNotImplemented || !strings.Contains(body, "expire_time") {
		t.Errorf("REST create with expireTime = %d %s; want 501 naming expire_time", code, body)
	}
	if code, body := do(t, srv, "POST", "/v1/projects/demo/secrets?secretId=ok", `{"replication":{"automatic":{}},"labels":{"env":"dev"}}`); code != http.StatusOK || !strings.Contains(body, `"env":"dev"`) {
		t.Errorf("REST create = %d %s", code, body)
	}
	if code, _ := do(t, srv, "POST", "/v1/projects/demo/secrets?secretId=empty", ""); code != http.StatusOK {
		t.Errorf("REST create with no body = %d; replication defaults to automatic", code)
	}
}

// A filter was ignored and every secret returned, which reads as a filter
// that matched them all.
func TestListFiltersAreRefusedNotIgnored(t *testing.T) {
	t.Parallel()
	g, s := newGRPC(t)
	seed(t, s, "demo", "a", "x")
	_, err := g.ListSecrets(context.Background(), &secretmanagerpb.ListSecretsRequest{Parent: "projects/demo", Filter: "labels.env=prod"})
	wantGRPCCode(t, err, codes.Unimplemented)
	_, err = g.ListSecretVersions(context.Background(), &secretmanagerpb.ListSecretVersionsRequest{Parent: "projects/demo/secrets/a", Filter: "state:ENABLED"})
	wantGRPCCode(t, err, codes.Unimplemented)
	srv, rs := newREST(t)
	seed(t, rs, "demo", "a", "x")
	if code, _ := do(t, srv, "GET", "/v1/projects/demo/secrets?filter=labels.env%3Dprod", ""); code != http.StatusNotImplemented {
		t.Errorf("REST list with a filter = %d; want 501", code)
	}
}

// A stale etag fails with FAILED_PRECONDITION, as Google documents; the
// current one, and none, succeed (#580).
func TestEtagsAreCompared(t *testing.T) {
	t.Parallel()
	g, s := newGRPC(t)
	ctx := context.Background()
	v := seed(t, s, "demo", "a", "one")[0]
	_, err := g.DisableSecretVersion(ctx, &secretmanagerpb.DisableSecretVersionRequest{Name: v.Name, Etag: `"stale"`})
	wantGRPCCode(t, err, codes.FailedPrecondition)
	if _, err := g.DisableSecretVersion(ctx, &secretmanagerpb.DisableSecretVersionRequest{Name: v.Name, Etag: v.Etag}); err != nil {
		t.Fatalf("with the current etag: %v", err)
	}
	if _, err := g.EnableSecretVersion(ctx, &secretmanagerpb.EnableSecretVersionRequest{Name: v.Name}); err != nil {
		t.Fatalf("with no etag: %v", err)
	}
	_, err = g.DestroySecretVersion(ctx, &secretmanagerpb.DestroySecretVersionRequest{Name: v.Name, Etag: `"stale"`})
	wantGRPCCode(t, err, codes.FailedPrecondition)
	_, err = g.DeleteSecret(ctx, &secretmanagerpb.DeleteSecretRequest{Name: "projects/demo/secrets/a", Etag: `"stale"`})
	wantGRPCCode(t, err, codes.FailedPrecondition)
	sec, _ := s.GetSecret("demo", "a")
	_, err = g.UpdateSecret(ctx, &secretmanagerpb.UpdateSecretRequest{Secret: &secretmanagerpb.Secret{Name: sec.Name, Etag: `"stale"`, Labels: map[string]string{"a": "b"}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"labels"}}})
	wantGRPCCode(t, err, codes.FailedPrecondition)
	if _, err := g.DeleteSecret(ctx, &secretmanagerpb.DeleteSecretRequest{Name: sec.Name, Etag: sec.Etag}); err != nil {
		t.Errorf("delete with the current etag: %v", err)
	}

	srv, rs := newREST(t)
	rv := seed(t, rs, "demo", "b", "one")[0]
	if code, _ := do(t, srv, "POST", "/v1/projects/demo/secrets/b/versions/1:disable", `{"etag":"\"stale\""}`); code != http.StatusBadRequest {
		t.Errorf("REST disable with a stale etag = %d; want 400 (FAILED_PRECONDITION)", code)
	}
	if code, body := do(t, srv, "POST", "/v1/projects/demo/secrets/b/versions/1:disable", `{"etag":`+strconv.Quote(rv.Etag)+`}`); code != http.StatusOK {
		t.Errorf("REST disable with the current etag = %d %s", code, body)
	}
	if code, _ := do(t, srv, "DELETE", "/v1/projects/demo/secrets/b?etag=%22stale%22", ""); code != http.StatusBadRequest {
		t.Errorf("REST delete with a stale etag = %d; want 400", code)
	}
}

// A client-supplied checksum is verified, and every access carries one: the
// official samples verify it and fail without it (#580).
func TestPayloadChecksums(t *testing.T) {
	t.Parallel()
	g, _ := newGRPC(t)
	ctx := context.Background()
	if _, err := g.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{Parent: "projects/demo", SecretId: "c", Secret: &secretmanagerpb.Secret{Replication: automatic()}}); err != nil {
		t.Fatal(err)
	}
	data := []byte("s3cret")
	_, err := g.AddSecretVersion(ctx, &secretmanagerpb.AddSecretVersionRequest{Parent: "projects/demo/secrets/c",
		Payload: &secretmanagerpb.SecretPayload{Data: data, DataCrc32C: proto.Int64(payloadCRC32C(data) + 1)}})
	wantGRPCCode(t, err, codes.InvalidArgument)
	if _, err := g.AddSecretVersion(ctx, &secretmanagerpb.AddSecretVersionRequest{Parent: "projects/demo/secrets/c",
		Payload: &secretmanagerpb.SecretPayload{Data: data, DataCrc32C: proto.Int64(payloadCRC32C(data))}}); err != nil {
		t.Fatalf("with the right checksum: %v", err)
	}
	got, err := g.AccessSecretVersion(ctx, &secretmanagerpb.AccessSecretVersionRequest{Name: "projects/demo/secrets/c/versions/latest"})
	if err != nil {
		t.Fatal(err)
	}
	// 0x982827d8 is CRC32C("s3cret"), computed outside this package.
	if got.GetPayload().DataCrc32C == nil || got.GetPayload().GetDataCrc32C() != 0x982827d8 {
		t.Errorf("access data_crc32c = %v; want %d", got.GetPayload().DataCrc32C, 0x982827d8)
	}
}
