package compute

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Cloud CDN (#2160) on backend services (enableCDN) and backend buckets
// (enableCdn), as https://docs.cloud.google.com/cdn/docs/caching,
// .../cdn/docs/using-ttl-overrides, .../using-negative-caching,
// .../using-signed-urls, .../using-signed-cookies,
// .../cache-invalidation-overview and .../cdn/docs/caching#cache-keys
// describe it (read 2026-11-22): a cache in the local proxy, keyed by the
// cache key policy, filled by GET responses that the cache mode, TTLs and
// negative caching make cacheable, served with Age while fresh and, when
// the origin fails, while stale (serveWhileStale), emptied by
// urlMaps.invalidateCache (path, host, cache tags). A backend with signed
// request keys (addSignedUrlKey) serves only signed URLs and signed
// cookies. The {cdn_cache_status} and {cdn_cache_id} custom header
// variables report each response's cache status: hit, miss, stale,
// uncacheable or disabled.

const (
	maxTTL         = 31622400
	maxCacheObject = 10 << 20
	maxCacheBytes  = 256 << 20
	cdnCacheID     = "LOCAL"
)

var cacheModes = []string{"CACHE_ALL_STATIC", "USE_ORIGIN_HEADERS", "FORCE_CACHE_ALL", "INVALID_CACHE_MODE"}

var negativeCodes = map[int]bool{300: true, 301: true, 302: true, 307: true, 308: true, 404: true, 405: true, 410: true,
	421: true, 451: true, 501: true}

// defaultNegativeTTL is negative caching's default TTL by status.
var defaultNegativeTTL = map[int]float64{300: 600, 301: 600, 308: 600, 404: 120, 410: 120, 451: 120, 405: 60, 501: 60}

var cacheableCodes = map[int]bool{200: true, 203: true, 204: true, 206: true, 300: true, 301: true, 302: true, 307: true,
	308: true, 404: true, 405: true, 410: true, 421: true, 451: true, 501: true}

// buildCdnPolicy checks a cdnPolicy and fills Google's defaults when CDN
// is enabled; bucket is a backend bucket's, whose cache key policy has
// only includeHttpHeaders and queryStringWhitelist.
func buildCdnPolicy(cp obj, enabled, bucket bool, old obj) (obj, error) {
	if cp == nil && !enabled {
		return nil, nil
	}
	o := obj{}
	if cp == nil {
		cp = obj{}
	}
	mode, err := oneOf("resource.cdnPolicy.cacheMode", str(cp, "cacheMode"), "CACHE_ALL_STATIC", cacheModes[:3]...)
	if err != nil {
		return nil, err
	}
	o["cacheMode"] = mode
	ttl := func(k string, def float64) (float64, bool, error) {
		v, ok := num(cp, k)
		if !ok {
			return def, false, nil
		}
		if v < 0 || v > maxTTL {
			return 0, true, invalid("Invalid value for field 'resource.cdnPolicy.%s': '%v'. Must be from 0 to %d.", k, v, maxTTL)
		}
		return v, true, nil
	}
	def, defSet, err := ttl("defaultTtl", 3600)
	if err != nil {
		return nil, err
	}
	mx, mxSet, err := ttl("maxTtl", 86400)
	if err != nil {
		return nil, err
	}
	cl, clSet, err := ttl("clientTtl", 3600)
	if err != nil {
		return nil, err
	}
	switch mode {
	case "USE_ORIGIN_HEADERS":
		if defSet || mxSet || clSet {
			return nil, invalid("Invalid value for field 'resource.cdnPolicy.cacheMode': 'USE_ORIGIN_HEADERS'. TTLs cannot be overridden with the USE_ORIGIN_HEADERS cache mode.")
		}
	case "FORCE_CACHE_ALL":
		if mxSet {
			return nil, invalid("Invalid value for field 'resource.cdnPolicy.maxTtl': '%v'. maxTtl cannot be set with the FORCE_CACHE_ALL cache mode.", mx)
		}
		o["defaultTtl"], o["clientTtl"] = def, cl
		if cl > def && clSet {
			return nil, invalid("Invalid value for field 'resource.cdnPolicy.clientTtl': '%v'. clientTtl must not exceed defaultTtl.", cl)
		}
	default:
		if def > mx {
			return nil, invalid("Invalid value for field 'resource.cdnPolicy.defaultTtl': '%v'. defaultTtl must not exceed maxTtl (%v).", def, mx)
		}
		if cl > mx {
			return nil, invalid("Invalid value for field 'resource.cdnPolicy.clientTtl': '%v'. clientTtl must not exceed maxTtl (%v).", cl, mx)
		}
		o["defaultTtl"], o["maxTtl"], o["clientTtl"] = def, mx, cl
	}
	o["negativeCaching"] = cp["negativeCaching"] == true
	if l := listOf(cp["negativeCachingPolicy"]); len(l) > 0 {
		if cp["negativeCaching"] != true {
			return nil, invalid("Invalid value for field 'resource.cdnPolicy.negativeCachingPolicy': ''. negativeCachingPolicy requires negativeCaching.")
		}
		seen := map[int]bool{}
		for i, e := range l {
			p := objOf(e)
			c := int(numOr(p, "code", 0))
			if !negativeCodes[c] {
				return nil, invalid("Invalid value for field 'resource.cdnPolicy.negativeCachingPolicy[%d].code': '%d'. Must be one of 300, 301, 302, 307, 308, 404, 405, 410, 421, 451 or 501.", i, c)
			}
			if seen[c] {
				return nil, invalid("Invalid value for field 'resource.cdnPolicy.negativeCachingPolicy[%d].code': '%d'. Each code can be given once.", i, c)
			}
			seen[c] = true
			if t := numOr(p, "ttl", 0); t < 0 || t > 1800 {
				return nil, invalid("Invalid value for field 'resource.cdnPolicy.negativeCachingPolicy[%d].ttl': '%v'. Must be from 0 to 1800.", i, t)
			}
		}
		o["negativeCachingPolicy"] = deepCopy(cp["negativeCachingPolicy"])
	}
	if v, ok := num(cp, "serveWhileStale"); ok {
		if v < 0 || v > 604800 {
			return nil, invalid("Invalid value for field 'resource.cdnPolicy.serveWhileStale': '%v'. Must be from 0 to 604800.", v)
		}
		o["serveWhileStale"] = v
	} else {
		o["serveWhileStale"] = float64(86400)
	}
	if v, ok := num(cp, "signedUrlCacheMaxAgeSec"); ok {
		o["signedUrlCacheMaxAgeSec"] = v
	} else {
		o["signedUrlCacheMaxAgeSec"] = float64(3600)
	}
	o["requestCoalescing"] = cp["requestCoalescing"] != false
	if l := listOf(cp["bypassCacheOnRequestHeaders"]); len(l) > 0 {
		if len(l) > 5 {
			return nil, invalid("Invalid value for field 'resource.cdnPolicy.bypassCacheOnRequestHeaders': ''. At most 5 headers.")
		}
		o["bypassCacheOnRequestHeaders"] = deepCopy(cp["bypassCacheOnRequestHeaders"])
	}
	ck := objOf(cp["cacheKeyPolicy"])
	if bucket {
		if ck != nil {
			for k := range ck {
				if k != "includeHttpHeaders" && k != "queryStringWhitelist" {
					return nil, invalid("Invalid value for field 'resource.cdnPolicy.cacheKeyPolicy.%s': ''. A backend bucket's cache key policy has only includeHttpHeaders and queryStringWhitelist.", k)
				}
			}
			o["cacheKeyPolicy"] = deepCopy(ck)
		}
	} else {
		k := obj{"includeHost": true, "includeProtocol": true, "includeQueryString": true}
		if ck != nil {
			for _, f := range []string{"includeHost", "includeProtocol", "includeQueryString"} {
				if v, ok := ck[f].(bool); ok {
					k[f] = v
				}
			}
			wl, bl := strs(ck, "queryStringWhitelist"), strs(ck, "queryStringBlacklist")
			if len(wl) > 0 && len(bl) > 0 {
				return nil, invalid("Invalid value for field 'resource.cdnPolicy.cacheKeyPolicy': ''. queryStringWhitelist and queryStringBlacklist cannot both be set.")
			}
			if (len(wl) > 0 || len(bl) > 0) && k["includeQueryString"] != true {
				return nil, invalid("Invalid value for field 'resource.cdnPolicy.cacheKeyPolicy.includeQueryString': 'false'. Query string lists require includeQueryString.")
			}
			copyFields(k, ck, "queryStringWhitelist", "queryStringBlacklist", "includeHttpHeaders", "includeNamedCookies")
		}
		o["cacheKeyPolicy"] = k
	}
	if old != nil {
		if names := strs(objOf(old["cdnPolicy"]), "signedUrlKeyNames"); len(names) > 0 {
			o["signedUrlKeyNames"] = toAny(names)
		}
	}
	return o, nil
}

func cdnOn(o obj) bool {
	if str(o, "kind") == "compute#backendBucket" {
		return o["enableCdn"] == true
	}
	return o["enableCDN"] == true
}

var keyNameRE = regexp.MustCompile(`^[a-z]([-a-z0-9]{0,61}[a-z0-9])?$`)

// addSignedURLKey is addSignedUrlKey: a 128-bit key, base64url-encoded;
// at most 3 per backend.
func addSignedURLKey(s *Server, x lbCtx, o, b obj) (obj, error) {
	name, val := str(b, "keyName"), str(b, "keyValue")
	if !keyNameRE.MatchString(name) {
		return nil, invalid("Invalid value for field 'signedUrlKey.keyName': '%s'. Must be a match of regex '[a-z]([-a-z0-9]*[a-z0-9])?' of 1 to 63 characters.", name)
	}
	raw, err := base64.URLEncoding.DecodeString(val)
	if err != nil {
		raw, err = base64.RawURLEncoding.DecodeString(val)
	}
	if err != nil || len(raw) != 16 {
		return nil, invalid("Invalid value for field 'signedUrlKey.keyValue': ''. Must be a 128-bit key, base64url-encoded.")
	}
	keys := objOf(o["_signedUrlKeys"])
	if _, ok := keys[name]; ok {
		return nil, errf(http.StatusConflict, "alreadyExists", "The key '%s' already exists on '%s'.", name, x.rel)
	}
	if len(keys) >= 3 {
		return nil, invalid("Invalid value for field 'signedUrlKey': ''. A backend can have at most 3 signed request keys.")
	}
	return s.lbSet(x, o, "addSignedUrlKey", func(nw obj) error {
		k := objOf(nw["_signedUrlKeys"])
		if k == nil {
			k = obj{}
		}
		k[name] = base64.RawURLEncoding.EncodeToString(raw)
		nw["_signedUrlKeys"] = k
		cp := objOf(nw["cdnPolicy"])
		if cp == nil {
			cp = obj{}
			nw["cdnPolicy"] = cp
		}
		cp["signedUrlKeyNames"] = toAny(sortedKeys(k))
		return nil
	})
}

func deleteSignedURLKey(s *Server, x lbCtx, o, b obj) (obj, error) {
	name := str(b, "_keyName")
	if name == "" {
		return nil, required("keyName")
	}
	if _, ok := objOf(o["_signedUrlKeys"])[name]; !ok {
		return nil, errf(http.StatusNotFound, "notFound", "The key '%s' was not found on '%s'.", name, x.rel)
	}
	return s.lbSet(x, o, "deleteSignedUrlKey", func(nw obj) error {
		k := objOf(nw["_signedUrlKeys"])
		delete(k, name)
		cp := objOf(nw["cdnPolicy"])
		if len(k) == 0 {
			delete(nw, "_signedUrlKeys")
			if cp != nil {
				delete(cp, "signedUrlKeyNames")
			}
		} else if cp != nil {
			cp["signedUrlKeyNames"] = toAny(sortedKeys(k))
		}
		return nil
	})
}

func sortedKeys(m obj) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// signParams are the signed request's query parameters, not passed to the
// origin nor part of the cache key.
var signParams = map[string]bool{"Expires": true, "KeyName": true, "Signature": true, "URLPrefix": true}

// cdnSignedOK checks a request to a backend with signed request keys: a
// signed URL (Expires, KeyName, Signature; or URLPrefix, Expires,
// KeyName, Signature) or the Cloud-CDN-Cookie signed cookie, signed with
// HMAC-SHA1 by one of the backend's keys and not expired.
func cdnSignedOK(r *http.Request, keys obj, now time.Time, scheme string) bool {
	q := r.URL.Query()
	full := scheme + "://" + r.Host + r.URL.Path
	check := func(signed, name, sigv, expires, prefix string) bool {
		k, ok := keys[name].(string)
		if !ok {
			return false
		}
		key, err := base64.RawURLEncoding.DecodeString(k)
		if err != nil {
			return false
		}
		exp, err := strconv.ParseInt(expires, 10, 64)
		if err != nil || now.Unix() > exp {
			return false
		}
		if prefix != "" {
			p, err := base64.URLEncoding.DecodeString(prefix)
			if err != nil {
				return false
			}
			if !strings.HasPrefix(full+queryString(r), string(p)) && !strings.HasPrefix(full, string(p)) {
				return false
			}
		}
		mac := hmac.New(sha1.New, key)
		mac.Write([]byte(signed))
		want := mac.Sum(nil)
		got, err := base64.URLEncoding.DecodeString(sigv)
		if err != nil {
			got, err = base64.RawURLEncoding.DecodeString(sigv)
		}
		return err == nil && hmac.Equal(want, got)
	}
	if sigv := q.Get("Signature"); sigv != "" {
		raw := r.URL.RawQuery
		i := strings.Index(raw, "&Signature=")
		if i < 0 {
			return false
		}
		signed := full + "?" + raw[:i]
		if p := q.Get("URLPrefix"); p != "" {
			signed = raw[:i]
			return check(signed, q.Get("KeyName"), sigv, q.Get("Expires"), p)
		}
		return check(signed, q.Get("KeyName"), sigv, q.Get("Expires"), "")
	}
	if c, err := r.Cookie("Cloud-CDN-Cookie"); err == nil {
		v := c.Value
		i := strings.Index(v, ":Signature=")
		if i < 0 {
			return false
		}
		f := map[string]string{}
		for _, kv := range strings.Split(v, ":") {
			if k, val, ok := strings.Cut(kv, "="); ok {
				f[k] = val
			}
		}
		return check(v[:i], f["KeyName"], f["Signature"], f["Expires"], f["URLPrefix"])
	}
	return false
}

func queryString(r *http.Request) string {
	if r.URL.RawQuery == "" {
		return ""
	}
	return "?" + r.URL.RawQuery
}

// cdnStripSigned removes the signing parameters from a request's URL.
func cdnStripSigned(r *http.Request) {
	q := r.URL.Query()
	changed := false
	for k := range signParams {
		if q.Has(k) {
			q.Del(k)
			changed = true
		}
	}
	if changed {
		r.URL.RawQuery = q.Encode()
	}
}

// cdnKey is a request's cache key under the backend's cache key policy.
func cdnKey(bs obj, r *http.Request, scheme string) string {
	cp := objOf(bs["cdnPolicy"])
	ck := objOf(cp["cacheKeyPolicy"])
	var b strings.Builder
	b.WriteString(str(bs, "selfLink"))
	b.WriteString("|")
	bucket := str(bs, "kind") == "compute#backendBucket"
	q := r.URL.Query()
	keep := func(k string) bool { return !signParams[k] }
	if bucket {
		wl := strs(ck, "queryStringWhitelist")
		keep = func(k string) bool { return k == "generation" || hasStr(wl, k) }
	} else {
		if ck["includeProtocol"] != false {
			b.WriteString(scheme + "://")
		}
		if ck["includeHost"] != false {
			b.WriteString(strings.ToLower(r.Host))
		}
		wl, bl := strs(ck, "queryStringWhitelist"), strs(ck, "queryStringBlacklist")
		switch {
		case ck["includeQueryString"] == false:
			keep = func(string) bool { return false }
		case len(wl) > 0:
			keep = func(k string) bool { return hasStr(wl, k) }
		case len(bl) > 0:
			keep = func(k string) bool { return !hasStr(bl, k) && !signParams[k] }
		}
	}
	b.WriteString(r.URL.EscapedPath())
	var keys []string
	for k := range q {
		if keep(k) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for i, k := range keys {
		if i == 0 {
			b.WriteString("?")
		} else {
			b.WriteString("&")
		}
		vs := q[k]
		sort.Strings(vs)
		b.WriteString(url.QueryEscape(k) + "=" + url.QueryEscape(strings.Join(vs, ",")))
	}
	for _, h := range strs(ck, "includeHttpHeaders") {
		b.WriteString("|" + strings.ToLower(h) + "=" + r.Header.Get(h))
	}
	for _, c := range strs(ck, "includeNamedCookies") {
		v := ""
		if ck, err := r.Cookie(c); err == nil {
			v = ck.Value
		}
		b.WriteString("|cookie:" + c + "=" + v)
	}
	return b.String()
}

// cdnEntry is one cached response.
type cdnEntry struct {
	status     int
	header     http.Header
	body       []byte
	stored     time.Time
	fresh      time.Duration
	stale      time.Duration
	urlMap     string
	host, path string
	tags       []string
	varyKeys   string
	size       int
}

// lbCDN is the proxy's cache.
type lbCDN struct {
	mu    sync.Mutex
	m     map[string]*cdnEntry
	bytes int
}

func (c *lbCDN) get(k string) *cdnEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.m[k]
}

func (c *lbCDN) put(k string, e *cdnEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]*cdnEntry{}
	}
	if old := c.m[k]; old != nil {
		c.bytes -= old.size
	}
	e.size = len(e.body) + 512
	for c.bytes+e.size > maxCacheBytes && len(c.m) > 0 {
		// Evict the oldest entry.
		var oldest string
		var at time.Time
		for kk, ee := range c.m {
			if oldest == "" || ee.stored.Before(at) {
				oldest, at = kk, ee.stored
			}
		}
		c.bytes -= c.m[oldest].size
		delete(c.m, oldest)
	}
	c.m[k] = e
	c.bytes += e.size
}

