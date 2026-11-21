package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"

	"cloud.google.com/go/container/apiv1/containerpb"
	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/genproto/googleapis/cloud/location"
	"google.golang.org/grpc"

	"github.com/cloudburrow/cloudburrow/internal/iameval"
	"github.com/cloudburrow/cloudburrow/internal/service/redisadmin"
	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
)

// The thirteenth round of IAM enforcement (#1879): Memorystore's
// Operations and Locations mixins, Kubernetes Engine's cluster
// credentials, and the deny policies' Cloud Identity customer principal
// sets (Logging's stream and JSON-only methods are wired with its table,
// in iamenforce.go; Compute's and the Cloud Run port's in rounds 7 and 8).

func init() {
	// The instance's one Cloud Identity customer owns its organization's
	// domain.
	iameval.DomainCustomers[resourcemanager.OrganizationDomain] = resourcemanager.OrganizationCustomerID
}

type iamEnforced13 struct {
	memorystore *memorystoreAdminService
	gke         *gkeService
}

func (s iamEnforced13) use(e *iameval.Enforcer) {
	if s.memorystore != nil {
		ms := s.memorystore
		typeOf := func(ctx context.Context, name string) string {
			if ms.api == nil {
				return ""
			}
			op, err := redisadmin.NewOperationsServer(ms.api).GetOperation(ctx, &longrunningpb.GetOperationRequest{Name: name})
			if err != nil {
				return ""
			}
			return op.GetMetadata().GetTypeUrl()
		}
		ms.interpose = append(ms.interpose, memorystoreMixins(e, typeOf))
		for _, wrap := range []*func(http.Handler) http.Handler{&ms.wrapRedisJSON, &ms.wrapValkeyJSON} {
			prev := *wrap
			*wrap = func(next http.Handler) http.Handler {
				h := memorystoreMixinsJSON(e, typeOf, next)
				if prev != nil {
					h = prev(h)
				}
				return h
			}
		}
	}
	if s.gke != nil {
		s.gke.interpose = append(s.gke.interpose, gkeCredentials(e))
		prev := s.gke.wrapJSON
		s.gke.wrapJSON = func(next http.Handler) http.Handler {
			h := gkeCredentialsJSON(e, next)
			if prev != nil {
				h = prev(h)
			}
			return h
		}
	}
}

// memorystoreMixins checks Memorystore's Operations and Locations mixins
// over gRPC: an operation by its own API's permission
// (iameval.MemorystoreOperation), a listing by any of the APIs' and then
// each operation by its own.
func memorystoreMixins(e *iameval.Enforcer, typeOf func(context.Context, string) string) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if e == nil || e.On == nil || !e.On() {
			return handler(ctx, req)
		}
		bearer := iameval.IncomingBearer(ctx)
		var err error
		switch in := req.(type) {
		case *longrunningpb.GetOperationRequest:
			err = memorystoreOperation(ctx, e, bearer, typeOf, in.GetName(), "get", false)
		case *longrunningpb.WaitOperationRequest:
			err = memorystoreOperation(ctx, e, bearer, typeOf, in.GetName(), "get", false)
		case *longrunningpb.DeleteOperationRequest:
			err = memorystoreOperation(ctx, e, bearer, typeOf, in.GetName(), "delete", false)
		case *longrunningpb.CancelOperationRequest:
			err = memorystoreOperation(ctx, e, bearer, typeOf, in.GetName(), "cancel", false)
		case *longrunningpb.ListOperationsRequest:
			if err := memorystoreList(ctx, e, bearer, iameval.MemorystoreListOperations, in.GetName()); err != nil {
				return nil, err
			}
			resp, err := handler(ctx, req)
			out, ok := resp.(*longrunningpb.ListOperationsResponse)
			if err != nil || !ok {
				return resp, err
			}
			kept := out.Operations[:0]
			for _, op := range out.GetOperations() {
				if memorystoreListed(ctx, e, bearer, op.GetMetadata().GetTypeUrl(), op.GetName(), false) {
					kept = append(kept, op)
				}
			}
			out.Operations = kept
			return out, nil
		case *location.ListLocationsRequest:
			err = memorystoreList(ctx, e, bearer, iameval.MemorystoreListLocations, in.GetName())
		case *location.GetLocationRequest:
			err = memorystoreList(ctx, e, bearer, iameval.MemorystoreGetLocation, in.GetName())
		}
		if err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
}

