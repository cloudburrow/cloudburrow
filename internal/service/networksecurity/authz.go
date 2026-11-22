package networksecurity

import (
	"crypto/sha256"
	"encoding/hex"
	"net"
	"strings"
	"sync"

	"cel.dev/cel-go/cel"
	nspb "cloud.google.com/go/networksecurity/apiv1/networksecuritypb"

	"github.com/cloudburrow/cloudburrow/internal/service/compute"
)

// The authorization policies' enforcement (#2160), as the AuthzPolicy
// reference and load-balancing/docs/auth-policy/auth-policy-overview
// describe it (read 2026-11-22): a DENY policy that matches denies the
// request; then, when the forwarding rule has ALLOW policies, one of them
// must match. A policy matches when one of its HTTP rules does (from AND
// to AND when); within a rule's source or operation, fields are ANDed and
// each field's values ORed. Principals match only a client certificate
// that frontend mutual TLS validated.

// whenEnv is the CEL environment of a rule's when: the request, source,
// destination and connection attributes.
var whenEnv = func() *cel.Env {
	env, err := cel.NewEnv(
		cel.Variable("request", cel.MapType(cel.StringType, cel.DynType)),
		cel.Variable("source", cel.MapType(cel.StringType, cel.DynType)),
		cel.Variable("destination", cel.MapType(cel.StringType, cel.DynType)),
		cel.Variable("connection", cel.MapType(cel.StringType, cel.DynType)),
	)
	if err != nil {
		panic(err)
	}
	return env
}()

var whenCache sync.Map // expression -> cel.Program

func compileWhen(e string) (cel.Program, error) {
	if p, ok := whenCache.Load(e); ok {
		return p.(cel.Program), nil
	}
	ast, iss := whenEnv.Compile(e)
	if iss != nil && iss.Err() != nil {
		return nil, iss.Err()
	}
	if ast.OutputType() != cel.BoolType && ast.OutputType() != cel.DynType {
		return nil, errNotBool
	}
	p, err := whenEnv.Program(ast)
	if err != nil {
		return nil, err
	}
	whenCache.Store(e, p)
	return p, nil
}

type whenErr string

func (e whenErr) Error() string { return string(e) }

const errNotBool = whenErr("the expression must be a boolean")

// Authorize evaluates the policies that target r's forwarding rule.
func (s *Server) Authorize(r compute.AuthzRequest) bool {
	pols := s.ForwardingRulePolicies(r.Rule)
	hasAllow, allowed := false, false
	for _, p := range pols {
		if p.GetAction() == nspb.AuthzPolicy_DENY && policyMatches(p, r) {
			return false
		}
	}
	for _, p := range pols {
		if p.GetAction() != nspb.AuthzPolicy_ALLOW {
			continue
		}
		hasAllow = true
		if policyMatches(p, r) {
			allowed = true
			break
		}
	}
	return !hasAllow || allowed
}

func policyMatches(p *nspb.AuthzPolicy, r compute.AuthzRequest) bool {
	rules := p.GetHttpRules()
	if len(rules) == 0 {
		rules = p.GetNetworkRules()
	}
	if len(rules) == 0 {
		return true
	}
	for _, rule := range rules {
		if ruleMatches(rule, r) {
			return true
		}
	}
	return false
}

func ruleMatches(rule *nspb.AuthzPolicy_AuthzRule, r compute.AuthzRequest) bool {
	if f := rule.GetFrom(); f != nil {
		ok := false
		for _, src := range f.GetSources() {
			if sourceMatches(src, r) {
				ok = true
			}
		}
		for _, src := range f.GetNotSources() {
			if !sourceMatches(src, r) {
				ok = true
			}
		}
		if !ok {
			return false
		}
	}
	if to := rule.GetTo(); to != nil {
		ok := false
		for _, op := range to.GetOperations() {
			if operationMatches(op, r) {
				ok = true
			}
		}
		for _, op := range to.GetNotOperations() {
			if !operationMatches(op, r) {
				ok = true
			}
		}
		if !ok {
			return false
		}
	}
	if w := rule.GetWhen(); w != "" {
		prg, err := compileWhen(w)
		if err != nil {
			return false
		}
		out, _, err := prg.Eval(whenActivation(r))
		if err != nil {
			return false
		}
		b, ok := out.Value().(bool)
		return ok && b
	}
	return true
}

