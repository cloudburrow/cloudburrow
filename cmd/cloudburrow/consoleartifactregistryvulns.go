package main

// An image's versions and an image digest's page as Google's console draws
// them (#1855): the versions list's Vulnerabilities totals, More actions >
// Edit tags and Delete over the checked versions
// (https://docs.cloud.google.com/artifact-registry/docs/docker/manage-images),
// the Vulnerabilities tab's Scan results summary
// (https://docs.cloud.google.com/artifact-analysis/docs/os-scanning-automatically)
// and the Dependencies tab
// (https://docs.cloud.google.com/artifact-analysis/docs/view-licenses-dependencies).

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	grafeaspb "google.golang.org/genproto/googleapis/grafeas/v1"

	arpb "cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"

	runadapter "github.com/cloudburrow/cloudburrow/internal/adapter/run"
	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/registry"
)

// arVersionsField is the field Delete's checked versions are sent in.
const arVersionsField = "versions"

// arSeverities are Artifact Analysis's effective severities, most severe
// first, with the words Google's console shows.
var arSeverities = []struct {
	sev  grafeaspb.Severity
	word string
}{{grafeaspb.Severity_CRITICAL, "Critical"}, {grafeaspb.Severity_HIGH, "High"}, {grafeaspb.Severity_MEDIUM, "Medium"},
	{grafeaspb.Severity_LOW, "Low"}, {grafeaspb.Severity_MINIMAL, "Minimal"}}

// arSeverityWord is an effective severity as the console words it.
func arSeverityWord(s grafeaspb.Severity) string {
	for _, x := range arSeverities {
		if x.sev == s {
			return x.word
		}
	}
	return "Unspecified"
}

// arEditTagsAction is a version's More actions > Edit tags: "Type new tags
// into the field and then click SAVE"; a tag left out is deleted, and a tag
// another version holds moves here.
func arEditTagsAction(tags []string) console.Action {
	return console.Action{ID: "edit-tags", Label: "Edit tags", Submit: "Save", Fields: []console.Field{
		{Name: "tags", Label: "Tags", Type: "text", Default: strings.Join(tags, ", "),
			Help: "Comma separated. A tag removed here is deleted; a tag of another version moves to this one."}}}
}

// arEditTags makes the version's tags exactly the ones given.
func arEditTags(st *registry.Store, repo registry.Repo, image, digest, given string) error {
	want := map[string]bool{}
	for _, t := range strings.Split(given, ",") {
		if t = strings.TrimSpace(t); t == "" {
			continue
		}
		if !registry.ValidTag(t) {
			return fmt.Errorf("%q is not a valid tag", t)
		}
		want[t] = true
	}
	m, err := st.Manifest(repo, image, digest)
	if err != nil {
		return err
	}
	for _, t := range m.Tags {
		if !want[t] {
			if err := st.DeleteTag(repo, image, t); err != nil {
				return err
			}
		}
	}
	held := map[string]bool{}
	for _, t := range m.Tags {
		held[t] = true
	}
	names := make([]string, 0, len(want))
	for t := range want {
		names = append(names, t)
	}
	sort.Strings(names)
	for _, t := range names {
		if held[t] {
			continue
		}
		if err := st.SetTag(repo, image, t, digest); err != nil {
			return err
		}
	}
	return nil
}

// arDeleteVersionsAction is Delete over the versions checked in an
// image's list: "Select versions that you want to delete. Click DELETE.
// In the confirmation dialog, click DELETE."
func arDeleteVersionsAction() console.Action {
	return console.Action{ID: "delete-versions", Label: "Delete", Destructive: true, ConfirmStyle: console.DeleteConfirmClick,
		SelectionField: arVersionsField, Fields: []console.Field{{Name: arVersionsField, Type: "hidden"}},
		Confirm: "The checked versions and their tags are deleted. This cannot be undone."}
}

// arVulnerabilityTotals is a version's Vulnerabilities column: its total
// and the count at each severity, or what the scan is doing.
func arVulnerabilityTotals(repo registry.Repo, image, digest string) string {
	return arTotalsOf(runadapter.ImageOccurrences(repo, image, digest))
}

