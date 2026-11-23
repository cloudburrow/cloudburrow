package main

import (
	"crypto/x509"
	"encoding/pem"
	"strconv"
	"strings"

	cmpb "cloud.google.com/go/certificatemanager/apiv1/certificatemanagerpb"

	"github.com/cloudburrow/cloudburrow/internal/resource"
	"github.com/cloudburrow/cloudburrow/internal/service/certificatemanager"
	"github.com/cloudburrow/cloudburrow/internal/service/compute"
	"github.com/cloudburrow/cloudburrow/internal/service/networksecurity"
)

// Network Security (#2160), served on the Compute Engine port beside
// Certificate Manager: what the load balancers read from it
// (compute.NetworkSecurity), with each server TLS policy's trust config
// resolved from Certificate Manager, and what it reads back (trust
// configs, forwarding rules, and the proxies and backend services that
// name its policies).

// computeNetSec is compute.NetworkSecurity over the two APIs.
type computeNetSec struct {
	ns *networksecurity.Server
	cm *certificatemanager.Server
}

func (c computeNetSec) PolicyExists(name string) bool { return c.ns.Exists(name) }

func (c computeNetSec) Authorize(r compute.AuthzRequest) compute.AuthzVerdict {
	return c.ns.Authorize(r)
}

// sourceTags are the permanent IDs of the tag values a resource carries,
// for an authorization policy's source resources (#2160).
func (s *computeService) sourceTags(full string) []int64 {
	if s.registry == nil {
		return nil
	}
	reg := s.registry()
	if reg == nil {
		return nil
	}
	// A binding names the instance by its project's number, as Google's
	// how-to does, or by its ID.
	names := []string{full}
	if p := strings.Split(full, "/"); len(p) > 4 && p[2] == "compute.googleapis.com" && p[3] == "projects" {
		if n := resource.ProjectNumber(p[4]); n != 0 {
			p[4] = strconv.FormatInt(n, 10)
			names = append(names, strings.Join(p, "/"))
		}
	}
	seen := map[int64]bool{}
	var out []int64
	for _, name := range names {
		tags, err := reg.EffectiveTags(name)
		if err != nil {
			continue
		}
		for _, t := range tags {
			if id, err := strconv.ParseInt(strings.TrimPrefix(t.GetTagValue(), "tagValues/"), 10, 64); err == nil && !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
	}
	return out
}

func (c computeNetSec) ServerTLS(name string) (compute.ServerTLSPolicy, bool) {
	p, ok := c.ns.ServerTLSPolicy(name)
	if !ok {
		return compute.ServerTLSPolicy{}, false
	}
	mt := p.GetMtlsPolicy()
	out := compute.ServerTLSPolicy{TrustConfig: mt.GetClientValidationTrustConfig()}
	switch mt.GetClientValidationMode().String() {
	case "ALLOW_INVALID_OR_MISSING_CLIENT_CERT", "REJECT_INVALID":
		out.Mode = mt.GetClientValidationMode().String()
	}
	if out.TrustConfig == "" {
		return out, true
	}
	tc, found := c.cm.TrustConfig(out.TrustConfig)
	if !found {
		return out, true
	}
	out.TrustFound = true
	roots, inter, allow := trustPools(tc)
	out.Roots, out.Intermediates, out.Allowlisted = roots, inter, allow
	return out, true
}

// trustPools are a trust config's anchors, intermediates and allowlist.
func trustPools(tc *cmpb.TrustConfig) (roots, inter *x509.CertPool, allow []*x509.Certificate) {
	parse := func(p string) []*x509.Certificate {
		var out []*x509.Certificate
		rest := []byte(p)
		for {
			var b *pem.Block
			b, rest = pem.Decode(rest)
			if b == nil {
				return out
			}
			if c, err := x509.ParseCertificate(b.Bytes); err == nil {
				out = append(out, c)
			}
		}
	}
	for _, ts := range tc.GetTrustStores() {
		for _, a := range ts.GetTrustAnchors() {
			for _, c := range parse(a.GetPemCertificate()) {
				if roots == nil {
					roots = x509.NewCertPool()
				}
				roots.AddCert(c)
			}
		}
		for _, a := range ts.GetIntermediateCas() {
			for _, c := range parse(a.GetPemCertificate()) {
				if inter == nil {
					inter = x509.NewCertPool()
				}
				inter.AddCert(c)
			}
		}
	}
	for _, a := range tc.GetAllowlistedCertificates() {
		allow = append(allow, parse(a.GetPemCertificate())...)
	}
	return roots, inter, allow
}

// useNetSec wires the hooks between Network Security, Certificate Manager
// and the Compute Engine API.
func useNetSec(api *compute.Server, ns *networksecurity.Server, cm *certificatemanager.Server) {
	ns.UseHooks(networksecurity.Hooks{
		TrustConfigExists: func(name string) bool { _, ok := cm.TrustConfig(name); return ok },
		ForwardingRule: func(rel string) (string, bool) {
			p := strings.Split(rel, "/")
			if len(p) < 4 {
				return "", false
			}
			out, err := api.Call("GET", p[1], strings.Join(p[2:], "/"), nil, nil)
			if err != nil {
				return "", false
			}
			return strAny(out["loadBalancingScheme"]), true
		},
		Users: func(name string) []string { return netSecUsers(api, name) },
	})
	cm.UseTrustUsers(ns.TrustConfigUsers)
}

// netSecUsers are the target HTTPS proxies and backend services that name
// a Network Security policy.
func netSecUsers(api *compute.Server, name string) []string {
	project := strings.Split(name, "/")[1]
	ref := "//networksecurity.googleapis.com/" + name
	var out []string
	scan := func(coll string, field func(m map[string]any) []string) {
		res, err := api.Call("GET", project, "aggregated/"+coll, nil, nil)
		if err != nil {
			return
		}
		items, _ := res["items"].(map[string]any)
		for _, scoped := range items {
			sm, _ := scoped.(map[string]any)
			for _, it := range listAny(sm[coll]) {
				m, _ := it.(map[string]any)
				for _, v := range field(m) {
					if v == ref {
						out = append(out, relOf(strAny(m["selfLink"])))
					}
				}
			}
		}
	}
	scan("targetHttpsProxies", func(m map[string]any) []string {
		return []string{strAny(m["serverTlsPolicy"]), strAny(m["authorizationPolicy"])}
	})
	scan("backendServices", func(m map[string]any) []string {
		ss, _ := m["securitySettings"].(map[string]any)
		return []string{strAny(ss["clientTlsPolicy"])}
	})
	return out
}
