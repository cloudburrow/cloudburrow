//go:build compat

package compat

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strings"
	"testing"

	"cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"
	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"google.golang.org/api/iterator"
)

// TestTerraformArtifactRegistryFormatDataSources (#1304): a DOCKER, a
// PYTHON, an NPM and a MAVEN google_artifact_registry_repository apply;
// an image is pushed to the first with go-containerregistry, a wheel
// uploaded to the second with twine's protocol, a package published to the
// third with npm's and a jar and pom put to the fourth with Maven's; the
// data sources google_artifact_registry_docker_image and _docker_images,
// _python_package and _python_packages, _npm_package and _npm_packages, and
// _maven_artifact and _maven_artifacts then read what the official
// artifactregistry client lists; the plan is clean; new descriptions apply
// in place and the data sources still read the same; the plan is clean
// again; destroy deletes the repositories.
// covers: google.devtools.artifactregistry.v1.ArtifactRegistry/ListDockerImages, google.devtools.artifactregistry.v1.ArtifactRegistry/ListPythonPackages, google.devtools.artifactregistry.v1.ArtifactRegistry/ListNpmPackages, google.devtools.artifactregistry.v1.ArtifactRegistry/ListMavenArtifacts
func TestTerraformArtifactRegistryFormatDataSources(t *testing.T) {
	h := New(t)
	_ = runClient(t, h) // skips without CLOUDBURROW_TEST_RUN
	ar, ctx := arClient(t, h)
	m := newTFModule(t)
	p := h.Project()
	suffix := strings.TrimPrefix(p, "cb-test-")
	docker, python, npm, maven := "tfds-docker-"+suffix, "tfds-py-"+suffix, "tfds-npm-"+suffix, "tfds-mvn-"+suffix
	repos := func(desc string) string {
		var b strings.Builder
		for _, r := range [][2]string{{"docker", docker}, {"python", python}, {"npm", npm}, {"maven", maven}} {
			fmt.Fprintf(&b, `
resource "google_artifact_registry_repository" "%[1]s" {
  project       = %[2]q
  location      = "us-central1"
  repository_id = %[3]q
  format        = %[4]q
  description   = %[5]q
}
`, r[0], p, r[1], strings.ToUpper(r[0]), desc)
		}
		return b.String()
	}
	dataSources := fmt.Sprintf(`
data "google_artifact_registry_docker_image" "i" {
  project       = %[1]q
  location      = "us-central1"
  repository_id = google_artifact_registry_repository.docker.repository_id
  image_name    = "app:v1"
}
data "google_artifact_registry_docker_images" "i" {
  project       = %[1]q
  location      = "us-central1"
  repository_id = google_artifact_registry_repository.docker.repository_id
}
data "google_artifact_registry_python_package" "p" {
  project       = %[1]q
  location      = "us-central1"
  repository_id = google_artifact_registry_repository.python.repository_id
  package_name  = "demo-pkg"
}
data "google_artifact_registry_python_packages" "p" {
  project       = %[1]q
  location      = "us-central1"
  repository_id = google_artifact_registry_repository.python.repository_id
}
data "google_artifact_registry_npm_package" "n" {
  project       = %[1]q
  location      = "us-central1"
  repository_id = google_artifact_registry_repository.npm.repository_id
  package_name  = "cb-demo"
}
data "google_artifact_registry_npm_packages" "n" {
  project       = %[1]q
  location      = "us-central1"
  repository_id = google_artifact_registry_repository.npm.repository_id
}
data "google_artifact_registry_maven_artifact" "m" {
  project       = %[1]q
  location      = "us-central1"
  repository_id = google_artifact_registry_repository.maven.repository_id
  group_id      = "com.example"
  artifact_id   = "demo-lib"
}
data "google_artifact_registry_maven_artifacts" "m" {
  project       = %[1]q
  location      = "us-central1"
  repository_id = google_artifact_registry_repository.maven.repository_id
}
output "outputs" {
  value = {
    docker_image  = "${data.google_artifact_registry_docker_image.i.name}|${join(",", sort(data.google_artifact_registry_docker_image.i.tags))}|${data.google_artifact_registry_docker_image.i.media_type}"
    docker_images = sort([for i in data.google_artifact_registry_docker_images.i.docker_images : "${i.name}|${join(",", sort(i.tags))}"])
    python        = "${data.google_artifact_registry_python_package.p.name}|${data.google_artifact_registry_python_package.p.version}"
    pythons       = sort([for x in data.google_artifact_registry_python_packages.p.python_packages : "${x.name}|${x.version}"])
    npm           = "${data.google_artifact_registry_npm_package.n.name}|${data.google_artifact_registry_npm_package.n.version}"
    npms          = sort([for x in data.google_artifact_registry_npm_packages.n.npm_packages : "${x.name}|${x.version}"])
    maven         = "${data.google_artifact_registry_maven_artifact.m.name}|${data.google_artifact_registry_maven_artifact.m.version}"
    mavens        = sort([for x in data.google_artifact_registry_maven_artifacts.m.maven_artifacts : "${x.name}|${x.version}"])
  }
}
`, p)

	m.write(repos("first"))
	m.must("init", "-input=false", "-no-color")
	m.apply()

	// The artifacts, each through its format's own protocol.
	_, port, _ := net.SplitHostPort(h.Endpoint(EnvRun))
	img, err := random.Image(512, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := crane.Push(img, "us-central1-docker.pkg.localhost:"+port+"/"+p+"/"+docker+"/app:v1", registryOptions(h)...); err != nil {
		t.Fatalf("crane push: %v", err)
	}
	file, wheel := pyWheel(t, "0.1.0")
	if code, b := twineUpload(t, pkgBase(h, "python", p, python), "demo-pkg", "0.1.0", file, wheel); code != http.StatusOK {
		t.Fatalf("twine upload = %d %s", code, b)
	}
	mustHTTP(t, "PUT", pkgBase(h, "npm", p, npm)+"/cb-demo", "application/json", npmPublishBody("cb-demo", "1.0.0", npmTarball(t, "cb-demo", "1.0.0")))
	dir := pkgBase(h, "maven", p, maven) + "/com/example/demo-lib/1.2.0/"
	for name, body := range map[string][]byte{"demo-lib-1.2.0.jar": []byte("PK-not-really-a-jar"),
		"demo-lib-1.2.0.pom": []byte("<project><groupId>com.example</groupId><artifactId>demo-lib</artifactId><version>1.2.0</version></project>")} {
		mustHTTP(t, "PUT", dir+name, "", body)
		s := sha1.Sum(body)
		mustHTTP(t, "PUT", dir+name+".sha1", "", []byte(hex.EncodeToString(s[:])))
	}

	parent := runParent(h) + "/repositories/"
	check := func(when string) {
		t.Helper()
		var got map[string]any
		if err := json.Unmarshal([]byte(m.must("output", "-json", "outputs")), &got); err != nil {
			t.Fatalf("%s, the outputs: %v", when, err)
		}
		list := func(next func() (string, error)) []any {
			t.Helper()
			var out []string
			for {
				s, err := next()
				if err == iterator.Done {
					break
				}
				if err != nil {
					t.Fatalf("%s: %v", when, err)
				}
				out = append(out, s)
			}
			sort.Strings(out)
			res := []any{}
			for _, s := range out {
				res = append(res, s)
			}
			return res
		}
		want := map[string]any{}
		di := ar.ListDockerImages(ctx, &artifactregistrypb.ListDockerImagesRequest{Parent: parent + docker})
		var image *artifactregistrypb.DockerImage
		want["docker_images"] = list(func() (string, error) {
			i, err := di.Next()
			if err != nil {
				return "", err
			}
			image = i
			tags := append([]string(nil), i.GetTags()...)
			sort.Strings(tags)
			return i.GetName() + "|" + strings.Join(tags, ","), nil
		})
		if image == nil {
			t.Fatalf("%s: ListDockerImages lists no image", when)
		}
		tags := append([]string(nil), image.GetTags()...)
		sort.Strings(tags)
		want["docker_image"] = image.GetName() + "|" + strings.Join(tags, ",") + "|" + image.GetMediaType()
		pi := ar.ListPythonPackages(ctx, &artifactregistrypb.ListPythonPackagesRequest{Parent: parent + python})
		want["pythons"] = list(func() (string, error) {
			x, err := pi.Next()
			if err != nil {
				return "", err
			}
			if x.GetPackageName() == "demo-pkg" {
				want["python"] = x.GetName() + "|" + x.GetVersion()
			}
			return x.GetName() + "|" + x.GetVersion(), nil
		})
		ni := ar.ListNpmPackages(ctx, &artifactregistrypb.ListNpmPackagesRequest{Parent: parent + npm})
		want["npms"] = list(func() (string, error) {
			x, err := ni.Next()
			if err != nil {
				return "", err
			}
			if x.GetPackageName() == "cb-demo" {
				want["npm"] = x.GetName() + "|" + x.GetVersion()
			}
			return x.GetName() + "|" + x.GetVersion(), nil
		})
		mi := ar.ListMavenArtifacts(ctx, &artifactregistrypb.ListMavenArtifactsRequest{Parent: parent + maven})
		want["mavens"] = list(func() (string, error) {
			x, err := mi.Next()
			if err != nil {
				return "", err
			}
			if x.GetGroupId() == "com.example" && x.GetArtifactId() == "demo-lib" {
				want["maven"] = x.GetName() + "|" + x.GetVersion()
			}
			return x.GetName() + "|" + x.GetVersion(), nil
		})
		if fmt.Sprint(got) != fmt.Sprint(want) || !strings.Contains(fmt.Sprint(want["docker_image"]), "|v1|") {
			t.Errorf("%s: the data sources read\n%v\nthe official client\n%v", when, got, want)
		}
	}

	m.write(repos("first") + dataSources)
	m.apply()
	check("after apply")
	m.planClean()

	m.write(repos("second") + dataSources)
	out := m.must("apply", "-auto-approve", "-input=false", "-no-color")
	if !strings.Contains(out, "0 added, 4 changed, 0 destroyed") {
		t.Errorf("the descriptions were not changed in place:\n%s", lastLines(out, 20))
	}
	check("after the change")
	m.planClean()

	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	for _, r := range []string{docker, python, npm, maven} {
		if _, err := ar.GetRepository(ctx, &artifactregistrypb.GetRepositoryRequest{Name: parent + r}); err == nil {
			t.Errorf("repository %s survived destroy", r)
		}
	}
}
