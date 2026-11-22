package main

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/iameval"
	"github.com/cloudburrow/cloudburrow/internal/metadata"
	"github.com/cloudburrow/cloudburrow/internal/resource"
	"github.com/cloudburrow/cloudburrow/internal/service/certificatemanager"
	"github.com/cloudburrow/cloudburrow/internal/service/compute"
	"github.com/cloudburrow/cloudburrow/internal/service/iap"
)

// Certificate Manager and Identity-Aware Proxy (#2160), served on the
// Compute Engine port beside Serverless VPC Access, and what the load
// balancers' data plane reads from them (compute.CertificateMaps,
// compute.IAPGate).

// relOf is a Compute Engine link as a relative name (projects/...).
func relOf(link string) string {
	if i := strings.Index(link, "projects/"); i >= 0 {
		return link[i:]
	}
	return link
}

// certMapTargets are the global target HTTPS and SSL proxies of project
// that name a certificate map, with their forwarding rules' addresses: a
// map's gclbTargets.
func certMapTargets(api *compute.Server, project string) []certificatemanager.Target {
	if api == nil {
		return nil
	}
	items := func(coll string) []map[string]any {
		out, err := api.Call("GET", project, "global/"+coll, nil, nil)
		if err != nil {
			return nil
		}
		var l []map[string]any
		for _, it := range listAny(out["items"]) {
			if m, ok := it.(map[string]any); ok {
				l = append(l, m)
			}
		}
		return l
	}
	rules := items("forwardingRules")
	var out []certificatemanager.Target
	for _, coll := range []string{"targetHttpsProxies", "targetSslProxies"} {
		for _, p := range items(coll) {
			m, _ := p["certificateMap"].(string)
			if m == "" {
				continue
			}
			self := relOf(strAny(p["selfLink"]))
			t := certificatemanager.Target{Map: m}
			if coll == "targetHttpsProxies" {
				t.HTTPSProxy = self
			} else {
				t.SSLProxy = self
			}
			for _, r := range rules {
				if relOf(strAny(r["target"])) != self {
					continue
				}
				ip := certificatemanager.IPConfig{IP: strAny(r["IPAddress"])}
				if lo, hi, ok := certPortRange(strAny(r["portRange"])); ok {
					for port := lo; port <= hi && port-lo < 64; port++ {
						ip.Ports = append(ip.Ports, uint32(port))
					}
				}
				t.IPs = append(t.IPs, ip)
			}
			out = append(out, t)
		}
	}
	return out
}

func certPortRange(s string) (int, int, bool) {
	a, b, found := strings.Cut(s, "-")
	lo, err := strconv.Atoi(a)
	if err != nil {
		return 0, 0, false
	}
	hi := lo
	if found {
		if hi, err = strconv.Atoi(b); err != nil {
			return 0, 0, false
		}
	}
	return lo, hi, true
}

func listAny(v any) []any {
	l, _ := v.([]any)
	return l
}

func strAny(v any) string {
	s, _ := v.(string)
	return s
}

// backendServiceExists is whether a backend service exists, for IAP's
// resources of one.
func backendServiceExists(api *compute.Server, rel string) bool {
	if api == nil {
		return true
	}
	p := strings.Split(rel, "/")
	if len(p) < 4 {
		return false
	}
	_, err := api.Call("GET", p[1], strings.Join(p[2:], "/"), nil, nil)
	return err == nil
}

// computeIAPGate is compute.IAPGate over the instance's credentials, the
// IAP API's policies and the host's IAM evaluator. Its fields are read
// at each request: up sets the evaluator after the services are made.
type computeIAPGate struct{ svc *computeService }

func (g computeIAPGate) VerifyIDToken(tok string) (string, string, bool) {
	creds := g.svc.creds
	if creds == nil {
		return "", "", false
	}
	return creds.IDTokenClaims(tok, time.Now())
}

// Allowed checks iap.webServiceVersions.accessViaIAP (granted by
// roles/iap.httpsResourceAccessor, and not by roles/owner, as on Google)
// on the resource, the IAP levels above it and the project's hierarchy,
// with each binding's condition.
func (g computeIAPGate) Allowed(_ context.Context, resource, email string, r compute.IAPRequest) bool {
	ev, srv := g.svc.iapEval, g.svc.iap
	if ev == nil || srv == nil {
		return false
	}
	req := iameval.Request{Email: strings.ToLower(email), Permission: "iap.webServiceVersions.accessViaIAP", Resource: resource,
		Type: "iap.googleapis.com/WebService", Service: "iap.googleapis.com", Project: iameval.Project(resource), Time: r.Time,
		RequestHost: r.Host, RequestPath: r.Path}
	for _, lv := range iap.Levels(resource) {
		req.Own = append(req.Own, iameval.Attached{Resource: "//iap.googleapis.com/" + lv, Policy: srv.Policy(lv)})
	}
	return ev.Check(req).Granted
}

func (g computeIAPGate) Settings(resource string) map[string]any {
	if g.svc.iap == nil {
		return nil
	}
	return g.svc.iap.Settings(resource)
}

// useIAP gives the compute service the host's evaluator and credentials,
// which IAP's checks need.
func (s *computeService) useIAP(e *iameval.Enforcer, creds *metadata.Credentials) {
	if s == nil {
		return
	}
	s.creds = creds
	if e != nil {
		s.iapEval = e.Eval
	}
}

// ProjectNumber is a project's number (the registry's, as every service
// numbers it), for IAP's signed header's audience.
func (g computeIAPGate) ProjectNumber(project string) string {
	if project == "" {
		return ""
	}
	return strconv.FormatInt(resource.ProjectNumber(project), 10)
}

// iapNormalize is an IAP resource name with its project by ID and its
// backend service by name: Google takes projects/NUMBER/... and
// .../services/SERVICE_ID for the same resource.
func (s *computeService) iapNormalize(name string) string {
	p := strings.Split(name, "/")
	if len(p) < 2 || p[0] != "projects" {
		return name
	}
	if _, err := strconv.ParseInt(p[1], 10, 64); err == nil && s.registry != nil {
		if reg := s.registry(); reg != nil {
			if l, err := reg.List(); err == nil {
				for _, pr := range l {
					if strconv.FormatInt(resource.ProjectNumber(pr.ProjectID), 10) == p[1] {
						p[1] = pr.ProjectID
						break
					}
				}
			}
		}
	}
	if len(p) >= 6 && p[2] == "iap_web" && p[4] == "services" && (p[3] == "compute" || strings.HasPrefix(p[3], "compute-")) {
		if _, err := strconv.ParseUint(p[5], 10, 64); err == nil && s.api != nil {
			coll := "global/backendServices"
			if p[3] != "compute" {
				coll = "regions/" + strings.TrimPrefix(p[3], "compute-") + "/backendServices"
			}
			if out, err := s.api.Call("GET", p[1], coll, nil, nil); err == nil {
				for _, it := range listAny(out["items"]) {
					if m, ok := it.(map[string]any); ok && strAny(m["id"]) == p[5] {
						p[5] = strAny(m["name"])
					}
				}
			}
		}
	}
	return strings.Join(p, "/")
}
