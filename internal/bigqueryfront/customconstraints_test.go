package bigqueryfront

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"cloud.google.com/go/orgpolicy/apiv2/orgpolicypb"

	"github.com/cloudburrow/cloudburrow/internal/orgpolicy"
)

// customSource enforces its custom constraints, by resource type, on
// every project.
type customSource map[string][]orgpolicy.CustomConstraint

func (customSource) Effective(context.Context, string, string) (orgpolicy.Effective, error) {
	return orgpolicy.Effective{}, nil
}

func (s customSource) Customs(_ context.Context, _, resourceType, _ string) ([]orgpolicy.CustomConstraint, error) {
	return s[resourceType], nil
}

func denyCustom(name, condition, desc string) orgpolicy.CustomConstraint {
	return orgpolicy.CustomConstraint{Name: name, Condition: condition, ActionType: "DENY", Description: desc,
		Effective: orgpolicy.AsResponse(orgpolicy.Effective{Spec: &orgpolicypb.PolicySpec{Rules: []*orgpolicypb.PolicySpec_PolicyRule{
			{Kind: &orgpolicypb.PolicySpec_PolicyRule_Enforce{Enforce: true}}}}})}
}

// TestCustomConstraintsOnBigQuery (#1895): the page's example (DENY
// datasetId starting with 'test') refuses datasets.insert, 412 with the
// page's message; a DENY on defaultTableExpirationMs (an int64 string)
// refuses a datasets.patch lowering it; table, routine, model and row
// access policy constraints read their references; a condition on an
// undocumented field (Table's schema) cannot be evaluated and refuses.
func TestCustomConstraintsOnBigQuery(t *testing.T) {
	desc := "All new datasets can't begin with 'test'."
	orgpolicy.Use(customSource{
		orgpolicy.TypeBigQueryDataset: {denyCustom("custom.enforceDatasetId", "resource.datasetReference.datasetId.startsWith('test')", desc),
			denyCustom("custom.expiry", "has(resource.defaultTableExpirationMs) && resource.defaultTableExpirationMs < 3600000", "An hour at least.")},
		orgpolicy.TypeBigQueryTable:           {denyCustom("custom.tmp", "resource.tableReference.tableId.startsWith('tmp')", "No tmp tables.")},
		orgpolicy.TypeBigQueryRoutine:         {denyCustom("custom.fn", "resource.routineReference.routineId == 'bad'", "No bad routines.")},
		orgpolicy.TypeBigQueryModel:           {denyCustom("custom.model", "resource.modelReference.modelId == 'm'", "No m.")},
		orgpolicy.TypeBigQueryRowAccessPolicy: {denyCustom("custom.rap", "resource.rowAccessPolicyReference.tableId == 't'", "No policies on t.")},
	})
	defer orgpolicy.Use(nil)
	h := Wrap(&datasetEmulator{datasets: map[string]string{}})
	code, got := do(t, h, "POST", base+"/datasets", `{"datasetReference":{"datasetId":"test1","projectId":"p"}}`)
	want := `Operation denied by custom org policies: ["customConstraints/custom.enforceDatasetId": "All new datasets can't begin with 'test'."]`
	if code != 412 || !strings.Contains(fmt.Sprint(got), want) {
		t.Fatalf("a test dataset = %d %v", code, got)
	}
	if code, got := do(t, h, "POST", base+"/datasets", `{"datasetReference":{"datasetId":"prod","projectId":"p"},"defaultTableExpirationMs":"7200000"}`); code != 200 {
		t.Fatalf("a prod dataset = %d %v", code, got)
	}
	if code, got := do(t, h, "PATCH", base+"/datasets/prod", `{"defaultTableExpirationMs":"60000"}`); code != 412 || !strings.Contains(fmt.Sprint(got), "custom.expiry") {
		t.Errorf("lowering the expiration = %d %v", code, got)
	}
	for _, c := range []struct{ method, path, body, name string }{
		{"POST", base + "/datasets/prod/tables", `{"tableReference":{"tableId":"tmp1"}}`, "custom.tmp"},
		{"PATCH", base + "/datasets/prod/tables/tmp2", `{}`, "custom.tmp"},
		{"POST", base + "/datasets/prod/routines", `{"routineReference":{"routineId":"bad"}}`, "custom.fn"},
		{"PUT", base + "/datasets/prod/routines/bad", `{}`, "custom.fn"},
		{"PATCH", base + "/datasets/prod/models/m", `{}`, "custom.model"},
		{"POST", base + "/datasets/prod/tables/t/rowAccessPolicies", `{"rowAccessPolicyReference":{"policyId":"x"}}`, "custom.rap"},
	} {
		if code, got := do(t, h, c.method, c.path, c.body); code != 412 || !strings.Contains(fmt.Sprint(got), c.name) {
			t.Errorf("%s %s = %d %v, want 412 %s", c.method, c.path, code, got, c.name)
		}
	}
	orgpolicy.Use(customSource{orgpolicy.TypeBigQueryTable: {denyCustom("custom.schema", "size(resource.schema.fields) > 3", "Small tables.")}})
	if code, got := do(t, h, "POST", base+"/datasets/prod/tables", `{"tableReference":{"tableId":"x"},"schema":{"fields":[]}}`); code != 412 {
		t.Errorf("a condition on the undocumented schema = %d %v, want 412", code, got)
	}
}
