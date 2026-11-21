//go:build compat

package compat

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	"cloud.google.com/go/iam"
	"cloud.google.com/go/kms/apiv1/kmspb"
	inventory "cloud.google.com/go/kms/inventory/apiv1"
	"cloud.google.com/go/kms/inventory/apiv1/inventorypb"
	"cloud.google.com/go/storage"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
)

// inventoryClients are the official Inventory clients, gRPC and REST, on
// Cloud KMS's port, where CloudBurrow serves kmsinventory.googleapis.com.
func inventoryClients(t *testing.T, h *Harness) (map[string]*inventory.KeyTrackingClient, map[string]*inventory.KeyDashboardClient) {
	t.Helper()
	ctx := h.Context()
	addr := h.Endpoint(EnvKMS)
	if addr == "" {
		t.Fatal("no Cloud KMS endpoint")
	}
	grpcOpts := []option.ClientOption{option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials()))}
	restOpts := []option.ClientOption{option.WithEndpoint("http://" + addr), option.WithoutAuthentication()}
	tg, err := inventory.NewKeyTrackingClient(ctx, grpcOpts...)
	if err != nil {
		t.Fatal(err)
	}
	tr, err := inventory.NewKeyTrackingRESTClient(ctx, restOpts...)
	if err != nil {
		t.Fatal(err)
	}
	dg, err := inventory.NewKeyDashboardClient(ctx, grpcOpts...)
	if err != nil {
		t.Fatal(err)
	}
	dr, err := inventory.NewKeyDashboardRESTClient(ctx, restOpts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tg.Close(); _ = tr.Close(); _ = dg.Close(); _ = dr.Close() })
	return map[string]*inventory.KeyTrackingClient{"grpc": tg, "rest": tr}, map[string]*inventory.KeyDashboardClient{"grpc": dg, "rest": dr}
}

