package sqladmin

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// migrationType PHYSICAL is decoded, not refused (#1570); an unknown one is
// a 400.
func TestDecodeSyncMigrationType(t *testing.T) {
	for body, want := range map[string]int{
		`{"migrationType":"PHYSICAL"}`: 0, `{"migrationType":"LOGICAL"}`: 0, `{}`: 0,
		`{"migrationType":"BLOCK"}`: 400,
	} {
		_, err := decodeSync(httptest.NewRequest("POST", "/", strings.NewReader(body)))
		got := 0
		if err != nil {
			got = err.(*apiError).code
		}
		if got != want {
			t.Errorf("%s: %v, want %d", body, err, want)
		}
	}
}

func TestPhysicalVersions(t *testing.T) {
	for v, want := range map[string]string{"8.0.46": "8.0", "8.4.11-11": "8.4", "MYSQL_8_0": "8.0", "MYSQL_8_4": "8.4"} {
		if got := versionMajor(v); got != want {
			t.Errorf("versionMajor(%q) = %q, want %q", v, got, want)
		}
	}
	for v, want := range map[string]int{"8.0.46": 46, "8.4.11-11": 11, "8.4": -1, "8.4.x": -1} {
		if got := versionPatch(v); got != want {
			t.Errorf("versionPatch(%q) = %d, want %d", v, got, want)
		}
	}
	if m := physicalInfoVersion.FindStringSubmatch("uuid = x\nserver_version = 8.0.46\nend = y\n"); m == nil || m[1] != "8.0.46" {
		t.Errorf("server_version of xtrabackup_info: %v", m)
	}
}
