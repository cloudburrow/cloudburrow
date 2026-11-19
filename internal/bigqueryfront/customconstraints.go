package bigqueryfront

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/orgpolicy"
)

// Custom constraints (#1895) on bigquery.googleapis.com/Dataset, Table,
// Routine, Model and RowAccessPolicy, CREATE and UPDATE ("Use
// organization policies to apply custom constraints to BigQuery
// resources", read 2026-11-19): the condition reads the REST resource by
// the fields the page lists alone (internal/orgpolicy/fields_bigquery.go),
// checked on the API's create and update methods (datasets.insert, patch,
// update; tables.insert, patch, update; routines.insert, update;
// models.patch; rowAccessPolicies.insert, update), the page describing
// those and no DDL. A Model is made by CREATE MODEL alone, so only its
// UPDATE is checked. A violation is the page's example message,
// `Operation denied by custom org policies:
// ["customConstraints/custom.enforceDatasetId": "All new datasets can't
// begin with 'test'."]`, answered as the front's other organization policy
// refusal (gcp.resourceLocations) is, 412 conditionNotMet.

// bigQueryCustomMessage is the BigQuery page's wording, a space after each
// constraint's colon.
func bigQueryCustomMessage(v *orgpolicy.CustomViolation) string {
	parts := make([]string, len(v.Names))
	for i, n := range v.Names {
		parts[i] = fmt.Sprintf("%q: %q", "customConstraints/"+n, v.Messages[i])
	}
	return "Operation denied by custom org policies: [" + strings.Join(parts, ", ") + "]"
}

// refuseCustom answers a violation of the custom constraints of
// resourceType enforced on project, and reports whether it did.
func refuseCustom(w http.ResponseWriter, r *http.Request, project, resourceType, method string, res map[string]any) bool {
	if !orgpolicy.SourceSet() || project == "" {
		return false
	}
	err := orgpolicy.CheckCustom(r.Context(), "projects/"+project, resourceType, method, res)
	var v *orgpolicy.CustomViolation
	if !errors.As(err, &v) {
		return false
	}
	writeError(w, http.StatusPreconditionFailed, "conditionNotMet", bigQueryCustomMessage(v))
	return true
}

// datasetResource is a Dataset's JSON as the condition reads it: its
// int64 fields, strings in REST, are numbers.
func datasetResource(m map[string]any, project, dataset string) map[string]any {
	if m == nil {
		m = map[string]any{}
	}
	ref, _ := m["datasetReference"].(map[string]any)
	if ref == nil {
		ref = map[string]any{}
	}
	if ref["projectId"] == nil {
		ref["projectId"] = project
	}
	if ref["datasetId"] == nil && dataset != "" {
		ref["datasetId"] = dataset
	}
	m["datasetReference"] = ref
	for _, k := range []string{"defaultPartitionExpirationMs", "defaultTableExpirationMs", "maxTimeTravelHours"} {
		if s, ok := m[k].(string); ok {
			if n, err := strconv.ParseInt(s, 10, 64); err == nil {
				m[k] = n
			}
		}
	}
	return m
}

// refuseCustomDatasetCreate checks a datasets.insert body, after the
// front's defaults (location) are in it.
func (f front) refuseCustomDatasetCreate(w http.ResponseWriter, r *http.Request, raw []byte) bool {
	if !orgpolicy.SourceSet() {
		return false
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return false
	}
	project := projectOf(f.base)
	return refuseCustom(w, r, project, orgpolicy.TypeBigQueryDataset, orgpolicy.MethodCreate, datasetResource(m, project, ""))
}

// bodyMap is r's JSON object body, or nil; the body stays readable.
func bodyMap(r *http.Request) map[string]any {
	if r.Header.Get("Content-Encoding") != "" {
		return nil
	}
	b, err := readBody(r)
	if err != nil {
		return nil
	}
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return m
}

func unescaped(s string) string {
	if u, err := url.PathUnescape(s); err == nil {
		return u
	}
	return s
}

