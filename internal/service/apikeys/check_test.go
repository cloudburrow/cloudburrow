package apikeys

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/store"
)

// testToolKey stands in for a key Google's tools ship, which this
// repository holds only by hash (toolKeys).
const testToolKey = "AIzaCloudBurrowTestToolKey"

func TestAuthorize(t *testing.T) {
	sum := sha256.Sum256([]byte(testToolKey))
	toolKeys[hex.EncodeToString(sum[:])] = true
	t.Cleanup(func() { delete(toolKeys, hex.EncodeToString(sum[:])) })
	s := NewServer("", store.NewMemory(), nil)
	mk := func(id string, r *Restrictions) string {
		t.Helper()
		k, err := s.Create("p1", id, "", r, nil, "")
		if err != nil {
			t.Fatal(err)
		}
		return k.KeyString
	}
	open := mk("open", nil)
	web := mk("web", &Restrictions{BrowserKeyRestrictions: &BrowserKeyRestrictions{AllowedReferrers: []string{"*.example.com/*", "www.example.org"}}})
	ip := mk("ip", &Restrictions{ServerKeyRestrictions: &ServerKeyRestrictions{AllowedIps: []string{"198.51.100.0/24", "2001:db8::1"}}})
	android := mk("android", &Restrictions{AndroidKeyRestrictions: &AndroidKeyRestrictions{AllowedApplications: []AndroidApplication{
		{PackageName: "com.example.app", Sha1Fingerprint: "DA39A3EE5E6B4B0D3255BFEF95601890AFD80709"}}}})
	ios := mk("ios", &Restrictions{IosKeyRestrictions: &IosKeyRestrictions{AllowedBundleIds: []string{"com.example.ios"}}})
	api := mk("api", &Restrictions{APITargets: []APITarget{{Service: "pubsub.googleapis.com", Methods: []string{"Get*", "google.pubsub.v1.Publisher.Publish"}}}})

	storage := Request{Service: "storage.googleapis.com", Method: "storage.objects.get"}
	for _, c := range []struct {
		name   string
		key    string
		req    Request
		reason string
	}{
		{"unknown key", "AIzaNope", storage, "API_KEY_INVALID"},
		{"a tool's key", testToolKey, storage, ""},
		{"open key", open, storage, ""},
		{"referrer ok", web, Request{Service: "storage.googleapis.com", Referrer: "https://app.example.com/page"}, ""},
		{"bare domain", web, Request{Service: "storage.googleapis.com", Referrer: "https://www.example.org/"}, ""},
		{"bare domain path", web, Request{Service: "storage.googleapis.com", Referrer: "https://www.example.org/x"}, "API_KEY_HTTP_REFERRER_BLOCKED"},
		{"apex not subdomain", web, Request{Service: "storage.googleapis.com", Referrer: "https://example.com/"}, "API_KEY_HTTP_REFERRER_BLOCKED"},
		{"no referrer", web, storage, "API_KEY_HTTP_REFERRER_BLOCKED"},
		{"ip ok", ip, Request{Service: "s", IP: "198.51.100.7"}, ""},
		{"ipv6 ok", ip, Request{Service: "s", IP: "2001:db8::1"}, ""},
		{"ip blocked", ip, Request{Service: "s", IP: "127.0.0.1"}, "API_KEY_IP_ADDRESS_BLOCKED"},
		{"android ok", android, Request{Service: "s", AndroidPackage: "com.example.app", AndroidCert: "da:39:a3:ee:5e:6b:4b:0d:32:55:bf:ef:95:60:18:90:af:d8:07:09"}, ""},
		{"android wrong cert", android, Request{Service: "s", AndroidPackage: "com.example.app", AndroidCert: "0000000000000000000000000000000000000000"}, "API_KEY_ANDROID_APP_BLOCKED"},
		{"ios ok", ios, Request{Service: "s", IOSBundle: "com.example.ios"}, ""},
		{"ios blocked", ios, Request{Service: "s"}, "API_KEY_IOS_APP_BLOCKED"},
		{"api ok wildcard", api, Request{Service: "pubsub.googleapis.com", Method: "google.pubsub.v1.Publisher.GetTopic"}, ""},
		{"api ok full", api, Request{Service: "PubSub.googleapis.com", Method: "google.pubsub.v1.Publisher.Publish"}, ""},
		{"api wrong method", api, Request{Service: "pubsub.googleapis.com", Method: "google.pubsub.v1.Publisher.DeleteTopic"}, "API_KEY_SERVICE_BLOCKED"},
		{"api wrong service", api, storage, "API_KEY_SERVICE_BLOCKED"},
	} {
		_, d := s.Authorize(c.key, c.req)
		got := ""
		if d != nil {
			got = d.Reason
		}
		if got != c.reason {
			t.Errorf("%s: reason %q, want %q (%v)", c.name, got, c.reason, d)
		}
	}
	_, d := s.Authorize("AIzaNope", storage)
	if d.HTTP != 400 || d.Status != "INVALID_ARGUMENT" || d.Message != "API key not valid. Please pass a valid API key." || d.Metadata["service"] != "storage.googleapis.com" {
		t.Errorf("invalid: %+v", d)
	}
	_, d = s.Authorize(web, storage)
	if d.HTTP != 403 || d.Status != "PERMISSION_DENIED" || d.Message != "Requests from referer <empty> are blocked." || d.Metadata["consumer"] == "" {
		t.Errorf("referrer: %+v", d)
	}
	_, d = s.Authorize(api, storage)
	if d.Message != "Requests to this API storage.googleapis.com method storage.objects.get are blocked." {
		t.Errorf("service blocked: %q", d.Message)
	}
}
