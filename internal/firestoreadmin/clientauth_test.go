package firestoreadmin

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestByCallerSendsClientSDKsPastIAM: Security Rules govern a Firebase
// client SDK's documents calls and IAM a server library's, so ByCaller
// sends only the latter to the IAM gate.
func TestByCallerSendsClientSDKsPastIAM(t *testing.T) {
	enc := base64.RawURLEncoding.EncodeToString
	firebase := enc([]byte(`{"alg":"none"}`)) + "." + enc([]byte(`{"iss":"https://securetoken.google.com/p","sub":"alice"}`)) + "."
	doc := "/v1/projects/p/databases/(default)/documents/users/alice"
	for _, c := range []struct {
		name, path, ctype, auth, client, want string
	}{
		{"a Firebase ID token", doc, "", "Bearer " + firebase, "", "client"},
		{"an unauthenticated web SDK", doc, "", "", "gl-js/ fire/12.0.0", "client"},
		{"the web SDK's WebChannel", "/google.firestore.v1.Firestore/Write/channel", "", "", "", "client"},
		{"a gRPC Firebase ID token", "/google.firestore.v1.Firestore/GetDocument", "application/grpc", "Bearer " + firebase, "", "client"},
		{"a Go server client", "/google.firestore.v1.Firestore/GetDocument", "application/grpc", "Bearer ya29.local", "gl-go/1.26 gccl/1.0", "server"},
		{"an OAuth token with a Firebase SDK's header", doc, "", "Bearer ya29.local", "gl-js/ fire/12.0.0", "server"},
		{"no credentials, no SDK", doc, "", "", "", "server"},
		{"a Firebase token on an admin call", "/v1/projects/p/databases", "", "Bearer " + firebase, "", "server"},
	} {
		var got string
		h := ByCaller(
			http.HandlerFunc(func(http.ResponseWriter, *http.Request) { got = "client" }),
			http.HandlerFunc(func(http.ResponseWriter, *http.Request) { got = "server" }))
		r := httptest.NewRequest(http.MethodPost, c.path, nil)
		if c.ctype != "" {
			r.Header.Set("Content-Type", c.ctype)
		}
		if c.auth != "" {
			r.Header.Set("Authorization", c.auth)
		}
		if c.client != "" {
			r.Header.Set("X-Goog-Api-Client", c.client)
		}
		h.ServeHTTP(httptest.NewRecorder(), r)
		if got != c.want {
			t.Errorf("%s: sent to the %s handler, want %s", c.name, got, c.want)
		}
	}
}
