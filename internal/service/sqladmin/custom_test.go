package sqladmin

import (
	"context"
	"strings"
	"testing"

	"cloud.google.com/go/orgpolicy/apiv2/orgpolicypb"

	"github.com/cloudburrow/cloudburrow/internal/orgpolicy"
)

// customSource enforces its custom constraints of each resource type on
// every project.
type customSource []orgpolicy.CustomConstraint

func (customSource) Effective(context.Context, string, string) (orgpolicy.Effective, error) {
	return orgpolicy.Effective{}, nil
}

func (s customSource) Customs(_ context.Context, _, resourceType, method string) ([]orgpolicy.CustomConstraint, error) {
	var out []orgpolicy.CustomConstraint
	for _, c := range s {
		if strings.HasPrefix(c.DisplayName, resourceType+" ") && strings.Contains(c.DisplayName, method) {
			out = append(out, c)
		}
	}
	return out, nil
}

func enforcedCustom(name, resourceType, condition, action, desc string, methods ...string) orgpolicy.CustomConstraint {
	return orgpolicy.CustomConstraint{Name: name, Condition: condition, ActionType: action, Description: desc,
		DisplayName: resourceType + " " + strings.Join(methods, ","),
		Effective: orgpolicy.AsResponse(orgpolicy.Effective{Spec: &orgpolicypb.PolicySpec{Rules: []*orgpolicypb.PolicySpec_PolicyRule{
			{Kind: &orgpolicypb.PolicySpec_PolicyRule_Enforce{Enforce: true}}}}})}
}

// TestCustomConstraintsOnCloudSQL (#1895): a DENY on
// resource.settings.dataDiskSizeGb (an int64 REST sends as a string)
// refuses an insert and a patch that would leave the disk too large, 412
// FAILED_PRECONDITION "Operation denied by custom org policies: [...]"; a
// condition on an undocumented field (rootPassword) cannot be evaluated
// and refuses; a DENY on a backup run's description refuses it.
func TestCustomConstraintsOnCloudSQL(t *testing.T) {
	orgpolicy.Use(customSource{
		enforcedCustom("custom.disk", orgpolicy.TypeSQLInstance, "resource.settings.dataDiskSizeGb > 100", "DENY", "Disks of 100 GB at most.", "CREATE", "UPDATE"),
		enforcedCustom("custom.adhoc", orgpolicy.TypeSQLBackupRun, "resource.description.startsWith('adhoc')", "DENY", "No ad hoc backups.", "CREATE"),
	})
	defer orgpolicy.Use(nil)
	ts, _ := fieldsServer(t)
	base := ts.URL + "/v1/projects/proj/instances"
	want := `Operation denied by custom org policies: [\"customConstraints/custom.disk\":\"Disks of 100 GB at most.\"]`
	code, body := call(t, "POST", base, `{"name":"big","databaseVersion":"POSTGRES_17","settings":{"tier":"db-custom-1-3840","dataDiskSizeGb":"200"}}`)
	if code != 412 || !strings.Contains(body, want) || !strings.Contains(body, "FAILED_PRECONDITION") {
		t.Fatalf("a 200 GB insert = %d %s", code, body)
	}
	code, body = call(t, "POST", base, `{"name":"small","databaseVersion":"POSTGRES_17","settings":{"tier":"db-custom-1-3840","dataDiskSizeGb":"50"}}`)
	if code != 200 {
		t.Fatalf("a 50 GB insert = %d %s", code, body)
	}
	getRunnable(t, base+"/small")
	if code, body := call(t, "PATCH", base+"/small", `{"settings":{"dataDiskSizeGb":"500"}}`); code != 412 || !strings.Contains(body, want) {
		t.Errorf("a patch to 500 GB = %d %s", code, body)
	}
	if code, body := call(t, "PATCH", base+"/small", `{"settings":{"userLabels":{"a":"b"}}}`); code != 200 {
		t.Errorf("a patch of the labels = %d %s", code, body)
	}
	if code, body := call(t, "POST", base+"/small/backupRuns", `{"description":"adhoc 1"}`); code != 412 || !strings.Contains(body, "custom.adhoc") {
		t.Errorf("an ad hoc backup = %d %s", code, body)
	}

	orgpolicy.Use(customSource{
		enforcedCustom("custom.pw", orgpolicy.TypeSQLInstance, "resource.rootPassword == ''", "DENY", "Undocumented field.", "CREATE"),
	})
	if code, body := call(t, "POST", base, `{"name":"pw","databaseVersion":"POSTGRES_17","rootPassword":"x","settings":{"tier":"db-custom-1-3840"}}`); code != 412 || !strings.Contains(body, "custom.pw") {
		t.Errorf("a condition on rootPassword = %d %s, want 412 (not evaluable)", code, body)
	}
}
