package run

import (
	"strings"
	"testing"
)

// The ConfigMap the K_* policy reads (#1448) names the service's ID, and
// each revision's ID under its Knative name, for every generation up to
// kEnvAhead past the latest created one: the IDs the API reports.
func TestKEnvConfigMapNamesCloudRunIDs(t *testing.T) {
	t.Parallel()
	name := "projects/p/locations/us-central1/services/web"
	kname := serviceKName(name)
	m := kEnvConfigMap(name, kname, "cloudburrow", "inst", kname+"-00003")
	if !strings.Contains(m, "  service."+kname+": \"web\"\n") || !strings.Contains(m, labelKEnv+": \"true\"") {
		t.Errorf("ConfigMap lacks the service ID or its label:\n%s", m)
	}
	for g := 1; g <= 3+kEnvAhead; g++ {
		krev := kname + "-0000" + string(rune('0'+g))
		want := "  " + krev + ": \"" + RevisionID(name, kname, krev) + "\"\n"
		if !strings.Contains(m, want) {
			t.Errorf("ConfigMap lacks %q:\n%s", want, m)
		}
		if !strings.HasPrefix(RevisionID(name, kname, krev), "web-0000") {
			t.Errorf("revision ID %q is not Cloud Run's form", RevisionID(name, kname, krev))
		}
	}
	if strings.Contains(m, kname+"-00009") {
		t.Errorf("ConfigMap names a generation past latest+%d:\n%s", kEnvAhead, m)
	}
	p := kEnvPolicy("cloudburrow")
	for _, want := range []string{"kind: MutatingAdmissionPolicy\n", "kind: MutatingAdmissionPolicyBinding\n",
		"kubernetes.io/metadata.name: cloudburrow", "failurePolicy: Ignore", "name: 'K_SERVICE'", "name: 'K_REVISION'",
		"name: 'K_CONFIGURATION'", "parameterNotFoundAction: Allow"} {
		if !strings.Contains(p, want) {
			t.Errorf("policy lacks %q", want)
		}
	}
}
