package compute

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	corepb "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	authpb "github.com/envoyproxy/go-control-plane/envoy/service/auth/v3"
	extprocpb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/structpb"
)

// The CUSTOM action of the authorization policies (#2160), as
// load-balancing/docs/auth-policy/auth-policy-overview,
// service-extensions/docs/lb-extensions-overview,
// .../callouts-overview and the authzExtensions reference describe it
// (read 2026-11-22):
//
//   - the CUSTOM policies are evaluated first, then DENY, then ALLOW; a
//     CUSTOM policy whose provider rejects the request denies it, and the
//     others are not evaluated; a CUSTOM policy with HTTP rules delegates
//     only the requests that match them;
//   - a Cloud IAP provider applies IAP to the request, with the forwarding
//     rule as the IAP resource (iap_web/forwarding_rule[-REGION]/services/RULE);
//   - an authorization extension is called once the request's headers
//     have arrived, with no body, on its backend service over HTTP/2:
//     Envoy's ext_proc API (EXT_PROC_GRPC, the default) or ext_authz
//     (EXT_AUTHZ_GRPC); OK allows the request, with the header changes the
//     extension returns, and anything else denies it with the extension's
//     response; a header change the load balancer does not allow (pseudo
//     headers, host, X-Forwarded-*, X-Google-*, X-GFE-*, connection,
//     upgrade) answers 500; each message has the extension's timeout, and
//     a call that fails or times out answers 500 unless failOpen is set,
//     when the request goes on.

// AuthzVerdict is the authorization policies' decision on a request.
type AuthzVerdict struct {
	// Custom are the CUSTOM policies whose rules the request matches, in
	// evaluation order; each provider must allow it.
	Custom []AuthzCustom
	// Allowed is the DENY and ALLOW policies' decision, applied once every
	// custom provider has allowed the request.
	Allowed bool
	// Content are the CUSTOM policies of the CONTENT_AUTHZ profile, asked
	// last, with the request's body.
	Content []AuthzCustom
}

// AuthzCustom is one CUSTOM policy's provider.
type AuthzCustom struct {
	// Policy is the authorization policy's name.
	Policy string
	// IAP delegates the request to Cloud IAP.
	IAP bool
	// Extensions are the authorization extensions' names
	// (projects/P/locations/L/authzExtensions/E).
	Extensions []string
}

// AuthzExtension is a Service Extensions authorization extension
// (networkservices.googleapis.com authzExtensions) as the load balancers
// call it.
type AuthzExtension struct {
	Name string
	// Service is the backend service's relative name
	// (projects/P/regions/R/backendServices/B or projects/P/global/...).
	Service string
	// Authority is the :authority of the calls.
	Authority string
	// WireFormat is EXT_PROC_GRPC (also for "") or EXT_AUTHZ_GRPC.
	WireFormat string
	// Timeout is each message's timeout.
	Timeout time.Duration
	// FailOpen lets a request through when the call fails.
	FailOpen bool
	// ForwardHeaders are the client headers sent, every one when empty.
	ForwardHeaders []string
	// Metadata is sent under com.google.authz_extension.<Name>, with
	// {forwarding_rule_id} replaced.
	Metadata map[string]any
}

// AuthzExtensions are the authorization extensions, by name.
type AuthzExtensions interface {
	AuthzExtension(name string) (AuthzExtension, bool)
}

// errHeaderNotAllowed is an extension's header change the load balancer
// refuses.
var errHeaderNotAllowed = errors.New("header mutation not allowed")

// headerChangeAllowed is whether an extension may set or remove name.
func headerChangeAllowed(name string) bool {
	n := strings.ToLower(name)
	if n == "" || strings.HasPrefix(n, ":") || n == "host" || n == "connection" || n == "upgrade" ||
		n == "keep-alive" || n == "proxy-connection" || n == "transfer-encoding" || n == "te" {
		return false
	}
	for _, p := range []string{"x-forwarded", "x-google", "x-gfe", "x-envoy"} {
		if strings.HasPrefix(n, p) {
			return false
		}
	}
	return true
}

