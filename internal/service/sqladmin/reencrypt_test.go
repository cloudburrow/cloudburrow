package sqladmin

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// versionedKMS wraps with its primary version, "v<n>:" before the
// plaintext, and decrypts only with an enabled version.
type versionedKMS struct {
	mu       sync.Mutex
	primary  int
	disabled map[int]bool
}

func (k *versionedKMS) Encrypt(_ context.Context, key string, pt []byte) ([]byte, string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.disabled[k.primary] {
		return nil, "", fmt.Errorf("the primary version of %s is DISABLED", key)
	}
	return append([]byte(fmt.Sprintf("v%d:", k.primary)), pt...), fmt.Sprintf("%s/cryptoKeyVersions/%d", key, k.primary), nil
}

func (k *versionedKMS) Decrypt(_ context.Context, key string, ct []byte) ([]byte, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	var v int
	if _, err := fmt.Sscanf(string(ct), "v%d:", &v); err != nil {
		return nil, fmt.Errorf("not wrapped by %s", key)
	}
	if k.disabled[v] {
		return nil, fmt.Errorf("CryptoKeyVersion %s/cryptoKeyVersions/%d is DISABLED", key, v)
	}
	return ct[strings.IndexByte(string(ct), ':')+1:], nil
}

// TestSQLReencrypt (#1614): reencrypt wraps the instance's data key with
// the key's primary version, after which the old version can be disabled;
// an instance without a key is 400, backupReencryptionConfig 501, and a
// data key whose version is disabled cannot be re-encrypted.
func TestSQLReencrypt(t *testing.T) {
	srv := NewServer("", newFake())
	if err := srv.EnableConnector("127.0.0.1:0", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	srv.EnableInstanceWrites(newFakeProv(), "")
	k := &versionedKMS{primary: 1, disabled: map[int]bool{}}
	srv.UseKMS(k)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	base := ts.URL + "/v1/projects/proj/instances"
	for _, b := range []string{
		`{"name":"enc","databaseVersion":"POSTGRES_17","settings":{"tier":"t"},"diskEncryptionConfiguration":{"kmsKeyName":"` + testKey + `"}}`,
		`{"name":"plain","databaseVersion":"POSTGRES_17","settings":{"tier":"t"}}`,
	} {
		if code, resp := call(t, "POST", base, b); code != 200 {
			t.Fatalf("insert = %d %s", code, resp)
		}
	}
	getRunnable(t, base+"/enc")
	getRunnable(t, base+"/plain")

	if code, resp := call(t, "POST", base+"/plain/reencrypt", ""); code != 400 || !strings.Contains(resp, "customer-managed") {
		t.Errorf("reencrypt without a key = %d %s", code, resp)
	}
	if code, resp := call(t, "POST", base+"/enc/reencrypt", `{"backupReencryptionConfig":{"backupLimit":3}}`); code != 501 ||
		!strings.Contains(resp, "backupReencryptionConfig") {
		t.Errorf("reencrypt with backupReencryptionConfig = %d %s", code, resp)
	}

	k.mu.Lock()
	k.primary = 2
	k.mu.Unlock()
	code, resp := call(t, "POST", base+"/enc/reencrypt", "")
	if code != 200 || !strings.Contains(resp, `"REENCRYPT"`) {
		t.Fatalf("reencrypt = %d %s", code, resp)
	}
	k.mu.Lock()
	k.disabled[1] = true
	k.mu.Unlock()
	in := getRunnable(t, base+"/enc")
	st, _ := in["diskEncryptionStatus"].(map[string]any)
	if in["state"] != "RUNNABLE" || st["kmsKeyVersionName"] != testKey+"/cryptoKeyVersions/2" {
		t.Errorf("after reencrypt, with version 1 disabled: %v %v", in["state"], st)
	}

	k.mu.Lock()
	k.disabled[1], k.disabled[2] = false, true
	k.mu.Unlock()
	if code, resp := call(t, "POST", base+"/enc/reencrypt", ""); code != 400 || !strings.Contains(resp, testKey) {
		t.Errorf("reencrypt with the wrapping version disabled = %d %s", code, resp)
	}
}
