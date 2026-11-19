package osvscan

import (
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"

	pb "google.golang.org/genproto/googleapis/grafeas/v1"
)

// Findings is one scan as Grafeas notes and occurrences. The occurrences
// carry no name or resource URI: the caller files them under the image's
// project and resource.
type Findings struct {
	// Notes are the provider notes the occurrences name: goog-vulnz's
	// vulnerability notes and goog-analysis's package notes.
	Notes []*pb.Note
	// Packages are the PACKAGE occurrences, one per package found.
	Packages []*pb.Occurrence
	// Vulnerabilities are the VULNERABILITY occurrences, one per
	// vulnerability and package.
	Vulnerabilities []*pb.Occurrence
	// AnalysisTypes are the package types the scan read ("OS", "GO",
	// "MAVEN", ...), for the discovery occurrence's analysisCompleted.
	AnalysisTypes []string
	// CPE is the image's operating system's CPE, when it has one.
	CPE string
}

// DiscoveryNote is the note every scan's DISCOVERY occurrence names, as
// Artifact Analysis's do.
const DiscoveryNote = "projects/goog-analysis/notes/PACKAGE_VULNERABILITY"

// PackageNote is the goog-analysis note a PACKAGE occurrence of a package
// type names.
func PackageNote(packageType string) string {
	return "projects/goog-analysis/notes/PACKAGE_" + packageType
}

// packageType is Artifact Analysis's package type of an OSV ecosystem.
func packageType(ecosystem string) (string, bool) {
	base := ecosystem
	if i := strings.Index(base, ":"); i >= 0 {
		base = base[:i]
	}
	switch base {
	case "Alpine", "Debian", "Ubuntu", "AlmaLinux", "Rocky Linux", "Red Hat", "SUSE", "openSUSE", "Wolfi", "Chainguard", "MinimOS":
		return "OS", true
	case "Go":
		return "GO", false
	case "Maven":
		return "MAVEN", false
	case "npm":
		return "NPM", false
	case "PyPI":
		return "PYPI", false
	case "RubyGems":
		return "RUBYGEMS", false
	case "NuGet":
		return "NUGET", false
	case "Packagist":
		return "COMPOSER", false
	case "crates.io":
		return "CARGO", false
	}
	return strings.ToUpper(base), false
}

var ecoVersion = regexp.MustCompile(`^v?([0-9][0-9.]*)`)

// osCPE is the operating system CPE Artifact Analysis names for an OS
// package's ecosystem, such as Alpine:v3.13 or Debian:12.
func osCPE(ecosystem string) string {
	i := strings.Index(ecosystem, ":")
	if i < 0 {
		return ""
	}
	v := ecoVersion.FindStringSubmatch(ecosystem[i+1:])
	if v == nil {
		return ""
	}
	ver := v[1]
	switch ecosystem[:i] {
	case "Alpine":
		return "cpe:/o:alpine:alpine_linux:" + ver
	case "Debian":
		return "cpe:/o:debian:debian_linux:" + ver
	case "Ubuntu":
		return "cpe:/o:canonical:ubuntu_linux:" + ver
	}
	return ""
}

// version splits an OS package's version into Grafeas's epoch, name and
// revision (1:2.3-4 is epoch 1, name 2.3, revision 4).
func version(full string, os bool) *pb.Version {
	v := &pb.Version{Kind: pb.Version_NORMAL, FullName: full, Name: full}
	if !os {
		return v
	}
	rest := full
	if i := strings.Index(rest, ":"); i > 0 {
		if e, err := strconv.Atoi(rest[:i]); err == nil {
			v.Epoch = int32(e)
			rest = rest[i+1:]
		}
	}
	v.Name = rest
	if i := strings.LastIndex(rest, "-"); i > 0 {
		v.Name, v.Revision = rest[:i], rest[i+1:]
	}
	return v
}

// NoteID is the vulnerability's note ID: its CVE when it has one, as
// Artifact Analysis names goog-vulnz's notes, else its own ID (GHSA-...,
// GO-..., PYSEC-...).
func NoteID(v Vulnerability) string {
	for _, id := range append(append([]string{v.ID}, v.Upstream...), v.Aliases...) {
		if strings.HasPrefix(id, "CVE-") {
			return id
		}
	}
	return v.ID
}

