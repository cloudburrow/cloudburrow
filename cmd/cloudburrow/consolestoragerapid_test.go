package main

import (
	"context"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	storagev1 "google.golang.org/api/storage/v1"

	"github.com/cloudburrow/cloudburrow/internal/console"
	gcsbuiltin "github.com/cloudburrow/cloudburrow/internal/service/storage"
)

// rapidProvider is a storage console on a server whose clock the test moves,
// so a Rapid Cache's operations finish when it says, with a US-CENTRAL1
// bucket "rcb" that has uniform bucket-level access.
func rapidProvider(t *testing.T) (storageProvider, *storagev1.Service, func(time.Duration)) {
	t.Helper()
	var mu sync.Mutex
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	srv, err := gcsbuiltin.NewServer(gcsbuiltin.Options{Now: func() time.Time { mu.Lock(); defer mu.Unlock(); return now }})
	if err != nil {
		t.Fatal(err)
	}
	h := httptest.NewServer(srv)
	t.Cleanup(h.Close)
	p := storageProvider{endpoint: strings.TrimPrefix(h.URL, "http://")}
	ctx := context.Background()
	s, err := jsonAPI(ctx, p.endpoint)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Buckets.Insert("p", &storagev1.Bucket{Name: "rcb", Location: "US-CENTRAL1",
		IamConfiguration: &storagev1.BucketIamConfiguration{UniformBucketLevelAccess: &storagev1.BucketIamConfigurationUniformBucketLevelAccess{Enabled: true}}}).Do(); err != nil {
		t.Fatal(err)
	}
	return p, s, func(d time.Duration) { mu.Lock(); now = now.Add(d); mu.Unlock() }
}

func rowNamed(l console.Listing, name string) *console.Resource {
	for i := range l.Items {
		if l.Items[i].Name == name {
			return &l.Items[i]
		}
	}
	return nil
}

// Rapid Caches (#1505) on a bucket's page: Create Rapid Cache answers with
// its operation and the Rapid caches tab lists the cache, saying no cache
// exists locally; once it runs, its row offers Edit Rapid Cache prefilled
// from rapidCaches.get, which changes it, and Disable Rapid Cache, which
// disables it.
func TestStorageRapidCachesThroughTheConsole(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p, s, advance := rapidProvider(t)
	if !hasAction(p.DetailActions(ctx, "p", []string{"rcb"}), "createrapidcache") {
		t.Fatal("the bucket's page does not offer Create Rapid Cache")
	}
	res, err := p.ActAtResult(ctx, "p", []string{"rcb"}, "createrapidcache", map[string]string{
		"zone": "us-central1-a", "cacheType": "rapid-cache-ultra", "ttl": "7200s", "admissionPolicy": "admit-on-second-miss", "ingestOnWrite": "true"})
	if err != nil || res == nil {
		t.Fatalf("Create Rapid Cache: %v", err)
	}
	advance(time.Minute)
	c, err := s.RapidCaches.Get("rcb", "us-central1-a").Do()
	if err != nil || c.CacheType != "rapid-cache-ultra" || c.Ttl != "7200s" || c.AdmissionPolicy != "admit-on-second-miss" || !c.IngestOnWrite || c.State != "running" {
		t.Fatalf("rapidCaches.get after Create Rapid Cache = %+v, %v", c, err)
	}
	d, err := p.Detail(ctx, "p", []string{"rcb"})
	if err != nil {
		t.Fatal(err)
	}
	sec := sectionNamed(d, "rapidcaches")
	if sec == nil || !strings.Contains(sec.Listing.Note, "no cache tier") {
		t.Fatalf("the Rapid caches tab is %+v", sec)
	}
	row := rowNamed(sec.Listing, "us-central1-a")
	if row == nil || row.Fields["Type"] != "rapid-cache-ultra" || row.Fields["State"] != "running" || strings.Join(row.Target, "|") != "_rapidcache|rcb|us-central1-a" {
		t.Fatalf("the cache's row is %+v", row)
	}
	acts := p.DetailActions(ctx, "p", row.Target)
	if !hasAction(acts, "editrapidcache") || !hasAction(acts, "disablerapidcache") {
		t.Fatalf("the cache's target offers %+v", acts)
	}
	for _, a := range acts {
		if a.ID == "editrapidcache" && a.Fields[0].Default != "7200s" {
			t.Errorf("Edit Rapid Cache is prefilled with TTL %q, want 7200s", a.Fields[0].Default)
		}
	}
	if _, err := p.ActAtResult(ctx, "p", row.Target, "editrapidcache", map[string]string{"ttl": "10800s", "admissionPolicy": "no_read_admission", "ingestOnWrite": "false"}); err != nil {
		t.Fatalf("Edit Rapid Cache: %v", err)
	}
	advance(time.Minute)
	if c, err = s.RapidCaches.Get("rcb", "us-central1-a").Do(); err != nil || c.Ttl != "10800s" || c.AdmissionPolicy != "no_read_admission" || c.IngestOnWrite {
		t.Fatalf("rapidCaches.get after Edit Rapid Cache = %+v, %v", c, err)
	}
	if err := p.ActAt(ctx, "p", row.Target, "disablerapidcache", nil); err != nil {
		t.Fatalf("Disable Rapid Cache: %v", err)
	}
	advance(time.Minute)
	if c, err = s.RapidCaches.Get("rcb", "us-central1-a").Do(); err != nil || c.State != "disabled" {
		t.Fatalf("rapidCaches.get after Disable Rapid Cache = %+v, %v", c, err)
	}
	if acts := p.DetailActions(ctx, "p", row.Target); len(acts) != 0 {
		t.Errorf("a disabled cache is offered %+v", acts)
	}
}

