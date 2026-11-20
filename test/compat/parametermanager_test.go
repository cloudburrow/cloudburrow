//go:build compat

package compat

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/iam/apiv1/iampb"
	"cloud.google.com/go/kms/apiv1/kmspb"
	parametermanager "cloud.google.com/go/parametermanager/apiv1"
	pmpb "cloud.google.com/go/parametermanager/apiv1/parametermanagerpb"
	secretmanager "cloud.google.com/go/secretmanager/apiv1"
	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	gaxerr "github.com/googleapis/gax-go/v2/apierror"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// EnvParameterManager points at the local Parameter Manager endpoint
// (#2054), gRPC and JSON on one port.
const EnvParameterManager = "CLOUDBURROW_TEST_PARAMETERMANAGER"

// parameterManagerClients are the official client over gRPC and over REST,
// both pointed at the local endpoint. Parameter Manager has no emulator
// variable, so the endpoint and credentials are explicit.
func parameterManagerClients(t *testing.T, h *Harness) map[string]*parametermanager.Client {
	t.Helper()
	addr := h.Endpoint(EnvParameterManager)
	ctx := h.Context()
	g, err := parametermanager.NewClient(ctx, option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	r, err := parametermanager.NewRESTClient(ctx, option.WithEndpoint("http://"+addr), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = g.Close(); _ = r.Close() })
	return map[string]*parametermanager.Client{"grpc": g, "rest": r}
}

func pmWantCode(t *testing.T, what string, err error, code codes.Code) {
	t.Helper()
	if pmCode(err) != code {
		t.Fatalf("%s: %v, want %v", what, err, code)
	}
}

// pmCode is err's code, from gRPC or REST alike.
func pmCode(err error) codes.Code {
	var he *googleapi.Error
	if errors.As(err, &he) {
		// The REST client's error, by its HTTP status, as Google maps
		// google.rpc.Code to HTTP.
		switch he.Code {
		case 400:
			if strings.Contains(strings.ToLower(he.Message), "precondition") || strings.Contains(he.Body, "FAILED_PRECONDITION") {
				return codes.FailedPrecondition
			}
			return codes.InvalidArgument
		case 403:
			return codes.PermissionDenied
		case 404:
			return codes.NotFound
		case 409:
			return codes.AlreadyExists
		}
	}
	if ae, ok := gaxerr.FromError(err); ok && ae.GRPCStatus() != nil {
		return ae.GRPCStatus().Code()
	}
	return status.Code(err)
}

// grantSecretAccessor gives a parameter's principal
// roles/secretmanager.secretAccessor on secret, as Google's "Reference
// secrets in a parameter" has it; with IAM enforced, its renders need it.
func grantSecretAccessor(t *testing.T, sm *secretmanager.Client, secret string, p *pmpb.Parameter) {
	t.Helper()
	bindOn(t, sm.GetIamPolicy, sm.SetIamPolicy, secret,
		&iampb.Binding{Role: "roles/secretmanager.secretAccessor", Members: []string{p.GetPolicyMember().GetIamPolicyUidPrincipal()}})
}

