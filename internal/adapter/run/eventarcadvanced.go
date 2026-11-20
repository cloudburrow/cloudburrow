package run

// Eventarc Advanced (#1947): message buses, pipelines, enrollments and
// Google API sources, as Google's "Eventarc Advanced overview" and the v1
// REST reference document them, kept in memory as triggers are. Each
// resource is created, read, listed, changed and deleted through a
// long-running operation that is done when it returns; events then flow
// for real (eventarcbus.go): a message published to a bus
// (eventarcpublishing.go), or a Google API source's event, reaches each
// enrollment whose CEL match accepts it, and the enrollment's pipeline
// decodes it in its input format, runs its transformation, encodes it in
// the destination's output format and delivers it, with the pipeline's
// retry policy, to an HTTP endpoint (a Cloud Run service of this instance
// among them), a Pub/Sub topic or another bus. Channels and channel
// connections (eventarcchannels.go) carry custom and third-party events to
// triggers.

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	eventarcpb "cloud.google.com/go/eventarc/apiv1/eventarcpb"
	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/resource"
)

// eaRecord is one Eventarc Advanced resource, or a channel.
type eaRecord interface {
	proto.Message
	GetName() string
}

// eaKind is one collection: its noun, its path segment, the fields an
// update may change and the check of a resource about to be stored.
type eaKind[T eaRecord] struct {
	noun, collection string
	mutable          []string
	m                map[string]T
	idRE             *regexp.Regexp
}

