package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// Cloud CDN's pages (#2160), over Compute Engine's backend services and
// buckets (enableCDN, cdnPolicy, addSignedUrlKey, deleteSignedUrlKey) and
// URL maps (invalidateCache), as Google's console has them
// (cdn/docs/setting-up-cdn-with-bucket, .../using-signed-urls and
// .../invalidating-cached-content, read 2026-11-22): Network services ›
// Cloud CDN, the origins (each backend service or bucket with Cloud CDN
// on; a row's menu has Edit and Remove); Add origin; an origin's Origin
// details page with its cache settings, its Restricted content (signed
// request keys: Add signing key, by name, Automatically generate or Let me
// enter, and Cache entry maximum age) and its Cache invalidation tab (the
// Associated load balancer, Host and Path, then Invalidate; the recent
// invalidations, from the URL maps' invalidateCache operations). The list's
// columns are UNVERIFIED (re-read 2026-11-22: the docs name none).

type computeCDNProvider struct{ computeScreen }

func (computeCDNProvider) ID() string    { return "netservices-cdn" }
func (computeCDNProvider) Title() string { return "Cloud CDN" }

const cdnNote = "Cloud CDN's cache is the load balancers' proxy on this machine: a cacheable response through an origin with Cloud CDN on is kept for its TTL and served again with an Age header, " +
	"signed URLs and cookies are checked against the origin's signed request keys, and an invalidation empties the matching entries at once."

var cdnModeText = map[string]string{"CACHE_ALL_STATIC": "Cache static content", "USE_ORIGIN_HEADERS": "Use origin headers", "FORCE_CACHE_ALL": "Force cache all"}

// cdnOrigin is one backend service or bucket, its path [coll, scope, name].
type cdnOrigin struct {
	path []string
	o    map[string]any
}

func cdnOn(o map[string]any) bool { return o["enableCDN"] == true || o["enableCdn"] == true }

func (p computeCDNProvider) backends(project string) []cdnOrigin {
	var out []cdnOrigin
	for _, o := range append(p.lbAll(project, "backendServices", true, false), p.lbAll(project, "backendBuckets", true, false)...) {
		coll := "backendServices"
		if vpcStr(o, "kind") == "compute#backendBucket" {
			coll = "backendBuckets"
		}
		sp := lbScopePath(o)
		out = append(out, cdnOrigin{path: []string{coll, sp[0], sp[1]}, o: o})
	}
	return out
}

func cdnRel(path []string) string { return lbScopeColl(path[1], path[0]) + "/" + path[2] }

// cdnMaps are the URL maps that send traffic to an origin: its associated
// load balancers.
func (p computeCDNProvider) cdnMaps(project string, path []string) []string {
	rel := "projects/" + project + "/" + cdnRel(path)
	var out []string
	for _, m := range p.lbAll(project, "urlMaps", true, false) {
		if hasString(urlMapServiceLinks(m), rel) {
			out = append(out, vpcStr(m, "name"))
		}
	}
	sort.Strings(out)
	return out
}

func (p computeCDNProvider) List(ctx context.Context, project string) (console.Listing, error) {
	listing := console.Listing{Columns: []string{"Origin type", "Cache mode", "Associated load balancers", "Signed request keys"}, NameColumn: "Origin", Noun: "origins", Note: cdnNote}
	if p.api() == nil {
		return listing, errors.New(computeNotStarted)
	}
	if project == "" {
		listing.Prompt = "Cloud CDN origins are listed per project. Choose one in the toolbar."
		return listing, nil
	}
	for _, b := range p.backends(project) {
		if !cdnOn(b.o) {
			continue
		}
		cdn := vmMap(b.o, "cdnPolicy")
		typ := "Backend service"
		if b.path[0] == "backendBuckets" {
			typ = "Backend bucket (" + vpcStr(b.o, "bucketName") + ")"
		}
		listing.Items = append(listing.Items, console.Resource{Name: b.path[2], Opens: b.path, Target: b.path,
			Fields: map[string]string{"Origin type": typ, "Cache mode": vpcDash(cdnModeText[vpcOr(vpcStr(cdn, "cacheMode"), "CACHE_ALL_STATIC")]),
				"Associated load balancers": vpcDash(strings.Join(p.cdnMaps(project, b.path), ", ")),
				"Signed request keys":       strconv.Itoa(len(vpcStrs(cdn, "signedUrlKeyNames")))},
			Actions: []console.Action{cdnRemoveAction(b.path)}})
	}
	listing.Total = len(listing.Items)
	return listing, nil
}