// covers: google.cloud.parametermanager.v1.ParameterManager/CreateParameter, google.cloud.parametermanager.v1.ParameterManager/GetParameter, google.cloud.parametermanager.v1.ParameterManager/ListParameters, google.cloud.parametermanager.v1.ParameterManager/UpdateParameter, google.cloud.parametermanager.v1.ParameterManager/DeleteParameter, google.cloud.parametermanager.v1.ParameterManager/CreateParameterVersion, google.cloud.parametermanager.v1.ParameterManager/GetParameterVersion, google.cloud.parametermanager.v1.ParameterManager/ListParameterVersions, google.cloud.parametermanager.v1.ParameterManager/UpdateParameterVersion, google.cloud.parametermanager.v1.ParameterManager/DeleteParameterVersion, google.cloud.parametermanager.v1.ParameterManager/RenderParameterVersion
//
// TestParameterManagerLifecycle (#2054), with the official client over gRPC
// and over REST, for a global and a regional parameter each: create (the
// parameter's principal answered), get, list, update the labels; a second
// create, a bad ID, a bad label, a changed format and a key in another
// location are refused; a JSON parameter refuses a version that is not
// JSON, a YAML one one that is not YAML, and every format a payload over
// 1 MiB; a version is read with its payload in the FULL view and without
// it in BASIC; a disabled version answers no payload and cannot be
// rendered; an UNFORMATTED version renders as it is; a parameter with
// versions cannot be deleted, and can once they are.
func TestParameterManagerLifecycle(t *testing.T) {
	h := New(t)
	clients := parameterManagerClients(t, h)
	ctx := h.Context()
	for transport, c := range clients {
		for _, loc := range []string{"global", "us-central1"} {
			t.Run(transport+"/"+loc, func(t *testing.T) {
				parent := "projects/" + h.Project() + "/locations/" + loc
				id := fmt.Sprintf("app-%s-%d", transport, time.Now().UnixNano()%1e9)
				p, err := c.CreateParameter(ctx, &pmpb.CreateParameterRequest{Parent: parent, ParameterId: id,
					Parameter: &pmpb.Parameter{Format: pmpb.ParameterFormat_JSON, Labels: map[string]string{"env": "dev"}}})
				if err != nil {
					t.Fatalf("CreateParameter: %v", err)
				}
				name := parent + "/parameters/" + id
				if p.GetName() != name || p.GetFormat() != pmpb.ParameterFormat_JSON || p.GetCreateTime() == nil ||
					!strings.HasPrefix(p.GetPolicyMember().GetIamPolicyUidPrincipal(), "principal://parametermanager.googleapis.com/projects/") ||
					!strings.Contains(p.GetPolicyMember().GetIamPolicyUidPrincipal(), "/uid/locations/"+loc+"/parameters/") {
					t.Fatalf("created %+v", p)
				}
				_, err = c.CreateParameter(ctx, &pmpb.CreateParameterRequest{Parent: parent, ParameterId: id, Parameter: &pmpb.Parameter{}})
				pmWantCode(t, "a second CreateParameter", err, codes.AlreadyExists)
				_, err = c.CreateParameter(ctx, &pmpb.CreateParameterRequest{Parent: parent, ParameterId: "bad id!", Parameter: &pmpb.Parameter{}})
				pmWantCode(t, "a parameter ID with a space", err, codes.InvalidArgument)
				_, err = c.CreateParameter(ctx, &pmpb.CreateParameterRequest{Parent: parent, ParameterId: id + "l",
					Parameter: &pmpb.Parameter{Labels: map[string]string{"Env": "x"}}})
				pmWantCode(t, "an upper-case label key", err, codes.InvalidArgument)
				other := "projects/" + h.Project() + "/locations/europe-west1/keyRings/r/cryptoKeys/k"
				_, err = c.CreateParameter(ctx, &pmpb.CreateParameterRequest{Parent: parent, ParameterId: id + "k",
					Parameter: &pmpb.Parameter{KmsKey: &other}})
				pmWantCode(t, "a key in another location", err, codes.InvalidArgument)

				got, err := c.GetParameter(ctx, &pmpb.GetParameterRequest{Name: name})
				if err != nil || got.GetLabels()["env"] != "dev" {
					t.Fatalf("GetParameter: %+v, %v", got, err)
				}
				found := false
				it := c.ListParameters(ctx, &pmpb.ListParametersRequest{Parent: parent})
				for {
					x, err := it.Next()
					if errors.Is(err, iterator.Done) {
						break
					}
					if err != nil {
						t.Fatalf("ListParameters: %v", err)
					}
					found = found || x.GetName() == name
				}
				if !found {
					t.Fatalf("ListParameters does not list %s", name)
				}
				up, err := c.UpdateParameter(ctx, &pmpb.UpdateParameterRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"labels"}},
					Parameter: &pmpb.Parameter{Name: name, Labels: map[string]string{"env": "prod"}}})
				if err != nil || up.GetLabels()["env"] != "prod" {
					t.Fatalf("UpdateParameter labels: %+v, %v", up, err)
				}
				_, err = c.UpdateParameter(ctx, &pmpb.UpdateParameterRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"format"}},
					Parameter: &pmpb.Parameter{Name: name, Format: pmpb.ParameterFormat_YAML}})
				pmWantCode(t, "UpdateParameter of the format", err, codes.InvalidArgument)

				mk := func(vid string, data []byte, disabled bool) (*pmpb.ParameterVersion, error) {
					return c.CreateParameterVersion(ctx, &pmpb.CreateParameterVersionRequest{Parent: name, ParameterVersionId: vid,
						ParameterVersion: &pmpb.ParameterVersion{Disabled: disabled, Payload: &pmpb.ParameterVersionPayload{Data: data}}})
				}
				_, err = mk("bad", []byte("{not json"), false)
				pmWantCode(t, "a version that is not JSON", err, codes.InvalidArgument)
				_, err = mk("big", append([]byte(`"`), append(bytes.Repeat([]byte("a"), 1<<20), '"')...), false)
				pmWantCode(t, "a version over 1 MiB", err, codes.InvalidArgument)
				data := []byte(`{"port": 8080}`)
				v, err := mk("v1", data, false)
				if err != nil || v.GetName() != name+"/versions/v1" {
					t.Fatalf("CreateParameterVersion: %+v, %v", v, err)
				}
				_, err = mk("v1", data, false)
				pmWantCode(t, "a second version v1", err, codes.AlreadyExists)
				full, err := c.GetParameterVersion(ctx, &pmpb.GetParameterVersionRequest{Name: v.GetName()})
				if err != nil || string(full.GetPayload().GetData()) != string(data) {
					t.Fatalf("GetParameterVersion FULL: %+v, %v", full, err)
				}
				basic, err := c.GetParameterVersion(ctx, &pmpb.GetParameterVersionRequest{Name: v.GetName(), View: pmpb.View_BASIC})
				if err != nil || basic.GetPayload() != nil {
					t.Fatalf("GetParameterVersion BASIC: %+v, %v", basic, err)
				}
				r, err := c.RenderParameterVersion(ctx, &pmpb.RenderParameterVersionRequest{Name: v.GetName()})
				if err != nil || string(r.GetRenderedPayload()) != string(data) || r.GetParameterVersion() != v.GetName() {
					t.Fatalf("RenderParameterVersion: %+v, %v", r, err)
				}
				dis, err := c.UpdateParameterVersion(ctx, &pmpb.UpdateParameterVersionRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"disabled"}},
					ParameterVersion: &pmpb.ParameterVersion{Name: v.GetName(), Disabled: true}})
				if err != nil || !dis.GetDisabled() {
					t.Fatalf("disable: %+v, %v", dis, err)
				}
				full, err = c.GetParameterVersion(ctx, &pmpb.GetParameterVersionRequest{Name: v.GetName(), View: pmpb.View_FULL})
				if err != nil || full.GetPayload() != nil {
					t.Fatalf("a disabled version in the FULL view: %+v, %v", full, err)
				}
				_, err = c.RenderParameterVersion(ctx, &pmpb.RenderParameterVersionRequest{Name: v.GetName()})
				pmWantCode(t, "RenderParameterVersion of a disabled version", err, codes.FailedPrecondition)
				var listed int
				vit := c.ListParameterVersions(ctx, &pmpb.ListParameterVersionsRequest{Parent: name})
				for {
					x, err := vit.Next()
					if errors.Is(err, iterator.Done) {
						break
					}
					if err != nil {
						t.Fatalf("ListParameterVersions: %v", err)
					}
					listed++
					if !x.GetDisabled() || x.GetPayload() != nil {
						t.Errorf("listed %+v", x)
					}
				}
				if listed != 1 {
					t.Fatalf("ListParameterVersions listed %d", listed)
				}
				err = c.DeleteParameter(ctx, &pmpb.DeleteParameterRequest{Name: name})
				pmWantCode(t, "DeleteParameter of a parameter with a version", err, codes.FailedPrecondition)
				if err := c.DeleteParameterVersion(ctx, &pmpb.DeleteParameterVersionRequest{Name: v.GetName()}); err != nil {
					t.Fatalf("DeleteParameterVersion: %v", err)
				}
				_, err = c.GetParameterVersion(ctx, &pmpb.GetParameterVersionRequest{Name: v.GetName()})
				pmWantCode(t, "a deleted version", err, codes.NotFound)
				if err := c.DeleteParameter(ctx, &pmpb.DeleteParameterRequest{Name: name}); err != nil {
					t.Fatalf("DeleteParameter: %v", err)
				}
				_, err = c.GetParameter(ctx, &pmpb.GetParameterRequest{Name: name})
				pmWantCode(t, "a deleted parameter", err, codes.NotFound)

				// YAML and UNFORMATTED.
				yid := id + "-y"
				if _, err := c.CreateParameter(ctx, &pmpb.CreateParameterRequest{Parent: parent, ParameterId: yid,
					Parameter: &pmpb.Parameter{Format: pmpb.ParameterFormat_YAML}}); err != nil {
					t.Fatal(err)
				}
				_, err = c.CreateParameterVersion(ctx, &pmpb.CreateParameterVersionRequest{Parent: parent + "/parameters/" + yid, ParameterVersionId: "v1",
					ParameterVersion: &pmpb.ParameterVersion{Payload: &pmpb.ParameterVersionPayload{Data: []byte("a: [1, 2\nb: :")}}})
				pmWantCode(t, "a version that is not YAML", err, codes.InvalidArgument)
				uid := id + "-u"
				u, err := c.CreateParameter(ctx, &pmpb.CreateParameterRequest{Parent: parent, ParameterId: uid, Parameter: &pmpb.Parameter{}})
				if err != nil || u.GetFormat() != pmpb.ParameterFormat_UNFORMATTED {
					t.Fatalf("a parameter without a format: %+v, %v", u, err)
				}
				raw := []byte(`anything __REF__("//secretmanager.googleapis.com/projects/p/secrets/s/versions/1") goes`)
				uv, err := c.CreateParameterVersion(ctx, &pmpb.CreateParameterVersionRequest{Parent: u.GetName(), ParameterVersionId: "v1",
					ParameterVersion: &pmpb.ParameterVersion{Payload: &pmpb.ParameterVersionPayload{Data: raw}}})
				if err != nil {
					t.Fatal(err)
				}
				ur, err := c.RenderParameterVersion(ctx, &pmpb.RenderParameterVersionRequest{Name: uv.GetName()})
				if err != nil || !bytes.Equal(ur.GetRenderedPayload(), raw) {
					t.Fatalf("an UNFORMATTED version renders as it is: %q, %v", ur.GetRenderedPayload(), err)
				}
			})
		}
	}
}