// arTotalsOf is the Vulnerabilities column for an image's occurrences.
func arTotalsOf(occs []*grafeaspb.Occurrence) string {
	var disc *grafeaspb.DiscoveryOccurrence
	counts := map[grafeaspb.Severity]int{}
	total := 0
	for _, o := range occs {
		switch {
		case o.GetDiscovery() != nil:
			disc = o.GetDiscovery()
		case o.GetVulnerability() != nil:
			counts[o.GetVulnerability().GetEffectiveSeverity()]++
			total++
		}
	}
	if disc == nil {
		return "-"
	}
	switch disc.GetAnalysisStatus() {
	case grafeaspb.DiscoveryOccurrence_PENDING, grafeaspb.DiscoveryOccurrence_SCANNING:
		return "Scanning"
	case grafeaspb.DiscoveryOccurrence_FINISHED_FAILED, grafeaspb.DiscoveryOccurrence_FINISHED_UNSUPPORTED:
		return "Not scanned"
	}
	if total == 0 {
		return "None found"
	}
	var parts []string
	for _, x := range arSeverities {
		if n := counts[x.sev]; n > 0 {
			parts = append(parts, strconv.Itoa(n)+" "+x.word)
		}
	}
	if n := counts[grafeaspb.Severity_SEVERITY_UNSPECIFIED]; n > 0 {
		parts = append(parts, strconv.Itoa(n)+" Unspecified")
	}
	return strconv.Itoa(total) + " (" + strings.Join(parts, ", ") + ")"
}

// arScanResults is the Vulnerabilities tab's Scan results section: "a
// summary of the package types scanned", the totals, how many are fixable
// and unfixable, and the count at each effective severity.
func arScanResults(vulns, all []*grafeaspb.Occurrence) console.PropertyGroup {
	types := map[string]bool{}
	for _, o := range all {
		if pk := o.GetPackage(); pk != nil && pk.GetPackageType() != "" {
			types[pk.GetPackageType()] = true
		}
	}
	for _, o := range vulns {
		for _, pi := range o.GetVulnerability().GetPackageIssue() {
			if pi.GetPackageType() != "" {
				types[pi.GetPackageType()] = true
			}
		}
	}
	names := make([]string, 0, len(types))
	for t := range types {
		names = append(names, t)
	}
	sort.Strings(names)
	fixable, counts := 0, map[grafeaspb.Severity]int{}
	for _, o := range vulns {
		if o.GetVulnerability().GetFixAvailable() {
			fixable++
		}
		counts[o.GetVulnerability().GetEffectiveSeverity()]++
	}
	props := []console.Property{{Label: "Package types scanned", Value: orDash(strings.Join(names, ", "))},
		{Label: "Total vulnerabilities", Value: strconv.Itoa(len(vulns))},
		{Label: "Fixable", Value: strconv.Itoa(fixable)}, {Label: "Unfixable", Value: strconv.Itoa(len(vulns) - fixable)}}
	for _, x := range arSeverities {
		props = append(props, console.Property{Label: x.word, Value: strconv.Itoa(counts[x.sev])})
	}
	return console.PropertyGroup{Heading: "Scan results", Properties: props}
}

// dependenciesSection is an image digest's Dependencies tab: the SBOM
// section, the Licenses section (Most common licenses) and the list of
// dependencies with Package name, Package version, Package type and
// License type, from the PACKAGE occurrences scanning wrote.
func dependenciesSection(repo registry.Repo, image, digest string) console.Section {
	sec := dependenciesOf(runadapter.ImageOccurrences(repo, image, digest))
	if sec.Listing.Unavailable != "" {
		cfg := runadapter.RepositoryScanning(repo)
		if cfg.GetEnablementState() != arpb.Repository_VulnerabilityScanningConfig_SCANNING_ACTIVE {
			sec.Listing.Unavailable = "Dependencies are found by vulnerability scanning, which is not active for this repository: " + cfg.GetEnablementStateReason()
		}
	}
	return sec
}

