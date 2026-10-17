//go:build compat

package compat

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/api/googleapi"
	storagev1 "google.golang.org/api/storage/v1"
)

// Bucket IP filters and Rapid Caches (#1505) through gcloud and Terraform,
// read back through Google's generated JSON API client. gcloud has no
// command that sets a managed folder's rapidCacheConfig, and hashicorp/google
// 8.4.0 has neither a Rapid Cache resource nor a managed folder's
// rapid_cache_config, so those are covered by the official clients alone
// (storageipfilter_test.go).

// TestGcloudStorageIPFilter: `gcloud storage buckets create --ip-filter-file`
// makes a bucket with the filter the file holds; with mode Enabled and a
// public range that leaves this machine out, `gcloud storage ls` of its
// objects is refused while `buckets describe` still reads it (the bucket's
// metadata is exempt); `buckets update --ip-filter-file` admitting the local
// ranges lets the listing through, and `--clear-ip-filter` sets the mode
// Disabled, as gcloud sends it.
//
// covers: storage.buckets.insert, storage.buckets.get, storage.buckets.patch, storage.objects.list
func TestGcloudStorageIPFilter(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvStorage)
	s := storageJSON(t, h)
	g := newGcloudSession(t, h)
	ctx := h.Context()
	bucket := h.Project() + "-gipf"
	url := "gs://" + bucket
	dir := t.TempDir()
	file := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	closed := file("closed.json", `{"mode": "Enabled",
  "publicNetworkSource": {"allowedIpCidrRanges": ["203.0.113.0/24"]},
  "vpcNetworkSources": [{"network": "projects/`+h.Project()+`/global/networks/default", "allowedIpCidrRanges": ["10.0.0.0/8"]}],
  "allowCrossOrgVpcs": true}`)
	open := file("open.json", fmt.Sprintf(`{"mode": "Enabled", "publicNetworkSource": {"allowedIpCidrRanges": ["%s"]}}`,
		strings.Join(localSources, `", "`)))

	g.must("storage", "buckets", "create", url, "--location=us-central1", "--ip-filter-file="+closed)
	t.Cleanup(func() {
		_, _ = s.Buckets.Patch(bucket, &storagev1.Bucket{NullFields: []string{"IpFilter"}}).Do()
		_ = s.Buckets.Delete(bucket).Do()
	})
	b, err := s.Buckets.Get(bucket).Context(ctx).Do()
	if err != nil {
		t.Fatalf("buckets.get: %v", err)
	}
	f := b.IpFilter
	if f == nil || f.Mode != "Enabled" || f.PublicNetworkSource == nil || strings.Join(f.PublicNetworkSource.AllowedIpCidrRanges, ",") != "203.0.113.0/24" ||
		len(f.VpcNetworkSources) != 1 || f.VpcNetworkSources[0].Network != "projects/"+h.Project()+"/global/networks/default" || !f.AllowCrossOrgVpcs {
		t.Fatalf("after create --ip-filter-file the bucket's ipFilter = %+v", f)
	}
	if out, err := g.run(nil, "storage", "ls", url); err == nil {
		t.Errorf("gcloud storage ls through a filter that leaves this machine out succeeded:\n%s", out)
	}
	if out := g.must("storage", "buckets", "describe", url, "--format=json"); !strings.Contains(out, "203.0.113.0/24") {
		t.Errorf("buckets describe does not show the filter:\n%s", out)
	}

	g.must("storage", "buckets", "update", url, "--ip-filter-file="+open)
	if b, err = s.Buckets.Get(bucket).Context(ctx).Do(); err != nil || b.IpFilter == nil ||
		strings.Join(b.IpFilter.PublicNetworkSource.AllowedIpCidrRanges, ",") != strings.Join(localSources, ",") {
		t.Fatalf("after update --ip-filter-file the bucket's ipFilter = %+v, %v", b.IpFilter, err)
	}
	g.must("storage", "ls", url)

	g.must("storage", "buckets", "update", url, "--clear-ip-filter")
	if b, err = s.Buckets.Get(bucket).Context(ctx).Do(); err != nil || b.IpFilter == nil || b.IpFilter.Mode != "Disabled" {
		t.Fatalf("after --clear-ip-filter the bucket's ipFilter = %+v, %v; want mode Disabled", b.IpFilter, err)
	}
}

