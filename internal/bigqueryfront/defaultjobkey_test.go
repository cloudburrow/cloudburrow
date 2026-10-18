package bigqueryfront

import (
	"encoding/json"
	"testing"
)

// TestUploadedJob: a multipart upload's job is its first part (#1456).
func TestUploadedJob(t *testing.T) {
	body := "--b\r\nContent-Type: application/json; charset=UTF-8\r\n\r\n{\"configuration\":{\"load\":{}}}\r\n" +
		"--b\r\nContent-Type: application/octet-stream\r\n\r\n1\n2\n\r\n--b--\r\n"
	if got := string(uploadedJob("multipart/related; boundary=b", []byte(body))); got != `{"configuration":{"load":{}}}` {
		t.Errorf("uploadedJob: %q", got)
	}
	if got := uploadedJob("application/json", []byte(body)); got != nil {
		t.Errorf("uploadedJob of a JSON body: %q, want nil", got)
	}
}

// TestIcebergConfigDefaults: a connection and a storage_uri make a table
// for Apache Iceberg, the formats defaulted (#1456); a storage_uri alone
// is kept.
func TestIcebergConfigDefaults(t *testing.T) {
	raw, _ := json.Marshal(map[string]string{"connectionId": "p.us.c", "storageUri": "gs://b/t"})
	if uri, ok, msg := icebergConfig(raw); !ok || uri != "gs://b/t" || msg != "" {
		t.Errorf("connection and storage_uri: %q %v %q", uri, ok, msg)
	}
	raw, _ = json.Marshal(map[string]string{"storageUri": "gs://b/t"})
	if _, ok, msg := icebergConfig(raw); ok || msg != "" {
		t.Errorf("storage_uri alone: %v %q, want kept", ok, msg)
	}
	raw, _ = json.Marshal(map[string]string{"connectionId": "p.us.c", "storageUri": "gs://b/t", "tableFormat": "DELTA"})
	if _, ok, msg := icebergConfig(raw); ok || msg == "" {
		t.Errorf("table_format DELTA: %v %q, want 400", ok, msg)
	}
}
