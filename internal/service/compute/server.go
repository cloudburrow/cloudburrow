// Package compute serves the VPC networking part of the Compute Engine API
// v1 (compute.googleapis.com, #1936) in the CLI process, over JSON as
// Google serves it: networks (auto and custom mode), subnetworks with their
// secondary ranges, firewall rules, routes, global and regional addresses,
// the global, regional and zonal operations, and the regions and zones.
//
// Nothing here carries traffic: a network is a record other services
// check their own network fields against (Memorystore's reserved ranges
// and PSC networks, Kubernetes Engine's cluster networks, Cloud SQL's
// private network), as Google checks them. Every mutation completes at
// once, and answers with a DONE operation in Compute's own shape
// (compute#operation), which clients poll or wait on as they do Google's.
//
// VM instances, disks, snapshots, machine types and the public images
// (#1963) are served too: each instance runs on a VMBackend (a systemd
// container in the instance's kind cluster), and its mutations answer
// operations that really run. Every other path answers 501.
package compute

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/hostguard"
	"github.com/cloudburrow/cloudburrow/internal/sysparams"
	"github.com/cloudburrow/cloudburrow/internal/vmrelay"
	"google.golang.org/grpc"
)

// Options configure a Server.
type Options struct {
	// Addr is the address to listen on; empty serves only through
	// ServeHTTP (tests, the console).
	Addr string
	// State is the JSON file the resources are kept in; empty keeps them
	// in memory only.
	State string
	// Now is time.Now when nil.
	Now func() time.Time
	// Extra serves a path under /compute/v1/ that this package does not:
	// the Kubernetes Engine API's node pool instance group managers
	// (#1913). It reports whether it answered.
	Extra func(w http.ResponseWriter, r *http.Request) bool
	// ExtraGroups are a zone's Kubernetes Engine node-pool instance group
	// managers, listed beside the zone's own managed instance groups.
	ExtraGroups func(project, zone string) map[string]map[string]any
	// NetworkUsers names the resources of other services that use a
	// network (projects/P/global/networks/N), which a network's deletion is
	// refused while they exist, as Google refuses it.
	NetworkUsers func(network string) []string
	// VMs runs VM instances and keeps their disks (#1963); nil answers
	// instances, disks and snapshots with 501.
	VMs VMBackend
	// ProjectOrganization is the organization a project is in ("" for a
	// project outside any) and whether the project is known, for Shared
	// VPC's organization rule (xpn.go); nil allows every project.
	ProjectOrganization func(project string) (org string, known bool)
	// ReadObject opens a Cloud Storage object of this instance and says its
	// size, for image import (rawDisk.source, #2139); a missing bucket or
	// object wraps ErrObjectNotFound. nil answers rawDisk with 501.
	ReadObject func(ctx context.Context, bucket, object string) (io.ReadCloser, int64, error)
	// MinFreeBytes is DefaultMinFreeBytes when zero.
	MinFreeBytes uint64
	// PrivateServices are the producer endpoints private services access
	// puts on a network (projects/P/global/networks/N): a Cloud SQL
	// instance's private IP (#2141), which the network's VMs reach. It is
	// called with the server's lock held, and must not call back into it.
	PrivateServices func(network string) []PrivateService
	// ServiceAttachment resolves a Private Service Connect endpoint's
	// target (#2141); nil serves no endpoint.
	ServiceAttachment ServiceAttachmentFunc
	// CPU reads a running VM's CPU time in seconds since it booted, the
	// autoscalers' load signal and compute.googleapis.com/instance/cpu/
	// utilization's; nil uses VMs' CPUUsage when it has one (VMCPU).
	CPU func(ctx context.Context, vm VMSpec) (float64, error)
	// AutoscaleEvery is how often the VMs' CPU is read and the autoscalers
	// run; 15 seconds when zero.
	AutoscaleEvery time.Duration
	// UpdateEvery is how often a managed instance group's PROACTIVE update
	// takes its next step (#2139); 2s when zero.
	UpdateEvery time.Duration
	// MIGRetryBase is the backoff before a managed instance group's
	// second retry of a VM it failed to make (#2220, migretry.go),
	// doubling to a minute; 5s when zero.
	MIGRetryBase time.Duration
	// LB reaches load balancers' backends (#2153, lbplane.go); nil
	// reaches only those this package can itself.
	LB LBBackends
	// CertMaps is Certificate Manager, which target HTTPS and SSL proxies'
	// certificate maps name (#2160, lbext.go); nil refuses certificateMap.
	CertMaps CertificateMaps
	// IAP is Identity-Aware Proxy, which a backend service's iap enforces
	// (#2160, lbext.go); nil refuses every request through an IAP-enabled
	// backend service.
	IAP IAPGate
	// NetSec is Network Security, which target HTTPS proxies'
	// serverTlsPolicy and authorizationPolicy and backend services'
	// securitySettings.clientTlsPolicy name, and whose authorization
	// policies target forwarding rules (#2160, lbmtls.go); nil refuses
	// those fields and authorizes every request.
	NetSec NetworkSecurity
	// XDSCredentialDir is where the xDS API's TLS bootstrap files are
	// written: its CA and each project's workload credentials (#2160,
	// lbxdssec.go); empty serves only the plaintext bootstrap.
	XDSCredentialDir string
	// AuthzExt are Service Extensions' authorization extensions, which a
	// CUSTOM authorization policy calls (#2160, lbauthzext.go); nil answers
	// 500 to a request such a policy delegates.
	AuthzExt AuthzExtensions
	// SourceTags are the permanent IDs of the tag values a resource carries
	// (its effective tags), by full resource name
	// (//compute.googleapis.com/projects/P/zones/Z/instances/ID); nil
	// gives instances no tags.
	SourceTags func(fullName string) []int64
	// Gateways are Secure Web Proxy's gateways, which VMs reach at their
	// VPC addresses through the relay (#2160, vmrelay.go); nil relays
	// only load balancers.
	Gateways RelayGateways
	// RelayBind is the address the relay listens on, 127.0.0.1 when
	// empty: where VMs' pods reach this machine (vmrelay.go).
	RelayBind string
	// FirewallLog writes a Firewall Rules Logging record to Cloud Logging
	// (#2180, relayfirewall.go); nil logs nothing.
	FirewallLog func(FirewallLogEntry)
	// GuestReach is the address (IP:port) at which a VM's pod reaches the
	// listener at loopback (127.0.0.1:port) on this machine, "" when it
	// cannot: internal load balancers and Private Service Connect
	// endpoints are carried from VMs to their listeners through it
	// (vpcservices.go). nil carries neither.
	GuestReach func(loopback string) string
	// Tokens mints the access and identity tokens a VM's metadata server
	// hands out for its service account (#2177, guestmetadata.go); nil
	// answers them 503.
	Tokens GuestTokens
	// IntegrityEvents writes a Shielded VM's integrity monitoring event
	// (#2179, vtpm.go), the jsonPayload of a
	// compute.googleapis.com/shielded_vm_integrity entry of the
	// instance's gce_instance resource; nil writes none.
	IntegrityEvents func(ctx context.Context, project, zone, instanceID string, payload map[string]any)
}

