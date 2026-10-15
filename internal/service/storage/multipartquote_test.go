package storage

import "testing"

// gcloud 586's apitools sends the multipart boundary in single quotes
// (#1347).
func TestParseMultipartTypeSingleQuotedBoundary(t *testing.T) {
	mt, params, err := parseMultipartType("multipart/related; boundary='===============4081333940775129848=='")
	if err != nil || mt != "multipart/related" || params["boundary"] != "===============4081333940775129848==" {
		t.Fatalf("parseMultipartType = %q, %v, %v", mt, params, err)
	}
	if _, _, err := parseMultipartType("multipart/related; boundary='a\"b'"); err == nil {
		t.Error("a quote inside the boundary was accepted")
	}
}