// Edit managed folder (#1505): a managed folder's row offers it, and it
// sets, changes and removes the folder's rapidCacheConfig policies, as
// managedFolders.get reads.
func TestStorageEditManagedFolderThroughTheConsole(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p, s, advance := rapidProvider(t)
	for _, z := range []string{"us-central1-a", "us-central1-b"} {
		if _, err := s.RapidCaches.Insert("rcb", &storagev1.RapidCache{Zone: z}).Do(); err != nil {
			t.Fatal(err)
		}
	}
	advance(time.Minute)
	if err := p.ActAt(ctx, "p", []string{"rcb"}, "createmanagedfolder", map[string]string{"name": "hot"}); err != nil {
		t.Fatal(err)
	}
	d, err := p.Detail(ctx, "p", []string{"rcb"})
	if err != nil {
		t.Fatal(err)
	}
	row := rowNamed(d.Sections[0].Listing, "hot/")
	if row == nil || !hasAction(row.Actions, "editmanagedfolder") || !hasAction(p.DetailActions(ctx, "p", row.Target), "editmanagedfolder") {
		t.Fatalf("the managed folder's row is %+v", row)
	}
	policies := func() map[string]storagev1.RapidCachePolicy {
		t.Helper()
		f, err := s.ManagedFolders.Get("rcb", "hot/").Do()
		if err != nil {
			t.Fatal(err)
		}
		if f.RapidCacheConfig == nil {
			return nil
		}
		return f.RapidCacheConfig.Policies
	}
	if err := p.ActAt(ctx, "p", row.Target, "editmanagedfolder", map[string]string{"rapidCaches": "us-central1-a ingest-on-write\nus-central1-b"}); err != nil {
		t.Fatalf("Edit managed folder: %v", err)
	}
	if got := policies(); len(got) != 2 || !ingestEnabled(got["us-central1-a"].IngestOnWrite) || ingestEnabled(got["us-central1-b"].IngestOnWrite) {
		t.Fatalf("after Edit managed folder the policies are %+v", got)
	}
	for _, a := range p.DetailActions(ctx, "p", row.Target) {
		if a.ID == "editmanagedfolder" && a.Fields[0].Default != "us-central1-a ingest-on-write\nus-central1-b" {
			t.Errorf("Edit managed folder is prefilled with %q", a.Fields[0].Default)
		}
	}
	if err := p.ActAt(ctx, "p", row.Target, "editmanagedfolder", map[string]string{"rapidCaches": "us-central1-b ingest-on-write"}); err != nil {
		t.Fatalf("Edit managed folder: %v", err)
	}
	if got := policies(); len(got) != 1 || !ingestEnabled(got["us-central1-b"].IngestOnWrite) {
		t.Fatalf("after taking one policy off the policies are %+v", got)
	}
	if err := p.ActAt(ctx, "p", row.Target, "editmanagedfolder", map[string]string{"rapidCaches": "us-central1-c"}); err == nil {
		t.Error("a policy naming a cache the bucket does not have was set")
	}
	if err := p.ActAt(ctx, "p", row.Target, "editmanagedfolder", map[string]string{"rapidCaches": ""}); err != nil {
		t.Fatalf("Edit managed folder to none: %v", err)
	}
	if got := policies(); len(got) != 0 {
		t.Fatalf("after emptying the list the policies are %+v", got)
	}
}

