package sqladmin

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/cloudburrow/cloudburrow/internal/orgpolicy"
)

// Custom constraints (#1895) on sqladmin.googleapis.com/Instance (insert,
// patch and update) and sqladmin.googleapis.com/BackupRun (insert), CREATE
// and UPDATE ("Add custom organization policies", Cloud SQL, read
// 2026-11-19): the condition reads the DatabaseInstance or BackupRun by
// its REST field names, the documented ones alone
// (internal/orgpolicy/fields_sql.go), the instance as the request would
// leave it. The page shows no error text beyond "This description is used
// as an error message when the policy is violated"; a violation is
// answered as the organization policy refusals here are, 412
// FAILED_PRECONDITION, with the "Operation denied by custom org policies:
// [...]" message the other services' pages show.

// checkCustom refuses method on a resource of resourceType in project.
func checkCustom(ctx context.Context, project, resourceType, method string, res map[string]any) error {
	if !orgpolicy.SourceSet() || project == "" {
		return nil
	}
	err := orgpolicy.CheckCustom(ctx, "projects/"+project, resourceType, method, res)
	var v *orgpolicy.CustomViolation
	if errors.As(err, &v) {
		return Errorf(http.StatusPreconditionFailed, "%s", v.Error())
	}
	return nil
}

// checkInstanceCustom checks an instance; the int64 fields REST sends as
// strings are numbers to the condition, as in the API's proto.
func checkInstanceCustom(ctx context.Context, project string, in map[string]any, method string) error {
	if !orgpolicy.SourceSet() {
		return nil
	}
	if st, ok := in["settings"].(map[string]any); ok {
		for _, k := range []string{"dataDiskSizeGb", "dataDiskProvisionedIops", "dataDiskProvisionedThroughput"} {
			if str, ok := st[k].(string); ok {
				if n, err := strconv.ParseInt(str, 10, 64); err == nil {
					st[k] = n
				}
			}
		}
	}
	return checkCustom(ctx, project, orgpolicy.TypeSQLInstance, method, in)
}

// insertResource is the instance an insert makes, as sent, with its
// project and region.
func insertResource(b instanceBody, project, region string) map[string]any {
	in := deepCopy(b.whole)
	in["project"] = project
	in["region"] = region
	return in
}

// mergeInstance is cur as body leaves it: a patch merges each object
// field into cur's (a null clears it), an update replaces every top-level
// field it sends.
func mergeInstance(cur, body map[string]any, replace bool) map[string]any {
	out := deepCopy(cur)
	for k, v := range body {
		switch {
		case v == nil:
			delete(out, k)
		case replace:
			out[k] = deepCopyValue(v)
		default:
			out[k] = mergeValue(out[k], v)
		}
	}
	return out
}

func mergeValue(cur, patch any) any {
	pm, ok := patch.(map[string]any)
	cm, ok2 := cur.(map[string]any)
	if !ok || !ok2 {
		return deepCopyValue(patch)
	}
	out := deepCopy(cm)
	for k, v := range pm {
		if v == nil {
			delete(out, k)
			continue
		}
		out[k] = mergeValue(out[k], v)
	}
	return out
}

func deepCopy(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = deepCopyValue(v)
	}
	return out
}

func deepCopyValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		return deepCopy(t)
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = deepCopyValue(x)
		}
		return out
	}
	return v
}
