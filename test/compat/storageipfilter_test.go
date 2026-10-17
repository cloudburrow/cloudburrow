//go:build compat

package compat

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/storage/control/apiv2/controlpb"
	"google.golang.org/api/iterator"
	storagev1 "google.golang.org/api/storage/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	pb "github.com/cloudburrow/cloudburrow/internal/service/storage/storagev2pb"
)

// Bucket IP filters, Rapid Caches and managedFolders.update (#1505),
// through Google's generated JSON API client (google.golang.org/api/
// storage/v1), the google.storage.v2 gRPC API, and the official Storage
// Control client's gRPC transport.

// localSources are the ranges this machine's callers come from: loopback
// and the private LAN ranges.
var localSources = []string{"127.0.0.0/8", "::1/128", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "fc00::/7"}

// TestStorageIPFilterJSON: a bucket's ipFilter is stored and returned; with
// mode Enabled and a public source that leaves this machine out, object
// reads and writes are refused 403 while the bucket's own metadata stays
// reachable (the exemptFromIpFilter bypass); a filter naming the local
// ranges, or mode Disabled, admits them. VPC sources are stored.
//
// unverified: storage.objects.list 403: a caller outside the bucket's IP filter (the message is not documented)
// unverified: storage.buckets.insert 400: an ipFilter mode other than Enabled or Disabled
// covers: storage.buckets.insert, storage.buckets.get, storage.buckets.patch, storage.objects.list, storage.objects.insert
func TestStorageIPFilterJSON(t *testing.T) {
	h := New(t)
	s := storageJSON(t, h)
	name := h.Project() + "-ipf"
	if _, err := s.Buckets.Insert(h.Project(), &storagev1.Bucket{Name: name, IpFilter: &storagev1.BucketIpFilter{Mode: "On"}}).Do(); httpCode(err) != http.StatusBadRequest {
		t.Errorf("mode On = %v; want 400", err)
	}
	b, err := s.Buckets.Insert(h.Project(), &storagev1.Bucket{Name: name, Location: "US", IpFilter: &storagev1.BucketIpFilter{
		Mode:                "Enabled",
		PublicNetworkSource: &storagev1.BucketIpFilterPublicNetworkSource{AllowedIpCidrRanges: []string{"203.0.113.0/24"}},
		VpcNetworkSources: []*storagev1.BucketIpFilterVpcNetworkSources{{Network: "projects/" + h.Project() + "/global/networks/default",
			AllowedIpCidrRanges: []string{"10.0.0.0/8"}}},
		AllowCrossOrgVpcs: true,
	}}).Do()
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	t.Cleanup(func() {
		_, _ = s.Buckets.Patch(name, &storagev1.Bucket{IpFilter: &storagev1.BucketIpFilter{Mode: "Disabled"}}).Do()
		emptyAndDelete(h.Context(), storageClient(t, h).Bucket(name))
	})
	if f := b.IpFilter; f == nil || f.Mode != "Enabled" || len(f.VpcNetworkSources) != 1 || !f.AllowCrossOrgVpcs {
		t.Errorf("insert returned ipFilter %+v", f)
	}
	got, err := s.Buckets.Get(name).Do()
	if err != nil || got.IpFilter == nil || got.IpFilter.PublicNetworkSource.AllowedIpCidrRanges[0] != "203.0.113.0/24" {
		t.Fatalf("get (exempt) = %+v, %v", got, err)
	}
	if _, err := s.Objects.List(name).Do(); httpCode(err) != http.StatusForbidden {
		t.Errorf("objects.list from this machine = %v; want 403", err)
	}
	if _, err := s.Objects.Insert(name, &storagev1.Object{Name: "o.txt"}).Media(strings.NewReader("x")).Do(); httpCode(err) != http.StatusForbidden {
		t.Errorf("objects.insert from this machine = %v; want 403", err)
	}
	if _, err := s.Buckets.Patch(name, &storagev1.Bucket{IpFilter: &storagev1.BucketIpFilter{Mode: "Enabled",
		PublicNetworkSource: &storagev1.BucketIpFilterPublicNetworkSource{AllowedIpCidrRanges: localSources}}}).Do(); err != nil {
		t.Fatalf("patch (exempt) to the local ranges: %v", err)
	}
	if _, err := s.Objects.Insert(name, &storagev1.Object{Name: "o.txt"}).Media(strings.NewReader("x")).Do(); err != nil {
		t.Errorf("objects.insert admitted = %v", err)
	}
	if l, err := s.Objects.List(name).Do(); err != nil || len(l.Items) != 1 {
		t.Errorf("objects.list admitted = %+v, %v", l, err)
	}
	if _, err := s.Buckets.Patch(name, &storagev1.Bucket{IpFilter: &storagev1.BucketIpFilter{Mode: "Disabled",
		PublicNetworkSource: &storagev1.BucketIpFilterPublicNetworkSource{AllowedIpCidrRanges: []string{"203.0.113.0/24"}}}}).Do(); err != nil {
		t.Fatalf("patch to Disabled: %v", err)
	}
	if _, err := s.Objects.List(name).Do(); err != nil {
		t.Errorf("objects.list with the filter Disabled = %v", err)
	}
}

