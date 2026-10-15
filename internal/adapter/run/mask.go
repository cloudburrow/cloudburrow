package run

import (
	"strings"

	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// serviceOutputOnly are the Service fields a caller cannot set: naming one
// in an update_mask is INVALID_ARGUMENT, as Google answers a mask that
// names a field the request may not change.
var serviceOutputOnly = map[string]bool{
	"name": true, "uid": true, "generation": true, "create_time": true, "update_time": true,
	"delete_time": true, "expire_time": true, "creator": true, "last_modifier": true,
	"observed_generation": true, "terminal_condition": true, "conditions": true,
	"latest_ready_revision": true, "latest_created_revision": true, "traffic_statuses": true,
	"uri": true, "urls": true, "reconciling": true, "etag": true, "satisfies_pzs": true,
}

// mergeServiceMask returns the current service with the fields the mask
// names taken from the request (#1260). A path is a dotted chain of proto
// field names ending anywhere in the message tree; the field it ends on is
// replaced whole (a repeated or map field included), or cleared when the
// request leaves it unset. "*" replaces every settable field.
func mergeServiceMask(current, in *runpb.Service, paths []string) (*runpb.Service, error) {
	out := proto.Clone(current).(*runpb.Service)
	for _, p := range paths {
		if p == "*" {
			merged := proto.Clone(in).(*runpb.Service)
			merged.Name = current.GetName()
			return merged, nil
		}
		if err := mergePath(out.ProtoReflect(), in.ProtoReflect(), p); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func mergePath(dst, src protoreflect.Message, path string) error {
	parts := strings.Split(path, ".")
	if serviceOutputOnly[parts[0]] {
		return apierror.InvalidArgument("update_mask: %q is output only and cannot be updated", path)
	}
	for i, part := range parts {
		fd := dst.Descriptor().Fields().ByName(protoreflect.Name(part))
		if fd == nil {
			return apierror.InvalidArgument("update_mask: %q is not a field of %s", path, dst.Descriptor().FullName())
		}
		if i == len(parts)-1 {
			if src.Has(fd) {
				dst.Set(fd, src.Get(fd))
			} else {
				dst.Clear(fd)
			}
			return nil
		}
		if fd.Kind() != protoreflect.MessageKind || fd.IsList() || fd.IsMap() {
			return apierror.InvalidArgument("update_mask: %q goes inside %q, which is not a message", path, part)
		}
		dst = dst.Mutable(fd).Message()
		src = src.Get(fd).Message()
	}
	return nil
}