// memorystoreOperation checks verb (get, delete or cancel) on one
// operation by its API's permission; an operation not found passes to the
// handler, which answers NOT_FOUND.
func memorystoreOperation(ctx context.Context, e *iameval.Enforcer, bearer string, typeOf func(context.Context, string) string, name, verb string, valkey bool) error {
	t := typeOf(ctx, name)
	if t == "" && !valkey {
		return nil
	}
	svc, get, _, del, cancel := iameval.MemorystoreOperation(t, valkey)
	if svc == nil {
		return nil
	}
	perm := map[string]string{"get": get, "delete": del, "cancel": cancel}[verb]
	return e.Authorize(ctx, svc, bearer, perm, name)
}

// memorystoreList refuses a call unless the caller holds one of perms on
// resource; the denial names the first (Redis').
func memorystoreList(ctx context.Context, e *iameval.Enforcer, bearer string, perms []string, resource string) error {
	ok, err := e.HoldsAny(ctx, iameval.Redis, bearer, perms, resource)
	if err != nil {
		return err
	}
	if !ok {
		return iameval.Redis.Deny(perms[0], resource)
	}
	return nil
}

// memorystoreListed is whether a listed operation is kept: its API's list
// permission held on its location.
func memorystoreListed(ctx context.Context, e *iameval.Enforcer, bearer, typeURL, name string, valkey bool) bool {
	svc, _, list, _, _ := iameval.MemorystoreOperation(typeURL, valkey)
	if svc == nil {
		return true
	}
	parent := name
	if i := strings.Index(name, "/operations/"); i >= 0 {
		parent = name[:i]
	}
	ok, err := e.HoldsAny(ctx, svc, bearer, []string{list}, parent)
	return err == nil && ok
}

var (
	msOpPath   = regexp.MustCompile(`^(/memorystore)?/v1/(projects/[^/]+/locations/[^/]+/operations/[^/:]+)(:cancel)?$`)
	msOpsPath  = regexp.MustCompile(`^(/memorystore)?/v1/(projects/[^/]+/locations/[^/]+)/operations$`)
	msLocsPath = regexp.MustCompile(`^(/memorystore)?/v1/(projects/[^/]+)/locations$`)
	msLocPath  = regexp.MustCompile(`^(/memorystore)?/v1/(projects/[^/]+/locations/[^/]+)$`)
)

// memorystoreMixinsJSON is memorystoreMixins over JSON, where Valkey's
// prefix (/memorystore) names its own API: Memorystore for Valkey's
// memorystore.* permissions, Redis' redis.* otherwise.
func memorystoreMixinsJSON(e *iameval.Enforcer, typeOf func(context.Context, string) string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if e == nil || e.On == nil || !e.On() {
			next.ServeHTTP(w, r)
			return
		}
		ctx, bearer, path := r.Context(), iameval.Bearer(r.Header.Get("Authorization")), r.URL.Path
		var err error
		switch {
		case msOpPath.MatchString(path):
			g := msOpPath.FindStringSubmatch(path)
			valkey := g[1] != ""
			switch {
			case g[3] != "" && r.Method == http.MethodPost:
				err = memorystoreOperation(ctx, e, bearer, typeOf, g[2], "cancel", valkey)
			case g[3] == "" && r.Method == http.MethodGet:
				err = memorystoreOperation(ctx, e, bearer, typeOf, g[2], "get", valkey)
			case g[3] == "" && r.Method == http.MethodDelete:
				err = memorystoreOperation(ctx, e, bearer, typeOf, g[2], "delete", valkey)
			}
		case msOpsPath.MatchString(path) && r.Method == http.MethodGet:
			g := msOpsPath.FindStringSubmatch(path)
			valkey := g[1] != ""
			perms := []string{"redis.operations.list"}
			if valkey {
				perms = []string{"memorystore.operations.list"}
			}
			if err := memorystoreList(ctx, e, bearer, perms, g[2]); err != nil {
				iameval.WriteJSONStatus(w, err)
				return
			}
			filterOperationsJSON(w, r, func(typeURL, name string) bool {
				return memorystoreListed(ctx, e, bearer, typeURL, name, false)
			}, next)
			return
		case msLocsPath.MatchString(path) && r.Method == http.MethodGet:
			g := msLocsPath.FindStringSubmatch(path)
			perm := "redis.locations.list"
			if g[1] != "" {
				perm = "memorystore.locations.list"
			}
			err = memorystoreList(ctx, e, bearer, []string{perm}, g[2])
		case msLocPath.MatchString(path) && r.Method == http.MethodGet:
			g := msLocPath.FindStringSubmatch(path)
			perm := "redis.locations.get"
			if g[1] != "" {
				perm = "memorystore.locations.get"
			}
			err = memorystoreList(ctx, e, bearer, []string{perm}, g[2])
		}
		if err != nil {
			iameval.WriteJSONStatus(w, err)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// clusterOf is a cluster's resource name from its self link.
func clusterOf(c *containerpb.Cluster) string {
	if i := strings.Index(c.GetSelfLink(), "/projects/"); i >= 0 {
		return c.GetSelfLink()[i+1:]
	}
	return ""
}

// hideCredentials clears a cluster's credentials unless the caller holds
// container.clusters.getCredentials on it (iameval.GKEGetCredentials).
func hideCredentials(ctx context.Context, e *iameval.Enforcer, bearer, name string, auth *containerpb.MasterAuth) {
	if auth == nil || name == "" {
		return
	}
	if ok, err := e.HoldsAny(ctx, iameval.GKE, bearer, []string{iameval.GKEGetCredentials}, name); err == nil && ok {
		return
	}
	auth.Username, auth.Password, auth.ClientCertificate, auth.ClientKey = "", "", "", ""
}

// gkeCredentials checks GetCluster's and ListClusters' credentials.
func gkeCredentials(e *iameval.Enforcer) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		resp, err := handler(ctx, req)
		if err != nil || e == nil || e.On == nil || !e.On() {
			return resp, err
		}
		bearer := iameval.IncomingBearer(ctx)
		switch out := resp.(type) {
		case *containerpb.Cluster:
			hideCredentials(ctx, e, bearer, clusterOf(out), out.GetMasterAuth())
		case *containerpb.ListClustersResponse:
			for _, c := range out.GetClusters() {
				hideCredentials(ctx, e, bearer, clusterOf(c), c.GetMasterAuth())
			}
		}
		return resp, err
	}
}

