package console

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

// budgetProvider records the deadline its create was given.
type budgetProvider struct {
	fakeProvider
	budget time.Duration
	left   *time.Duration
}

func (budgetProvider) CreateForm() (string, []Field) {
	return "Create", []Field{{Name: "name", Label: "Name", Type: "text"}}
}

func (p budgetProvider) Create(ctx context.Context, _ string, _ map[string]string) (string, error) {
	if d, ok := ctx.Deadline(); ok {
		*p.left = time.Until(d)
	}
	return "made", nil
}

func (p budgetProvider) MutationBudget() time.Duration { return p.budget }

// A provider whose create waits on a rollout gets its own budget, and every
// other keeps the default minute; a budget under the default is ignored
// (#1348: a Cloud Run instance's create waits until it is ready).
func TestMutationBudgetIsTheProviders(t *testing.T) {
	for _, tc := range []struct {
		budget time.Duration
		min    time.Duration
		max    time.Duration
	}{
		{0, 55 * time.Second, mutationBudget},
		{time.Second, 55 * time.Second, mutationBudget},
		{5*time.Minute + 30*time.Second, 5 * time.Minute, 5*time.Minute + 30*time.Second},
	} {
		var left time.Duration
		srv := serve(t, budgetProvider{fakeProvider: fakeProvider{id: "b", title: "B"}, budget: tc.budget, left: &left})
		resp, err := http.Post(srv.URL+"/api/resources/b?project=p", "application/json", strings.NewReader(`{"name":"x"}`))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || left < tc.min || left > tc.max {
			t.Errorf("budget %v: create = %d with %v left; want between %v and %v", tc.budget, resp.StatusCode, left, tc.min, tc.max)
		}
	}
}
