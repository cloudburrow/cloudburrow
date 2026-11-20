//go:build compat

package compat

import (
	"encoding/base64"
	"fmt"
	"strings"
	"testing"

	database "cloud.google.com/go/spanner/admin/database/apiv1"
	"cloud.google.com/go/spanner/admin/database/apiv1/databasepb"
	instance "cloud.google.com/go/spanner/admin/instance/apiv1"
	"cloud.google.com/go/spanner/admin/instance/apiv1/instancepb"
	sp "cloud.google.com/go/spanner/apiv1"
	"cloud.google.com/go/spanner/apiv1/spannerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/known/structpb"
)

// spannerMusicProtos is a FileDescriptorSet of one file, package cb.music,
// with a message SingerInfo and an enum Genre: the shape of Google's own
// example (protocol-buffers, "CREATE PROTO BUNDLE").
func spannerMusicProtos() *descriptorpb.FileDescriptorSet {
	return &descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{{
		Name:    proto.String("cb/music.proto"),
		Package: proto.String("cb.music"),
		Syntax:  proto.String("proto2"),
		MessageType: []*descriptorpb.DescriptorProto{{
			Name: proto.String("SingerInfo"),
			Field: []*descriptorpb.FieldDescriptorProto{
				{Name: proto.String("singer_id"), Number: proto.Int32(1), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum(), JsonName: proto.String("singerId")},
				{Name: proto.String("nationality"), Number: proto.Int32(2), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(), JsonName: proto.String("nationality")},
			},
		}},
		EnumType: []*descriptorpb.EnumDescriptorProto{{
			Name: proto.String("Genre"),
			Value: []*descriptorpb.EnumValueDescriptorProto{
				{Name: proto.String("POP"), Number: proto.Int32(0)},
				{Name: proto.String("JAZZ"), Number: proto.Int32(1)},
				{Name: proto.String("ROCK"), Number: proto.Int32(2)},
			},
		}},
	}}}
}

