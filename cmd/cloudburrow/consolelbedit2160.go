package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"strconv"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// A load balancer's Edit page (#2160), as Google's console has it
// (load-balancing/docs/https/setup-global-ext-https-compute and
// .../https/setting-up-https, "Edit", read 2026-11-22): on the load
// balancer's details page, Edit opens the load balancer's steps — Frontend
// configuration, Backend configuration, Routing rules — then Review and
// finalize, which lists every change, and Update (the review dialog's button). Each step changes the
// real records: the forwarding rule (its port: Google replaces a rule whose
// port changes), the target proxy's certificates, certificate map and SSL
// policy, the backend service's timeout, session affinity, health check,
// Cloud CDN, Cloud Armor policy and IAP, and the URL map's host and path
// rules.

// v1 sends one request to the Compute port's handler (the APIs served
// beside /compute/v1/: Certificate Manager and IAP) and decodes its JSON
// answer, or returns the API's own error message.
func (c computeScreen) v1(ctx context.Context, method, path string, body any) (map[string]any, error) {
	h := c.svc.v1Handler()
	if h == nil {
		return nil, errors.New(computeNotStarted)
	}
	var raw []byte
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		raw = b
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(raw)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if rec.Code >= 300 {
		if e, ok := out["error"].(map[string]any); ok {
			if m, _ := e["message"].(string); m != "" {
				return nil, errors.New(m)
			}
		}
		return nil, fmt.Errorf("%s %s: HTTP %d", method, path, rec.Code)
	}
	if e, ok := out["error"].(map[string]any); ok && out["done"] == true {
		if m, _ := e["message"].(string); m != "" {
			return nil, errors.New(m)
		}
	}
	return out, nil
}

// relPath is a relative name's part after projects/P/, for call.
func relPath(rel string) string {
	p := strings.SplitN(lbRel(rel), "/", 3)
	if len(p) < 3 {
		return rel
	}
	return p[2]
}

// lbPrimaryBackend is the backend service (or bucket) a load balancer's
// Backend configuration step edits: an Application Load Balancer's URL
// map's default, else the load balancer's own backend service.
func lbPrimaryBackend(lm lbModel) string {
	if lm.kind == "urlMap" {
		return lbRel(vpcStr(lm.self, "defaultService"))
	}
	if lm.kind == "backendService" {
		return lbRel(vpcStr(lm.self, "selfLink"))
	}
	return ""
}

// securityPolicyNames are the project's Cloud Armor policies of a type
// (CLOUD_ARMOR, CLOUD_ARMOR_EDGE) in a scope, by name.
func (c computeScreen) securityPolicyNames(project, scope, typ string) []string {
	out := []string{""}
	for _, sp := range c.lbAll(project, "securityPolicies", scope == "global", scope != "global") {
		if vpcOr(vpcStr(sp, "type"), "CLOUD_ARMOR") != typ {
			continue
		}
		if r := vpcLast(vpcStr(sp, "region")); (scope == "global" && r != "") || (scope != "global" && r != scope) {
			continue
		}
		out = append(out, vpcStr(sp, "name"))
	}
	return out
}

// lbRoutingDefault is the URL map's host and path rules as the Routing
// rules step's map field holds them: path, or host|path, to the backend.
func lbRoutingDefault(m map[string]any) string {
	matchers := map[string]map[string]any{}
	for _, pm := range vmMaps(m, "pathMatchers") {
		matchers[vpcStr(pm, "name")] = pm
	}
	out := map[string]string{}
	for _, hr := range vmMaps(m, "hostRules") {
		for _, host := range vpcStrs(hr, "hosts") {
			for _, pr := range vmMaps(matchers[vpcStr(hr, "pathMatcher")], "pathRules") {
				svc := relPath(vpcStr(pr, "service"))
				if svc == "" {
					continue
				}
				if strings.Contains(svc, "/backendServices/") {
					svc = vpcLast(svc)
				}
				for _, path := range vpcStrs(pr, "paths") {
					k := path
					if host != "*" {
						k = host + "|" + path
					}
					out[k] = svc
				}
			}
		}
	}
	return console.FormatMap(out)
}

func num2s(o map[string]any, k string) string {
	if v, ok := o[k].(float64); ok {
		return strconv.FormatFloat(v, 'f', -1, 64)
	}
	return ""
}