func sourceMatches(src *nspb.AuthzPolicy_AuthzRule_From_RequestSource, r compute.AuthzRequest) bool {
	if len(src.GetPrincipals()) > 0 {
		ok := false
		for _, p := range src.GetPrincipals() {
			if principalMatches(p, r) {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	if len(src.GetIpBlocks()) > 0 {
		ip := net.ParseIP(r.SourceIP)
		ok := false
		for _, b := range src.GetIpBlocks() {
			bits := 32
			if p := net.ParseIP(b.GetPrefix()); p != nil && p.To4() == nil {
				bits = 128
			}
			n := &net.IPNet{IP: net.ParseIP(b.GetPrefix()), Mask: net.CIDRMask(int(b.GetLength()), bits)}
			if ip != nil && n.IP != nil && n.Contains(ip) {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

func principalMatches(p *nspb.AuthzPolicy_AuthzRule_Principal, r compute.AuthzRequest) bool {
	c := r.Cert
	if !c.Verified || c.Leaf == nil {
		return false
	}
	var vals []string
	switch p.GetPrincipalSelector() {
	case nspb.AuthzPolicy_AuthzRule_Principal_CLIENT_CERT_DNS_NAME_SAN:
		vals = c.Leaf.DNSNames
	case nspb.AuthzPolicy_AuthzRule_Principal_CLIENT_CERT_COMMON_NAME:
		vals = []string{c.Leaf.Subject.CommonName}
	default:
		for _, u := range c.Leaf.URIs {
			vals = append(vals, u.String())
		}
	}
	for _, v := range vals {
		if stringMatches(p.GetPrincipal(), v) {
			return true
		}
	}
	return false
}

func stringMatches(m *nspb.AuthzPolicy_AuthzRule_StringMatch, v string) bool {
	if m == nil {
		return false
	}
	if m.GetIgnoreCase() {
		v = strings.ToLower(v)
	}
	low := func(s string) string {
		if m.GetIgnoreCase() {
			return strings.ToLower(s)
		}
		return s
	}
	switch m.GetMatchPattern().(type) {
	case *nspb.AuthzPolicy_AuthzRule_StringMatch_Exact:
		return v == low(m.GetExact())
	case *nspb.AuthzPolicy_AuthzRule_StringMatch_Prefix:
		return strings.HasPrefix(v, low(m.GetPrefix()))
	case *nspb.AuthzPolicy_AuthzRule_StringMatch_Suffix:
		return strings.HasSuffix(v, low(m.GetSuffix()))
	case *nspb.AuthzPolicy_AuthzRule_StringMatch_Contains:
		return strings.Contains(v, low(m.GetContains()))
	}
	return false
}

func anyMatch(ms []*nspb.AuthzPolicy_AuthzRule_StringMatch, v string) bool {
	for _, m := range ms {
		if stringMatches(m, v) {
			return true
		}
	}
	return false
}

func operationMatches(op *nspb.AuthzPolicy_AuthzRule_To_RequestOperation, r compute.AuthzRequest) bool {
	if len(op.GetHosts()) > 0 && !anyMatch(op.GetHosts(), hostOnly(r.Host)) {
		return false
	}
	if len(op.GetPaths()) > 0 && !anyMatch(op.GetPaths(), r.Path) {
		return false
	}
	if len(op.GetSnis()) > 0 && (r.SNI == "" || !anyMatch(op.GetSnis(), r.SNI)) {
		return false
	}
	if len(op.GetMethods()) > 0 {
		ok := false
		for _, m := range op.GetMethods() {
			if m == r.Method {
				ok = true
			}
		}
		if !ok {
			return false
		}
	}
	if hs := op.GetHeaderSet().GetHeaders(); len(hs) > 0 {
		ok := false
		for _, h := range hs {
			for _, v := range r.Header.Values(h.GetName()) {
				if stringMatches(h.GetValue(), v) {
					ok = true
				}
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

func hostOnly(h string) string {
	if host, _, err := net.SplitHostPort(h); err == nil {
		return host
	}
	return h
}

func whenActivation(r compute.AuthzRequest) map[string]any {
	headers := map[string]any{}
	for k, v := range r.Header {
		headers[strings.ToLower(k)] = strings.Join(v, ",")
	}
	path, query, _ := strings.Cut(r.Path, "?")
	conn := map[string]any{"requested_server_name": r.SNI, "sha256_peer_certificate_digest": "", "uri_san_peer_certificate": "",
		"dns_san_peer_certificate": "", "subject_peer_certificate": ""}
	if c := r.Cert; c.Verified && c.Leaf != nil {
		sum := sha256.Sum256(c.Leaf.Raw)
		conn["sha256_peer_certificate_digest"] = hex.EncodeToString(sum[:])
		if len(c.Leaf.URIs) > 0 {
			conn["uri_san_peer_certificate"] = c.Leaf.URIs[0].String()
		}
		if len(c.Leaf.DNSNames) > 0 {
			conn["dns_san_peer_certificate"] = c.Leaf.DNSNames[0]
		}
		conn["subject_peer_certificate"] = c.Leaf.Subject.String()
	}
	return map[string]any{
		"request":     map[string]any{"headers": headers, "method": r.Method, "host": hostOnly(r.Host), "path": path, "query": query, "scheme": r.Scheme},
		"source":      map[string]any{"ip": r.SourceIP, "port": int64(r.SourcePort)},
		"destination": map[string]any{"ip": r.DestIP, "port": int64(r.DestPort)},
		"connection":  conn,
	}
}