// PrivateService is one private services access endpoint: its address and
// port on the network, and where its connections go, an address the VMs'
// pods reach (a cluster Service's).
type PrivateService struct {
	Name       string
	IP         string
	Port       int
	TargetIP   string
	TargetPort int
	// ExtraPorts are further ports of the endpoint, each sent on to the
	// same port of the target: a Cloud SQL instance's managed connection
	// pooler, on 6432 beside its server (#2141).
	ExtraPorts []int
}

// obj is one resource, as its JSON is kept: link fields hold relative
// names (projects/P/...), made absolute when it is returned.
type obj = map[string]any

// Server is the Compute Engine API's VPC networking.
type Server struct {
	opts Options

	xdsOnce sync.Once
	xdsGRPC *grpc.Server

	mu sync.Mutex
	// kms is Cloud KMS, for disks' customer-managed keys (#2206).
	kms KeyService
	// openKeys are the unwrapped data keys of encrypted disks and
	// snapshots this process has opened, by wrapped key (diskkeys.go);
	// never saved.
	openKeys map[string][]byte
	// csekPEM is the CSEK ingress key (PKCS #8 PEM), kept in the state
	// file; csekPriv is it parsed.
	csekPEM  string
	csekPriv *rsa.PrivateKey
	res      map[string]obj // by relative name: projects/P/global/networks/N
	ops      map[string]obj // by relative name: projects/P/global/operations/O
	// defaultDeleted are the projects whose default network was deleted,
	// so it is not made again.
	defaultDeleted map[string]bool
	// seeded are the projects whose default network has been made.
	seeded map[string]bool
	// automated are the forwarding rules service connectivity automation
	// made (#2141), which are not deleted directly.
	automated map[string]bool
	// gateways are Cloud Run's ways onto VPC networks, by key
	// (gateways.go).
	gateways map[string]*runGateway
	// bgpStates are what Cloud Router's speakers reported, by speaker key
	// (bgpspeakers.go).
	bgpStates map[string]BGPState
	// waiters are the running operations, closed when each is done.
	waiters map[string]chan struct{}
	// queue is, by resource, the last operation started on it, closed when
	// it is done, and queued how many of its operations have not finished.
	// A resource's operations run one after another, in the order they were
	// asked for, as Google's do: a Stop asked while an attachDisk is still
	// restarting the VM's container waits for it, so the attach can neither
	// restart a VM that was stopped nor set it RUNNING again.
	queue  map[string]chan struct{}
	queued map[string]int
	// pods are the pod IPs the running VMs' guests last reported (#1968),
	// by instance.
	pods map[string]podSeen
	// natLow is, by NAT gateway key and VM IP, since when a VM of a gateway
	// with dynamic port allocation has used few of its ports
	// (natgateways.go), so its ports are halved after natShrinkAfter.
	natLow map[string]time.Time

	// stopLoop ends the schedules' runner (resourcepolicies.go) and the
	// autoscalers' (autoscalers.go); loops is done once both have ended.
	stopLoop context.CancelFunc
	loops    sync.WaitGroup
	// scale is the autoscalers' memory: the VMs' CPU readings and each
	// autoscaler's recent recommendations and sizes (autoscalers.go).
	scale scaleState
	// lb is the load balancers' data plane (lbplane.go).
	lb *lbPlane

	srv *http.Server
	ln  net.Listener
	// relay is the listener VMs' guests relay their connections to load
	// balancers and gateways through (vmrelay.go).
	relay *vmrelay.Server
	// relayAt is the relay's in-cluster address (UseRelayAddress).
	relayAt func() string
	// fwDenied is when each denied relayed 5-tuple was last logged
	// (relayfirewall.go).
	fwDenied map[string]time.Time
	// ca issues the vTPMs' EK and AK certificates (vtpm.go).
	ca *vtpmCA
	// gaRate is each instance's guest attribute writes (guestmetadata.go).
	gaRate map[string][]time.Time
}