// eaAdvIDRE is the ID Google documents for a bus, pipeline, enrollment
// and Google API source: ^[a-z]([a-z0-9-]{0,61}[a-z0-9])?$.
var eaAdvIDRE = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,61}[a-z0-9])?$`)

var eaAdv = struct {
	mu          sync.Mutex
	buses       *eaKind[*eventarcpb.MessageBus]
	pipelines   *eaKind[*eventarcpb.Pipeline]
	enrollments *eaKind[*eventarcpb.Enrollment]
	sources     *eaKind[*eventarcpb.GoogleApiSource]
	channels    *eaKind[*eventarcpb.Channel]
	conns       *eaKind[*eventarcpb.ChannelConnection]
	configs     map[string]*eventarcpb.GoogleChannelConfig
}{
	buses: &eaKind[*eventarcpb.MessageBus]{noun: "message bus", collection: "messageBuses", idRE: eaAdvIDRE,
		mutable: []string{"labels", "annotations", "display_name", "crypto_key_name", "logging_config"},
		m:       map[string]*eventarcpb.MessageBus{}},
	pipelines: &eaKind[*eventarcpb.Pipeline]{noun: "pipeline", collection: "pipelines", idRE: eaAdvIDRE,
		mutable: []string{"labels", "annotations", "display_name", "destinations", "mediations", "crypto_key_name",
			"input_payload_format", "logging_config", "retry_policy"},
		m: map[string]*eventarcpb.Pipeline{}},
	enrollments: &eaKind[*eventarcpb.Enrollment]{noun: "enrollment", collection: "enrollments", idRE: eaAdvIDRE,
		mutable: []string{"labels", "annotations", "display_name", "cel_match", "destination"},
		m:       map[string]*eventarcpb.Enrollment{}},
	sources: &eaKind[*eventarcpb.GoogleApiSource]{noun: "Google API source", collection: "googleApiSources", idRE: eaAdvIDRE,
		mutable: []string{"labels", "annotations", "display_name", "destination", "crypto_key_name", "logging_config",
			"organization_subscription", "project_subscriptions"},
		m: map[string]*eventarcpb.GoogleApiSource{}},
	channels: &eaKind[*eventarcpb.Channel]{noun: "channel", collection: "channels", idRE: eaTriggerIDRE,
		mutable: []string{"crypto_key_name", "labels"}, m: map[string]*eventarcpb.Channel{}},
	conns: &eaKind[*eventarcpb.ChannelConnection]{noun: "channel connection", collection: "channelConnections", idRE: eaTriggerIDRE,
		m: map[string]*eventarcpb.ChannelConnection{}},
	configs: map[string]*eventarcpb.GoogleChannelConfig{},
}

// eaLocOf is the projects/*/locations/* of a resource name.
func eaLocOf(name string) string {
	parts := strings.SplitN(name, "/", 5)
	if len(parts) < 4 {
		return name
	}
	return strings.Join(parts[:4], "/")
}

func eaCheckKey(key string) error {
	if key != "" && !cryptoKeyRE.MatchString(key) {
		return apierror.InvalidArgument("crypto_key_name %q must be projects/*/locations/*/keyRings/*/cryptoKeys/*", key)
	}
	return nil
}

// eaStamp sets a resource's output fields: name, uid and create_time on
// create, update_time, and its etag, the fields its type has.
func eaStamp(m proto.Message, name string, create bool) {
	r := m.ProtoReflect()
	fs := r.Descriptor().Fields()
	now := timestamppb.Now()
	set := func(f string, v protoreflect.Value) {
		if fd := fs.ByName(protoreflect.Name(f)); fd != nil {
			r.Set(fd, v)
		}
	}
	if create {
		set("name", protoreflect.ValueOfString(name))
		set("uid", protoreflect.ValueOfString(uuid.NewString()))
		set("create_time", protoreflect.ValueOfMessage(now.ProtoReflect()))
	}
	set("update_time", protoreflect.ValueOfMessage(proto.Clone(now).ProtoReflect()))
	if fs.ByName("etag") != nil {
		set("etag", protoreflect.ValueOfString(fmt.Sprintf("%x", resource.ProjectNumber(name+now.AsTime().String()))))
	}
}

// eaEtagOf is a resource's etag, or "" for a type with none.
func eaEtagOf(m proto.Message) string {
	r := m.ProtoReflect()
	if fd := r.Descriptor().Fields().ByName("etag"); fd != nil {
		return r.Get(fd).String()
	}
	return ""
}

// eaClearOutput clears what a caller may not set.
func eaClearOutput(m proto.Message) {
	r := m.ProtoReflect()
	for _, f := range []string{"uid", "etag", "create_time", "update_time", "state", "activation_token", "satisfies_pzs"} {
		if fd := r.Descriptor().Fields().ByName(protoreflect.Name(f)); fd != nil {
			r.Clear(fd)
		}
	}
}

// eaSnake turns a JSON field-mask path into its proto name.
func eaSnake(p string) string {
	var b strings.Builder
	for _, c := range p {
		if c >= 'A' && c <= 'Z' {
			b.WriteByte('_')
			b.WriteRune(c + 'a' - 'A')
			continue
		}
		b.WriteRune(c)
	}
	return b.String()
}

// eaApplyMask copies the masked top-level fields of in onto cur; an empty
// mask or * copies every mutable field.
func eaApplyMask(cur, in proto.Message, mask *fieldmaskpb.FieldMask, mutable []string, noun string) error {
	paths := mask.GetPaths()
	if len(paths) == 0 || (len(paths) == 1 && paths[0] == "*") {
		paths = mutable
	}
	cr, ir := cur.ProtoReflect(), in.ProtoReflect()
	for _, p := range paths {
		top, _, _ := strings.Cut(eaSnake(p), ".")
		ok := false
		for _, m := range mutable {
			ok = ok || m == top
		}
		if !ok {
			return apierror.InvalidArgument("update_mask path %q is not a field of a %s that can be changed", p, noun)
		}
		fd := cr.Descriptor().Fields().ByName(protoreflect.Name(top))
		if fd == nil {
			return apierror.InvalidArgument("update_mask path %q is not a field of a %s", p, noun)
		}
		if od := fd.ContainingOneof(); od != nil && !od.IsSynthetic() {
			for i := 0; i < od.Fields().Len(); i++ {
				cr.Clear(od.Fields().Get(i))
			}
		}
		if ir.Has(fd) {
			cr.Set(fd, ir.Get(fd))
		} else {
			cr.Clear(fd)
		}
	}
	return nil
}

// eaGet reads one resource.
func eaGet[T eaRecord](k *eaKind[T], name string) (T, error) {
	eaAdv.mu.Lock()
	defer eaAdv.mu.Unlock()
	x, ok := k.m[name]
	if !ok {
		var zero T
		return zero, apierror.NotFound("%s %s not found", k.noun, name)
	}
	return proto.Clone(x).(T), nil
}

// eaList lists a location's resources (every location's for -), by name,
// as order_by "name" lists them; a filter is not supported.
func eaList[T eaRecord](k *eaKind[T], parent string, size int32, token, filter, orderBy string) ([]T, string, error) {
	if _, _, err := resource.ParseLocation(parent); err != nil {
		return nil, "", apierror.InvalidArgument("%v", err)
	}
	if filter != "" {
		return nil, "", apierror.Unimplemented("the List filter of %s is not supported", k.collection)
	}
	desc := false
	switch strings.TrimSpace(orderBy) {
	case "", "name", "name asc":
	case "name desc":
		desc = true
	default:
		return nil, "", apierror.Unimplemented("order_by %q is not supported: %s are listed by name", orderBy, k.collection)
	}
	prefix := parent + "/" + k.collection + "/"
	if strings.HasSuffix(parent, "/locations/-") {
		prefix = strings.TrimSuffix(parent, "-")
	}
	eaAdv.mu.Lock()
	var all []T
	for _, n := range sortedKeys(k.m) {
		if strings.HasPrefix(n, prefix) && strings.Contains(n, "/"+k.collection+"/") {
			all = append(all, proto.Clone(k.m[n]).(T))
		}
	}
	eaAdv.mu.Unlock()
	if desc {
		sort.SliceStable(all, func(i, j int) bool { return all[i].GetName() > all[j].GetName() })
	}
	return page(all, size, token, parent+"/"+k.collection+orderBy)
}

// eaCreate checks and stores a new resource; check sees it named, before
// it is stored.
func eaCreate[T eaRecord](e *EventarcServer, k *eaKind[T], parent, id string, in T, validateOnly bool, check func(T) error) (*longrunningpb.Operation, error) {
	if _, _, err := resource.ParseLocation(parent); err != nil {
		return nil, apierror.InvalidArgument("%v", err)
	}
	if !k.idRE.MatchString(id) {
		return nil, apierror.InvalidArgument("the %s ID %q must match %s", k.noun, id, k.idRE.String())
	}
	if any(in) == nil || !in.ProtoReflect().IsValid() {
		return nil, apierror.InvalidArgument("the %s is required", k.noun)
	}
	x := proto.Clone(in).(T)
	eaClearOutput(x)
	name := parent + "/" + k.collection + "/" + id
	eaStamp(x, name, true)
	if err := check(x); err != nil {
		return nil, err
	}
	eaAdv.mu.Lock()
	if _, ok := k.m[name]; ok {
		eaAdv.mu.Unlock()
		return nil, apierror.AlreadyExists("%s %s already exists", k.noun, name)
	}
	if !validateOnly {
		k.m[name] = x
	}
	eaAdv.mu.Unlock()
	return e.op(name, "create", proto.Clone(x))
}

// eaUpdate changes a resource's masked fields; with allowMissing a
// missing one is created.
func eaUpdate[T eaRecord](e *EventarcServer, k *eaKind[T], in T, mask *fieldmaskpb.FieldMask, allowMissing, validateOnly bool, check func(T) error) (*longrunningpb.Operation, error) {
	if any(in) == nil || !in.ProtoReflect().IsValid() {
		return nil, apierror.InvalidArgument("the %s is required", k.noun)
	}
	name := in.GetName()
	eaAdv.mu.Lock()
	cur, ok := k.m[name]
	eaAdv.mu.Unlock()
	if !ok {
		i := strings.LastIndex(name, "/"+k.collection+"/")
		if allowMissing && i > 0 {
			return eaCreate(e, k, name[:i], name[i+len(k.collection)+2:], in, validateOnly, check)
		}
		return nil, apierror.NotFound("%s %s not found", k.noun, name)
	}
	if et := eaEtagOf(in); et != "" && et != eaEtagOf(cur) {
		return nil, apierror.Aborted("the %s's etag %q is not its current %q", k.noun, et, eaEtagOf(cur))
	}
	next := proto.Clone(cur).(T)
	if err := eaApplyMask(next, in, mask, k.mutable, k.noun); err != nil {
		return nil, err
	}
	if err := check(next); err != nil {
		return nil, err
	}
	eaStamp(next, name, false)
	if !validateOnly {
		eaAdv.mu.Lock()
		k.m[name] = next
		eaAdv.mu.Unlock()
	}
	return e.op(name, "update", proto.Clone(next))
}

// eaDelete removes a resource.
func eaDelete[T eaRecord](e *EventarcServer, k *eaKind[T], name, etag string, allowMissing, validateOnly bool, check func(T) error) (*longrunningpb.Operation, error) {
	eaAdv.mu.Lock()
	cur, ok := k.m[name]
	eaAdv.mu.Unlock()
	if !ok {
		if allowMissing {
			var zero T
			return e.op(name, "delete", zero.ProtoReflect().New().Interface())
		}
		return nil, apierror.NotFound("%s %s not found", k.noun, name)
	}
	if etag != "" && etag != eaEtagOf(cur) {
		return nil, apierror.Aborted("the %s's etag %q is not its current %q", k.noun, etag, eaEtagOf(cur))
	}
	if check != nil {
		if err := check(cur); err != nil {
			return nil, err
		}
	}
	if !validateOnly {
		eaAdv.mu.Lock()
		delete(k.m, name)
		eaAdv.mu.Unlock()
	}
	return e.op(name, "delete", proto.Clone(cur))
}

// eaExists reports whether a resource is stored.
func eaExists[T eaRecord](k *eaKind[T], name string) bool {
	eaAdv.mu.Lock()
	defer eaAdv.mu.Unlock()
	_, ok := k.m[name]
	return ok
}

// --- Message buses.

func eaCheckLogging(c *eventarcpb.LoggingConfig) error {
	if c != nil && (c.GetLogSeverity() < 0 || c.GetLogSeverity() > eventarcpb.LoggingConfig_EMERGENCY) {
		return apierror.InvalidArgument("logging_config.log_severity %d is not a LogSeverity", c.GetLogSeverity())
	}
	return nil
}

func eaCheckBus(b *eventarcpb.MessageBus) error {
	if err := eaCheckKey(b.GetCryptoKeyName()); err != nil {
		return err
	}
	return eaCheckLogging(b.GetLoggingConfig())
}

func (e *EventarcServer) GetMessageBus(_ context.Context, req *eventarcpb.GetMessageBusRequest) (*eventarcpb.MessageBus, error) {
	return eaGet(eaAdv.buses, req.GetName())
}

func (e *EventarcServer) ListMessageBuses(_ context.Context, req *eventarcpb.ListMessageBusesRequest) (*eventarcpb.ListMessageBusesResponse, error) {
	items, next, err := eaList(eaAdv.buses, req.GetParent(), req.GetPageSize(), req.GetPageToken(), req.GetFilter(), req.GetOrderBy())
	if err != nil {
		return nil, err
	}
	return &eventarcpb.ListMessageBusesResponse{MessageBuses: items, NextPageToken: next}, nil
}

// ListMessageBusEnrollments names the enrollments on a bus.
func (e *EventarcServer) ListMessageBusEnrollments(_ context.Context, req *eventarcpb.ListMessageBusEnrollmentsRequest) (*eventarcpb.ListMessageBusEnrollmentsResponse, error) {
	if !eaExists(eaAdv.buses, req.GetParent()) {
		return nil, apierror.NotFound("message bus %s not found", req.GetParent())
	}
	var names []string
	for _, en := range eaEnrollmentsOn(req.GetParent()) {
		names = append(names, en.GetName())
	}
	items, next, err := page(names, req.GetPageSize(), req.GetPageToken(), req.GetParent()+":listEnrollments")
	if err != nil {
		return nil, err
	}
	return &eventarcpb.ListMessageBusEnrollmentsResponse{Enrollments: items, NextPageToken: next}, nil
}

func (e *EventarcServer) CreateMessageBus(_ context.Context, req *eventarcpb.CreateMessageBusRequest) (*longrunningpb.Operation, error) {
	return eaCreate(e, eaAdv.buses, req.GetParent(), req.GetMessageBusId(), req.GetMessageBus(), req.GetValidateOnly(), eaCheckBus)
}

func (e *EventarcServer) UpdateMessageBus(_ context.Context, req *eventarcpb.UpdateMessageBusRequest) (*longrunningpb.Operation, error) {
	return eaUpdate(e, eaAdv.buses, req.GetMessageBus(), req.GetUpdateMask(), req.GetAllowMissing(), req.GetValidateOnly(), eaCheckBus)
}

func (e *EventarcServer) DeleteMessageBus(_ context.Context, req *eventarcpb.DeleteMessageBusRequest) (*longrunningpb.Operation, error) {
	return eaDelete(e, eaAdv.buses, req.GetName(), req.GetEtag(), req.GetAllowMissing(), req.GetValidateOnly(), nil)
}

// --- Enrollments.

func eaCheckEnrollment(en *eventarcpb.Enrollment) error {
	if en.GetCelMatch() == "" {
		return apierror.InvalidArgument("enrollment.cel_match is required")
	}
	if _, err := eaCompileMatch(en.GetCelMatch()); err != nil {
		return apierror.InvalidArgument("enrollment.cel_match %q: %v", en.GetCelMatch(), err)
	}
	if en.GetMessageBus() == "" {
		return apierror.InvalidArgument("enrollment.message_bus is required")
	}
	if !eaExists(eaAdv.buses, en.GetMessageBus()) {
		return apierror.NotFound("the enrollment's message bus %s not found", en.GetMessageBus())
	}
	if eaLocOf(en.GetMessageBus()) != eaLocOf(en.GetName()) {
		return apierror.InvalidArgument("the enrollment's message bus %s must be in its location %s", en.GetMessageBus(), eaLocOf(en.GetName()))
	}
	if en.GetDestination() == "" {
		return apierror.InvalidArgument("enrollment.destination is required: projects/*/locations/*/pipelines/*")
	}
	if !strings.Contains(en.GetDestination(), "/pipelines/") {
		return apierror.InvalidArgument("enrollment.destination %q must be projects/*/locations/*/pipelines/*", en.GetDestination())
	}
	if !eaExists(eaAdv.pipelines, en.GetDestination()) {
		return apierror.NotFound("the enrollment's destination pipeline %s not found", en.GetDestination())
	}
	return nil
}

// eaEnrollmentsOn are the enrollments on a bus, by name.
func eaEnrollmentsOn(bus string) []*eventarcpb.Enrollment {
	eaAdv.mu.Lock()
	defer eaAdv.mu.Unlock()
	var out []*eventarcpb.Enrollment
	for _, n := range sortedKeys(eaAdv.enrollments.m) {
		if en := eaAdv.enrollments.m[n]; en.GetMessageBus() == bus {
			out = append(out, proto.Clone(en).(*eventarcpb.Enrollment))
		}
	}
	return out
}

func (e *EventarcServer) GetEnrollment(_ context.Context, req *eventarcpb.GetEnrollmentRequest) (*eventarcpb.Enrollment, error) {
	return eaGet(eaAdv.enrollments, req.GetName())
}

func (e *EventarcServer) ListEnrollments(_ context.Context, req *eventarcpb.ListEnrollmentsRequest) (*eventarcpb.ListEnrollmentsResponse, error) {
	items, next, err := eaList(eaAdv.enrollments, req.GetParent(), req.GetPageSize(), req.GetPageToken(), req.GetFilter(), req.GetOrderBy())
	if err != nil {
		return nil, err
	}
	return &eventarcpb.ListEnrollmentsResponse{Enrollments: items, NextPageToken: next}, nil
}

func (e *EventarcServer) CreateEnrollment(_ context.Context, req *eventarcpb.CreateEnrollmentRequest) (*longrunningpb.Operation, error) {
	return eaCreate(e, eaAdv.enrollments, req.GetParent(), req.GetEnrollmentId(), req.GetEnrollment(), req.GetValidateOnly(), eaCheckEnrollment)
}

func (e *EventarcServer) UpdateEnrollment(_ context.Context, req *eventarcpb.UpdateEnrollmentRequest) (*longrunningpb.Operation, error) {
	if in := req.GetEnrollment(); in != nil {
		if cur, err := eaGet(eaAdv.enrollments, in.GetName()); err == nil && in.GetMessageBus() != "" && in.GetMessageBus() != cur.GetMessageBus() {
			return nil, apierror.InvalidArgument("enrollment.message_bus is immutable")
		}
	}
	return eaUpdate(e, eaAdv.enrollments, req.GetEnrollment(), req.GetUpdateMask(), req.GetAllowMissing(), req.GetValidateOnly(), eaCheckEnrollment)
}

func (e *EventarcServer) DeleteEnrollment(_ context.Context, req *eventarcpb.DeleteEnrollmentRequest) (*longrunningpb.Operation, error) {
	return eaDelete(e, eaAdv.enrollments, req.GetName(), req.GetEtag(), req.GetAllowMissing(), req.GetValidateOnly(), nil)
}

// --- Pipelines.

func eaCheckPipeline(e *EventarcServer) func(*eventarcpb.Pipeline) error {
	return func(p *eventarcpb.Pipeline) error {
		if err := eaCheckKey(p.GetCryptoKeyName()); err != nil {
			return err
		}
		if err := eaCheckLogging(p.GetLoggingConfig()); err != nil {
			return err
		}
		if len(p.GetDestinations()) != 1 {
			return apierror.InvalidArgument("pipeline.destinations must hold exactly one destination; it holds %d", len(p.GetDestinations()))
		}
		if len(p.GetMediations()) > 1 {
			return apierror.InvalidArgument("pipeline.mediations may hold at most one transformation; it holds %d", len(p.GetMediations()))
		}
		for _, m := range p.GetMediations() {
			tmpl := m.GetTransformation().GetTransformationTemplate()
			if tmpl == "" {
				return apierror.InvalidArgument("pipeline.mediations[0].transformation.transformation_template is required")
			}
			if _, err := eaCompileTemplate(tmpl, false); err != nil {
				return apierror.InvalidArgument("the transformation template %q: %v", tmpl, err)
			}
		}
		if _, err := eaFormatOf(p.GetInputPayloadFormat()); err != nil {
			return apierror.InvalidArgument("pipeline.input_payload_format: %v", err)
		}
		if rp := p.GetRetryPolicy(); rp != nil {
			if n := rp.GetMaxAttempts(); n != 0 && (n < 1 || n > 100) {
				return apierror.InvalidArgument("retry_policy.max_attempts %d must be between 1 and 100", n)
			}
			for f, d := range map[string]time.Duration{"min_retry_delay": rp.GetMinRetryDelay().AsDuration(), "max_retry_delay": rp.GetMaxRetryDelay().AsDuration()} {
				if d != 0 && (d < time.Second || d > 600*time.Second) {
					return apierror.InvalidArgument("retry_policy.%s %s must be between 1 and 600 seconds", f, d)
				}
			}
			if lo, hi := rp.GetMinRetryDelay().AsDuration(), rp.GetMaxRetryDelay().AsDuration(); lo != 0 && hi != 0 && lo > hi {
				return apierror.InvalidArgument("retry_policy.min_retry_delay %s exceeds max_retry_delay %s", lo, hi)
			}
		}
		project := strings.Split(p.GetName(), "/")[1]
		d := p.GetDestinations()[0]
		if d.GetOutputPayloadFormat() != nil {
			if p.GetInputPayloadFormat() == nil {
				return apierror.InvalidArgument("destinations[0].output_payload_format can be set only when pipeline.input_payload_format is")
			}
			if _, err := eaFormatOf(d.GetOutputPayloadFormat()); err != nil {
				return apierror.InvalidArgument("destinations[0].output_payload_format: %v", err)
			}
		}
		switch a := d.GetAuthenticationConfig(); {
		case a == nil:
		case a.GetGoogleOidc() != nil:
			if a.GetGoogleOidc().GetServiceAccount() == "" {
				return apierror.InvalidArgument("authentication_config.google_oidc.service_account is required")
			}
		case a.GetOauthToken() != nil:
			return apierror.Unimplemented("authentication_config.oauth_token is not supported: an OAuth token authenticates a call to a " +
				"Google API on *.googleapis.com, which this local instance never reaches; use google_oidc for an HTTP endpoint")
		}
		switch {
		case d.GetHttpEndpoint() != nil:
			he := d.GetHttpEndpoint()
			if he.GetUri() == "" {
				return apierror.InvalidArgument("destinations[0].http_endpoint.uri is required")
			}
			if _, local, err := e.eaEndpoint(eaLocOf(p.GetName()), he.GetUri()); err != nil {
				return err
			} else if !local && !strings.HasPrefix(he.GetUri(), "https://") {
				return apierror.InvalidArgument("destinations[0].http_endpoint.uri %q: only the HTTPS protocol is supported", he.GetUri())
			}
			if t := he.GetMessageBindingTemplate(); t != "" {
				if _, err := eaCompileTemplate(t, true); err != nil {
					return apierror.InvalidArgument("the message binding template %q: %v", t, err)
				}
			}
		case d.GetWorkflow() != "":
			return apierror.Unimplemented("destinations[0].workflow is not supported: this instance does not serve Workflows")
		case d.GetMessageBus() != "":
			if strings.Split(d.GetMessageBus(), "/")[1] != project {
				return apierror.InvalidArgument("destinations[0].message_bus %s must be in the pipeline's project %s", d.GetMessageBus(), project)
			}
			if !eaExists(eaAdv.buses, d.GetMessageBus()) {
				return apierror.NotFound("destinations[0].message_bus %s not found", d.GetMessageBus())
			}
		case d.GetTopic() != "":
			if eaTopicName(d.GetTopic()) == "" {
				return apierror.InvalidArgument("destinations[0].topic %q must be projects/*/topics/*", d.GetTopic())
			}
		default:
			return apierror.InvalidArgument("destinations[0] must name an http_endpoint, a workflow, a message_bus or a topic")
		}
		return nil
	}
}

// eaTopicName is a pipeline topic destination as Pub/Sub names it:
// projects/P/topics/T, given with or without locations/L.
func eaTopicName(t string) string {
	parts := strings.Split(t, "/")
	switch {
	case len(parts) == 4 && parts[0] == "projects" && parts[2] == "topics" && parts[1] != "" && parts[3] != "":
		return t
	case len(parts) == 6 && parts[0] == "projects" && parts[2] == "locations" && parts[4] == "topics" && parts[1] != "" && parts[5] != "":
		return "projects/" + parts[1] + "/topics/" + parts[5]
	}
	return ""
}

func (e *EventarcServer) GetPipeline(_ context.Context, req *eventarcpb.GetPipelineRequest) (*eventarcpb.Pipeline, error) {
	return eaGet(eaAdv.pipelines, req.GetName())
}

func (e *EventarcServer) ListPipelines(_ context.Context, req *eventarcpb.ListPipelinesRequest) (*eventarcpb.ListPipelinesResponse, error) {
	items, next, err := eaList(eaAdv.pipelines, req.GetParent(), req.GetPageSize(), req.GetPageToken(), req.GetFilter(), req.GetOrderBy())
	if err != nil {
		return nil, err
	}
	return &eventarcpb.ListPipelinesResponse{Pipelines: items, NextPageToken: next}, nil
}

func (e *EventarcServer) CreatePipeline(_ context.Context, req *eventarcpb.CreatePipelineRequest) (*longrunningpb.Operation, error) {
	return eaCreate(e, eaAdv.pipelines, req.GetParent(), req.GetPipelineId(), req.GetPipeline(), req.GetValidateOnly(), eaCheckPipeline(e))
}

func (e *EventarcServer) UpdatePipeline(_ context.Context, req *eventarcpb.UpdatePipelineRequest) (*longrunningpb.Operation, error) {
	return eaUpdate(e, eaAdv.pipelines, req.GetPipeline(), req.GetUpdateMask(), req.GetAllowMissing(), req.GetValidateOnly(), eaCheckPipeline(e))
}

func (e *EventarcServer) DeletePipeline(_ context.Context, req *eventarcpb.DeletePipelineRequest) (*longrunningpb.Operation, error) {
	return eaDelete(e, eaAdv.pipelines, req.GetName(), req.GetEtag(), req.GetAllowMissing(), req.GetValidateOnly(), nil)
}

// --- Google API sources.

func eaCheckSource(s *eventarcpb.GoogleApiSource) error {
	if err := eaCheckKey(s.GetCryptoKeyName()); err != nil {
		return err
	}
	if err := eaCheckLogging(s.GetLoggingConfig()); err != nil {
		return err
	}
	if s.GetDestination() == "" {
		return apierror.InvalidArgument("google_api_source.destination is required: projects/*/locations/*/messageBuses/*")
	}
	if !eaExists(eaAdv.buses, s.GetDestination()) {
		return apierror.NotFound("the Google API source's destination message bus %s not found", s.GetDestination())
	}
	for _, p := range s.GetProjectSubscriptions().GetList() {
		if !strings.HasPrefix(p, "projects/") || strings.Count(p, "/") != 1 {
			return apierror.InvalidArgument("project_subscriptions.list entry %q must be projects/{project}", p)
		}
	}
	return nil
}

func (e *EventarcServer) GetGoogleApiSource(_ context.Context, req *eventarcpb.GetGoogleApiSourceRequest) (*eventarcpb.GoogleApiSource, error) {
	return eaGet(eaAdv.sources, req.GetName())
}

func (e *EventarcServer) ListGoogleApiSources(_ context.Context, req *eventarcpb.ListGoogleApiSourcesRequest) (*eventarcpb.ListGoogleApiSourcesResponse, error) {
	items, next, err := eaList(eaAdv.sources, req.GetParent(), req.GetPageSize(), req.GetPageToken(), req.GetFilter(), req.GetOrderBy())
	if err != nil {
		return nil, err
	}
	return &eventarcpb.ListGoogleApiSourcesResponse{GoogleApiSources: items, NextPageToken: next}, nil
}

func (e *EventarcServer) CreateGoogleApiSource(_ context.Context, req *eventarcpb.CreateGoogleApiSourceRequest) (*longrunningpb.Operation, error) {
	return eaCreate(e, eaAdv.sources, req.GetParent(), req.GetGoogleApiSourceId(), req.GetGoogleApiSource(), req.GetValidateOnly(), eaCheckSource)
}

func (e *EventarcServer) UpdateGoogleApiSource(_ context.Context, req *eventarcpb.UpdateGoogleApiSourceRequest) (*longrunningpb.Operation, error) {
	return eaUpdate(e, eaAdv.sources, req.GetGoogleApiSource(), req.GetUpdateMask(), req.GetAllowMissing(), req.GetValidateOnly(), eaCheckSource)
}

func (e *EventarcServer) DeleteGoogleApiSource(_ context.Context, req *eventarcpb.DeleteGoogleApiSourceRequest) (*longrunningpb.Operation, error) {
	return eaDelete(e, eaAdv.sources, req.GetName(), req.GetEtag(), req.GetAllowMissing(), req.GetValidateOnly(), nil)
}

// eaSourcesFor are the Google API sources that receive a project's
// events: the project's own, those whose project_subscriptions name it,
// and those with organization_subscription enabled (this instance's
// projects are all in its one organization).
func eaSourcesFor(project string) []*eventarcpb.GoogleApiSource {
	eaAdv.mu.Lock()
	defer eaAdv.mu.Unlock()
	var out []*eventarcpb.GoogleApiSource
	for _, n := range sortedKeys(eaAdv.sources.m) {
		s := eaAdv.sources.m[n]
		own := strings.HasPrefix(n, "projects/"+project+"/")
		sub := s.GetOrganizationSubscription().GetEnabled()
		for _, p := range s.GetProjectSubscriptions().GetList() {
			sub = sub || p == "projects/"+project
		}
		if own || sub {
			out = append(out, proto.Clone(s).(*eventarcpb.GoogleApiSource))
		}
	}
	return out
}

// --- Google channel config.

func eaChannelConfigName(name string) (string, error) {
	loc, rest, ok := strings.Cut(name, "/googleChannelConfig")
	if _, _, err := resource.ParseLocation(loc); !ok || rest != "" || err != nil {
		return "", apierror.InvalidArgument("%q is not projects/*/locations/*/googleChannelConfig", name)
	}
	return name, nil
}

// GetGoogleChannelConfig reads a location's config; one never set has
// only its name.
func (e *EventarcServer) GetGoogleChannelConfig(_ context.Context, req *eventarcpb.GetGoogleChannelConfigRequest) (*eventarcpb.GoogleChannelConfig, error) {
	name, err := eaChannelConfigName(req.GetName())
	if err != nil {
		return nil, err
	}
	eaAdv.mu.Lock()
	defer eaAdv.mu.Unlock()
	if c, ok := eaAdv.configs[name]; ok {
		return proto.Clone(c).(*eventarcpb.GoogleChannelConfig), nil
	}
	return &eventarcpb.GoogleChannelConfig{Name: name}, nil
}

// UpdateGoogleChannelConfig sets a location's CMEK key and labels.
func (e *EventarcServer) UpdateGoogleChannelConfig(ctx context.Context, req *eventarcpb.UpdateGoogleChannelConfigRequest) (*eventarcpb.GoogleChannelConfig, error) {
	in := req.GetGoogleChannelConfig()
	cur, err := e.GetGoogleChannelConfig(ctx, &eventarcpb.GetGoogleChannelConfigRequest{Name: in.GetName()})
	if err != nil {
		return nil, err
	}
	if err := eaApplyMask(cur, in, req.GetUpdateMask(), []string{"crypto_key_name", "labels"}, "Google channel config"); err != nil {
		return nil, err
	}
	if err := eaCheckKey(cur.GetCryptoKeyName()); err != nil {
		return nil, err
	}
	cur.UpdateTime = timestamppb.Now()
	eaAdv.mu.Lock()
	eaAdv.configs[cur.GetName()] = proto.Clone(cur).(*eventarcpb.GoogleChannelConfig)
	eaAdv.mu.Unlock()
	return cur, nil
}
