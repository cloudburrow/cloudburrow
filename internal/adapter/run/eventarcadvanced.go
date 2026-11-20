package run

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/ext"
	eventarcpb "cloud.google.com/go/eventarc/apiv1/eventarcpb"
	publishingpb "cloud.google.com/go/eventarc/publishing/apiv1/publishingpb"
	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/aipfilter"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/resource"
	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// Eventarc Advanced (#1996): message buses, enrollments and pipelines of
// google.cloud.eventarc.v1, and the Publishing API's Publish to a bus
// (google.cloud.eventarc.publishing.v1), as Google's Eventarc Advanced
// pages document them:
//
//   - https://docs.cloud.google.com/eventarc/advanced/docs/publish-events/publish-events-direct-format:
//     a CloudEvent published to a bus as json_message (or proto_message),
//     with specversion, type, source and id required;
//   - https://docs.cloud.google.com/eventarc/advanced/docs/receive-events/create-enrollment:
//     an enrollment has cel_match, message_bus and destination (a
//     pipeline); the pipeline is in the enrollment's project and the bus's
//     region; one pipeline has one destination;
//   - https://docs.cloud.google.com/eventarc/advanced/docs/receive-events/use-cel:
//     the expression reads the event's attributes from `message`
//     (message.type, message.source, message.id, message.time, extension
//     attributes as strings), never message.data or message.data_base64;
//     a missing key does not match; the strings and lists extensions.
//
// A matching event goes to the pipeline's destination: a workflow starts
// an execution with the CloudEvent in its JSON format as the argument, as
// Eventarc passes events to Workflows; an HTTP endpoint on this machine
// receives it as a binary-mode CloudEvent. Delivery is asynchronous and
// retried by the pipeline's retry_policy (Google's defaults: 5 attempts,
// 1 to 60 s). Kept in memory, as triggers are.
//
// UNIMPLEMENTED, naming the field: a destination that is a message bus or
// a Pub/Sub topic, an HTTP endpoint off this machine (refused, never sent),
// mediations (transformations), message_binding_template, payload formats
// other than JSON, avro_message, customer-managed keys, Google API sources,
// and CEL's re.* functions.

