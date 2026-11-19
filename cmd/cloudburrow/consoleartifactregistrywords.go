package main

// The words Google's Artifact Registry pages show a repository's fields
// in (#1855), and its Setup instructions, pointed at this instance's
// registry on the Run port.

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"

	arpb "cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	runadapter "github.com/cloudburrow/cloudburrow/internal/adapter/run"
	"github.com/cloudburrow/cloudburrow/internal/registry"
)

func arFormatWord(f arpb.Repository_Format) string {
	if w := arFormatLabels[f.String()]; w != "" {
		return w
	}
	return f.String()
}

func arModeWord(m arpb.Repository_Mode) string {
	if w := arModeLabels[m.String()]; w != "" {
		return w
	}
	return "Standard"
}

// arLabelsText is key:value pairs, sorted, as Google's Labels column.
func arLabelsText(m map[string]string) string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+":"+v)
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}

func arEncryptionWord(r *arpb.Repository) string {
	if r.GetKmsKeyName() != "" {
		return "Customer-managed key"
	}
	return "Google-managed key"
}

// arCleanupWord is the Cleanup policies column: none, or how many and
// whether they run as a dry run.
func arCleanupWord(r *arpb.Repository) string {
	n := len(r.GetCleanupPolicies())
	switch {
	case n == 0:
		return orDash("")
	case r.GetCleanupPolicyDryRun():
		return fmt.Sprintf("%d (dry run)", n)
	}
	return fmt.Sprintf("%d (delete artifacts)", n)
}

// arTime is a time as the pages show one, local and to the minute.
func arTime(ts *timestamppb.Timestamp) string {
	if ts == nil || !ts.IsValid() || ts.AsTime().IsZero() {
		return orDash("")
	}
	return ts.AsTime().Local().Format("2006-01-02 15:04")
}

// arRepositorySize is what a repository holds, in bytes: a Docker
// repository's manifests' image sizes, any other's files.
func arRepositorySize(ctx context.Context, r *arpb.Repository) int64 {
	var total int64
	if r.GetFormat() == arpb.Repository_DOCKER {
		st := runadapter.RegistryStore()
		if st == nil {
			return 0
		}
		parts := strings.Split(r.GetName(), "/")
		repo := registry.Repo{Location: parts[3], Project: parts[1], Repository: parts[5]}
		for _, im := range st.Images(repo) {
			ms, _ := st.Manifests(repo, im)
			for _, m := range ms {
				total += m.ImageSize
			}
		}
		return total
	}
	token := ""
	for {
		fs, err := runadapter.ConfigServer().ListFiles(ctx, &arpb.ListFilesRequest{Parent: r.GetName(), PageToken: token})
		if err != nil {
			return total
		}
		for _, f := range fs.GetFiles() {
			total += f.GetSizeBytes()
		}
		if token = fs.GetNextPageToken(); token == "" {
			return total
		}
	}
}

// runPort is the Run port's number from its address, or a placeholder.
func (p artifactRegistryProvider) runPort() string {
	if p.runAddr != nil {
		if _, port, err := net.SplitHostPort(p.runAddr()); err == nil && port != "" {
			return port
		}
	}
	return "RUN_PORT"
}

// setupInstructions are the repository's Setup instructions: Google's,
// with this instance's registry address in place of pkg.dev.
func (p artifactRegistryProvider) setupInstructions(r *arpb.Repository) string {
	parts := strings.Split(r.GetName(), "/")
	project, loc, id := parts[1], parts[3], parts[5]
	port := p.runPort()
	base := "http://127.0.0.1:" + port + "/" + r.GetRegistryUri()
	const noAuth = "# This instance asks for no credential, so the credential helper Google's console\n" +
		"# configures first is not needed here.\n\n"
	switch r.GetFormat() {
	case arpb.Repository_DOCKER:
		host := loc + "-docker.pkg.localhost:" + port
		img := host + "/" + project + "/" + id + "/IMAGE"
		return "# On Google Cloud: gcloud auth configure-docker " + loc + "-docker.pkg.dev\n" + noAuth +
			"# Tag and push an image (a .localhost name is loopback, which docker treats as an insecure registry):\n" +
			"docker tag SOURCE-IMAGE " + img + ":TAG\n" +
			"docker push " + img + ":TAG\n\n" +
			"# Pull it:\n" +
			"docker pull " + img + ":TAG\n"
	case arpb.Repository_PYTHON:
		return "# On Google Cloud: pip install keyrings.google-artifactregistry-auth\n" + noAuth +
			"# ~/.pypirc\n[distutils]\nindex-servers =\n    " + id + "\n\n[" + id + "]\nrepository: " + base + "/\n\n" +
			"# Upload: twine upload --repository-url " + base + "/ dist/*\n\n" +
			"# pip.conf\n[global]\nindex-url = " + base + "/simple/\n\n" +
			"# Install: pip install --index-url " + base + "/simple/ PACKAGE\n"
	case arpb.Repository_NPM:
		return "# On Google Cloud: npx google-artifactregistry-auth\n" + noAuth +
			"# .npmrc\nregistry=" + base + "/\n\n" +
			"# Publish: npm publish --registry " + base + "/\n" +
			"# Install: npm install --registry " + base + "/ PACKAGE\n"
	case arpb.Repository_MAVEN:
		return "# On Google Cloud: the artifactregistry-maven-wagon extension authenticates mvn.\n" + noAuth +
			"<!-- pom.xml -->\n<distributionManagement>\n  <snapshotRepository>\n    <id>" + id + "</id>\n    <url>" + base + "</url>\n" +
			"  </snapshotRepository>\n  <repository>\n    <id>" + id + "</id>\n    <url>" + base + "</url>\n  </repository>\n" +
			"</distributionManagement>\n<repositories>\n  <repository>\n    <id>" + id + "</id>\n    <url>" + base + "</url>\n" +
			"    <releases><enabled>true</enabled></releases>\n    <snapshots><enabled>true</enabled></snapshots>\n  </repository>\n</repositories>\n"
	case arpb.Repository_APT:
		return noAuth + "# Import .deb packages from this instance's Cloud Storage with Import from Cloud Storage, then:\n" +
			"echo 'deb [trusted=yes] " + base + " " + id + " main' | sudo tee /etc/apt/sources.list.d/" + id + ".list\n" +
			"sudo apt update\n"
	case arpb.Repository_YUM:
		return noAuth + "# Import .rpm packages from this instance's Cloud Storage with Import from Cloud Storage, then:\n" +
			"# /etc/yum.repos.d/" + id + ".repo\n[" + id + "]\nname=" + id + "\nbaseurl=" + base + "/" + id + "\nenabled=1\n" +
			"repo_gpgcheck=0\ngpgcheck=0\n"
	case arpb.Repository_GENERIC:
		return noAuth + "# Upload a file:\ngcloud artifacts generic upload --project=" + project + " --location=" + loc +
			" --repository=" + id + " --package=PACKAGE --version=VERSION --source=FILE\n\n" +
			"# Download it:\ngcloud artifacts generic download --project=" + project + " --location=" + loc +
			" --repository=" + id + " --package=PACKAGE --version=VERSION --destination=DIR\n\n" +
			"# gcloud reaches this instance through the overrides `cloudburrow gcloud-setup` prints.\n"
	}
	return packageUse(r)
}

// arTimeOf is a time.Time as arTime shows it.
func arTimeOf(t time.Time) string { return arTime(timestamppb.New(t)) }
