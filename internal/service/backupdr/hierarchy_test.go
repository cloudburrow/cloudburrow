package backupdr

import (
	"strings"
	"testing"
)

// inventory is Cloud SQL with instances in several projects.
type inventory struct {
	*fakeSQL
	instances map[string][]string // by project
}

func (v inventory) Instance(project, instance string) (Instance, bool) {
	for _, n := range v.instances[project] {
		if n == instance {
			return Instance{DatabaseVersion: "POSTGRES_17", Region: "us-central1"}, true
		}
	}
	return Instance{}, false
}

func (v inventory) Instances(project string) []string { return v.instances[project] }

// tree is a Hierarchy: folders/top holds p1 and folders/sub, which holds
// p2; the organization holds both folders and p3.
type tree map[string][]string

func (t tree) ProjectsUnder(parent string) ([]string, error) {
	ids, ok := t[parent]
	if !ok {
		return nil, ErrNoParent
	}
	return ids, nil
}

func TestFetchUnderFolderAndOrganization(t *testing.T) {
	s := NewServer("", inventory{&fakeSQL{runs: map[string]Run{}}, map[string][]string{
		"p1": {"b-one"}, "p2": {"a-two", "c-two"}, "p3": {"d-three"}, "outside": {"e-out"}}}, "")
	path := func(parent string) string {
		return "/v1/" + parent + "/locations/us-central1/resourceBackupConfigs:fetch"
	}
	// Without a hierarchy both are refused, naming why.
	if code, out := call(t, s, "GET", path("folders/top"), ""); code != 501 ||
		!strings.Contains(out["error"].(map[string]any)["message"].(string), "Resource Manager") {
		t.Fatalf("no hierarchy: %d %v", code, out)
	}
	s.UseHierarchy(tree{"folders/top": {"p2", "p1"}, "folders/sub": {"p2"}, "organizations/100000000001": {"p1", "p2", "p3"}})
	names := func(out map[string]any) []string {
		var got []string
		for _, c := range out["resourceBackupConfigs"].([]any) {
			got = append(got, c.(map[string]any)["targetResourceDisplayName"].(string))
		}
		return got
	}
	for _, c := range []struct{ parent, query, want string }{
		{"folders/top", "", "a-two b-one c-two"},
		{"folders/sub", "", "a-two c-two"},
		{"organizations/100000000001", "", "a-two b-one c-two d-three"},
		{"organizations/100000000001", "?orderBy=target_resource_display_name%20desc", "d-three c-two b-one a-two"},
		{"organizations/100000000001", "?filter=target_resource_display_name%3D%22b-one%22%20OR%20target_resource_display_name%3D%22d-three%22", "b-one d-three"},
	} {
		code, out := call(t, s, "GET", path(c.parent)+c.query, "")
		if code != 200 || strings.Join(names(out), " ") != c.want {
			t.Errorf("%s%s: %d %v, want %s", c.parent, c.query, code, out, c.want)
		}
	}
	// Each config is named under its own project.
	_, out := call(t, s, "GET", path("folders/sub"), "")
	if n := out["resourceBackupConfigs"].([]any)[0].(map[string]any)["name"].(string); !strings.HasPrefix(n, "projects/p2/locations/us-central1/resourceBackupConfigs/") {
		t.Errorf("name %s", n)
	}
	// Paging, one at a time; a token is the folder's own.
	var got []string
	tok := ""
	for i := 0; i < 5; i++ {
		q := "?pageSize=1"
		if tok != "" {
			q += "&pageToken=" + tok
		}
		code, out := call(t, s, "GET", path("folders/top")+q, "")
		if code != 200 {
			t.Fatalf("page %d: %d %v", i, code, out)
		}
		got = append(got, names(out)...)
		tok, _ = out["nextPageToken"].(string)
		if tok == "" {
			break
		}
		if i == 0 {
			if code, _ := call(t, s, "GET", path("folders/sub")+"?pageSize=1&pageToken="+tok, ""); code != 400 {
				t.Errorf("another folder's token: %d, want 400", code)
			}
		}
	}
	if strings.Join(got, " ") != "a-two b-one c-two" {
		t.Errorf("pages %v", got)
	}
	if code, out := call(t, s, "GET", path("folders/nope"), ""); code != 403 {
		t.Errorf("unknown folder: %d %v, want 403", code, out)
	}
}