// extOutcome is an extension's answer.
type extOutcome struct {
	allow bool
	// set and remove change the request's headers; respAdd the response's.
	set     http.Header
	remove  []string
	respAdd http.Header
	// status, header and body are a denial's response.
	status int
	header http.Header
	body   []byte
}

// customCheck applies a CUSTOM policy's provider; false has answered.
func (h *lbHTTP) customCheck(w http.ResponseWriter, r *http.Request, scheme string, c AuthzCustom, req AuthzRequest, content bool) bool {
	if c.IAP {
		return h.s.iapCheck(w, r, h.s.ruleIAPObj(h.rule), scheme)
	}
	exts := h.s.opts.AuthzExt
	for _, name := range c.Extensions {
		var ext AuthzExtension
		ok := false
		if exts != nil {
			ext, ok = exts.AuthzExtension(name)
		}
		if !ok {
			lbError(w, http.StatusInternalServerError, "Internal Error")
			return false
		}
		out, err := h.s.callAuthzExtension(r.Context(), ext, r, req, content)
		if errors.Is(err, errHeaderNotAllowed) {
			lbError(w, http.StatusInternalServerError, "Internal Error")
			return false
		}
		if err != nil {
			if ext.FailOpen {
				continue
			}
			lbError(w, http.StatusInternalServerError, "Internal Error")
			return false
		}
		if !out.allow {
			for k, v := range out.header {
				w.Header()[k] = v
			}
			code := out.status
			if code < 200 || code > 599 {
				code = http.StatusForbidden
			}
			w.WriteHeader(code)
			_, _ = w.Write(out.body)
			return false
		}
		for _, k := range out.remove {
			r.Header.Del(k)
		}
		for k, v := range out.set {
			r.Header[k] = v
		}
		for k, v := range out.respAdd {
			for _, x := range v {
				w.Header().Add(k, x)
			}
		}
	}
	return true
}

// ruleIAPObj is the IAP view of a forwarding rule, as iapCheck reads a
// backend service's: IAP enabled with the Google-managed OAuth client, the
// forwarding rule as the resource and the signed header's audience.
func (s *Server) ruleIAPObj(rel string) obj {
	s.mu.Lock()
	rule := deepCopy(s.res[rel]).(obj)
	s.mu.Unlock()
	p := strings.Split(rel, "/")
	res, scope := "projects/"+p[1]+"/iap_web/forwarding_rule", "global"
	if len(p) > 3 && p[2] == "regions" {
		res += "-" + p[3]
		scope = "regions/" + p[3]
	}
	id := ""
	if rule != nil {
		id = fmt.Sprint(rule["id"])
	}
	return obj{"selfLink": rel, "id": id, "iap": obj{"enabled": true},
		"cloudburrowIAPResource": res + "/services/" + lastSeg(rel),
		"cloudburrowIAPAudience": scope + "/forwardingRules/" + id}
}

// extEndpoint is an address of the extension's backend service, and
// whether it speaks TLS (HTTP2) or cleartext (H2C).
func (s *Server) extEndpoint(ctx context.Context, svc string) (string, bool, error) {
	s.mu.Lock()
	set, ok := s.backendSetLocked(svc)
	s.mu.Unlock()
	if !ok {
		return "", false, fmt.Errorf("backend service %s not found", svc)
	}
	ep, ok := s.lb.bal.pick(set, "", nil, false)
	if !ok {
		return "", false, fmt.Errorf("backend service %s has no healthy endpoint", svc)
	}
	defer s.lb.bal.done(ep)
	tlsOn := set.protocol == "HTTP2" || set.protocol == "HTTPS"
	switch ep.kind {
	case "psc":
		a, err := s.pscTargetAddr(ep.neg)
		if err != nil {
			return "", false, err
		}
		if u, err := url.Parse(a); err == nil && u.Host != "" {
			a = u.Host
		}
		return a, tlsOn, nil
	case "internet", "serverless":
		return "", false, fmt.Errorf("an authorization extension's backend cannot be a %s endpoint", ep.kind)
	}
	a, err := s.dialAddr(ctx, ep, ep.port)
	return a, tlsOn, err
}