// covers: google.cloud.parametermanager.v1.ParameterManager/RenderParameterVersion, google.cloud.parametermanager.v1.ParameterManager/CreateParameterVersion
//
// TestParameterManagerRenderSecretReference (#2054): a JSON and a YAML
// version's __REF__ references to Secret Manager versions, global and
// regional, by number and as latest, render as the secret versions'
// payloads, read through the official Secret Manager client's instance;
// the payload itself is answered unchanged; a reference to a missing
// secret, or to a disabled secret version, fails the render; a reference
// that names no secret version is refused at create.
func TestParameterManagerRenderSecretReference(t *testing.T) {
	h := New(t)
	c := parameterManagerClients(t, h)["grpc"]
	sm := secretsClient(t, h)
	ctx := h.Context()
	project := h.Project()
	secret := func(parent, id, value string) string {
		t.Helper()
		s := &secretmanagerpb.Secret{}
		if !strings.Contains(parent, "/locations/") {
			s.Replication = &secretmanagerpb.Replication{Replication: &secretmanagerpb.Replication_Automatic_{Automatic: &secretmanagerpb.Replication_Automatic{}}}
		}
		if _, err := sm.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{Parent: parent, SecretId: id, Secret: s}); err != nil {
			t.Fatalf("CreateSecret %s: %v", id, err)
		}
		v, err := sm.AddSecretVersion(ctx, &secretmanagerpb.AddSecretVersionRequest{Parent: parent + "/secrets/" + id,
			Payload: &secretmanagerpb.SecretPayload{Data: []byte(value)}})
		if err != nil {
			t.Fatalf("AddSecretVersion %s: %v", id, err)
		}
		return v.GetName()
	}
	dbVersion := secret("projects/"+project, "db-password", `pa"ss`)
	_ = secret("projects/"+project, "api-key", "k-123")
	regional := secret("projects/"+project+"/locations/us-central1", "regional-token", "r-456")

	parent := "projects/" + project + "/locations/global"
	mkParam := func(id string, f pmpb.ParameterFormat) string {
		t.Helper()
		p, err := c.CreateParameter(ctx, &pmpb.CreateParameterRequest{Parent: parent, ParameterId: id, Parameter: &pmpb.Parameter{Format: f}})
		if err != nil {
			t.Fatalf("CreateParameter %s: %v", id, err)
		}
		for _, s := range []string{"db-password", "api-key", "nope"} {
			if s != "nope" {
				grantSecretAccessor(t, sm, "projects/"+project+"/secrets/"+s, p)
			}
		}
		grantSecretAccessor(t, sm, "projects/"+project+"/locations/us-central1/secrets/regional-token", p)
		return p.GetName()
	}
	mkVersion := func(param, vid, data string) (*pmpb.ParameterVersion, error) {
		return c.CreateParameterVersion(ctx, &pmpb.CreateParameterVersionRequest{Parent: param, ParameterVersionId: vid,
			ParameterVersion: &pmpb.ParameterVersion{Payload: &pmpb.ParameterVersionPayload{Data: []byte(data)}}})
	}
	ref := func(name string) string { return `__REF__("//secretmanager.googleapis.com/` + name + `")` }

	jp := mkParam("json-config", pmpb.ParameterFormat_JSON)
	payload := fmt.Sprintf(`{"db": {"password": %q}, "key": %q, "token": %q}`, ref(dbVersion),
		ref("projects/"+project+"/secrets/api-key/versions/latest"), ref(regional))
	v, err := mkVersion(jp, "v1", payload)
	if err != nil {
		t.Fatalf("CreateParameterVersion with references: %v", err)
	}
	r, err := c.RenderParameterVersion(ctx, &pmpb.RenderParameterVersionRequest{Name: v.GetName()})
	if err != nil {
		t.Fatalf("RenderParameterVersion: %v", err)
	}
	if want := `{"db": {"password": "pa\"ss"}, "key": "k-123", "token": "r-456"}`; string(r.GetRenderedPayload()) != want {
		t.Errorf("rendered %q, want %q", r.GetRenderedPayload(), want)
	}
	if string(r.GetPayload().GetData()) != payload {
		t.Errorf("render's payload %q, want the version's own", r.GetPayload().GetData())
	}

	yp := mkParam("yaml-config", pmpb.ParameterFormat_YAML)
	yv, err := mkVersion(yp, "v1", "db:\n  password: "+ref(dbVersion)+"\n")
	if err != nil {
		t.Fatal(err)
	}
	yr, err := c.RenderParameterVersion(ctx, &pmpb.RenderParameterVersionRequest{Name: yv.GetName()})
	if err != nil || string(yr.GetRenderedPayload()) != "db:\n  password: pa\"ss\n" {
		t.Fatalf("YAML render: %q, %v", yr.GetRenderedPayload(), err)
	}

	_, err = mkVersion(jp, "badref", `{"x": "__REF__(\"//secretmanager.googleapis.com/projects/p/secrets/s\")"}`)
	pmWantCode(t, "a reference naming a secret, not a version", err, codes.InvalidArgument)
	mv, err := mkVersion(jp, "missing", fmt.Sprintf(`{"x": %q}`, ref("projects/"+project+"/secrets/nope/versions/1")))
	if err != nil {
		t.Fatal(err)
	}
	// NOT_FOUND; PERMISSION_DENIED where IAM is enforced, as no one holds
	// a role on a secret that does not exist.
	if _, err = c.RenderParameterVersion(ctx, &pmpb.RenderParameterVersionRequest{Name: mv.GetName()}); pmCode(err) != codes.NotFound && pmCode(err) != codes.PermissionDenied {
		t.Fatalf("a reference to a missing secret: %v, want NOT_FOUND", err)
	}
	if _, err := sm.DisableSecretVersion(ctx, &secretmanagerpb.DisableSecretVersionRequest{Name: dbVersion}); err != nil {
		t.Fatal(err)
	}
	_, err = c.RenderParameterVersion(ctx, &pmpb.RenderParameterVersionRequest{Name: yv.GetName()})
	pmWantCode(t, "a reference to a disabled secret version", err, codes.FailedPrecondition)
}