func cdnRemoveAction(path []string) console.Action {
	return console.Action{ID: "remove", Label: "Remove", Destructive: true, ConfirmStyle: console.DeleteConfirmClick, ConfirmTitle: "Remove origin",
		Confirm: "Turn Cloud CDN off for " + path[2] + "? Its cached responses are dropped; the backend itself is kept."}
}

func (computeCDNProvider) CreateOnPage() bool  { return true }
func (computeCDNProvider) CreateTitle() string { return "Add origin" }

func (p computeCDNProvider) CreateForm() (string, []console.Field) {
	return "Add origin", cdnCacheFields(nil, "Origin basics", "Cache performance", nil)
}

func (p computeCDNProvider) CreateFormFor(ctx context.Context, project string) []console.Field {
	opts := []string{}
	for _, b := range p.backends(project) {
		if !cdnOn(b.o) {
			opts = append(opts, strings.Join(b.path, "/"))
		}
	}
	return cdnCacheFields(nil, "Origin basics", "Cache performance", opts)
}

// cdnCacheFields are an origin's cache settings, prefilled from cdn;
// origins, when not nil, makes the first a choice of origin.
func cdnCacheFields(cdn map[string]any, basics, perf string, origins []string) []console.Field {
	var fields []console.Field
	if basics != "" {
		f := console.Field{Name: "origin", Label: "Origin", Type: "text", Required: true, Section: basics,
			Help: "A backend service or bucket without Cloud CDN: backendServices/global/NAME or backendBuckets/global/NAME."}
		if origins != nil {
			f.Type, f.Options = "select", origins
		}
		fields = append(fields, f)
	}
	return append(fields,
		console.Field{Name: "cacheMode", Label: "Cache mode", Type: "select", Default: vpcOr(vpcStr(cdn, "cacheMode"), "CACHE_ALL_STATIC"), Section: perf,
			Options: []string{"CACHE_ALL_STATIC", "USE_ORIGIN_HEADERS", "FORCE_CACHE_ALL"},
			OptionLabels: map[string]string{"CACHE_ALL_STATIC": "Cache static content (recommended)", "USE_ORIGIN_HEADERS": "Use origin settings based on Cache-Control headers",
				"FORCE_CACHE_ALL": "Force cache all content"}},
		console.Field{Name: "clientTtl", Label: "Client time to live", Type: "text", Default: vpcOr(num2s(cdn, "clientTtl"), "3600"), Section: perf, Pattern: `^[0-9]*$`, Help: "Seconds."},
		console.Field{Name: "defaultTtl", Label: "Default time to live", Type: "text", Default: vpcOr(num2s(cdn, "defaultTtl"), "3600"), Section: perf, Pattern: `^[0-9]*$`, Help: "Seconds."},
		console.Field{Name: "maxTtl", Label: "Maximum time to live", Type: "text", Default: vpcOr(num2s(cdn, "maxTtl"), "86400"), Section: perf, Pattern: `^[0-9]*$`,
			Help: "Seconds; not used with Use origin headers."},
		console.Field{Name: "negativeCaching", Label: "Negative caching", Type: "checkbox", Default: strconv.FormatBool(cdn["negativeCaching"] == true), Section: perf,
			Help: "Cache error responses (404, 410, 501 and others) for Google's default times."},
		console.Field{Name: "serveWhileStale", Label: "Serve while stale", Type: "text", Default: num2s(cdn, "serveWhileStale"), Section: perf, Pattern: `^[0-9]*$`,
			Help: "Optional. Seconds a stale response may be served while it is revalidated."},
	)
}

// cdnPolicyOf is cdnPolicy from the cache fields' values.
func cdnPolicyOf(v map[string]string) (map[string]any, error) {
	cdn := map[string]any{"cacheMode": vpcOr(v["cacheMode"], "CACHE_ALL_STATIC"), "negativeCaching": v["negativeCaching"] == "true"}
	for _, k := range []string{"clientTtl", "defaultTtl", "maxTtl", "serveWhileStale"} {
		if !cdnTTLAllowed(cdn["cacheMode"].(string), k) {
			// A merge patch's null clears what the origin had.
			cdn[k] = nil
			continue
		}
		s := strings.TrimSpace(v[k])
		if s == "" {
			continue
		}
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("%s: a whole number of seconds", k)
		}
		cdn[k] = float64(n)
	}
	return cdn, nil
}

