package bigtablefront

import (
	"context"
	"strings"

	btapb "cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Memory layers and hot tablets (#1499), of the clusters the instance
// admin keeps (instances.go).
//
// A cluster's memory layer is its configuration: GetMemoryLayer and
// ListMemoryLayers read it, disabled (no memory_config) until
// UpdateMemoryLayer enables it, and a cluster's deletion takes it along.
// The emulator holds every table in memory anyway, so enabling it changes
// no read; storage_size_gib, which Google reports, is 0.
//
// ListHotTablets lists the tablets whose CPU use was high: the emulator
// splits no table into tablets and measures no CPU, so a cluster has none.

func memoryLayerName(cluster string) string { return cluster + "/memoryLayer" }

// memoryLayers is keyed by cluster name, in the instance admin's state and
// under its lock.
func (s *instances) memoryLayerMethods() map[string]method {
	return map[string]method{
		instanceAdmin + "GetMemoryLayer":    unary(func() *btapb.GetMemoryLayerRequest { return &btapb.GetMemoryLayerRequest{} }, s.getMemoryLayer),
		instanceAdmin + "ListMemoryLayers":  unary(func() *btapb.ListMemoryLayersRequest { return &btapb.ListMemoryLayersRequest{} }, s.listMemoryLayers),
		instanceAdmin + "UpdateMemoryLayer": unary(func() *btapb.UpdateMemoryLayerRequest { return &btapb.UpdateMemoryLayerRequest{} }, s.updateMemoryLayer),
		instanceAdmin + "ListHotTablets":    unary(func() *btapb.ListHotTabletsRequest { return &btapb.ListHotTabletsRequest{} }, s.listHotTablets),
	}
}

// memoryLayer returns the memory layer of cluster, which exists. s.mu is
// held.
func (s *instances) memoryLayer(cluster string) *btapb.MemoryLayer {
	if s.layers == nil {
		s.layers = map[string]*btapb.MemoryLayer{}
	}
	l, ok := s.layers[cluster]
	if !ok {
		l = &btapb.MemoryLayer{Name: memoryLayerName(cluster), Etag: newEtag()}
		s.layers[cluster] = l
	}
	return l
}

func (s *instances) getMemoryLayer(_ context.Context, req *btapb.GetMemoryLayerRequest) (proto.Message, error) {
	cluster, ok := strings.CutSuffix(req.GetName(), "/memoryLayer")
	if !ok {
		return nil, status.Errorf(codes.InvalidArgument, "invalid memory layer name %q: want projects/{project}/instances/{instance}/clusters/{cluster}/memoryLayer", req.GetName())
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, _, err := s.cluster(cluster); err != nil {
		return nil, err
	}
	return proto.Clone(s.memoryLayer(cluster)), nil
}

func (s *instances) listMemoryLayers(_ context.Context, req *btapb.ListMemoryLayersRequest) (proto.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var clusters []string
	if inst, ok := strings.CutSuffix(req.GetParent(), "/clusters/-"); ok {
		in, err := s.lookup(inst)
		if err != nil {
			return nil, err
		}
		for _, id := range sortedKeys(in.clusters) {
			clusters = append(clusters, inst+"/clusters/"+id)
		}
	} else {
		if _, _, err := s.cluster(req.GetParent()); err != nil {
			return nil, err
		}
		clusters = []string{req.GetParent()}
	}
	start, end, next, err := page(len(clusters), req.GetPageToken(), req.GetPageSize())
	if err != nil {
		return nil, err
	}
	out := &btapb.ListMemoryLayersResponse{NextPageToken: next}
	for _, c := range clusters[start:end] {
		out.MemoryLayers = append(out.MemoryLayers, proto.Clone(s.memoryLayer(c)).(*btapb.MemoryLayer))
	}
	return out, nil
}

func (s *instances) updateMemoryLayer(_ context.Context, req *btapb.UpdateMemoryLayerRequest) (proto.Message, error) {
	start := s.now()
	upd := req.GetMemoryLayer()
	cluster, ok := strings.CutSuffix(upd.GetName(), "/memoryLayer")
	if !ok {
		return nil, status.Errorf(codes.InvalidArgument, "invalid memory_layer.name %q: want projects/{project}/instances/{instance}/clusters/{cluster}/memoryLayer", upd.GetName())
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, _, err := s.cluster(cluster); err != nil {
		return nil, err
	}
	l := s.memoryLayer(cluster)
	if e := upd.GetEtag(); e != "" && e != l.GetEtag() {
		return nil, status.Error(codes.Aborted, "the etag does not match the memory layer's current etag")
	}
	paths := req.GetUpdateMask().GetPaths()
	if len(paths) == 0 {
		paths = []string{"memory_config"}
	}
	next := proto.Clone(l).(*btapb.MemoryLayer)
	for _, p := range paths {
		if p != "memory_config" {
			return nil, status.Errorf(codes.InvalidArgument, "update_mask path %q is not updatable: want memory_config", p)
		}
		next.MemoryConfig = nil
		if upd.GetMemoryConfig() != nil {
			// storage_size_gib is output only.
			next.MemoryConfig = &btapb.MemoryLayer_MemoryConfig{}
		}
	}
	next.Etag = newEtag()
	s.layers[cluster] = next
	return s.done(next.Name, &btapb.UpdateMemoryLayerMetadata{OriginalRequest: req, RequestTime: timestamppb.New(start),
		FinishTime: timestamppb.New(s.now())}, proto.Clone(next))
}

func (s *instances) listHotTablets(_ context.Context, req *btapb.ListHotTabletsRequest) (proto.Message, error) {
	if req.GetPageSize() < 0 {
		return nil, status.Error(codes.InvalidArgument, "page_size cannot be negative")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, _, err := s.cluster(req.GetParent()); err != nil {
		return nil, err
	}
	return &btapb.ListHotTabletsResponse{}, nil
}