var (
	eaAdvIDRE      = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,61}[a-z0-9])?$`)
	eaBusRE        = regexp.MustCompile(`^projects/([^/]+)/locations/([^/]+)/messageBuses/([^/]+)$`)
	eaPipelineRE   = regexp.MustCompile(`^projects/([^/]+)/locations/([^/]+)/pipelines/([^/]+)$`)
	eaEnrollmentRE = regexp.MustCompile(`^projects/([^/]+)/locations/([^/]+)/enrollments/([^/]+)$`)
)

type eaEnrollment struct {
	e   *eventarcpb.Enrollment
	prg cel.Program
}

var eaAdv = struct {
	mu          sync.Mutex
	buses       map[string]*eventarcpb.MessageBus
	pipelines   map[string]*eventarcpb.Pipeline
	enrollments map[string]*eaEnrollment
}{buses: map[string]*eventarcpb.MessageBus{}, pipelines: map[string]*eventarcpb.Pipeline{}, enrollments: map[string]*eaEnrollment{}}

// eaAdvOp is a done operation on a bus, pipeline or enrollment.
func (e *EventarcServer) eaAdvOp(target, verb string, response proto.Message) (*longrunningpb.Operation, error) {
	s := e.f.s
	parts := strings.SplitN(target, "/", 5)
	loc := strings.Join(parts[:4], "/")
	op := s.ops.CreateNamed(loc+"/operations/"+uuid.NewString(), target)
	now := timestamppb.Now()
	_ = s.ops.SetMetadata(op.Name, &eventarcpb.OperationMetadata{CreateTime: now, EndTime: now, Target: target, Verb: verb, ApiVersion: "v1"})
	_ = s.ops.Succeed(op.Name, response)
	return s.toProtoOperation(op.Name)
}

func eaAdvEtag(name string, t *timestamppb.Timestamp) string {
	return fmt.Sprintf("%x", resource.ProjectNumber(name+t.AsTime().String()))
}

// eaAdvParent checks a create's parent and ID.
func eaAdvParent(parent, id, what string) (string, error) {
	if _, _, err := resource.ParseLocation(parent); err != nil {
		return "", apierror.InvalidArgument("%v", err)
	}
	if !eaAdvIDRE.MatchString(id) {
		return "", apierror.InvalidArgument("%s_id %q must be 1-63 lowercase letters, digits and hyphens, starting with a letter and not ending with a hyphen", what, id)
	}
	return parent, nil
}

func eaAdvInParent(name, parent, collection string) bool {
	if strings.HasSuffix(parent, "/locations/-") {
		return strings.HasPrefix(name, strings.TrimSuffix(parent, "-")) && strings.Contains(name, "/"+collection+"/")
	}
	return strings.HasPrefix(name, parent+"/"+collection+"/")
}

// ---- message buses ----

func checkBus(b *eventarcpb.MessageBus) error {
	if b.GetCryptoKeyName() != "" {
		return apierror.Unimplemented("message_bus.crypto_key_name: customer-managed encryption keys for Eventarc Advanced are not supported by CloudBurrow")
	}
	return nil
}

// CreateMessageBus makes a bus that events are published to.
func (e *EventarcServer) CreateMessageBus(_ context.Context, req *eventarcpb.CreateMessageBusRequest) (*longrunningpb.Operation, error) {
	if _, err := eaAdvParent(req.GetParent(), req.GetMessageBusId(), "message_bus"); err != nil {
		return nil, err
	}
	in := req.GetMessageBus()
	if in == nil {
		in = &eventarcpb.MessageBus{}
	}
	if err := checkBus(in); err != nil {
		return nil, err
	}
	b := proto.Clone(in).(*eventarcpb.MessageBus)
	name := req.GetParent() + "/messageBuses/" + req.GetMessageBusId()
	now := timestamppb.Now()
	b.Name, b.Uid, b.CreateTime, b.UpdateTime = name, uuid.NewString(), now, now
	b.Etag = eaAdvEtag(name, now)
	eaAdv.mu.Lock()
	defer eaAdv.mu.Unlock()
	if _, ok := eaAdv.buses[name]; ok {
		return nil, apierror.AlreadyExists("message bus %s already exists", name)
	}
	if !req.GetValidateOnly() {
		eaAdv.buses[name] = b
	}
	return e.eaAdvOp(name, "create", proto.Clone(b))
}

// GetMessageBus reads a bus.
func (e *EventarcServer) GetMessageBus(_ context.Context, req *eventarcpb.GetMessageBusRequest) (*eventarcpb.MessageBus, error) {
	eaAdv.mu.Lock()
	defer eaAdv.mu.Unlock()
	b, ok := eaAdv.buses[req.GetName()]
	if !ok {
		return nil, apierror.NotFound("message bus %s not found", req.GetName())
	}
	return proto.Clone(b).(*eventarcpb.MessageBus), nil
}

// ListMessageBuses lists a location's buses.
func (e *EventarcServer) ListMessageBuses(_ context.Context, req *eventarcpb.ListMessageBusesRequest) (*eventarcpb.ListMessageBusesResponse, error) {
	eaAdv.mu.Lock()
	var all []*eventarcpb.MessageBus
	for _, n := range sortedKeys(eaAdv.buses) {
		if eaAdvInParent(n, req.GetParent(), "messageBuses") {
			all = append(all, proto.Clone(eaAdv.buses[n]).(*eventarcpb.MessageBus))
		}
	}
	eaAdv.mu.Unlock()
	all, err := aipfilter.List(req.GetFilter(), req.GetOrderBy(), all)
	if err != nil {
		return nil, err
	}
	items, next, err := page(all, req.GetPageSize(), req.GetPageToken(), req.GetParent()+"|buses|"+req.GetFilter()+"|"+req.GetOrderBy())
	if err != nil {
		return nil, err
	}
	return &eventarcpb.ListMessageBusesResponse{MessageBuses: items, NextPageToken: next}, nil
}

// ListMessageBusEnrollments lists the enrollments of a bus, by name.
func (e *EventarcServer) ListMessageBusEnrollments(_ context.Context, req *eventarcpb.ListMessageBusEnrollmentsRequest) (*eventarcpb.ListMessageBusEnrollmentsResponse, error) {
	eaAdv.mu.Lock()
	if _, ok := eaAdv.buses[req.GetParent()]; !ok {
		eaAdv.mu.Unlock()
		return nil, apierror.NotFound("message bus %s not found", req.GetParent())
	}
	var all []string
	for _, n := range sortedKeys(eaAdv.enrollments) {
		if eaAdv.enrollments[n].e.GetMessageBus() == req.GetParent() {
			all = append(all, n)
		}
	}
	eaAdv.mu.Unlock()
	items, next, err := page(all, req.GetPageSize(), req.GetPageToken(), req.GetParent()+"|busEnrollments")
	if err != nil {
		return nil, err
	}
	return &eventarcpb.ListMessageBusEnrollmentsResponse{Enrollments: items, NextPageToken: next}, nil
}

// UpdateMessageBus changes a bus's labels, annotations, display name and
// logging config.
func (e *EventarcServer) UpdateMessageBus(ctx context.Context, req *eventarcpb.UpdateMessageBusRequest) (*longrunningpb.Operation, error) {
	in := req.GetMessageBus()
	m := eaBusRE.FindStringSubmatch(in.GetName())
	if m == nil {
		return nil, apierror.InvalidArgument("message_bus.name %q must be projects/{project}/locations/{location}/messageBuses/{bus}", in.GetName())
	}
	if err := checkBus(in); err != nil {
		return nil, err
	}
	eaAdv.mu.Lock()
	cur, ok := eaAdv.buses[in.GetName()]
	eaAdv.mu.Unlock()
	if !ok {
		if req.GetAllowMissing() {
			return e.CreateMessageBus(ctx, &eventarcpb.CreateMessageBusRequest{Parent: "projects/" + m[1] + "/locations/" + m[2],
				MessageBusId: m[3], MessageBus: in, ValidateOnly: req.GetValidateOnly()})
		}
		return nil, apierror.NotFound("message bus %s not found", in.GetName())
	}
	if in.GetEtag() != "" && in.GetEtag() != cur.GetEtag() {
		return nil, apierror.Aborted("the message bus's etag %q is not its current %q", in.GetEtag(), cur.GetEtag())
	}
	b := proto.Clone(cur).(*eventarcpb.MessageBus)
	paths := req.GetUpdateMask().GetPaths()
	if len(paths) == 0 || (len(paths) == 1 && paths[0] == "*") {
		paths = []string{"labels", "annotations", "display_name", "logging_config"}
	}
	for _, p := range paths {
		switch p {
		case "labels":
			b.Labels = in.GetLabels()
		case "annotations":
			b.Annotations = in.GetAnnotations()
		case "display_name", "displayName":
			b.DisplayName = in.GetDisplayName()
		case "logging_config", "loggingConfig":
			b.LoggingConfig = in.GetLoggingConfig()
		default:
			return nil, apierror.InvalidArgument("update_mask path %q cannot be updated on a message bus", p)
		}
	}
	b.UpdateTime = timestamppb.Now()
	b.Etag = eaAdvEtag(b.GetName(), b.GetUpdateTime())
	if !req.GetValidateOnly() {
		eaAdv.mu.Lock()
		eaAdv.buses[b.GetName()] = b
		eaAdv.mu.Unlock()
	}
	return e.eaAdvOp(b.GetName(), "update", proto.Clone(b))
}

// DeleteMessageBus removes a bus.
func (e *EventarcServer) DeleteMessageBus(_ context.Context, req *eventarcpb.DeleteMessageBusRequest) (*longrunningpb.Operation, error) {
	eaAdv.mu.Lock()
	b, ok := eaAdv.buses[req.GetName()]
	if ok && req.GetEtag() != "" && req.GetEtag() != b.GetEtag() {
		eaAdv.mu.Unlock()
		return nil, apierror.Aborted("the message bus's etag %q is not its current %q", req.GetEtag(), b.GetEtag())
	}
	if ok && !req.GetValidateOnly() {
		delete(eaAdv.buses, req.GetName())
	}
	eaAdv.mu.Unlock()
	if !ok {
		if req.GetAllowMissing() {
			return e.eaAdvOp(req.GetName(), "delete", &eventarcpb.MessageBus{Name: req.GetName()})
		}
		return nil, apierror.NotFound("message bus %s not found", req.GetName())
	}
	return e.eaAdvOp(req.GetName(), "delete", proto.Clone(b))
}

// ---- pipelines ----

// eaLocalURL reports whether an HTTP endpoint is on this machine: a
// loopback address or a .localhost name. Anything else is never sent.
func eaLocalURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	return isLoopbackHost(u.Hostname())
}

func isLoopbackHost(host string) bool {
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (f *FunctionsServer) checkPipeline(name string, p *eventarcpb.Pipeline) error {
	if p.GetCryptoKeyName() != "" {
		return apierror.Unimplemented("pipeline.crypto_key_name: customer-managed encryption keys for Eventarc Advanced are not supported by CloudBurrow")
	}
	if len(p.GetMediations()) > 0 {
		return apierror.Unimplemented("pipeline.mediations: transformations are not supported by CloudBurrow")
	}
	if pf := p.GetInputPayloadFormat(); pf != nil && pf.GetJson() == nil {
		return apierror.Unimplemented("pipeline.input_payload_format: only the JSON format is supported by CloudBurrow")
	}
	if len(p.GetDestinations()) != 1 {
		return apierror.InvalidArgument("a pipeline has exactly one destination; this one has %d", len(p.GetDestinations()))
	}
	d := p.GetDestinations()[0]
	if pf := d.GetOutputPayloadFormat(); pf != nil && pf.GetJson() == nil {
		return apierror.Unimplemented("destinations[0].output_payload_format: only the JSON format is supported by CloudBurrow")
	}
	project := strings.Split(name, "/")[1]
	switch {
	case d.GetWorkflow() != "":
		if !eaWorkflowRE.MatchString(d.GetWorkflow()) {
			return apierror.InvalidArgument("destinations[0].workflow %q must be projects/{project}/locations/{location}/workflows/{workflow}", d.GetWorkflow())
		}
		if strings.Split(d.GetWorkflow(), "/")[1] != project {
			return apierror.InvalidArgument("destinations[0].workflow %q must be in the pipeline's project %s", d.GetWorkflow(), project)
		}
		return f.checkWorkflow(d.GetWorkflow())
	case d.GetHttpEndpoint() != nil:
		he := d.GetHttpEndpoint()
		if he.GetMessageBindingTemplate() != "" {
			return apierror.Unimplemented("destinations[0].http_endpoint.message_binding_template is not supported by CloudBurrow")
		}
		if !eaLocalURL(he.GetUri()) {
			return apierror.InvalidArgument("destinations[0].http_endpoint.uri %q is not on this machine: CloudBurrow runs locally and sends events only to a loopback address or a .localhost name", he.GetUri())
		}
		return nil
	case d.GetMessageBus() != "":
		return apierror.Unimplemented("destinations[0].message_bus: a pipeline to another bus is not supported by CloudBurrow")
	case d.GetTopic() != "":
		return apierror.Unimplemented("destinations[0].topic: a pipeline to a Pub/Sub topic is not supported by CloudBurrow")
	}
	return apierror.InvalidArgument("destinations[0] needs a workflow or an http_endpoint")
}

// CreatePipeline makes a pipeline to one destination.
func (e *EventarcServer) CreatePipeline(_ context.Context, req *eventarcpb.CreatePipelineRequest) (*longrunningpb.Operation, error) {
	if _, err := eaAdvParent(req.GetParent(), req.GetPipelineId(), "pipeline"); err != nil {
		return nil, err
	}
	in := req.GetPipeline()
	if in == nil {
		return nil, apierror.InvalidArgument("pipeline is required")
	}
	name := req.GetParent() + "/pipelines/" + req.GetPipelineId()
	if err := e.f.checkPipeline(name, in); err != nil {
		return nil, err
	}
	p := proto.Clone(in).(*eventarcpb.Pipeline)
	now := timestamppb.Now()
	p.Name, p.Uid, p.CreateTime, p.UpdateTime = name, uuid.NewString(), now, now
	p.Etag = eaAdvEtag(name, now)
	eaAdv.mu.Lock()
	defer eaAdv.mu.Unlock()
	if _, ok := eaAdv.pipelines[name]; ok {
		return nil, apierror.AlreadyExists("pipeline %s already exists", name)
	}
	if !req.GetValidateOnly() {
		eaAdv.pipelines[name] = p
	}
	return e.eaAdvOp(name, "create", proto.Clone(p))
}

// GetPipeline reads a pipeline.
func (e *EventarcServer) GetPipeline(_ context.Context, req *eventarcpb.GetPipelineRequest) (*eventarcpb.Pipeline, error) {
	eaAdv.mu.Lock()
	defer eaAdv.mu.Unlock()
	p, ok := eaAdv.pipelines[req.GetName()]
	if !ok {
		return nil, apierror.NotFound("pipeline %s not found", req.GetName())
	}
	return proto.Clone(p).(*eventarcpb.Pipeline), nil
}

// ListPipelines lists a location's pipelines.
func (e *EventarcServer) ListPipelines(_ context.Context, req *eventarcpb.ListPipelinesRequest) (*eventarcpb.ListPipelinesResponse, error) {
	eaAdv.mu.Lock()
	var all []*eventarcpb.Pipeline
	for _, n := range sortedKeys(eaAdv.pipelines) {
		if eaAdvInParent(n, req.GetParent(), "pipelines") {
			all = append(all, proto.Clone(eaAdv.pipelines[n]).(*eventarcpb.Pipeline))
		}
	}
	eaAdv.mu.Unlock()
	all, err := aipfilter.List(req.GetFilter(), req.GetOrderBy(), all)
	if err != nil {
		return nil, err
	}
	items, next, err := page(all, req.GetPageSize(), req.GetPageToken(), req.GetParent()+"|pipelines|"+req.GetFilter()+"|"+req.GetOrderBy())
	if err != nil {
		return nil, err
	}
	return &eventarcpb.ListPipelinesResponse{Pipelines: items, NextPageToken: next}, nil
}

// UpdatePipeline changes a pipeline's destination, retry policy, labels,
// annotations and display name.
func (e *EventarcServer) UpdatePipeline(ctx context.Context, req *eventarcpb.UpdatePipelineRequest) (*longrunningpb.Operation, error) {
	in := req.GetPipeline()
	m := eaPipelineRE.FindStringSubmatch(in.GetName())
	if m == nil {
		return nil, apierror.InvalidArgument("pipeline.name %q must be projects/{project}/locations/{location}/pipelines/{pipeline}", in.GetName())
	}
	eaAdv.mu.Lock()
	cur, ok := eaAdv.pipelines[in.GetName()]
	eaAdv.mu.Unlock()
	if !ok {
		if req.GetAllowMissing() {
			return e.CreatePipeline(ctx, &eventarcpb.CreatePipelineRequest{Parent: "projects/" + m[1] + "/locations/" + m[2],
				PipelineId: m[3], Pipeline: in, ValidateOnly: req.GetValidateOnly()})
		}
		return nil, apierror.NotFound("pipeline %s not found", in.GetName())
	}
	if in.GetEtag() != "" && in.GetEtag() != cur.GetEtag() {
		return nil, apierror.Aborted("the pipeline's etag %q is not its current %q", in.GetEtag(), cur.GetEtag())
	}
	p := proto.Clone(cur).(*eventarcpb.Pipeline)
	paths := req.GetUpdateMask().GetPaths()
	if len(paths) == 0 || (len(paths) == 1 && paths[0] == "*") {
		paths = []string{"labels", "annotations", "display_name", "destinations", "retry_policy", "logging_config"}
	}
	for _, path := range paths {
		switch path {
		case "labels":
			p.Labels = in.GetLabels()
		case "annotations":
			p.Annotations = in.GetAnnotations()
		case "display_name", "displayName":
			p.DisplayName = in.GetDisplayName()
		case "destinations":
			p.Destinations = in.GetDestinations()
		case "retry_policy", "retryPolicy":
			p.RetryPolicy = in.GetRetryPolicy()
		case "logging_config", "loggingConfig":
			p.LoggingConfig = in.GetLoggingConfig()
		case "mediations", "input_payload_format", "crypto_key_name":
			return nil, apierror.Unimplemented("pipeline.%s is not supported by CloudBurrow", path)
		default:
			return nil, apierror.InvalidArgument("update_mask path %q cannot be updated on a pipeline", path)
		}
	}
	if err := e.f.checkPipeline(p.GetName(), p); err != nil {
		return nil, err
	}
	p.UpdateTime = timestamppb.Now()
	p.Etag = eaAdvEtag(p.GetName(), p.GetUpdateTime())
	if !req.GetValidateOnly() {
		eaAdv.mu.Lock()
		eaAdv.pipelines[p.GetName()] = p
		eaAdv.mu.Unlock()
	}
	return e.eaAdvOp(p.GetName(), "update", proto.Clone(p))
}

// DeletePipeline removes a pipeline.
func (e *EventarcServer) DeletePipeline(_ context.Context, req *eventarcpb.DeletePipelineRequest) (*longrunningpb.Operation, error) {
	eaAdv.mu.Lock()
	p, ok := eaAdv.pipelines[req.GetName()]
	if ok && req.GetEtag() != "" && req.GetEtag() != p.GetEtag() {
		eaAdv.mu.Unlock()
		return nil, apierror.Aborted("the pipeline's etag %q is not its current %q", req.GetEtag(), p.GetEtag())
	}
	if ok && !req.GetValidateOnly() {
		delete(eaAdv.pipelines, req.GetName())
	}
	eaAdv.mu.Unlock()
	if !ok {
		if req.GetAllowMissing() {
			return e.eaAdvOp(req.GetName(), "delete", &eventarcpb.Pipeline{Name: req.GetName()})
		}
		return nil, apierror.NotFound("pipeline %s not found", req.GetName())
	}
	return e.eaAdvOp(req.GetName(), "delete", proto.Clone(p))
}

// ---- enrollments ----

var (
	eaCELOnce sync.Once
	eaCELEnv  *cel.Env
	eaCELErr  error
)

// eaCEL is the environment an enrollment's cel_match is compiled in: the
// event's attributes in `message`, with the strings and lists extensions.
func eaCEL() (*cel.Env, error) {
	eaCELOnce.Do(func() {
		eaCELEnv, eaCELErr = cel.NewEnv(cel.Variable("message", cel.MapType(cel.StringType, cel.DynType)), ext.Strings(), ext.Lists())
	})
	return eaCELEnv, eaCELErr
}

var eaReservedRE = regexp.MustCompile(`\bmessage\s*(\.\s*(data|data_base64)\b|\[\s*['"](data|data_base64)['"]\s*\])`)

// compileMatch compiles a cel_match: a boolean over message's attributes.
func compileMatch(src string) (cel.Program, error) {
	if strings.TrimSpace(src) == "" {
		return nil, apierror.InvalidArgument("enrollment.cel_match is required")
	}
	if eaReservedRE.MatchString(src) {
		return nil, apierror.InvalidArgument("enrollment.cel_match %q: message.data and message.data_base64 are reserved and cannot be used", src)
	}
	env, err := eaCEL()
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	ast, iss := env.Compile(src)
	if iss.Err() != nil {
		return nil, apierror.InvalidArgument("enrollment.cel_match %q: %v", src, iss.Err())
	}
	if t := ast.OutputType(); !t.IsExactType(cel.BoolType) && !t.IsExactType(cel.DynType) {
		return nil, apierror.InvalidArgument("enrollment.cel_match %q must be a boolean expression, not %s", src, t)
	}
	prg, err := env.Program(ast)
	if err != nil {
		return nil, apierror.InvalidArgument("enrollment.cel_match %q: %v", src, err)
	}
	return prg, nil
}

// enrollmentMatches evaluates an enrollment's expression; an error (such as a
// missing key) does not match.
func enrollmentMatches(prg cel.Program, attrs map[string]any) bool {
	out, _, err := prg.Eval(map[string]any{"message": attrs})
	if err != nil {
		return false
	}
	return out == types.True
}

func checkEnrollment(name string, en *eventarcpb.Enrollment) (cel.Program, error) {
	prg, err := compileMatch(en.GetCelMatch())
	if err != nil {
		return nil, err
	}
	bm := eaBusRE.FindStringSubmatch(en.GetMessageBus())
	if bm == nil {
		return nil, apierror.InvalidArgument("enrollment.message_bus %q must be projects/{project}/locations/{location}/messageBuses/{bus}", en.GetMessageBus())
	}
	pm := eaPipelineRE.FindStringSubmatch(en.GetDestination())
	if pm == nil {
		return nil, apierror.InvalidArgument("enrollment.destination %q must be projects/{project}/locations/{location}/pipelines/{pipeline}", en.GetDestination())
	}
	parts := strings.Split(name, "/")
	if pm[1] != parts[1] {
		return nil, apierror.InvalidArgument("the pipeline %s must be in the enrollment's project %s", en.GetDestination(), parts[1])
	}
	if pm[2] != bm[2] {
		return nil, apierror.InvalidArgument("the pipeline %s must be in the bus's region %s", en.GetDestination(), bm[2])
	}
	eaAdv.mu.Lock()
	_, busOK := eaAdv.buses[en.GetMessageBus()]
	_, pipeOK := eaAdv.pipelines[en.GetDestination()]
	eaAdv.mu.Unlock()
	if !busOK {
		return nil, apierror.InvalidArgument("enrollment.message_bus %s not found", en.GetMessageBus())
	}
	if !pipeOK {
		return nil, apierror.InvalidArgument("enrollment.destination %s not found", en.GetDestination())
	}
	return prg, nil
}

// CreateEnrollment subscribes a pipeline to a bus's events that match.
func (e *EventarcServer) CreateEnrollment(_ context.Context, req *eventarcpb.CreateEnrollmentRequest) (*longrunningpb.Operation, error) {
	if _, err := eaAdvParent(req.GetParent(), req.GetEnrollmentId(), "enrollment"); err != nil {
		return nil, err
	}
	in := req.GetEnrollment()
	if in == nil {
		return nil, apierror.InvalidArgument("enrollment is required")
	}
	name := req.GetParent() + "/enrollments/" + req.GetEnrollmentId()
	prg, err := checkEnrollment(name, in)
	if err != nil {
		return nil, err
	}
	en := proto.Clone(in).(*eventarcpb.Enrollment)
	now := timestamppb.Now()
	en.Name, en.Uid, en.CreateTime, en.UpdateTime = name, uuid.NewString(), now, now
	en.Etag = eaAdvEtag(name, now)
	eaAdv.mu.Lock()
	defer eaAdv.mu.Unlock()
	if _, ok := eaAdv.enrollments[name]; ok {
		return nil, apierror.AlreadyExists("enrollment %s already exists", name)
	}
	if !req.GetValidateOnly() {
		eaAdv.enrollments[name] = &eaEnrollment{e: en, prg: prg}
	}
	return e.eaAdvOp(name, "create", proto.Clone(en))
}

// GetEnrollment reads an enrollment.
func (e *EventarcServer) GetEnrollment(_ context.Context, req *eventarcpb.GetEnrollmentRequest) (*eventarcpb.Enrollment, error) {
	eaAdv.mu.Lock()
	defer eaAdv.mu.Unlock()
	x, ok := eaAdv.enrollments[req.GetName()]
	if !ok {
		return nil, apierror.NotFound("enrollment %s not found", req.GetName())
	}
	return proto.Clone(x.e).(*eventarcpb.Enrollment), nil
}

// ListEnrollments lists a location's enrollments.
func (e *EventarcServer) ListEnrollments(_ context.Context, req *eventarcpb.ListEnrollmentsRequest) (*eventarcpb.ListEnrollmentsResponse, error) {
	eaAdv.mu.Lock()
	var all []*eventarcpb.Enrollment
	for _, n := range sortedKeys(eaAdv.enrollments) {
		if eaAdvInParent(n, req.GetParent(), "enrollments") {
			all = append(all, proto.Clone(eaAdv.enrollments[n].e).(*eventarcpb.Enrollment))
		}
	}
	eaAdv.mu.Unlock()
	all, err := aipfilter.List(req.GetFilter(), req.GetOrderBy(), all)
	if err != nil {
		return nil, err
	}
	items, next, err := page(all, req.GetPageSize(), req.GetPageToken(), req.GetParent()+"|enrollments|"+req.GetFilter()+"|"+req.GetOrderBy())
	if err != nil {
		return nil, err
	}
	return &eventarcpb.ListEnrollmentsResponse{Enrollments: items, NextPageToken: next}, nil
}

// UpdateEnrollment changes an enrollment's expression, destination,
// labels, annotations and display name; its bus is fixed.
func (e *EventarcServer) UpdateEnrollment(ctx context.Context, req *eventarcpb.UpdateEnrollmentRequest) (*longrunningpb.Operation, error) {
	in := req.GetEnrollment()
	m := eaEnrollmentRE.FindStringSubmatch(in.GetName())
	if m == nil {
		return nil, apierror.InvalidArgument("enrollment.name %q must be projects/{project}/locations/{location}/enrollments/{enrollment}", in.GetName())
	}
	eaAdv.mu.Lock()
	cur, ok := eaAdv.enrollments[in.GetName()]
	eaAdv.mu.Unlock()
	if !ok {
		if req.GetAllowMissing() {
			return e.CreateEnrollment(ctx, &eventarcpb.CreateEnrollmentRequest{Parent: "projects/" + m[1] + "/locations/" + m[2],
				EnrollmentId: m[3], Enrollment: in, ValidateOnly: req.GetValidateOnly()})
		}
		return nil, apierror.NotFound("enrollment %s not found", in.GetName())
	}
	if in.GetEtag() != "" && in.GetEtag() != cur.e.GetEtag() {
		return nil, apierror.Aborted("the enrollment's etag %q is not its current %q", in.GetEtag(), cur.e.GetEtag())
	}
	en := proto.Clone(cur.e).(*eventarcpb.Enrollment)
	paths := req.GetUpdateMask().GetPaths()
	if len(paths) == 0 || (len(paths) == 1 && paths[0] == "*") {
		paths = []string{"labels", "annotations", "display_name", "cel_match", "destination"}
	}
	for _, p := range paths {
		switch p {
		case "labels":
			en.Labels = in.GetLabels()
		case "annotations":
			en.Annotations = in.GetAnnotations()
		case "display_name", "displayName":
			en.DisplayName = in.GetDisplayName()
		case "cel_match", "celMatch":
			en.CelMatch = in.GetCelMatch()
		case "destination":
			en.Destination = in.GetDestination()
		case "message_bus", "messageBus":
			if in.GetMessageBus() != en.GetMessageBus() {
				return nil, apierror.InvalidArgument("an enrollment's message_bus cannot be changed")
			}
		default:
			return nil, apierror.InvalidArgument("update_mask path %q cannot be updated on an enrollment", p)
		}
	}
	prg, err := checkEnrollment(en.GetName(), en)
	if err != nil {
		return nil, err
	}
	en.UpdateTime = timestamppb.Now()
	en.Etag = eaAdvEtag(en.GetName(), en.GetUpdateTime())
	if !req.GetValidateOnly() {
		eaAdv.mu.Lock()
		eaAdv.enrollments[en.GetName()] = &eaEnrollment{e: en, prg: prg}
		eaAdv.mu.Unlock()
	}
	return e.eaAdvOp(en.GetName(), "update", proto.Clone(en))
}

// DeleteEnrollment removes an enrollment.
func (e *EventarcServer) DeleteEnrollment(_ context.Context, req *eventarcpb.DeleteEnrollmentRequest) (*longrunningpb.Operation, error) {
	eaAdv.mu.Lock()
	x, ok := eaAdv.enrollments[req.GetName()]
	if ok && req.GetEtag() != "" && req.GetEtag() != x.e.GetEtag() {
		eaAdv.mu.Unlock()
		return nil, apierror.Aborted("the enrollment's etag %q is not its current %q", req.GetEtag(), x.e.GetEtag())
	}
	if ok && !req.GetValidateOnly() {
		delete(eaAdv.enrollments, req.GetName())
	}
	eaAdv.mu.Unlock()
	if !ok {
		if req.GetAllowMissing() {
			return e.eaAdvOp(req.GetName(), "delete", &eventarcpb.Enrollment{Name: req.GetName()})
		}
		return nil, apierror.NotFound("enrollment %s not found", req.GetName())
	}
	return e.eaAdvOp(req.GetName(), "delete", proto.Clone(x.e))
}

// ---- publishing ----

// PublisherServer is google.cloud.eventarc.publishing.v1.Publisher: Publish
// to a message bus. PublishEvents and PublishChannelConnectionEvents are
// for channels, which are not served (eventarcchannels.go).
type PublisherServer struct {
	publishingpb.UnimplementedPublisherServer
	f *FunctionsServer
}

// EventarcPublisher is the Publishing API over the functions' events.
func (f *FunctionsServer) EventarcPublisher() *PublisherServer { return &PublisherServer{f: f} }

// Register adds the service to a gRPC server or a JSON transcoder.
func (p *PublisherServer) Register(g grpc.ServiceRegistrar) {
	publishingpb.RegisterPublisherServer(g, p)
}

// advEvent is a published event: its attributes as CEL sees them (time a
// timestamp, the rest strings) and its CloudEvent JSON form.
type advEvent struct {
	attrs map[string]any
	json  map[string]any
}

// parseJSONEvent reads a json_message: a CloudEvent in JSON format.
func parseJSONEvent(s string) (*advEvent, error) {
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return nil, apierror.InvalidArgument("json_message is not a JSON CloudEvent: %v", err)
	}
	ev := &advEvent{attrs: map[string]any{}, json: m}
	for _, k := range []string{"specversion", "type", "source", "id"} {
		if v, _ := m[k].(string); v == "" {
			return nil, apierror.InvalidArgument("json_message: the CloudEvent attribute %q is required", k)
		}
	}
	for k, v := range m {
		if k == "data" || k == "data_base64" {
			continue
		}
		switch x := v.(type) {
		case string:
			ev.attrs[k] = x
		case nil:
		default:
			b, _ := json.Marshal(x)
			ev.attrs[k] = string(b)
		}
	}
	if t, ok := ev.attrs["time"].(string); ok {
		if ts, err := time.Parse(time.RFC3339Nano, t); err == nil {
			ev.attrs["time"] = ts
		}
	}
	if _, ok := m["datacontenttype"]; !ok {
		if _, hasData := m["data"]; hasData {
			m["datacontenttype"] = "application/json"
		}
	}
	return ev, nil
}

// protoEvent reads a proto_message.
func protoEvent(c *publishingpb.CloudEvent) (*advEvent, error) {
	if c.GetSpecVersion() == "" || c.GetType() == "" || c.GetSource() == "" || c.GetId() == "" {
		return nil, apierror.InvalidArgument("proto_message: id, source, spec_version and type are required")
	}
	ev := &advEvent{attrs: map[string]any{"specversion": c.GetSpecVersion(), "type": c.GetType(), "source": c.GetSource(), "id": c.GetId()},
		json: map[string]any{"specversion": c.GetSpecVersion(), "type": c.GetType(), "source": c.GetSource(), "id": c.GetId()}}
	for k, v := range c.GetAttributes() {
		var s string
		var j any
		switch a := v.GetAttr().(type) {
		case *publishingpb.CloudEvent_CloudEventAttributeValue_CeBoolean:
			s, j = fmt.Sprint(a.CeBoolean), a.CeBoolean
		case *publishingpb.CloudEvent_CloudEventAttributeValue_CeInteger:
			s, j = fmt.Sprint(a.CeInteger), a.CeInteger
		case *publishingpb.CloudEvent_CloudEventAttributeValue_CeString:
			s, j = a.CeString, a.CeString
		case *publishingpb.CloudEvent_CloudEventAttributeValue_CeBytes:
			s = base64.StdEncoding.EncodeToString(a.CeBytes)
			j = s
		case *publishingpb.CloudEvent_CloudEventAttributeValue_CeUri:
			s, j = a.CeUri, a.CeUri
		case *publishingpb.CloudEvent_CloudEventAttributeValue_CeUriRef:
			s, j = a.CeUriRef, a.CeUriRef
		case *publishingpb.CloudEvent_CloudEventAttributeValue_CeTimestamp:
			s = a.CeTimestamp.AsTime().UTC().Format(time.RFC3339Nano)
			j = s
			if k == "time" {
				ev.attrs[k] = a.CeTimestamp.AsTime()
				ev.json[k] = j
				continue
			}
		}
		ev.attrs[k], ev.json[k] = s, j
	}
	switch d := c.GetData().(type) {
	case *publishingpb.CloudEvent_TextData:
		var v any
		if json.Unmarshal([]byte(d.TextData), &v) == nil && (ev.json["datacontenttype"] == nil || strings.Contains(fmt.Sprint(ev.json["datacontenttype"]), "json")) {
			ev.json["data"] = v
		} else {
			ev.json["data"] = d.TextData
		}
	case *publishingpb.CloudEvent_BinaryData:
		ev.json["data_base64"] = base64.StdEncoding.EncodeToString(d.BinaryData)
	case *publishingpb.CloudEvent_ProtoData:
		b, err := protojson.Marshal(d.ProtoData)
		if err != nil {
			return nil, apierror.InvalidArgument("proto_message.proto_data: %v", err)
		}
		var v any
		_ = json.Unmarshal(b, &v)
		ev.json["data"] = v
	}
	return ev, nil
}

// Publish publishes one event to a bus; each enrollment whose cel_match
// matches routes it to its pipeline.
func (p *PublisherServer) Publish(_ context.Context, req *publishingpb.PublishRequest) (*publishingpb.PublishResponse, error) {
	if !eaBusRE.MatchString(req.GetMessageBus()) {
		return nil, apierror.InvalidArgument("message_bus %q must be projects/{project}/locations/{location}/messageBuses/{bus}", req.GetMessageBus())
	}
	var ev *advEvent
	var err error
	switch f := req.GetFormat().(type) {
	case *publishingpb.PublishRequest_JsonMessage:
		ev, err = parseJSONEvent(f.JsonMessage)
	case *publishingpb.PublishRequest_ProtoMessage_:
		ev, err = protoEvent(f.ProtoMessage_)
	case *publishingpb.PublishRequest_AvroMessage:
		return nil, apierror.Unimplemented("avro_message is not supported by CloudBurrow: publish json_message or proto_message")
	default:
		return nil, apierror.InvalidArgument("one of json_message, proto_message or avro_message is required")
	}
	if err != nil {
		return nil, err
	}
	eaAdv.mu.Lock()
	if _, ok := eaAdv.buses[req.GetMessageBus()]; !ok {
		eaAdv.mu.Unlock()
		return nil, apierror.NotFound("message bus %s not found", req.GetMessageBus())
	}
	var targets []*eventarcpb.Pipeline
	for _, n := range sortedKeys(eaAdv.enrollments) {
		x := eaAdv.enrollments[n]
		if x.e.GetMessageBus() != req.GetMessageBus() || !enrollmentMatches(x.prg, ev.attrs) {
			continue
		}
		if pl, ok := eaAdv.pipelines[x.e.GetDestination()]; ok {
			targets = append(targets, proto.Clone(pl).(*eventarcpb.Pipeline))
		}
	}
	eaAdv.mu.Unlock()
	for _, pl := range targets {
		go p.f.deliverPipeline(pl, ev)
	}
	return &publishingpb.PublishResponse{}, nil
}

// pipelineRetry is a pipeline's retry policy, Google's defaults filled in.
func pipelineRetry(pl *eventarcpb.Pipeline) (attempts int, minDelay, maxDelay time.Duration) {
	attempts, minDelay, maxDelay = 5, time.Second, 60*time.Second
	if r := pl.GetRetryPolicy(); r != nil {
		if r.GetMaxAttempts() > 0 {
			attempts = int(r.GetMaxAttempts())
		}
		if d := r.GetMinRetryDelay().AsDuration(); d > 0 {
			minDelay = d
		}
		if d := r.GetMaxRetryDelay().AsDuration(); d > 0 {
			maxDelay = d
		}
	}
	return attempts, minDelay, maxDelay
}

func (f *FunctionsServer) deliverPipeline(pl *eventarcpb.Pipeline, ev *advEvent) {
	attempts, delay, maxDelay := pipelineRetry(pl)
	for i := 0; i < attempts; i++ {
		err := f.sendPipeline(context.Background(), pl, ev)
		if err == nil {
			return
		}
		slog.Warn("eventarc: pipeline delivery failed", "pipeline", pl.GetName(), "attempt", i+1, "err", err)
		if i+1 < attempts {
			time.Sleep(delay)
			delay = min(delay*2, maxDelay)
		}
	}
}

// eaAdvWorkflowArgument is the event as a workflow's runtime argument: the
// CloudEvent in its JSON format.
func eaAdvWorkflowArgument(ev *advEvent) (string, error) {
	b, err := json.Marshal(ev.json)
	return string(b), err
}

// eaAdvClient consults no proxy, so an event to a local URL never leaves
// the machine through one.
var eaAdvClient = &http.Client{Timeout: time.Minute, Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(r *http.Request, _ []*http.Request) error {
	if !isLoopbackHost(r.URL.Hostname()) {
		return fmt.Errorf("refused a redirect to %s: CloudBurrow sends events only to this machine", r.URL.Host)
	}
	return nil
}}

// sendPipeline sends one event to a pipeline's destination.
func (f *FunctionsServer) sendPipeline(ctx context.Context, pl *eventarcpb.Pipeline, ev *advEvent) error {
	if len(pl.GetDestinations()) == 0 {
		return fmt.Errorf("the pipeline has no destination")
	}
	d := pl.GetDestinations()[0]
	if wf := d.GetWorkflow(); wf != "" {
		if err := f.checkWorkflow(wf); err != nil {
			return err
		}
		arg, err := eaAdvWorkflowArgument(ev)
		if err != nil {
			return err
		}
		return f.events.Workflows.StartWorkflow(ctx, wf, arg)
	}
	he := d.GetHttpEndpoint()
	if he == nil || !eaLocalURL(he.GetUri()) {
		return fmt.Errorf("the destination is not an HTTP endpoint on this machine; nothing was sent")
	}
	var body []byte
	if v, ok := ev.json["data"]; ok {
		if s, isStr := v.(string); isStr {
			body = []byte(s)
		} else {
			body, _ = json.Marshal(v)
		}
	} else if s, ok := ev.json["data_base64"].(string); ok {
		body, _ = base64.StdEncoding.DecodeString(s)
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, he.GetUri(), strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	for k, v := range ev.json {
		if k == "data" || k == "data_base64" {
			continue
		}
		if k == "datacontenttype" {
			req.Header.Set("Content-Type", fmt.Sprint(v))
			continue
		}
		req.Header.Set("ce-"+k, fmt.Sprint(v))
	}
	resp, err := eaAdvClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("the endpoint answered %d", resp.StatusCode)
	}
	return nil
}

// eaPublishPathRE is the Publishing API's JSON path for a bus,
// POST /v1/{message_bus}:publish, which no other API on this port has.
var eaPublishPathRE = regexp.MustCompile(`^/v1/projects/[^/]+/locations/[^/]+/messageBuses/[^/:]+:publish$`)

// PublisherHandlerOr serves the Publishing API's JSON Publish (register
// adds the Publisher) and passes every other request to next.
func PublisherHandlerOr(register func(grpc.ServiceRegistrar), next http.Handler) http.Handler {
	t := &rest.Transcoder{Packages: []string{"google.cloud.eventarc.publishing.v1"}}
	register(t)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !eaPublishPathRE.MatchString(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		t.ServeHTTP(w, r)
	})
}
