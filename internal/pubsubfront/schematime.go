package pubsubfront

// Schema revisions' creation times (#1874): the emulator answers every
// Avro schema revision with a revision_create_time hours after the Unix
// epoch (measured: 1970-01-01T08:06:21Z), which the console showed as
// 1970-01-01. A time before 2001 is taken as unset. Google gives "The timestamp that the
// revision was created" (google/pubsub/v1/schema.proto). The front gives
// each revision it first sees with an unset or epoch time the time it saw
// it, on its own clock, and answers that time from then on.

import (
	"strings"

	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// unsetBefore is 2001-01-01T00:00:00Z: an emulator time before it is no
// creation time.
const unsetBefore = 978307200

// withRevisionTime sets a schema revision's creation time if the emulator
// left it unset; it reports whether it changed it.
func (f *Front) withRevisionTime(s *pubsubpb.Schema) bool {
	if s == nil || s.GetName() == "" {
		return false
	}
	if t := s.GetRevisionCreateTime(); t != nil && t.GetSeconds() >= unsetBefore {
		return false
	}
	key := s.GetName() + "@" + s.GetRevisionId()
	f.mu.Lock()
	if f.revisionTimes == nil {
		f.revisionTimes = map[string]*timestamppb.Timestamp{}
	}
	t, ok := f.revisionTimes[key]
	if !ok {
		t = timestamppb.New(f.clock.Now())
		f.revisionTimes[key] = t
	}
	f.mu.Unlock()
	s.RevisionCreateTime = proto.Clone(t).(*timestamppb.Timestamp)
	return true
}

// schemaResponse rewrites the emulator's SchemaService answers.
func (f *Front) schemaResponse(method string, fr frame) frame {
	var m proto.Message
	var schemas func() []*pubsubpb.Schema
	switch strings.TrimPrefix(method, schemaService) {
	case "CreateSchema", "GetSchema", "CommitSchema", "RollbackSchema", "DeleteSchemaRevision":
		s := &pubsubpb.Schema{}
		m, schemas = s, func() []*pubsubpb.Schema { return []*pubsubpb.Schema{s} }
	case "ListSchemas":
		l := &pubsubpb.ListSchemasResponse{}
		m, schemas = l, l.GetSchemas
	case "ListSchemaRevisions":
		l := &pubsubpb.ListSchemaRevisionsResponse{}
		m, schemas = l, l.GetSchemas
	default:
		return fr
	}
	if proto.Unmarshal(fr, m) != nil {
		return fr
	}
	changed := false
	for _, s := range schemas() {
		changed = f.withRevisionTime(s) || changed
	}
	if !changed {
		return fr
	}
	if b, err := proto.Marshal(m); err == nil {
		return b
	}
	return fr
}
