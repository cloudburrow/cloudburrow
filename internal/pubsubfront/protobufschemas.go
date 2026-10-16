package pubsubfront

// Protocol Buffer schemas (#1406). The emulator serves the Schema service
// for AVRO and refuses every PROTOCOL_BUFFER definition UNIMPLEMENTED
// "Protocol buffer support not implemented in emulator" (ValidateSchema,
// CreateSchema, CommitSchema). The front serves them itself: it parses the
// definition (protoschema.go), keeps the schema and its revisions, and
// answers every Schema service call that names one, over gRPC and REST,
// with an AVRO schema's calls still the emulator's. ListSchemas names the
// front's schemas after the emulator's last page.
//
// Google's rules, from https://cloud.google.com/pubsub/docs/schemas and
// google/pubsub/v1/schema.proto:
//   - a definition is one top-level message type, proto2 or proto3, with no
//     imports (protoschema.go);
//   - GetSchema returns every field unless view is BASIC; ListSchemas and
//     ListSchemaRevisions return name and type unless view is FULL;
//   - a revision is named name@revision_id; CommitSchema adds one, of the
//     same type; RollbackSchema commits a copy of the one it names;
//     DeleteSchemaRevision refuses the last one ("Cannot delete last
//     revision. Please use DeleteSchema.", the emulator's words for AVRO);
//   - ValidateMessage checks a message, BINARY or JSON, against the schema
//     named (its latest revision, or name@revision_id) or given;
//   - a topic's schema_settings bind it to a schema, with an encoding and
//     optionally a first_revision_id and last_revision_id; a message
//     published to it must conform to one revision in that range, and is
//     refused INVALID_ARGUMENT otherwise. Once the schema is deleted the
//     topic names "_deleted-schema_".
// The emulator would refuse a topic bound to a schema it does not hold, so
// the front creates the topic without the binding, keeps the binding, names
// it on every topic read, and checks every Publish to the topic. An
// UpdateTopic of schema_settings that names a front-kept schema, or on a
// topic bound to one, is refused UNIMPLEMENTED with the reason.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// schemaIDRule is Pub/Sub's rule for a resource ID, a schema's as a
// topic's: 3-255 characters, starting with a letter.
var schemaIDRule = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9._~%+\-]{2,254}$`)

// deletedSchemaName is the schema Pub/Sub names for a topic whose schema
// was deleted.
const deletedSchemaName = "_deleted-schema_"

// errInvalidMessage is Google's refusal of a published message that does
// not conform to its topic's schema.
func errInvalidMessage() error {
	return apierror.InvalidArgument("Invalid data in message: Message failed schema validation.")
}

// newRevisionID is a new revision's ID: eight hexadecimal digits, as
// Google's are.
func newRevisionID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// protoSchema is one front-kept schema: its revisions, oldest first.
type protoSchema struct {
	revisions []*pubsubpb.Schema
}

// splitRevision is a schema name and the revision after an "@", if any.
func splitRevision(name string) (string, string) {
	n, rev, _ := strings.Cut(name, "@")
	return n, rev
}

// keptSchema is the front's schema name, a copy of its revisions, and
// whether it keeps it.
func (f *Front) keptSchema(name string) ([]*pubsubpb.Schema, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.protoSchemas[name]
	if !ok {
		return nil, false
	}
	out := make([]*pubsubpb.Schema, len(s.revisions))
	for i, r := range s.revisions {
		out[i] = proto.Clone(r).(*pubsubpb.Schema)
	}
	return out, true
}

// revision is the named revision of a kept schema, or its latest.
func revision(revs []*pubsubpb.Schema, id string) (*pubsubpb.Schema, bool) {
	if id == "" {
		return revs[len(revs)-1], true
	}
	for _, r := range revs {
		if r.GetRevisionId() == id {
			return r, true
		}
	}
	return nil, false
}

// basic is a schema as the BASIC view shows it.
func basic(s *pubsubpb.Schema) *pubsubpb.Schema {
	return &pubsubpb.Schema{Name: s.GetName(), Type: s.GetType()}
}

// errBadDefinition is the refusal of a definition the front cannot parse,
// worded as the emulator words an AVRO one.
func errBadDefinition(err error) error {
	return apierror.InvalidArgument("Could not parse schema definition: %v", err)
}

// schemaNotFound is the refusal of a schema that does not exist.
func schemaNotFound(name string) error {
	return apierror.NotFound("Resource not found (resource=%s).", name)
}

// isProtobuf reports whether a request's schema is a Protocol Buffer one.
func isProtobuf(s *pubsubpb.Schema) bool { return s.GetType() == pubsubpb.Schema_PROTOCOL_BUFFER }

// schemaCall answers a SchemaService call when it is the front's to answer:
// a PROTOCOL_BUFFER definition, or a schema the front keeps. handled is false
// for every other call, which goes to the emulator as it came. method is the
// RPC's name, req its request, decoded.
func (f *Front) schemaCall(ctx context.Context, method string, req proto.Message) (resp proto.Message, handled bool, err error) {
	switch r := req.(type) {
	case *pubsubpb.ValidateSchemaRequest:
		if !isProtobuf(r.GetSchema()) {
			return nil, false, nil
		}
		if _, err := parseProtoSchema(r.GetSchema().GetDefinition()); err != nil {
			return nil, true, errBadDefinition(err)
		}
		return &pubsubpb.ValidateSchemaResponse{}, true, nil
	case *pubsubpb.CreateSchemaRequest:
		if !isProtobuf(r.GetSchema()) {
			return nil, false, nil
		}
		s, err := f.createProtoSchema(ctx, r)
		return s, true, err
	case *pubsubpb.GetSchemaRequest:
		name, rev := splitRevision(r.GetName())
		revs, ok := f.keptSchema(name)
		if !ok {
			return nil, false, nil
		}
		s, ok := revision(revs, rev)
		if !ok {
			return nil, true, schemaNotFound(r.GetName())
		}
		if r.GetView() == pubsubpb.SchemaView_BASIC {
			return basic(s), true, nil
		}
		return s, true, nil
	case *pubsubpb.ListSchemasRequest:
		if !f.keepsSchemasIn(r.GetParent()) {
			return nil, false, nil
		}
		l, err := pubsubpb.NewSchemaServiceClient(f.upstream).ListSchemas(ctx, r)
		if err != nil {
			return nil, true, err
		}
		if l.GetNextPageToken() == "" {
			for _, s := range f.schemasIn(r.GetParent()) {
				if r.GetView() != pubsubpb.SchemaView_FULL {
					s = basic(s)
				}
				l.Schemas = append(l.Schemas, s)
			}
		}
		return l, true, nil
	case *pubsubpb.ListSchemaRevisionsRequest:
		revs, ok := f.keptSchema(r.GetName())
		if !ok {
			return nil, false, nil
		}
		l := &pubsubpb.ListSchemaRevisionsResponse{}
		for i := len(revs) - 1; i >= 0; i-- {
			s := revs[i]
			if r.GetView() != pubsubpb.SchemaView_FULL {
				s = &pubsubpb.Schema{Name: s.GetName(), Type: s.GetType(), RevisionId: s.GetRevisionId(),
					RevisionCreateTime: s.GetRevisionCreateTime()}
			}
			l.Schemas = append(l.Schemas, s)
		}
		return l, true, nil
	case *pubsubpb.CommitSchemaRequest:
		revs, ok := f.keptSchema(r.GetName())
		if !ok {
			return nil, false, nil
		}
		if !isProtobuf(r.GetSchema()) {
			return nil, true, apierror.InvalidArgument("The schema type cannot change: %s is a PROTOCOL_BUFFER schema", r.GetName())
		}
		if _, err := parseProtoSchema(r.GetSchema().GetDefinition()); err != nil {
			return nil, true, errBadDefinition(err)
		}
		s, err := f.addRevision(r.GetName(), revs, r.GetSchema().GetDefinition())
		return s, true, err
	case *pubsubpb.RollbackSchemaRequest:
		revs, ok := f.keptSchema(r.GetName())
		if !ok {
			return nil, false, nil
		}
		if r.GetRevisionId() == "" {
			return nil, true, apierror.InvalidArgument("revision_id is required")
		}
		old, ok := revision(revs, r.GetRevisionId())
		if !ok {
			return nil, true, schemaNotFound(r.GetName() + "@" + r.GetRevisionId())
		}
		s, err := f.addRevision(r.GetName(), revs, old.GetDefinition())
		return s, true, err
	case *pubsubpb.DeleteSchemaRevisionRequest:
		name, rev := splitRevision(r.GetName())
		if rev == "" {
			rev = r.GetRevisionId() //nolint:staticcheck // the deprecated field is still sent by older clients
		}
		revs, ok := f.keptSchema(name)
		if !ok {
			return nil, false, nil
		}
		s, err := f.deleteRevision(name, revs, rev)
		return s, true, err
	case *pubsubpb.DeleteSchemaRequest:
		if _, ok := f.keptSchema(r.GetName()); !ok {
			return nil, false, nil
		}
		f.mu.Lock()
		delete(f.protoSchemas, r.GetName())
		for topic, ss := range f.topicSchemas {
			if ss.GetSchema() == r.GetName() {
				ss.Schema = deletedSchemaName
				f.topicSchemas[topic] = ss
			}
		}
		f.mu.Unlock()
		f.forget(r.GetName())
		f.persist()
		return &emptypb.Empty{}, true, nil
	case *pubsubpb.ValidateMessageRequest:
		var def string
		switch spec := r.GetSchemaSpec().(type) {
		case *pubsubpb.ValidateMessageRequest_Schema:
			if !isProtobuf(spec.Schema) {
				return nil, false, nil
			}
			def = spec.Schema.GetDefinition()
		case *pubsubpb.ValidateMessageRequest_Name:
			name, rev := splitRevision(spec.Name)
			revs, ok := f.keptSchema(name)
			if !ok {
				return nil, false, nil
			}
			s, ok := revision(revs, rev)
			if !ok {
				return nil, true, schemaNotFound(spec.Name)
			}
			def = s.GetDefinition()
		default:
			return nil, false, nil
		}
		md, err := parseProtoSchema(def)
		if err != nil {
			return nil, true, errBadDefinition(err)
		}
		if err := validateProtoMessage(md, r.GetMessage(), r.GetEncoding()); err != nil {
			return nil, true, apierror.InvalidArgument("Invalid data in message: %v", err)
		}
		return &pubsubpb.ValidateMessageResponse{}, true, nil
	}
	return nil, false, nil
}

// createProtoSchema creates a PROTOCOL_BUFFER schema the front keeps.
func (f *Front) createProtoSchema(ctx context.Context, r *pubsubpb.CreateSchemaRequest) (*pubsubpb.Schema, error) {
	id := r.GetSchemaId()
	if id == "" {
		id = strings.TrimPrefix(r.GetSchema().GetName(), r.GetParent()+"/schemas/")
	}
	if !schemaIDRule.MatchString(id) {
		return nil, apierror.InvalidArgument("Invalid resource name given (name=%s/schemas/%s).", r.GetParent(), id)
	}
	if _, err := parseProtoSchema(r.GetSchema().GetDefinition()); err != nil {
		return nil, errBadDefinition(err)
	}
	name := r.GetParent() + "/schemas/" + id
	if _, ok := f.keptSchema(name); ok {
		return nil, apierror.AlreadyExists("Resource already exists in the project (resource=%s).", name)
	}
	if _, err := pubsubpb.NewSchemaServiceClient(f.upstream).GetSchema(ctx,
		&pubsubpb.GetSchemaRequest{Name: name, View: pubsubpb.SchemaView_BASIC}); err == nil {
		return nil, apierror.AlreadyExists("Resource already exists in the project (resource=%s).", name)
	} else if status.Code(err) != codes.NotFound {
		return nil, err
	}
	s := &pubsubpb.Schema{Name: name, Type: pubsubpb.Schema_PROTOCOL_BUFFER, Definition: r.GetSchema().GetDefinition(),
		RevisionId: newRevisionID(), RevisionCreateTime: timestamppb.New(f.clock.Now())}
	f.mu.Lock()
	f.protoSchemas[name] = &protoSchema{revisions: []*pubsubpb.Schema{s}}
	f.mu.Unlock()
	f.dropIAM(name)
	f.persist()
	return proto.Clone(s).(*pubsubpb.Schema), nil
}

// addRevision commits a new revision of a kept schema.
func (f *Front) addRevision(name string, revs []*pubsubpb.Schema, def string) (*pubsubpb.Schema, error) {
	s := &pubsubpb.Schema{Name: name, Type: pubsubpb.Schema_PROTOCOL_BUFFER, Definition: def,
		RevisionId: newRevisionID(), RevisionCreateTime: timestamppb.New(f.clock.Now())}
	f.mu.Lock()
	cur, ok := f.protoSchemas[name]
	if !ok {
		f.mu.Unlock()
		return nil, schemaNotFound(name)
	}
	cur.revisions = append(cur.revisions, s)
	f.mu.Unlock()
	f.persist()
	return proto.Clone(s).(*pubsubpb.Schema), nil
}

// deleteRevision deletes one revision of a kept schema, never its last.
func (f *Front) deleteRevision(name string, revs []*pubsubpb.Schema, rev string) (*pubsubpb.Schema, error) {
	if rev == "" {
		return nil, apierror.InvalidArgument("The revision to delete must be named, as name@revision_id")
	}
	s, ok := revision(revs, rev)
	if !ok {
		return nil, schemaNotFound(name + "@" + rev)
	}
	if len(revs) == 1 {
		return nil, apierror.InvalidArgument("Cannot delete last revision. Please use DeleteSchema.")
	}
	f.mu.Lock()
	if cur, ok := f.protoSchemas[name]; ok {
		kept := cur.revisions[:0]
		for _, r := range cur.revisions {
			if r.GetRevisionId() != rev {
				kept = append(kept, r)
			}
		}
		cur.revisions = kept
	}
	f.mu.Unlock()
	f.persist()
	return s, nil
}

// keepsSchemasIn reports whether the front keeps a schema in parent.
func (f *Front) keepsSchemasIn(parent string) bool { return len(f.schemasIn(parent)) > 0 }

// schemasIn is the latest revision of each kept schema in parent, by name.
func (f *Front) schemasIn(parent string) []*pubsubpb.Schema {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*pubsubpb.Schema
	for name, s := range f.protoSchemas {
		if strings.HasPrefix(name, parent+"/schemas/") {
			out = append(out, proto.Clone(s.revisions[len(s.revisions)-1]).(*pubsubpb.Schema))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GetName() < out[j].GetName() })
	return out
}

// schemaRequest is a new, empty request of a SchemaService method.
func schemaRequest(method string) proto.Message {
	switch method {
	case "ValidateSchema":
		return &pubsubpb.ValidateSchemaRequest{}
	case "CreateSchema":
		return &pubsubpb.CreateSchemaRequest{}
	case "GetSchema":
		return &pubsubpb.GetSchemaRequest{}
	case "ListSchemas":
		return &pubsubpb.ListSchemasRequest{}
	case "ListSchemaRevisions":
		return &pubsubpb.ListSchemaRevisionsRequest{}
	case "CommitSchema":
		return &pubsubpb.CommitSchemaRequest{}
	case "RollbackSchema":
		return &pubsubpb.RollbackSchemaRequest{}
	case "DeleteSchemaRevision":
		return &pubsubpb.DeleteSchemaRevisionRequest{}
	case "DeleteSchema":
		return &pubsubpb.DeleteSchemaRequest{}
	case "ValidateMessage":
		return &pubsubpb.ValidateMessageRequest{}
	}
	return nil
}

// handleSchema serves a SchemaService call over gRPC: the front answers it
// when schemaCall does, and forwards it otherwise, its first request read
// already (replayStream).
func (f *Front) handleSchema(ctx context.Context, ss grpc.ServerStream, method string) (grpc.ServerStream, bool, error) {
	req := schemaRequest(method[len(schemaService):])
	if req == nil {
		return ss, false, nil
	}
	var in frame
	if err := ss.RecvMsg(&in); err != nil {
		return ss, true, err
	}
	replay := &replayStream{ServerStream: ss, first: in}
	if proto.Unmarshal(in, req) != nil {
		return replay, false, nil // the emulator's refusal is the answer
	}
	resp, handled, err := f.schemaCall(ctx, method[len(schemaService):], req)
	if !handled {
		return replay, false, nil
	}
	f.sawProject(in)
	if err != nil {
		return ss, true, err
	}
	return ss, true, sendProto(ss, resp)
}

// topicSchema is the binding the front keeps for topic, and whether it
// keeps one.
func (f *Front) topicSchema(topic string) (*pubsubpb.SchemaSettings, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.topicSchemas[topic]
	if !ok {
		return nil, false
	}
	return proto.Clone(s).(*pubsubpb.SchemaSettings), true
}

// withTopicSchema names the binding the front keeps for a topic on it; it
// reports whether it changed anything.
func (f *Front) withTopicSchema(t *pubsubpb.Topic) bool {
	if t == nil {
		return false
	}
	s, ok := f.topicSchema(t.GetName())
	if !ok || proto.Equal(s, t.GetSchemaSettings()) {
		return false
	}
	t.SchemaSettings = s
	return true
}

// takeTopicSchema checks a new topic's binding to a kept schema and takes
// it out of the topic the emulator sees; it returns the binding, nil when
// the topic names no kept schema.
func (f *Front) takeTopicSchema(t *pubsubpb.Topic) (*pubsubpb.SchemaSettings, error) {
	ss := t.GetSchemaSettings()
	if ss == nil {
		return nil, nil
	}
	name, _ := splitRevision(ss.GetSchema())
	revs, ok := f.keptSchema(name)
	if !ok {
		return nil, nil
	}
	if ss.GetEncoding() == pubsubpb.Encoding_ENCODING_UNSPECIFIED {
		return nil, apierror.InvalidArgument("schema_settings.encoding must be JSON or BINARY")
	}
	for _, id := range []string{ss.GetFirstRevisionId(), ss.GetLastRevisionId()} {
		if _, ok := revision(revs, id); !ok {
			return nil, schemaNotFound(name + "@" + id)
		}
	}
	out := proto.Clone(ss).(*pubsubpb.SchemaSettings)
	out.Schema = name
	t.SchemaSettings = nil
	return out, nil
}

// setTopicSchema keeps a topic's binding to a kept schema.
func (f *Front) setTopicSchema(topic string, ss *pubsubpb.SchemaSettings) {
	f.mu.Lock()
	f.topicSchemas[topic] = proto.Clone(ss).(*pubsubpb.SchemaSettings)
	f.mu.Unlock()
	f.persist()
}

// dropTopicSchema forgets a topic's binding.
func (f *Front) dropTopicSchema(topic string) {
	f.mu.Lock()
	_, ok := f.topicSchemas[topic]
	delete(f.topicSchemas, topic)
	f.mu.Unlock()
	if ok {
		f.persist()
	}
}

// checkPublish refuses messages that do not conform to their topic's
// front-kept schema: each must parse, in the binding's encoding, as one of
// the revisions from first_revision_id to last_revision_id.
func (f *Front) checkPublish(topic string, msgs []*pubsubpb.PubsubMessage) error {
	ss, ok := f.topicSchema(topic)
	if !ok {
		return nil
	}
	if ss.GetSchema() == deletedSchemaName {
		return apierror.InvalidArgument("Invalid data in message: the topic's schema has been deleted.")
	}
	revs, ok := f.keptSchema(ss.GetSchema())
	if !ok {
		return nil
	}
	var mds []protoreflect.MessageDescriptor
	in := ss.GetFirstRevisionId() == ""
	for _, r := range revs {
		if r.GetRevisionId() == ss.GetFirstRevisionId() {
			in = true
		}
		if in {
			if md, err := parseProtoSchema(r.GetDefinition()); err == nil {
				mds = append(mds, md)
			}
		}
		if r.GetRevisionId() == ss.GetLastRevisionId() {
			break
		}
	}
	for _, m := range msgs {
		ok := false
		for _, md := range mds {
			if validateProtoMessage(md, m.GetData(), ss.GetEncoding()) == nil {
				ok = true
				break
			}
		}
		if !ok {
			return errInvalidMessage()
		}
	}
	return nil
}

// topicSchemaUpdate is what an UpdateTopic of schema_settings does to a
// topic's binding to a front-kept schema (#1446): set is the binding it
// then has, nil when it has none the front keeps.
type topicSchemaUpdate struct {
	topic string
	set   *pubsubpb.SchemaSettings
}

// planTopicSchemaUpdate is what an UpdateTopic of schema_settings does to
// the front's bindings, nil when it names no front-kept schema and is on no
// topic bound to one, which the emulator then applies alone. A binding to a
// kept schema is checked as a CreateTopic's is and taken out of r, so the
// emulator clears the topic's own binding; a binding to any other schema,
// or none, is the emulator's to apply, and the front's is dropped. Google
// documents schema_settings as updatable, and an empty schema_settings as
// removing the binding ("gcloud pubsub topics update --clear-schema-settings").
func (f *Front) planTopicSchemaUpdate(r *pubsubpb.UpdateTopicRequest) (*topicSchemaUpdate, error) {
	if !masks(r.GetUpdateMask().GetPaths(), "schema_settings") {
		return nil, nil
	}
	t := r.GetTopic()
	name, _ := splitRevision(t.GetSchemaSettings().GetSchema())
	_, kept := f.keptSchema(name)
	_, bound := f.topicSchema(t.GetName())
	if !kept && !bound {
		return nil, nil
	}
	plan := &topicSchemaUpdate{topic: t.GetName()}
	if kept {
		ss, err := f.takeTopicSchema(t)
		if err != nil {
			return nil, err
		}
		plan.set = ss
	}
	return plan, nil
}

// apply keeps the binding an UpdateTopic set, and returns the one it
// replaced, for undo.
func (u *topicSchemaUpdate) apply(f *Front) (undo func()) {
	prev, had := f.topicSchema(u.topic)
	if u.set != nil {
		f.setTopicSchema(u.topic, u.set)
	} else {
		f.dropTopicSchema(u.topic)
	}
	return func() {
		if had {
			f.setTopicSchema(u.topic, prev)
		} else {
			f.dropTopicSchema(u.topic)
		}
	}
}

// schemaPath is a REST path under /v1/projects/{p}/schemas: the project's
// parent, the schema's name ("" for the collection) and the custom verb.
func schemaPath(path string) (parent, name, verb string, ok bool) {
	rest, found := strings.CutPrefix(path, "/v1/projects/")
	if !found {
		return "", "", "", false
	}
	parts := strings.Split(rest, "/")
	if len(parts) < 2 || parts[0] == "" {
		return "", "", "", false
	}
	parent = "projects/" + parts[0]
	switch {
	case len(parts) == 2:
		c, v, _ := strings.Cut(parts[1], ":")
		if c != "schemas" {
			return "", "", "", false
		}
		return parent, "", v, true
	case len(parts) == 3 && parts[1] == "schemas" && parts[2] != "":
		id, v, _ := strings.Cut(parts[2], ":")
		return parent, parent + "/schemas/" + id, v, true
	}
	return "", "", "", false
}

// restSchemaRequest is the SchemaService request a REST call makes, and its
// method, by Pub/Sub's HTTP rules; nil when it is none.
func restSchemaRequest(r *http.Request, parent, name, verb string, body []byte) (string, proto.Message, error) {
	q := r.URL.Query()
	view := func() pubsubpb.SchemaView { return pubsubpb.SchemaView(pubsubpb.SchemaView_value[q.Get("view")]) }
	decode := func(m proto.Message) error {
		if len(strings.TrimSpace(string(body))) == 0 {
			return nil
		}
		if err := restJSON.Unmarshal(body, m); err != nil {
			return apierror.InvalidArgument("the body is not a %s: %v", m.ProtoReflect().Descriptor().Name(), err)
		}
		return nil
	}
	pageSize := func() int32 { n, _ := strconv.ParseInt(q.Get("pageSize"), 10, 32); return int32(n) }
	switch {
	case name == "" && verb == "validate" && r.Method == http.MethodPost:
		m := &pubsubpb.ValidateSchemaRequest{}
		err := decode(m)
		m.Parent = parent
		return "ValidateSchema", m, err
	case name == "" && verb == "validateMessage" && r.Method == http.MethodPost:
		m := &pubsubpb.ValidateMessageRequest{}
		err := decode(m)
		m.Parent = parent
		return "ValidateMessage", m, err
	case name == "" && verb == "" && r.Method == http.MethodPost:
		s := &pubsubpb.Schema{}
		err := decode(s)
		return "CreateSchema", &pubsubpb.CreateSchemaRequest{Parent: parent, SchemaId: q.Get("schemaId"), Schema: s}, err
	case name == "" && verb == "" && r.Method == http.MethodGet:
		return "ListSchemas", &pubsubpb.ListSchemasRequest{Parent: parent, View: view(), PageSize: pageSize(), PageToken: q.Get("pageToken")}, nil
	case name != "" && verb == "" && r.Method == http.MethodGet:
		return "GetSchema", &pubsubpb.GetSchemaRequest{Name: name, View: view()}, nil
	case name != "" && verb == "" && r.Method == http.MethodDelete:
		return "DeleteSchema", &pubsubpb.DeleteSchemaRequest{Name: name}, nil
	case name != "" && verb == "listRevisions" && r.Method == http.MethodGet:
		return "ListSchemaRevisions", &pubsubpb.ListSchemaRevisionsRequest{Name: name, View: view(), PageSize: pageSize(), PageToken: q.Get("pageToken")}, nil
	case name != "" && verb == "commit" && r.Method == http.MethodPost:
		m := &pubsubpb.CommitSchemaRequest{}
		err := decode(m)
		m.Name = name
		return "CommitSchema", m, err
	case name != "" && verb == "rollback" && r.Method == http.MethodPost:
		m := &pubsubpb.RollbackSchemaRequest{}
		err := decode(m)
		m.Name = name
		return "RollbackSchema", m, err
	case name != "" && verb == "deleteRevision" && r.Method == http.MethodDelete:
		return "DeleteSchemaRevision", &pubsubpb.DeleteSchemaRevisionRequest{Name: name, RevisionId: q.Get("revisionId")}, nil
	}
	return "", nil, nil
}

// serveRESTSchema answers a REST SchemaService call when it is the front's
// to answer, and reports whether it did.
func (f *Front) serveRESTSchema(w http.ResponseWriter, r *http.Request, parent, name, verb string) bool {
	var body []byte
	if r.Method == http.MethodPost {
		b, err := readBody(r)
		if err != nil {
			writeRESTError(w, err)
			return true
		}
		body = b
	}
	method, req, err := restSchemaRequest(r, parent, name, verb, body)
	if req == nil || err != nil {
		return false // the emulator answers it, or refuses it
	}
	resp, handled, err := f.schemaCall(r.Context(), method, req)
	if !handled {
		return false
	}
	if err != nil {
		writeRESTError(w, err)
		return true
	}
	if _, empty := resp.(*emptypb.Empty); empty {
		w.Header().Set("Content-Type", "application/json; charset=UTF-8")
		_, _ = w.Write([]byte("{}"))
		return true
	}
	writeProtoJSON(w, resp)
	return true
}

// keptSchemasJSON is the front's schemas as the state file holds them:
// each schema's revisions, oldest first, as protojson.
func keptSchemasJSON(m map[string]*protoSchema) (map[string][]string, error) {
	if len(m) == 0 {
		return nil, nil
	}
	out := make(map[string][]string, len(m))
	for name, s := range m {
		for _, r := range s.revisions {
			b, err := protojson.Marshal(r)
			if err != nil {
				return nil, err
			}
			out[name] = append(out[name], string(b))
		}
	}
	return out, nil
}

// editObjectBody is a JSON object's body with edit applied.
func editObjectBody(body []byte, edit func(jsonObject)) ([]byte, error) {
	var o jsonObject
	if err := json.Unmarshal(body, &o); err != nil || o == nil {
		return body, err
	}
	edit(o)
	return encodeJSON(o)
}

// withTopicJSON is a Topic's JSON with the labels (#949) and the schema
// binding the front keeps for it, and whether it changed.
func (f *Front) withTopicJSON(res json.RawMessage) (json.RawMessage, bool) {
	out, labelled := f.withLabelsJSON(res)
	var o jsonObject
	if json.Unmarshal(out, &o) != nil || o == nil {
		return out, labelled
	}
	var name string
	if raw, key := o.field("name"); key == "" || json.Unmarshal(raw, &name) != nil {
		return out, labelled
	}
	ss, ok := f.topicSchema(name)
	if !ok {
		return out, labelled
	}
	b, err := protojson.Marshal(ss)
	if err != nil {
		return out, labelled
	}
	delete(o, "schema_settings")
	o["schemaSettings"] = b
	enc, err := encodeJSON(o)
	if err != nil {
		return out, labelled
	}
	return enc, true
}