// TestKMSInventoryCountsCMEKResources (#2053): the Cloud KMS Inventory API
// with the official Go client, over gRPC and REST, reports real key usage. A
// Cloud Storage bucket with the key as its default (and an object written
// under it) and a BigQuery dataset with the key as its default are what
// GetProtectedResourcesSummary counts and SearchProtectedResources lists;
// a version's summary counts the bucket, whose object recorded it; a
// resource-type filter narrows the search; ListCryptoKeys lists the key.
//
// covers: google.cloud.kms.inventory.v1.KeyTrackingService/GetProtectedResourcesSummary
// covers: google.cloud.kms.inventory.v1.KeyTrackingService/SearchProtectedResources
// covers: google.cloud.kms.inventory.v1.KeyDashboardService/ListCryptoKeys
func TestKMSInventoryCountsCMEKResources(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvStorage)
	h.Endpoint(EnvKMS)
	bq, project := bigqueryClient(t, h)
	tracking, dashboard := inventoryClients(t, h)
	sc := storageClient(t, h)
	ctx := h.Context()
	k := kmsClients(t, h)["grpc"]

	suffix := fmt.Sprint(time.Now().UnixNano() % 1e9)
	ring, err := k.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: "projects/" + project + "/locations/us", KeyRingId: "inv-" + suffix})
	if err != nil {
		t.Fatalf("CreateKeyRing: %v", err)
	}
	key, err := k.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "cmek",
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}})
	if err != nil {
		t.Fatalf("CreateCryptoKey: %v", err)
	}
	agent, err := sc.ServiceAccount(ctx, project)
	if err != nil {
		t.Fatal(err)
	}
	ih := k.ResourceIAM(key.GetName())
	p, err := ih.Policy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	p.Add("serviceAccount:"+agent, iam.RoleName("roles/cloudkms.cryptoKeyEncrypterDecrypter"))
	if err := ih.SetPolicy(ctx, p); err != nil {
		t.Fatal(err)
	}

	bucket := "inv-" + suffix + "-" + strings.ToLower(project)
	if len(bucket) > 63 {
		bucket = bucket[:63]
	}
	bh := sc.Bucket(bucket)
	if err := bh.Create(ctx, project, &storage.BucketAttrs{Location: "US", Labels: map[string]string{"team": "inv"},
		Encryption: &storage.BucketEncryption{DefaultKMSKeyName: key.GetName()}}); err != nil {
		t.Fatalf("create the CMEK bucket: %v", err)
	}
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		emptyAndDelete(cctx, bh)
	})
	if a, err := cmekWrite(h, bh.Object("o"), "", "protected"); err != nil || a.KMSKeyName != key.GetName()+"/cryptoKeyVersions/1" {
		t.Fatalf("write under the default key = %+v, %v", a, err)
	}
	ds := bq.Dataset("inv_" + suffix)
	if err := ds.Create(ctx, &bigquery.DatasetMetadata{Location: "US",
		DefaultEncryptionConfig: &bigquery.EncryptionConfig{KMSKeyName: key.GetName()}}); err != nil {
		t.Fatalf("create the CMEK dataset: %v", err)
	}
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = ds.DeleteWithContents(cctx)
	})

	wantBucket := "//storage.googleapis.com/" + bucket
	wantDataset := "//bigquery.googleapis.com/projects/" + project + "/datasets/inv_" + suffix
	for tr, c := range tracking {
		s, err := c.GetProtectedResourcesSummary(ctx, &inventorypb.GetProtectedResourcesSummaryRequest{Name: key.GetName()})
		if err != nil {
			t.Fatalf("%s GetProtectedResourcesSummary: %v", tr, err)
		}
		if s.GetName() != key.GetName()+"/protectedResourcesSummary" || s.GetResourceCount() != 2 || s.GetProjectCount() != 1 ||
			s.GetResourceTypes()["storage.googleapis.com/Bucket"] != 1 || s.GetResourceTypes()["bigquery.googleapis.com/Dataset"] != 1 ||
			s.GetCloudProducts()["storage"] != 1 || s.GetCloudProducts()["bigquery"] != 1 || s.GetLocations()["us"] != 2 {
			t.Errorf("%s summary = %v; want a bucket and a dataset in us, one project", tr, s)
		}
		v, err := c.GetProtectedResourcesSummary(ctx, &inventorypb.GetProtectedResourcesSummaryRequest{Name: key.GetName() + "/cryptoKeyVersions/1"})
		if err != nil || v.GetResourceCount() != 1 || v.GetResourceTypes()["storage.googleapis.com/Bucket"] != 1 {
			t.Errorf("%s version summary = %v, %v; want the bucket", tr, v, err)
		}

		var got []*inventorypb.ProtectedResource
		it := c.SearchProtectedResources(ctx, &inventorypb.SearchProtectedResourcesRequest{Scope: "projects/" + project, CryptoKey: key.GetName()})
		for {
			r, err := it.Next()
			if err == iterator.Done {
				break
			}
			if err != nil {
				t.Fatalf("%s SearchProtectedResources: %v", tr, err)
			}
			got = append(got, r)
		}
		if len(got) != 2 || got[0].GetName() != wantDataset || got[1].GetName() != wantBucket {
			t.Fatalf("%s search = %v; want %s and %s", tr, got, wantDataset, wantBucket)
		}
		b := got[1]
		if b.GetProjectId() != project || !strings.HasPrefix(b.GetProject(), "projects/") || b.GetCloudProduct() != "storage" ||
			b.GetResourceType() != "storage.googleapis.com/Bucket" || b.GetLocation() != "us" || b.GetLabels()["team"] != "inv" ||
			b.GetCryptoKeyVersion() != key.GetName()+"/cryptoKeyVersions/1" || b.GetCreateTime() == nil {
			t.Errorf("%s bucket = %v", tr, b)
		}

		it = c.SearchProtectedResources(ctx, &inventorypb.SearchProtectedResourcesRequest{Scope: "projects/" + project, CryptoKey: key.GetName(),
			ResourceTypes: []string{"bigquery.googleapis.com.*"}})
		r, err := it.Next()
		if err != nil || r.GetName() != wantDataset {
			t.Errorf("%s search of BigQuery types = %v, %v", tr, r, err)
		} else if _, err := it.Next(); err != iterator.Done {
			t.Errorf("%s search of BigQuery types gave more than the dataset: %v", tr, err)
		}

		_, err = c.SearchProtectedResources(ctx, &inventorypb.SearchProtectedResourcesRequest{Scope: "projects/" + project, CryptoKey: key.GetName(),
			ResourceTypes: []string{"nothing.googleapis.com/Thing"}}).Next()
		if code := kmsCode(tr, err); code != codes.InvalidArgument {
			t.Errorf("%s search of an untracked type = %v; want INVALID_ARGUMENT", tr, err)
		}
		_, err = c.GetProtectedResourcesSummary(ctx, &inventorypb.GetProtectedResourcesSummaryRequest{Name: ring.GetName() + "/cryptoKeys/missing"})
		if code := kmsCode(tr, err); code != codes.NotFound {
			t.Errorf("%s summary of a missing key = %v; want NOT_FOUND", tr, err)
		}
	}

	for tr, c := range dashboard {
		found := false
		it := c.ListCryptoKeys(ctx, &inventorypb.ListCryptoKeysRequest{Parent: "projects/" + project})
		for {
			ck, err := it.Next()
			if err == iterator.Done {
				break
			}
			if err != nil {
				t.Fatalf("%s ListCryptoKeys: %v", tr, err)
			}
			found = found || ck.GetName() == key.GetName()
		}
		if !found {
			t.Errorf("%s ListCryptoKeys left out %s", tr, key.GetName())
		}
	}
}

