package bigqueryfront

import (
	"bytes"
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
