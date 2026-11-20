//go:build compat

package compat

import (
	"errors"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"

	"cloud.google.com/go/documentai/apiv1/documentaipb"
	"cloud.google.com/go/iam"
	kms "cloud.google.com/go/kms/apiv1"
	"cloud.google.com/go/kms/apiv1/kmspb"
	"google.golang.org/api/googleapi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/cloudburrow/cloudburrow/internal/resource"
)

// aiCMEKKey makes an ENCRYPT_DECRYPT key in location of a new key ring,
// granting agent (when set) roles/cloudkms.cryptoKeyEncrypterDecrypter on
// it, through the official Cloud KMS client.
func aiCMEKKey(t *testing.T, h *Harness, k *kms.KeyManagementClient, location, ring, agent string) string {
	t.Helper()
	ctx := h.Context()
	r, err := k.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: "projects/" + h.Project() + "/locations/" + location, KeyRingId: ring})
	if err != nil {
		t.Fatalf("CreateKeyRing: %v", err)
	}
	key, err := k.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: r.GetName(), CryptoKeyId: "key",
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}})
	if err != nil {
		t.Fatalf("CreateCryptoKey: %v", err)
	}
	if agent != "" {
		ih := k.ResourceIAM(key.GetName())
		p, err := ih.Policy(ctx)
		if err != nil {
			t.Fatal(err)
		}
		p.Add("serviceAccount:"+agent, iam.RoleName("roles/cloudkms.cryptoKeyEncrypterDecrypter"))
		if err := ih.SetPolicy(ctx, p); err != nil {
			t.Fatalf("grant %s: %v", agent, err)
		}
	}
	return key.GetName()
}

// aiServiceAgent is project's service agent at domain.
func aiServiceAgent(project, domain string) string {
	return "service-" + strconv.FormatInt(resource.ProjectNumber(project), 10) + "@" + domain
}

// TestDocumentAICMEK (#1950): a Document AI processor with kms_key_name, a
// key of the instance's Cloud KMS in us-central1 (the us multi-region's key
// location), through the official cloud.google.com/go/documentai client:
// its versions report the key and the key version, it processes a document,
// and while the version is disabled ProcessDocument and GetProcessorVersion
// are FAILED_PRECONDITION; enabled again, it processes. A key in another
// location is INVALID_ARGUMENT. Measured first: kms_key_name was 501.
//
// covers: google.cloud.documentai.v1.DocumentProcessorService/CreateProcessor, google.cloud.documentai.v1.DocumentProcessorService/GetProcessorVersion, google.cloud.documentai.v1.DocumentProcessorService/ProcessDocument
func TestDocumentAICMEK(t *testing.T) {
	h := New(t)
	// The run shard's local AI endpoint, which Cloud Run starts without a
	// model, and its Cloud KMS (#1972).
	shardEndpoint(h, envRunLocalAI, EnvLocalAI)
	shardEndpoint(h, envRunKMS, EnvKMS)
	g, r := documentAIClients(t, h)
	k := kmsClients(t, h)["grpc"]
	ctx := h.Context()
	agent := aiServiceAgent(h.Project(), "gcp-sa-prod-dai-core.iam.gserviceaccount.com")
	key := aiCMEKKey(t, h, k, "us-central1", "docai-cmek", agent)
	wrong := aiCMEKKey(t, h, k, "europe-west4", "docai-cmek-eu", agent)
	parent := "projects/" + h.Project() + "/locations/us"
	if _, err := g.CreateProcessor(ctx, &documentaipb.CreateProcessorRequest{Parent: parent,
		Processor: &documentaipb.Processor{Type: "OCR_PROCESSOR", DisplayName: "cmek-eu-" + h.Project(), KmsKeyName: wrong}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a key outside the processor's location: %v", err)
	}
	p, err := g.CreateProcessor(ctx, &documentaipb.CreateProcessorRequest{Parent: parent,
		Processor: &documentaipb.Processor{Type: "OCR_PROCESSOR", DisplayName: "cmek-" + h.Project(), KmsKeyName: key}})
	if err != nil {
		t.Fatalf("CreateProcessor with kms_key_name: %v", err)
	}
	t.Cleanup(func() {
		if op, err := g.DeleteProcessor(h.Context(), &documentaipb.DeleteProcessorRequest{Name: p.GetName()}); err == nil {
			_ = op.Wait(h.Context())
		}
	})
	if got, err := r.GetProcessor(ctx, &documentaipb.GetProcessorRequest{Name: p.GetName()}); err != nil || got.GetKmsKeyName() != key {
		t.Fatalf("GetProcessor over REST: %v %v", got, err)
	}
	v1 := key + "/cryptoKeyVersions/1"
	v, err := g.GetProcessorVersion(ctx, &documentaipb.GetProcessorVersionRequest{Name: p.GetDefaultProcessorVersion()})
	if err != nil || v.GetKmsKeyName() != key || v.GetKmsKeyVersionName() != v1 {
		t.Fatalf("GetProcessorVersion: %v %v; want kms_key_version_name %s", v, err, v1)
	}
	scan, err := os.ReadFile("../../internal/ocr/testdata/scan.png")
	if err != nil {
		t.Fatal(err)
	}
	raw := &documentaipb.ProcessRequest_RawDocument{RawDocument: &documentaipb.RawDocument{Content: scan, MimeType: "image/png"}}
	res, err := g.ProcessDocument(ctx, &documentaipb.ProcessRequest{Name: p.GetName(), Source: raw})
	if err != nil || !strings.Contains(res.GetDocument().GetText(), "INVOICE") {
		t.Fatalf("ProcessDocument with the key: %v", err)
	}
	setVersionState(t, h, k, v1, kmspb.CryptoKeyVersion_DISABLED)
	if _, err := g.ProcessDocument(ctx, &documentaipb.ProcessRequest{Name: p.GetName(), Source: raw}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("ProcessDocument with the version disabled: %v; want FAILED_PRECONDITION", err)
	}
	if _, err := r.GetProcessorVersion(ctx, &documentaipb.GetProcessorVersionRequest{Name: p.GetDefaultProcessorVersion()}); !restFailedPrecondition(err) {
		t.Errorf("GetProcessorVersion over REST with the version disabled: %v; want FAILED_PRECONDITION", err)
	}
	setVersionState(t, h, k, v1, kmspb.CryptoKeyVersion_ENABLED)
	if _, err := g.ProcessDocument(ctx, &documentaipb.ProcessRequest{Name: p.GetName(), Source: raw}); err != nil {
		t.Errorf("ProcessDocument with the version enabled again: %v", err)
	}
}

// restFailedPrecondition reports whether a REST client's error is Google's
// FAILED_PRECONDITION over HTTP: 400 with that status in the body.
func restFailedPrecondition(err error) bool {
	var ge *googleapi.Error
	return errors.As(err, &ge) && ge.Code == http.StatusBadRequest && strings.Contains(ge.Body, "FAILED_PRECONDITION")
}