// Convert turns a scan's result into notes and occurrences.
func Convert(r *Result) Findings {
	var f Findings
	types := map[string]bool{}
	notes := map[string]*pb.Note{}
	for _, res := range r.Results {
		for _, p := range res.Packages {
			if c := osCPE(p.Package.Ecosystem); c != "" && f.CPE == "" {
				f.CPE = c
			}
		}
	}
	for _, res := range r.Results {
		for _, p := range res.Packages {
			pt, isOS := packageType(p.Package.Ecosystem)
			types[pt] = true
			cpe := f.CPE
			if isOS {
				cpe = osCPE(p.Package.Ecosystem)
			}
			ver := version(p.Package.Version, isOS)
			pn := PackageNote(pt)
			if notes[pn] == nil {
				notes[pn] = &pb.Note{Name: pn, ShortDescription: pt + " packages",
					LongDescription: "Packages of type " + pt + " found by Artifact Analysis's container scanning.",
					Type:            &pb.Note_Package{Package: &pb.PackageNote{Name: pt, PackageType: pt}}}
			}
			f.Packages = append(f.Packages, &pb.Occurrence{NoteName: pn, Details: &pb.Occurrence_Package{Package: &pb.PackageOccurrence{
				Name: p.Package.Name, PackageType: pt, CpeUri: cpe, Version: ver,
				Location: []*pb.Location{{CpeUri: cpe, Version: ver, Path: res.Source.Path}},
			}}})
			score := map[string]float64{}
			for _, g := range p.Groups {
				s, err := strconv.ParseFloat(g.MaxSeverity, 64)
				if err != nil {
					continue
				}
				for _, id := range append(g.IDs, g.Aliases...) {
					score[id] = s
				}
			}
			seen := map[string]bool{}
			for _, v := range p.Vulnerabilities {
				id := NoteID(v)
				if seen[id] {
					continue
				}
				seen[id] = true
				occ, note := vulnerability(v, p, pt, isOS, cpe, ver, res.Source.Path, score)
				f.Vulnerabilities = append(f.Vulnerabilities, occ)
				if notes[note.Name] == nil {
					notes[note.Name] = note
				}
			}
		}
	}
	for t := range types {
		f.AnalysisTypes = append(f.AnalysisTypes, t)
	}
	sort.Strings(f.AnalysisTypes)
	for _, n := range sortedNotes(notes) {
		f.Notes = append(f.Notes, n)
	}
	return f
}

func sortedNotes(m map[string]*pb.Note) []*pb.Note {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]*pb.Note, len(keys))
	for i, k := range keys {
		out[i] = m[k]
	}
	return out
}