func (c *lbCDN) drop(k string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e := c.m[k]; e != nil {
		c.bytes -= e.size
		delete(c.m, k)
	}
}

// invalidate removes the entries of a URL map that a cache invalidation
// rule matches: its path (exact, or a prefix ending in *), host and cache
// tags.
func (c *lbCDN) invalidate(urlMap string, rule obj) int {
	path, host, tags := str(rule, "path"), strings.ToLower(str(rule, "host")), strs(rule, "cacheTags")
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for k, e := range c.m {
		if e.urlMap != urlMap {
			continue
		}
		if host != "" && !strings.EqualFold(hostOnly(e.host), hostOnly(host)) {
			continue
		}
		if path != "" {
			if strings.HasSuffix(path, "*") {
				if !strings.HasPrefix(e.path, strings.TrimSuffix(path, "*")) {
					continue
				}
			} else if e.path != path {
				continue
			}
		}
		if len(tags) > 0 {
			hit := false
			for _, t := range tags {
				if hasStr(e.tags, t) {
					hit = true
				}
			}
			if !hit {
				continue
			}
		}
		c.bytes -= e.size
		delete(c.m, k)
		n++
	}
	return n
}

func hostOnly(h string) string {
	if i := strings.LastIndex(h, ":"); i > 0 && !strings.Contains(h[i:], "]") {
		return h[:i]
	}
	return h
}

