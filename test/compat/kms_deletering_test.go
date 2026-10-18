//go:build compat

package compat

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"cloud.google.com/go/kms/apiv1/kmspb"
	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// TestKMSDeleteKeyRing (#1595): DeleteKeyRing as
// https://docs.cloud.google.com/kms/docs/reference/rest/v1/projects.locations.keyRings/delete
// documents it, "Permanently deletes the given KeyRing. All child
// resources of the KeyRing must have been previously deleted", with
// delete-kms-resources' "All import jobs in the key ring must be expired".
// The Go client has no DeleteKeyRing (googleapis' service.proto has none
// yet), so the call is made over gRPC at the method Google's would be, on
// the official client's own connection settings, and over REST as
// documented; the rings are made, filled and checked with the official
// client. A ring with a key or an ACTIVE import job is refused
// FAILED_PRECONDITION; once empty it is deleted, the Operation done, and
// GetKeyRing is NOT_FOUND. gcloud's `kms keyrings delete` is
// TestGcloudKMSDeleteKeyRing.
//
// covers: google.cloud.kms.v1.KeyManagementService/DeleteKeyRing, google.cloud.kms.v1.KeyManagementService/CreateKeyRing, google.cloud.kms.v1.KeyManagementService/GetKeyRing, google.cloud.kms.v1.KeyManagementService/DeleteCryptoKey, google.cloud.kms.v1.KeyManagementService/CreateImportJob
func TestKMSDeleteKeyRing(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	addr := h.Endpoint(EnvKMS)
	c := kmsClients(t, h)["grpc"]
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	viaGRPC := func(name string) (bool, codes.Code) {
		var op longrunningpb.Operation
		err := conn.Invoke(ctx, "/google.cloud.kms.v1.KeyManagementService/DeleteKeyRing", &kmspb.GetKeyRingRequest{Name: name}, &op)
		return op.GetDone() && op.GetError() == nil, status.Code(err)
	}
	viaREST := func(name string) (bool, codes.Code) {
		req, err := http.NewRequestWithContext(ctx, http.MethodDelete, "http://"+addr+"/v1/"+name, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var body struct {
			Done  bool `json:"done"`
			Error struct {
				Status string `json:"status"`
			} `json:"error"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("DELETE %s: %v", name, err)
		}
		if resp.StatusCode == http.StatusOK {
			return body.Done, codes.OK
		}
		var code codes.Code
		if err := code.UnmarshalJSON([]byte(`"` + body.Error.Status + `"`)); err != nil {
			t.Fatalf("DELETE %s: status %q", name, body.Error.Status)
		}
		return false, code
	}
	for variant, del := range map[string]func(string) (bool, codes.Code){"grpc": viaGRPC, "rest": viaREST} {
		t.Run(variant, func(t *testing.T) {
			parent := "projects/" + h.Project() + "/locations/us-east1"
			id := "del-" + variant + "-" + strings.ToLower(h.Project())
			ring, err := c.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: parent, KeyRingId: id})
			if err != nil {
				t.Fatal(err)
			}
			if _, code := del(parent + "/keyRings/absent-" + variant); code != codes.NotFound {
				t.Errorf("DeleteKeyRing(absent) = %v; want NotFound", code)
			}
			key, err := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "k", SkipInitialVersionCreation: true,
				CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}})
			if err != nil {
				t.Fatal(err)
			}
			if _, code := del(ring.GetName()); code != codes.FailedPrecondition {
				t.Errorf("DeleteKeyRing with a key = %v; want FailedPrecondition", code)
			}
			op, err := c.DeleteCryptoKey(ctx, &kmspb.DeleteCryptoKeyRequest{Name: key.GetName()})
			if err != nil {
				t.Fatal(err)
			}
			if err := op.Wait(ctx); err != nil {
				t.Fatal(err)
			}
			job, err := c.CreateImportJob(ctx, &kmspb.CreateImportJobRequest{Parent: ring.GetName(), ImportJobId: "j", ImportJob: &kmspb.ImportJob{
				ImportMethod: kmspb.ImportJob_RSA_OAEP_3072_SHA1_AES_256, ProtectionLevel: kmspb.ProtectionLevel_SOFTWARE}})
			if err != nil {
				t.Fatal(err)
			}
			if _, code := del(ring.GetName()); code != codes.FailedPrecondition {
				t.Errorf("DeleteKeyRing with ACTIVE import job %s = %v; want FailedPrecondition", job.GetName(), code)
			}
			// An import job expires after 3 days, beyond a test: the empty
			// ring is a second one.
			empty, err := c.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: parent, KeyRingId: id + "-empty"})
			if err != nil {
				t.Fatal(err)
			}
			if done, code := del(empty.GetName()); code != codes.OK || !done {
				t.Fatalf("DeleteKeyRing(empty) = done %v, %v; want a done Operation", done, code)
			}
			if _, err := c.GetKeyRing(ctx, &kmspb.GetKeyRingRequest{Name: empty.GetName()}); status.Code(err) != codes.NotFound {
				t.Errorf("GetKeyRing after DeleteKeyRing = %v; want NotFound", err)
			}
		})
	}
}

// TestGcloudKMSDeleteKeyRing (#1595): `gcloud kms keyrings delete`, the
// documented command, deletes an empty key ring and is refused for one
// that has a key.
//
// covers: google.cloud.kms.v1.KeyManagementService/DeleteKeyRing, google.cloud.kms.v1.KeyManagementService/CreateKeyRing, google.cloud.kms.v1.KeyManagementService/GetKeyRing
func TestGcloudKMSDeleteKeyRing(t *testing.T) {
	h := New(t)
	run, project := gcloudKMS(t, h)
	c := kmsClients(t, h)["grpc"]
	ctx := h.Context()
	parent := "projects/" + project + "/locations/global"
	id := "gdel-" + strings.ToLower(h.Project())
	ring, err := c.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: parent, KeyRingId: id})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "k",
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}}); err != nil {
		t.Fatal(err)
	}
	if out, err := run("kms", "keyrings", "delete", id, "--location", "global"); err == nil {
		t.Errorf("gcloud kms keyrings delete of a ring with a key succeeded:\n%s", out)
	}
	empty := id + "-empty"
	if _, err := c.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: parent, KeyRingId: empty}); err != nil {
		t.Fatal(err)
	}
	if out, err := run("kms", "keyrings", "delete", empty, "--location", "global"); err != nil {
		t.Fatalf("gcloud kms keyrings delete: %v\n%s", err, out)
	}
	if _, err := c.GetKeyRing(ctx, &kmspb.GetKeyRingRequest{Name: parent + "/keyRings/" + empty}); status.Code(err) != codes.NotFound {
		t.Errorf("GetKeyRing after gcloud kms keyrings delete = %v; want NotFound", err)
	}
}