func vulnerability(v Vulnerability, p Package, pt string, isOS bool, cpe string, ver *pb.Version, path string, groupScore map[string]float64) (*pb.Occurrence, *pb.Note) {
	id := NoteID(v)
	var vector string
	for _, s := range v.Severity {
		if s.Type == "CVSS_V3" && vector == "" {
			vector = s.Score
		}
	}
	cvss := parseCVSS3(vector)
	score := groupScore[v.ID]
	if score == 0 && cvss != nil {
		score = float64(cvss.BaseScore)
	}
	if cvss != nil {
		cvss.BaseScore = float32(score)
	}
	sev := severityOf(score, vector != "")
	eff := sev
	fixed := &pb.Version{Kind: pb.Version_MAXIMUM}
	fixAvailable := false
	for _, a := range v.Affected {
		if a.Package.Name != p.Package.Name && a.Package.Name != p.Package.OSPackageName {
			continue
		}
		if a.Package.Ecosystem != p.Package.Ecosystem && !strings.HasPrefix(p.Package.Ecosystem, a.Package.Ecosystem) {
			continue
		}
		for _, s := range a.Severity {
			if e, ok := distroSeverity(s.Type, s.Score); ok {
				eff = e
			}
		}
		if u, ok := a.EcosystemSpecific["urgency"].(string); ok {
			if e, ok := distroSeverity("Debian", u); ok {
				eff = e
			}
		}
		for _, r := range a.Ranges {
			for _, e := range r.Events {
				if e.Fixed != "" && !fixAvailable {
					fixed = version(e.Fixed, isOS)
					fixAvailable = true
				}
			}
		}
	}
	for _, s := range v.Severity {
		if e, ok := distroSeverity(s.Type, s.Score); ok {
			eff = e
		}
	}
	desc := v.Details
	if desc == "" {
		desc = v.Summary
	}
	var urls []*pb.RelatedUrl
	for _, r := range v.References {
		urls = append(urls, &pb.RelatedUrl{Url: r.URL, Label: "More Info"})
	}
	if len(urls) == 0 {
		urls = append(urls, &pb.RelatedUrl{Url: "https://osv.dev/vulnerability/" + v.ID, Label: "More Info"})
	}
	occ := &pb.Occurrence{NoteName: "projects/goog-vulnz/notes/" + id, Details: &pb.Occurrence_Vulnerability{Vulnerability: &pb.VulnerabilityOccurrence{
		Type: pt, Severity: sev, CvssScore: float32(score), Cvssv3: cvss, EffectiveSeverity: eff, FixAvailable: fixAvailable,
		CvssVersion: cvssVersion(cvss), ShortDescription: id, LongDescription: desc, RelatedUrls: urls,
		PackageIssue: []*pb.VulnerabilityOccurrence_PackageIssue{{
			AffectedCpeUri: cpe, AffectedPackage: p.Package.Name, AffectedVersion: ver,
			FixedCpeUri: cpe, FixedPackage: p.Package.Name, FixedVersion: fixed, FixAvailable: fixAvailable,
			PackageType: pt, EffectiveSeverity: eff,
			FileLocation: []*pb.FileLocation{{FilePath: path}},
		}},
	}}}
	note := &pb.Note{Name: "projects/goog-vulnz/notes/" + id, ShortDescription: id, LongDescription: desc, RelatedUrl: urls,
		Type: &pb.Note_Vulnerability{Vulnerability: &pb.VulnerabilityNote{
			CvssScore: float32(score), Severity: sev, CvssVersion: cvssVersion(cvss),
			Details: []*pb.VulnerabilityNote_Detail{{
				SeverityName: strings.ToUpper(eff.String()), Description: desc, PackageType: pt,
				AffectedCpeUri: cpe, AffectedPackage: p.Package.Name, FixedCpeUri: cpe, FixedPackage: p.Package.Name,
				FixedVersion: fixed, Source: v.ID, Vendor: "OSV",
			}},
		}}}
	return occ, note
}

func cvssVersion(c *pb.CVSS) pb.CVSSVersion {
	if c == nil {
		return pb.CVSSVersion_CVSS_VERSION_UNSPECIFIED
	}
	return pb.CVSSVersion_CVSS_VERSION_3
}

// severityOf is the CVSS v3 qualitative rating of a base score
// (https://www.first.org/cvss/v3.1/specification-document, 5).
func severityOf(score float64, scored bool) pb.Severity {
	switch {
	case !scored && score == 0:
		return pb.Severity_SEVERITY_UNSPECIFIED
	case score >= 9:
		return pb.Severity_CRITICAL
	case score >= 7:
		return pb.Severity_HIGH
	case score >= 4:
		return pb.Severity_MEDIUM
	case score > 0:
		return pb.Severity_LOW
	}
	return pb.Severity_MINIMAL
}

// distroSeverity is a distribution's own rating (Ubuntu's priority,
// Debian's urgency), which Artifact Analysis reports as the effective
// severity.
func distroSeverity(kind, s string) (pb.Severity, bool) {
	if kind != "Ubuntu" && kind != "Debian" {
		return 0, false
	}
	switch strings.ToLower(strings.TrimSuffix(strings.TrimSpace(s), "*")) {
	case "negligible", "unimportant":
		return pb.Severity_MINIMAL, true
	case "low":
		return pb.Severity_LOW, true
	case "medium":
		return pb.Severity_MEDIUM, true
	case "high":
		return pb.Severity_HIGH, true
	case "critical":
		return pb.Severity_CRITICAL, true
	}
	return 0, false
}

