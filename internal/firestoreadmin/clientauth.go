package firestoreadmin

// Who Security Rules apply to (#2048).
//
// Google evaluates Firestore Security Rules for the mobile and web client
// libraries alone: "server client libraries bypass all Cloud Firestore
// Security Rules and instead authenticate through Google Application
// Default Credentials" (firebase.google.com/docs/firestore/security/
// get-started). The emulator decides by the bearer token, as measured on
// this front: with rules set, a call with no Authorization is evaluated as
// an unauthenticated client (request.auth == null), a JWT is evaluated as
// the Firebase user it names, and "Bearer owner" or any other token
// bypasses them.
//
// So the front sends a server library's call as the owner: one with no
// Authorization, or an OAuth token, that does not say it is a Firebase
// client SDK's. A Firebase ID token (issued by
// https://securetoken.google.com/<project>, as Firebase Authentication and
// its emulator issue them), or a call whose x-goog-api-client names a
// Firebase SDK ("fire/<version>", as the Android, Apple and web SDKs send
// it), is passed on as it came, so the emulator enforces the rules the
// Firebase Rules API released (internal/service/firebaserules). The web
// SDK's WebChannel carries its headers in the URL and is never rewritten.

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
)

// firebaseIssuer prefixes a Firebase ID token's issuer.
const firebaseIssuer = "https://securetoken.google.com/"

// ownerToken is the emulator's admin bearer token.
const ownerToken = "Bearer owner"

// ClientAuth sends a server library's documents call as the emulator's
// owner, so only Firebase client SDK calls are evaluated against rules.
func ClientAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isDocumentsCall(r) && !isWebChannel(r) && !isFirebaseClient(r.Header) {
			r.Header.Set("Authorization", ownerToken)
		}
		next.ServeHTTP(w, r)
	})
}

// isWebChannel reports the web SDK's WebChannel transport
// (/google.firestore.v1.Firestore/Listen/channel and Write/channel).
func isWebChannel(r *http.Request) bool {
	return strings.HasPrefix(r.URL.Path, "/google.firestore.v1.Firestore/") && strings.HasSuffix(r.URL.Path, "/channel")
}

// isFirebaseClient reports a call a Firebase client SDK made, or one made
// as a Firebase user.
func isFirebaseClient(h http.Header) bool {
	for _, v := range h.Values("X-Goog-Api-Client") {
		for _, f := range strings.Fields(v) {
			if strings.HasPrefix(f, "fire/") {
				return true
			}
		}
	}
	tok, ok := strings.CutPrefix(h.Get("Authorization"), "Bearer ")
	if !ok {
		return false
	}
	return firebaseIDToken(strings.TrimSpace(tok))
}

// firebaseIDToken reports a JWT whose issuer is Firebase Authentication's.
// The signature is not checked: the emulator does not check it either.
func firebaseIDToken(tok string) bool {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return false
	}
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return false
	}
	var claims struct {
		Iss string `json:"iss"`
	}
	return json.Unmarshal(b, &claims) == nil && strings.HasPrefix(claims.Iss, firebaseIssuer)
}