// cacheDirectives are a Cache-Control header's directives.
func cacheDirectives(h http.Header) map[string]string {
	out := map[string]string{}
	for _, v := range h.Values("Cache-Control") {
		for _, d := range strings.Split(v, ",") {
			d = strings.TrimSpace(strings.ToLower(d))
			if d == "" {
				continue
			}
			k, val, _ := strings.Cut(d, "=")
			out[k] = strings.Trim(val, `"`)
		}
	}
	return out
}

var allowedVary = map[string]bool{"accept": true, "accept-encoding": true, "access-control-request-headers": true,
	"access-control-request-method": true, "available-dictionary": true, "origin": true, "sec-fetch-dest": true,
	"sec-fetch-mode": true, "sec-fetch-site": true, "x-goog-allowed-resources": true, "x-origin": true}

func isStaticType(ct string) bool {
	ct = strings.ToLower(strings.TrimSpace(strings.Split(ct, ";")[0]))
	switch {
	case ct == "text/css", ct == "text/ecmascript", ct == "text/javascript", ct == "application/javascript",
		ct == "application/pdf", ct == "application/postscript":
		return true
	case strings.HasPrefix(ct, "font/"), strings.HasPrefix(ct, "image/"), strings.HasPrefix(ct, "video/"), strings.HasPrefix(ct, "audio/"):
		return true
	}
	return false
}

