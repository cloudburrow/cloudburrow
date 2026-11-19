package pubsubfront

import (
	"context"
	"net/http"
	"strings"

	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/cloudburrow/cloudburrow/internal/iampolicy"
	"github.com/cloudburrow/cloudburrow/internal/orgpolicy"
)

// Organization policies (#1890), asked of the host's Resource Manager
// (--resourcemanager) whatever --iam-enforcement, as on Google Cloud:
//
//   - a topic's message storage policy may name only regions
//     gcp.resourceLocations allows, on CreateTopic and on an UpdateTopic
//     of message_storage_policy, over gRPC and REST: FAILED_PRECONDITION
//     with the constraint's documented message;
//   - setIamPolicy refuses a member iam.allowedPolicyMemberDomains does not
//     permit (iam.go).
//
// Nothing is refused when no policy is set.

// checkTopicRegions refuses t's allowed persistence regions that
// gcp.resourceLocations does not allow.
func checkTopicRegions(ctx context.Context, name string, t *pubsubpb.Topic) error {
	regions := t.GetMessageStoragePolicy().GetAllowedPersistenceRegions()
	if len(regions) == 0 {
		return nil
	}
	project := orgpolicy.ProjectOf(name)
	if project == "" {
		return nil
	}
	if err := orgpolicy.CheckLocations(ctx, project, project, regions...); err != nil {
		return status.Error(codes.FailedPrecondition, err.Error())
	}
	return nil
}

// updatesStoragePolicy reports whether an UpdateTopic's mask names the
// message storage policy.
func updatesStoragePolicy(paths []string) bool {
	for _, p := range paths {
		if p == "message_storage_policy" || p == "messageStoragePolicy" || strings.HasPrefix(p, "message_storage_policy.") {
			return true
		}
	}
	return false
}

// checkCreateTopic reads a CreateTopic's first frame and checks its
// regions, answering a stream that replays it.
func checkCreateTopic(ctx context.Context, ss grpc.ServerStream) (grpc.ServerStream, error) {
	var first frame
	if err := ss.RecvMsg(&first); err != nil {
		return nil, err
	}
	var t pubsubpb.Topic
	if proto.Unmarshal(first, &t) == nil {
		// Custom constraints first (#1891, custom.go).
		if err := checkCustom(ctx, t.GetName(), orgpolicy.TypeTopic, orgpolicy.MethodCreate, &t); err != nil {
			return nil, err
		}
		if err := checkTopicRegions(ctx, t.GetName(), &t); err != nil {
			return nil, err
		}
	}
	return &replayStream{ServerStream: ss, first: first}, nil
}

// checkCreateSubscription reads a CreateSubscription's first frame and
// checks it against the custom constraints (#1891).
func checkCreateSubscription(ctx context.Context, ss grpc.ServerStream) (grpc.ServerStream, error) {
	var first frame
	if err := ss.RecvMsg(&first); err != nil {
		return nil, err
	}
	var s pubsubpb.Subscription
	if proto.Unmarshal(first, &s) == nil {
		if err := checkCustom(ctx, s.GetName(), orgpolicy.TypeSubscription, orgpolicy.MethodCreate, &s); err != nil {
			return nil, err
		}
	}
	return &replayStream{ServerStream: ss, first: first}, nil
}

// checkUpdateTopic checks an UpdateTopic's frame.
func checkUpdateTopic(ctx context.Context, in frame) error {
	var req pubsubpb.UpdateTopicRequest
	if proto.Unmarshal(in, &req) != nil || !updatesStoragePolicy(req.GetUpdateMask().GetPaths()) {
		return nil
	}
	return checkTopicRegions(ctx, req.GetTopic().GetName(), req.GetTopic())
}

// checkRESTTopic checks a REST CreateTopic (PUT, the body a Topic) or
// UpdateTopic (PATCH, the body {topic, updateMask}) of topic name.
func checkRESTTopic(r *http.Request, name string) error {
	if r.Method != http.MethodPut && r.Method != http.MethodPatch {
		return nil
	}
	body, err := readBody(r)
	if err != nil {
		return err
	}
	if r.Method == http.MethodPut {
		var t pubsubpb.Topic
		if restJSON.Unmarshal(body, &t) != nil {
			return nil
		}
		return checkTopicRegions(r.Context(), name, &t)
	}
	var req pubsubpb.UpdateTopicRequest
	if restJSON.Unmarshal(body, &req) != nil {
		return nil
	}
	paths := req.GetUpdateMask().GetPaths()
	if len(paths) == 0 && r.URL.Query().Get("updateMask") != "" {
		paths = strings.Split(r.URL.Query().Get("updateMask"), ",")
	}
	if !updatesStoragePolicy(paths) {
		return nil
	}
	return checkTopicRegions(r.Context(), name, req.GetTopic())
}

// checkOrgMembers refuses a policy adding a member
// iam.allowedPolicyMemberDomains does not permit on resource's project.
func checkOrgMembers(ctx context.Context, resource string, before, after *iampolicy.Stored) error {
	var was, now []string
	if before != nil {
		for _, b := range before.Bindings {
			was = append(was, b.Members...)
		}
	}
	for _, b := range after.Bindings {
		now = append(now, b.Members...)
	}
	if err := orgpolicy.CheckMembers(ctx, orgpolicy.ProjectOf(resource), was, now); err != nil {
		return status.Error(codes.FailedPrecondition, err.Error())
	}
	return nil
}
