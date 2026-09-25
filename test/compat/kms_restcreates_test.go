//go:build compat

package compat

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"cloud.google.com/go/kms/apiv1/kmspb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestKMSCreatesOverREST (#423): the request Terraform sends for a key ring
// (no body), an unknown body field, and an unsupported purpose, which must
// read the same over REST as over gRPC.
//
// unverified: google.cloud.kms.v1.KeyManagementService/CreateCryptoKey INVALID_ARGUMENT: an unknown field in the JSON body
func TestKMSCreatesOverREST(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	clients := kmsClients(t, h)
	loc := "projects/" + h.Project() + "/locations/global"
	base := "http://" + h.Endpoint(EnvKMS) + "/v1/" + loc
	post := func(url, body string) (int, string) {
		t.Helper()
		var rd io.Reader
		if body != "" {
			rd = strings.NewReader(body)
		}
		req, _ := http.NewRequest("POST", url, rd)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, body := post(base+"/keyRings?alt=json&keyRingId=tf-shape", ""); code != 200 {
		t.Fatalf("CreateKeyRing with no body = %d %s", code, body)
	}
	if _, err := clients["grpc"].GetKeyRing(ctx, &kmspb.GetKeyRingRequest{Name: loc + "/keyRings/tf-shape"}); err != nil {
		t.Errorf("the ring created with no body: %v", err)
	}
	if code, body := post(base+"/keyRings/tf-shape/cryptoKeys?cryptoKeyId=x", `{"purpose":"ENCRYPT_DECRYPT","bogus":1}`); code != 400 || !strings.Contains(body, "INVALID_ARGUMENT") {
		t.Errorf("an unknown body field = %d %s; want 400 INVALID_ARGUMENT", code, body)
	}
	var msgs []string
	for _, variant := range []string{"grpc", "rest"} {
		_, err := clients[variant].CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: loc + "/keyRings/tf-shape", CryptoKeyId: "sign",
			CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ASYMMETRIC_SIGN}})
		if kmsCode(variant, err) != codes.Unimplemented {
			t.Errorf("%s: ASYMMETRIC_SIGN = %v, want UNIMPLEMENTED", variant, err)
		}
		msgs = append(msgs, status.Convert(err).Message())
	}
	if !strings.Contains(msgs[0], "crypto_key.purpose") || !strings.Contains(msgs[1], "crypto_key.purpose") {
		t.Errorf("messages %q do not both name crypto_key.purpose", msgs)
	}
	if code, _ := post(base+"/keyRings/tf-shape/cryptoKeys/x:nope", "{}"); code != 404 {
		t.Errorf("an unbound verb = %d, want 404", code)
	}
}

// TestKMSLifecycleOverRESTWire (#424): on the wire, an unknown PATCH body
// field is refused, the delete RPCs and an untranscoded bound verb are 501,
// and a verb nothing binds is 404.
//
// unverified: google.cloud.kms.v1.KeyManagementService/UpdateCryptoKey INVALID_ARGUMENT: an unknown field in the JSON body
func TestKMSLifecycleOverRESTWire(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	g := kmsClients(t, h)["grpc"]
	ring, err := g.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: "projects/" + h.Project() + "/locations/global", KeyRingId: "wire"})
	if err != nil {
		t.Fatal(err)
	}
	key, err := g.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "k",
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}})
	if err != nil {
		t.Fatal(err)
	}
	k := "http://" + h.Endpoint(EnvKMS) + "/v1/" + key.GetName()
	for _, c := range []struct {
		method, url, body string
		want              int
	}{
		{"PATCH", k + "?updateMask=labels", `{"labels":{},"bogus":1}`, 400},
		{"PATCH", k + "/cryptoKeyVersions/1?updateMask=state", `{"state":"DISABLED"}`, 200},
		{"POST", k + "/cryptoKeyVersions/1:destroy", ``, 200},
		{"POST", k + "/cryptoKeyVersions/1:restore", `{}`, 200},
		{"POST", k + "/cryptoKeyVersions/1:asymmetricSign", `{}`, 501},
		{"POST", k + "/cryptoKeyVersions/1:nope", `{}`, 404},
		{"DELETE", k, ``, 501},
		{"DELETE", k + "/cryptoKeyVersions/1", ``, 501},
	} {
		var rd io.Reader
		if c.body != "" {
			rd = strings.NewReader(c.body)
		}
		req, _ := http.NewRequest(c.method, c.url, rd)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != c.want {
			t.Errorf("%s %s = %d %s; want %d", c.method, c.url, resp.StatusCode, b, c.want)
		}
	}
}