// New returns a Server with its state restored.
func New(opts Options) (*Server, error) {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	s := &Server{opts: opts, res: map[string]obj{}, ops: map[string]obj{},
		defaultDeleted: map[string]bool{}, seeded: map[string]bool{}, automated: map[string]bool{}, waiters: map[string]chan struct{}{},
		queue: map[string]chan struct{}{}, queued: map[string]int{}, lb: newLBPlane()}
	if err := s.restore(); err != nil {
		return nil, err
	}
	return s, nil
}

// Name is the lifecycle name.
func (s *Server) Name() string { return "compute" }

// Start listens on Options.Addr.
func (s *Server) Start(context.Context) error {
	if s.opts.VMs != nil && s.stopLoop == nil {
		ctx, cancel := context.WithCancel(context.Background())
		s.stopLoop = cancel
		s.loops.Add(3)
		go func() { defer s.loops.Done(); s.RunSnapshotSchedules(ctx) }()
		go func() { defer s.loops.Done(); s.autoscaleLoop(ctx) }()
		go func() { defer s.loops.Done(); s.rollLoop(ctx) }()
	}
	s.lbStart()
	s.startRelay()
	if s.opts.Addr == "" {
		return nil
	}
	ln, err := net.Listen("tcp", s.opts.Addr)
	if err != nil {
		return fmt.Errorf("compute: %w", err)
	}
	ln = newMDSListener(s, ln)
	s.ln = ln
	s.srv = &http.Server{Handler: hostguard.Wrap(sysparams.Wrap(s, sysparams.Options{V1Errors: true})), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = s.srv.Serve(ln) }()
	return nil
}

