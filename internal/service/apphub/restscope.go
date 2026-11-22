package apphub

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// restScopeNames names Scope.Type MULTI_REGIONAL (3) on the JSON API,
// which protojson cannot: the generated apphubpb (v1.4.1) has no such
// value, so a request's "MULTI_REGIONAL" would not parse and a response
// would carry the number. Requests have it turned into 3 before they are
// transcoded, and responses have 3 turned back into its name, wherever a
// "scope" object's "type" is it. Remove once cloud.google.com/go/apphub
// ships the value.
func restScopeNames(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil && r.Method != http.MethodGet {
			b, err := io.ReadAll(r.Body)
			_ = r.Body.Close()
			if err == nil && bytes.Contains(b, []byte(`"MULTI_REGIONAL"`)) {
				b = rewriteScope(b, func(v any) (any, bool) {
					if s, ok := v.(string); ok && s == "MULTI_REGIONAL" {
						return json.Number(strconv.Itoa(int(ScopeMultiRegional))), true
					}
					return nil, false
				})
			}
			r.Body = io.NopCloser(bytes.NewReader(b))
			r.ContentLength = int64(len(b))
			r.Header.Del("Content-Length")
		}
		rec := &scopeRecorder{header: http.Header{}, code: http.StatusOK}
		next.ServeHTTP(rec, r)
		body := rec.buf.Bytes()
		if strings.Contains(rec.header.Get("Content-Type"), "json") && bytes.Contains(body, []byte(`"scope"`)) {
			body = rewriteScope(body, func(v any) (any, bool) {
				if n, ok := v.(json.Number); ok && n.String() == strconv.Itoa(int(ScopeMultiRegional)) {
					return "MULTI_REGIONAL", true
				}
				return nil, false
			})
		}
		for k, vs := range rec.header {
			w.Header()[k] = vs
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(rec.code)
		_, _ = w.Write(body)
	})
}

// rewriteScope applies f to the "type" of every "scope" object in the JSON
// document b; b is returned unchanged when nothing changes or it does not
// parse.
func rewriteScope(b []byte, f func(any) (any, bool)) []byte {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var doc any
	if err := d.Decode(&doc); err != nil {
		return b
	}
	changed := false
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, c := range x {
				if sc, ok := c.(map[string]any); ok && k == "scope" {
					if nv, ok := f(sc["type"]); ok {
						sc["type"] = nv
						changed = true
					}
				}
				walk(c)
			}
		case []any:
			for _, c := range x {
				walk(c)
			}
		}
	}
	walk(doc)
	if !changed {
		return b
	}
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(doc); err != nil {
		return b
	}
	return bytes.TrimSuffix(out.Bytes(), []byte("\n"))
}

// scopeRecorder holds a response so its body can be rewritten.
type scopeRecorder struct {
	header http.Header
	code   int
	buf    bytes.Buffer
}

func (r *scopeRecorder) Header() http.Header         { return r.header }
func (r *scopeRecorder) WriteHeader(code int)        { r.code = code }
func (r *scopeRecorder) Write(b []byte) (int, error) { return r.buf.Write(b) }