// serveCustomConstraints refuses a create or update of a dataset, table,
// routine, model or row access policy that violates a custom constraint,
// and reports whether it answered. Dataset creates are checked by
// insertDataset, once the front has filled in their defaults.
func serveCustomConstraints(w http.ResponseWriter, r *http.Request, next http.Handler) bool {
	if !orgpolicy.SourceSet() || (r.Method != http.MethodPost && r.Method != http.MethodPut && r.Method != http.MethodPatch) {
		return false
	}
	path := r.URL.EscapedPath()
	write := r.Method == http.MethodPut || r.Method == http.MethodPatch
	if m := policiesRoute.FindStringSubmatch(path); m != nil && m[4] == "" {
		project, ds, table := unescaped(m[1]), unescaped(m[2]), unescaped(m[3])
		method := orgpolicy.MethodCreate
		switch {
		case r.Method == http.MethodPost && m[5] == "":
		case r.Method == http.MethodPut && m[5] != "":
			method = orgpolicy.MethodUpdate
		default:
			return false
		}
		body := bodyMap(r)
		ref, _ := body["rowAccessPolicyReference"].(map[string]any)
		if ref == nil {
			ref = map[string]any{}
		}
		ref["projectId"], ref["datasetId"], ref["tableId"] = project, ds, table
		if m[5] != "" {
			ref["policyId"] = unescaped(m[5])
		}
		return refuseCustom(w, r, project, orgpolicy.TypeBigQueryRowAccessPolicy, method, map[string]any{"rowAccessPolicyReference": ref})
	}
	if m := modelsRoute.FindStringSubmatch(path); m != nil && m[4] != "" && r.Method == http.MethodPatch {
		project := unescaped(m[2])
		return refuseCustom(w, r, project, orgpolicy.TypeBigQueryModel, orgpolicy.MethodUpdate, map[string]any{"modelReference": map[string]any{
			"projectId": project, "datasetId": unescaped(m[3]), "modelId": unescaped(m[4])}})
	}
	if m := routinesRoute.FindStringSubmatch(path); m != nil && r.Method == http.MethodPost {
		project := unescaped(m[2])
		ref, _ := bodyMap(r)["routineReference"].(map[string]any)
		if ref == nil {
			ref = map[string]any{}
		}
		ref["projectId"], ref["datasetId"] = project, unescaped(m[3])
		return refuseCustom(w, r, project, orgpolicy.TypeBigQueryRoutine, orgpolicy.MethodCreate, map[string]any{"routineReference": ref})
	}
	if m := routineRoute.FindStringSubmatch(path); m != nil && write {
		project := unescaped(m[2])
		return refuseCustom(w, r, project, orgpolicy.TypeBigQueryRoutine, orgpolicy.MethodUpdate, map[string]any{"routineReference": map[string]any{
			"projectId": project, "datasetId": unescaped(m[3]), "routineId": unescaped(m[4])}})
	}
	m := route.FindStringSubmatch(path)
	if m == nil || m[3] == "" || m[5] != "" {
		return false
	}
	project, ds := unescaped(m[2]), unescaped(m[3])
	tables := strings.HasSuffix(path, "/tables")
	switch {
	case r.Method == http.MethodPost && m[4] == "" && tables:
		ref, _ := bodyMap(r)["tableReference"].(map[string]any)
		if ref == nil {
			ref = map[string]any{}
		}
		ref["projectId"], ref["datasetId"] = project, ds
		return refuseCustom(w, r, project, orgpolicy.TypeBigQueryTable, orgpolicy.MethodCreate, map[string]any{"tableReference": ref})
	case write && m[4] != "":
		return refuseCustom(w, r, project, orgpolicy.TypeBigQueryTable, orgpolicy.MethodUpdate, map[string]any{"tableReference": map[string]any{
			"projectId": project, "datasetId": ds, "tableId": unescaped(m[4])}})
	case write && m[4] == "" && !tables:
		f := front{next: next, base: m[1] + "/projects/" + m[2]}
		status, raw := f.get(r, "/datasets/"+m[3])
		var cur map[string]any
		if status != http.StatusOK || json.Unmarshal(raw, &cur) != nil {
			return false // the update answers it
		}
		body := bodyMap(r)
		if body == nil {
			return false
		}
		after := map[string]any{}
		if r.Method == http.MethodPatch {
			for k, v := range cur {
				after[k] = v
			}
		} else {
			after["datasetReference"], after["location"] = cur["datasetReference"], cur["location"]
		}
		for k, v := range body {
			if v == nil {
				delete(after, k)
				continue
			}
			after[k] = v
		}
		return refuseCustom(w, r, project, orgpolicy.TypeBigQueryDataset, orgpolicy.MethodUpdate, datasetResource(after, project, ds))
	}
	return false
}
