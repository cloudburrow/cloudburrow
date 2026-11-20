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

func TestPredefinedRoles(t *testing.T) {
	if n := len(PredefinedNames()); n < 2400 {
		t.Fatalf("%d predefined roles, want Google's 2,423 and the 6 basic roles", n)
	}
	r := Predefined("roles/storage.objectViewer")
	if r == nil || r.Title != "Storage Object Viewer" || r.Stage != "GA" || !Grants(r.Permissions, "storage.objects.get") || Grants(r.Permissions, "storage.objects.delete") {
		t.Fatalf("roles/storage.objectViewer = %+v", r)
	}
	if r := Predefined("roles/aiplatform.expressUser"); r == nil || r.Stage != "BETA" {
		t.Fatalf("roles/aiplatform.expressUser = %+v, want BETA", r)
	}
	if r := Predefined("roles/viewer"); r == nil || r.Title != "Viewer" {
		t.Fatalf("roles/viewer = %+v", r)
	}
	if Predefined("roles/nope.nope") != nil {
		t.Fatal("an unknown role is in the catalog")
	}
	org := Predefined("roles/iam.organizationRoleAdmin")
	if !org.GrantableOn("Organization") || org.GrantableOn("Project") || org.GrantableOn("Folder") {
		t.Fatalf("roles/iam.organizationRoleAdmin grantable on a project or folder: %+v", org.Lowest)
	}
	if !Predefined("roles/storage.admin").GrantableOn("Project") {
		t.Fatal("roles/storage.admin is not grantable on a project")
	}
	if f := Predefined("roles/resourcemanager.folderAdmin"); f.GrantableOn("Project") || !f.GrantableOn("Folder") {
		t.Fatalf("roles/resourcemanager.folderAdmin: %+v", f.Lowest)
	}
}

func TestPredefinedRoleProducts(t *testing.T) {
	for name, want := range map[string]string{
		"roles/storage.objectViewer": "Cloud Storage",
		"roles/compute.admin":        "Compute Engine",
		"roles/datastore.user":       "Firestore",
		"roles/iam.devOps":           "Roles for job functions",
		"roles/viewer":               BasicProduct,
	} {
		if r := Predefined(name); r == nil || r.Product != want {
			t.Errorf("%s's product = %+v, want %q", name, r, want)
		}
	}
	for _, n := range PredefinedNames() {
		if Predefined(n).Product == "" {
			t.Errorf("%s has no product", n)
		}
	}
}