// cdnTTL is how long a response is fresh in the cache; ok is false when
// it is not cacheable.
func cdnTTL(cp obj, req *http.Request, status int, h http.Header, now time.Time) (time.Duration, bool) {
	if req.Method != http.MethodGet || !cacheableCodes[status] {
		return 0, false
	}
	mode := str(cp, "cacheMode")
	if mode == "" {
		mode = "CACHE_ALL_STATIC"
	}
	rd := cacheDirectives(req.Header)
	if _, ok := rd["no-store"]; ok {
		return 0, false
	}
	if h.Get("Set-Cookie") != "" {
		return 0, false
	}
	keyHeaders := map[string]bool{}
	for _, k := range strs(objOf(cp["cacheKeyPolicy"]), "includeHttpHeaders") {
		keyHeaders[strings.ToLower(k)] = true
	}
	for _, v := range h.Values("Vary") {
		for _, f := range strings.Split(v, ",") {
			f = strings.ToLower(strings.TrimSpace(f))
			if f != "" && !allowedVary[f] && !keyHeaders[f] {
				return 0, false
			}
		}
	}
	d := cacheDirectives(h)
	_, noStore := d["no-store"]
	_, private := d["private"]
	_, public := d["public"]
	_, smax := d["s-maxage"]
	if req.Header.Get("Authorization") != "" && !public && !smax {
		return 0, false
	}
	origin, hasOrigin := originTTL(d, h, now)
	negative := func() (time.Duration, bool) {
		if cp["negativeCaching"] != true || status < 300 {
			return 0, false
		}
		if l := listOf(cp["negativeCachingPolicy"]); len(l) > 0 {
			for _, e := range l {
				if int(numOr(objOf(e), "code", 0)) == status {
					return time.Duration(numOr(objOf(e), "ttl", 0)) * time.Second, true
				}
			}
			return 0, false
		}
		if t, ok := defaultNegativeTTL[status]; ok {
			return time.Duration(t) * time.Second, true
		}
		return 0, false
	}
	defTTL := time.Duration(numOr(cp, "defaultTtl", 3600)) * time.Second
	maxT := time.Duration(numOr(cp, "maxTtl", 86400)) * time.Second
	switch mode {
	case "FORCE_CACHE_ALL":
		if status >= 300 && status != 304 {
			return negative()
		}
		return defTTL, defTTL > 0
	case "USE_ORIGIN_HEADERS":
		if noStore || private {
			return 0, false
		}
		if hasOrigin {
			return origin, origin > 0
		}
		if t, ok := negative(); ok {
			return t, true
		}
		return 0, false
	default:
		if noStore || private {
			return 0, false
		}
		if hasOrigin {
			if origin > maxT {
				origin = maxT
			}
			return origin, origin > 0
		}
		if t, ok := negative(); ok {
			return t, true
		}
		if status == http.StatusOK && isStaticType(h.Get("Content-Type")) {
			return defTTL, defTTL > 0
		}
		return 0, false
	}
}

