package dataproc

import (
	"context"
	"errors"
	"strings"
	"time"

	dataprocpb "cloud.google.com/go/dataproc/v2/apiv1/dataprocpb"
	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/aipfilter"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/sparkproc"
)

// ConnectEndpoint is the key of a Spark Connect session's endpoint in its
// runtimeInfo.endpoints, the key Google's Spark Connect client reads
// (dataproc-spark-connect's session.py, Apache-2.0): "Spark Connect
// Server", an sc:// URI whose host the client tunnels to.
const ConnectEndpoint = "Spark Connect Server"

// Default TTLs: idle_ttl "Defaults to 1 hour if not set"; ttl, unset,
// leaves a session running until it idles out.
const defaultIdleTTL = time.Hour

func sessionDone(st dataprocpb.Session_State) bool {
	return st == dataprocpb.Session_TERMINATED || st == dataprocpb.Session_FAILED
}

type authorityKey struct{}

// WithAuthority records the host a JSON request was sent to, for a
// session's Spark Connect endpoint.
func WithAuthority(ctx context.Context, host string) context.Context {
	return context.WithValue(ctx, authorityKey{}, host)
}

// authority is the host:port the caller reached this API at: a session's
// Spark Connect endpoint is this API's own address, as the caller sees it.
func authority(ctx context.Context) string {
	if h, ok := ctx.Value(authorityKey{}).(string); ok && h != "" {
		return h
	}
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if v := md.Get(":authority"); len(v) > 0 && v[0] != "" {
			return v[0]
		}
	}
	return ""
}

// connectURI is the Spark Connect endpoint for a caller that reached the
// API at host: sc://HOST[:PORT]. Google's client tunnels to the host on
// port 443 (wss://HOST/tcp-over-websocket-bridge/...), which a container's
// relay serves for host.docker.internal; a plain Spark Connect client
// connects to the port itself, sending the session's UUID as its
// session_id.
func connectURI(host string) string {
	if host == "" {
		return ""
	}
	if h, port, ok := strings.Cut(host, ":"); ok && port == "443" {
		host = h
	}
	return "sc://" + host
}

func (s *Server) render(ctx context.Context, x *dataprocpb.Session) *dataprocpb.Session {
	out := clone(x)
	if out.State == dataprocpb.Session_ACTIVE && out.GetSparkConnectSession() != nil {
		if u := connectURI(authority(ctx)); u != "" {
			if out.RuntimeInfo == nil {
				out.RuntimeInfo = &dataprocpb.RuntimeInfo{}
			}
			if out.RuntimeInfo.Endpoints == nil {
				out.RuntimeInfo.Endpoints = map[string]string{}
			}
			out.RuntimeInfo.Endpoints[ConnectEndpoint] = u
		}
	}
	return out
}

// applyTemplate fills what the session leaves unset from its template.
func applyTemplate(x *dataprocpb.Session, t *dataprocpb.SessionTemplate) {
	if x.SessionConfig == nil {
		switch c := t.GetSessionConfig().(type) {
		case *dataprocpb.SessionTemplate_JupyterSession:
			x.SessionConfig = &dataprocpb.Session_JupyterSession{JupyterSession: clone(c.JupyterSession)}
		case *dataprocpb.SessionTemplate_SparkConnectSession:
			x.SessionConfig = &dataprocpb.Session_SparkConnectSession{SparkConnectSession: clone(c.SparkConnectSession)}
		}
	}
	if t.GetRuntimeConfig() != nil {
		rc := clone(t.GetRuntimeConfig())
		if x.RuntimeConfig != nil {
			proto.Merge(rc, x.RuntimeConfig)
		}
		x.RuntimeConfig = rc
	}
	if t.GetEnvironmentConfig() != nil {
		ec := clone(t.GetEnvironmentConfig())
		if x.EnvironmentConfig != nil {
			proto.Merge(ec, x.EnvironmentConfig)
		}
		x.EnvironmentConfig = ec
	}
	if len(t.GetLabels()) > 0 {
		labels := map[string]string{}
		for k, v := range t.GetLabels() {
			labels[k] = v
		}
		for k, v := range x.GetLabels() {
			labels[k] = v
		}
		x.Labels = labels
	}
}