// lbEditForm is the Edit page's form for a load balancer, prefilled.
func (p computeLBProvider) lbEditForm(project string, lm lbModel) *console.EditForm {
	const front, back, route = "Frontend configuration", "Backend configuration", "Routing rules"
	var fields []console.Field
	if len(lm.rules) > 0 {
		r := lm.rules[0]
		port := vpcStr(r, "portRange")
		if lo, hi, ok := strings.Cut(port, "-"); ok && lo == hi {
			port = lo
		}
		if port == "" {
			port = strings.Join(vpcStrs(r, "ports"), ",")
		}
		fields = append(fields, console.Field{Name: "port", Label: "Port", Type: "text", Default: port, Section: front, Pattern: `^[0-9]{1,5}(-[0-9]{1,5})?$`,
			Help: "Changing the port replaces the forwarding rule " + vpcStr(r, "name") + ", as Google's Update does; it keeps its name and address."})
		for _, px := range lm.proxies {
			if lbRel(vpcStr(px, "selfLink")) != lbRel(vpcStr(r, "target")) {
				continue
			}
			k := vpcStr(px, "kind")
			if k != "compute#targetHttpsProxy" && k != "compute#targetSslProxy" {
				continue
			}
			var certs []string
			for _, c := range vpcStrs(px, "sslCertificates") {
				certs = append(certs, vpcLast(c))
			}
			fields = append(fields,
				console.Field{Name: "certificates", Label: "Certificate", Type: "text", Default: strings.Join(certs, ", "), Section: front,
					Help: "Classic SSL certificates' names, comma separated; empty when a certificate map serves the proxy."},
				console.Field{Name: "certificateMap", Label: "Certificate map", Type: "text", Default: vpcLast(vpcStr(px, "certificateMap")), Section: front,
					Help: "A Certificate Manager certificate map's name (global), which then serves the proxy instead of its classic certificates."},
				console.Field{Name: "sslPolicy", Label: "SSL policy", Type: "text", Default: vpcLast(vpcStr(px, "sslPolicy")), Section: front,
					Help: "An SSL policy's name, or empty for the GCP default."})
		}
	}
	if svc := lbPrimaryBackend(lm); svc != "" {
		o, err := p.call("GET", project, relPath(svc), nil, nil)
		if err == nil {
			scope := lm.path[0]
			cdn := vmMap(o, "cdnPolicy")
			bucket := vpcStr(o, "kind") == "compute#backendBucket"
			onCDN := map[string]string{"enableCDN": "true"}
			fields = append(fields, console.Field{Name: "backend", Label: "Backend service", Type: "text", Default: vpcStr(o, "name"), Section: back,
				Help: "The backend this step edits: " + relPath(svc) + "."})
			if bucket {
				fields = append(fields, console.Field{Name: "enableCDN", Label: "Enable Cloud CDN", Type: "checkbox", Default: strconv.FormatBool(o["enableCdn"] == true), Section: back})
			} else {
				hc := ""
				if l := vpcStrs(o, "healthChecks"); len(l) > 0 {
					hc = vpcLast(l[0])
				}
				fields = append(fields,
					console.Field{Name: "timeoutSec", Label: "Timeout", Type: "text", Default: vpcOr(num2s(o, "timeoutSec"), "30"), Section: back, Pattern: `^[0-9]+$`,
						Help: "Seconds the load balancer waits for a backend's response."},
					console.Field{Name: "healthCheck", Label: "Health check", Type: "text", Default: hc, Section: back},
					console.Field{Name: "sessionAffinity", Label: "Session affinity", Type: "select", Default: vpcOr(vpcStr(o, "sessionAffinity"), "NONE"), Section: back,
						Options: []string{"NONE", "CLIENT_IP", "GENERATED_COOKIE", "HEADER_FIELD", "HTTP_COOKIE"}},
					console.Field{Name: "enableCDN", Label: "Enable Cloud CDN", Type: "checkbox", Default: strconv.FormatBool(o["enableCDN"] == true), Section: back},
				)
			}
			fields = append(fields,
				console.Field{Name: "cacheMode", Label: "Cache mode", Type: "select", Default: vpcOr(vpcStr(cdn, "cacheMode"), "CACHE_ALL_STATIC"), Section: back,
					Options: []string{"CACHE_ALL_STATIC", "USE_ORIGIN_HEADERS", "FORCE_CACHE_ALL"}, VisibleWhen: onCDN,
					OptionLabels: map[string]string{"CACHE_ALL_STATIC": "Cache static content (recommended)", "USE_ORIGIN_HEADERS": "Use origin settings based on Cache-Control headers",
						"FORCE_CACHE_ALL": "Force cache all content"}},
				console.Field{Name: "clientTtl", Label: "Client time to live", Type: "text", Default: vpcOr(num2s(cdn, "clientTtl"), "3600"), Section: back, VisibleWhen: onCDN, Pattern: `^[0-9]*$`, Help: "Seconds."},
				console.Field{Name: "defaultTtl", Label: "Default time to live", Type: "text", Default: vpcOr(num2s(cdn, "defaultTtl"), "3600"), Section: back, VisibleWhen: onCDN, Pattern: `^[0-9]*$`, Help: "Seconds."},
				console.Field{Name: "maxTtl", Label: "Maximum time to live", Type: "text", Default: vpcOr(num2s(cdn, "maxTtl"), "86400"), Section: back, VisibleWhen: onCDN, Pattern: `^[0-9]*$`, Help: "Seconds."},
				console.Field{Name: "negativeCaching", Label: "Negative caching", Type: "checkbox", Default: strconv.FormatBool(cdn["negativeCaching"] == true), Section: back, VisibleWhen: onCDN},
			)
			edge := p.securityPolicyNames(project, scope, "CLOUD_ARMOR_EDGE")
			if !bucket {
				fields = append(fields, console.Field{Name: "securityPolicy", Label: "Cloud Armor backend security policy", Type: "select",
					Default: vpcLast(vpcStr(o, "securityPolicy")), Options: p.securityPolicyNames(project, scope, "CLOUD_ARMOR"), Section: back,
					OptionLabels: map[string]string{"": "None"}})
			}
			if scope == "global" {
				fields = append(fields, console.Field{Name: "edgeSecurityPolicy", Label: "Cloud Armor edge security policy", Type: "select",
					Default: vpcLast(vpcStr(o, "edgeSecurityPolicy")), Options: edge, Section: back, OptionLabels: map[string]string{"": "None"}})
			}
			if !bucket {
				fields = append(fields, console.Field{Name: "iap", Label: "Identity-Aware Proxy", Type: "checkbox",
					Default: strconv.FormatBool(vmMap(o, "iap")["enabled"] == true), Section: back,
					Help: "Requests then need an identity token of a principal with IAP-secured Web App User (roles/iap.httpsResourceAccessor)."})
			}
			if proto := vpcStr(o, "protocol"); !bucket && (proto == "HTTPS" || proto == "HTTP2") {
				// Advanced configurations › Backend authentication
				// (load-balancing/docs/backend-authenticated-tls-setup).
				fields = append(fields, lbBackendAuthFields(p.ccmBackendAuthNames(project, scope), o)...)
			}
		}
	}
	if lm.kind == "urlMap" {
		fields = append(fields,
			console.Field{Name: "defaultBackend", Label: "Default backend", Type: "text", Default: relPath(vpcStr(lm.self, "defaultService")), Section: route,
				Help: "Requests that match no rule go here: global/backendServices/NAME or global/backendBuckets/NAME."},
			console.Field{Name: "routing", Label: "Host and path rules", Type: "map", Default: lbRoutingDefault(lm.self), Section: route,
				KeyHelp: "A path pattern, such as /api/*, or host|path such as example.com|/images/*", ValueHelp: "A backend service's name, or global/backendBuckets/NAME"})
	}
	if len(fields) == 0 {
		return nil
	}
	return &console.EditForm{Label: "Edit", Title: "Edit " + strings.ToLower(lm.typ) + " load balancer", Submit: "Review and finalize", Page: true,
		Steps: "continue", Review: "Update", Fields: fields,
		Note: "The load balancer's name, type and region cannot be changed. Its records change as Update sends them, each through its own API method."}
}