// callAuthzExtension calls an extension for r.
func (s *Server) callAuthzExtension(ctx context.Context, ext AuthzExtension, r *http.Request, req AuthzRequest, content bool) (extOutcome, error) {
	timeout := ext.Timeout
	if timeout <= 0 {
		timeout = time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	addr, tlsOn, err := s.extEndpoint(ctx, ext.Service)
	if err != nil {
		return extOutcome{}, err
	}
	creds := insecure.NewCredentials()
	if tlsOn {
		creds = credentials.NewTLS(&tls.Config{InsecureSkipVerify: true, NextProtos: []string{"h2"}}) //nolint:gosec // Google does not verify backends' certificates
	}
	opts := []grpc.DialOption{grpc.WithTransportCredentials(creds)}
	if ext.Authority != "" {
		opts = append(opts, grpc.WithAuthority(ext.Authority))
	}
	cc, err := grpc.NewClient("passthrough:///"+addr, opts...)
	if err != nil {
		return extOutcome{}, err
	}
	defer cc.Close()
	headers := forwardedHeaders(r, req, ext.ForwardHeaders)
	md := extMetadata(ext, req.Rule)
	if ext.WireFormat == "EXT_AUTHZ_GRPC" && !content {
		return callExtAuthz(ctx, cc, r, req, headers, md)
	}
	var body []byte
	if content {
		var err error
		if body, err = io.ReadAll(io.LimitReader(r.Body, maxContentAuthzBody+1)); err != nil {
			return extOutcome{}, err
		}
		if len(body) > maxContentAuthzBody {
			return extOutcome{}, errors.New("the request body is over the content authorization limit")
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
	}
	return callExtProc(ctx, cc, headers, md, body, content)
}

// maxContentAuthzBody is the most of a request body a CONTENT_AUTHZ
// extension is sent here (the body is buffered whole).
const maxContentAuthzBody = 32 << 20

// forwardedHeaders are the request's headers an extension gets, lower
// case, with the pseudo headers.
func forwardedHeaders(r *http.Request, req AuthzRequest, only []string) [][2]string {
	keep := map[string]bool{}
	for _, h := range only {
		keep[strings.ToLower(h)] = true
	}
	path := req.Path
	if path == "" {
		path = r.URL.RequestURI()
	}
	out := [][2]string{{":authority", r.Host}, {":method", r.Method}, {":path", path}, {":scheme", req.Scheme}}
	for k, vs := range r.Header {
		lk := strings.ToLower(k)
		if len(keep) > 0 && !keep[lk] {
			continue
		}
		for _, v := range vs {
			out = append(out, [2]string{lk, v})
		}
	}
	return out
}

// extMetadata is the metadata_context an extension gets.
func extMetadata(ext AuthzExtension, rule string) *corepb.Metadata {
	if len(ext.Metadata) == 0 {
		return nil
	}
	m := replaceRuleID(ext.Metadata, rule)
	st, err := structpb.NewStruct(m)
	if err != nil {
		return nil
	}
	return &corepb.Metadata{FilterMetadata: map[string]*structpb.Struct{"com.google.authz_extension." + ext.Name: st}}
}

func replaceRuleID(m map[string]any, rule string) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		switch x := v.(type) {
		case string:
			out[k] = strings.ReplaceAll(x, "{forwarding_rule_id}", rule)
		case map[string]any:
			out[k] = replaceRuleID(x, rule)
		default:
			out[k] = v
		}
	}
	return out
}

func headerValue(h *corepb.HeaderValue) string {
	if len(h.GetRawValue()) > 0 {
		return string(h.GetRawValue())
	}
	return h.GetValue()
}

