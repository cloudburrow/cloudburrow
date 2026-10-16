package run

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"

	cbpb "cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"
	longrunning "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Build approvals (#1449). A trigger whose approvalConfig.approvalRequired
// is true starts each build PENDING, with build.approval PENDING; nothing
// runs until ApproveBuild approves it, which queues it, or rejects it,
// which ends it CANCELLED with approval REJECTED. CancelBuild on a pending
// build ends it CANCELLED with approval CANCELLED.
//
// This release of the Cloud Build protos has no BuildTrigger.approvalConfig
// field, so the JSON API carries it beside the proto: the request body's
// approvalConfig is taken out before the transcoder reads it and given to
// CreateBuildTrigger and UpdateBuildTrigger in the request's context, and
// it is written back into every trigger the JSON API returns.

// triggerApprovals are the triggers whose builds need approval, by
// resource name, under triggerStore.mu.
var triggerApprovals = map[string]bool{}

type approvalCtxKey struct{}

// approvalFrom is the approvalConfig.approvalRequired a JSON request's body
// carried; ok is false when it carried none.
func approvalFrom(ctx context.Context) (required, ok bool) {
	v, ok := ctx.Value(approvalCtxKey{}).(bool)
	return v, ok
}

// pendingBuilds start a build once it is approved, by build name.
var pendingBuilds = struct {
	mu    sync.Mutex
	start map[string]func()
}{start: map[string]func(){}}

// TriggerNeedsApproval reports whether a trigger's builds need approval,
// for the console.
func TriggerNeedsApproval(resourceName string) bool {
	triggerStore.mu.Lock()
	defer triggerStore.mu.Unlock()
	return triggerApprovals[resourceName]
}

// SetTriggerApproval sets whether a trigger's builds need approval, for
// the console.
func SetTriggerApproval(resourceName string, required bool) error {
	triggerStore.mu.Lock()
	defer triggerStore.mu.Unlock()
	if _, ok := triggerStore.triggers[resourceName]; !ok {
		return apierror.NotFound("trigger %s was not found", resourceName)
	}
	setApprovalLocked(resourceName, required)
	return nil
}

func setApprovalLocked(resourceName string, required bool) {
	if required {
		triggerApprovals[resourceName] = true
	} else {
		delete(triggerApprovals, resourceName)
	}
}

// ApproveBuild approves or rejects a build awaiting approval, and returns
// its operation, already done with the build.
func (c *CloudBuildServer) ApproveBuild(ctx context.Context, req *cbpb.ApproveBuildRequest) (*longrunning.Operation, error) {
	res := req.GetApprovalResult()
	if res == nil || res.GetDecision() == cbpb.ApprovalResult_DECISION_UNSPECIFIED {
		return nil, apierror.InvalidArgument("approval_result.decision is required: APPROVED or REJECTED")
	}
	get := &cbpb.GetBuildRequest{Name: req.GetName()}
	if parts := strings.Split(req.GetName(), "/"); len(parts) == 4 && parts[0] == "projects" && parts[2] == "builds" {
		// The global form, projects/{project}/builds/{id}, which gcloud
		// sends for a build in any location.
		get = &cbpb.GetBuildRequest{ProjectId: parts[1], Id: parts[3]}
	}
	b, err := c.GetBuild(ctx, get)
	if err != nil {
		return nil, err
	}
	pendingBuilds.mu.Lock()
	start, ok := pendingBuilds.start[b.GetName()]
	delete(pendingBuilds.start, b.GetName())
	pendingBuilds.mu.Unlock()
	if !ok || b.GetApproval().GetState() != cbpb.BuildApproval_PENDING {
		return nil, apierror.FailedPrecondition("build %s is not awaiting approval", b.GetId())
	}
	now := timestamppb.Now()
	result := &cbpb.ApprovalResult{Decision: res.GetDecision(), Comment: res.GetComment(), Url: res.GetUrl(), ApprovalTime: now}
	var final *cbpb.Build
	if res.GetDecision() == cbpb.ApprovalResult_APPROVED {
		final = updateCloudBuild(b.GetName(), func(x *cbpb.Build) {
			x.Status = cbpb.Build_QUEUED
			x.Approval.State, x.Approval.Result = cbpb.BuildApproval_APPROVED, result
		})
		pendingOps(b.GetName())
		start()
	} else {
		final = c.endPending(b.GetName(), cbpb.BuildApproval_REJECTED, result, "Build was rejected")
	}
	s := c.builds.s
	op := s.ops.CreateNamed(b.GetName()[:len(b.GetName())-len(b.GetId())-len("/builds/")]+"/operations/"+uuid.NewString(), b.GetName())
	_ = s.ops.SetMetadata(op.Name, &cbpb.BuildOperationMetadata{Build: final})
	_ = s.ops.Succeed(op.Name, proto.Clone(final))
	return s.toProtoOperation(op.Name)
}