// originTTL is the origin's freshness: s-maxage, then max-age, then
// Expires.
func originTTL(d map[string]string, h http.Header, now time.Time) (time.Duration, bool) {
	for _, k := range []string{"s-maxage", "max-age"} {
		if v, ok := d[k]; ok {
			if n, err := strconv.Atoi(v); err == nil {
				age, _ := strconv.Atoi(h.Get("Age"))
				return time.Duration(n-age) * time.Second, true
			}
		}
	}
	if e := h.Get("Expires"); e != "" {
		if t, err := http.ParseTime(e); err == nil {
			return t.Sub(now), true
		}
		return 0, true
	}
	return 0, false
}

// clientCacheControl rewrites a cached response's Cache-Control for the
// client by the cache mode and client TTL.
func clientCacheControl(cp obj, h http.Header, fresh time.Duration) {
	cl := int(numOr(cp, "clientTtl", 3600))
	switch str(cp, "cacheMode") {
	case "FORCE_CACHE_ALL":
		ttl := int(fresh.Seconds())
		if ttl > cl {
			ttl = cl
		}
		h.Set("Cache-Control", "public,max-age="+strconv.Itoa(ttl))
		h.Del("Expires")
	case "USE_ORIGIN_HEADERS":
	default:
		d := cacheDirectives(h)
		if v, ok := d["max-age"]; ok {
			if n, err := strconv.Atoi(v); err == nil && n > cl {
				h.Set("Cache-Control", strings.Replace(h.Get("Cache-Control"), "max-age="+v, "max-age="+strconv.Itoa(cl), 1))
			}
		} else if e := h.Get("Expires"); e != "" {
			h.Del("Expires")
			ttl := int(fresh.Seconds())
			if ttl > cl {
				ttl = cl
			}
			h.Set("Cache-Control", "public,max-age="+strconv.Itoa(ttl))
		}
	}
}

