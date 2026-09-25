//go:build compat

package compat

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"cloud.google.com/go/kms/apiv1/kmspb"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
)

// TestKMSReadsAgreeOverREST (#422): a missing ring, key and version, and a
// malformed name, give the REST client the code gRPC gives; paging works over
// REST; and the JSON API's query-parameter policy holds on the wire.
//
// unverified: google.cloud.kms.v1.KeyManagementService/GetKeyRing NOT_FOUND: a well-formed name that does not exist
// unverified: google.cloud.kms.v1.KeyManagementService/GetCryptoKeyVersion INVALID_ARGUMENT: a malformed version ID (01)
// unverified: google.cloud.kms.v1.KeyManagementService/ListKeyRings INVALID_ARGUMENT: an unknown query parameter over REST
func TestKMSReadsAgreeOverREST(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	clients := kmsClients(t, h)
	g := clients["grpc"]
	loc := "projects/" + h.Project() + "/locations/global"
	var rings []string
	for _, id := range []string{"page-a", "page-b", "page-c"} {
		r, err := g.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: loc, KeyRingId: id})
		if err != nil {
			t.Fatal(err)
		}
		rings = append(rings, r.GetName())
	}
	key, err := g.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: rings[0], CryptoKeyId: "k",
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}})
	if err != nil {
		t.Fatal(err)
	}

	for name, call := range map[string]func(variant string) error{
		"missing ring": func(v string) error {
			_, err := clients[v].GetKeyRing(ctx, &kmspb.GetKeyRingRequest{Name: loc + "/keyRings/absent"})
			return err
		},
		"missing key": func(v string) error {
			_, err := clients[v].GetCryptoKey(ctx, &kmspb.GetCryptoKeyRequest{Name: rings[0] + "/cryptoKeys/absent"})
			return err
		},
		"missing version": func(v string) error {
			_, err := clients[v].GetCryptoKeyVersion(ctx, &kmspb.GetCryptoKeyVersionRequest{Name: key.GetName() + "/cryptoKeyVersions/9"})
			return err
		},
		"malformed version ID": func(v string) error {
			_, err := clients[v].GetCryptoKeyVersion(ctx, &kmspb.GetCryptoKeyVersionRequest{Name: key.GetName() + "/cryptoKeyVersions/01"})
			return err
		},
	} {
		gc, rc := kmsCode("grpc", call("grpc")), kmsCode("rest", call("rest"))
		if gc != rc || gc == codes.OK {
			t.Errorf("%s: gRPC %s, REST %s; want the same error code", name, gc, rc)
		}
	}

	// Three rings, two to a page, over REST.
	var paged []string
	it := clients["rest"].ListKeyRings(ctx, &kmspb.ListKeyRingsRequest{Parent: loc, PageSize: 2})
	for {
		r, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListKeyRings over REST: %v", err)
		}
		paged = append(paged, r.GetName())
		if len(paged) == 2 && it.PageInfo().Token == "" {
			t.Error("no nextPageToken after the first page of two")
		}
	}
	if strings.Join(paged, ",") != strings.Join(rings, ",") {
		t.Errorf("paged over REST: %v, want %v", paged, rings)
	}

	// On the wire.
	base := "http://" + h.Endpoint(EnvKMS) + "/v1/"
	get := func(path string) (int, map[string]any) {
		t.Helper()
		resp, err := http.Get(base + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		var body map[string]any
		_ = json.Unmarshal(b, &body)
		return resp.StatusCode, body
	}
	if code, _ := get(rings[0] + "?alt=json&prettyPrint=false"); code != 200 {
		t.Errorf("alt=json&prettyPrint=false = %d", code)
	}
	if _, v := get(key.GetName() + "/cryptoKeyVersions/1?%24alt=json%3Benum-encoding%3Dint"); v["state"] != float64(kmspb.CryptoKeyVersion_ENABLED) {
		t.Errorf("enum-encoding=int state = %v, want a number", v["state"])
	}
	if _, p := get(loc + "/keyRings?page_size=1"); len(p["keyRings"].([]any)) != 1 {
		t.Errorf("page_size=1 = %v", p)
	}
	if code, e := get(loc + "/keyRings?bogus=1"); code != 400 || !strings.Contains(e["error"].(map[string]any)["message"].(string), "bogus") {
		t.Errorf("bogus=1 = %d %v; want 400 naming bogus", code, e)
	}
	if code, e := get(loc + "/keyRings?fields=name"); code != 501 || !strings.Contains(e["error"].(map[string]any)["message"].(string), "fields") {
		t.Errorf("fields=name = %d %v; want 501 naming fields", code, e)
	}
	if _, k := get(key.GetName()); k["createTime"] == nil || k["create_time"] != nil {
		t.Errorf("a key over REST = %v; want protojson names", k)
	}
}