// cdnTTLAllowed is whether a cache mode takes a TTL, as Google's API has
// it (cdn/docs/using-cache-modes, read 2026-11-22): Use origin settings
// based on Cache-Control headers takes none, Force cache all content no
// maximum TTL; the form sends null for the others, which clears them.
func cdnTTLAllowed(mode, ttl string) bool {
	switch mode {
	case "USE_ORIGIN_HEADERS":
		return ttl != "clientTtl" && ttl != "defaultTtl" && ttl != "maxTtl"
	case "FORCE_CACHE_ALL":
		return ttl != "maxTtl"
	}
	return true
}

func cdnEnablePatch(path []string, on bool, cdn map[string]any) map[string]any {
	patch := map[string]any{}
	if path[0] == "backendBuckets" {
		patch["enableCdn"] = on
	} else {
		patch["enableCDN"] = on
	}
	if cdn != nil {
		patch["cdnPolicy"] = cdn
	}
	return patch
}

func cdnPath(s string) ([]string, error) {
	p := strings.Split(strings.TrimSpace(s), "/")
	if len(p) != 3 || (p[0] != "backendServices" && p[0] != "backendBuckets") {
		return nil, errors.New("Origin: backendServices/global/NAME or backendBuckets/global/NAME")
	}
	return p, nil
}

func (p computeCDNProvider) Create(ctx context.Context, project string, v map[string]string) (string, error) {
	path, err := cdnPath(v["origin"])
	if err != nil {
		return "", err
	}
	cdn, err := cdnPolicyOf(v)
	if err != nil {
		return "", err
	}
	if _, err := p.call("PATCH", project, cdnRel(path), nil, cdnEnablePatch(path, true, cdn)); err != nil {
		return "", err
	}
	return path[2], nil
}

func (p computeCDNProvider) origin(project string, path []string) (map[string]any, error) {
	if len(path) < 3 {
		return nil, fmt.Errorf("an origin is addressed by its kind, scope and name, not %v", path)
	}
	return p.call("GET", project, cdnRel(path[:3]), nil, nil)
}

func (p computeCDNProvider) invalidations(project string, maps []string) console.Listing {
	l := console.Listing{Columns: []string{"Load balancer", "Status", "Requested"}, NameColumn: "Invalidation", Noun: "cache invalidations", Items: []console.Resource{}}
	ops, err := p.list(project, "global/operations")
	if err != nil {
		l.Unavailable = err.Error()
		return l
	}
	for _, op := range ops {
		if vpcStr(op, "operationType") != "invalidateCache" || !hasString(maps, vpcLast(vpcStr(op, "targetLink"))) {
			continue
		}
		l.Items = append(l.Items, console.Resource{Name: vpcStr(op, "name"), Fields: map[string]string{"Load balancer": vpcLast(vpcStr(op, "targetLink")), "Status": vpcTitle(strings.ToLower(vpcStr(op, "status"))),
			"Requested": vpcStr(op, "insertTime")}})
	}
	sort.Slice(l.Items, func(i, j int) bool { return l.Items[i].Fields["Requested"] > l.Items[j].Fields["Requested"] })
	l.Total = len(l.Items)
	return l
}