// cdnWriter fills the cache as a response is sent, and fills the
// {cdn_cache_status} and {cdn_cache_id} header variables.
type cdnWriter struct {
	http.ResponseWriter
	status   string
	decide   func(code int, h http.Header) string
	code     int
	buf      bytes.Buffer
	over     bool
	wrote    bool
	capture  bool
	swallow  bool
	stale    func()
	onFinish func(w *cdnWriter)
}

func (w *cdnWriter) WriteHeader(code int) {
	if w.wrote {
		return
	}
	w.wrote = true
	w.code = code
	if w.decide != nil {
		w.status = w.decide(code, w.Header())
	}
	if w.swallow {
		return
	}
	fillCacheVars(w.Header(), w.status)
	w.ResponseWriter.WriteHeader(code)
}

func (w *cdnWriter) Write(b []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	if w.swallow {
		return len(b), nil
	}
	if w.capture && !w.over {
		if w.buf.Len()+len(b) > maxCacheObject {
			w.over = true
			w.buf.Reset()
		} else {
			w.buf.Write(b)
		}
	}
	return w.ResponseWriter.Write(b)
}

func (w *cdnWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// fillCacheVars replaces the cache variables in a response's headers.
func fillCacheVars(h http.Header, status string) {
	for k, vs := range h {
		for i, v := range vs {
			if strings.Contains(v, "{cdn_cache_") {
				vs[i] = strings.NewReplacer("{cdn_cache_status}", status, "{cdn_cache_id}", cdnCacheID).Replace(v)
			}
		}
		h[k] = vs
	}
}

// invalidateCacheOn is urlMaps.invalidateCache: the matching entries of
// the URL map's backends leave the cache, and the operation is done.
func invalidateCacheOn(s *Server, x lbCtx, o, b obj) (obj, error) {
	if str(b, "path") == "" && str(b, "host") == "" && b["cacheTags"] == nil {
		return nil, required("cacheInvalidationRule.path")
	}
	if p := str(b, "path"); p != "" && !strings.HasPrefix(p, "/") {
		return nil, invalid("Invalid value for field 'cacheInvalidationRule.path': '%s'. A path must begin with /.", p)
	}
	if i := strings.Index(str(b, "path"), "*"); i >= 0 && i != len(str(b, "path"))-1 {
		return nil, invalid("Invalid value for field 'cacheInvalidationRule.path': '%s'. A * may only end a path.", str(b, "path"))
	}
	if len(strs(b, "cacheTags")) > 10 {
		return nil, invalid("Invalid value for field 'cacheInvalidationRule.cacheTags': ''. At most 10 cache tags.")
	}
	s.lb.cdn.invalidate(x.rel, b)
	op, err := s.done(x.project, "global", "invalidateCache", x.rel, o["id"])
	if err == nil {
		// The operation's description carries the rule, as Google's does.
		rule := obj{}
		copyFields(rule, b, "path", "host", "cacheTags")
		op["description"] = jsonString(rule)
	}
	return op, err
}

// cdnFront is Cloud CDN in front of a backend: with CDN off, a writer
// that reports "disabled"; with it on, the signed request check, then a
// fresh cached response (done true), else a writer that fills the cache
// from the origin's response, and serves the stale entry if the origin
// fails within serveWhileStale.
func (h *lbHTTP) cdnFront(w http.ResponseWriter, r *http.Request, so obj, urlMap, scheme string) (*cdnWriter, bool) {
	s := h.s
	cw := &cdnWriter{ResponseWriter: w, status: "disabled"}
	if !cdnOn(so) {
		return cw, false
	}
	now := s.opts.Now()
	if keys := objOf(so["_signedUrlKeys"]); len(keys) > 0 {
		if !cdnSignedOK(r, keys, now, scheme) {
			armorDeny(w, http.StatusForbidden, "")
			return nil, true
		}
		cdnStripSigned(r)
	}
	cp := objOf(so["cdnPolicy"])
	cw.status = "uncacheable"
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return cw, false
	}
	for _, e := range listOf(cp["bypassCacheOnRequestHeaders"]) {
		if r.Header.Get(str(objOf(e), "headerName")) != "" {
			return cw, false
		}
	}
	key := cdnKey(so, r, scheme)
	e := s.lb.cdn.get(key)
	if e != nil {
		age := now.Sub(e.stored)
		if age < e.fresh {
			serveEntry(w, r, cp, e, age, "hit")
			return nil, true
		}
		if age >= e.fresh+e.stale {
			s.lb.cdn.drop(key)
			e = nil
		}
	}
	cw.status = "miss"
	ranged := r.Header.Get("Range") != ""
	cw.decide = func(code int, hdr http.Header) string {
		if e != nil && code >= 500 {
			// The origin failed: serve the stale entry instead.
			cw.swallow = true
			return "stale"
		}
		ttl, ok := cdnTTL(cp, r, code, hdr, now)
		if !ok || ranged || r.Method != http.MethodGet {
			return "uncacheable"
		}
		cw.capture = true
		stale := time.Duration(numOr(cp, "serveWhileStale", 86400)) * time.Second
		d := cacheDirectives(hdr)
		if _, ok := d["must-revalidate"]; ok {
			stale = 0
		}
		if _, ok := d["s-maxage"]; ok {
			stale = 0
		}
		snap := hdr.Clone()
		snap.Del("Via")
		cw.onFinish = func(cw *cdnWriter) {
			if cw.over {
				return
			}
			var tags []string
			for _, v := range snap.Values("Cache-Tag") {
				for _, t := range strings.Split(v, ",") {
					if t = strings.TrimSpace(t); t != "" {
						tags = append(tags, t)
					}
				}
			}
			s.lb.cdn.put(key, &cdnEntry{status: code, header: snap, body: append([]byte(nil), cw.buf.Bytes()...), stored: now,
				fresh: ttl, stale: stale, urlMap: urlMap, host: r.Host, path: r.URL.Path, tags: tags})
		}
		clientCacheControl(cp, hdr, ttl)
		return "miss"
	}
	cw.stale = func() {
		serveEntry(w, r, cp, e, s.opts.Now().Sub(e.stored), "stale")
	}
	return cw, false
}

