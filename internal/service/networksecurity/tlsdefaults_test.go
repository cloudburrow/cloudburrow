package networksecurity

import (
	"encoding/json"
	"testing"
)

// TestTLSInspectionDefaultsNameUnsetEnums (#1578): a TLS inspection
// policy's JSON, alone, listed or in an operation's response, names an
// unset minTlsVersion and tlsFeatureProfile; a set one, and every other
// resource, is left as it is.
func TestTLSInspectionDefaultsNameUnsetEnums(t *testing.T) {
	const p = "projects/p/locations/us-central1/tlsInspectionPolicies/t"
	for _, tc := range []struct{ in, path string }{
		{`{"name":"` + p + `","caPool":"x"}`, ""},
		{`{"tlsInspectionPolicies":[{"name":"` + p + `"}]}`, "list"},
		{`{"name":"op","done":true,"response":{"@type":"t","name":"` + p + `"}}`, "op"},
	} {
		var m map[string]any
		if err := json.Unmarshal(tlsInspectionDefaults([]byte(tc.in)), &m); err != nil {
			t.Fatal(err)
		}
		got := m
		switch tc.path {
		case "list":
			got = m["tlsInspectionPolicies"].([]any)[0].(map[string]any)
		case "op":
			got = m["response"].(map[string]any)
		}
		if got["minTlsVersion"] != "TLS_VERSION_UNSPECIFIED" || got["tlsFeatureProfile"] != "PROFILE_UNSPECIFIED" {
			t.Errorf("%s: %v", tc.in, got)
		}
	}
	set := `{"name":"` + p + `","minTlsVersion":"TLS_1_2","tlsFeatureProfile":"PROFILE_MODERN"}`
	if got := string(tlsInspectionDefaults([]byte(set))); got != set {
		t.Errorf("a set policy changed: %s", got)
	}
	other := `{"name":"projects/p/locations/us-central1/urlLists/u"}`
	if got := string(tlsInspectionDefaults([]byte(other))); got != other {
		t.Errorf("a URL list changed: %s", got)
	}
}