// Stop stops listening.
func (s *Server) Stop(ctx context.Context) error {
	if s.stopLoop != nil {
		s.stopLoop()
		s.stopLoop = nil
		s.loops.Wait()
	}
	s.lbStop()
	s.stopRelay()
	if s.srv == nil {
		return nil
	}
	return s.srv.Shutdown(ctx)
}

// Addr is the bound address, or "" before Start.
func (s *Server) Addr() string {
	if s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}

// state is what the State file holds.
type state struct {
	Resources      map[string]obj  `json:"resources"`
	Operations     map[string]obj  `json:"operations"`
	DefaultDeleted map[string]bool `json:"defaultDeleted,omitempty"`
	Seeded         map[string]bool `json:"seeded,omitempty"`
	// Automated are the Private Service Connect endpoints service
	// connectivity automation made (#2141, autoendpoints.go).
	Automated map[string]bool `json:"automated,omitempty"`
	// CSEKIngress is the key rsaEncryptedKey is unwrapped with
	// (diskkeys.go).
	CSEKIngress string `json:"csekIngress,omitempty"`
}

func (s *Server) restore() error {
	if s.opts.State == "" {
		return nil
	}
	b, err := os.ReadFile(s.opts.State)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var st state
	if err := json.Unmarshal(b, &st); err != nil {
		return fmt.Errorf("compute: %s: %w", s.opts.State, err)
	}
	if st.Resources != nil {
		s.res = st.Resources
	}
	if st.Operations != nil {
		s.ops = st.Operations
	}
	if st.DefaultDeleted != nil {
		s.defaultDeleted = st.DefaultDeleted
	}
	if st.Automated != nil {
		s.automated = st.Automated
	}
	if st.Seeded != nil {
		s.seeded = st.Seeded
	}
	s.csekPEM = st.CSEKIngress
	return nil
}

// saveLocked writes the state file; s.mu is held.
func (s *Server) saveLocked() error {
	if s.opts.State == "" {
		return nil
	}
	b, err := json.Marshal(state{Resources: s.res, Operations: s.ops, DefaultDeleted: s.defaultDeleted, Seeded: s.seeded, Automated: s.automated, CSEKIngress: s.csekPEM})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.opts.State), 0o700); err != nil {
		return err
	}
	tmp := s.opts.State + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.opts.State)
}

// ResetProject forgets one project's resources, or every project's for "".
func (s *Server) ResetProject(project string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if project == "" {
		s.res, s.ops = map[string]obj{}, map[string]obj{}
		s.defaultDeleted, s.seeded, s.automated = map[string]bool{}, map[string]bool{}, map[string]bool{}
		return s.saveLocked()
	}
	prefix := "projects/" + project + "/"
	for k := range s.automated {
		if strings.HasPrefix(k, prefix) {
			delete(s.automated, k)
		}
	}
	for k := range s.res {
		if strings.HasPrefix(k, prefix) {
			delete(s.res, k)
		}
	}
	for k := range s.ops {
		if strings.HasPrefix(k, prefix) {
			delete(s.ops, k)
		}
	}
	delete(s.defaultDeleted, project)
	delete(s.seeded, project)
	return s.saveLocked()
}

// apiError is an error in Compute's JSON shape.
type apiError struct {
	code   int
	reason string
	msg    string
}

func (e *apiError) Error() string { return e.msg }

// Status is the error's HTTP status and reason.
func (e *apiError) Status() (int, string) { return e.code, e.reason }