// covers: google.cloud.parametermanager.v1.ParameterManager/CreateParameter, google.cloud.parametermanager.v1.ParameterManager/CreateParameterVersion, google.cloud.parametermanager.v1.ParameterManager/GetParameterVersion, google.cloud.parametermanager.v1.ParameterManager/UpdateParameter
//
// TestParameterManagerCMEK (#2054): a parameter with a Cloud KMS key of
// its location encrypts each new version with the key's primary version,
// which the version names in kms_key_version, and decrypts it to read;
// while that key version is disabled the version cannot be read; a version
// made after the key is removed carries none, and the old one keeps its
// own.
func TestParameterManagerCMEK(t *testing.T) {
	h := New(t)
	c := parameterManagerClients(t, h)["grpc"]
	kc, ring := firestoreKMS(t, h, "global")
	ctx := h.Context()
	key := newKey(t, h, kc, ring, "pm-key", kmspb.CryptoKey_ENCRYPT_DECRYPT)
	keyName := key.GetName()
	parent := "projects/" + h.Project() + "/locations/global"
	p, err := c.CreateParameter(ctx, &pmpb.CreateParameterRequest{Parent: parent, ParameterId: "encrypted",
		Parameter: &pmpb.Parameter{KmsKey: &keyName}})
	if err != nil {
		t.Fatalf("CreateParameter with a key: %v", err)
	}
	if p.GetKmsKey() != keyName {
		t.Fatalf("kms_key %q", p.GetKmsKey())
	}
	// The Parameter Manager service agent encrypts and decrypts with the
	// key, as Google's "Enable customer-managed encryption keys" grants it.
	number := strings.Split(strings.TrimPrefix(p.GetPolicyMember().GetIamPolicyUidPrincipal(), "principal://parametermanager.googleapis.com/projects/"), "/")[0]
	bindOn(t, kc.GetIamPolicy, kc.SetIamPolicy, keyName, &iampb.Binding{Role: "roles/cloudkms.cryptoKeyEncrypterDecrypter",
		Members: []string{"serviceAccount:service-" + number + "@gcp-sa-pm.iam.gserviceaccount.com"}})
	v, err := c.CreateParameterVersion(ctx, &pmpb.CreateParameterVersionRequest{Parent: p.GetName(), ParameterVersionId: "v1",
		ParameterVersion: &pmpb.ParameterVersion{Payload: &pmpb.ParameterVersionPayload{Data: []byte("sealed")}}})
	if err != nil {
		t.Fatalf("CreateParameterVersion with a key: %v", err)
	}
	if !strings.HasPrefix(v.GetKmsKeyVersion(), keyName+"/cryptoKeyVersions/") {
		t.Fatalf("kms_key_version %q", v.GetKmsKeyVersion())
	}
	got, err := c.GetParameterVersion(ctx, &pmpb.GetParameterVersionRequest{Name: v.GetName()})
	if err != nil || string(got.GetPayload().GetData()) != "sealed" {
		t.Fatalf("GetParameterVersion: %+v, %v", got, err)
	}
	setVersionState(t, h, kc, v.GetKmsKeyVersion(), kmspb.CryptoKeyVersion_DISABLED)
	_, err = c.GetParameterVersion(ctx, &pmpb.GetParameterVersionRequest{Name: v.GetName()})
	pmWantCode(t, "a version whose key version is disabled", err, codes.FailedPrecondition)
	setVersionState(t, h, kc, v.GetKmsKeyVersion(), kmspb.CryptoKeyVersion_ENABLED)
	empty := ""
	if _, err := c.UpdateParameter(ctx, &pmpb.UpdateParameterRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"kms_key"}},
		Parameter: &pmpb.Parameter{Name: p.GetName(), KmsKey: &empty}}); err != nil {
		t.Fatalf("UpdateParameter removing the key: %v", err)
	}
	v2, err := c.CreateParameterVersion(ctx, &pmpb.CreateParameterVersionRequest{Parent: p.GetName(), ParameterVersionId: "v2",
		ParameterVersion: &pmpb.ParameterVersion{Payload: &pmpb.ParameterVersionPayload{Data: []byte("plain")}}})
	if err != nil || v2.KmsKeyVersion != nil {
		t.Fatalf("a version after the key is removed: %+v, %v", v2, err)
	}
	old, err := c.GetParameterVersion(ctx, &pmpb.GetParameterVersionRequest{Name: v.GetName()})
	if err != nil || old.GetKmsKeyVersion() != v.GetKmsKeyVersion() || string(old.GetPayload().GetData()) != "sealed" {
		t.Fatalf("the earlier version: %+v, %v", old, err)
	}
}

