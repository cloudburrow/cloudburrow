package main

// The pages of a PYTHON, NPM, MAVEN, GENERIC, APT or YUM repository
// (#1531): its packages, a package's versions and tags, and a version's
// files, read through the same Artifact Registry API calls gcloud makes,
// with Delete, Add tag and Remove tag on them.

import (
	"context"
	"fmt"
	"strings"

	arpb "cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"

	runadapter "github.com/cloudburrow/cloudburrow/internal/adapter/run"
	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/registry"
)

// packageUse is how a format's client reaches the repository on the Run
// port: {location}-{format}.pkg.dev first in the path.
func packageUse(r *arpb.Repository) string {
	base := "http://127.0.0.1:<run port>/" + r.GetRegistryUri()
	switch r.GetFormat() {
	case arpb.Repository_PYTHON:
		return "pip install --index-url " + base + "/simple/ PACKAGE; twine upload --repository-url " + base + "/ dist/*"
	case arpb.Repository_NPM:
		return "npm publish --registry " + base + "/; npm install --registry " + base + "/ PACKAGE"
	case arpb.Repository_MAVEN:
		return "a <repository> with url " + base + " for mvn deploy and dependency resolution"
	case arpb.Repository_GENERIC:
		return "gcloud artifacts generic upload, or POST /upload/v1/" + r.GetName() + "/genericArtifacts:create"
	}
	return "ImportAptArtifacts or ImportYumArtifacts from this instance's Cloud Storage"
}

func (p artifactRegistryProvider) packageRepoDetail(ctx context.Context, r *arpb.Repository, path []string) (console.Detail, error) {
	cs := runadapter.ConfigServer()
	switch len(path) {
	case 1:
		pkgs := console.Listing{Columns: []string{"Versions", "Updated"}, NameColumn: "Package", Noun: "packages", RowsOpenable: true}
		resp, err := cs.ListPackages(ctx, &arpb.ListPackagesRequest{Parent: r.GetName()})
		if err != nil {
			return console.Detail{}, err
		}
		for _, x := range resp.GetPackages() {
			vs, _ := cs.ListVersions(ctx, &arpb.ListVersionsRequest{Parent: x.GetName()})
			pkgs.Items = append(pkgs.Items, console.Resource{Name: registry.UnescapePackage(x.GetName()[strings.LastIndex(x.GetName(), "/")+1:]),
				Fields: map[string]string{"Versions": fmt.Sprint(len(vs.GetVersions())), "Updated": stampAge(x.GetUpdateTime())}})
		}
		pkgs.Total = len(pkgs.Items)
		mode := r.GetMode().String()
		if ups := r.GetVirtualRepositoryConfig().GetUpstreamPolicies(); len(ups) > 0 {
			var names []string
			for _, u := range ups {
				names = append(names, fmt.Sprintf("%s (priority %d)", u.GetRepository()[strings.LastIndex(u.GetRepository(), "/")+1:], u.GetPriority()))
			}
			mode += ": " + strings.Join(names, ", ")
		}
		sections := []console.Section{{ID: "packages", Label: "Packages", Listing: pkgs}}
		if l, label, ok := formatListing(ctx, r); ok {
			sections = append(sections, console.Section{ID: "artifacts", Label: label, Listing: l})
		}
		return console.Detail{
			Summary: []console.Property{{Label: "Format", Value: r.GetFormat().String()}, {Label: "Mode", Value: mode},
				{Label: "URL", Value: r.GetRegistryUri()}, {Label: "Use", Value: packageUse(r)},
				{Label: "Description", Value: orDash(r.GetDescription())}},
			Sections: sections,
		}, nil
	case 2:
		pkg := r.GetName() + "/packages/" + registry.EscapePackage(path[1])
		vs, err := cs.ListVersions(ctx, &arpb.ListVersionsRequest{Parent: pkg, View: arpb.VersionView_FULL})
		if err != nil {
			return console.Detail{}, err
		}
		versions := console.Listing{Columns: []string{"Tags", "Created"}, NameColumn: "Version", Noun: "versions", RowsOpenable: true}
		for _, v := range vs.GetVersions() {
			var tags []string
			for _, t := range v.GetRelatedTags() {
				tags = append(tags, registry.UnescapePackage(t.GetName()[strings.LastIndex(t.GetName(), "/")+1:]))
			}
			versions.Items = append(versions.Items, console.Resource{Name: registry.UnescapePackage(v.GetName()[strings.LastIndex(v.GetName(), "/")+1:]),
				Fields: map[string]string{"Tags": orDash(strings.Join(tags, ", ")), "Created": stampAge(v.GetCreateTime())}})
		}
		versions.Total = len(versions.Items)
		return console.Detail{
			Summary:  []console.Property{{Label: "Package", Value: path[1]}, {Label: "Versions", Value: fmt.Sprint(versions.Total)}},
			Sections: []console.Section{{ID: "versions", Label: "Versions", Listing: versions}},
		}, nil
	case 3:
		version := r.GetName() + "/packages/" + registry.EscapePackage(path[1]) + "/versions/" + registry.EscapePackage(path[2])
		if _, err := cs.GetVersion(ctx, &arpb.GetVersionRequest{Name: version}); err != nil {
			return console.Detail{}, err
		}
		fs, err := cs.ListFiles(ctx, &arpb.ListFilesRequest{Parent: r.GetName(), Filter: `owner="` + version + `"`})
		if err != nil {
			return console.Detail{}, err
		}
		files := console.Listing{Columns: []string{"Size", "Download"}, NameColumn: "File", Noun: "files"}
		for _, f := range fs.GetFiles() {
			files.Items = append(files.Items, console.Resource{Name: registry.UnescapePackage(f.GetName()[strings.LastIndex(f.GetName(), "/")+1:]),
				Fields: map[string]string{"Size": humanBytes(f.GetSizeBytes()), "Download": "/v1/" + f.GetName() + ":download?alt=media"}})
		}
		files.Total = len(files.Items)
		summary := []console.Property{{Label: "Package", Value: path[1]}, {Label: "Version", Value: path[2]}}
		if p, ok := formatSummary(ctx, r, path[1], path[2]); ok {
			summary = append(summary, p)
		}
		return console.Detail{
			Summary:  summary,
			Sections: []console.Section{{ID: "files", Label: "Files", Listing: files}},
		}, nil
	}
	return console.DeeperThan(3, path), nil
}

