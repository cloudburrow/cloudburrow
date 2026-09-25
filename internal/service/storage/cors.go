package storage

import (
	"net/http"
	"strconv"
	"strings"
)

// CORS (#502), to docs.cloud.google.com/storage/docs/cross-origin. XML API
// endpoints allow a cross-origin request only as the bucket's cors
// configuration says: a preflight that matches no rule gets 200 with no
// CORS headers. JSON API endpoints (/storage/v1, uploads, downloads, batch)
// always allow it with default values, whatever the bucket says: the
// request's Origin echoed, the methods DELETE, GET, HEAD, PATCH, POST and
// PUT, the requested headers echoed, and a Max-Age of 3600. A resumable
// session answers with the Origin that started it.

const jsonCORSMethods = "DELETE, GET, HEAD, PATCH, POST, PUT"

// defaultCORSMaxAge is the documented default of maxAgeSeconds, and the
// JSON API's fixed value.
const defaultCORSMaxAge = 3600

type corsRule struct {
	Origins, Methods, Headers []string
	MaxAge                    int64
}

// parseCORS validates a bucket's cors field.
func parseCORS(v any) ([]corsRule, error) {
	if v == nil {
		return nil, nil
	}
	list, ok := v.([]any)
	if !ok {
		return nil, badRequest("Invalid argument: cors must be a list")
	}
	var rules []corsRule
	for i, e := range list {
		m, ok := e.(map[string]any)
		if !ok {
			return nil, badRequest("Invalid argument: cors[%d] must be an object", i)
		}
		var r corsRule
		for k, fv := range m {
			var err error
			switch k {
			case "origin":
				r.Origins, err = corsStrings(i, k, fv)
			case "method":
				r.Methods, err = corsStrings(i, k, fv)
			case "responseHeader":
				r.Headers, err = corsStrings(i, k, fv)
			case "maxAgeSeconds":
				n, perr := strconv.ParseInt(numberString(fv), 10, 64)
				if perr != nil || n < 0 {
					err = badRequest("Invalid argument: cors[%d].maxAgeSeconds %v must be a non-negative integer", i, fv)
				}
				r.MaxAge = n
			default:
				err = badRequest("Invalid argument: cors[%d].%s is not a CORS field", i, k)
			}
			if err != nil {
				return nil, err
			}
		}
		if _, set := m["maxAgeSeconds"]; !set {
			r.MaxAge = defaultCORSMaxAge
		}
		rules = append(rules, r)
	}
	return rules, nil
}

func corsStrings(i int, k string, v any) ([]string, error) {
	out, err := condStrings(k, v)
	if err != nil {
		return nil, badRequest("Invalid argument: cors[%d].%s must be a list of strings", i, k)
	}
	return out, nil
}

// isJSONSurface reports a path the JSON API serves.
func isJSONSurface(path string) bool {
	for _, p := range []string{jsonPrefix, uploadPrefix, resumablePrefix, downloadPrefix} {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return path == batchPath || strings.HasPrefix(path, batchPath+"/")
}

// serveCORS adds CORS headers for a request that carries an Origin, and
// answers a preflight. It reports whether the request is fully answered.
func (s *Server) serveCORS(w http.ResponseWriter, r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return false
	}
	preflight := r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != ""
	h := w.Header()
	h.Add("Vary", "Origin")
	if isJSONSurface(r.URL.EscapedPath()) {
		h.Set("Access-Control-Allow-Origin", s.sessionOrigin(r, origin))
		if !preflight {
			h.Set("Access-Control-Allow-Credentials", "true")
			return false
		}
		h.Set("Access-Control-Allow-Methods", jsonCORSMethods)
		if rh := r.Header.Get("Access-Control-Request-Headers"); rh != "" {
			h.Set("Access-Control-Allow-Headers", rh)
		}
		h.Set("Access-Control-Max-Age", strconv.Itoa(defaultCORSMaxAge))
		w.WriteHeader(http.StatusOK)
		return true
	}
	bucket, _ := s.xmlTarget(r)
	rules := s.bucketCORS(bucket)
	method := r.Method
	if preflight {
		method = r.Header.Get("Access-Control-Request-Method")
	}
	var asked []string
	for _, v := range strings.Split(r.Header.Get("Access-Control-Request-Headers"), ",") {
		if v = strings.TrimSpace(v); v != "" {
			asked = append(asked, v)
		}
	}
	rule, ok := matchCORS(rules, origin, method, asked, preflight)
	if !ok {
		if preflight {
			w.WriteHeader(http.StatusOK)
			return true
		}
		return false
	}
	h.Set("Access-Control-Allow-Origin", origin)
	if preflight {
		h.Set("Access-Control-Allow-Methods", strings.Join(rule.Methods, ", "))
		if len(asked) > 0 {
			h.Set("Access-Control-Allow-Headers", strings.Join(asked, ", "))
		}
		h.Set("Access-Control-Max-Age", strconv.FormatInt(rule.MaxAge, 10))
		w.WriteHeader(http.StatusOK)
		return true
	}
	if len(rule.Headers) > 0 {
		h.Set("Access-Control-Expose-Headers", strings.Join(rule.Headers, ", "))
	}
	return false
}

// matchCORS returns the first rule allowing origin and method, and on a
// preflight every requested header.
func matchCORS(rules []corsRule, origin, method string, headers []string, preflight bool) (corsRule, bool) {
	for _, r := range rules {
		if !anyEqualFold(r.Origins, origin, true) || !anyEqualFold(r.Methods, method, false) {
			continue
		}
		all := true
		if preflight {
			for _, h := range headers {
				if !anyEqualFold(r.Headers, h, false) {
					all = false
					break
				}
			}
		}
		if all {
			return r, true
		}
	}
	return corsRule{}, false
}

// anyEqualFold reports whether list holds v, ignoring case; star lets "*"
// match anything (an origin rule).
func anyEqualFold(list []string, v string, star bool) bool {
	for _, e := range list {
		if star && e == "*" || strings.EqualFold(e, v) {
			return true
		}
	}
	return false
}

func (s *Server) bucketCORS(bucket string) []corsRule {
	var rules []corsRule
	_ = s.meta.View(func(tx Tx) error {
		if b, ok, err := s.getBucket(tx, bucket); err == nil && ok {
			rules, _ = parseCORS(b.Fields["cors"])
		}
		return nil
	})
	return rules
}

// sessionOrigin is the Origin a resumable session was started from, for a
// request to its session URI; otherwise the request's own.
func (s *Server) sessionOrigin(r *http.Request, origin string) string {
	id := r.URL.Query().Get("upload_id")
	if id == "" {
		return origin
	}
	_ = s.meta.View(func(tx Tx) error {
		if u, ok, err := getSession(tx, id); err == nil && ok && u.Origin != "" {
			origin = u.Origin
		}
		return nil
	})
	return origin
}
