package bigqueryfront

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
)

// The project configurations that a new dataset or table takes (#1417,
// projectoptions.go): a dataset made with datasets.insert, given no
// maxTimeTravelHours or storageBillingModel, takes its region's
// default_max_time_travel_hours and default_storage_billing_model
// ("applies to new datasets only"); a table made with tables.insert, given
// no encryptionConfiguration, in a dataset with no
// defaultEncryptionConfiguration, takes its region's default_kms_key_name
// as its encryptionConfiguration.kmsKeyName. As every key here, it is
// recorded, not used: the emulator encrypts nothing.

// datasetInsertDefaults writes the defaults into datasets.insert's body,
// whose location datasetInsertLocation has set.
func datasetInsertDefaults(r *http.Request, settings *projectOptions, project string) {
	if !settings.hasAny("default_max_time_travel_hours") && !settings.hasAny("default_storage_billing_model") {
		return
	}
	b, err := readBody(r)
	if err != nil {
		return
	}
	var body map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if dec.Decode(&body) != nil || body == nil {
		return
	}
	var loc string
	_ = json.Unmarshal(body["location"], &loc)
	changed := false
	for field, option := range map[string]string{"maxTimeTravelHours": "default_max_time_travel_hours",
		"storageBillingModel": "default_storage_billing_model"} {
		if v, ok := body[field]; ok && !isNull(v) {
			continue
		}
		if v := settings.effective(project, loc, option); v != "" {
			body[field], _ = json.Marshal(v)
			changed = true
		}
	}
	if !changed {
		return
	}
	if out, err := json.Marshal(body); err == nil {
		setBody(r, out)
	}
}

// defaultTableKey returns tables.insert's body, raw, with the default key
// written in when one applies (above), and sets it as r's.
func (f front) defaultTableKey(r *http.Request, dataset string, raw []byte) []byte {
	if f.settings == nil || !f.settings.hasAny("default_kms_key_name") {
		return raw
	}
	var body map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if dec.Decode(&body) != nil || body == nil {
		return raw
	}
	if v, ok := body["encryptionConfiguration"]; ok && !isNull(v) {
		return raw
	}
	st, got := f.get(r, "/datasets/"+url.PathEscape(dataset))
	var ds map[string]json.RawMessage
	if st != http.StatusOK || json.Unmarshal(got, &ds) != nil || ds == nil {
		return raw
	}
	if v, ok := ds["defaultEncryptionConfiguration"]; ok && !isNull(v) {
		return raw
	}
	withLocation(ds)
	var loc string
	_ = json.Unmarshal(ds["location"], &loc)
	key := f.settings.effective(projectOf(f.base), loc, "default_kms_key_name")
	if key == "" {
		return raw
	}
	body["encryptionConfiguration"], _ = json.Marshal(map[string]string{"kmsKeyName": key})
	out, err := json.Marshal(body)
	if err != nil {
		return raw
	}
	setBody(r, out)
	return out
}

// default_kms_key_name is "the default Cloud KMS key for encrypting table
// data" (https://cloud.google.com/bigquery/docs/default-configuration),
// whatever makes the table: a lone CREATE [OR REPLACE] TABLE statement
// that names no kms_key_name takes it too (#1456), when the table's
// dataset has no defaultEncryptionConfiguration. The front runs the
// statement, then writes the key into the table it made with
// tables.update, as it writes kms_key_name's (ddloptions.go). A script's
// CREATE TABLE is given it so too, as the front runs each statement in its
// turn (scriptflow.go, tempsession.go), and the destination table a query,
// load or copy job makes is given it by withDefaultJobKey
// (defaultjobkey.go).

// defaultKeyKey marks a request whose CREATE TABLE is being given the
// default key, so that runQuery runs it as it would without one.
type defaultKeyKey struct{}

