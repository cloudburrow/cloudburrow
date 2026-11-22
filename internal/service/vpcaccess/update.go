package vpcaccess

import (
	"context"
	"io"
	"net/http"
	"sort"
	"strings"

	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"cloud.google.com/go/vpcaccess/apiv1/vpcaccesspb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/sysparams"
)

// Updating a connector (#2140): projects.locations.connectors.patch, which
// the v1 REST discovery document publishes (PATCH v1/{+name}, updateMask,
// a Connector body, an Operation back) and the v1 proto the Go gRPC client
// binds does not, so it is served over JSON only, beside the transcoder.
//
// What may change is what vpc/docs/configure-serverless-vpc-access "Update
// a connector" (read 2026-11-22) lists: the machine type, and the minimum
// and maximum number of instances, which may only grow ("Decreasing the
// number of instances for existing connectors is not supported"): the
// minimum no lower than its current value and below the maximum, the
// maximum no lower than its current value and at most 10. Any other field
// in the mask is refused. With no mask, "all modifiable fields are
// updated": those the body sets.

// updatable maps an updateMask path, snake_case or camelCase, to its field.
var updatable = map[string]string{
	"min_instances": "min_instances", "minInstances": "min_instances",
	"max_instances": "max_instances", "maxInstances": "max_instances",
	"machine_type": "machine_type", "machineType": "machine_type",
}

// UpdateConnector is projects.locations.connectors.patch: it changes the
// fields of mask (or, when mask is empty, those c sets) of the connector
// name, and answers its done operation.
func (s *Server) UpdateConnector(_ context.Context, name string, c *vpcaccesspb.Connector, mask []string) (*longrunningpb.Operation, error) {
	m := connectorRE.FindStringSubmatch(name)
	if m == nil {
		return nil, apierror.InvalidArgument("name %q must be projects/{project}/locations/{location}/connectors/{connector}", name)
	}
	if c == nil {
		c = &vpcaccesspb.Connector{}
	}
	fields := map[string]bool{}
	for _, p := range mask {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		f, ok := updatable[p]
		if !ok {
			return nil, apierror.InvalidArgument("Field '%s' cannot be updated: only min_instances, max_instances and machine_type of a connector can be changed.", p)
		}
		fields[f] = true
	}
	if len(fields) == 0 {
		fields["min_instances"] = c.GetMinInstances() != 0
		fields["max_instances"] = c.GetMaxInstances() != 0
		fields["machine_type"] = c.GetMachineType() != ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.connectors[name]
	if !ok {
		return nil, apierror.NotFound("Connector '%s' was not found.", name)
	}
	next := proto.Clone(cur).(*vpcaccesspb.Connector)
	if fields["min_instances"] {
		next.MinInstances = c.GetMinInstances()
	}
	if fields["max_instances"] {
		next.MaxInstances = c.GetMaxInstances()
	}
	if fields["machine_type"] {
		next.MachineType = c.GetMachineType()
	}
	switch next.GetMachineType() {
	case "e2-micro", "f1-micro", "e2-standard-4":
	default:
		return nil, apierror.InvalidArgument("Invalid value for field 'machine_type': '%s'. Must be one of f1-micro, e2-micro or e2-standard-4.", next.GetMachineType())
	}
	if next.GetMinInstances() < cur.GetMinInstances() {
		return nil, apierror.InvalidArgument("Invalid value for field 'min_instances': %d. Decreasing the number of instances for existing connectors is not supported: it is %d.", next.GetMinInstances(), cur.GetMinInstances())
	}
	if next.GetMaxInstances() < cur.GetMaxInstances() {
		return nil, apierror.InvalidArgument("Invalid value for field 'max_instances': %d. Decreasing the number of instances for existing connectors is not supported: it is %d.", next.GetMaxInstances(), cur.GetMaxInstances())
	}
	if next.GetMinInstances() > 9 {
		return nil, apierror.InvalidArgument("Invalid value for field 'min_instances': %d. Must be between 2 and 9.", next.GetMinInstances())
	}
	if next.GetMaxInstances() > 10 {
		return nil, apierror.InvalidArgument("Invalid value for field 'max_instances': %d. Must be between 3 and 10.", next.GetMaxInstances())
	}
	if next.GetMinInstances() >= next.GetMaxInstances() {
		return nil, apierror.InvalidArgument("min_instances (%d) must be lower than max_instances (%d).", next.GetMinInstances(), next.GetMaxInstances())
	}
	next.MinThroughput, next.MaxThroughput = next.GetMinInstances()*mbpsPerInstance, next.GetMaxInstances()*mbpsPerInstance
	s.connectors[name] = next
	return s.opLocked("projects/"+m[1]+"/locations/"+m[2], "UpdateConnector", name, next)
}

// servePatch serves PATCH /v1/projects/P/locations/L/connectors/C: true
// when r is that request.
func (s *Server) servePatch(w http.ResponseWriter, r *http.Request) bool {
	name := strings.TrimPrefix(r.URL.Path, "/v1/")
	if r.Method != http.MethodPatch || !connectorRE.MatchString(name) {
		return false
	}
	sys, _ := sysparams.FromRequest(r)
	sysparams.MarkHandled(r)
	b, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		sys.WriteError(w, apierror.InvalidArgument("read the request body: %v", err))
		return true
	}
	c := &vpcaccesspb.Connector{}
	if len(strings.TrimSpace(string(b))) > 0 {
		if err := protojson.Unmarshal(b, c); err != nil {
			sys.WriteError(w, apierror.InvalidArgument("Invalid JSON payload received. %v", err))
			return true
		}
	}
	var mask []string
	for _, v := range r.URL.Query()["updateMask"] {
		mask = append(mask, strings.Split(v, ",")...)
	}
	for _, v := range r.URL.Query()["update_mask"] {
		mask = append(mask, strings.Split(v, ",")...)
	}
	op, err := s.UpdateConnector(r.Context(), name, c, mask)
	if err != nil {
		sys.WriteError(w, err)
		return true
	}
	out, err := protojson.Marshal(op)
	if err != nil {
		sys.WriteError(w, apierror.Internal(err, "encode the response"))
		return true
	}
	out, ct := sys.Body(out, true)
	w.Header().Set("Content-Type", ct)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
	return true
}