// IP filtering (#1505): the Configuration tab shows it, Edit IP filtering
// sets it as buckets.get reads, an enabled filter that leaves out this
// console's address refuses the object listing while the page and the
// action stay, and Not configured removes it.
func TestStorageIPFilterThroughTheConsole(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p, s, _ := rapidProvider(t)
	group := func() map[string]string {
		t.Helper()
		d, err := p.Detail(ctx, "p", []string{"rcb"})
		if err != nil {
			t.Fatal(err)
		}
		if !hasAction(d.Actions, "editipfilter") {
			t.Fatal("the bucket's page does not offer Edit IP filtering")
		}
		out := map[string]string{}
		for _, g := range sectionNamed(d, "configuration").Groups {
			if g.Heading == "IP filtering" {
				for _, pr := range g.Properties {
					out[pr.Label] = pr.Value
				}
			}
		}
		out["objects"] = d.Sections[0].Listing.Unavailable
		return out
	}
	if g := group(); g["IP filtering"] != ipFilterNone {
		t.Fatalf("a bucket with no filter shows %+v", g)
	}
	if err := p.ActAt(ctx, "p", []string{"rcb"}, "editipfilter", map[string]string{"mode": "Disabled",
		"publicRanges": "203.0.113.0/24\n2001:db8::/32", "vpcSources": "projects/p/global/networks/default 10.0.0.0/8,10.1.0.0/16",
		"allowCrossOrgVpcs": "true"}); err != nil {
		t.Fatalf("Edit IP filtering: %v", err)
	}
	b, err := s.Buckets.Get("rcb").Do()
	if err != nil {
		t.Fatal(err)
	}
	f := b.IpFilter
	if f == nil || f.Mode != "Disabled" || strings.Join(f.PublicNetworkSource.AllowedIpCidrRanges, ",") != "203.0.113.0/24,2001:db8::/32" ||
		len(f.VpcNetworkSources) != 1 || f.VpcNetworkSources[0].Network != "projects/p/global/networks/default" ||
		len(f.VpcNetworkSources[0].AllowedIpCidrRanges) != 2 || !f.AllowCrossOrgVpcs || f.AllowAllServiceAgentAccess {
		t.Fatalf("buckets.get after Edit IP filtering reads %+v", f)
	}
	if g := group(); g["IP filtering"] != "Disabled" || g["Public network ranges"] != "203.0.113.0/24, 2001:db8::/32" || g["Allow cross-org VPCs"] != "Yes" || g["objects"] != "" {
		t.Fatalf("the IP filtering group is %+v", g)
	}
	if err := p.ActAt(ctx, "p", []string{"rcb"}, "editipfilter", map[string]string{"mode": "Enabled", "publicRanges": "203.0.113.0/24"}); err != nil {
		t.Fatalf("Edit IP filtering: %v", err)
	}
	if g := group(); g["IP filtering"] != "Enabled" || !strings.Contains(g["objects"], "IP filter") {
		t.Fatalf("an enabled filter leaving out the console's address shows %+v", g)
	}
	if err := p.ActAt(ctx, "p", []string{"rcb"}, "editipfilter", map[string]string{"mode": "Enabled", "publicRanges": "not-a-range"}); err == nil {
		t.Error("a range that does not parse was set")
	}
	if err := p.ActAt(ctx, "p", []string{"rcb"}, "editipfilter", map[string]string{"mode": "Enabled", "publicRanges": "127.0.0.0/8, ::1/128"}); err != nil {
		t.Fatalf("Edit IP filtering: %v", err)
	}
	if g := group(); g["objects"] != "" {
		t.Fatalf("a filter admitting the console's address still refuses it: %+v", g)
	}
	if err := p.ActAt(ctx, "p", []string{"rcb"}, "editipfilter", map[string]string{"mode": ipFilterNone}); err != nil {
		t.Fatalf("Edit IP filtering to none: %v", err)
	}
	if b, err := s.Buckets.Get("rcb").Do(); err != nil || b.IpFilter != nil {
		t.Fatalf("buckets.get after removing the filter reads %+v, %v", b.IpFilter, err)
	}
}