var gkeClusterPath = regexp.MustCompile(`^/v1(?:beta1)?/projects/[^/]+/(?:locations|zones)/[^/]+/clusters(?:/[^/:]+)?$`)

// gkeCredentialsJSON is gkeCredentials over JSON.
func gkeCredentialsJSON(e *iameval.Enforcer, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if e == nil || e.On == nil || !e.On() || r.Method != http.MethodGet || !gkeClusterPath.MatchString(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		rec := httptest.NewRecorder()
		next.ServeHTTP(rec, r)
		var out map[string]any
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil {
			for k, v := range rec.Header() {
				w.Header()[k] = v
			}
			w.WriteHeader(rec.Code)
			_, _ = w.Write(rec.Body.Bytes())
			return
		}
		ctx, bearer := r.Context(), iameval.Bearer(r.Header.Get("Authorization"))
		hide := func(c map[string]any) {
			auth, _ := c["masterAuth"].(map[string]any)
			link, _ := c["selfLink"].(string)
			i := strings.Index(link, "/projects/")
			if auth == nil || i < 0 {
				return
			}
			if ok, err := e.HoldsAny(ctx, iameval.GKE, bearer, []string{iameval.GKEGetCredentials}, link[i+1:]); err == nil && ok {
				return
			}
			for _, k := range []string{"username", "password", "clientCertificate", "clientKey"} {
				delete(auth, k)
			}
		}
		if list, ok := out["clusters"].([]any); ok {
			for _, c := range list {
				if m, ok := c.(map[string]any); ok {
					hide(m)
				}
			}
		} else {
			hide(out)
		}
		b, _ := json.Marshal(out)
		w.Header().Set("Content-Type", "application/json; charset=UTF-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(b)
	})
}