// formatListing is a MAVEN, NPM or PYTHON repository's format-specific
// listing, ListMavenArtifacts, ListNpmPackages or ListPythonPackages, with
// what each adds: a pom URI, dist-tags, a file URI.
func formatListing(ctx context.Context, r *arpb.Repository) (console.Listing, string, bool) {
	cs := runadapter.ConfigServer()
	l := console.Listing{NameColumn: "Artifact", Noun: "artifacts"}
	add := func(name string, fields map[string]string) {
		l.Items = append(l.Items, console.Resource{Name: name[strings.LastIndex(name, "/")+1:], Fields: fields})
	}
	label := ""
	switch r.GetFormat() {
	case arpb.Repository_MAVEN:
		label, l.Columns = "Maven artifacts", []string{"Group", "Artifact", "Version", "POM"}
		resp, err := cs.ListMavenArtifacts(ctx, &arpb.ListMavenArtifactsRequest{Parent: r.GetName()})
		if err != nil {
			return l, "", false
		}
		for _, m := range resp.GetMavenArtifacts() {
			add(m.GetName(), map[string]string{"Group": m.GetGroupId(), "Artifact": m.GetArtifactId(), "Version": m.GetVersion(), "POM": orDash(m.GetPomUri())})
		}
	case arpb.Repository_NPM:
		label, l.Columns = "npm packages", []string{"Package", "Version", "Dist-tags"}
		resp, err := cs.ListNpmPackages(ctx, &arpb.ListNpmPackagesRequest{Parent: r.GetName()})
		if err != nil {
			return l, "", false
		}
		for _, n := range resp.GetNpmPackages() {
			add(n.GetName(), map[string]string{"Package": n.GetPackageName(), "Version": n.GetVersion(), "Dist-tags": orDash(strings.Join(n.GetTags(), ", "))})
		}
	case arpb.Repository_PYTHON:
		label, l.Columns = "Python packages", []string{"Package", "Version", "URI"}
		resp, err := cs.ListPythonPackages(ctx, &arpb.ListPythonPackagesRequest{Parent: r.GetName()})
		if err != nil {
			return l, "", false
		}
		for _, p := range resp.GetPythonPackages() {
			add(p.GetName(), map[string]string{"Package": p.GetPackageName(), "Version": p.GetVersion(), "URI": orDash(p.GetUri())})
		}
	default:
		return l, "", false
	}
	l.Total = len(l.Items)
	return l, label, true
}

// formatSummary is a version page's format-specific line: GetMavenArtifact,
// GetNpmPackage or GetPythonPackage.
func formatSummary(ctx context.Context, r *arpb.Repository, pkg, version string) (console.Property, bool) {
	cs := runadapter.ConfigServer()
	id := registry.EscapePackage(pkg) + ":" + registry.EscapePackage(version)
	switch r.GetFormat() {
	case arpb.Repository_MAVEN:
		if m, err := cs.GetMavenArtifact(ctx, &arpb.GetMavenArtifactRequest{Name: r.GetName() + "/mavenArtifacts/" + id}); err == nil {
			return console.Property{Label: "POM", Value: orDash(m.GetPomUri())}, true
		}
	case arpb.Repository_NPM:
		if n, err := cs.GetNpmPackage(ctx, &arpb.GetNpmPackageRequest{Name: r.GetName() + "/npmPackages/" + id}); err == nil {
			return console.Property{Label: "Dist-tags", Value: orDash(strings.Join(n.GetTags(), ", "))}, true
		}
	case arpb.Repository_PYTHON:
		if p, err := cs.GetPythonPackage(ctx, &arpb.GetPythonPackageRequest{Name: r.GetName() + "/pythonPackages/" + id}); err == nil {
			return console.Property{Label: "URI", Value: orDash(p.GetUri())}, true
		}
	}
	return console.Property{}, false
}