// covers: google.cloud.parametermanager.v1.ParameterManager/RenderParameterVersion
//
// TestIAMEnforcementParameterManagerRender (#2054), on an instance with
// `--iam-enforcement=on`: a parameter renders its secret reference as its
// own principal, so the render is PERMISSION_DENIED (Secret Manager's
// denial of secretmanager.versions.access) until the principal holds
// roles/secretmanager.secretAccessor on the secret, as Google's
// "Reference secrets in a parameter" has it; and a caller without
// parametermanager.parameterVersions.render is refused, and with
// roles/parametermanager.parameterAccessor renders.
func TestIAMEnforcementParameterManagerRender(t *testing.T) {
	h := New(t)
	endpoint := h.Endpoint(EnvParameterManager)
	smEndpoint := h.Endpoint(EnvSecrets)
	h.Endpoint(EnvResourceManager)
	h.Endpoint(EnvMetadata)
	requireIAMEnforcement(t)
	ctx := h.Context()
	project := iamProject(t, h, "iepm-")
	sm, err := secretmanager.NewClient(ctx, grpcAs(smEndpoint, "")...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sm.Close() })
	pm, err := parametermanager.NewClient(ctx, grpcAs(endpoint, "")...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pm.Close() })
	secret := "projects/" + project + "/secrets/referenced"
	if _, err := sm.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{Parent: "projects/" + project, SecretId: "referenced",
		Secret: &secretmanagerpb.Secret{Replication: &secretmanagerpb.Replication{Replication: &secretmanagerpb.Replication_Automatic_{
			Automatic: &secretmanagerpb.Replication_Automatic{}}}}}); err != nil {
		t.Fatal(err)
	}
	sv, err := sm.AddSecretVersion(ctx, &secretmanagerpb.AddSecretVersionRequest{Parent: secret, Payload: &secretmanagerpb.SecretPayload{Data: []byte("v4lue")}})
	if err != nil {
		t.Fatal(err)
	}
	p, err := pm.CreateParameter(ctx, &pmpb.CreateParameterRequest{Parent: "projects/" + project + "/locations/global", ParameterId: "cfg",
		Parameter: &pmpb.Parameter{Format: pmpb.ParameterFormat_JSON}})
	if err != nil {
		t.Fatal(err)
	}
	v, err := pm.CreateParameterVersion(ctx, &pmpb.CreateParameterVersionRequest{Parent: p.GetName(), ParameterVersionId: "v1",
		ParameterVersion: &pmpb.ParameterVersion{Payload: &pmpb.ParameterVersionPayload{
			Data: []byte(`{"v": "__REF__(\"//secretmanager.googleapis.com/` + sv.GetName() + `\")"}`)}}})
	if err != nil {
		t.Fatal(err)
	}
	render := func(c *parametermanager.Client) func() error {
		return func() error {
			r, err := c.RenderParameterVersion(ctx, &pmpb.RenderParameterVersionRequest{Name: v.GetName()})
			if err == nil && string(r.GetRenderedPayload()) != `{"v": "v4lue"}` {
				return fmt.Errorf("rendered %q", r.GetRenderedPayload())
			}
			return err
		}
	}
	wantDenied(t, "a render before the principal's grant", render(pm)(),
		"Permission 'secretmanager.versions.access' denied for resource '"+sv.GetName()+"' (or it may not exist).", "secretmanager.versions.access", true)
	undo := bindOn(t, sm.GetIamPolicy, sm.SetIamPolicy, secret,
		&iampb.Binding{Role: "roles/secretmanager.secretAccessor", Members: []string{p.GetPolicyMember().GetIamPolicyUidPrincipal()}})
	waitGranted(t, "a render with secretAccessor granted to the parameter's principal", render(pm))

	caller := newIAMPrincipal(t, h, project, "iepm")
	as, err := parametermanager.NewClient(ctx, grpcAs(endpoint, caller.token)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = as.Close() })
	wantDenied(t, "a render by a caller without a role", render(as)(),
		"Permission 'parametermanager.parameterVersions.render' denied for resource '"+v.GetName()+"' (or it may not exist).",
		"parametermanager.parameterVersions.render", true)
	caller.grant("roles/parametermanager.parameterAccessor")
	waitGranted(t, "a render with parameterAccessor", render(as))
	undo()
	wantDenied(t, "a render once the principal's grant is gone", render(pm)(),
		"Permission 'secretmanager.versions.access' denied for resource '"+sv.GetName()+"' (or it may not exist).", "secretmanager.versions.access", true)
}

// createSecretWithVersion makes an automatically replicated secret with
// one version holding value, and answers the version's name.
func createSecretWithVersion(t *testing.T, h *Harness, sm *secretmanager.Client, parent, id, value string) string {
	t.Helper()
	ctx := h.Context()
	if _, err := sm.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{Parent: parent, SecretId: id,
		Secret: &secretmanagerpb.Secret{Replication: &secretmanagerpb.Replication{Replication: &secretmanagerpb.Replication_Automatic_{
			Automatic: &secretmanagerpb.Replication_Automatic{}}}}}); err != nil {
		t.Fatalf("CreateSecret %s: %v", id, err)
	}
	v, err := sm.AddSecretVersion(ctx, &secretmanagerpb.AddSecretVersionRequest{Parent: parent + "/secrets/" + id,
		Payload: &secretmanagerpb.SecretPayload{Data: []byte(value)}})
	if err != nil {
		t.Fatalf("AddSecretVersion %s: %v", id, err)
	}
	return v.GetName()
}