// TestGcloudKMSInventory (#2053): gcloud kms inventory, configured by
// `cloudburrow gcloud-setup` alone (its kmsinventory override), lists the
// project's keys and reports a CMEK bucket in the key's summary and search.
func TestGcloudKMSInventory(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvStorage)
	run, project := gcloudKMS(t, h)
	sc := storageClient(t, h)
	ctx := h.Context()
	k := kmsClients(t, h)["grpc"]
	suffix := fmt.Sprint(time.Now().UnixNano() % 1e9)
	ring, err := k.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: "projects/" + project + "/locations/us", KeyRingId: "ginv-" + suffix})
	if err != nil {
		t.Fatal(err)
	}
	key, err := k.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "cmek",
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}})
	if err != nil {
		t.Fatal(err)
	}
	grantStorageAgent(t, h, sc, k, project, key.GetName())
	bucket := "ginv-" + suffix + "-" + strings.ToLower(project)
	if len(bucket) > 63 {
		bucket = bucket[:63]
	}
	bh := sc.Bucket(bucket)
	if err := bh.Create(ctx, project, &storage.BucketAttrs{Location: "US", Encryption: &storage.BucketEncryption{DefaultKMSKeyName: key.GetName()}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		emptyAndDelete(cctx, bh)
	})
	gc := func(args ...string) string {
		t.Helper()
		out, err := run(args...)
		if err != nil {
			t.Fatalf("gcloud %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return out
	}
	if out := gc("kms", "inventory", "list-keys", "--project", project, "--format", "value(name)"); !strings.Contains(out, key.GetName()) {
		t.Errorf("list-keys left out %s:\n%s", key.GetName(), out)
	}
	if out := gc("kms", "inventory", "get-protected-resources-summary", "--keyname", key.GetName(), "--format", "value(resourceCount)"); strings.TrimSpace(out) != "1" {
		t.Errorf("get-protected-resources-summary resourceCount = %q; want 1", out)
	}
	if out := gc("kms", "inventory", "search-protected-resources", "--keyname", key.GetName(), "--scope", "projects/"+project,
		"--resource-types", "storage.googleapis.com/Bucket", "--format", "value(name)"); strings.TrimSpace(out) != "//storage.googleapis.com/"+bucket {
		t.Errorf("search-protected-resources = %q; want the bucket", out)
	}
}