func (p computeCDNProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if p.api() == nil {
		return console.Detail{Unavailable: computeNotStarted}, nil
	}
	if len(path) != 3 {
		return console.Detail{Unavailable: "an origin is addressed by its kind, scope and name"}, nil
	}
	o, err := p.origin(project, path)
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	cdn := vmMap(o, "cdnPolicy")
	maps := p.cdnMaps(project, path)
	ttl := func(k string) string {
		if s := num2s(cdn, k); s != "" {
			return s + " seconds"
		}
		return "—"
	}
	typ := "Backend service"
	if path[0] == "backendBuckets" {
		typ = "Backend bucket: " + vpcStr(o, "bucketName")
	}
	props := []console.Property{{Label: "Origin", Value: path[2]}, {Label: "Origin type", Value: typ},
		{Label: "Cloud CDN", Value: map[bool]string{true: "Enabled", false: "Disabled"}[cdnOn(o)]},
		{Label: "Associated load balancers", Value: vpcDash(strings.Join(maps, ", "))},
		{Label: "Cache mode", Value: vpcDash(cdnModeText[vpcOr(vpcStr(cdn, "cacheMode"), "CACHE_ALL_STATIC")])},
		{Label: "Client time to live", Value: ttl("clientTtl")}, {Label: "Default time to live", Value: ttl("defaultTtl")},
		{Label: "Maximum time to live", Value: ttl("maxTtl")},
		{Label: "Negative caching", Value: map[bool]string{true: "Enabled", false: "Disabled"}[cdn["negativeCaching"] == true]}}
	keys := console.Listing{Columns: []string{"Cache entry maximum age"}, NameColumn: "Key name", Noun: "signing keys", Items: []console.Resource{},
		Note: "Requests to this origin must then be signed URLs or carry a signed cookie made with one of these keys (Restrict access using signed URLs and signed cookies)."}
	for _, k := range vpcStrs(cdn, "signedUrlKeyNames") {
		keys.Items = append(keys.Items, console.Resource{Name: k, Target: append(append([]string{}, path...), "key", k),
			Fields: map[string]string{"Cache entry maximum age": ttl("signedUrlCacheMaxAgeSec")},
			Actions: []console.Action{{ID: "deletekey", Label: "Delete", Destructive: true, ConfirmStyle: console.DeleteConfirmClick, ConfirmTitle: "Delete signing key",
				Confirm: "Delete signing key " + k + "? URLs and cookies signed with it are refused."}}})
	}
	keys.Total = len(keys.Items)
	inv := p.invalidations(project, maps)
	return console.Detail{Title: path[2], Heading: "Origin details",
		Sections: []console.Section{{ID: "details", Label: "Details", Kind: console.KindProperties, Groups: []console.PropertyGroup{{Properties: props}}},
			{ID: "restricted", Label: "Restricted content", Listing: keys, Actions: []console.Action{cdnAddKeyAction()}},
			{ID: "invalidation", Label: "Cache invalidation", Listing: inv, Actions: []console.Action{cdnInvalidateAction(maps)}, Note: cdnNote}},
		Edit: &console.EditForm{Label: "Edit", Title: "Edit origin", Submit: "Update", Page: true, Steps: "next",
			Fields: cdnCacheFields(cdn, "", "Cache performance", nil), Note: "The origin's backend cannot be changed; Remove turns Cloud CDN off."}}, nil
}

func cdnAddKeyAction() console.Action {
	return console.Action{ID: "addkey", Label: "Add signing key", Title: "Add signing key", Submit: "Done", Fields: []console.Field{
		{Name: "keyName", Label: "Name", Type: "text", Required: true, Pattern: computeNamePattern, Help: "A unique name for the new signing key."},
		{Name: "method", Label: "Key creation method", Type: "radio", Default: "generate", Options: []string{"generate", "enter"},
			OptionLabels: map[string]string{"generate": "Automatically generate", "enter": "Let me enter"},
			Help:         "An automatically generated key is shown once, after Done; keep it to sign URLs."},
		{Name: "keyValue", Label: "Key value", Type: "text", VisibleWhen: map[string]string{"method": "enter"},
			Help: "A 128-bit key, base64url encoded (RFC 4648 §5), 22 characters with no padding or 24 with it."},
		{Name: "maxAge", Label: "Cache entry maximum age", Type: "text", Default: "3600", Pattern: `^[0-9]*$`,
			Help: "Seconds a response to a signed request is cached (signedUrlCacheMaxAgeSec)."},
	}}
}

func cdnInvalidateAction(maps []string) console.Action {
	lb := console.Field{Name: "urlMap", Label: "Associated load balancers", Type: "select", Required: true, Options: maps}
	if len(maps) == 0 {
		lb.Type, lb.Options = "text", nil
		lb.Help = "No load balancer sends traffic to this origin yet."
	}
	return console.Action{ID: "invalidate", Label: "Invalidate cache", Title: "Cache invalidation", Submit: "Invalidate", Fields: []console.Field{lb,
		{Name: "host", Label: "Host", Type: "text", Help: "Optional. Leave the field empty to invalidate the path across all hostnames."},
		{Name: "path", Label: "Path", Type: "text", Required: true, Help: "The path and filename, such as /images/file.jpg, or a prefix ending in /*."}}}
}