// NetworkUsers names the connectors that use rel, a network
// (projects/P/global/networks/N) or a subnet
// (projects/P/regions/R/subnetworks/S), which Compute Engine refuses to
// delete while they exist (compute.Options.NetworkUsers), as Google
// refuses it with resourceInUseByAnotherResource. A connector on a subnet
// keeps the subnet, and the subnet its network.
func (s *Server) NetworkUsers(rel string) []string {
	p := strings.Split(rel, "/")
	s.umu.Lock()
	defer s.umu.Unlock()
	var out []string
	for k, c := range s.uses {
		switch {
		case len(p) == 5 && p[0] == "projects" && p[2] == "global" && p[3] == "networks":
			if projectOf(k) == p[1] && c.GetNetwork() == p[4] && c.GetSubnet().GetName() == "" {
				out = append(out, k)
			}
		case len(p) == 6 && p[0] == "projects" && p[2] == "regions" && p[4] == "subnetworks":
			if c.GetSubnet().GetName() == p[5] && subnetProject(c, k) == p[1] && regionOf(k) == p[3] {
				out = append(out, k)
			}
		}
	}
	sort.Strings(out)
	return out
}

// refreshUsesLocked copies connectors into uses.
func (s *Server) refreshUsesLocked() {
	m := make(map[string]*vpcaccesspb.Connector, len(s.connectors))
	for k, c := range s.connectors {
		m[k] = proto.Clone(c).(*vpcaccesspb.Connector)
	}
	s.umu.Lock()
	s.uses = m
	s.umu.Unlock()
}