// readBody reads a request's body and puts it back.
func readBody(r *http.Request) []byte {
	if r.Body == nil {
		return nil
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	_ = r.Body.Close()
	r.Body = io.NopCloser(bytes.NewReader(b))
	if err != nil {
		return nil
	}
	return b
}

var (
	computeAddrInsertPath = regexp.MustCompile(`^/compute/v1/projects/([^/]+)/(?:regions/[^/]+|(global))/addresses$`)
	computeInsertPath     = regexp.MustCompile(`^/compute/v1/projects/([^/]+)/zones/([^/]+)/instances$`)
	computePatchPath      = regexp.MustCompile(`^/compute/v1/(projects/([^/]+)/(?:global/(networks|firewalls)|regions/[^/]+/(subnetworks))/[^/]+)$`)
)

// computeInternalInsert is an addresses.insert of an INTERNAL address: its
// permission (iameval.ComputeInternalAddress) and project.
func computeInternalInsert(r *http.Request) (perm, project string, ok bool) {
	if r.Method != http.MethodPost {
		return "", "", false
	}
	g := computeAddrInsertPath.FindStringSubmatch(r.URL.Path)
	if g == nil {
		return "", "", false
	}
	var b struct {
		AddressType string `json:"addressType"`
	}
	if json.Unmarshal(readBody(r), &b) != nil || b.AddressType != "INTERNAL" {
		return "", "", false
	}
	return iameval.ComputeInternalAddress(g[2] != ""), "projects/" + g[1], true
}

// computeRound13Checks are Compute's further field-level checks:
//
//   - instances.insert's accessConfigs[].natIP needs compute.addresses.use
//     on the reserved address of that IP in the instance's region (the
//     page's natIP field, read 2026-11-20); an IP the project reserves
//     none of names no address and is not checked;
//   - networks.patch, subnetworks.patch and firewalls.patch (and update)
//     list the resource's get beside its update (compute.networks.get,
//     compute.subnetworks.get, compute.firewalls.get), and a firewall's
//     compute.networks.updatePolicy on its network (the body's, else the
//     rule's own), as their pages name them (read 2026-11-20).
func computeRound13Checks(e *iameval.Enforcer, svc *computeService, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if e == nil || e.On == nil || !e.On() || svc == nil {
			next.ServeHTTP(w, r)
			return
		}
		bearer := iameval.Bearer(r.Header.Get("Authorization"))
		var checks [][2]string
		switch {
		case r.Method == http.MethodPost && computeInsertPath.MatchString(r.URL.Path):
			g := computeInsertPath.FindStringSubmatch(r.URL.Path)
			project, zone := g[1], g[2]
			region := zone
			if i := strings.LastIndex(zone, "-"); i > 0 {
				region = zone[:i]
			}
			var b struct {
				NetworkInterfaces []struct {
					AccessConfigs []struct {
						NatIP string `json:"natIP"`
					} `json:"accessConfigs"`
				} `json:"networkInterfaces"`
			}
			if json.Unmarshal(readBody(r), &b) == nil {
				for _, n := range b.NetworkInterfaces {
					for _, ac := range n.AccessConfigs {
						if addr := svc.addressOf(project, region, ac.NatIP); addr != "" {
							checks = append(checks, [2]string{"compute.addresses.use", addr})
						}
					}
				}
			}
		case (r.Method == http.MethodPatch || r.Method == http.MethodPut) && computePatchPath.MatchString(r.URL.Path):
			g := computePatchPath.FindStringSubmatch(r.URL.Path)
			name, project, kind := g[1], g[2], g[3]+g[4]
			checks = append(checks, [2]string{"compute." + kind + ".get", name})
			if kind == "firewalls" {
				var b struct {
					Network string `json:"network"`
				}
				_ = json.Unmarshal(readBody(r), &b)
				nw := b.Network
				if nw == "" {
					nw = svc.firewallNetwork(project, name)
				}
				if nw != "" {
					checks = append(checks, [2]string{"compute.networks.updatePolicy", computeRelName(nw, project, "global/networks/")})
				}
			}
		}
		for _, c := range checks {
			if err := e.Authorize(r.Context(), iameval.Compute, bearer, c[0], c[1]); err != nil {
				iameval.WriteJSONStatus(w, err)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// computeRelName is a Compute link as a relative resource name, a short
// name taken as under project's prefix.
func computeRelName(link, project, prefix string) string {
	if i := strings.Index(link, "projects/"); i >= 0 {
		return link[i:]
	}
	return "projects/" + project + "/" + prefix + link
}

// addressOf is the reserved address of ip in project's region, "" for
// none.
func (s *computeService) addressOf(project, region, ip string) string {
	if s.api == nil || ip == "" {
		return ""
	}
	out, err := s.api.Call(http.MethodGet, project, "regions/"+region+"/addresses", nil, nil)
	if err != nil {
		return ""
	}
	items, _ := out["items"].([]any)
	for _, it := range items {
		a, _ := it.(map[string]any)
		if addr, _ := a["address"].(string); addr == ip {
			name, _ := a["name"].(string)
			return "projects/" + project + "/regions/" + region + "/addresses/" + name
		}
	}
	return ""
}

// firewallNetwork is a firewall rule's network, "" for none.
func (s *computeService) firewallNetwork(project, name string) string {
	if s.api == nil {
		return ""
	}
	rel := strings.TrimPrefix(name, "projects/"+project+"/")
	out, err := s.api.Call(http.MethodGet, project, rel, nil, nil)
	if err != nil {
		return ""
	}
	nw, _ := out["network"].(string)
	return nw
}
