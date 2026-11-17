package firestoreadmin

// Enterprise-only values on Standard databases (#1703).
//
// The Firestore emulator runs as an Enterprise edition emulator
// (--database-edition=enterprise), so that Firestore with MongoDB
// compatibility can keep the BSON types Google maps to Firestore values.
// An emulator of that edition accepts them in every database, where Google
// accepts them in Enterprise edition databases alone: "Supported data
// types" marks 32-bit integer, BSON timestamp, bytes with subtype,
// Decimal128, max key, min key, ObjectId and regular expression
// "Supported only in Enterprise edition", and says "In Standard edition
// databases, an array cannot contain another array value as one of its
// elements" (https://docs.cloud.google.com/firestore/docs/concepts/data-types).
//
// The guard below is an intercept that claims a write (Commit, BatchWrite,
// CreateDocument, UpdateDocument, over gRPC or REST) naming a Standard
// database when it carries such a value, a field's or a transform's
// operand, and answers it as the same emulator does in Standard mode; every
// other call goes on to the emulator untouched. Google's page names no
// error, so the Standard emulator's is the closest evidence (UNVERIFIED
// against Google; docs re-read 2026-11-17, still unanswered:
// https://docs.cloud.google.com/firestore/docs/concepts/data-types): INVALID_ARGUMENT, "Value type INT is not
// allowed" (OBJECT_ID, DECIMAL128, REQUEST_TIMESTAMP, REGEX, MIN_KEY,
// MAX_KEY), and "Nested arrays are not allowed". Two cases differ from
// that emulator, which follows the page less closely: it accepts bytes
// with a subtype and a transform's operands on a Standard database, which
// the page says Standard does not have, so they are refused here, bytes
// with a subtype as BYTES_WITH_SUBTYPE; and a BSON value inside a map is
// refused with its value type rather than its "Property v contains an
// invalid nested entity". A malformed one (an __int__ that is not an
// integer, a __binary__ of subtype 0) is left to the emulator, whose
// refusal is the same in both editions. Queries are not refused: the
// Standard emulator answers a filter on such a value, matching nothing.
// The Write stream (the Firebase client SDKs' channel) is checked request
// by request, over gRPC (editionstream.go, #1738) and over the browser's
// WebChannel transport (editionchannel.go, #1744).

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"io"
	"net/http"
	"regexp"
	"strings"

	adminpb "cloud.google.com/go/firestore/apiv1/admin/adminpb"
	firestorepb "cloud.google.com/go/firestore/apiv1/firestorepb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// enterpriseOnly maps the key of each one-key map Google's clients write a
// BSON value as to the value type the refusal names, and the shape the
// value under it has when well formed.
var enterpriseOnly = map[string]struct {
	name string
	ok   func(*firestorepb.Value) bool
}{
	"__int__": {"INT", func(v *firestorepb.Value) bool {
		i, ok := v.GetValueType().(*firestorepb.Value_IntegerValue)
		return ok && i.IntegerValue >= -1<<31 && i.IntegerValue < 1<<31
	}},
	"__oid__":               {"OBJECT_ID", isString},
	"__decimal128__":        {"DECIMAL128", isString},
	"__request_timestamp__": {"REQUEST_TIMESTAMP", isMap},
	"__regex__":             {"REGEX", isMap},
	"__min__":               {"MIN_KEY", isNull},
	"__max__":               {"MAX_KEY", isNull},
	"__binary__": {"BYTES_WITH_SUBTYPE", func(v *firestorepb.Value) bool {
		b, ok := v.GetValueType().(*firestorepb.Value_BytesValue)
		return ok && len(b.BytesValue) > 0 && b.BytesValue[0] != 0
	}},
}

func isString(v *firestorepb.Value) bool {
	_, ok := v.GetValueType().(*firestorepb.Value_StringValue)
	return ok
}

func isMap(v *firestorepb.Value) bool {
	_, ok := v.GetValueType().(*firestorepb.Value_MapValue)
	return ok
}

// enterpriseValue is the refusal for v on a Standard database, or nil.
// inArray is whether v is an element of an array.
func enterpriseValue(v *firestorepb.Value, inArray bool) error {
	switch x := v.GetValueType().(type) {
	case *firestorepb.Value_ArrayValue:
		if inArray {
			return status.Error(codes.InvalidArgument, "Nested arrays are not allowed")
		}
	case *firestorepb.Value_MapValue:
		if f := x.MapValue.GetFields(); len(f) == 1 {
			for k, inner := range f {
				if t, ok := enterpriseOnly[k]; ok && t.ok(inner) {
					return status.Errorf(codes.InvalidArgument, "Value type %s is not allowed", t.name)
				}
			}
		}
	}
	return nil
}