func errf(code int, reason, format string, args ...any) error {
	return &apiError{code: code, reason: reason, msg: fmt.Sprintf(format, args...)}
}

func invalid(format string, args ...any) error {
	return errf(http.StatusBadRequest, "invalid", format, args...)
}

func notFound(name string) error {
	return errf(http.StatusNotFound, "notFound", "The resource '%s' was not found", name)
}

func unimplemented(format string, args ...any) error {
	return errf(http.StatusNotImplemented, "notImplemented", "Not available locally: "+format, args...)
}

// inUse is Google's refusal to delete a resource another uses.
func inUse(kind, name, user string) error {
	return errf(http.StatusBadRequest, "resourceInUseByAnotherResource",
		"The %s resource '%s' is already being used by '%s'", kind, name, user)
}

// ErrorStatus is err's HTTP status, for callers in process (the console).
func ErrorStatus(err error) int {
	var ae *apiError
	if errors.As(err, &ae) {
		return ae.code
	}
	return http.StatusInternalServerError
}

func writeError(w http.ResponseWriter, err error) {
	var ae *apiError
	if !errors.As(err, &ae) {
		ae = &apiError{code: http.StatusInternalServerError, reason: "backendError", msg: err.Error()}
	}
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(ae.code)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
		"code": ae.code, "message": ae.msg,
		"errors": []map[string]string{{"domain": "global", "reason": ae.reason, "message": ae.msg}}}})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	_ = json.NewEncoder(w).Encode(v)
}

// newID is a resource's numeric ID, as a decimal string as Compute's JSON
// carries uint64s.
func newID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return strconv.FormatUint(binary.BigEndian.Uint64(b[:])>>1, 10)
}

// sameFingerprint is whether a supplied fingerprint is cur. A fingerprint
// is bytes (format "byte" in the discovery document), so a client may
// send it base64url-encoded, as gcloud's apitools does, rather than in the
// standard encoding the server writes: '-' and '_' for '+' and '/'.
func sameFingerprint(supplied, cur string) bool {
	if supplied == cur {
		return true
	}
	a, err1 := base64.URLEncoding.DecodeString(strings.NewReplacer("+", "-", "/", "_").Replace(supplied))
	b, err2 := base64.StdEncoding.DecodeString(cur)
	return err1 == nil && err2 == nil && string(a) == string(b)
}

func newFingerprint() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return base64.StdEncoding.EncodeToString(b[:])
}

func (s *Server) stamp() string {
	return s.opts.Now().Format("2006-01-02T15:04:05.000-07:00")
}

// linkKeys are the fields that hold another resource's name, made absolute
// on the way out.
var linkKeys = map[string]bool{"selfLink": true, "network": true, "subnetwork": true, "region": true, "zone": true,
	"targetLink": true, "nextHopGateway": true, "nextHopNetwork": true, "nextHopInstance": true, "nextHopVpnTunnel": true,
	"subnetworks": true, "users": true, "zones": true, "machineType": true, "source": true, "sourceImage": true,
	"sourceSnapshot": true, "sourceDisk": true, "type": true, "resourcePolicies": true, "sourceInstance": true,
	"sourceInstanceTemplate": true, "sourceMachineImage": true, "instanceTemplate": true, "instanceGroup": true,
	"sourceSnapshotSchedulePolicy": true, "instance": true, "replacement": true, "replicaZones": true,
	// Private Service Connect (#2140, psc.go).
	"target": true, "targetService": true, "natSubnets": true, "endpoint": true, "consumerNetwork": true,
	// Regional disks and consistency groups (#2206).
	"disk": true, "consistencyGroupPolicy": true, "sourceConsistencyGroupPolicy": true,
	// Workload policies on managed instance groups (#2206).
	"workloadPolicy": true}

// renderNested makes the link fields of objects inside lists and objects
// absolute (an instance's disks and network interfaces).
func renderNested(v any, base string) any {
	switch t := v.(type) {
	case map[string]any:
		return render(t, base)
	case []any:
		l := make([]any, len(t))
		for i, e := range t {
			l[i] = renderNested(e, base)
		}
		return l
	}
	return v
}

