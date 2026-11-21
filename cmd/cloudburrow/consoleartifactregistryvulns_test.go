package main

import (
	"fmt"
	"strings"
	"testing"

	grafeaspb "google.golang.org/genproto/googleapis/grafeas/v1"
	spb "google.golang.org/genproto/googleapis/rpc/status"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// A Vulnerabilities row opens its Vulnerability details, View or View
// Fixed, with its file locations and their layers; the tab's Layer bar
// lists each layer with vulnerabilities (#2066).
func TestConsoleArtifactRegistryVulnerabilityPanelAndLayerBar(t *testing.T) {
	layer := &grafeaspb.LayerDetails{Index: 2, DiffId: "sha256:5f1a", Command: "RUN apk add musl",
		BaseImages: []*grafeaspb.BaseImage{{Name: "alpine:3.20"}}}
	v := &grafeaspb.VulnerabilityOccurrence{ShortDescription: "CVE-1", FixAvailable: true, LongDescription: "a bug",
		RelatedUrls: []*grafeaspb.RelatedUrl{{Url: "https://osv.dev/vulnerability/CVE-1"}}}
	pi := &grafeaspb.VulnerabilityOccurrence_PackageIssue{AffectedPackage: "musl", FileLocation: []*grafeaspb.FileLocation{
		{FilePath: "/lib/apk/db/installed", LayerDetails: layer}}}
	row := console.Resource{Name: "CVE-1", Fields: map[string]string{"Package": "musl", "Effective severity": "High"}}
	p := arVulnerabilityPanel(row, v, pi)
	if p.Label != "View Fixed" || p.Title != "Vulnerability details" || len(p.Tables) != 1 || p.Tables[0].Title != "File location(s)" ||
		fmt.Sprint(p.Tables[0].Rows) != "[[/lib/apk/db/installed 2 RUN apk add musl sha256:5f1a alpine:3.20]]" ||
		!strings.Contains(fmt.Sprint(p.Groups), "{Description a bug") {
		t.Errorf("panel = %+v", p)
	}
	v.FixAvailable = false
	pi.FileLocation[0].LayerDetails = nil
	if p := arVulnerabilityPanel(row, v, pi); p.Label != "View" || fmt.Sprint(p.Tables[0].Rows) != "[[/lib/apk/db/installed - - - -]]" {
		t.Errorf("unfixed panel with no layer = %+v", p)
	}

	sec := console.Section{ID: "vulnerabilities", Listing: console.Listing{Items: []console.Resource{{Name: "CVE-1"}, {Name: "CVE-2"}, {Name: "CVE-3"}}}}
	bottom := &grafeaspb.LayerDetails{Index: 0, Command: "ADD rootfs.tar.gz /"}
	got := arLayerSections(sec, []*grafeaspb.LayerDetails{layer, bottom, layer})
	if len(got) != 2 || got[0].ID != "layer-0" || got[0].ShownIn != "vulnerabilities" || got[0].ShowAs != "Layer 0: ADD rootfs.tar.gz / (1 vulnerability)" ||
		got[1].ShowAs != "Layer 2: RUN apk add musl (2 vulnerabilities)" || len(got[1].Listing.Items) != 2 || got[1].Listing.Items[1].Name != "CVE-3" {
		t.Errorf("layer bar = %+v", got)
	}
	if got := arLayerSections(sec, []*grafeaspb.LayerDetails{nil, nil, nil}); got != nil {
		t.Errorf("a layer bar with no layer metadata: %+v", got)
	}
}

// An image digest's Vulnerabilities column, Scan results summary and
// Dependencies tab (#1855) are read from the occurrences scanning wrote.
func TestConsoleArtifactRegistryScanResultsAndDependencies(t *testing.T) {
	disc := &grafeaspb.Occurrence{Details: &grafeaspb.Occurrence_Discovery{Discovery: &grafeaspb.DiscoveryOccurrence{
		AnalysisStatus: grafeaspb.DiscoveryOccurrence_FINISHED_SUCCESS}}}
	vuln := func(id string, sev grafeaspb.Severity, fix bool) *grafeaspb.Occurrence {
		return &grafeaspb.Occurrence{Details: &grafeaspb.Occurrence_Vulnerability{Vulnerability: &grafeaspb.VulnerabilityOccurrence{
			ShortDescription: id, EffectiveSeverity: sev, FixAvailable: fix,
			PackageIssue: []*grafeaspb.VulnerabilityOccurrence_PackageIssue{{AffectedPackage: "openssl", PackageType: "OS"}}}}}
	}
	pkg := func(name, ver, lic string) *grafeaspb.Occurrence {
		return &grafeaspb.Occurrence{Details: &grafeaspb.Occurrence_Package{Package: &grafeaspb.PackageOccurrence{
			Name: name, PackageType: "OS", Version: &grafeaspb.Version{FullName: ver}, License: &grafeaspb.License{Expression: lic}}}}
	}
	vulns := []*grafeaspb.Occurrence{vuln("CVE-1", grafeaspb.Severity_CRITICAL, true), vuln("CVE-2", grafeaspb.Severity_HIGH, false),
		vuln("CVE-3", grafeaspb.Severity_HIGH, true)}
	all := append([]*grafeaspb.Occurrence{disc, pkg("zlib", "1.3", "Zlib"), pkg("openssl", "3.0.1", "Apache-2.0"), pkg("musl", "1.2", "MIT")}, vulns...)

	if got := arTotalsOf(all); got != "3 (1 Critical, 2 High)" {
		t.Errorf("Vulnerabilities column = %q", got)
	}
	if got := arTotalsOf(nil); got != "-" {
		t.Errorf("an unscanned digest's column = %q", got)
	}
	if got := arTotalsOf([]*grafeaspb.Occurrence{disc}); got != "None found" {
		t.Errorf("a clean digest's column = %q", got)
	}
	g := arScanResults(vulns, all)
	sum := fmt.Sprint(g.Properties)
	for _, want := range []string{"{Package types scanned OS", "{Total vulnerabilities 3", "{Fixable 2", "{Unfixable 1", "{Critical 1", "{High 2", "{Medium 0"} {
		if g.Heading != "Scan results" || !strings.Contains(sum, want) {
			t.Errorf("Scan results %q = %s; want %s", g.Heading, sum, want)
		}
	}
	dep := dependenciesOf(all)
	if dep.Label != "Dependencies" || dep.Listing.NameColumn != "Package name" || fmt.Sprint(dep.Listing.Columns) != "[Package version Package type License type]" ||
		len(dep.Listing.Items) != 3 || dep.Listing.Items[0].Name != "musl" || dep.Listing.Items[1].Fields["Package version"] != "3.0.1" ||
		dep.Listing.Items[2].Fields["License type"] != "Zlib" {
		t.Errorf("Dependencies = %+v", dep.Listing)
	}
	// Most common licenses is a bar graph, a bar per license (#2066).
	if len(dep.Groups) != 2 || dep.Groups[0].Heading != "SBOM" || dep.Groups[1].Heading != "Most common licenses" || !dep.Groups[1].Bars ||
		propertyPairs(dep.Groups[1].Properties) != "Apache-2.0=1 MIT=1 Zlib=1" {
		t.Errorf("Dependencies sections = %+v", dep.Groups)
	}
	if g := dependenciesOf([]*grafeaspb.Occurrence{disc, pkg("a", "1", "")}).Groups[1]; g.Bars || !strings.Contains(fmt.Sprint(g.Properties), "No license") {
		t.Errorf("no license found = %+v", g)
	}
	if d := dependenciesOf(nil); !strings.Contains(d.Listing.Unavailable, "not scanned") {
		t.Errorf("an unscanned digest's Dependencies = %+v", d.Listing)
	}
	// A scan that never ran is not an image without dependencies.
	failed := &grafeaspb.Occurrence{Details: &grafeaspb.Occurrence_Discovery{Discovery: &grafeaspb.DiscoveryOccurrence{
		AnalysisStatus:      grafeaspb.DiscoveryOccurrence_FINISHED_FAILED,
		AnalysisStatusError: &spb.Status{Message: "OSV-Scanner and its database are not prefetched: run `cloudburrow prefetch`"}}}}
	if d := dependenciesOf([]*grafeaspb.Occurrence{failed}); len(d.Listing.Items) != 0 || !strings.Contains(d.Listing.Unavailable, "not prefetched") {
		t.Errorf("a failed scan's Dependencies = %+v", d.Listing)
	}
	scanning := &grafeaspb.Occurrence{Details: &grafeaspb.Occurrence_Discovery{Discovery: &grafeaspb.DiscoveryOccurrence{
		AnalysisStatus: grafeaspb.DiscoveryOccurrence_SCANNING}}}
	if d := dependenciesOf([]*grafeaspb.Occurrence{scanning}); !strings.HasPrefix(d.Listing.Unavailable, "Scanning") {
		t.Errorf("a running scan's Dependencies = %+v", d.Listing)
	}
}

// propertyPairs is a group's properties as "Label=Value" pairs, so a check
// does not depend on the Property struct's other fields.
func propertyPairs(ps []console.Property) string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.Label + "=" + p.Value
	}
	return strings.Join(out, " ")
}
