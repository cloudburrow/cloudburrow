package run

import (
	"strconv"

	fv1 "cloud.google.com/go/functions/apiv1/functionspb"
	functionspb "cloud.google.com/go/functions/apiv2/functionspb"
	"google.golang.org/protobuf/proto"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// A 1st-gen function's earlier versions (#1958). Google documents a 1st
// gen function's version as "an integer that starts from 1 and gets
// incremented on redeployments", and that it "may keep historical configs
// for old versions": v1 GetFunction's and GenerateDownloadUrl's
// version_id, and v2 GetFunction's revision, read one. Each update keeps
// the version it replaces, as it last stood, beside the function; a delete
// drops them with it.

// functionVersion is one earlier version of a 1st-gen function.
type functionVersion struct {
	fn *functionspb.Function
	v1 *fv1.CloudFunction
}

// keepVersion records sf's current version before an update replaces it.
// The caller holds functions.mu.
func (sf *storedFunction) keepVersion() {
	if sf.v1 == nil {
		return
	}
	if sf.history == nil {
		sf.history = map[int64]functionVersion{}
	}
	sf.history[sf.version] = functionVersion{fn: cloneFunction(sf.fn), v1: proto.Clone(sf.v1).(*fv1.CloudFunction)}
}

// atVersion is sf as it stood at version, or sf itself for 0 or the
// current version. The caller holds functions.mu.
func (sf *storedFunction) atVersion(version int64) (*storedFunction, error) {
	if version == 0 || version == sf.version {
		return sf, nil
	}
	h, ok := sf.history[version]
	if !ok {
		return nil, apierror.NotFound("Version %d of function %s was not found", version, sf.fn.GetName())
	}
	return &storedFunction{fn: h.fn, v1: h.v1, version: version, policy: sf.policy}, nil
}

// revisionOf is v2 GetFunction's revision as a 1st-gen version: valid
// only for a 1st-gen function, as Google documents it.
func revisionOf(sf *storedFunction, revision string) (*storedFunction, error) {
	if revision == "" {
		return sf, nil
	}
	if sf.v1 == nil {
		return nil, apierror.InvalidArgument("revision is valid only for 1st gen functions; %s is a 2nd gen function", sf.fn.GetName())
	}
	n, err := strconv.ParseInt(revision, 10, 64)
	if err != nil || n < 1 {
		return nil, apierror.InvalidArgument("revision %q is not a 1st gen function version: want an integer from 1", revision)
	}
	return sf.atVersion(n)
}