// createdTable reads sql as one CREATE [OR REPLACE] TABLE statement that
// names no kms_key_name, CLONE or COPY: its table's path, and whether it
// replaces or (ifNotExists) keeps a table that exists.
func createdTable(sql string) (parts []string, replace, ifNotExists, ok bool) {
	return createdTableOf(sql, true)
}

// createdTableOf is createdTable, which with noKey false also reads a
// statement that names a kms_key_name (#1459, collation.go).
func createdTableOf(sql string, noKey bool) (parts []string, replace, ifNotExists, ok bool) {
	t, lexed := lex(sql)
	if !lexed || len(t) < 3 || !t[0].is("CREATE") {
		return nil, false, false, false
	}
	end := len(t)
	if t[end-1].punct(";") {
		end--
	}
	for _, tok := range t[:end] {
		if tok.punct(";") || noKey && tok.is("kms_key_name") || tok.is("CLONE") || tok.is("COPY") || tok.is("LIKE") {
			return nil, false, false, false
		}
	}
	i := 1
	if i+1 < end && t[i].is("OR") && t[i+1].is("REPLACE") {
		replace = true
		i += 2
	}
	if i >= end || !t[i].is("TABLE") {
		return nil, false, false, false
	}
	i++
	if i < end && t[i].is("FUNCTION") {
		return nil, false, false, false
	}
	if i+2 < end && t[i].is("IF") && t[i+1].is("NOT") && t[i+2].is("EXISTS") {
		ifNotExists = true
		i += 3
	}
	parts, _ = path(t[:end], i)
	if len(parts) == 0 || len(parts) > 3 {
		return nil, false, false, false
	}
	return parts, replace, ifNotExists, true
}

// withDefaultTableKey runs q, a lone CREATE TABLE, and gives the table it
// made the default key (above). It reports false, having done nothing,
// when q is not one or no default key is set.
func (f front) withDefaultTableKey(w http.ResponseWriter, r *http.Request, q queryOptions, insert bool) bool {
	if f.settings == nil || !f.settings.hasAny("default_kms_key_name") || r.Context().Value(defaultKeyKey{}) != nil {
		return false
	}
	parts, replace, _, ok := createdTable(q.Query)
	if !ok {
		return false
	}
	project := projectOf(f.base)
	dataset, table := "", parts[len(parts)-1]
	switch len(parts) {
	case 1:
		dataset = defaultDatasetOf(q)
	case 2:
		dataset = parts[0]
	case 3:
		if parts[0] != project {
			return false
		}
		dataset = parts[1]
	}
	if dataset == "" {
		return false
	}
	st, got := f.get(r, "/datasets/"+url.PathEscape(dataset))
	var ds map[string]json.RawMessage
	if st != http.StatusOK || json.Unmarshal(got, &ds) != nil || ds == nil {
		return false
	}
	if v, ok := ds["defaultEncryptionConfiguration"]; ok && !isNull(v) {
		return false
	}
	withLocation(ds)
	var loc string
	_ = json.Unmarshal(ds["location"], &loc)
	key := f.settings.effective(project, loc, "default_kms_key_name")
	if key == "" {
		return false
	}
	existed, _ := f.get(r, tablePath(dataset, table))
	rec := newRecorder()
	f.runQuery(rec, r.WithContext(context.WithValue(r.Context(), defaultKeyKey{}, true)), q, insert)
	if rec.status == http.StatusOK && (existed != http.StatusOK || replace) {
		if st, got := f.get(r, tablePath(dataset, table)); st == http.StatusOK {
			var tb map[string]json.RawMessage
			if json.Unmarshal(got, &tb) == nil && (len(tb["encryptionConfiguration"]) == 0 || isNull(tb["encryptionConfiguration"])) {
				tb["encryptionConfiguration"], _ = json.Marshal(map[string]string{"kmsKeyName": key})
				if body, err := json.Marshal(tb); err == nil {
					f.send(r, http.MethodPut, tablePath(dataset, table), body) // tables.update keeps it, as ddloptions.go's
				}
			}
		}
	}
	rec.copyTo(w)
	return true
}