// endPending ends a build that never ran: CANCELLED, with its approval in
// state, and its operation failed.
func (c *CloudBuildServer) endPending(name string, state cbpb.BuildApproval_State, result *cbpb.ApprovalResult, why string) *cbpb.Build {
	now := timestamppb.Now()
	final := updateCloudBuild(name, func(x *cbpb.Build) {
		x.Status, x.FinishTime, x.StatusDetail = cbpb.Build_CANCELLED, now, why
		x.Approval.State = state
		if result != nil {
			x.Approval.Result = result
		}
	})
	if op := pendingOps(name); op != "" {
		_ = c.builds.s.ops.Fail(op, &apierror.Error{Code: codes.Canceled, Message: why})
	}
	return final
}

// pendingOp is each pending build's CreateBuild operation, by build name.
var pendingOp = struct {
	mu  sync.Mutex
	ops map[string]string
}{ops: map[string]string{}}

func pendingOps(name string) string {
	pendingOp.mu.Lock()
	defer pendingOp.mu.Unlock()
	op := pendingOp.ops[name]
	delete(pendingOp.ops, name)
	return op
}

// cancelPending cancels a build awaiting approval; ok is false when the
// build is not awaiting it.
func (c *CloudBuildServer) cancelPending(name string) (*cbpb.Build, bool) {
	pendingBuilds.mu.Lock()
	_, ok := pendingBuilds.start[name]
	delete(pendingBuilds.start, name)
	pendingBuilds.mu.Unlock()
	if !ok {
		return nil, false
	}
	return c.endPending(name, cbpb.BuildApproval_CANCELLED, nil, "Build was cancelled"), true
}

// takeApprovalConfig moves a trigger request body's approvalConfig into
// the request's context, for CreateBuildTrigger and UpdateBuildTrigger.
func takeApprovalConfig(r *http.Request) (*http.Request, error) {
	if (r.Method != http.MethodPost && r.Method != http.MethodPatch) || !triggerPath.MatchString(r.URL.Path) || r.Body == nil {
		return r, nil
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 10<<20))
	if err != nil {
		return r, apierror.InvalidArgument("the request body could not be read")
	}
	r.Body = io.NopCloser(bytes.NewReader(raw))
	var body map[string]json.RawMessage
	if json.Unmarshal(raw, &body) != nil {
		return r, nil // the transcoder reports it
	}
	ac, ok := body["approvalConfig"]
	if !ok {
		return r, nil
	}
	var cfg struct {
		ApprovalRequired bool `json:"approvalRequired"`
	}
	if json.Unmarshal(ac, &cfg) != nil {
		return r, apierror.InvalidArgument("invalid approvalConfig")
	}
	delete(body, "approvalConfig")
	raw, _ = json.Marshal(body)
	r = r.WithContext(context.WithValue(r.Context(), approvalCtxKey{}, cfg.ApprovalRequired))
	r.Body = io.NopCloser(bytes.NewReader(raw))
	r.ContentLength = int64(len(raw))
	r.Header.Set("Content-Length", strconv.Itoa(len(raw)))
	return r, nil
}

// approvalWriter buffers a trigger response to add approvalConfig to it.
type approvalWriter struct {
	http.ResponseWriter
	code int
	buf  bytes.Buffer
}

func (w *approvalWriter) WriteHeader(code int) { w.code = code }
func (w *approvalWriter) Write(p []byte) (int, error) {
	return w.buf.Write(p)
}

// serveTriggerJSON serves a trigger request, adding approvalConfig to each
// trigger in the answer.
func serveTriggerJSON(next http.Handler, w http.ResponseWriter, r *http.Request) {
	aw := &approvalWriter{ResponseWriter: w, code: http.StatusOK}
	next.ServeHTTP(aw, r)
	out := aw.buf.Bytes()
	if aw.code == http.StatusOK {
		out = addApprovalConfig(out)
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(out)))
	w.WriteHeader(aw.code)
	_, _ = w.Write(out)
}

func addApprovalConfig(raw []byte) []byte {
	var body map[string]json.RawMessage
	if json.Unmarshal(raw, &body) != nil {
		return raw
	}
	triggerStore.mu.Lock()
	defer triggerStore.mu.Unlock()
	set := func(t map[string]json.RawMessage) {
		var name string
		if json.Unmarshal(t["resourceName"], &name) != nil || name == "" {
			return
		}
		cfg := map[string]bool{}
		if triggerApprovals[name] {
			cfg["approvalRequired"] = true
		}
		t["approvalConfig"], _ = json.Marshal(cfg)
	}
	if list, ok := body["triggers"]; ok {
		var ts []map[string]json.RawMessage
		if json.Unmarshal(list, &ts) != nil {
			return raw
		}
		for _, t := range ts {
			set(t)
		}
		body["triggers"], _ = json.Marshal(ts)
	} else if _, ok := body["resourceName"]; ok {
		set(body)
	} else {
		return raw
	}
	out, err := json.Marshal(body)
	if err != nil {
		return raw
	}
	return out
}
