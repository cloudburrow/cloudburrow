package console

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/logfilter"
)

// The Logs Explorer's query editor (#1863) takes the Logging query language
// Google's Logs Explorer takes
// (https://docs.cloud.google.com/logging/docs/view/logging-query-language),
// parsed and evaluated by internal/logfilter, the same code the Logging API's
// filters use (#2026), over the fields the console's entries carry, each
// named as the LogEntry field it stands for (LogEntryFields). A field an
// entry does not carry is missing or defaulted, as the language says.

// LogEntryFields are an entry's fields as Cloud Logging's LogEntry names
// them, flattened to their paths: what the query editor matches, the Fields
// pane counts and an expanded entry shows.
func LogEntryFields(e Entry) map[string]string {
	f := map[string]string{
		"insertId":      strconv.FormatUint(e.ID, 10),
		"timestamp":     e.Timestamp.UTC().Format(time.RFC3339Nano),
		"severity":      string(e.Severity),
		"resource.type": productOf(e.Source),
		"textPayload":   e.Message,
	}
	if e.Source != "" {
		f["resource.labels.source"] = e.Source
	}
	if strings.Contains(e.Resource, "/logs/") {
		f["logName"] = e.Resource
	} else if e.Resource != "" {
		f["resource.labels.name"] = e.Resource
	}
	if e.Project != "" {
		f["resource.labels.project_id"] = e.Project
	}
	if e.OperationID != "" {
		f["operation.id"] = e.OperationID
	}
	return f
}

// logEntryJSON nests LogEntryFields back into the LogEntry shape, for the
// expanded entry.
func logEntryJSON(e Entry) map[string]any {
	out := map[string]any{}
	for path, v := range LogEntryFields(e) {
		parts := strings.Split(path, ".")
		m := out
		for _, p := range parts[:len(parts)-1] {
			next, ok := m[p].(map[string]any)
			if !ok {
				next = map[string]any{}
				m[p] = next
			}
			m = next
		}
		m[parts[len(parts)-1]] = v
	}
	return out
}

// MarshalJSON adds the entry's LogEntry form, so the page shows and copies
// what its query matches.
func (e Entry) MarshalJSON() ([]byte, error) {
	type plain Entry
	return json.Marshal(struct {
		plain
		LogEntry map[string]any `json:"logEntry"`
	}{plain(e), logEntryJSON(e)})
}

// CompileQuery compiles a Logs Explorer query; empty matches everything.
func CompileQuery(q string) (func(Entry) bool, error) {
	f, err := logfilter.Parse(q, logfilter.Options{})
	if err != nil || f == nil {
		return nil, err
	}
	return func(e Entry) bool { return f.Match(logfilter.Map(logEntryJSON(e))) }, nil
}
