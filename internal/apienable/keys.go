package apienable

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// API keys (#1915). A request that carries an API key (the
// x-goog-api-key header, or the key query parameter) is checked against
// the key's restrictions before it is served, as Google's front end
// checks it: an unknown key is API_KEY_INVALID, a key whose application
// or API restrictions do not allow the call is refused with the reason
// Google gives. The decision is the API Keys server's (KeyChecker); the
// servers in the cluster ask the host at KeyPath.

// KeyCall is what a key is checked against.
type KeyCall struct {
	Key      string `json:"key"`
	Service  string `json:"service"`
	Method   string `json:"method,omitempty"`
	Referrer string `json:"referrer,omitempty"`
	IP       string `json:"ip,omitempty"`
	// AndroidPackage and AndroidCert are the X-Android-Package and
	// X-Android-Cert headers, IOSBundle X-Ios-Bundle-Identifier, which
	// Google's client libraries send for a restricted key.
	AndroidPackage string `json:"androidPackage,omitempty"`
	AndroidCert    string `json:"androidCert,omitempty"`
	IOSBundle      string `json:"iosBundle,omitempty"`
}

// Refusal is a refused call, as Google's error model answers it.
type Refusal struct {
	HTTP     int               `json:"http"`
	Status   string            `json:"status"`
	Message  string            `json:"message"`
	Reason   string            `json:"reason"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

// KeyChecker checks a call's key; nil is a key that allows the call.
type KeyChecker interface {
	CheckKey(ctx context.Context, call KeyCall) *Refusal
}

var globalKeys atomic.Pointer[keyBox]

type keyBox struct{ k KeyChecker }

// UseKeys makes k the key checker of every Gate; nil checks no key.
func UseKeys(k KeyChecker) {
	if k == nil {
		globalKeys.Store(nil)
		return
	}
	globalKeys.Store(&keyBox{k: k})
}

// keys is the checker UseKeys set, else the decider Use set when it is
// one (Remote is both).
func keys() KeyChecker {
	if b := globalKeys.Load(); b != nil {
		return b.k
	}
	if k, ok := global().(KeyChecker); ok {
		return k
	}
	return nil
}

// KeyPath is the host's key check, on the Resource Manager port: POST a
// KeyCall, answered 200 with {} when the key allows the call, else with
// {"refusal": Refusal}.
const KeyPath = "/cloudburrow/v1/apikeys:check"

// KeyHandler serves KeyPath from k.
func KeyHandler(k KeyChecker) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		var call KeyCall
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&call); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(struct {
			Refusal *Refusal `json:"refusal,omitempty"`
		}{k.CheckKey(r.Context(), call)})
	})
}

// CheckKey implements KeyChecker by asking the host, uncached: only a call
// that carries a key is asked about. When the host does not answer, the
// call is refused UNAVAILABLE's 503, as Google refuses what it cannot
// check.
func (c *Remote) CheckKey(ctx context.Context, call KeyCall) *Refusal {
	body, _ := json.Marshal(call)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(c.Base, "/")+KeyPath, bytes.NewReader(body))
	if err != nil {
		return unavailable(err)
	}
	req.Header.Set("Content-Type", "application/json")
	hc := c.Client
	if hc == nil {
		hc = remoteClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return unavailable(err)
	}
	defer resp.Body.Close()
	var out struct {
		Refusal *Refusal `json:"refusal"`
	}
	if resp.StatusCode != http.StatusOK {
		return unavailable(fmt.Errorf("%s", resp.Status))
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return unavailable(err)
	}
	return out.Refusal
}

func unavailable(err error) *Refusal {
	return &Refusal{HTTP: http.StatusServiceUnavailable, Status: "UNAVAILABLE", Reason: "API_KEY_CHECK_UNAVAILABLE",
		Message: "The API key could not be checked: " + err.Error()}
}

// KeyOf is the API key r carries, "" for none.
func KeyOf(r *http.Request) string {
	if k := strings.TrimSpace(r.Header.Get("X-Goog-Api-Key")); k != "" {
		return k
	}
	if IsGRPC(r) {
		return ""
	}
	return strings.TrimSpace(r.URL.Query().Get("key"))
}

// keyCallOf is r's call to service, as a key is checked against it.
func keyCallOf(r *http.Request, service, key string) KeyCall {
	c := KeyCall{Key: key, Service: service, Referrer: r.Header.Get("Referer"),
		AndroidPackage: r.Header.Get("X-Android-Package"), AndroidCert: r.Header.Get("X-Android-Cert"),
		IOSBundle: r.Header.Get("X-Ios-Bundle-Identifier")}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		c.IP = host
	}
	if IsGRPC(r) {
		// "/google.pubsub.v1.Publisher/Publish" is method
		// google.pubsub.v1.Publisher.Publish, as API restrictions name it.
		c.Method = strings.ReplaceAll(strings.TrimPrefix(r.URL.Path, "/"), "/", ".")
	}
	return c
}

// checkKey refuses r when it carries a key its restrictions do not allow.
func checkKey(w http.ResponseWriter, r *http.Request, service string) bool {
	k := keys()
	if k == nil {
		return false
	}
	key := KeyOf(r)
	if key == "" {
		return false
	}
	ref := k.CheckKey(r.Context(), keyCallOf(r, service, key))
	if ref == nil {
		return false
	}
	if IsGRPC(r) {
		ref.WriteGRPC(w)
	} else {
		ref.WriteJSON(w)
	}
	return true
}

// KeyUnaryCheck is the key check of a gRPC call by its metadata, for the
// servers that serve gRPC outside a Gate.
func KeyUnaryCheck(ctx context.Context, service, fullMethod string) error {
	k := keys()
	if k == nil {
		return nil
	}
	md, _ := metadata.FromIncomingContext(ctx)
	get := func(n string) string {
		if v := md.Get(n); len(v) > 0 {
			return v[0]
		}
		return ""
	}
	key := get("x-goog-api-key")
	if key == "" {
		return nil
	}
	ref := k.CheckKey(ctx, KeyCall{Key: key, Service: service,
		Method:         strings.ReplaceAll(strings.TrimPrefix(fullMethod, "/"), "/", "."),
		AndroidPackage: get("x-android-package"), AndroidCert: get("x-android-cert"), IOSBundle: get("x-ios-bundle-identifier")})
	if ref == nil {
		return nil
	}
	return ref.GRPC().Err()
}

func (f *Refusal) code() codes.Code {
	switch f.Status {
	case "UNAVAILABLE":
		return codes.Unavailable
	case "INVALID_ARGUMENT":
		return codes.InvalidArgument
	case "UNAUTHENTICATED":
		return codes.Unauthenticated
	}
	return codes.PermissionDenied
}

// GRPC is the refusal as a gRPC status with its ErrorInfo.
func (f *Refusal) GRPC() *status.Status {
	st := status.New(f.code(), f.Message)
	if d, err := st.WithDetails(
		&errdetails.ErrorInfo{Reason: f.Reason, Domain: Domain, Metadata: f.Metadata},
		&errdetails.LocalizedMessage{Locale: "en-US", Message: f.Message},
	); err == nil {
		return d
	}
	return st
}

// WriteJSON answers the refusal as Google's JSON error body.
func (f *Refusal) WriteJSON(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(f.HTTP)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
		"code": f.HTTP, "message": f.Message, "status": f.Status,
		"details": []any{
			map[string]any{"@type": "type.googleapis.com/google.rpc.ErrorInfo", "reason": f.Reason, "domain": Domain, "metadata": f.Metadata},
			map[string]any{"@type": "type.googleapis.com/google.rpc.LocalizedMessage", "locale": "en-US", "message": f.Message},
		},
	}})
}

// WriteGRPC answers a gRPC call with a trailers-only status.
func (f *Refusal) WriteGRPC(w http.ResponseWriter) {
	st := f.GRPC()
	h := w.Header()
	h.Set("Content-Type", "application/grpc")
	h.Set("Grpc-Status", strconv.Itoa(int(st.Code())))
	h.Set("Grpc-Message", encodeGRPCMessage(st.Message()))
	if b, err := proto.Marshal(st.Proto()); err == nil {
		h.Set("Grpc-Status-Details-Bin", base64.RawStdEncoding.EncodeToString(b))
	}
	w.WriteHeader(http.StatusOK)
}