// checkStandardValues walks every Value in m, an array's elements told
// apart from other values.
func checkStandardValues(m protoreflect.Message) error {
	return walkMessage(m, false)
}

var (
	valueName      = (&firestorepb.Value{}).ProtoReflect().Descriptor().FullName()
	arrayValueName = (&firestorepb.ArrayValue{}).ProtoReflect().Descriptor().FullName()
)

// walkMessage checks every Value in m; inArray is whether m is an array's
// element. An ArrayValue's elements are, whether it is a Value's array or
// an arrayUnion's or arrayRemove's operand.
func walkMessage(m protoreflect.Message, inArray bool) error {
	switch m.Descriptor().FullName() {
	case valueName:
		if v, ok := m.Interface().(*firestorepb.Value); ok {
			if err := enterpriseValue(v, inArray); err != nil {
				return err
			}
		}
	case arrayValueName:
		if a, ok := m.Interface().(*firestorepb.ArrayValue); ok {
			for _, e := range a.GetValues() {
				if err := walkMessage(e.ProtoReflect(), true); err != nil {
					return err
				}
			}
			return nil
		}
	}
	var err error
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		switch {
		case fd.IsMap():
			if fd.MapValue().Message() == nil {
				return true
			}
			v.Map().Range(func(_ protoreflect.MapKey, mv protoreflect.Value) bool {
				err = walkMessage(mv.Message(), false)
				return err == nil
			})
		case fd.Message() == nil:
		case fd.IsList():
			l := v.List()
			for i := 0; i < l.Len() && err == nil; i++ {
				err = walkMessage(l.Get(i).Message(), false)
			}
		default:
			err = walkMessage(v.Message(), false)
		}
		return err == nil
	})
	return err
}

// The writes, whose requests carry values.
const firestoreService = "/google.firestore.v1.Firestore/"

var guardedGRPC = map[string]func() proto.Message{
	"Commit":         func() proto.Message { return &firestorepb.CommitRequest{} },
	"BatchWrite":     func() proto.Message { return &firestorepb.BatchWriteRequest{} },
	"CreateDocument": func() proto.Message { return &firestorepb.CreateDocumentRequest{} },
	"UpdateDocument": func() proto.Message { return &firestorepb.UpdateDocumentRequest{} },
}

var restDocumentsPath = regexp.MustCompile(`^/v1/(projects/[^/]+/databases/[^/]+)/documents(/[^:]*)?(?::([A-Za-z]+))?$`)

// guardedREST is the request message of a REST documents call, or nil.
func guardedREST(r *http.Request) (proto.Message, string) {
	m := restDocumentsPath.FindStringSubmatch(r.URL.Path)
	if m == nil {
		return nil, ""
	}
	switch {
	case r.Method == http.MethodPost && m[3] == "commit" && m[2] == "":
		return &firestorepb.CommitRequest{}, m[1]
	case r.Method == http.MethodPost && m[3] == "batchWrite" && m[2] == "":
		return &firestorepb.BatchWriteRequest{}, m[1]
	case r.Method == http.MethodPost && m[3] == "" && m[2] != "":
		return &firestorepb.Document{}, m[1] // createDocument
	case r.Method == http.MethodPatch && m[3] == "" && m[2] != "":
		return &firestorepb.Document{}, m[1] // updateDocument
	}
	return nil, ""
}

// requestDatabase is the database a gRPC request names.
func requestDatabase(req proto.Message) string {
	var name string
	switch x := req.(type) {
	case *firestorepb.CommitRequest:
		name = x.GetDatabase()
	case *firestorepb.BatchWriteRequest:
		name = x.GetDatabase()
	case *firestorepb.CreateDocumentRequest:
		name = x.GetParent()
	case *firestorepb.UpdateDocumentRequest:
		name = x.GetDocument().GetName()
	}
	return databaseOf(name)
}

// maxGuardedBody is the most of a request the guard reads; a larger one
// goes to the emulator unread (Google's own request limit is 10 MiB).
const maxGuardedBody = 32 << 20

// EditionGuard refuses Enterprise-only values in a Standard database's
// documents calls. It is an intercept of the call front.
type EditionGuard struct {
	a        *Admin
	grpc     *grpc.Server
	channels channels // the WebChannel Write sessions (editionchannel.go)
}