// TestSpannerProtoBundleColumns (#2017): a database made with a proto
// bundle (CreateDatabase's proto_descriptors and CREATE PROTO BUNDLE) keeps
// the descriptors, which GetDatabaseDdl returns with the bundle's DDL; a
// PROTO and an ENUM column take typed parameters (TypeCode PROTO and ENUM
// with proto_type_fqn) and return their values with the same types, and a
// message field reads by path.
//
// covers: google.spanner.admin.database.v1.DatabaseAdmin/CreateDatabase, google.spanner.admin.database.v1.DatabaseAdmin/GetDatabaseDdl, google.spanner.v1.Spanner/ExecuteSql
func TestSpannerProtoBundleColumns(t *testing.T) {
	h := New(t)
	addr := h.Endpoint(EnvSpanner)
	ctx := h.Context()
	opts := spannerOpts(addr)
	project := h.Project()

	instAdmin, err := instance.NewInstanceAdminClient(ctx, opts...)
	if err != nil {
		t.Fatalf("NewInstanceAdminClient: %v", err)
	}
	defer instAdmin.Close()
	op, err := instAdmin.CreateInstance(ctx, &instancepb.CreateInstanceRequest{
		Parent: "projects/" + project, InstanceId: "cb-proto",
		Instance: &instancepb.Instance{Config: fmt.Sprintf("projects/%s/instanceConfigs/emulator-config", project), DisplayName: "CloudBurrow", NodeCount: 1},
	})
	if err != nil && status.Code(err) != codes.AlreadyExists {
		t.Fatalf("CreateInstance: %v", err)
	}
	if err == nil {
		if _, err := op.Wait(ctx); err != nil {
			t.Fatalf("waiting for the instance: %v", err)
		}
	}
	dbAdmin, err := database.NewDatabaseAdminClient(ctx, opts...)
	if err != nil {
		t.Fatalf("NewDatabaseAdminClient: %v", err)
	}
	defer dbAdmin.Close()

	fds := spannerMusicProtos()
	raw, err := proto.Marshal(fds)
	if err != nil {
		t.Fatal(err)
	}
	dbName := fmt.Sprintf("projects/%s/instances/cb-proto/databases/protodb", project)
	_ = dbAdmin.DropDatabase(ctx, &databasepb.DropDatabaseRequest{Database: dbName})
	dbOp, err := dbAdmin.CreateDatabase(ctx, &databasepb.CreateDatabaseRequest{
		Parent:          fmt.Sprintf("projects/%s/instances/cb-proto", project),
		CreateStatement: "CREATE DATABASE protodb",
		ExtraStatements: []string{
			"CREATE PROTO BUNDLE (cb.music.SingerInfo, cb.music.Genre)",
			"CREATE TABLE Singers (Id INT64 NOT NULL, Info cb.music.SingerInfo, Genre cb.music.Genre) PRIMARY KEY (Id)",
		},
		ProtoDescriptors: raw,
	})
	if err != nil {
		t.Fatalf("CreateDatabase with a proto bundle: %v", err)
	}
	if _, err := dbOp.Wait(ctx); err != nil {
		t.Fatalf("CreateDatabase with a proto bundle: %v", err)
	}
	t.Cleanup(func() { _ = dbAdmin.DropDatabase(h.Context(), &databasepb.DropDatabaseRequest{Database: dbName}) })

	ddl, err := dbAdmin.GetDatabaseDdl(ctx, &databasepb.GetDatabaseDdlRequest{Database: dbName})
	if err != nil {
		t.Fatalf("GetDatabaseDdl: %v", err)
	}
	t.Logf("DDL: %q", ddl.GetStatements())
	var got descriptorpb.FileDescriptorSet
	if err := proto.Unmarshal(ddl.GetProtoDescriptors(), &got); err != nil {
		t.Fatalf("GetDatabaseDdl proto_descriptors do not parse: %v", err)
	}
	files, err := protodesc.NewFiles(&got)
	if err != nil {
		t.Fatalf("GetDatabaseDdl proto_descriptors: %v", err)
	}
	if _, err := files.FindDescriptorByName("cb.music.SingerInfo"); err != nil {
		t.Errorf("GetDatabaseDdl proto_descriptors have no cb.music.SingerInfo: %v", err)
	}
	bundle := false
	for _, s := range ddl.GetStatements() {
		if strings.HasPrefix(s, "CREATE PROTO BUNDLE") {
			bundle = true
		}
	}
	if !bundle {
		t.Errorf("GetDatabaseDdl statements have no CREATE PROTO BUNDLE: %q", ddl.GetStatements())
	}

	// A PROTO and an ENUM value bound as typed parameters.
	c, err := sp.NewClient(ctx, opts...)
	if err != nil {
		t.Fatalf("spanner apiv1 NewClient: %v", err)
	}
	defer c.Close()
	sess, err := c.CreateSession(ctx, &spannerpb.CreateSessionRequest{Database: dbName})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	infoBytes := []byte{0x08, 0x07, 0x12, 0x02, 'F', 'R'} // singer_id 7, nationality "FR"
	ins, err := c.ExecuteSql(ctx, &spannerpb.ExecuteSqlRequest{
		Session: sess.GetName(), Seqno: 1,
		Sql: "INSERT INTO Singers (Id, Info, Genre) VALUES (1, @info, @genre)",
		Params: &structpb.Struct{Fields: map[string]*structpb.Value{
			"info":  structpb.NewStringValue(b64(infoBytes)),
			"genre": structpb.NewStringValue("2"),
		}},
		ParamTypes: map[string]*spannerpb.Type{
			"info":  {Code: spannerpb.TypeCode_PROTO, ProtoTypeFqn: "cb.music.SingerInfo"},
			"genre": {Code: spannerpb.TypeCode_ENUM, ProtoTypeFqn: "cb.music.Genre"},
		},
		Transaction: &spannerpb.TransactionSelector{Selector: &spannerpb.TransactionSelector_Begin{Begin: spannerReadWrite()}},
	})
	if err != nil {
		t.Fatalf("INSERT with PROTO and ENUM parameters: %v", err)
	}
	if _, err := c.Commit(ctx, &spannerpb.CommitRequest{Session: sess.GetName(),
		Transaction: &spannerpb.CommitRequest_TransactionId{TransactionId: ins.GetMetadata().GetTransaction().GetId()}}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	rs, err := c.ExecuteSql(ctx, &spannerpb.ExecuteSqlRequest{Session: sess.GetName(),
		Sql: "SELECT Info, Genre, Info.nationality FROM Singers WHERE Id = 1"})
	if err != nil {
		t.Fatalf("SELECT PROTO and ENUM: %v", err)
	}
	fields := rs.GetMetadata().GetRowType().GetFields()
	if len(fields) != 3 || fields[0].GetType().GetCode() != spannerpb.TypeCode_PROTO || fields[0].GetType().GetProtoTypeFqn() != "cb.music.SingerInfo" ||
		fields[1].GetType().GetCode() != spannerpb.TypeCode_ENUM || fields[1].GetType().GetProtoTypeFqn() != "cb.music.Genre" {
		t.Fatalf("row type = %v, want PROTO<cb.music.SingerInfo>, ENUM<cb.music.Genre>, STRING", fields)
	}
	row := rs.GetRows()[0].GetValues()
	if got := row[0].GetStringValue(); got != b64(infoBytes) {
		t.Errorf("Info = %q, want %q", got, b64(infoBytes))
	}
	if got := row[1].GetStringValue(); got != "2" {
		t.Errorf("Genre = %q, want \"2\"", got)
	}
	if got := row[2].GetStringValue(); got != "FR" {
		t.Errorf("Info.nationality = %q, want FR", got)
	}
}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }
