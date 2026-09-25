package kms

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"cloud.google.com/go/kms/apiv1/kmspb"

	"github.com/cloudburrow/cloudburrow/internal/store"
)

// Raw JSON through :encrypt and :decrypt (#415): CRCs sent as JSON strings or
// numbers are accepted, int64 fields come back as strings, and a malformed
// body is refused without quoting it.
func TestRESTEncryptDecryptOverRawJSON(t *testing.T) {
	ctx := context.Background()
	srvKMS := NewServer(store.NewMemory())
	c := clientOf(t, srvKMS)
	ring, _ := c.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: loc, KeyRingId: "json"})
	key, err := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "k",
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}})
	if err != nil {
		t.Fatal(err)
	}
	web := httptest.NewServer(NewRESTHandler(srvKMS))
	defer web.Close()
	post := func(path, body string) (int, map[string]any, string) {
		t.Helper()
		resp, err := http.Post(web.URL+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		return resp.StatusCode, m, string(raw)
	}
	pt := []byte("hello")
	crc := crc32.Checksum(pt, crc32.MakeTable(crc32.Castagnoli))
	b64 := base64.StdEncoding.EncodeToString(pt)
	var ciphertext string
	for form, crcJSON := range map[string]string{"string": fmt.Sprintf("%q", fmt.Sprint(crc)), "number": fmt.Sprint(crc)} {
		code, m, raw := post("/v1/"+key.GetName()+":encrypt", `{"plaintext":"`+b64+`","plaintextCrc32c":`+crcJSON+`}`)
		if code != 200 || m["verifiedPlaintextCrc32c"] != true {
			t.Fatalf("CRC as a JSON %s: %d %s", form, code, raw)
		}
		if _, isString := m["ciphertextCrc32c"].(string); !isString {
			t.Errorf("ciphertextCrc32c is %T, want a JSON string", m["ciphertextCrc32c"])
		}
		ciphertext = m["ciphertext"].(string)
	}
	code, m, raw := post("/v1/"+key.GetName()+":decrypt", `{"ciphertext":"`+ciphertext+`"}`)
	if code != 200 || m["plaintext"] != b64 {
		t.Fatalf("decrypt: %d %s", code, raw)
	}
	if _, isString := m["plaintextCrc32c"].(string); !isString {
		t.Errorf("plaintextCrc32c is %T, want a JSON string", m["plaintextCrc32c"])
	}
	for what, body := range map[string]string{
		"a syntax error":   `{"plaintext": "PLAINMARKER`,
		"an unknown field": `{"plaintext":"aGk=","PLAINMARKER":1}`,
		"a wrong type":     `{"plaintext": ["PLAINMARKER"]}`,
	} {
		code, _, raw := post("/v1/"+key.GetName()+":encrypt", body)
		if code != 400 || strings.Contains(raw, "PLAINMARKER") {
			t.Errorf("%s: %d %s; want 400 without the body quoted", what, code, raw)
		}
	}
	if code, _, raw := post("/v1/"+key.GetPrimary().GetName()+":decrypt", `{"ciphertext":"`+ciphertext+`"}`); code != 404 {
		t.Errorf("a version :decrypt = %d %s, want 404", code, raw)
	}
}