func (s *Server) CreateSession(ctx context.Context, req *dataprocpb.CreateSessionRequest) (*longrunningpb.Operation, error) {
	project, location, err := checkParent(req.GetParent())
	if err != nil {
		return nil, err
	}
	if req.GetSession() == nil {
		return nil, apierror.InvalidArgument("session is required")
	}
	id := req.GetSessionId()
	if !idRE.MatchString(id) {
		return nil, apierror.InvalidArgument("session_id %q must be 4-63 characters; valid characters are /[a-z][0-9]-/", id)
	}
	if r := req.GetRequestId(); r != "" && !requestIDRE.MatchString(r) {
		return nil, apierror.InvalidArgument("request_id %q must be at most 40 letters, digits, underscores and hyphens", r)
	}
	x := clone(req.GetSession())
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, status.Error(codes.Unavailable, "the instance is stopping")
	}
	if tn := x.GetSessionTemplate(); tn != "" {
		t, ok := s.templates[tn]
		if !ok {
			return nil, apierror.NotFound("Session template %s not found", tn)
		}
		applyTemplate(x, t)
	}
	switch x.GetSessionConfig().(type) {
	case *dataprocpb.Session_SparkConnectSession:
	case *dataprocpb.Session_JupyterSession:
		// A Jupyter session (#2162) runs the same Spark Connect server
		// and, beside it, a Jupyter Server of its kernels (PySpark and
		// Almond's Scala, each `spark` a Spark Connect session of it),
		// which the kernel gateway (gateway.go) starts kernels on; a
		// BigQuery Studio notebook's IPython kernel binds to it too.
		if x.GetJupyterSession().GetKernel() == dataprocpb.JupyterConfig_KERNEL_UNSPECIFIED {
			x.GetJupyterSession().Kernel = dataprocpb.JupyterConfig_PYTHON
		}
	default:
		return nil, apierror.InvalidArgument("session needs spark_connect_session or jupyter_session")
	}
	if len(x.GetLabels()) > maxLabels {
		return nil, apierror.InvalidArgument("No more than %d labels can be associated with a session", maxLabels)
	}
	if err := checkExecution(x.GetEnvironmentConfig().GetExecutionConfig(), true); err != nil {
		return nil, err
	}
	version, err := runtimeVersion(x.GetRuntimeConfig().GetVersion())
	if err != nil {
		return nil, err
	}
	key := req.GetParent() + "|session|" + req.GetRequestId()
	if req.GetRequestId() != "" {
		if op, ok := s.ops[s.requests[key]]; ok {
			return clone(op), nil
		}
	}
	name := req.GetParent() + "/sessions/" + id
	if _, ok := s.sessions[name]; ok {
		return nil, apierror.AlreadyExists("Session %s already exists", name)
	}
	now := timestamppb.New(s.now())
	x.Name, x.Uuid, x.CreateTime, x.State, x.StateTime, x.StateMessage = name, newUUID(), now, dataprocpb.Session_CREATING, now, ""
	x.Creator, x.StateHistory, x.RuntimeInfo = s.caller(ctx), nil, &dataprocpb.RuntimeInfo{}
	if x.User == "" {
		x.User = x.Creator
	}
	if x.RuntimeConfig == nil {
		x.RuntimeConfig = &dataprocpb.RuntimeConfig{}
	}
	x.RuntimeConfig.Version = version
	op := s.newOpLocked(opName(project, location), &dataprocpb.SessionOperationMetadata{Session: name, SessionUuid: x.Uuid, CreateTime: now,
		OperationType: dataprocpb.SessionOperationMetadata_CREATE, Description: "Create session " + id, Labels: x.GetLabels()})
	s.sessions[name] = x
	if req.GetRequestId() != "" {
		s.requests[key] = op.Name
	}
	s.save()
	endpoint := connectURI(authority(ctx))
	rctx, cancel := context.WithCancel(context.Background())
	s.cancels[name] = cancel
	s.wg.Add(1)
	go s.startSession(rctx, name, op.Name, endpoint)
	return clone(op), nil
}

