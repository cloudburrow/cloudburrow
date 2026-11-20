package main

import (
	"fmt"
	"strings"
	"testing"

	grafeaspb "google.golang.org/genproto/googleapis/grafeas/v1"
)

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
	if s := fmt.Sprint(dep.Groups); !strings.Contains(s, "SBOM") || !strings.Contains(s, "Most common licenses Apache-2.0 (1), MIT (1), Zlib (1)") {
		t.Errorf("Dependencies sections = %s", s)
	}
	if d := dependenciesOf(nil); !strings.Contains(d.Listing.Unavailable, "not scanned") {
		t.Errorf("an unscanned digest's Dependencies = %+v", d.Listing)
	}
}