func (p computeCDNProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if p.api() == nil || len(path) < 3 {
		return nil
	}
	if _, err := p.origin(project, path); err != nil {
		return nil
	}
	if len(path) == 5 && path[3] == "key" {
		return []console.Action{{ID: "deletekey", Label: "Delete", Destructive: true}}
	}
	if len(path) != 3 {
		return nil
	}
	r := cdnRemoveAction(path)
	r.Leaves = true
	return []console.Action{cdnAddKeyAction(), cdnInvalidateAction(p.cdnMaps(project, path)), r}
}

func (p computeCDNProvider) ActAt(ctx context.Context, project string, path []string, action string, v map[string]string) error {
	_, err := p.ActAtResult(ctx, project, path, action, v)
	return err
}

// ActAtResult implements console.ResultActor: an automatically generated
// signing key is shown once, as the key value is never read back.
func (p computeCDNProvider) ActAtResult(ctx context.Context, project string, path []string, action string, v map[string]string) (*console.Listing, error) {
	if len(path) < 3 {
		return nil, fmt.Errorf("%s is not available here", action)
	}
	rel := cdnRel(path[:3])
	switch action {
	case "remove":
		_, err := p.call("PATCH", project, rel, nil, cdnEnablePatch(path, false, nil))
		return nil, err
	case "addkey":
		name := strings.TrimSpace(v["keyName"])
		value := strings.TrimSpace(v["keyValue"])
		generated := v["method"] != "enter"
		if generated {
			b := make([]byte, 16)
			if _, err := rand.Read(b); err != nil {
				return nil, err
			}
			value = base64.URLEncoding.EncodeToString(b)
		}
		if value == "" {
			return nil, errors.New("Key value: give one, or choose Automatically generate")
		}
		if _, err := p.call("POST", project, rel+"/addSignedUrlKey", nil, map[string]any{"keyName": name, "keyValue": value}); err != nil {
			return nil, err
		}
		if s := strings.TrimSpace(v["maxAge"]); s != "" {
			n, err := strconv.Atoi(s)
			if err != nil || n < 0 {
				return nil, errors.New("Cache entry maximum age: a whole number of seconds")
			}
			if _, err := p.call("PATCH", project, rel, nil, map[string]any{"cdnPolicy": map[string]any{"signedUrlCacheMaxAgeSec": float64(n)}}); err != nil {
				return nil, err
			}
		}
		if !generated {
			return nil, nil
		}
		return &console.Listing{Columns: []string{"Key value"}, NameColumn: "Key name", Noun: "signing keys",
			Note:  "Save this key value now: it is not shown again.",
			Items: []console.Resource{{Name: name, Fields: map[string]string{"Key value": value}}}, Total: 1}, nil
	case "deletekey":
		if len(path) != 5 {
			return nil, errors.New("Delete applies to a signing key")
		}
		_, err := p.call("POST", project, rel+"/deleteSignedUrlKey", map[string][]string{"keyName": {path[4]}}, nil)
		return nil, err
	case "invalidate":
		m := strings.TrimSpace(v["urlMap"])
		if m == "" {
			return nil, errors.New("Associated load balancers: choose one")
		}
		body := map[string]any{"path": strings.TrimSpace(v["path"])}
		if h := strings.TrimSpace(v["host"]); h != "" {
			body["host"] = h
		}
		_, err := p.call("POST", project, "global/urlMaps/"+m+"/invalidateCache", nil, body)
		return nil, err
	}
	return nil, fmt.Errorf("%s is not available here", action)
}

// Edit implements console.Editor: the origin's cache settings.
func (p computeCDNProvider) Edit(ctx context.Context, project string, path []string, v map[string]string) error {
	if len(path) != 3 {
		return errors.New("Edit applies to an origin")
	}
	cdn, err := cdnPolicyOf(v)
	if err != nil {
		return err
	}
	_, err = p.call("PATCH", project, cdnRel(path), nil, map[string]any{"cdnPolicy": cdn})
	return err
}