// dependenciesOf is the Dependencies tab for an image's occurrences.
func dependenciesOf(occs []*grafeaspb.Occurrence) console.Section {
	sec := console.Section{ID: "dependencies", Label: "Dependencies", Listing: console.Listing{
		Columns: []string{"Package version", "Package type", "License type"}, NameColumn: "Package name", Noun: "dependencies"}}
	var disc *grafeaspb.DiscoveryOccurrence
	var sbom []string
	licenses := map[string]int{}
	for _, o := range occs {
		switch {
		case o.GetDiscovery() != nil:
			disc = o.GetDiscovery()
		case o.GetSbomReference() != nil:
			sbom = append(sbom, o.GetSbomReference().GetPayload().GetPredicate().GetLocation())
		case o.GetPackage() != nil:
			pk := o.GetPackage()
			lic := pk.GetLicense().GetExpression()
			if lic != "" {
				licenses[lic]++
			}
			sec.Listing.Items = append(sec.Listing.Items, console.Resource{Name: pk.GetName(), Fields: map[string]string{
				"Package version": orDash(pk.GetVersion().GetFullName()), "Package type": orDash(pk.GetPackageType()),
				"License type": orDash(lic)}})
		}
	}
	if disc == nil {
		sec.Listing.Items = nil
		sec.Listing.Unavailable = "This image was not scanned: it was pushed before scanning was active. Push it again under a new digest to scan it."
		return sec
	}
	sort.SliceStable(sec.Listing.Items, func(i, j int) bool { return sec.Listing.Items[i].Name < sec.Listing.Items[j].Name })
	sec.Listing.Total = len(sec.Listing.Items)
	sbomText := "No SBOM: SBOMs are not created automatically, and none is stored for this image."
	if len(sbom) > 0 {
		sbomText = strings.Join(sbom, ", ")
	}
	// Most common licenses, as Google's bar graph (#2066): the five
	// licenses most packages carry, each a bar of its count.
	licGroup := console.PropertyGroup{Heading: "Licenses", Properties: []console.Property{
		{Label: "Most common licenses", Value: "No license was detected in this image's packages."}}}
	if len(licenses) > 0 {
		keys := make([]string, 0, len(licenses))
		for k := range licenses {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool {
			if licenses[keys[i]] != licenses[keys[j]] {
				return licenses[keys[i]] > licenses[keys[j]]
			}
			return keys[i] < keys[j]
		})
		if len(keys) > 5 {
			keys = keys[:5]
		}
		licGroup = console.PropertyGroup{Heading: "Most common licenses", Bars: true}
		for _, k := range keys {
			licGroup.Properties = append(licGroup.Properties, console.Property{Label: k, Value: strconv.Itoa(licenses[k])})
		}
	}
	sec.Groups = []console.PropertyGroup{{Heading: "SBOM", Properties: []console.Property{{Label: "SBOM", Value: sbomText}}}, licGroup}
	return sec
}

// arVulnerabilityPanel is a Vulnerabilities row's Vulnerability details
// (#2066): "Click VIEW or VIEW FIX beside a vulnerability to open a side
// panel" with the file locations where the vulnerable package was found,
// each with its layer's index, build command, digest and base image (the
// table's question mark on Google's page). The link reads View Fixed
// when a fix is available, View when none is.
func arVulnerabilityPanel(row console.Resource, v *grafeaspb.VulnerabilityOccurrence, pi *grafeaspb.VulnerabilityOccurrence_PackageIssue) *console.SidePanel {
	label := "View"
	if v.GetFixAvailable() {
		label = "View Fixed"
	}
	props := []console.Property{{Label: "Name", Value: row.Name}}
	for _, c := range []string{"Effective severity", "CVSS", "Fix available", "Package", "Package type", "Installed version", "Fixed version"} {
		props = append(props, console.Property{Label: c, Value: row.Fields[c]})
	}
	if d := v.GetLongDescription(); d != "" {
		props = append(props, console.Property{Label: "Description", Value: d})
	}
	for _, u := range v.GetRelatedUrls() {
		if u.GetUrl() != "" {
			props = append(props, console.Property{Label: "More info", Value: u.GetUrl()})
			break
		}
	}
	locs := console.PanelTable{Title: "File location(s)", Columns: []string{"File path", "Layer", "Layer build command", "Layer digest", "Base image"}}
	for _, l := range pi.GetFileLocation() {
		if l.GetFilePath() == "" {
			continue
		}
		layer, cmd, diff, base := "-", "-", "-", "-"
		if d := l.GetLayerDetails(); d != nil {
			layer, cmd, diff = strconv.Itoa(int(d.GetIndex())), orDash(d.GetCommand()), orDash(d.GetDiffId())
			var names []string
			for _, b := range d.GetBaseImages() {
				names = append(names, b.GetName())
			}
			base = orDash(strings.Join(names, ", "))
		}
		locs.Rows = append(locs.Rows, []string{l.GetFilePath(), layer, cmd, diff, base})
	}
	return &console.SidePanel{Label: label, Title: "Vulnerability details",
		Groups: []console.PropertyGroup{{Properties: props}}, Tables: []console.PanelTable{locs}}
}