func (s *Server) setSessionStateLocked(x *dataprocpb.Session, st dataprocpb.Session_State, msg string) {
	x.StateHistory = append(x.StateHistory, &dataprocpb.Session_SessionStateHistory{State: x.State, StateMessage: x.StateMessage, StateStartTime: x.StateTime})
	x.State, x.StateMessage, x.StateTime = st, msg, timestamppb.New(s.now())
}

// startSession starts the session's Spark Connect server; the create
// operation ends when it answers (ACTIVE) or fails (FAILED).
func (s *Server) startSession(ctx context.Context, name, opName, endpoint string) {
	defer s.wg.Done()
	s.mu.Lock()
	x := s.sessions[name]
	if x == nil {
		s.mu.Unlock()
		return
	}
	m := sessionRE.FindStringSubmatch(name)
	job := baseJob(x.GetUuid(), m[1], m[2], name, x.GetRuntimeConfig(), x.GetEnvironmentConfig())
	if js := x.GetJupyterSession(); js != nil {
		job.Kernel = js.GetKernel().String()
	}
	job.Log = &sparkproc.LogTarget{Type: "cloud_dataproc_session",
		Labels: map[string]string{"project_id": m[1], "location": m[2], "session_id": m[3]},
		Output: "dataproc.googleapis.com%2Foutput", Spark: "dataproc.googleapis.com%2Fspark"}
	if s.opts.ProjectNumber != nil && job.KMSKey != "" {
		job.KeyAgent = "service-" + s.opts.ProjectNumber(m[1]) + "@dataproc-accounts.iam.gserviceaccount.com"
	}
	ec := x.GetEnvironmentConfig().GetExecutionConfig()
	idle, ttl := defaultIdleTTL, time.Duration(0)
	if ec.GetIdleTtl() != nil {
		idle = ec.GetIdleTtl().AsDuration()
	}
	if ec.GetTtl() != nil {
		ttl = ec.GetTtl().AsDuration()
	}
	s.mu.Unlock()

	var c Conn
	var err error
	if s.opts.Engine == nil {
		err = &sparkproc.Unavailable{Msg: "this instance runs no Apache Spark"}
	} else {
		c, err = s.opts.Engine.StartConnect(ctx, job)
	}
	s.mu.Lock()
	x = s.sessions[name]
	if err != nil || x == nil || x.State != dataprocpb.Session_CREATING {
		if c != nil {
			defer c.Stop()
		}
		if x != nil && !sessionDone(x.State) && (ctx.Err() != nil || x.State == dataprocpb.Session_TERMINATING) {
			s.finishOpLocked(opName, nil, status.Error(codes.Canceled, "Session "+name+" was terminated before it started"))
			s.mu.Unlock()
			s.endSession(name, "", s.now())
			return
		}
		if x != nil && !sessionDone(x.State) {
			msg := "Session " + name + " was terminated"
			if err != nil {
				msg = err.Error()
				var u *sparkproc.Unavailable
				if errors.As(err, &u) {
					msg = "Apache Spark is not available on this instance: " + u.Msg
				}
			}
			s.setSessionStateLocked(x, dataprocpb.Session_FAILED, msg)
			s.finishOpLocked(opName, nil, status.Error(codes.Unknown, msg))
			s.save()
		}
		delete(s.cancels, name)
		s.mu.Unlock()
		return
	}
	s.connects[name] = c
	s.activity[name] = s.now()
	if endpoint != "" {
		x.RuntimeInfo.Endpoints = map[string]string{ConnectEndpoint: endpoint}
	}
	s.setSessionStateLocked(x, dataprocpb.Session_ACTIVE, "")
	s.finishOpLocked(opName, clone(x), nil)
	s.save()
	s.mu.Unlock()
	s.logf("dataproc: session %s is ACTIVE, its Spark Connect server on %s", name, c.Address())

	// Watch the server: it stops when terminated, when it idles past
	// idle_ttl or runs past ttl, or by itself (FAILED).
	started := s.now()
	tick := time.NewTicker(10 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			c.Stop()
			s.endSession(name, "", started)
			return
		case <-c.Done():
			res := c.Result()
			msg := res.Message
			if !res.Failed {
				msg = "the Spark Connect server stopped"
			}
			s.endSession(name, msg, started)
			return
		case <-tick.C:
			s.mu.Lock()
			last := s.activity[name]
			s.mu.Unlock()
			now := s.now()
			if now.Sub(last) > idle || (ttl > 0 && now.Sub(started) > ttl) {
				s.logf("dataproc: session %s idled past its idle_ttl or ran past its ttl", name)
				c.Stop()
				s.endSession(name, "", started)
				return
			}
		}
	}
}