// Edit implements console.Editor: Update on a load balancer's Edit page.
func (p computeLBProvider) Edit(ctx context.Context, project string, path []string, v map[string]string) error {
	lm, err := p.model(project, path)
	if err != nil {
		return err
	}
	form := p.lbEditForm(project, lm)
	if form == nil {
		return errors.New("this load balancer has nothing to edit")
	}
	cur := map[string]string{}
	for _, f := range form.Fields {
		cur[f.Name] = f.Default
	}
	changed := func(k string) bool { _, ok := v[k]; return ok && strings.TrimSpace(v[k]) != cur[k] }
	scope := lm.path[0]
	link := func(coll, name string) string {
		if name == "" {
			return ""
		}
		return "projects/" + project + "/" + lbScopeColl(scope, coll) + "/" + name
	}

	// Backend configuration.
	if svc := lbPrimaryBackend(lm); svc != "" {
		bucket := strings.Contains(svc, "/backendBuckets/")
		patch := map[string]any{}
		if changed("timeoutSec") {
			n, err := strconv.Atoi(strings.TrimSpace(v["timeoutSec"]))
			if err != nil || n < 1 {
				return errors.New("Timeout: a whole number of seconds, at least 1")
			}
			patch["timeoutSec"] = float64(n)
		}
		if changed("sessionAffinity") {
			patch["sessionAffinity"] = v["sessionAffinity"]
		}
		if changed("healthCheck") {
			patch["healthChecks"] = []any{link("healthChecks", strings.TrimSpace(v["healthCheck"]))}
		}
		if changed("enableCDN") {
			if bucket {
				patch["enableCdn"] = v["enableCDN"] == "true"
			} else {
				patch["enableCDN"] = v["enableCDN"] == "true"
			}
		}
		cdn := map[string]any{}
		if changed("cacheMode") {
			cdn["cacheMode"] = v["cacheMode"]
		}
		mode := vpcOr(v["cacheMode"], "CACHE_ALL_STATIC")
		for _, k := range []string{"clientTtl", "defaultTtl", "maxTtl"} {
			if !cdnTTLAllowed(mode, k) {
				if changed("cacheMode") {
					cdn[k] = nil // a merge patch's null clears it
				}
				continue
			}
			if changed(k) {
				n, err := strconv.Atoi(strings.TrimSpace(v[k]))
				if err != nil || n < 0 {
					return fmt.Errorf("%s: a whole number of seconds", k)
				}
				cdn[k] = float64(n)
			}
		}
		if changed("negativeCaching") {
			cdn["negativeCaching"] = v["negativeCaching"] == "true"
		}
		if len(cdn) > 0 {
			patch["cdnPolicy"] = cdn
		}
		if changed("iap") {
			patch["iap"] = map[string]any{"enabled": v["iap"] == "true"}
		}
		if changed("backendAuth") || changed("tlsSni") || changed("tlsSans") || changed("tlsAuthConfig") {
			patch["tlsSettings"] = lbTLSSettings(project, scope, v)
		}
		if len(patch) > 0 {
			if _, err := p.call("PATCH", project, relPath(svc), nil, patch); err != nil {
				return err
			}
		}
		if changed("securityPolicy") {
			if _, err := p.call("POST", project, relPath(svc)+"/setSecurityPolicy", nil,
				map[string]any{"securityPolicy": link("securityPolicies", strings.TrimSpace(v["securityPolicy"]))}); err != nil {
				return err
			}
		}
		if changed("edgeSecurityPolicy") {
			if _, err := p.call("POST", project, relPath(svc)+"/setEdgeSecurityPolicy", nil,
				map[string]any{"securityPolicy": link("securityPolicies", strings.TrimSpace(v["edgeSecurityPolicy"]))}); err != nil {
				return err
			}
		}
	}

	// Routing rules.
	if lm.kind == "urlMap" && (changed("routing") || changed("defaultBackend")) {
		def := strings.TrimSpace(v["defaultBackend"])
		if def == "" {
			def = cur["defaultBackend"]
		}
		if !strings.Contains(def, "/") {
			def = lbScopeColl(scope, "backendServices") + "/" + def
		}
		rules, err := lbRoutingRules(v["routing"], scope)
		if err != nil {
			return err
		}
		patch := map[string]any{"defaultService": def, "hostRules": []any{}, "pathMatchers": []any{}}
		if len(rules) > 0 {
			patch["hostRules"], patch["pathMatchers"] = lbHostRules(rules, def)
		}
		if _, err := p.call("PATCH", project, relPath(vpcStr(lm.self, "selfLink")), nil, patch); err != nil {
			return err
		}
	}

	// Frontend configuration.
	if len(lm.rules) > 0 {
		r := lm.rules[0]
		target := lbRel(vpcStr(r, "target"))
		if changed("certificates") {
			var certs []any
			for _, c := range vpcList(v["certificates"]) {
				certs = append(certs, link("sslCertificates", fmt.Sprint(c)))
			}
			if _, err := p.call("POST", project, relPath(target)+"/setSslCertificates", nil, map[string]any{"sslCertificates": certs}); err != nil {
				return err
			}
		}
		if changed("certificateMap") {
			m := strings.TrimSpace(v["certificateMap"])
			if m != "" {
				m = "//certificatemanager.googleapis.com/projects/" + project + "/locations/global/certificateMaps/" + m
			}
			if _, err := p.call("POST", project, relPath(target)+"/setCertificateMap", nil, map[string]any{"certificateMap": m}); err != nil {
				return err
			}
		}
		if changed("sslPolicy") {
			if _, err := p.call("POST", project, relPath(target)+"/setSslPolicy", nil,
				map[string]any{"sslPolicy": link("sslPolicies", strings.TrimSpace(v["sslPolicy"]))}); err != nil {
				return err
			}
		}
		if changed("port") {
			body := map[string]any{}
			for _, k := range []string{"name", "description", "IPAddress", "IPProtocol", "loadBalancingScheme", "target", "backendService",
				"network", "subnetwork", "networkTier", "labels", "allowGlobalAccess", "ipVersion"} {
				if val, ok := r[k]; ok {
					body[k] = val
				}
			}
			port := strings.TrimSpace(v["port"])
			if strings.Contains(port, ",") {
				body["ports"] = vpcList(port)
			} else {
				body["portRange"] = port
			}
			rel := relPath(vpcStr(r, "selfLink"))
			if _, err := p.call("DELETE", project, rel, nil, nil); err != nil {
				return err
			}
			if _, err := p.call("POST", project, rel[:strings.LastIndex(rel, "/")], nil, body); err != nil {
				return fmt.Errorf("Port: the forwarding rule was removed but could not be made again: %w", err)
			}
		}
	}
	return nil
}

// lbBackendAuthFields are the Edit page's Backend authentication section
// of an HTTPS or HTTP/2 backend service: Enable, the SNI hostname, the
// accepted SANs and the backend authentication config.
func lbBackendAuthFields(configs []string, o map[string]any) []console.Field {
	const sec = "Backend authentication"
	ts := vmMap(o, "tlsSettings")
	on := map[string]string{"backendAuth": "true"}
	var sans []string
	for _, x := range vmMaps(ts, "subjectAltNames") {
		sans = append(sans, vpcOr(vpcStr(x, "dnsName"), vpcStr(x, "uniformResourceIdentifier")))
	}
	return []console.Field{
		{Name: "backendAuth", Label: "Enable", Type: "checkbox", Default: strconv.FormatBool(ts != nil), Section: sec,
			Help: "The load balancer then verifies its backends' certificates, and with a client certificate presents its own (backend mTLS)."},
		{Name: "tlsSni", Label: "SNI hostname", Type: "text", Default: vpcStr(ts, "sni"), Section: sec, VisibleWhen: on, Help: "Optional."},
		{Name: "tlsSans", Label: "Accepted SANs", Type: "text", Default: strings.Join(sans, ", "), Section: sec, VisibleWhen: on,
			Help: "Optional, at most 5, comma separated: DNS names, or URIs such as spiffe://example.org/ns/backend. They replace the SNI hostname in the check."},
		{Name: "tlsAuthConfig", Label: "Backend authentication config", Type: "select", Default: vpcLast(vpcStr(ts, "authenticationConfig")),
			Options: configs, OptionLabels: map[string]string{"": "None"}, Section: sec, VisibleWhen: on},
	}
}

// lbTLSSettings is tlsSettings from the Edit page's values; nil clears it.
func lbTLSSettings(project, scope string, v map[string]string) any {
	if v["backendAuth"] != "true" {
		return nil
	}
	ts := map[string]any{}
	if s := strings.TrimSpace(v["tlsSni"]); s != "" {
		ts["sni"] = s
	}
	var sans []any
	for _, x := range strings.Split(v["tlsSans"], ",") {
		if x = strings.TrimSpace(x); x == "" {
			continue
		}
		if strings.Contains(x, "://") {
			sans = append(sans, map[string]any{"uniformResourceIdentifier": x})
		} else {
			sans = append(sans, map[string]any{"dnsName": x})
		}
	}
	if len(sans) > 0 {
		ts["subjectAltNames"] = sans
	}
	if c := strings.TrimSpace(v["tlsAuthConfig"]); c != "" {
		loc := "global"
		if strings.HasPrefix(scope, "regions/") {
			loc = strings.TrimPrefix(scope, "regions/")
		} else if scope != "global" && scope != "" {
			loc = scope
		}
		ts["authenticationConfig"] = "//networksecurity.googleapis.com/projects/" + project + "/locations/" + loc + "/backendAuthenticationConfigs/" + c
	}
	return ts
}

// ccmBackendAuthNames are the backend authentication configs of a
// scope's location, by name, after "" (None).
func (p computeLBProvider) ccmBackendAuthNames(project, scope string) []string {
	loc := "global"
	if strings.HasPrefix(scope, "regions/") {
		loc = strings.TrimPrefix(scope, "regions/")
	} else if scope != "global" && scope != "" {
		loc = scope
	}
	out := []string{""}
	m, err := p.v1(context.Background(), "GET", "/v1/projects/"+project+"/locations/"+loc+"/backendAuthenticationConfigs?pageSize=500", nil)
	if err != nil {
		return out
	}
	for _, c := range vmMaps(m, "backendAuthenticationConfigs") {
		out = append(out, vpcLast(vpcStr(c, "name")))
	}
	return out
}
