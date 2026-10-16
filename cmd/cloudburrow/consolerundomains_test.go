package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	runv1 "google.golang.org/api/run/v1"
)

// fakeRunV1Domains serves the v1 domain mapping calls the service page
// makes, and records the mappings created and deleted.
type fakeRunV1Domains struct {
	mu       sync.Mutex
	mappings map[string]*runv1.DomainMapping
	paths    []string
}

func (f *fakeRunV1Domains) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.paths = append(f.paths, r.Method+" "+r.URL.Path)
	const ns = "/apis/domains.cloudrun.com/v1/namespaces/demo/domainmappings"
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v1/projects/demo/locations/us-central1/domainmappings":
		out := &runv1.ListDomainMappingsResponse{}
		for _, dm := range f.mappings {
			out.Items = append(out.Items, dm)
		}
		_ = json.NewEncoder(w).Encode(out)
	case r.Method == http.MethodPost && r.URL.Path == ns:
		var dm runv1.DomainMapping
		_ = json.NewDecoder(r.Body).Decode(&dm)
		f.mappings[dm.Metadata.Name] = &dm
		_ = json.NewEncoder(w).Encode(&dm)
	case strings.HasPrefix(r.URL.Path, ns+"/"):
		name := strings.TrimPrefix(r.URL.Path, ns+"/")
		dm, ok := f.mappings[name]
		if !ok {
			http.Error(w, `{"error":{"code":404,"message":"not found"}}`, http.StatusNotFound)
			return
		}
		if r.Method == http.MethodDelete {
			delete(f.mappings, name)
			_ = json.NewEncoder(w).Encode(&runv1.Status{Status: "Success"})
			return
		}
		_ = json.NewEncoder(w).Encode(dm)
	default:
		http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotImplemented)
	}
}

// TestConsoleRunDomainMappings (#1450): a service's page has a Domain
// mappings tab, listing the domains mapped to that service with each
// condition, its reason and the DNS records, and the actions Map a domain
// and Remove domain mapping, which create and delete a mapping through the
// v1 API; removing a domain mapped to another service is refused.
func TestConsoleRunDomainMappings(t *testing.T) {
	fake := &fakeRunV1Domains{mappings: map[string]*runv1.DomainMapping{
		"other.example.com": {Metadata: &runv1.ObjectMeta{Name: "other.example.com"}, Spec: &runv1.DomainMappingSpec{RouteName: "billing"}},
	}}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")
	p := runProvider{runAddr: func() string { return host }, defaultProject: "demo"}
	ctx := context.Background()

	if err := p.actOnDomains(ctx, "", "api", runMapDomain, map[string]string{"domain": "WWW.Example.com", "certificateMode": "NONE"}); err != nil {
		t.Fatalf("Map a domain: %v", err)
	}
	created := fake.mappings["www.example.com"]
	if created == nil || created.Spec.RouteName != "api" || created.Spec.CertificateMode != "NONE" ||
		created.Metadata.Labels["cloud.googleapis.com/location"] != "us-central1" {
		t.Fatalf("created mapping = %+v", created)
	}
	created.Status = &runv1.DomainMappingStatus{
		ResourceRecords: []*runv1.ResourceRecord{{Name: "www", Type: "CNAME", Rrdata: "ghs.googlehosted.com."}},
		Conditions: []*runv1.GoogleCloudRunV1Condition{
			{Type: "Ready", Status: "True"},
			{Type: "CertificateProvisioned", Status: "True"},
			{Type: "DomainRoutable", Status: "True", Message: "routed by this instance's Cloud Run gateway"},
		}}
	sec := p.domainsSection(ctx, "", "api")
	if sec.Listing.Unavailable != "" || len(sec.Listing.Items) != 1 {
		t.Fatalf("Domain mappings tab = %+v; want the one domain mapped to api", sec.Listing)
	}
	row := sec.Listing.Items[0]
	if row.Name != "www.example.com" || row.Fields["Ready"] != "True" || row.Fields["DNS records"] != "www CNAME ghs.googlehosted.com." ||
		!strings.Contains(row.Fields["Detail"], "gateway") || sec.Listing.Note == "" {
		t.Errorf("row = %+v", row)
	}
	if err := p.actOnDomains(ctx, "", "api", runUnmapDomain, map[string]string{"domain": "other.example.com"}); err == nil {
		t.Error("removing a domain mapped to another service was accepted")
	}
	if err := p.actOnDomains(ctx, "", "api", runUnmapDomain, map[string]string{"domain": "www.example.com"}); err != nil {
		t.Fatalf("Remove domain mapping: %v", err)
	}
	if _, ok := fake.mappings["www.example.com"]; ok {
		t.Error("the mapping was not deleted")
	}
}

// An AUTOMATIC mapping's row says why it is not Ready.
func TestDomainRowSaysWhyNotReady(t *testing.T) {
	row := domainRow(&runv1.DomainMapping{Metadata: &runv1.ObjectMeta{Name: "a.example.com"},
		Spec: &runv1.DomainMappingSpec{CertificateMode: "AUTOMATIC"},
		Status: &runv1.DomainMappingStatus{Conditions: []*runv1.GoogleCloudRunV1Condition{
			{Type: "Ready", Status: "False", Reason: "CertificateNotIssued", Message: "CloudBurrow issues no certificate"},
			{Type: "CertificateProvisioned", Status: "False", Reason: "CertificateNotIssued"},
			{Type: "DomainRoutable", Status: "True"},
		}}})
	if row.Fields["Ready"] != "False (CertificateNotIssued)" || row.Fields["Detail"] != "CloudBurrow issues no certificate" || row.Fields["Routable"] != "True" {
		t.Errorf("row = %+v", row.Fields)
	}
}
