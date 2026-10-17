package storage

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
)

// Bucket IP filtering (#1505), to docs.cloud.google.com/storage/docs/
// ip-filtering-overview, create-ip-filter and bypass-ip-filter, and the
// discovery document's Bucket.ipFilter (google.storage.v2's Bucket.IpFilter
// over gRPC, grpcconv.go).
//
// A bucket's ipFilter is stored and returned as sent, after the checks
// Google documents: mode Enabled or Disabled, CIDR ranges that parse, at
// most 200 ranges across the public and VPC sources, at most 25 VPC
// networks, each named projects/{project}/global/networks/{network}.
//
// With mode Enabled, "any incoming request to the bucket is evaluated
// against these rules": a request whose caller address is in none of
// publicNetworkSource's ranges is refused 403. The caller's address is the
// one the connection came from: on an `up` instance the tunnel carries it in
// clientAddrHeader (internal/netfwd's guard sets it), so a caller on this
// machine is its loopback address and one on the LAN its LAN address.
//
// What is not enforced, and why:
//   - vpcNetworkSources admit nobody: no request reaches CloudBurrow from a
//     VPC network, so a VPC source is stored and never matches. Whether its
//     network exists is not checked (networks are Compute Engine's, which the
//     storage server does not reach), as Google skips the check with
//     allowCrossOrgVpcs.
//   - allowAllServiceAgentAccess changes nothing: no Google service agent
//     calls CloudBurrow.
//   - The bucket's own metadata (GET, PATCH, PUT and DELETE of the bucket)
//     is exempt, as it is on Google for a caller holding
//     storage.buckets.exemptFromIpFilter: IAM is not enforced locally, so
//     every caller holds it, and a filter can always be read, fixed or
//     removed (bypass-ip-filter).
//
// UNVERIFIED: the refusal's message (Google does not document it), and the
// Docs re-read 2026-10-17, still unanswered: https://docs.cloud.google.com/storage/docs/ip-filtering-overview
// set of operations the exemption covers beyond those three.

// clientAddrHeader is the caller's address the tunnel passes on
// (internal/netfwd.ClientAddrHeader); it is trusted only from a loopback
// peer, the tunnel's end inside the pod.
const clientAddrHeader = "X-Cloudburrow-Client-Addr"

var vpcNetworkRE = regexp.MustCompile(`^projects/[^/]+/global/networks/[^/]+$`)

const (
	maxIPFilterRanges   = 200
	maxIPFilterNetworks = 25
)

// validateIPFilter checks a bucket's ipFilter value.
func validateIPFilter(v any) error {
	if v == nil {
		return nil
	}
	f, ok := v.(map[string]any)
	if !ok {
		return badRequest("Invalid argument: ipFilter must be an object")
	}
	ranges, networks := 0, 0
	cidrs := func(where string, v any) error {
		if v == nil {
			return nil
		}
		list, ok := v.([]any)
		if !ok {
			return badRequest("Invalid argument: %s.allowedIpCidrRanges must be a list", where)
		}
		for _, e := range list {
			s, _ := e.(string)
			if _, _, err := net.ParseCIDR(s); err != nil {
				return badRequest("Invalid argument: %v in %s.allowedIpCidrRanges is not an IPv4 or IPv6 CIDR range", e, where)
			}
			ranges++
		}
		return nil
	}
	for k, x := range f {
		switch k {
		case "mode":
			if s, _ := x.(string); s != "Enabled" && s != "Disabled" {
				return badRequest("Invalid argument: ipFilter.mode %v must be Enabled or Disabled", x)
			}
		case "allowCrossOrgVpcs", "allowAllServiceAgentAccess":
			if _, ok := x.(bool); !ok && x != nil {
				return badRequest("Invalid argument: ipFilter.%s must be a boolean", k)
			}
		case "publicNetworkSource":
			if x == nil {
				continue
			}
			p, ok := x.(map[string]any)
			if !ok {
				return badRequest("Invalid argument: ipFilter.publicNetworkSource must be an object")
			}
			for pk := range p {
				if pk != "allowedIpCidrRanges" {
					return badRequest("Invalid argument: %s is not an ipFilter.publicNetworkSource field", pk)
				}
			}
			if err := cidrs("ipFilter.publicNetworkSource", p["allowedIpCidrRanges"]); err != nil {
				return err
			}
		case "vpcNetworkSources":
			if x == nil {
				continue
			}
			list, ok := x.([]any)
			if !ok {
				return badRequest("Invalid argument: ipFilter.vpcNetworkSources must be a list")
			}
			for _, e := range list {
				src, ok := e.(map[string]any)
				if !ok {
					return badRequest("Invalid argument: each of ipFilter.vpcNetworkSources must be an object")
				}
				for sk := range src {
					if sk != "network" && sk != "allowedIpCidrRanges" {
						return badRequest("Invalid argument: %s is not an ipFilter.vpcNetworkSources field", sk)
					}
				}
				if n, _ := src["network"].(string); !vpcNetworkRE.MatchString(n) {
					return badRequest("Invalid argument: ipFilter.vpcNetworkSources.network %v must be projects/{project}/global/networks/{network}", src["network"])
				}
				networks++
				if err := cidrs("ipFilter.vpcNetworkSources", src["allowedIpCidrRanges"]); err != nil {
					return err
				}
			}
		default:
			return badRequest("Invalid argument: %s is not an ipFilter field", k)
		}
	}
	if _, ok := f["mode"]; !ok {
		return required("ipFilter.mode")
	}
	if ranges > maxIPFilterRanges {
		return badRequest("Invalid argument: ipFilter has %d CIDR ranges; at most %d are allowed across public and VPC network sources", ranges, maxIPFilterRanges)
	}
	if networks > maxIPFilterNetworks {
		return badRequest("Invalid argument: ipFilter has %d VPC network sources; at most %d are allowed", networks, maxIPFilterNetworks)
	}
	return nil
}

