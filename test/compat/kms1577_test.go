//go:build compat

package compat

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"testing"

	kms "cloud.google.com/go/kms/apiv1"
	"cloud.google.com/go/kms/apiv1/kmspb"
	rmpb "cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"
	cloudkms "google.golang.org/api/cloudkms/v1"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestKMSDefaultKeyAccessJustificationsPolicies (#1577): the
// KeyAccessJustificationsPolicyConfig of a folder and of a project in it,
// set and read with Google's generated Cloud KMS JSON client
// (google.golang.org/api/cloudkms/v1, the official client of methods no
// published proto carries), as
// https://docs.cloud.google.com/assured-workloads/key-access-justifications/docs/set-default-policy
// documents them: a key created without a policy, with the official
// KeyManagementClient over gRPC, gets the nearest active default, the
// folder's and then the project's, and that default is enforced; a key
// created with a policy keeps its own, and an existing key is not changed.
// showEffectiveKeyAccessJustificationsPolicyConfig names the config in force.
// covers: google.cloud.kms.v1.KeyManagementService/CreateCryptoKey, google.cloud.kms.v1.KeyManagementService/GetCryptoKey, google.cloud.kms.v1.KeyManagementService/Encrypt
func TestKMSDefaultKeyAccessJustificationsPolicies(t *testing.T) {
	h := New(t)
	addr := h.Endpoint(EnvKMS)
	shardEndpoint(h, envLoggingResourceManager, EnvResourceManager)
	ctx := h.Context()
	js, err := cloudkms.NewService(ctx, option.WithEndpoint("http://"+addr+"/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	kc, err := kms.NewKeyManagementClient(ctx, kmsOptions(h, "grpc")...)
	if err != nil {
		t.Fatal(err)
	}
	defer kc.Close()
	fc, _, pc := foldersClients(t, h)
	fop, err := fc.CreateFolder(ctx, &rmpb.CreateFolderRequest{Folder: &rmpb.Folder{Parent: localOrg, DisplayName: folderLabel(h, "kaj-")}})
	if err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}
	folder, err := fop.Wait(ctx)
	if err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}
	project := v1ID(h, "kaj-")
	pop, err := pc.CreateProject(ctx, &rmpb.CreateProjectRequest{Project: &rmpb.Project{ProjectId: project, Parent: folder.GetName()}})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if _, err := pop.Wait(ctx); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	folderCfg, projectCfg := folder.GetName()+"/kajPolicyConfig", "projects/"+project+"/kajPolicyConfig"
	policy := func(name string, reasons ...string) *cloudkms.KeyAccessJustificationsPolicyConfig {
		return &cloudkms.KeyAccessJustificationsPolicyConfig{Name: name,
			DefaultKeyAccessJustificationPolicy: &cloudkms.KeyAccessJustificationsPolicy{AllowedAccessReasons: reasons}}
	}
	const mask = "defaultKeyAccessJustificationPolicy"
	t.Cleanup(func() {
		_, _ = js.Folders.UpdateKajPolicyConfig(folderCfg, policy(folderCfg)).UpdateMask(mask).Context(context.Background()).Do()
		_, _ = js.Projects.UpdateKajPolicyConfig(projectCfg, policy(projectCfg)).UpdateMask(mask).Context(context.Background()).Do()
		if op, err := pc.DeleteProject(context.Background(), &rmpb.DeleteProjectRequest{Name: "projects/" + project}); err == nil {
			_, _ = op.Wait(context.Background())
		}
		_, _ = fc.DeleteFolder(context.Background(), &rmpb.DeleteFolderRequest{Name: folder.GetName()})
	})

	got, err := js.Projects.GetKajPolicyConfig(projectCfg).Context(ctx).Do()
	if err != nil {
		t.Fatalf("GetKajPolicyConfig: %v", err)
	}
	if got.Name != projectCfg || got.DefaultKeyAccessJustificationPolicy != nil || !got.DefaultPolicyAvailable {
		t.Errorf("an unset config = %+v", got)
	}
	ring, err := kc.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: "projects/" + project + "/locations/global", KeyRingId: "kaj"})
	if err != nil {
		t.Fatalf("CreateKeyRing: %v", err)
	}
	mk := func(id string, p *kmspb.KeyAccessJustificationsPolicy) *kmspb.CryptoKey {
		t.Helper()
		k, err := kc.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: id,
			CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT, KeyAccessJustificationsPolicy: p}})
		if err != nil {
			t.Fatalf("CreateCryptoKey %s: %v", id, err)
		}
		return k
	}
	reasons := func(k *kmspb.CryptoKey) []string {
		var out []string
		for _, r := range k.GetKeyAccessJustificationsPolicy().GetAllowedAccessReasons() {
			out = append(out, r.String())
		}
		return out
	}
	before := mk("before", nil)

	fcfg, err := js.Folders.UpdateKajPolicyConfig(folderCfg, policy(folderCfg, "GOOGLE_INITIATED_SERVICE")).UpdateMask(mask).Context(ctx).Do()
	if err != nil {
		t.Fatalf("UpdateKajPolicyConfig on the folder: %v", err)
	}
	if !slices.Equal(fcfg.DefaultKeyAccessJustificationPolicy.AllowedAccessReasons, []string{"GOOGLE_INITIATED_SERVICE"}) {
		t.Errorf("the folder's config = %+v", fcfg.DefaultKeyAccessJustificationPolicy)
	}
	if got, err := js.Folders.GetKajPolicyConfig(folderCfg).Context(ctx).Do(); err != nil || got.DefaultKeyAccessJustificationPolicy == nil {
		t.Errorf("GetKajPolicyConfig on the folder = %+v, %v", got, err)
	}
	// Describing the project returns only its own policy.
	if got, err := js.Projects.GetKajPolicyConfig(projectCfg).Context(ctx).Do(); err != nil || got.DefaultKeyAccessJustificationPolicy != nil {
		t.Errorf("the project's own config under a folder default = %+v, %v", got, err)
	}
	eff, err := js.Projects.ShowEffectiveKeyAccessJustificationsPolicyConfig("projects/" + project).Context(ctx).Do()
	if err != nil {
		t.Fatalf("ShowEffectiveKeyAccessJustificationsPolicyConfig: %v", err)
	}
	if eff.EffectiveKajPolicy == nil || eff.EffectiveKajPolicy.Name != folderCfg {
		t.Errorf("effective = %+v, want the folder's", eff.EffectiveKajPolicy)
	}
	fromFolder := mk("from-folder", nil)
	if !slices.Equal(reasons(fromFolder), []string{"GOOGLE_INITIATED_SERVICE"}) {
		t.Errorf("a key made under the folder's default has %v", reasons(fromFolder))
	}
	// Enforced: GOOGLE_INITIATED_SERVICE alone refuses a call of one's own.
	if _, err := kc.Encrypt(ctx, &kmspb.EncryptRequest{Name: fromFolder.GetName(), Plaintext: []byte("hi")}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("Encrypt under the folder's default = %v, want FAILED_PRECONDITION", err)
	}

	if _, err := js.Projects.UpdateKajPolicyConfig(projectCfg, policy(projectCfg, "CUSTOMER_INITIATED_ACCESS")).UpdateMask(mask).Context(ctx).Do(); err != nil {
		t.Fatalf("UpdateKajPolicyConfig on the project: %v", err)
	}
	if eff, err = js.Projects.ShowEffectiveKeyAccessJustificationsPolicyConfig("projects/" + project).Context(ctx).Do(); err != nil {
		t.Fatal(err)
	}
	if eff.EffectiveKajPolicy == nil || eff.EffectiveKajPolicy.Name != projectCfg {
		t.Errorf("effective = %+v, want the project's", eff.EffectiveKajPolicy)
	}
	fromProject := mk("from-project", nil)
	if !slices.Equal(reasons(fromProject), []string{"CUSTOMER_INITIATED_ACCESS"}) {
		t.Errorf("a key made under the project's default has %v", reasons(fromProject))
	}
	if _, err := kc.Encrypt(ctx, &kmspb.EncryptRequest{Name: fromProject.GetName(), Plaintext: []byte("hi")}); err != nil {
		t.Errorf("Encrypt under the project's default: %v", err)
	}
	own := mk("own", &kmspb.KeyAccessJustificationsPolicy{AllowedAccessReasons: []kmspb.AccessReason{kmspb.AccessReason_CUSTOMER_AUTHORIZED_WORKFLOW_SERVICING}})
	if !slices.Equal(reasons(own), []string{"CUSTOMER_AUTHORIZED_WORKFLOW_SERVICING"}) {
		t.Errorf("a key made with its own policy has %v", reasons(own))
	}
	again, err := kc.GetCryptoKey(ctx, &kmspb.GetCryptoKeyRequest{Name: before.GetName()})
	if err != nil {
		t.Fatal(err)
	}
	if again.GetKeyAccessJustificationsPolicy() != nil {
		t.Errorf("a key made before any default now has %v", reasons(again))
	}
	var gerr *googleapi.Error
	if _, err := js.Projects.UpdateKajPolicyConfig(projectCfg, policy(projectCfg, "NOT_A_REASON")).UpdateMask(mask).Context(ctx).Do(); !errors.As(err, &gerr) || gerr.Code != http.StatusBadRequest {
		t.Errorf("an unknown reason = %v, want 400", err)
	}
}