// importAction is Import from Cloud Storage on an APT or YUM repository's
// page.
func importAction(r *arpb.Repository) (console.Action, bool) {
	kind := map[arpb.Repository_Format]string{arpb.Repository_APT: ".deb", arpb.Repository_YUM: ".rpm"}[r.GetFormat()]
	if kind == "" {
		return console.Action{}, false
	}
	return console.Action{ID: "import", Label: "Import from Cloud Storage", Fields: []console.Field{
		{Name: "uris", Label: "Objects", Type: "text", Required: true,
			Help: "gs://BUCKET/OBJECT of " + kind + " packages in this instance's Cloud Storage, comma separated"}}}, true
}

func importPackages(ctx context.Context, r *arpb.Repository, uris string) error {
	var list []string
	for _, u := range strings.Split(uris, ",") {
		if u = strings.TrimSpace(u); u != "" {
			list = append(list, u)
		}
	}
	cs := runadapter.ConfigServer()
	var err error
	if r.GetFormat() == arpb.Repository_APT {
		_, err = cs.ImportAptArtifacts(ctx, &arpb.ImportAptArtifactsRequest{Parent: r.GetName(),
			Source: &arpb.ImportAptArtifactsRequest_GcsSource{GcsSource: &arpb.ImportAptArtifactsGcsSource{Uris: list}}})
	} else {
		_, err = cs.ImportYumArtifacts(ctx, &arpb.ImportYumArtifactsRequest{Parent: r.GetName(),
			Source: &arpb.ImportYumArtifactsRequest_GcsSource{GcsSource: &arpb.ImportYumArtifactsGcsSource{Uris: list}}})
	}
	return err
}

func packageRepoActions(r *arpb.Repository, path []string) []console.Action {
	switch len(path) {
	case 2:
		return []console.Action{{ID: "delete", Label: "Delete package", Destructive: true, Leaves: true,
			Confirm: "Every version, file and tag of the package is deleted, as DeletePackage does."}}
	case 3:
		acts := []console.Action{
			{ID: "add-tag", Label: "Add tag", Fields: []console.Field{{Name: "tag", Label: "Tag", Type: "text", Required: true}}},
			{ID: "remove-tag", Label: "Remove tag", Fields: []console.Field{{Name: "tag", Label: "Tag", Type: "text", Required: true}}},
			{ID: "delete", Label: "Delete version", Destructive: true, Leaves: true,
				Confirm: "The version, its files and its tags are deleted, as DeleteVersion with force does."}}
		if r.GetFormat() == arpb.Repository_GENERIC {
			acts = append(acts, console.Action{ID: "delete-file", Label: "Delete file", Destructive: true, Fields: []console.Field{
				{Name: "file", Label: "File", Type: "text", Required: true, Help: "one of the Files below; a generic repository's files are deleted one by one"}}})
		}
		return acts
	}
	return nil
}

func packageRepoAct(ctx context.Context, r *arpb.Repository, path []string, action string, values map[string]string) error {
	cs := runadapter.ConfigServer()
	pkg := r.GetName() + "/packages/" + registry.EscapePackage(path[1])
	switch {
	case len(path) == 2 && action == "delete":
		_, err := cs.DeletePackage(ctx, &arpb.DeletePackageRequest{Name: pkg})
		return err
	case len(path) == 3:
		version := pkg + "/versions/" + registry.EscapePackage(path[2])
		tag := pkg + "/tags/" + registry.EscapePackage(strings.TrimSpace(values["tag"]))
		switch action {
		case "delete-file":
			_, err := cs.DeleteFile(ctx, &arpb.DeleteFileRequest{Name: r.GetName() + "/files/" + registry.EscapePackage(strings.TrimSpace(values["file"]))})
			return err
		case "delete":
			_, err := cs.DeleteVersion(ctx, &arpb.DeleteVersionRequest{Name: version, Force: true})
			return err
		case "add-tag":
			if _, err := cs.GetTag(ctx, &arpb.GetTagRequest{Name: tag}); err == nil {
				_, err = cs.UpdateTag(ctx, &arpb.UpdateTagRequest{Tag: &arpb.Tag{Name: tag, Version: version}})
				return err
			}
			_, err := cs.CreateTag(ctx, &arpb.CreateTagRequest{Parent: pkg, TagId: strings.TrimSpace(values["tag"]), Tag: &arpb.Tag{Version: version}})
			return err
		case "remove-tag":
			t, err := cs.GetTag(ctx, &arpb.GetTagRequest{Name: tag})
			if err != nil || t.GetVersion() != version {
				return fmt.Errorf("%q is not a tag of this version", values["tag"])
			}
			_, err = cs.DeleteTag(ctx, &arpb.DeleteTagRequest{Name: tag})
			return err
		}
	}
	return fmt.Errorf("unknown action %q", action)
}
