package dataform

// google.cloud.dataform.v1beta1.Dataform, served over the v1 server.
//
// Every message v1beta1 shares with v1 has the same fields under the same
// numbers (TestBetaWireCompatible checks it), so a v1beta1 call is its
// request re-read as the v1 request, the v1 method, and the v1 answer
// re-read as the v1beta1 one. What only v1beta1 has is served here:
// workspaces' branch management, shallow clones and original branch
// (branches.go), workflow configs' table update triggers (triggers.go),
// DeleteRepositoryLongRunning, and the folder and team folder IDs a
// create may choose.

import (
	"context"
	"strings"

	dfpb "cloud.google.com/go/dataform/apiv1/dataformpb"
	b1pb "cloud.google.com/go/dataform/apiv1beta1/dataformpb"
	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

const betaService = "google.cloud.dataform.v1beta1.Dataform"

// workspaceExtra is what a v1beta1 CreateWorkspace sets that v1's
// Workspace has no field for.
type workspaceExtra struct {
	OriginalBranch   string `json:"originalBranch,omitempty"`
	BranchManagement bool   `json:"enableBranchManagement,omitempty"`
	Depth            int32  `json:"depth,omitempty"`
}

type ctxKey int

const (
	// wsExtraKey carries a v1beta1 CreateWorkspace's workspaceExtra to
	// the v1 method.
	wsExtraKey ctxKey = iota
	// resourceIDKey carries CreateFolder's folder_id or
	// CreateTeamFolder's team_folder_id.
	resourceIDKey
)

// RegisterBeta serves google.cloud.dataform.v1beta1.Dataform alone on g
// (Register serves it with v1 and the mixins).
func (s *Server) RegisterBeta(g grpc.ServiceRegistrar) { g.RegisterService(s.betaDesc(), s) }

// CallBeta calls a v1beta1 method in process, as a request over gRPC
// would (the console's branch and trigger screens use it).
func (s *Server) CallBeta(ctx context.Context, method string, in proto.Message) (proto.Message, error) {
	for _, m := range s.betaDesc().Methods {
		if m.MethodName == method {
			out, err := m.Handler(s, ctx, func(v any) error {
				proto.Merge(v.(proto.Message), in)
				return nil
			}, nil)
			if err != nil {
				return nil, err
			}
			return out.(proto.Message), nil
		}
	}
	return nil, apierror.Unimplemented("%s/%s is not a method", betaService, method)
}

// betaOnly are the v1beta1 methods v1 has not.
func (s *Server) betaOnly(name string) func(context.Context, proto.Message) (proto.Message, error) {
	switch name {
	case "DeleteRepositoryLongRunning":
		return func(ctx context.Context, m proto.Message) (proto.Message, error) {
			return s.deleteRepositoryLongRunning(ctx, m.(*b1pb.DeleteRepositoryLongRunningRequest))
		}
	case "FetchWorkspaceBranches":
		return func(ctx context.Context, m proto.Message) (proto.Message, error) {
			return s.fetchWorkspaceBranches(ctx, m.(*b1pb.FetchWorkspaceBranchesRequest))
		}
	case "FetchCurrentWorkspaceBranch":
		return func(ctx context.Context, m proto.Message) (proto.Message, error) {
			return s.fetchCurrentWorkspaceBranch(ctx, m.(*b1pb.FetchCurrentWorkspaceBranchRequest))
		}
	case "CheckoutWorkspaceBranch":
		return func(ctx context.Context, m proto.Message) (proto.Message, error) {
			return s.checkoutWorkspaceBranch(ctx, m.(*b1pb.CheckoutWorkspaceBranchRequest))
		}
	case "DeleteBranch":
		return func(ctx context.Context, m proto.Message) (proto.Message, error) {
			return s.deleteBranch(ctx, m.(*b1pb.DeleteBranchRequest))
		}
	case "SyncWorkspaceRefs":
		return func(ctx context.Context, m proto.Message) (proto.Message, error) {
			return s.syncWorkspaceRefs(ctx, m.(*b1pb.SyncWorkspaceRefsRequest))
		}
	case "FetchWorkspaceHistory":
		return func(ctx context.Context, m proto.Message) (proto.Message, error) {
			return s.fetchWorkspaceHistory(ctx, m.(*b1pb.FetchWorkspaceHistoryRequest))
		}
	}
	return nil
}

// betaDesc is the v1beta1 service, each method served as the package
// comment says, gated by the repository's key as v1 is.
func (s *Server) betaDesc() *grpc.ServiceDesc {
	v1 := map[string]grpc.MethodDesc{}
	for _, m := range s.keyedDesc().Methods {
		v1[m.MethodName] = m
	}
	sd := b1pb.File_google_cloud_dataform_v1beta1_dataform_proto.Services().ByName("Dataform")
	desc := &grpc.ServiceDesc{ServiceName: betaService, HandlerType: (*any)(nil), Metadata: b1pb.Dataform_ServiceDesc.Metadata}
	for _, m := range b1pb.Dataform_ServiceDesc.Methods {
		name := m.MethodName
		md := sd.Methods().ByName(protoreflect.Name(name))
		inT, err := protoregistry.GlobalTypes.FindMessageByName(md.Input().FullName())
		if err != nil {
			panic("dataform v1beta1: " + err.Error())
		}
		outT, err := protoregistry.GlobalTypes.FindMessageByName(md.Output().FullName())
		if err != nil {
			panic("dataform v1beta1: " + err.Error())
		}
		var call func(context.Context, proto.Message) (proto.Message, error)
		if f := s.betaOnly(name); f != nil {
			call = func(ctx context.Context, in proto.Message) (proto.Message, error) {
				if r := requestResource(in); r != "" && !keyFree[name] {
					if err := s.checkKey(ctx, r); err != nil {
						return nil, err
					}
				}
				return f(ctx, in)
			}
		} else if v1m, ok := v1[name]; ok {
			call = func(ctx context.Context, in proto.Message) (proto.Message, error) {
				return s.betaThroughV1(ctx, name, v1m, in, outT)
			}
		} else {
			call = func(context.Context, proto.Message) (proto.Message, error) {
				return nil, apierror.Unimplemented("%s/%s is not implemented", betaService, name)
			}
		}
		full := "/" + betaService + "/" + name
		desc.Methods = append(desc.Methods, grpc.MethodDesc{MethodName: name,
			Handler: func(srv any, ctx context.Context, dec func(any) error, ic grpc.UnaryServerInterceptor) (any, error) {
				in := inT.New().Interface()
				if err := dec(in); err != nil {
					return nil, err
				}
				h := func(ctx context.Context, req any) (any, error) { return call(ctx, req.(proto.Message)) }
				if ic == nil {
					return h(ctx, in)
				}
				return ic(ctx, in, &grpc.UnaryServerInfo{Server: srv, FullMethod: full}, h)
			}})
	}
	return desc
}

// reread copies src into dst by its wire form, dropping fields dst has not.
func reread(src, dst proto.Message) error {
	b, err := proto.Marshal(src)
	if err != nil {
		return apierror.Internal(err, "encode the request")
	}
	if err := (proto.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(b, dst); err != nil {
		return apierror.Internal(err, "decode the request")
	}
	return nil
}

// betaThroughV1 serves a v1beta1 method v1 has too.
func (s *Server) betaThroughV1(ctx context.Context, name string, v1m grpc.MethodDesc, in proto.Message, outT protoreflect.MessageType) (proto.Message, error) {
	var after func(proto.Message) error
	switch r := in.(type) {
	case *b1pb.CreateRepositoryRequest:
		if err := refuseAccountConnector(r.GetRepository()); err != nil {
			return nil, err
		}
	case *b1pb.UpdateRepositoryRequest:
		if err := refuseAccountConnector(r.GetRepository()); err != nil {
			return nil, err
		}
	case *b1pb.CreateWorkspaceRequest:
		x, err := checkWorkspaceExtra(r.GetWorkspace())
		if err != nil {
			return nil, err
		}
		ctx = context.WithValue(ctx, wsExtraKey, x)
	case *b1pb.CreateFolderRequest:
		if id := r.GetFolderId(); id != "" {
			if err := checkID("folder_id", id); err != nil {
				return nil, err
			}
			ctx = context.WithValue(ctx, resourceIDKey, id)
		}
	case *b1pb.CreateTeamFolderRequest:
		if id := r.GetTeamFolderId(); id != "" {
			if err := checkID("team_folder_id", id); err != nil {
				return nil, err
			}
			ctx = context.WithValue(ctx, resourceIDKey, id)
		}
	case *b1pb.CreateWorkflowConfigRequest:
		tc, err := s.checkTriggerConfig(r.GetParent(), r.GetWorkflowConfig().GetWorkflowTriggerConfig())
		if err != nil {
			return nil, err
		}
		if tc != nil {
			after = func(out proto.Message) error { return s.setTrigger(out.(*dfpb.WorkflowConfig).GetName(), tc) }
		}
	case *b1pb.UpdateWorkflowConfigRequest:
		paths := r.GetUpdateMask().GetPaths()
		trig, rest := len(paths) == 0, []string{}
		for _, p := range paths {
			if p == "workflow_trigger_config" || strings.HasPrefix(p, "workflow_trigger_config.") {
				trig = true
			} else {
				rest = append(rest, p)
			}
		}
		name := r.GetWorkflowConfig().GetName()
		var tc *b1pb.WorkflowTriggerConfig
		if trig {
			m := workflowRE.FindStringSubmatch(name)
			if m == nil {
				return nil, apierror.InvalidArgument("name %q must be projects/*/locations/*/repositories/*/workflowConfigs/*", name)
			}
			var err error
			if tc, err = s.checkTriggerConfig(m[1], r.GetWorkflowConfig().GetWorkflowTriggerConfig()); err != nil {
				return nil, err
			}
			after = func(proto.Message) error { return s.setTrigger(name, tc) }
		}
		if len(paths) > 0 && len(rest) == 0 {
			// Only the trigger changes: v1 has nothing to update.
			s.mu.Lock()
			_, err := s.workflow(name)
			s.mu.Unlock()
			if err != nil {
				return nil, err
			}
			if err := after(nil); err != nil {
				return nil, err
			}
			got, err := s.GetWorkflowConfig(ctx, &dfpb.GetWorkflowConfigRequest{Name: name})
			if err != nil {
				return nil, err
			}
			return s.betaOut(got, outT)
		}
		if len(paths) > 0 {
			c := clone(r)
			c.UpdateMask = &fieldmaskpb.FieldMask{Paths: rest}
			in = c
		}
	}
	out, err := v1m.Handler(s, ctx, func(v any) error { return reread(in, v.(proto.Message)) }, nil)
	if err != nil {
		return nil, err
	}
	msg := out.(proto.Message)
	if after != nil {
		if err := after(msg); err != nil {
			return nil, err
		}
	}
	return s.betaOut(msg, outT)
}

// betaOut is a v1 answer as the v1beta1 one: an operation's metadata and
// response retyped, and the fields only v1beta1 has filled in.
func (s *Server) betaOut(v1 proto.Message, outT protoreflect.MessageType) (proto.Message, error) {
	if op, ok := v1.(*longrunningpb.Operation); ok {
		return s.betaOperation(op), nil
	}
	out := outT.New().Interface()
	if err := reread(v1, out); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch o := out.(type) {
	case *b1pb.Workspace:
		s.decorateWorkspace(o)
	case *b1pb.ListWorkspacesResponse:
		for _, w := range o.GetWorkspaces() {
			s.decorateWorkspace(w)
		}
	case *b1pb.WorkflowConfig:
		s.decorateWorkflow(o)
	case *b1pb.ListWorkflowConfigsResponse:
		for _, w := range o.GetWorkflowConfigs() {
			s.decorateWorkflow(w)
		}
	}
	return out, nil
}

// betaOperation retypes a done operation's metadata and response as their
// v1beta1 messages, in the record GetOperation reads too.
func (s *Server) betaOperation(op *longrunningpb.Operation) *longrunningpb.Operation {
	const v1, beta = "type.googleapis.com/google.cloud.dataform.v1.", "type.googleapis.com/google.cloud.dataform.v1beta1."
	op = clone(op)
	if a := op.GetMetadata(); a != nil && strings.HasPrefix(a.TypeUrl, v1) {
		a.TypeUrl = beta + strings.TrimPrefix(a.TypeUrl, v1)
	}
	if a := op.GetResponse(); a != nil && strings.HasPrefix(a.TypeUrl, v1) {
		a.TypeUrl = beta + strings.TrimPrefix(a.TypeUrl, v1)
	}
	s.mu.Lock()
	if _, ok := s.ops[op.GetName()]; ok {
		s.ops[op.GetName()] = clone(op)
	}
	s.mu.Unlock()
	return op
}

// decorateWorkspace sets a workspace's v1beta1 output fields; the caller
// holds mu.
func (s *Server) decorateWorkspace(w *b1pb.Workspace) {
	x := s.wsExtras[w.GetName()]
	if x == nil {
		return
	}
	if x.BranchManagement {
		w.EnableBranchManagement = proto.Bool(true)
	}
	w.Shallow = proto.Bool(x.Depth > 0)
}

// decorateWorkflow sets a workflow config's trigger; the caller holds mu.
func (s *Server) decorateWorkflow(w *b1pb.WorkflowConfig) {
	if t := s.triggers[w.GetName()]; t != nil && t.Config != nil {
		w.WorkflowTriggerConfig = clone(t.Config)
	}
}

// refuseAccountConnector: an Account Connector authenticates a Google end
// user (personal authentication), which no local instance holds.
func refuseAccountConnector(r *b1pb.Repository) error {
	if r.GetGitRemoteSettings().GetAccountConnector() != "" {
		return apierror.Unimplemented("git_remote_settings.account_connector (personal authentication through a Developer Connect Account Connector) needs a Google end user's OAuth credentials, which a local instance does not hold; use authentication_token_secret_version or git_repository_link")
	}
	return nil
}

func checkWorkspaceExtra(w *b1pb.Workspace) (*workspaceExtra, error) {
	x := &workspaceExtra{OriginalBranch: w.GetOriginalBranch(), BranchManagement: w.GetEnableBranchManagement(), Depth: w.GetDepth()}
	if x.Depth < 0 {
		return nil, apierror.InvalidArgument("workspace.depth must not be negative")
	}
	if x.Depth > 0 && !x.BranchManagement {
		return nil, apierror.InvalidArgument("workspace.depth is available only with enable_branch_management")
	}
	if x.OriginalBranch != "" {
		if err := checkBranchName("workspace.original_branch", x.OriginalBranch); err != nil {
			return nil, err
		}
	}
	return x, nil
}