// TestGcloudStorageRapidCaches: `gcloud alpha storage buckets rapid-caches`
// create (rapid-cache-ultra, a TTL and no-read-admission, as gcloud spells
// it), describe, list, update and disable a bucket's Rapid Cache, read back
// through the generated JSON API client.
//
// covers: storage.rapidCaches.insert, storage.rapidCaches.get, storage.rapidCaches.list, storage.rapidCaches.update, storage.rapidCaches.disable
func TestGcloudStorageRapidCaches(t *testing.T) {
	h := New(t)
	s := storageJSON(t, h)
	b := locatedBucket(t, h, "-grc", "US-CENTRAL1")
	g := newGcloudSession(t, h)
	const zone = "us-central1-a"
	id := b + "/" + zone
	wait := func(want string) *storagev1.RapidCache {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for {
			c, err := s.RapidCaches.Get(b, zone).Do()
			if err == nil && c.State == want && !c.PendingUpdate {
				return c
			}
			if time.Now().After(deadline) {
				t.Fatalf("cache = %+v, %v; want %s", c, err, want)
			}
			time.Sleep(500 * time.Millisecond)
		}
	}

	g.must("alpha", "storage", "buckets", "rapid-caches", "create", "gs://"+b, zone,
		"--cache-type=rapid-cache-ultra", "--ttl=6h", "--admission-policy=no-read-admission")
	if c := wait("running"); c.CacheType != "rapid-cache-ultra" || c.Ttl != "21600s" || c.AdmissionPolicy != "no_read_admission" {
		t.Errorf("after create the cache = %+v", c)
	}
	if out := g.must("alpha", "storage", "buckets", "rapid-caches", "describe", id, "--format=json"); !strings.Contains(out, "21600s") || !strings.Contains(out, "running") {
		t.Errorf("describe = %s", out)
	}
	if out := g.must("alpha", "storage", "buckets", "rapid-caches", "list", "gs://"+b, "--format=json"); !strings.Contains(out, zone) {
		t.Errorf("list = %s", out)
	}
	g.must("alpha", "storage", "buckets", "rapid-caches", "update", id, "--ttl=2h", "--admission-policy=admit-on-first-miss")
	if c := wait("running"); c.Ttl != "7200s" || c.AdmissionPolicy != "admit-on-first-miss" {
		t.Errorf("after update the cache = %+v", c)
	}
	g.must("alpha", "storage", "buckets", "rapid-caches", "disable", id)
	wait("disabled")
}

// TestTerraformStorageBucketIPFilter: google_storage_bucket's ip_filter
// block applies (buckets.insert with the filter, Disabled, with public and
// VPC sources and allow_cross_org_vpcs), is read back by the generated JSON
// API client, plans clean, takes a change in place (Enabled, admitting every
// address, allow_all_service_agent_access), plans clean again, and is
// destroyed with the bucket.
//
// covers: storage.buckets.insert, storage.buckets.get, storage.buckets.patch, storage.buckets.delete
func TestTerraformStorageBucketIPFilter(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvStorage)
	s := storageJSON(t, h)
	m := newTFModule(t)
	ctx := h.Context()
	bucket := h.Project() + "-tfipf"
	network := "projects/" + h.Project() + "/global/networks/default"
	module := func(filter string) string {
		return fmt.Sprintf(`
resource "google_storage_bucket" "b" {
  project                     = %q
  name                        = %q
  location                    = "US-CENTRAL1"
  force_destroy               = true
  uniform_bucket_level_access = true
  ip_filter {
%s
  }
}
`, h.Project(), bucket, filter)
	}
	first := fmt.Sprintf(`    mode = "Disabled"
    allow_cross_org_vpcs = true
    public_network_source {
      allowed_ip_cidr_ranges = ["203.0.113.0/24", "2001:db8::/32"]
    }
    vpc_network_sources {
      network                = %q
      allowed_ip_cidr_ranges = ["10.0.0.0/8"]
    }`, network)
	second := `    mode = "Enabled"
    allow_all_service_agent_access = true
    public_network_source {
      allowed_ip_cidr_ranges = ["0.0.0.0/0", "::/0"]
    }`
	get := func(when string) *storagev1.BucketIpFilter {
		t.Helper()
		b, err := s.Buckets.Get(bucket).Context(ctx).Do()
		if err != nil {
			t.Fatalf("%s, buckets.get: %v", when, err)
		}
		if b.IpFilter == nil {
			t.Fatalf("%s, the bucket has no ipFilter", when)
		}
		return b.IpFilter
	}

	m.write(module(first))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	if f := get("after apply"); f.Mode != "Disabled" || strings.Join(f.PublicNetworkSource.AllowedIpCidrRanges, ",") != "203.0.113.0/24,2001:db8::/32" ||
		len(f.VpcNetworkSources) != 1 || f.VpcNetworkSources[0].Network != network || !f.AllowCrossOrgVpcs {
		t.Errorf("after apply the ipFilter = %+v", f)
	}
	m.planClean()

	m.write(module(second))
	out := m.must("apply", "-auto-approve", "-input=false", "-no-color")
	if !strings.Contains(out, "0 added, 1 changed, 0 destroyed") {
		t.Errorf("the filter change was not applied in place:\n%s", lastLines(out, 20))
	}
	if f := get("after the change"); f.Mode != "Enabled" || strings.Join(f.PublicNetworkSource.AllowedIpCidrRanges, ",") != "0.0.0.0/0,::/0" ||
		len(f.VpcNetworkSources) != 0 || !f.AllowAllServiceAgentAccess {
		t.Errorf("after the change the ipFilter = %+v", f)
	}
	m.planClean()

	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	_, err := s.Buckets.Get(bucket).Context(ctx).Do()
	if e, ok := err.(*googleapi.Error); !ok || e.Code != 404 {
		t.Errorf("bucket %s after destroy: %v; want 404", bucket, err)
	}
}
