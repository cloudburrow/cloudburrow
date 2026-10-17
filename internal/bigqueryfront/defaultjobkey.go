package bigqueryfront

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
)

// default_kms_key_name and the tables a job makes (#1456). The key is "the
// default Cloud KMS key for encrypting table data"
// (https://cloud.google.com/bigquery/docs/default-configuration), whatever
// makes the table, and projectdefaults.go gives it to tables.insert and a
// lone CREATE TABLE. A query, load or copy job given a destinationTable
// and no destinationEncryptionConfiguration makes its table in the
// emulator alone, so once such a job is done, a destination table it made
// (one that did not exist before it) and that has no
// encryptionConfiguration is given the key with tables.update, as the
// front gives a CREATE TABLE's, when its dataset has no
// defaultEncryptionConfiguration. As every key here, it is recorded, not
// used: the emulator encrypts nothing.

// datasetDefaultKey is the default key a new table of dataset takes, or "".
func (f front) datasetDefaultKey(r *http.Request, dataset string) string {
	if f.settings == nil || !f.settings.hasAny("default_kms_key_name") {
		return ""
	}
	st, got := f.get(r, "/datasets/"+url.PathEscape(dataset))
	var ds map[string]json.RawMessage
	if st != http.StatusOK || json.Unmarshal(got, &ds) != nil || ds == nil {
		return ""
	}
	if v, ok := ds["defaultEncryptionConfiguration"]; ok && !isNull(v) {
		return ""
	}
	withLocation(ds)
	var loc string
	_ = json.Unmarshal(ds["location"], &loc)
	return f.settings.effective(projectOf(f.base), loc, "default_kms_key_name")
}

// giveTableKey writes key into dataset.table's encryptionConfiguration
// when it has none.
func (f front) giveTableKey(r *http.Request, dataset, table, key string) {
	st, got := f.get(r, tablePath(dataset, table))
	if st != http.StatusOK {
		return
	}
	var tb map[string]json.RawMessage
	if json.Unmarshal(got, &tb) != nil || len(tb["encryptionConfiguration"]) != 0 && !isNull(tb["encryptionConfiguration"]) {
		return
	}
	tb["encryptionConfiguration"], _ = json.Marshal(map[string]string{"kmsKeyName": key})
	if body, err := json.Marshal(tb); err == nil {
		f.send(r, http.MethodPut, tablePath(dataset, table), body) // tables.update keeps it, as ddloptions.go's
	}
}

// jobDestination reads jobs.insert's body: the destination table of its
// query, load or copy configuration, when it names one of this project
// and no destinationEncryptionConfiguration, and the job is no dry run.
func jobDestination(body []byte, project string) (dataset, table string, ok bool) {
	var job struct {
		Configuration map[string]json.RawMessage `json:"configuration"`
	}
	if json.Unmarshal(body, &job) != nil || job.Configuration == nil {
		return "", "", false
	}
	var dry bool
	_ = json.Unmarshal(job.Configuration["dryRun"], &dry)
	if dry {
		return "", "", false
	}
	for _, kind := range []string{"query", "load", "copy"} {
		raw, has := job.Configuration[kind]
		if !has {
			continue
		}
		var c struct {
			DestinationTable *struct {
				ProjectID string `json:"projectId"`
				DatasetID string `json:"datasetId"`
				TableID   string `json:"tableId"`
			} `json:"destinationTable"`
			Encryption json.RawMessage `json:"destinationEncryptionConfiguration"`
		}
		if json.Unmarshal(raw, &c) != nil || c.DestinationTable == nil || len(c.Encryption) != 0 && !isNull(c.Encryption) {
			return "", "", false
		}
		d := c.DestinationTable
		if d.DatasetID == "" || d.TableID == "" || d.ProjectID != "" && d.ProjectID != project {
			return "", "", false
		}
		return d.DatasetID, d.TableID, true
	}
	return "", "", false
}

// withDefaultJobKey runs insert, jobs.insert, and gives the destination
// table the job made the default key (above).
func (f front) withDefaultJobKey(w http.ResponseWriter, r *http.Request, insert func(http.ResponseWriter)) {
	// An upload's body (a load's data) and a compressed one are not read.
	if f.settings == nil || !f.settings.hasAny("default_kms_key_name") || r.Method != http.MethodPost ||
		strings.HasPrefix(r.URL.Path, "/upload/") || r.Header.Get("Content-Encoding") != "" {
		insert(w)
		return
	}
	body, err := readBody(r)
	if err != nil {
		insert(w)
		return
	}
	dataset, table, ok := jobDestination(body, projectOf(f.base))
	key := ""
	if ok {
		key = f.datasetDefaultKey(r, dataset)
	}
	if key == "" {
		insert(w)
		return
	}
	existed, _ := f.get(r, tablePath(dataset, table))
	if existed == http.StatusOK {
		insert(w)
		return
	}
	rec := newRecorder()
	insert(rec)
	if rec.status == http.StatusOK {
		var job struct {
			Status struct {
				State       string          `json:"state"`
				ErrorResult json.RawMessage `json:"errorResult"`
			} `json:"status"`
		}
		if json.NewDecoder(bytes.NewReader(rec.body.Bytes())).Decode(&job) == nil && job.Status.State == "DONE" && len(job.Status.ErrorResult) == 0 {
			f.giveTableKey(r, dataset, table, key)
		}
	}
	rec.copyTo(w)
}