// TestStorageIPFilterGRPC: google.storage.v2 stores ip_filter through
// UpdateBucket's mask and returns it from GetBucket; an Enabled filter that
// leaves this machine out refuses ListObjects PERMISSION_DENIED.
//
// covers: storage.buckets.patch, storage.buckets.get, storage.objects.list
func TestStorageIPFilterGRPC(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	name := locatedBucket(t, h, "-ipg", "US")
	conn, err := grpc.NewClient(stripScheme(h.Endpoint(EnvStorage)), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	c := pb.NewStorageClient(conn)
	res := "projects/_/buckets/" + name
	mask := &fieldmaskpb.FieldMask{Paths: []string{"ip_filter"}}
	set := func(mode string, ranges ...string) (*pb.Bucket, error) {
		return c.UpdateBucket(ctx, &pb.UpdateBucketRequest{Bucket: &pb.Bucket{Name: res, IpFilter: &pb.Bucket_IpFilter{
			Mode: proto.String(mode), PublicNetworkSource: &pb.Bucket_IpFilter_PublicNetworkSource{AllowedIpCidrRanges: ranges}}}, UpdateMask: mask})
	}
	t.Cleanup(func() { _, _ = set("Disabled") })
	if _, err := set("Enabled", "not-a-cidr"); grpcCode(err) != codes.InvalidArgument {
		t.Errorf("a bad range = %v; want INVALID_ARGUMENT", err)
	}
	if _, err := set("Enabled", "198.51.100.0/24"); err != nil {
		t.Fatalf("UpdateBucket ip_filter: %v", err)
	}
	got, err := c.GetBucket(ctx, &pb.GetBucketRequest{Name: res})
	if err != nil || got.GetIpFilter().GetMode() != "Enabled" || got.GetIpFilter().GetPublicNetworkSource().GetAllowedIpCidrRanges()[0] != "198.51.100.0/24" {
		t.Fatalf("GetBucket = %v, %v", got.GetIpFilter(), err)
	}
	if _, err := c.ListObjects(ctx, &pb.ListObjectsRequest{Parent: res}); grpcCode(err) != codes.PermissionDenied {
		t.Errorf("ListObjects from this machine = %v; want PERMISSION_DENIED", err)
	}
	if _, err := set("Enabled", localSources...); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ListObjects(ctx, &pb.ListObjectsRequest{Parent: res}); err != nil {
		t.Errorf("ListObjects admitted = %v", err)
	}
}

// TestStorageRapidCachesJSON: insert returns a create operation, after which
// the cache is running with its type; a second cache in the zone and a zone
// outside the location are refused; update and disable are operations; a
// managed folder's rapidCacheConfig names the cache, through
// managedFolders.update with patch semantics and metageneration
// preconditions.
//
// unverified: storage.rapidCaches.insert 409: a second cache in the zone
// unverified: storage.rapidCaches.insert 400: a zone outside the bucket's location
// unverified: storage.managedFolders.update 400: a policy naming a cache the bucket does not have
// covers: storage.rapidCaches.insert, storage.rapidCaches.get, storage.rapidCaches.list, storage.rapidCaches.update, storage.rapidCaches.disable
// covers: storage.managedFolders.update, storage.managedFolders.insert, storage.managedFolders.get
func TestStorageRapidCachesJSON(t *testing.T) {
	h := New(t)
	s := storageJSON(t, h)
	b := locatedBucket(t, h, "-rcj", "US-CENTRAL1")
	if _, err := s.Buckets.Patch(b, &storagev1.Bucket{IamConfiguration: &storagev1.BucketIamConfiguration{
		UniformBucketLevelAccess: &storagev1.BucketIamConfigurationUniformBucketLevelAccess{Enabled: true}}}).Do(); err != nil {
		t.Fatal(err)
	}
	op, err := s.RapidCaches.Insert(b, &storagev1.RapidCache{Zone: "us-central1-a", CacheType: "rapid-cache-ultra", Ttl: "7200s"}).Do()
	if err != nil || op.Done {
		t.Fatalf("insert = %+v, %v; want a running operation", op, err)
	}
	if md := opMetadata(t, op); md["rapidCacheId"] != "us-central1-a" || md["cacheType"] != "rapid-cache-ultra" ||
		md["@type"] != "type.googleapis.com/google.storage.control.v2.CreateRapidCacheMetadata" {
		t.Errorf("create metadata = %v", md)
	}
	if _, err := s.RapidCaches.Insert(b, &storagev1.RapidCache{Zone: "us-central1-a"}).Do(); httpCode(err) != http.StatusConflict {
		t.Errorf("second cache in the zone = %v; want 409", err)
	}
	if _, err := s.RapidCaches.Insert(b, &storagev1.RapidCache{Zone: "europe-west1-b"}).Do(); httpCode(err) != http.StatusBadRequest {
		t.Errorf("cache outside the location = %v; want 400", err)
	}
	if done := waitOp(t, s, b, lastSegment(op.Name)); !done.Done || done.Error != nil {
		t.Fatalf("create operation = %+v", done)
	}
	if c, err := s.RapidCaches.Get(b, "us-central1-a").Do(); err != nil || c.State != "running" || c.CacheType != "rapid-cache-ultra" || c.Ttl != "7200s" {
		t.Errorf("get = %+v, %v", c, err)
	}
	up, err := s.RapidCaches.Update(b, "us-central1-a", &storagev1.RapidCache{AdmissionPolicy: "admit-on-second-miss"}).Do()
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	waitOp(t, s, b, lastSegment(up.Name))
	if l, err := s.RapidCaches.List(b).Do(); err != nil || len(l.Items) != 1 || l.Items[0].AdmissionPolicy != "admit-on-second-miss" {
		t.Errorf("list = %+v, %v", l, err)
	}

	mf, err := s.ManagedFolders.Insert(b, &storagev1.ManagedFolder{Name: "team/"}).Do()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ManagedFolders.Update(b, "team/", &storagev1.ManagedFolder{RapidCacheConfig: &storagev1.RapidCacheConfig{
		Policies: map[string]storagev1.RapidCachePolicy{"us-east1-b": {RapidCacheId: "us-east1-b"}}}}).Do(); httpCode(err) != http.StatusBadRequest {
		t.Errorf("a policy naming no cache = %v; want 400", err)
	}
	got, err := s.ManagedFolders.Update(b, "team/", &storagev1.ManagedFolder{RapidCacheConfig: &storagev1.RapidCacheConfig{
		Policies: map[string]storagev1.RapidCachePolicy{"us-central1-a": {RapidCacheId: "us-central1-a", IngestOnWrite: "enabled"}}}}).
		IfMetagenerationMatch(mf.Metageneration).Do()
	if err != nil || got.Metageneration != mf.Metageneration+1 || got.RapidCacheConfig.Policies["us-central1-a"].IngestOnWrite != "enabled" {
		t.Fatalf("update = %+v, %v", got, err)
	}
	if _, err := s.ManagedFolders.Update(b, "team/", &storagev1.ManagedFolder{}).IfMetagenerationMatch(mf.Metageneration).Do(); httpCode(err) != http.StatusPreconditionFailed {
		t.Errorf("a stale metageneration = %v; want 412", err)
	}
	if g, err := s.ManagedFolders.Get(b, "team/").Do(); err != nil || g.RapidCacheConfig == nil {
		t.Errorf("get = %+v, %v", g, err)
	}

	dis, err := s.RapidCaches.Disable(b, "us-central1-a").Do()
	if err != nil {
		t.Fatalf("disable: %v", err)
	}
	if done := waitOp(t, s, b, lastSegment(dis.Name)); !done.Done || done.Error != nil {
		t.Fatalf("disable operation = %+v", done)
	}
	if c, err := s.RapidCaches.Get(b, "us-central1-a").Do(); err != nil || c.State != "disabled" {
		t.Errorf("get after disable = %+v, %v", c, err)
	}
	_ = s.ManagedFolders.Delete(b, "team/").Do()
}

// TestStorageRapidCachesControl: the Storage Control client's gRPC transport
// creates, waits on, gets, lists, updates and disables a Rapid Cache, and
// creates, updates (rapid_cache_config), gets, lists and deletes a managed
// folder.
//
// covers: storage.rapidCaches.insert, storage.rapidCaches.get, storage.rapidCaches.list, storage.rapidCaches.update, storage.rapidCaches.disable
// covers: storage.managedFolders.insert, storage.managedFolders.update, storage.managedFolders.get, storage.managedFolders.list, storage.managedFolders.delete
func TestStorageRapidCachesControl(t *testing.T) {
	h := New(t)
	sc := storageControlGRPC(t, h)
	s := storageJSON(t, h)
	ctx := h.Context()
	b := locatedBucket(t, h, "-rcg", "US-CENTRAL1")
	if _, err := s.Buckets.Patch(b, &storagev1.Bucket{IamConfiguration: &storagev1.BucketIamConfiguration{
		UniformBucketLevelAccess: &storagev1.BucketIamConfigurationUniformBucketLevelAccess{Enabled: true}}}).Do(); err != nil {
		t.Fatal(err)
	}
	parent := "projects/_/buckets/" + b
	op, err := sc.CreateRapidCache(ctx, &controlpb.CreateRapidCacheRequest{Parent: parent,
		RapidCache: &controlpb.RapidCache{Zone: "us-central1-b", CacheType: "rapid-cache", Ttl: durationpb.New(3 * time.Hour)}})
	if err != nil {
		t.Fatalf("CreateRapidCache: %v", err)
	}
	rc, err := op.Wait(ctx)
	if err != nil || rc.GetState() != "running" || rc.GetName() != parent+"/rapidCaches/us-central1-b" {
		t.Fatalf("create wait = %v, %v", rc, err)
	}
	if g, err := sc.GetRapidCache(ctx, &controlpb.GetRapidCacheRequest{Name: rc.GetName()}); err != nil || g.GetTtl().AsDuration() != 3*time.Hour {
		t.Errorf("GetRapidCache = %v, %v", g, err)
	}
	upOp, err := sc.UpdateRapidCache(ctx, &controlpb.UpdateRapidCacheRequest{RapidCache: &controlpb.RapidCache{Name: rc.GetName(), Ttl: durationpb.New(4 * time.Hour)},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"ttl"}}})
	if err != nil {
		t.Fatalf("UpdateRapidCache: %v", err)
	}
	if u, err := upOp.Wait(ctx); err != nil || u.GetTtl().AsDuration() != 4*time.Hour {
		t.Errorf("update wait = %v, %v", u, err)
	}
	it := sc.ListRapidCaches(ctx, &controlpb.ListRapidCachesRequest{Parent: parent})
	n := 0
	for {
		_, err := it.Next()
		if err == iterator.Done {
			break
		} else if err != nil {
			t.Fatalf("ListRapidCaches: %v", err)
		}
		n++
	}
	if n != 1 {
		t.Errorf("ListRapidCaches = %d caches, want 1", n)
	}

	mf, err := sc.CreateManagedFolder(ctx, &controlpb.CreateManagedFolderRequest{Parent: parent, ManagedFolderId: "data/"})
	if err != nil {
		t.Fatalf("CreateManagedFolder: %v", err)
	}
	cfg := &controlpb.ManagedFolder_RapidCacheConfig{Policies: map[string]*controlpb.ManagedFolder_RapidCacheConfig_RapidCachePolicy{
		"us-central1-b": {RapidCacheId: "us-central1-b", IngestOnWrite: controlpb.ManagedFolder_RapidCacheConfig_RapidCachePolicy_INGEST_ON_WRITE_ENABLED}}}
	up, err := sc.UpdateManagedFolder(ctx, &controlpb.UpdateManagedFolderRequest{ManagedFolder: &controlpb.ManagedFolder{Name: mf.GetName(), RapidCacheConfig: cfg},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"rapid_cache_config"}}, IfMetagenerationMatch: proto.Int64(mf.GetMetageneration())})
	if err != nil || up.GetMetageneration() != mf.GetMetageneration()+1 ||
		up.GetRapidCacheConfig().GetPolicies()["us-central1-b"].GetIngestOnWrite() != controlpb.ManagedFolder_RapidCacheConfig_RapidCachePolicy_INGEST_ON_WRITE_ENABLED {
		t.Fatalf("UpdateManagedFolder = %v, %v", up, err)
	}
	if _, err := sc.UpdateManagedFolder(ctx, &controlpb.UpdateManagedFolderRequest{ManagedFolder: &controlpb.ManagedFolder{Name: mf.GetName()},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"rapid_cache_config.policies.us-central1-b.ingest_on_write"}}}); grpcCode(err) != codes.InvalidArgument {
		t.Errorf("a path within a policy = %v; want INVALID_ARGUMENT", err)
	}
	if g, err := sc.GetManagedFolder(ctx, &controlpb.GetManagedFolderRequest{Name: mf.GetName()}); err != nil || len(g.GetRapidCacheConfig().GetPolicies()) != 1 {
		t.Errorf("GetManagedFolder = %v, %v", g, err)
	}
	mit := sc.ListManagedFolders(ctx, &controlpb.ListManagedFoldersRequest{Parent: parent})
	if f, err := mit.Next(); err != nil || f.GetName() != mf.GetName() {
		t.Errorf("ListManagedFolders = %v, %v", f, err)
	}
	if err := sc.DeleteManagedFolder(ctx, &controlpb.DeleteManagedFolderRequest{Name: mf.GetName()}); err != nil {
		t.Errorf("DeleteManagedFolder: %v", err)
	}
	dop, err := sc.DisableRapidCache(ctx, &controlpb.DisableRapidCacheRequest{Name: rc.GetName()})
	if err != nil {
		t.Fatalf("DisableRapidCache: %v", err)
	}
	if d, err := dop.Wait(ctx); err != nil || d.GetState() != "disabled" {
		t.Errorf("disable wait = %v, %v", d, err)
	}
}
