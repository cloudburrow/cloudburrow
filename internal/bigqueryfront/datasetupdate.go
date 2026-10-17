package bigqueryfront

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/resource"
)

// datasets.patch, datasets.update and the dataset's etag (#1205).
//
// The emulator answered both methods with the dataset as sent and saved
// nothing (#1200); bigquery-emulator patch 0010 makes its datasets.update
// save the dataset it is given. The front carries out both methods as that
// datasets.update of the whole dataset:
//
//   - datasets.update "replaces the entire dataset resource"
//     (https://cloud.google.com/bigquery/docs/reference/rest/v2/datasets/update):
//     what the request leaves out is cleared;
//   - datasets.patch "only replaces fields that are provided"
//     (https://cloud.google.com/bigquery/docs/reference/rest/v2/datasets/patch),
//     its labels merged and a label given null removed, as tables.patch's
//     (patchTable, tablepatch.go).
//
// Either way the dataset keeps what the client cannot change: its
// reference, ID, self link, location, kind and creation time; and its
// lastModifiedTime is now.
//
// The emulator keeps no etag, so the front gives one: a hash of the
// dataset's resource as the emulator keeps it, without the etag. It changes
// when a saved field does. datasets.get and both methods answer it, and
// both methods honour an If-Match header (which the Go client's
// Dataset.Update sends with an etag) as BigQuery does: 412 when it is not
// the dataset's etag, and nothing changed.

// datasetRoute is in unserved.go: projects/P/datasets/D, optionally :undelete.

// datasetFixed are the fields an update or patch cannot change.
var datasetFixed = []string{"datasetReference", "id", "selfLink", "location", "kind", "creationTime"}

// datasetEtag is the etag the front gives the dataset whose resource, as
// the emulator answers it, is content.
func datasetEtag(content map[string]json.RawMessage) string {
	keep := map[string]json.RawMessage{}
	for k, v := range content {
		if k != "etag" {
			keep[k] = v
		}
	}
	b, _ := json.Marshal(keep) // sorted keys: the same resource, the same etag
	sum := sha256.Sum256(b)
	return base64.StdEncoding.EncodeToString(sum[:12])
}

// withDatasetEtag returns the dataset resource body with the front's etag,
// and its location (the default for one stored without one, #1371).
func withDatasetEtag(body []byte) ([]byte, bool) {
	var content map[string]json.RawMessage
	if json.Unmarshal(body, &content) != nil || content == nil {
		return nil, false
	}
	withLocation(content) // #1371, location.go: a dataset stored without one is in the default
	etag, _ := json.Marshal(datasetEtag(content))
	content["etag"] = etag
	b, err := json.Marshal(content)
	return b, err == nil
}

// getDataset answers datasets.get with the front's etag.
func (f front) getDataset(w http.ResponseWriter, r *http.Request, dataset string) {
	status, got := f.get(r, "/datasets/"+url.PathEscape(dataset))
	if status != http.StatusOK {
		f.next.ServeHTTP(w, r)
		return
	}
	b, ok := withDatasetEtag(got)
	if !ok {
		f.next.ServeHTTP(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}

// updateDataset carries out a datasets.patch or datasets.update of dataset.
func (f front) updateDataset(w http.ResponseWriter, r *http.Request, dataset string) {
	path := "/datasets/" + url.PathEscape(dataset)
	var patch map[string]json.RawMessage
	_, ok := decode(r, &patch)
	if !ok || patch == nil {
		if ok {
			writeError(w, http.StatusBadRequest, "invalid", "Invalid dataset resource: the request body is not a JSON object.")
			return
		}
		f.next.ServeHTTP(w, r)
		return
	}
	status, got := f.get(r, path)
	var current map[string]json.RawMessage
	if status != http.StatusOK || json.Unmarshal(got, &current) != nil {
		f.next.ServeHTTP(w, r) // the emulator's own answer: 404 for a dataset it does not have
		return
	}
	withLocation(current) // #1371: as datasets.get gives it, so its etag and location hold
	name := projectOf(f.base) + ":" + dataset
	if match := r.Header.Get("If-Match"); match != "" && match != "*" && strings.Trim(match, `"`) != datasetEtag(current) {
		writePreconditionFailed(w, "Precondition check failed. The etag given does not match dataset "+name+"'s etag. Nothing was changed.")
		return
	}
	if msg := checkDatasetUpdate(name, current, patch); msg != "" {
		writeError(w, http.StatusBadRequest, "invalid", msg)
		return
	}
	var next map[string]json.RawMessage
	if r.Method == http.MethodPatch {
		b, ok := patchTable(current, patch) // the same merge as tables.patch
		if !ok || json.Unmarshal(b, &next) != nil {
			writeError(w, http.StatusBadRequest, "invalid", "Invalid dataset resource for "+name+".")
			return
		}
	} else {
		next = map[string]json.RawMessage{}
		for k, v := range patch {
			if !bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
				next[k] = v
			}
		}
	}
	for _, k := range datasetFixed {
		if v, ok := current[k]; ok {
			next[k] = v
		} else {
			delete(next, k)
		}
	}
	if v, ok := next["maxTimeTravelHours"]; ok || r.Method == http.MethodPut {
		// #1503, changehistoryretention.go: the history older than the
		// window it had, or the shorter it gets, is let go of first.
		old, _ := jsonInt(current["maxTimeTravelHours"])
		if old <= 0 {
			old = 168
		}
		h, err := jsonInt(v)
		if !ok || isNull(v) || err != nil || h <= 0 {
			h = 168
		}
		if h != old {
			f.pruneDataset(r, dataset, min(h, old))
		}
	}
	delete(next, "etag")
	next["lastModifiedTime"], _ = json.Marshal(strconv.FormatInt(f.now().UnixMilli(), 10))
	body, err := json.Marshal(next)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internalError", "Could not encode dataset "+name+".")
		return
	}
	status, got = f.send(r, http.MethodPut, path, body)
	if status != http.StatusOK {
		w.Header().Set("Content-Type", "application/json; charset=UTF-8")
		w.WriteHeader(status)
		_, _ = w.Write(got)
		return
	}
	// The dataset as saved, read back, so the answer is what a get reads.
	if s, saved := f.get(r, path); s == http.StatusOK {
		got = saved
	}
	b, ok := withDatasetEtag(got)
	if !ok {
		b = got
	}
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}

