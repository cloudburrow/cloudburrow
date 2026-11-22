package certificatemanager

import (
	"context"
	"crypto/tls"
	"sort"
	"strings"

	cmpb "cloud.google.com/go/certificatemanager/apiv1/certificatemanagerpb"
	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/protobuf/proto"

	"github.com/cloudburrow/cloudburrow/internal/aipfilter"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// Certificate maps and their entries
// (https://docs.cloud.google.com/certificate-manager/docs/maps and the
// certificateMaps and certificateMapEntries REST references, read
// 2026-11-22): a map is global; an entry matches a hostname (exact or a
// wildcard such as *.example.com) or is the map's PRIMARY entry, served
// when the handshake has no SNI or the SNI matches no entry, and names up
// to four certificates.

const maxCertsPerEntry = 4

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// mapOfEntry is an entry's map name.
func mapOfEntry(entry string) string {
	if i := strings.Index(entry, "/"+collEntries+"/"); i > 0 {
		return entry[:i]
	}
	return ""
}

// normMap is a map name as a proxy's certificateMap gives it: a full
// resource name (//certificatemanager.googleapis.com/projects/...) or a
// relative one.
func normMap(name string) string {
	name = strings.TrimPrefix(name, "https://certificatemanager.googleapis.com/v1/")
	return strings.TrimPrefix(name, "//certificatemanager.googleapis.com/")
}

func (s *Server) CreateCertificateMap(_ context.Context, req *cmpb.CreateCertificateMapRequest) (*longrunningpb.Operation, error) {
	if _, _, err := parseParent(req.GetParent(), true); err != nil {
		return nil, err
	}
	if err := checkID("certificate_map_id", req.GetCertificateMapId()); err != nil {
		return nil, err
	}
	m, _ := proto.Clone(req.GetCertificateMap()).(*cmpb.CertificateMap)
	if m == nil {
		m = &cmpb.CertificateMap{}
	}
	name := req.GetParent() + "/" + collMaps + "/" + req.GetCertificateMapId()
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.res[name]; ok {
		return nil, apierror.AlreadyExists("CertificateMap '%s' already exists.", name)
	}
	m.Name, m.GclbTargets = name, nil
	stamp(m, s.opts.Now(), true)
	s.res[name] = m
	return s.opLocked(name, "create", m)
}

// targetsOf are the target proxies that name a map, read outside the lock.
func (s *Server) targetsOf(project string) map[string][]*cmpb.CertificateMap_GclbTarget {
	s.mu.Lock()
	f := s.targets
	s.mu.Unlock()
	out := map[string][]*cmpb.CertificateMap_GclbTarget{}
	if f == nil {
		return out
	}
	for _, t := range f(project) {
		g := &cmpb.CertificateMap_GclbTarget{}
		if t.HTTPSProxy != "" {
			g.TargetProxy = &cmpb.CertificateMap_GclbTarget_TargetHttpsProxy{TargetHttpsProxy: t.HTTPSProxy}
		} else {
			g.TargetProxy = &cmpb.CertificateMap_GclbTarget_TargetSslProxy{TargetSslProxy: t.SSLProxy}
		}
		for _, ip := range t.IPs {
			g.IpConfigs = append(g.IpConfigs, &cmpb.CertificateMap_GclbTarget_IpConfig{IpAddress: ip.IP, Ports: ip.Ports})
		}
		m := normMap(t.Map)
		out[m] = append(out[m], g)
	}
	return out
}

func (s *Server) GetCertificateMap(_ context.Context, req *cmpb.GetCertificateMapRequest) (*cmpb.CertificateMap, error) {
	if err := checkName(req.GetName(), collMaps); err != nil {
		return nil, err
	}
	ts := s.targetsOf(projectOf(req.GetName()))
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.res[req.GetName()].(*cmpb.CertificateMap)
	if !ok {
		return nil, notFound(collMaps, req.GetName())
	}
	o := proto.Clone(m).(*cmpb.CertificateMap)
	o.GclbTargets = ts[o.GetName()]
	return o, nil
}

func (s *Server) ListCertificateMaps(_ context.Context, req *cmpb.ListCertificateMapsRequest) (*cmpb.ListCertificateMapsResponse, error) {
	project, _, err := parseParent(req.GetParent(), false)
	if err != nil {
		return nil, err
	}
	ts := s.targetsOf(project)
	s.mu.Lock()
	var items []*cmpb.CertificateMap
	for _, k := range s.childrenLocked(req.GetParent(), collMaps) {
		o := proto.Clone(s.res[k]).(*cmpb.CertificateMap)
		o.GclbTargets = ts[k]
		items = append(items, o)
	}
	s.mu.Unlock()
	page, next, err := aipfilter.ListPage("maps:"+req.GetParent(), req.GetFilter(), req.GetOrderBy(), items, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, err
	}
	return &cmpb.ListCertificateMapsResponse{CertificateMaps: page, NextPageToken: next}, nil
}

func (s *Server) UpdateCertificateMap(_ context.Context, req *cmpb.UpdateCertificateMapRequest) (*longrunningpb.Operation, error) {
	in := req.GetCertificateMap()
	if err := checkName(in.GetName(), collMaps); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.res[in.GetName()].(*cmpb.CertificateMap)
	if !ok {
		return nil, notFound(collMaps, in.GetName())
	}
	next := proto.Clone(cur).(*cmpb.CertificateMap)
	if err := applyMask(next, in, req.GetUpdateMask(), "description", "labels"); err != nil {
		return nil, err
	}
	stamp(next, s.opts.Now(), false)
	s.res[cur.GetName()] = next
	return s.opLocked(cur.GetName(), "update", next)
}

func (s *Server) DeleteCertificateMap(_ context.Context, req *cmpb.DeleteCertificateMapRequest) (*longrunningpb.Operation, error) {
	if err := checkName(req.GetName(), collMaps); err != nil {
		return nil, err
	}
	ts := s.targetsOf(projectOf(req.GetName()))
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.res[req.GetName()]; !ok {
		return nil, notFound(collMaps, req.GetName())
	}
	if t := ts[req.GetName()]; len(t) > 0 {
		user := t[0].GetTargetHttpsProxy() + t[0].GetTargetSslProxy()
		return nil, apierror.FailedPrecondition("CertificateMap '%s' is in use by '%s': detach it from the target proxy first.", req.GetName(), user)
	}
	if es := s.childrenLocked(req.GetName(), collEntries); len(es) > 0 {
		return nil, apierror.FailedPrecondition("CertificateMap '%s' has certificate map entries: delete them first ('%s').", req.GetName(), es[0])
	}
	delete(s.res, req.GetName())
	return s.opLocked(req.GetName(), "delete", nil)
}

// checkEntryLocked checks an entry's match and certificates against its
// map's other entries.
func (s *Server) checkEntryLocked(mapName, name string, e *cmpb.CertificateMapEntry) error {
	switch {
	case e.GetHostname() != "":
		if !validDomain(e.GetHostname(), true) {
			return apierror.InvalidArgument("Invalid value for field 'hostname': '%s'. It must be a fully qualified domain name or a wildcard such as *.example.com.", e.GetHostname())
		}
	case e.GetMatcher() == cmpb.CertificateMapEntry_PRIMARY:
	default:
		return apierror.InvalidArgument("Invalid argument: one of hostname or matcher (PRIMARY) must be set.")
	}
	for _, k := range s.childrenLocked(mapName, collEntries) {
		o := s.res[k].(*cmpb.CertificateMapEntry)
		if k == name {
			continue
		}
		if e.GetHostname() != "" && strings.EqualFold(o.GetHostname(), e.GetHostname()) {
			return apierror.AlreadyExists("CertificateMapEntry '%s' already matches hostname '%s' in this map.", k, e.GetHostname())
		}
		if e.GetMatcher() == cmpb.CertificateMapEntry_PRIMARY && o.GetMatcher() == cmpb.CertificateMapEntry_PRIMARY {
			return apierror.AlreadyExists("CertificateMapEntry '%s' is already the map's PRIMARY entry.", k)
		}
	}
	if len(e.GetCertificates()) > maxCertsPerEntry {
		return apierror.InvalidArgument("A certificate map entry can have at most %d certificates, got %d.", maxCertsPerEntry, len(e.GetCertificates()))
	}
	for _, c := range e.GetCertificates() {
		if err := checkName(c, collCerts); err != nil {
			return apierror.InvalidArgument("Invalid value for field 'certificates': '%s'.", c)
		}
		if _, ok := s.res[c]; !ok {
			return apierror.InvalidArgument("Certificate '%s' not found.", c)
		}
		if locationOf(c) != "global" {
			return apierror.InvalidArgument("Certificate '%s' must be in location 'global' to be used in a certificate map.", c)
		}
	}
	return nil
}

func (s *Server) CreateCertificateMapEntry(_ context.Context, req *cmpb.CreateCertificateMapEntryRequest) (*longrunningpb.Operation, error) {
	parent := req.GetParent()
	if err := checkName(parent, collMaps); err != nil {
		return nil, err
	}
	if err := checkID("certificate_map_entry_id", req.GetCertificateMapEntryId()); err != nil {
		return nil, err
	}
	e, _ := proto.Clone(req.GetCertificateMapEntry()).(*cmpb.CertificateMapEntry)
	if e == nil {
		e = &cmpb.CertificateMapEntry{}
	}
	name := parent + "/" + collEntries + "/" + req.GetCertificateMapEntryId()
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.res[parent]; !ok {
		return nil, notFound(collMaps, parent)
	}
	if _, ok := s.res[name]; ok {
		return nil, apierror.AlreadyExists("CertificateMapEntry '%s' already exists.", name)
	}
	if err := s.checkEntryLocked(parent, name, e); err != nil {
		return nil, err
	}
	e.Name, e.State = name, cmpb.ServingState_ACTIVE
	stamp(e, s.opts.Now(), true)
	s.res[name] = e
	return s.opLocked(name, "create", e)
}

func (s *Server) GetCertificateMapEntry(_ context.Context, req *cmpb.GetCertificateMapEntryRequest) (*cmpb.CertificateMapEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.getLocked(collEntries, req.GetName())
	if err != nil {
		return nil, err
	}
	return m.(*cmpb.CertificateMapEntry), nil
}

func (s *Server) ListCertificateMapEntries(_ context.Context, req *cmpb.ListCertificateMapEntriesRequest) (*cmpb.ListCertificateMapEntriesResponse, error) {
	if err := checkName(req.GetParent(), collMaps); err != nil {
		return nil, err
	}
	s.mu.Lock()
	if _, ok := s.res[req.GetParent()]; !ok {
		s.mu.Unlock()
		return nil, notFound(collMaps, req.GetParent())
	}
	var items []*cmpb.CertificateMapEntry
	for _, k := range s.childrenLocked(req.GetParent(), collEntries) {
		items = append(items, proto.Clone(s.res[k]).(*cmpb.CertificateMapEntry))
	}
	s.mu.Unlock()
	page, next, err := aipfilter.ListPage("entries:"+req.GetParent(), req.GetFilter(), req.GetOrderBy(), items, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, err
	}
	return &cmpb.ListCertificateMapEntriesResponse{CertificateMapEntries: page, NextPageToken: next}, nil
}

func (s *Server) UpdateCertificateMapEntry(_ context.Context, req *cmpb.UpdateCertificateMapEntryRequest) (*longrunningpb.Operation, error) {
	in := req.GetCertificateMapEntry()
	if err := checkName(in.GetName(), collEntries); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.res[in.GetName()].(*cmpb.CertificateMapEntry)
	if !ok {
		return nil, notFound(collEntries, in.GetName())
	}
	next := proto.Clone(cur).(*cmpb.CertificateMapEntry)
	if err := applyMask(next, in, req.GetUpdateMask(), "description", "labels", "certificates"); err != nil {
		return nil, err
	}
	if err := s.checkEntryLocked(mapOfEntry(cur.GetName()), cur.GetName(), next); err != nil {
		return nil, err
	}
	stamp(next, s.opts.Now(), false)
	s.res[cur.GetName()] = next
	return s.opLocked(cur.GetName(), "update", next)
}

func (s *Server) DeleteCertificateMapEntry(_ context.Context, req *cmpb.DeleteCertificateMapEntryRequest) (*longrunningpb.Operation, error) {
	if err := checkName(req.GetName(), collEntries); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.res[req.GetName()]; !ok {
		return nil, notFound(collEntries, req.GetName())
	}
	delete(s.res, req.GetName())
	return s.opLocked(req.GetName(), "delete", nil)
}

// MapExists reports whether a certificate map exists (compute.CertificateMaps).
func (s *Server) MapExists(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.res[normMap(name)].(*cmpb.CertificateMap)
	return ok
}

// MapCertificates are the certificates a handshake for sni gets from a map
// (compute.CertificateMaps): the entry whose hostname is sni, then the
// wildcard entry of sni's parent domain, else the PRIMARY entry. A
// handshake through a map means a load balancer serves it, so its
// load-balancer-authorized certificates are issued (in the background),
// as Google provisions them once they are served.
func (s *Server) MapCertificates(name, sni string) []tls.Certificate {
	name = normMap(name)
	sni = strings.ToLower(strings.TrimSuffix(sni, "."))
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.res[name]; !ok {
		return nil
	}
	var exact, wild, primary *cmpb.CertificateMapEntry
	for _, k := range s.childrenLocked(name, collEntries) {
		e := s.res[k].(*cmpb.CertificateMapEntry)
		h := strings.ToLower(e.GetHostname())
		switch {
		case e.GetMatcher() == cmpb.CertificateMapEntry_PRIMARY:
			primary = e
		case sni != "" && h == sni:
			exact = e
		case sni != "" && strings.HasPrefix(h, "*.") && strings.Contains(sni, ".") && sni[strings.Index(sni, ".")+1:] == h[2:]:
			wild = e
		}
	}
	e := exact
	if e == nil {
		e = wild
	}
	if e == nil {
		e = primary
	}
	if e == nil {
		return nil
	}
	return s.certsLocked(e.GetCertificates())
}

// Certificates are certificates by name, those ready to serve
// (compute.CertificateMaps).
func (s *Server) Certificates(names []string) []tls.Certificate {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.certsLocked(names)
}

// CertificateExists reports whether a certificate exists, ready or not
// (a regional target HTTPS proxy's certificateManagerCertificates).
func (s *Server) CertificateExists(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.res[normMap(name)].(*cmpb.Certificate)
	return ok
}

// certsLocked are the ready certificates of names. A handshake that names
// a managed certificate not yet issued means a load balancer serves it,
// as Google provisions a load-balancer-authorized one only then: it is
// issued in the background, since the Compute Engine API that issues it
// may hold its own lock while it asks, and the next handshake gets it.
func (s *Server) certsLocked(names []string) []tls.Certificate {
	var out []tls.Certificate
	for _, n := range names {
		n = normMap(n)
		if c, ok := s.res[n].(*cmpb.Certificate); ok && c.GetManaged() != nil &&
			c.GetManaged().GetState() != cmpb.Certificate_ManagedCertificate_ACTIVE && !s.issuing[n] {
			go s.issueIf(n, true)
		}
		if pair, ok := s.pairLocked(n); ok {
			out = append(out, pair)
		}
	}
	return out
}
