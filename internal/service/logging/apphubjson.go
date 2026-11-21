package logging

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
)

// AppHubOf is the output-only LogEntry.apphub (#2096) of an entry with
// the monitored resource resourceType and labels, as its JSON nests it;
// nil for none.
type AppHubOf func(resourceType string, labels map[string]string) map[string]any

// entriesListPath is ListLogEntries' JSON binding.
const entriesListPath = "/v2/entries:list"

// WithAppHub adds apphub to each entry entries.list answers, from of.
// Google's LogEntry REST reference documents the field ("Output only.
// AppHub application metadata associated with this LogEntry"), and
// googleapis' google/logging/v2/log_entry.proto (last changed 2025-02-28)
// does not have it, so the transcoded answer is completed here, as
// serveSettingsJSON completes Settings. gRPC answers carry no apphub: the
// proto has no field for it.
// Docs re-read 2026-11-21: https://docs.cloud.google.com/logging/docs/reference/v2/rest/v2/LogEntry.
func WithAppHub(next http.Handler, of AppHubOf) http.Handler {
	if of == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != entriesListPath {
			next.ServeHTTP(w, r)
			return
		}
		rec := httptest.NewRecorder()
		next.ServeHTTP(rec, r)
		body := rec.Body.Bytes()
		if rec.Code == http.StatusOK {
			if b, ok := addAppHub(body, of); ok {
				body = b
			}
		}
		for k, v := range rec.Header() {
			w.Header()[k] = v
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(rec.Code)
		_, _ = bytes.NewReader(body).WriteTo(w)
	})
}

// addAppHub is a ListLogEntriesResponse body with each entry's apphub; ok
// is false when none has one.
func addAppHub(body []byte, of AppHubOf) ([]byte, bool) {
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil {
		return nil, false
	}
	var entries []map[string]any
	raw, ok := m["entries"]
	if !ok {
		return nil, false
	}
	// Numbers are kept as written (a payload's large integer too).
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if dec.Decode(&entries) != nil {
		return nil, false
	}
	changed := false
	for _, e := range entries {
		res, _ := e["resource"].(map[string]any)
		typ, _ := res["type"].(string)
		labels := map[string]string{}
		if ls, ok := res["labels"].(map[string]any); ok {
			for k, v := range ls {
				if s, ok := v.(string); ok {
					labels[k] = s
				}
			}
		}
		if a := of(typ, labels); a != nil {
			e["apphub"] = a
			changed = true
		}
	}
	if !changed {
		return nil, false
	}
	raw, err := json.Marshal(entries)
	if err != nil {
		return nil, false
	}
	m["entries"] = raw
	out, err := json.Marshal(m)
	return out, err == nil
}