// checkDatasetUpdate returns BigQuery's refusal of a datasets.patch or
// datasets.update body, patch, of the dataset name, or "".
func checkDatasetUpdate(name string, current, patch map[string]json.RawMessage) string {
	if v, ok := patch["location"]; ok && !isNull(v) {
		var have, give string
		_ = json.Unmarshal(current["location"], &have)
		if json.Unmarshal(v, &give) == nil && give != "" && !strings.EqualFold(give, have) && have != "" {
			return "Cannot change the location of dataset " + name + " from " + have + " to " + give + "."
		}
	}
	if v, ok := patch["maxTimeTravelHours"]; ok && !isNull(v) {
		h, err := jsonInt(v)
		if err != nil || h < 48 || h > 168 || h%24 != 0 {
			return fmt.Sprintf("Invalid value for maxTimeTravelHours: %s. It must be a multiple of 24 hours between 48 (2 days) and 168 (7 days).", strings.Trim(string(v), `"`))
		}
	}
	if v, ok := patch["defaultTableExpirationMs"]; ok && !isNull(v) {
		ms, err := jsonInt(v)
		if err != nil || ms < 3600000 {
			return fmt.Sprintf("Invalid value for defaultTableExpirationMs: %s. The minimum is 3600000 milliseconds (one hour).", strings.Trim(string(v), `"`))
		}
	}
	if v, ok := patch["defaultPartitionExpirationMs"]; ok && !isNull(v) {
		if ms, err := jsonInt(v); err != nil || ms < 0 {
			return fmt.Sprintf("Invalid value for defaultPartitionExpirationMs: %s.", strings.Trim(string(v), `"`))
		}
	}
	return ""
}

func isNull(v json.RawMessage) bool { return bytes.Equal(bytes.TrimSpace(v), []byte("null")) }

// jsonInt reads an int64 the API sends as a string, or a number.
func jsonInt(v json.RawMessage) (int64, error) {
	var s string
	if json.Unmarshal(v, &s) == nil {
		return strconv.ParseInt(s, 10, 64)
	}
	var n int64
	err := json.Unmarshal(v, &n)
	return n, err
}

// writePreconditionFailed answers 412 as BigQuery does for an If-Match
// that is not the resource's etag.
func writePreconditionFailed(w http.ResponseWriter, message string) {
	writeJSON(w, http.StatusPreconditionFailed, map[string]any{"error": map[string]any{
		"code":    http.StatusPreconditionFailed,
		"message": message,
		"errors":  []map[string]string{{"message": message, "domain": "global", "reason": "conditionNotMet"}},
		"status":  "FAILED_PRECONDITION",
	}})
}

// serviceAccountEmail is the BigQuery service agent of project, which
// projects.getServiceAccount names: bq-PROJECT_NUMBER@bigquery-encryption.iam.gserviceaccount.com,
// as BigQuery names it, with the project number CloudBurrow derives for
// every project (resource.ProjectNumber), the one the metadata server and
// Cloud Storage's service agent use.
func serviceAccountEmail(project string) string {
	return "bq-" + strconv.FormatInt(resource.ProjectNumber(project), 10) + "@bigquery-encryption.iam.gserviceaccount.com"
}

// serveServiceAccount answers projects.getServiceAccount (#1205).
func serveServiceAccount(w http.ResponseWriter, r *http.Request) bool {
	m := serviceAccountRoute.FindStringSubmatch(r.URL.Path)
	if m == nil || r.Method != http.MethodGet {
		return false
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"kind":  "bigquery#getServiceAccountResponse",
		"email": serviceAccountEmail(m[1]),
	})
	return true
}
