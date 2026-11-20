package run

import (
	"context"

	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// instanceOutputOnly are the Instance fields a caller cannot set: naming
// one in an update_mask is INVALID_ARGUMENT, as for a service.
var instanceOutputOnly = map[string]bool{
	"name": true, "uid": true, "generation": true, "create_time": true, "update_time": true,
	"delete_time": true, "expire_time": true, "creator": true, "last_modifier": true,
	"observed_generation": true, "terminal_condition": true, "conditions": true,
	"container_statuses": true, "log_uri": true, "urls": true, "reconciling": true,
	"etag": true, "satisfies_pzs": true,
}

// mergeInstanceMask returns the stored instance with the fields the mask
// names taken from the request (AIP-134), as mergeServiceMask does for a
// service; "*" replaces every settable field.
func mergeInstanceMask(current, in *runpb.Instance, paths []string) (*runpb.Instance, error) {
	out := proto.Clone(current).(*runpb.Instance)
	for _, p := range paths {
		if p == "*" {
			merged := proto.Clone(in).(*runpb.Instance)
			merged.Name = current.GetName()
			return merged, nil
		}
		if err := mergePath(out.ProtoReflect(), in.ProtoReflect(), p, instanceOutputOnly); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// UpdateInstance (#1958) changes an instance's configuration: the fields
// update_mask names, or the whole instance with no mask, as
// UpdateWorkerPool does. The new configuration is a new generation whose
// Deployment rolls the instance's pod; a stopped instance stays stopped.
// allow_missing creates a missing instance, a non-empty etag must be the
// stored one, and validate_only checks the change without writing it.
func (i *InstancesServer) UpdateInstance(ctx context.Context, req *runpb.UpdateInstanceRequest) (*longrunningpb.Operation, error) {
	s := i.s
	if req.GetInstance() == nil {
		return nil, apierror.InvalidArgument("instance is required")
	}
	name := req.GetInstance().GetName()
	w, parent, err := parseInstanceName(name)
	if err != nil {
		return nil, err
	}
	cm, err := s.loadSpec(ctx, &w)
	if err != nil {
		if apierror.From(err).Code == codes.NotFound && req.GetAllowMissing() {
			return i.CreateInstance(ctx, &runpb.CreateInstanceRequest{Parent: parent, InstanceId: w.id,
				Instance: req.GetInstance(), ValidateOnly: req.GetValidateOnly()})
		}
		return nil, err
	}
	if err := checkResourceEtag(req.GetInstance().GetEtag(), cm.Metadata.ResourceVersion, "instance"); err != nil {
		return nil, err
	}
	cur, _, err := s.readInstance(ctx, w, cm)
	if err != nil {
		return nil, err
	}
	in := proto.Clone(req.GetInstance()).(*runpb.Instance)
	if paths := req.GetUpdateMask().GetPaths(); len(paths) > 0 {
		stored := instanceSpec(cur, cur.GetGeneration())
		stored.Name = name
		if in, err = mergeInstanceMask(stored, in, paths); err != nil {
			return nil, err
		}
	}
	// The etag was checked above.
	in.Name, in.Etag = name, ""
	if _, err := i.replaceInstance(ctx, in, req.GetValidateOnly()); err != nil {
		return nil, err
	}
	if req.GetValidateOnly() {
		spec := instanceSpec(in, cur.GetGeneration()+1)
		spec.CreateTime, spec.Uid = cur.GetCreateTime(), cur.GetUid()
		return s.doneOperation(parent, name, spec)
	}
	return s.instanceOperation(ctx, w, parent)
}