// EditionGuard is the guard over a's databases.
func (a *Admin) EditionGuard() *EditionGuard {
	g := &EditionGuard{a: a, grpc: grpc.NewServer()}
	firestorepb.RegisterFirestoreServer(g.grpc, editionRefusals{g: g})
	return g
}

// standard reports whether the database is a Standard edition one the
// Admin API knows. One it does not know is the emulator's to answer.
func (g *EditionGuard) standard(name string) bool {
	if name == "" {
		return false
	}
	g.a.mu.Lock()
	defer g.a.mu.Unlock()
	db, err := g.a.database(name)
	return err == nil && db.GetDatabaseEdition() != adminpb.Database_ENTERPRISE
}

// refusal decodes r's request, putting its body back, and is the error a
// Standard database gives it, or nil.
func (g *EditionGuard) refusal(r *http.Request) error {
	var req proto.Message
	var db string
	grpcCall := strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc")
	if grpcCall {
		mk, ok := guardedGRPC[strings.TrimPrefix(r.URL.Path, firestoreService)]
		if !ok || !strings.HasPrefix(r.URL.Path, firestoreService) {
			return nil
		}
		req = mk()
	} else if req, db = guardedREST(r); req == nil {
		return nil
	}
	if r.Body == nil {
		return nil
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, maxGuardedBody+1))
	r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(b), r.Body))
	if err != nil || len(b) > maxGuardedBody {
		return nil
	}
	if grpcCall {
		msg, ok := grpcMessage(b, r.Header.Get("Grpc-Encoding"))
		if !ok || proto.Unmarshal(msg, req) != nil {
			return nil
		}
		db = requestDatabase(req)
	} else if len(b) == 0 || (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(b, req) != nil {
		return nil
	}
	if !g.standard(db) {
		return nil
	}
	return checkStandardValues(req.ProtoReflect())
}

// grpcMessage is the first message of a gRPC request body.
func grpcMessage(b []byte, encoding string) ([]byte, bool) {
	if len(b) < 5 {
		return nil, false
	}
	n := binary.BigEndian.Uint32(b[1:5])
	if uint64(len(b)-5) < uint64(n) {
		return nil, false
	}
	msg := b[5 : 5+n]
	if b[0] == 0 {
		return msg, true
	}
	if encoding != "gzip" {
		return nil, false
	}
	zr, err := gzip.NewReader(bytes.NewReader(msg))
	if err != nil {
		return nil, false
	}
	out, err := io.ReadAll(io.LimitReader(zr, maxGuardedBody))
	return out, err == nil
}

// Claims reports whether r is a documents call on a Standard database
// carrying an Enterprise-only value, or a Write stream, which the guard
// passes on itself: over gRPC (editionstream.go, #1738), and over
// WebChannel on a Standard database (editionchannel.go, #1744).
func (g *EditionGuard) Claims(r *http.Request) bool {
	return claimsWriteStream(r) || g.claimsWriteChannel(r) || g.refusal(r) != nil
}

// ServeHTTP answers a call Claims claimed with its refusal.
func (g *EditionGuard) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == writeChannelPath {
		g.serveWriteChannel(w, r)
		return
	}
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc") {
		g.grpc.ServeHTTP(w, r)
		return
	}
	err := g.refusal(r)
	if err == nil {
		err = status.Error(codes.Internal, "the call was claimed but not refused")
	}
	writeError(w, err)
}

// editionRefusals answers the gRPC calls the guard claimed.
type editionRefusals struct {
	firestorepb.UnimplementedFirestoreServer
	g *EditionGuard
}

func (e editionRefusals) refuse(req proto.Message) error {
	if !e.g.standard(requestDatabase(req)) {
		return status.Error(codes.Internal, "the call was claimed but not refused")
	}
	if err := checkStandardValues(req.ProtoReflect()); err != nil {
		return err
	}
	return status.Error(codes.Internal, "the call was claimed but not refused")
}

func (e editionRefusals) Commit(_ context.Context, r *firestorepb.CommitRequest) (*firestorepb.CommitResponse, error) {
	return nil, e.refuse(r)
}

func (e editionRefusals) BatchWrite(_ context.Context, r *firestorepb.BatchWriteRequest) (*firestorepb.BatchWriteResponse, error) {
	return nil, e.refuse(r)
}

func (e editionRefusals) CreateDocument(_ context.Context, r *firestorepb.CreateDocumentRequest) (*firestorepb.Document, error) {
	return nil, e.refuse(r)
}

func (e editionRefusals) UpdateDocument(_ context.Context, r *firestorepb.UpdateDocumentRequest) (*firestorepb.Document, error) {
	return nil, e.refuse(r)
}