// arLayerSections are the Vulnerabilities tab's Layer bar (Preview,
// #2066): "a drop-down list of numbered layers", layer 0 the bottom one,
// listing only layers with vulnerabilities, each with its build command
// and its count; choosing one shows that layer's vulnerabilities. "The
// Layer bar appears only when layer-based metadata is available": none
// when the scan named no layer.
func arLayerSections(sec console.Section, layers []*grafeaspb.LayerDetails) []console.Section {
	byIndex := map[int32][]console.Resource{}
	details := map[int32]*grafeaspb.LayerDetails{}
	for i, l := range layers {
		if l == nil || i >= len(sec.Listing.Items) {
			continue
		}
		byIndex[l.GetIndex()] = append(byIndex[l.GetIndex()], sec.Listing.Items[i])
		details[l.GetIndex()] = l
	}
	if len(byIndex) == 0 {
		return nil
	}
	idx := make([]int32, 0, len(byIndex))
	for i := range byIndex {
		idx = append(idx, i)
	}
	sort.Slice(idx, func(a, b int) bool { return idx[a] < idx[b] })
	out := make([]console.Section, 0, len(idx))
	for _, i := range idx {
		rows := byIndex[i]
		cmd := details[i].GetCommand()
		if len(cmd) > 80 {
			cmd = cmd[:77] + "..."
		}
		if cmd == "" {
			cmd = "no build command recorded"
		}
		n := "vulnerabilities"
		if len(rows) == 1 {
			n = "vulnerability"
		}
		l := sec.Listing
		l.Items, l.Total = rows, len(rows)
		out = append(out, console.Section{ID: "layer-" + strconv.Itoa(int(i)), Label: "Layer " + strconv.Itoa(int(i)), ShownIn: sec.ID,
			ShowAs:  fmt.Sprintf("Layer %d: %s (%d %s)", i, cmd, len(rows), n),
			Listing: l})
	}
	return out
}

// arImageCreated is a version's Created: when the image was built, its
// config's created time, as Google's versions list shows it beside
// Uploaded (#2066). An index is its first platform image's; "-" when the
// image records none.
func arImageCreated(st *registry.Store, repo registry.Repo, image string, m registry.ManifestInfo) string {
	doc := m.Doc
	for depth := 0; doc != nil && depth < 2; depth++ {
		if doc.Config != nil {
			b, err := st.ReadRepoBlob(repo, doc.Config.Digest)
			if err != nil {
				return "-"
			}
			var cfg struct {
				Created *time.Time `json:"created"`
			}
			if json.Unmarshal(b, &cfg) != nil || cfg.Created == nil || cfg.Created.IsZero() {
				return "-"
			}
			return arTimeOf(*cfg.Created)
		}
		if len(doc.Manifests) == 0 {
			break
		}
		child, err := st.Manifest(repo, image, doc.Manifests[0].Digest)
		if err != nil {
			break
		}
		doc = child.Doc
	}
	return "-"
}