// serveEntry answers a request from a cached response, with its Age.
func serveEntry(w http.ResponseWriter, r *http.Request, cp obj, e *cdnEntry, age time.Duration, status string) {
	hd := w.Header()
	for k, v := range e.header {
		hd[k] = append([]string(nil), v...)
	}
	clientCacheControl(cp, hd, e.fresh-age)
	hd.Set("Age", strconv.Itoa(int(age.Seconds())))
	hd.Set("Via", "1.1 google")
	hd.Del("Cache-Tag")
	fillCacheVars(hd, status)
	if e.status == http.StatusOK {
		hd.Del("Content-Length")
		var mod time.Time
		if lm := e.header.Get("Last-Modified"); lm != "" {
			mod, _ = http.ParseTime(lm)
		}
		http.ServeContent(w, r, "", mod, bytes.NewReader(e.body))
		return
	}
	hd.Set("Content-Length", strconv.Itoa(len(e.body)))
	w.WriteHeader(e.status)
	if r.Method != http.MethodHead {
		_, _ = w.Write(e.body)
	}
}

// finish stores the captured response, or serves the stale entry the
// origin's failure fell back to.
func (w *cdnWriter) finish() {
	if w.swallow && w.stale != nil {
		for k := range w.Header() {
			delete(w.Header(), k)
		}
		w.stale()
		return
	}
	if w.capture && w.onFinish != nil {
		w.onFinish(w)
	}
}