// endSession ends a session that ran: TERMINATED, or FAILED with msg.
func (s *Server) endSession(name, failed string, started time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.connects, name)
	delete(s.activity, name)
	delete(s.cancels, name)
	x := s.sessions[name]
	if x == nil || sessionDone(x.State) {
		return
	}
	if failed != "" && x.State != dataprocpb.Session_TERMINATING {
		s.setSessionStateLocked(x, dataprocpb.Session_FAILED, failed)
	} else {
		s.setSessionStateLocked(x, dataprocpb.Session_TERMINATED, "")
	}
	if x.RuntimeInfo == nil {
		x.RuntimeInfo = &dataprocpb.RuntimeInfo{}
	}
	x.RuntimeInfo.Endpoints = nil
	x.RuntimeInfo.ApproximateUsage = usage(x.GetRuntimeConfig().GetProperties(), s.now().Sub(started), timestamppb.New(s.now()))
	for opName, op := range s.ops {
		if op.Done {
			continue
		}
		md, _ := op.Metadata.UnmarshalNew()
		if m, ok := md.(*dataprocpb.SessionOperationMetadata); ok && m.Session == name {
			if m.OperationType == dataprocpb.SessionOperationMetadata_DELETE {
				continue // deleteSession ends it
			}
			s.finishOpLocked(opName, clone(x), nil)
		}
	}
	s.save()
}

func (s *Server) sessionLocked(name string) (*dataprocpb.Session, error) {
	if !sessionRE.MatchString(name) {
		return nil, apierror.InvalidArgument("name %q must be projects/{project}/locations/{location}/sessions/{session}", name)
	}
	x, ok := s.sessions[name]
	if !ok {
		return nil, apierror.NotFound("Session %s not found", name)
	}
	return x, nil
}

func (s *Server) GetSession(ctx context.Context, req *dataprocpb.GetSessionRequest) (*dataprocpb.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	x, err := s.sessionLocked(req.GetName())
	if err != nil {
		return nil, err
	}
	return s.render(ctx, x), nil
}

// sessionFilter is ListSessions' filter: "Supported fields are
// session_id, session_uuid, state, create_time, and labels".
var sessionFilter = []aipfilter.Option{
	aipfilter.Fields("session_id", "session_uuid", "state", "create_time", "labels"),
	aipfilter.Computed("session_id", func(m proto.Message) string { return lastSegment(m.(*dataprocpb.Session).GetName()) }),
	aipfilter.Alias("session_uuid", "uuid"),
}

func (s *Server) ListSessions(ctx context.Context, req *dataprocpb.ListSessionsRequest) (*dataprocpb.ListSessionsResponse, error) {
	if _, _, err := checkParent(req.GetParent()); err != nil {
		return nil, err
	}
	s.mu.Lock()
	var all []*dataprocpb.Session
	for _, k := range sortedKeys(s.sessions) {
		if strings.HasPrefix(k, req.GetParent()+"/sessions/") {
			all = append(all, s.render(ctx, s.sessions[k]))
		}
	}
	s.mu.Unlock()
	sortNewest(all, func(x *dataprocpb.Session) *timestamppb.Timestamp { return x.GetCreateTime() })
	out, next, err := aipfilter.ListPage("dataproc-sessions:"+req.GetParent(), req.GetFilter(), "", all, req.GetPageToken(), int(req.GetPageSize()), sessionFilter...)
	if err != nil {
		return nil, err
	}
	return &dataprocpb.ListSessionsResponse{Sessions: out, NextPageToken: next}, nil
}