// render is o with its link fields made absolute under base.
func render(o obj, base string) obj {
	out := make(obj, len(o))
	for k, v := range o {
		if strings.HasPrefix(k, "_") {
			continue // kept, never returned
		}
		if linkKeys[k] {
			switch t := v.(type) {
			case string:
				if k == "sourceImage" && imageProjectLink(t) {
					// A public image lives in Google's image project, and
					// Google names it under www.googleapis.com whichever
					// endpoint was called: Terraform's provider recognises a
					// boot disk's image only in that form (#1965).
					out[k] = abs(t, googleBase)
					continue
				}
				out[k] = abs(t, base)
				continue
			case []any:
				l := make([]any, len(t))
				for i, e := range t {
					if es, ok := e.(string); ok {
						l[i] = abs(es, base)
					} else {
						l[i] = e
					}
				}
				out[k] = l
				continue
			}
		}
		if k != "items" {
			v = renderNested(v, base)
		}
		out[k] = v
	}
	if sl, ok := out["selfLink"].(string); ok {
		if id, ok := o["id"].(string); ok {
			if i := strings.LastIndex(sl, "/"); i >= 0 {
				out["selfLinkWithId"] = sl[:i+1] + id
			}
		}
	}
	return out
}

// googleBase is the base of Google's own Compute Engine links.
const googleBase = "https://www.googleapis.com/compute/v1/"

// imageProjectLink reports whether rel names an image of a public image
// project.
func imageProjectLink(rel string) bool {
	for _, p := range PublicImages {
		if strings.HasPrefix(rel, "projects/"+p.Project+"/global/images/") {
			return true
		}
	}
	return false
}

func abs(rel, base string) string {
	if strings.HasPrefix(rel, "projects/") {
		return base + rel
	}
	return rel
}

// Rel is the relative name ("projects/P/...") a link names: a full URL of
// any host, a projects/... name, or one relative to project (global/...,
// regions/..., zones/...). Anything else is returned as it is.
func Rel(link, project string) string {
	if i := strings.Index(link, "/projects/"); i >= 0 && strings.Contains(link, "://") {
		return link[i+1:]
	}
	switch {
	case strings.HasPrefix(link, "projects/"):
		return link
	case strings.HasPrefix(link, "global/"), strings.HasPrefix(link, "regions/"), strings.HasPrefix(link, "zones/"):
		return "projects/" + project + "/" + link
	}
	return link
}

// base is the absolute prefix of r's own selfLinks.
func base(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host + "/compute/v1/"
}

// list pages through the objects under prefix (a collection's relative
// name and a slash), filtered.
func (s *Server) listLocked(m map[string]obj, prefix string, q map[string][]string) ([]obj, string, error) {
	f, err := parseFilter(first(q, "filter"))
	if err != nil {
		return nil, "", err
	}
	var keys []string
	for k := range m {
		if strings.HasPrefix(k, prefix) && !strings.Contains(k[len(prefix):], "/") {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	var all []obj
	for _, k := range keys {
		if f.match(m[k]) {
			all = append(all, m[k])
		}
	}
	return page(all, q)
}

func page(all []obj, q map[string][]string) ([]obj, string, error) {
	start := 0
	if t := first(q, "pageToken"); t != "" {
		n, err := strconv.Atoi(t)
		if err != nil || n < 0 || n > len(all) {
			return nil, "", invalid("Invalid value for field 'pageToken': '%s'. Supplied page token is not valid.", t)
		}
		start = n
	}
	size := 500
	if v := first(q, "maxResults"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > 500 {
			return nil, "", invalid("Invalid value for field 'maxResults': '%s'. Must be less than or equal to 500", v)
		}
		if n > 0 {
			size = n
		}
	}
	end := min(start+size, len(all))
	next := ""
	if end < len(all) {
		next = strconv.Itoa(end)
	}
	return all[start:end], next, nil
}

func first(q map[string][]string, k string) string {
	if v := q[k]; len(v) > 0 {
		return v[0]
	}
	return ""
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
