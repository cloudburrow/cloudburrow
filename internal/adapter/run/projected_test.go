package run

import (
	"strings"
	"testing"

	runpb "cloud.google.com/go/run/apiv2/runpb"
	"gopkg.in/yaml.v3"
)

// versionResolver resolves each version to a Kubernetes Secret of its own,
// as Secret Manager does since #1596.
type versionResolver struct{}

func (versionResolver) ResolveSecretRef(project, secret, version string) (string, string, error) {
	return "cb-secret-" + secret + "-v" + version, "v" + version, nil
}

// A secret volume of two versions reads two Kubernetes Secrets, so it is a
// projected volume with a source per Secret; one version stays a plain
// secret volume (#1596).
func TestSecretVolumeOfSeveralVersionsIsProjected(t *testing.T) {
	t.Parallel()
	s := svc("app", &runpb.Container{Image: "gcr.io/p/app:v1", VolumeMounts: []*runpb.VolumeMount{
		{Name: "creds", MountPath: "/secrets"}, {Name: "one", MountPath: "/one"}}})
	s.Template.Volumes = []*runpb.Volume{
		{Name: "creds", VolumeType: &runpb.Volume_Secret{Secret: &runpb.SecretVolumeSource{Secret: "db", DefaultMode: 0o440,
			Items: []*runpb.VersionToPath{{Path: "old", Version: "1"}, {Path: "new", Version: "2", Mode: 0o400}}}}},
		{Name: "one", VolumeType: &runpb.Volume_Secret{Secret: &runpb.SecretVolumeSource{Secret: "db",
			Items: []*runpb.VersionToPath{{Path: "x", Version: "2"}}}}},
	}
	y, err := ToKnative(s, "default", "test", versionResolver{})
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Spec struct {
			Template struct {
				Spec struct {
					Volumes []struct {
						Name   string `yaml:"name"`
						Secret *struct {
							SecretName string `yaml:"secretName"`
						} `yaml:"secret"`
						Projected *struct {
							DefaultMode int `yaml:"defaultMode"`
							Sources     []struct {
								Secret struct {
									Name  string `yaml:"name"`
									Items []struct {
										Key  string `yaml:"key"`
										Path string `yaml:"path"`
										Mode int    `yaml:"mode"`
									} `yaml:"items"`
								} `yaml:"secret"`
							} `yaml:"sources"`
						} `yaml:"projected"`
					} `yaml:"volumes"`
				} `yaml:"spec"`
			} `yaml:"template"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal([]byte(y), &doc); err != nil {
		t.Fatalf("manifest does not parse: %v\n%s", err, y)
	}
	vols := doc.Spec.Template.Spec.Volumes
	if len(vols) != 2 {
		t.Fatalf("volumes = %+v\n%s", vols, y)
	}
	p := vols[0].Projected
	if p == nil || vols[0].Secret != nil || p.DefaultMode != 0o440 || len(p.Sources) != 2 {
		t.Fatalf("creds = %+v\n%s", vols[0], y)
	}
	first, second := p.Sources[0].Secret, p.Sources[1].Secret
	if first.Name != "cb-secret-db-v1" || len(first.Items) != 1 || first.Items[0].Key != "v1" || first.Items[0].Path != "old" {
		t.Errorf("first source = %+v", first)
	}
	if second.Name != "cb-secret-db-v2" || len(second.Items) != 1 || second.Items[0].Path != "new" || second.Items[0].Mode != 0o400 {
		t.Errorf("second source = %+v", second)
	}
	if vols[1].Secret == nil || vols[1].Secret.SecretName != "cb-secret-db-v2" || strings.Contains(y, "name: one\n          projected") {
		t.Errorf("one version is not a secret volume: %+v\n%s", vols[1], y)
	}
}
