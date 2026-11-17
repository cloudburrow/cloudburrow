package iamroles

import "testing"

func TestCatalog(t *testing.T) {
	if n := len(Names()); n < 14000 {
		t.Fatalf("the catalog has %d permissions", n)
	}
	for p, want := range map[string]Support{
		"datastore.entities.get":        Supported,
		"redis.clusters.connect":        Supported,
		"memorystore.instances.connect": Supported,
		"appengine.runtimes.actAsAdmin": NotSupported,
		"datastore.entities.frobnicate": Unknown,
		"resourcemanager.projects.get":  Supported,
	} {
		if got := Level(p); got != want {
			t.Errorf("Level(%s) = %v, want %v", p, got, want)
		}
	}
	if ProjectApplicable("resourcemanager.organizations.get") || ProjectApplicable("resourcemanager.folders.list") {
		t.Error("an organization or folder permission is applicable to a project role")
	}
	if !ProjectApplicable("storage.folders.create") || !ProjectApplicable("datastore.entities.get") {
		t.Error("a project permission is not applicable to a project role")
	}
}