// terminateLocked starts the session's termination: TERMINATING, its
// server stopped by its watcher; a CREATING session's start is cancelled.
func (s *Server) terminateLocked(x *dataprocpb.Session) {
	s.setSessionStateLocked(x, dataprocpb.Session_TERMINATING, "")
	if c := s.cancels[x.Name]; c != nil {
		c()
	}
}

func (s *Server) TerminateSession(_ context.Context, req *dataprocpb.TerminateSessionRequest) (*longrunningpb.Operation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	x, err := s.sessionLocked(req.GetName())
	if err != nil {
		return nil, err
	}
	if sessionDone(x.State) || x.State == dataprocpb.Session_TERMINATING {
		return nil, apierror.FailedPrecondition("Session %s is already %s", x.Name, x.State)
	}
	m := sessionRE.FindStringSubmatch(x.Name)
	op := s.newOpLocked(opName(m[1], m[2]), &dataprocpb.SessionOperationMetadata{Session: x.Name, SessionUuid: x.Uuid,
		CreateTime: timestamppb.New(s.now()), OperationType: dataprocpb.SessionOperationMetadata_TERMINATE,
		Description: "Terminate session " + m[3], Labels: x.GetLabels()})
	s.terminateLocked(x)
	s.save()
	return clone(op), nil
}

// DeleteSession: "If the session is not in terminal state, it is
// terminated, and then deleted."
func (s *Server) DeleteSession(_ context.Context, req *dataprocpb.DeleteSessionRequest) (*longrunningpb.Operation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	x, err := s.sessionLocked(req.GetName())
	if err != nil {
		return nil, err
	}
	m := sessionRE.FindStringSubmatch(x.Name)
	op := s.newOpLocked(opName(m[1], m[2]), &dataprocpb.SessionOperationMetadata{Session: x.Name, SessionUuid: x.Uuid,
		CreateTime: timestamppb.New(s.now()), OperationType: dataprocpb.SessionOperationMetadata_DELETE,
		Description: "Delete session " + m[3], Labels: x.GetLabels()})
	if !sessionDone(x.State) && x.State != dataprocpb.Session_TERMINATING {
		s.terminateLocked(x)
	}
	s.save()
	name, opn := x.Name, op.Name
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for deadline := time.Now().Add(5 * time.Minute); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
			s.mu.Lock()
			x := s.sessions[name]
			if x == nil || sessionDone(x.State) {
				if x != nil {
					delete(s.sessions, name)
				}
				s.finishOpLocked(opn, x, nil)
				s.save()
				s.mu.Unlock()
				return
			}
			s.mu.Unlock()
		}
		s.mu.Lock()
		s.finishOpLocked(opn, nil, status.Error(codes.DeadlineExceeded, "the session did not terminate"))
		s.save()
		s.mu.Unlock()
	}()
	return clone(op), nil
}

// ---- session templates ----

func (s *Server) CreateSessionTemplate(ctx context.Context, req *dataprocpb.CreateSessionTemplateRequest) (*dataprocpb.SessionTemplate, error) {
	if _, _, err := checkParent(req.GetParent()); err != nil {
		return nil, err
	}
	t := clone(req.GetSessionTemplate())
	if t == nil {
		return nil, apierror.InvalidArgument("session_template is required")
	}
	m := templateRE.FindStringSubmatch(t.GetName())
	if m == nil || "projects/"+m[1]+"/locations/"+m[2] != req.GetParent() {
		return nil, apierror.InvalidArgument("session_template.name %q must be %s/sessionTemplates/{template}", t.GetName(), req.GetParent())
	}
	if !templateIDRE.MatchString(m[3]) {
		return nil, apierror.InvalidArgument("session template ID %q must be 4-63 lowercase letters, digits and hyphens, starting and ending with a letter or digit", m[3])
	}
	if err := checkTemplate(t); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.templates[t.Name]; ok {
		return nil, apierror.AlreadyExists("Session template %s already exists", t.Name)
	}
	now := timestamppb.New(s.now())
	t.CreateTime, t.UpdateTime, t.Uuid, t.Creator = now, now, newUUID(), s.caller(ctx)
	s.templates[t.Name] = t
	s.save()
	return clone(t), nil
}

