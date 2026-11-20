package run

import (
	"sort"
	"strconv"
	"strings"

	fv1 "cloud.google.com/go/functions/apiv1/functionspb"
	functionspb "cloud.google.com/go/functions/apiv2/functionspb"
	"google.golang.org/protobuf/proto"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/paging"
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

// functionOrderKey is one ListFunctions order_by term.
type functionOrderKey struct {
	field string
	desc  bool
}

// parseFunctionOrder reads ListFunctions' order_by, "a comma separated
// list of fields", each ascending unless followed by desc (AIP-132).
func parseFunctionOrder(orderBy string) ([]functionOrderKey, error) {
	var keys []functionOrderKey
	for _, term := range strings.Split(orderBy, ",") {
		f := strings.Fields(term)
		if len(f) == 0 {
			continue
		}
		k := functionOrderKey{}
		switch f[0] {
		case "name":
			k.field = "name"
		case "createTime", "create_time":
			k.field = "create_time"
		case "updateTime", "update_time":
			k.field = "update_time"
		case "state":
			k.field = "state"
		case "environment":
			k.field = "environment"
		default:
			return nil, apierror.InvalidArgument("order_by field %q is not supported: order by name, createTime, updateTime, state or environment", f[0])
		}
		switch {
		case len(f) == 1:
		case len(f) == 2 && (f[1] == "desc" || f[1] == "asc"):
			k.desc = f[1] == "desc"
		default:
			return nil, apierror.InvalidArgument("order_by term %q: want a field, optionally followed by asc or desc", strings.TrimSpace(term))
		}
		keys = append(keys, k)
	}
	return keys, nil
}

func compareFunctions(a, b *functionspb.Function, k functionOrderKey) int {
	c := 0
	switch k.field {
	case "name":
		c = strings.Compare(a.GetName(), b.GetName())
	case "create_time":
		c = a.GetCreateTime().AsTime().Compare(b.GetCreateTime().AsTime())
	case "update_time":
		c = a.GetUpdateTime().AsTime().Compare(b.GetUpdateTime().AsTime())
	case "state":
		c = strings.Compare(a.GetState().String(), b.GetState().String())
	case "environment":
		c = strings.Compare(a.GetEnvironment().String(), b.GetEnvironment().String())
	}
	if k.desc {
		c = -c
	}
	return c
}

// maxFunctionsPage is ListFunctions' largest page_size, and its default.
const maxFunctionsPage = 1000

// pageFunctions orders fns (already by name) by order_by and returns the
// page token's page of them.
func pageFunctions(scope string, fns []*functionspb.Function, orderBy, token string, size int32) ([]*functionspb.Function, string, error) {
	keys, err := parseFunctionOrder(orderBy)
	if err != nil {
		return nil, "", err
	}
	sort.SliceStable(fns, func(i, j int) bool {
		for _, k := range keys {
			if c := compareFunctions(fns[i], fns[j], k); c != 0 {
				return c < 0
			}
		}
		return false
	})
	n := int(size)
	if n <= 0 || n > maxFunctionsPage {
		n = maxFunctionsPage
	}
	scope += "|" + orderBy
	after, err := paging.DecodeToken(scope, token)
	if err != nil {
		return nil, "", apierror.InvalidArgument("%v", err)
	}
	// The token names the last function of the previous page; one deleted
	// since resumes after the names before it.
	start := 0
	if after != "" {
		start = len(fns)
		for i, fn := range fns {
			if fn.GetName() == after {
				start = i + 1
				break
			}
		}
		if start == len(fns) && len(keys) == 0 {
			start = sort.Search(len(fns), func(i int) bool { return fns[i].GetName() > after })
		}
	}
	end := min(start+n, len(fns))
	if start >= len(fns) {
		return nil, "", nil
	}
	next := ""
	if end < len(fns) {
		next = paging.EncodeToken(scope, fns[end-1].GetName())
	}
	return fns[start:end], next, nil
}
