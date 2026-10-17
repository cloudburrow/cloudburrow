package run

import (
	"context"
	"strings"
	"testing"

	arpb "cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestListRepositoriesFilterAndOrder: ListRepositories' documented name
// filter, * a wildcard and case insensitive, as Terraform's
// google_artifact_registry_repositories sends its name_filter (#1304), and
// order_by name, ascending or desc; anything else is INVALID_ARGUMENT.
func TestListRepositoriesFilterAndOrder(t *testing.T) {
	ctx := context.Background()
	a := &ArtifactRegistryServer{}
	parent := "projects/list-filter-test/locations/us-west1"
	repositories.mu.Lock()
	if repositories.repos == nil {
		repositories.repos = map[string]*arpb.Repository{}
	}
	for _, id := range []string{"tf-a", "tf-b1", "other"} {
		name := parent + "/repositories/" + id
		repositories.repos[name] = &arpb.Repository{Name: name, Format: arpb.Repository_DOCKER}
		t.Cleanup(func() {
			repositories.mu.Lock()
			delete(repositories.repos, name)
			repositories.mu.Unlock()
		})
	}
	repositories.mu.Unlock()
	list := func(filter, order string) (string, error) {
		resp, err := a.ListRepositories(ctx, &arpb.ListRepositoriesRequest{Parent: parent, Filter: filter, OrderBy: order})
		if err != nil {
			return "", err
		}
		var ids []string
		for _, r := range resp.GetRepositories() {
			ids = append(ids, strings.TrimPrefix(r.GetName(), parent+"/repositories/"))
		}
		return strings.Join(ids, ","), nil
	}
	for _, c := range []struct{ filter, order, want string }{
		{"", "", "other,tf-a,tf-b1"},
		{`name="` + parent + `/repositories/tf-*"`, "", "tf-a,tf-b1"},
		{` name = "` + parent + `/repositories/*1" `, "", "tf-b1"},
		{`name="` + strings.ToUpper(parent) + `/repositories/TF-A"`, "", "tf-a"},
		{`name="` + parent + `/repositories/t*-*1"`, "", "tf-b1"},
		{"", "name desc", "tf-b1,tf-a,other"},
		{"", "Name", "other,tf-a,tf-b1"},
	} {
		if got, err := list(c.filter, c.order); err != nil || got != c.want {
			t.Errorf("ListRepositories(filter %q, order_by %q) = %q, %v; want %q", c.filter, c.order, got, err, c.want)
		}
	}
	for _, c := range []struct{ filter, order string }{{"format=DOCKER", ""}, {"", "create_time"}} {
		if _, err := list(c.filter, c.order); status.Code(err) != codes.InvalidArgument {
			t.Errorf("ListRepositories(filter %q, order_by %q) = %v; want InvalidArgument", c.filter, c.order, err)
		}
	}
}