// clientIP is the address a request came from.
func clientIP(r *http.Request) net.IP {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	if ip != nil && ip.IsLoopback() {
		if fwd := net.ParseIP(strings.TrimSpace(r.Header.Get(clientAddrHeader))); fwd != nil {
			return fwd
		}
	}
	return ip
}

// grpcCaller is a gRPC call's peer and forwarded address, for the JSON
// request grpc.go makes of it.
func grpcCaller(ctx context.Context, req *http.Request) {
	if p, ok := peer.FromContext(ctx); ok && p.Addr != nil {
		req.RemoteAddr = p.Addr.String()
	}
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if v := md.Get(strings.ToLower(clientAddrHeader)); len(v) > 0 {
			req.Header.Set(clientAddrHeader, v[0])
		}
	}
}

// ipFilterTarget is the bucket a request is to, and whether it is to the
// bucket's own metadata, which is exempt.
func (s *Server) ipFilterTarget(r *http.Request) (bucket string, exempt bool) {
	path := r.URL.EscapedPath()
	var rest string
	switch {
	case strings.HasPrefix(path, jsonPrefix):
		rest = strings.TrimPrefix(path, jsonPrefix)
	case strings.HasPrefix(path, uploadPrefix):
		rest = strings.TrimPrefix(path, uploadPrefix)
	case strings.HasPrefix(path, resumablePrefix):
		rest = strings.TrimPrefix(path, resumablePrefix)
	case strings.HasPrefix(path, downloadPrefix):
		rest = strings.TrimPrefix(path, downloadPrefix)
	case strings.HasPrefix(path, controlPrefix):
		_, after, ok := strings.Cut(path, "/buckets/")
		if !ok {
			return "", false
		}
		b, _, _ := strings.Cut(after, "/")
		b, _ = url.PathUnescape(b)
		return b, false
	case path == batchPath || strings.HasPrefix(path, batchPath+"/"), path == lifecyclePath, path == resetPath,
		path == statePath, path == ChannelsPath, path == ChannelOutboxPath, path == eventsPath, path == CloudBuildLogsPath:
		return "", false
	default:
		b, _ := s.xmlTarget(r)
		return b, false
	}
	seg, ok := strings.CutPrefix(rest, "b/")
	if !ok {
		return "", false
	}
	b, tail, _ := strings.Cut(seg, "/")
	b, _ = url.PathUnescape(b)
	switch r.Method {
	case http.MethodGet, http.MethodPatch, http.MethodPut, http.MethodDelete:
		exempt = tail == "" && strings.HasPrefix(path, jsonPrefix)
	}
	return b, exempt
}

// ipFilterRefusal is the refusal of a request a bucket's IP filter does not
// admit, or nil.
func (s *Server) ipFilterRefusal(r *http.Request) error {
	bucket, exempt := s.ipFilterTarget(r)
	if bucket == "" || exempt {
		return nil
	}
	var filter map[string]any
	err := s.meta.View(func(tx Tx) error {
		raw, ok := tx.Get(bucketPrefix + bucket)
		if !ok {
			return nil
		}
		var b struct {
			Fields struct {
				IPFilter map[string]any `json:"ipFilter"`
			} `json:"fields"`
		}
		if err := json.Unmarshal(raw, &b); err != nil {
			return err
		}
		filter = b.Fields.IPFilter
		return nil
	})
	if err != nil || filter == nil {
		return err
	}
	if mode, _ := filter["mode"].(string); mode != "Enabled" {
		return nil
	}
	ip := clientIP(r)
	if ip != nil {
		if p, ok := filter["publicNetworkSource"].(map[string]any); ok {
			list, _ := p["allowedIpCidrRanges"].([]any)
			for _, e := range list {
				s, _ := e.(string)
				if _, n, err := net.ParseCIDR(s); err == nil && n.Contains(ip) {
					return nil
				}
			}
		}
	}
	return errorf(http.StatusForbidden, "forbidden", "Request is prohibited by the IP filter of bucket %s: the caller's address %v is not in an allowed network source.", bucket, ip)
}

// ipFilterGuard refuses a request the bucket's IP filter does not admit, and
// says whether it did.
func (s *Server) ipFilterGuard(w http.ResponseWriter, r *http.Request) bool {
	err := s.ipFilterRefusal(r)
	if err == nil {
		return false
	}
	path := r.URL.EscapedPath()
	if strings.HasPrefix(path, jsonPrefix) || strings.HasPrefix(path, uploadPrefix) || strings.HasPrefix(path, resumablePrefix) ||
		strings.HasPrefix(path, downloadPrefix) || strings.HasPrefix(path, controlPrefix) {
		writeError(w, err)
		return true
	}
	b, _ := s.xmlTarget(r)
	writeXMLFromError(w, r, err, b)
	return true
}