// parseCVSS3 reads a CVSS v3.x vector into Grafeas's CVSS, with its base,
// exploitability and impact scores (specification 7.1).
func parseCVSS3(vector string) *pb.CVSS {
	if !strings.HasPrefix(vector, "CVSS:3.") {
		return nil
	}
	m := map[string]string{}
	for _, part := range strings.Split(vector, "/")[1:] {
		kv := strings.SplitN(part, ":", 2)
		if len(kv) == 2 {
			m[kv[0]] = kv[1]
		}
	}
	c := &pb.CVSS{}
	var av, ac, pr, ui float64
	switch m["AV"] {
	case "N":
		c.AttackVector, av = pb.CVSS_ATTACK_VECTOR_NETWORK, 0.85
	case "A":
		c.AttackVector, av = pb.CVSS_ATTACK_VECTOR_ADJACENT, 0.62
	case "L":
		c.AttackVector, av = pb.CVSS_ATTACK_VECTOR_LOCAL, 0.55
	case "P":
		c.AttackVector, av = pb.CVSS_ATTACK_VECTOR_PHYSICAL, 0.2
	default:
		return nil
	}
	switch m["AC"] {
	case "L":
		c.AttackComplexity, ac = pb.CVSS_ATTACK_COMPLEXITY_LOW, 0.77
	case "H":
		c.AttackComplexity, ac = pb.CVSS_ATTACK_COMPLEXITY_HIGH, 0.44
	default:
		return nil
	}
	changed := m["S"] == "C"
	if changed {
		c.Scope = pb.CVSS_SCOPE_CHANGED
	} else {
		c.Scope = pb.CVSS_SCOPE_UNCHANGED
	}
	switch m["PR"] {
	case "N":
		c.PrivilegesRequired, pr = pb.CVSS_PRIVILEGES_REQUIRED_NONE, 0.85
	case "L":
		c.PrivilegesRequired, pr = pb.CVSS_PRIVILEGES_REQUIRED_LOW, 0.62
		if changed {
			pr = 0.68
		}
	case "H":
		c.PrivilegesRequired, pr = pb.CVSS_PRIVILEGES_REQUIRED_HIGH, 0.27
		if changed {
			pr = 0.5
		}
	default:
		return nil
	}
	switch m["UI"] {
	case "N":
		c.UserInteraction, ui = pb.CVSS_USER_INTERACTION_NONE, 0.85
	case "R":
		c.UserInteraction, ui = pb.CVSS_USER_INTERACTION_REQUIRED, 0.62
	default:
		return nil
	}
	impact := func(s string, set *pb.CVSS_Impact) float64 {
		switch s {
		case "H":
			*set = pb.CVSS_IMPACT_HIGH
			return 0.56
		case "L":
			*set = pb.CVSS_IMPACT_LOW
			return 0.22
		}
		*set = pb.CVSS_IMPACT_NONE
		return 0
	}
	ci := impact(m["C"], &c.ConfidentialityImpact)
	ii := impact(m["I"], &c.IntegrityImpact)
	ai := impact(m["A"], &c.AvailabilityImpact)
	iss := 1 - (1-ci)*(1-ii)*(1-ai)
	var imp float64
	if changed {
		imp = 7.52*(iss-0.029) - 3.25*math.Pow(iss-0.02, 15)
	} else {
		imp = 6.42 * iss
	}
	exp := 8.22 * av * ac * pr * ui
	base := 0.0
	if imp > 0 {
		if changed {
			base = roundUp(math.Min(1.08*(imp+exp), 10))
		} else {
			base = roundUp(math.Min(imp+exp, 10))
		}
	}
	c.BaseScore = float32(base)
	c.ExploitabilityScore = float32(math.Round(exp*10) / 10)
	c.ImpactScore = float32(math.Round(math.Max(imp, 0)*10) / 10)
	return c
}

// roundUp is the specification's Roundup (Appendix A).
func roundUp(x float64) float64 {
	i := int64(math.Round(x * 100000))
	if i%10000 == 0 {
		return float64(i) / 100000
	}
	return (math.Floor(float64(i)/10000) + 1) / 10
}