// callExtAuthz is one ext_authz Check.
func callExtAuthz(ctx context.Context, cc *grpc.ClientConn, r *http.Request, req AuthzRequest, headers [][2]string, md *corepb.Metadata) (extOutcome, error) {
	hm := map[string]string{}
	for _, kv := range headers {
		if strings.HasPrefix(kv[0], ":") {
			continue
		}
		if cur, ok := hm[kv[0]]; ok {
			hm[kv[0]] = cur + "," + kv[1]
		} else {
			hm[kv[0]] = kv[1]
		}
	}
	_, query, _ := strings.Cut(req.Path, "?")
	sock := func(ip string, port int) *corepb.Address {
		return &corepb.Address{Address: &corepb.Address_SocketAddress{SocketAddress: &corepb.SocketAddress{
			Address: ip, PortSpecifier: &corepb.SocketAddress_PortValue{PortValue: uint32(port)}}}} //nolint:gosec // a port
	}
	in := &authpb.CheckRequest{Attributes: &authpb.AttributeContext{
		Source:      &authpb.AttributeContext_Peer{Address: sock(req.SourceIP, req.SourcePort)},
		Destination: &authpb.AttributeContext_Peer{Address: sock(req.DestIP, req.DestPort)},
		Request: &authpb.AttributeContext_Request{Http: &authpb.AttributeContext_HttpRequest{
			Id: r.Header.Get("X-Cloud-Trace-Context"), Method: r.Method, Headers: hm, Path: req.Path, Host: r.Host,
			Scheme: req.Scheme, Query: query, Protocol: r.Proto}},
		MetadataContext: md,
	}}
	resp, err := authpb.NewAuthorizationClient(cc).Check(ctx, in)
	if err != nil {
		return extOutcome{}, err
	}
	if codes.Code(resp.GetStatus().GetCode()) == codes.OK {
		out := extOutcome{allow: true, set: http.Header{}, respAdd: http.Header{}}
		ok := resp.GetOkResponse()
		for _, h := range ok.GetHeaders() {
			k := h.GetHeader().GetKey()
			if !headerChangeAllowed(k) {
				return extOutcome{}, errHeaderNotAllowed
			}
			ck := http.CanonicalHeaderKey(k)
			if h.GetAppend().GetValue() || h.GetAppendAction() == corepb.HeaderValueOption_APPEND_IF_EXISTS_OR_ADD {
				out.set[ck] = append(append([]string{}, r.Header.Values(ck)...), headerValue(h.GetHeader()))
			} else {
				out.set[ck] = []string{headerValue(h.GetHeader())}
			}
		}
		for _, k := range ok.GetHeadersToRemove() {
			if !headerChangeAllowed(k) {
				return extOutcome{}, errHeaderNotAllowed
			}
			out.remove = append(out.remove, k)
		}
		for _, h := range ok.GetResponseHeadersToAdd() {
			if !headerChangeAllowed(h.GetHeader().GetKey()) {
				return extOutcome{}, errHeaderNotAllowed
			}
			out.respAdd.Add(h.GetHeader().GetKey(), headerValue(h.GetHeader()))
		}
		return out, nil
	}
	d := resp.GetDeniedResponse()
	out := extOutcome{status: int(d.GetStatus().GetCode()), header: http.Header{}, body: []byte(d.GetBody())}
	if out.status == 0 {
		out.status = http.StatusForbidden
	}
	for _, h := range d.GetHeaders() {
		out.header.Add(h.GetHeader().GetKey(), headerValue(h.GetHeader()))
	}
	return out, nil
}

