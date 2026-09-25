package kms

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"cloud.google.com/go/kms/apiv1/kmspb"

	"github.com/cloudburrow/cloudburrow/internal/store"
)

// restGet issues a GET and returns the status and decoded JSON body.
func restGet(t *testing.T, url string) (int, map[string]any, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("GET %s: not JSON: %s", url, raw)
	}
	return resp.StatusCode, body, string(raw)
}

// TestRESTReads: the six read bindings are transcoded to the Server methods,
// with the query-parameter policy of #422.
func TestRESTReads(t *testing.T) {
	srv := NewServer(store.NewMemory())
	h := httptest.NewServer(NewRESTHandler(srv))
	defer h.Close()
	ctx := context.Background()
	ring, err := srv.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: loc, KeyRingId: "r"})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"r2", "r3"} {
		if _, err := srv.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: loc, KeyRingId: id}); err != nil {
			t.Fatal(err)
		}
	}
	key, err := srv.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "k",
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}})
	if err != nil {
		t.Fatal(err)
	}
	base := h.URL + "/v1/"

	for _, path := range []string{ring.GetName(), key.GetName(), key.GetName() + "/cryptoKeyVersions/1",
		loc + "/keyRings", ring.GetName() + "/cryptoKeys", key.GetName() + "/cryptoKeyVersions"} {
		if code, _, raw := restGet(t, base+path+"?alt=json&prettyPrint=false"); code != 200 {
			t.Errorf("GET %s = %d %s", path, code, raw)
		}
	}

	// A key in protojson: camelCase, Timestamps as RFC 3339, int64 as a
	// string, enums by name; no snake_case keys.
	_, k, raw := restGet(t, base+key.GetName())
	if _, ok := k["createTime"].(string); !ok || strings.Contains(raw, "create_time") || strings.Contains(raw, "version_template") {
		t.Errorf("GetCryptoKey JSON = %s", raw)
	}
	if prim, _ := k["primary"].(map[string]any); prim["state"] != "ENABLED" || prim["generateTime"] == nil {
		t.Errorf("primary = %v", k["primary"])
	}
	// Raw, and escaped as the GAPIC REST client sends it.
	for _, alt := range []string{"$alt=json;enum-encoding=int", "%24alt=json%3Benum-encoding%3Dint"} {
		_, v, _ := restGet(t, base+key.GetName()+"/cryptoKeyVersions/1?"+alt)
		if v["state"] != float64(kmspb.CryptoKeyVersion_ENABLED) {
			t.Errorf("state with %s = %v, want a number", alt, v["state"])
		}
	}

	// Paging, in both spellings.
	_, p1, _ := restGet(t, base+loc+"/keyRings?pageSize=2")
	tok, _ := p1["nextPageToken"].(string)
	if n := len(p1["keyRings"].([]any)); n != 2 || tok == "" {
		t.Fatalf("first page = %d rings, token %q", n, tok)
	}
	if p1["totalSize"] != float64(3) {
		t.Errorf("totalSize = %v, want 3 (an int32, so a number)", p1["totalSize"])
	}
	_, p2, _ := restGet(t, base+loc+"/keyRings?page_size=2&page_token="+tok)
	if n := len(p2["keyRings"].([]any)); n != 1 || p2["nextPageToken"] != nil {
		t.Errorf("second page = %v", p2)
	}
	if _, one, _ := restGet(t, base+loc+"/keyRings?page_size=1"); len(one["keyRings"].([]any)) != 1 {
		t.Errorf("page_size=1 gave %v", one)
	}

	for _, c := range []struct {
		path   string
		code   int
		status string
		names  string
	}{
		{loc + "/keyRings?bogus=1", 400, "INVALID_ARGUMENT", "bogus"},
		{ring.GetName() + "?pageSize=1", 400, "INVALID_ARGUMENT", "pageSize"}, // Get takes no parameters
		{loc + "/keyRings?fields=name", 501, "UNIMPLEMENTED", "fields"},
		{loc + "/keyRings?$fields=name", 501, "UNIMPLEMENTED", "$fields"},
		{loc + "/keyRings?alt=proto", 501, "UNIMPLEMENTED", "alt"},
		{loc + "/keyRings?pageSize=two", 400, "INVALID_ARGUMENT", "pageSize"},
		{ring.GetName() + "/cryptoKeys?versionView=SIDEWAYS", 400, "INVALID_ARGUMENT", "versionView"},
		{loc + "/keyRings/absent", 404, "NOT_FOUND", "absent"},
		{key.GetName() + "/cryptoKeyVersions/01", 400, "INVALID_ARGUMENT", "01"},
		{"projects/p/locations/global/keyRings/r", 400, "INVALID_ARGUMENT", "project"},
	} {
		code, body, raw := restGet(t, base+c.path)
		e, _ := body["error"].(map[string]any)
		if code != c.code || e["status"] != c.status || !strings.Contains(e["message"].(string), c.names) {
			t.Errorf("GET %s = %d %s; want %d %s naming %q", c.path, code, raw, c.code, c.status, c.names)
		}
	}
	// FULL views are accepted by name and by number.
	for _, q := range []string{"versionView=FULL", "version_view=1"} {
		if code, _, raw := restGet(t, base+ring.GetName()+"/cryptoKeys?"+q); code != 200 {
			t.Errorf("ListCryptoKeys?%s = %d %s", q, code, raw)
		}
	}
}