func checkTemplate(t *dataprocpb.SessionTemplate) error {
	if len(t.GetLabels()) > maxLabels {
		return apierror.InvalidArgument("No more than %d labels can be associated with a session template", maxLabels)
	}
	if v := t.GetRuntimeConfig().GetVersion(); v != "" {
		if _, err := runtimeVersion(v); err != nil {
			return err
		}
	}
	return checkExecution(t.GetEnvironmentConfig().GetExecutionConfig(), true)
}

// UpdateSessionTemplate replaces the template: the request names no
// field mask.
func (s *Server) UpdateSessionTemplate(_ context.Context, req *dataprocpb.UpdateSessionTemplateRequest) (*dataprocpb.SessionTemplate, error) {
	t := clone(req.GetSessionTemplate())
	if t == nil {
		return nil, apierror.InvalidArgument("session_template is required")
	}
	if err := checkTemplate(t); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	old, err := s.templateLocked(t.GetName())
	if err != nil {
		return nil, err
	}
	t.CreateTime, t.Uuid, t.Creator, t.UpdateTime = old.CreateTime, old.Uuid, old.Creator, timestamppb.New(s.now())
	s.templates[t.Name] = t
	s.save()
	return clone(t), nil
}

func (s *Server) templateLocked(name string) (*dataprocpb.SessionTemplate, error) {
	if !templateRE.MatchString(name) {
		return nil, apierror.InvalidArgument("name %q must be projects/{project}/locations/{location}/sessionTemplates/{template}", name)
	}
	t, ok := s.templates[name]
	if !ok {
		return nil, apierror.NotFound("Session template %s not found", name)
	}
	return t, nil
}

func (s *Server) GetSessionTemplate(_ context.Context, req *dataprocpb.GetSessionTemplateRequest) (*dataprocpb.SessionTemplate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.templateLocked(req.GetName())
	if err != nil {
		return nil, err
	}
	return clone(t), nil
}

func (s *Server) ListSessionTemplates(_ context.Context, req *dataprocpb.ListSessionTemplatesRequest) (*dataprocpb.ListSessionTemplatesResponse, error) {
	if _, _, err := checkParent(req.GetParent()); err != nil {
		return nil, err
	}
	s.mu.Lock()
	var all []*dataprocpb.SessionTemplate
	for _, k := range sortedKeys(s.templates) {
		if strings.HasPrefix(k, req.GetParent()+"/sessionTemplates/") {
			all = append(all, clone(s.templates[k]))
		}
	}
	s.mu.Unlock()
	out, next, err := aipfilter.ListPage("dataproc-templates:"+req.GetParent(), req.GetFilter(), "", all, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, err
	}
	return &dataprocpb.ListSessionTemplatesResponse{SessionTemplates: out, NextPageToken: next}, nil
}

func (s *Server) DeleteSessionTemplate(_ context.Context, req *dataprocpb.DeleteSessionTemplateRequest) (*emptypb.Empty, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.templateLocked(req.GetName())
	if err != nil {
		return nil, err
	}
	delete(s.templates, t.Name)
	s.save()
	return &emptypb.Empty{}, nil
}

// KernelConnectURI is the Spark Connect URI a Jupyter session's kernel
// reaches its ACTIVE session's server by, through this API's gRPC port at
// addr (the Spark Connect proxy finds the session by its UUID).
func (s *Server) KernelConnectURI(name, addr string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	x := s.sessions[name]
	if x == nil {
		return "", apierror.NotFound("Session %s not found", name)
	}
	if x.State != dataprocpb.Session_ACTIVE {
		return "", status.Errorf(codes.FailedPrecondition, "Session %s is %s, not ACTIVE", name, x.State)
	}
	return "sc://" + addr + "/;session_id=" + x.Uuid, nil
}