// callExtProc sends the request headers on an ext_proc stream, and the
// body when withBody (a CONTENT_AUTHZ policy's FULL_DUPLEX_STREAMED call,
// the body as one chunk), and reads an answer to each.
func callExtProc(ctx context.Context, cc *grpc.ClientConn, headers [][2]string, md *corepb.Metadata, body []byte, withBody bool) (extOutcome, error) {
	st, err := extprocpb.NewExternalProcessorClient(cc).Process(ctx)
	if err != nil {
		return extOutcome{}, err
	}
	hm := &corepb.HeaderMap{}
	for _, kv := range headers {
		hm.Headers = append(hm.Headers, &corepb.HeaderValue{Key: kv[0], RawValue: []byte(kv[1])})
	}
	if err := st.Send(&extprocpb.ProcessingRequest{
		Request:         &extprocpb.ProcessingRequest_RequestHeaders{RequestHeaders: &extprocpb.HttpHeaders{Headers: hm, EndOfStream: !withBody}},
		MetadataContext: md,
	}); err != nil {
		return extOutcome{}, err
	}
	want := 1
	if withBody {
		if err := st.Send(&extprocpb.ProcessingRequest{Request: &extprocpb.ProcessingRequest_RequestBody{
			RequestBody: &extprocpb.HttpBody{Body: body, EndOfStream: true}}}); err != nil {
			return extOutcome{}, err
		}
		want = 2
	}
	out := extOutcome{allow: true, set: http.Header{}, respAdd: http.Header{}}
	for got := 0; got < want; got++ {
		resp, err := st.Recv()
		if err == io.EOF {
			return extOutcome{}, errors.New("the extension closed the stream without an answer")
		}
		if err != nil {
			return extOutcome{}, err
		}
		if ir := resp.GetImmediateResponse(); ir != nil {
			_ = st.CloseSend()
			d := extOutcome{status: int(ir.GetStatus().GetCode()), header: http.Header{}, body: ir.GetBody()}
			for _, h := range ir.GetHeaders().GetSetHeaders() {
				d.header.Add(h.GetHeader().GetKey(), headerValue(h.GetHeader()))
			}
			return d, nil
		}
		var cr *extprocpb.CommonResponse
		switch {
		case resp.GetRequestHeaders() != nil:
			cr = resp.GetRequestHeaders().GetResponse()
		case resp.GetRequestBody() != nil:
			cr = resp.GetRequestBody().GetResponse()
		default:
			return extOutcome{}, fmt.Errorf("the extension answered %T to the request", resp.GetResponse())
		}
		mut := cr.GetHeaderMutation()
		for _, h := range mut.GetSetHeaders() {
			k := h.GetHeader().GetKey()
			if !headerChangeAllowed(k) {
				return extOutcome{}, errHeaderNotAllowed
			}
			out.set[http.CanonicalHeaderKey(k)] = []string{headerValue(h.GetHeader())}
		}
		for _, k := range mut.GetRemoveHeaders() {
			if !headerChangeAllowed(k) {
				return extOutcome{}, errHeaderNotAllowed
			}
			out.remove = append(out.remove, k)
		}
	}
	_ = st.CloseSend()
	return out, nil
}

// sourceIdentity fills a request's source VM identity: the instance whose
// internal address the request comes from, its first service account and
// its secure tags' value IDs.
func (s *Server) sourceIdentity(req *AuthzRequest) {
	req.SourceVM, req.SourceServiceAccount, req.SourceTagValues = s.InstanceIdentity(req.SourceIP)
}

// InstanceIdentity is the VM instance whose internal address is ip (its
// relative name), its first service account and its secure tags' value
// IDs; all empty when no instance has it, as for a client on this
// machine. Secure Web Proxy's source matchers read it too.
func (s *Server) InstanceIdentity(addr string) (vm, serviceAccount string, tagValues []int64) {
	ip := net.ParseIP(addr)
	if ip == nil || ip.IsLoopback() {
		return
	}
	s.mu.Lock()
	var inst obj
	var rel string
	for k, o := range s.res {
		if !strings.Contains(k, "/instances/") || str(o, "kind") != "compute#instance" {
			continue
		}
		for _, n := range listOf(o["networkInterfaces"]) {
			if str(objOf(n), "networkIP") == ip.String() {
				inst, rel = o, k
			}
		}
	}
	if inst != nil {
		inst = deepCopy(inst).(obj)
	}
	s.mu.Unlock()
	if inst == nil {
		return
	}
	vm = rel
	if sas := listOf(inst["serviceAccounts"]); len(sas) > 0 {
		serviceAccount = str(objOf(sas[0]), "email")
	}
	if f := s.opts.SourceTags; f != nil {
		p := strings.Split(rel, "/")
		if len(p) == 6 {
			full := "//compute.googleapis.com/projects/" + p[1] + "/zones/" + p[3] + "/instances/" + fmt.Sprint(inst["id"])
			tagValues = f(full)
		}
	}
	return
}

// UseAuthzExtensions sets Options.AuthzExt after New, before Start.
func (s *Server) UseAuthzExtensions(a AuthzExtensions) { s.opts.AuthzExt = a }
