package iameval

import (
	"reflect"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/iampolicy"
)

// TestProjectTeams (#1887): the basic roles on the project's own policy,
// through groups too, name Cloud Storage's project teams.
func TestProjectTeams(t *testing.T) {
	src := fakeSource{policies: map[string]*iampolicy.Stored{"projects/p": pol(
		iampolicy.Binding{Role: "roles/editor", Members: []string{"group:eng@example.com"}},
		iampolicy.Binding{Role: "roles/viewer", Members: []string{"serviceAccount:" + sa}},
		iampolicy.Binding{Role: "roles/storage.admin", Members: []string{"serviceAccount:" + sa}},
	)}, groups: map[string][]string{sa: {"group:eng@example.com"}}}
	e := &Evaluator{Source: src}
	if got := e.ProjectTeams("p", sa, time.Now()); !reflect.DeepEqual(got, []string{"editors", "viewers"}) {
		t.Errorf("ProjectTeams = %v", got)
	}
	if got := e.ProjectTeams("p", "other@example.com", time.Now()); len(got) != 0 {
		t.Errorf("ProjectTeams of a stranger = %v", got)
	}
}
