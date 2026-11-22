package bigtablefront

import (
	"context"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigtable"
	btpb "cloud.google.com/go/bigtable/apiv2/bigtablepb"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

// musicFiles is "Query protobuf data"'s example schema: package_name's
// Artist and Album, with a Genre enum and repeated tags added. When drop
// is set, Album's release_year field is left out.
func musicFiles(t *testing.T, drop bool) (*descriptorpb.FileDescriptorSet, protoreflect.MessageDescriptor) {
	t.Helper()
	s := func(v string) *string { return &v }
	n := func(v int32) *int32 { return &v }
	opt := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum()
	album := &descriptorpb.DescriptorProto{Name: s("Album"), Field: []*descriptorpb.FieldDescriptorProto{
		{Name: s("title"), Number: n(1), Label: opt, Type: descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(), JsonName: s("title")},
		{Name: s("artist"), Number: n(2), Label: opt, Type: descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(), TypeName: s(".package_name.Artist"), JsonName: s("artist")},
		{Name: s("release_year"), Number: n(3), Label: opt, Type: descriptorpb.FieldDescriptorProto_TYPE_INT32.Enum(), JsonName: s("releaseYear")},
		{Name: s("genre"), Number: n(4), Label: opt, Type: descriptorpb.FieldDescriptorProto_TYPE_ENUM.Enum(), TypeName: s(".package_name.Genre"), JsonName: s("genre")},
		{Name: s("tags"), Number: n(5), Label: descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(), JsonName: s("tags")},
	}}
	if drop {
		album.Field = append(album.Field[:2:2], album.Field[3:]...)
	}
	artist := &descriptorpb.FileDescriptorProto{Name: s("artist.proto"), Package: s("package_name"), Syntax: s("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{{Name: s("Artist"), Field: []*descriptorpb.FieldDescriptorProto{
			{Name: s("name"), Number: n(1), Label: opt, Type: descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(), JsonName: s("name")}}}}}
	albumFile := &descriptorpb.FileDescriptorProto{Name: s("album.proto"), Package: s("package_name"), Syntax: s("proto3"),
		Dependency: []string{"artist.proto"}, MessageType: []*descriptorpb.DescriptorProto{album},
		EnumType: []*descriptorpb.EnumDescriptorProto{{Name: s("Genre"), Value: []*descriptorpb.EnumValueDescriptorProto{
			{Name: s("GENRE_UNSPECIFIED"), Number: n(0)}, {Name: s("ROCK"), Number: n(1)}, {Name: s("JAZZ"), Number: n(2)}}}}}
	set := &descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{artist, albumFile}}
	files, err := protodesc.NewFiles(set)
	if err != nil {
		t.Fatal(err)
	}
	d, err := files.FindDescriptorByName("package_name.Album")
	if err != nil {
		t.Fatal(err)
	}
	return set, d.(protoreflect.MessageDescriptor)
}

// TestSQLProtoColumns reads PROTO columns through the official clients:
// a schema bundle's messages and enums in CAST, field access, WHERE,
// aggregates, expression.*, results and parameters, and the bundle rules
// "Create and manage protobuf schemas" documents.
func TestSQLProtoColumns(t *testing.T) {
	addr := serve(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	conn := dial(t, addr)
	opt := option.WithGRPCConn(conn)
	iac, err := bigtable.NewInstanceAdminClient(ctx, "p", opt)
	if err != nil {
		t.Fatal(err)
	}
	if err := iac.CreateInstance(ctx, &bigtable.InstanceConf{InstanceId: "proto-inst", DisplayName: "protos", ClusterId: "proto-cluster", Zone: "us-central1-b", NumNodes: 1}); err != nil {
		t.Fatal(err)
	}
	ac, err := bigtable.NewAdminClient(ctx, "p", "proto-inst", opt)
	if err != nil {
		t.Fatal(err)
	}
	c, err := bigtable.NewClient(ctx, "p", "proto-inst", opt)
	if err != nil {
		t.Fatal(err)
	}
	if err := ac.CreateTable(ctx, "Music"); err != nil {
		t.Fatal(err)
	}
	if err := ac.CreateColumnFamily(ctx, "Music", "album_details"); err != nil {
		t.Fatal(err)
	}
	set, md := musicFiles(t, false)
	raw, _ := proto.Marshal(set)
	if err := ac.CreateSchemaBundle(ctx, &bigtable.SchemaBundleConf{TableID: "Music", SchemaBundleID: "bundle_name",
		ProtoSchema: &bigtable.ProtoSchemaInfo{ProtoDescriptors: raw}}); err != nil {
		t.Fatalf("CreateSchemaBundle: %v", err)
	}
	if err := ac.CreateSchemaBundle(ctx, &bigtable.SchemaBundleConf{TableID: "Music", SchemaBundleID: "a.b",
		ProtoSchema: &bigtable.ProtoSchemaInfo{ProtoDescriptors: raw}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a bundle ID with a period = %v, want InvalidArgument", err)
	}
	album := func(title, artist string, year int32, genre protoreflect.EnumNumber, tags ...string) []byte {
		m := dynamicpb.NewMessage(md)
		m.Set(md.Fields().ByName("title"), protoreflect.ValueOfString(title))
		if artist != "" {
			am := dynamicpb.NewMessage(md.Fields().ByName("artist").Message())
			am.Set(am.Descriptor().Fields().ByName("name"), protoreflect.ValueOfString(artist))
			m.Set(md.Fields().ByName("artist"), protoreflect.ValueOfMessage(am))
		}
		m.Set(md.Fields().ByName("release_year"), protoreflect.ValueOfInt32(year))
		m.Set(md.Fields().ByName("genre"), protoreflect.ValueOfEnum(genre))
		l := m.Mutable(md.Fields().ByName("tags")).List()
		for _, tg := range tags {
			l.Append(protoreflect.ValueOfString(tg))
		}
		b, err := proto.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	rows := map[string][]byte{
		"r1": album("First", "Dana A.", 2020, 1, "live", "debut"),
		"r2": album("Second", "Dana A.", 2022, 2),
		"r3": album("Third", "", 2024, 0),
	}
	for k, v := range rows {
		m := bigtable.NewMutation()
		m.Set("album_details", "album", bigtable.Timestamp(1000), v)
		if err := c.Open("Music").Apply(ctx, k, m); err != nil {
			t.Fatal(err)
		}
	}
	const a = "CAST(album_details['album'] AS bundle_name.package_name.Album)"
	run := func(sql string, params map[string]bigtable.SQLType, bind map[string]any) []bigtable.ResultRow {
		t.Helper()
		ps, err := c.PrepareStatement(ctx, sql, params)
		if err != nil {
			t.Fatalf("PrepareStatement(%s): %v", sql, err)
		}
		bs, err := ps.Bind(bind)
		if err != nil {
			t.Fatalf("Bind: %v", err)
		}
		var out []bigtable.ResultRow
		if err := bs.Execute(ctx, func(r bigtable.ResultRow) bool { out = append(out, r); return true }); err != nil {
			t.Fatalf("Execute(%s): %v", sql, err)
		}
		return out
	}
	str := func(r bigtable.ResultRow, i int) string {
		t.Helper()
		var s *string
		if err := r.GetByIndex(i, &s); err != nil {
			t.Fatalf("GetByIndex(%d): %v", i, err)
		}
		if s == nil {
			return "NULL"
		}
		return *s
	}
	// The page's queries: a field, a nested field, WHERE, AVG.
	out := run("SELECT _key, "+a+".title, "+a+".artist.name FROM Music ORDER BY _key", nil, nil)
	var got []string
	for _, r := range out {
		got = append(got, str(r, 1)+"/"+str(r, 2))
	}
	if strings.Join(got, ",") != "First/Dana A.,Second/Dana A.,Third/NULL" {
		t.Errorf("title, artist.name = %v", got)
	}
	out = run("SELECT _key FROM Music WHERE "+a+".artist.name = 'Dana A.' ORDER BY _key", nil, nil)
	if len(out) != 2 {
		t.Errorf("WHERE artist.name = 'Dana A.' gave %d rows, want 2", len(out))
	}
	out = run("SELECT AVG("+a+".release_year) AS y FROM Music", nil, nil)
	var avg *float64
	if err := out[0].GetByName("y", &avg); err != nil || avg == nil || *avg != 2022 {
		t.Errorf("AVG(release_year) = %v, %v", avg, err)
	}
	// Enums by name, CAST to STRING and INT64, has_, repeated fields.
	out = run("SELECT _key, CAST("+a+".genre AS STRING), CAST("+a+".genre AS INT64), "+a+".has_artist, ARRAY_LENGTH("+a+".tags) FROM Music WHERE "+a+".genre = 'JAZZ'", nil, nil)
	if len(out) != 1 {
		t.Fatalf("WHERE genre = 'JAZZ' gave %d rows, want 1", len(out))
	}
	var g int64
	var has bool
	var n int64
	if err := out[0].GetByIndex(2, &g); err != nil || str(out[0], 1) != "JAZZ" || g != 2 {
		t.Errorf("genre = %q, %d, %v", str(out[0], 1), g, err)
	}
	if err := out[0].GetByIndex(3, &has); err != nil || !has {
		t.Errorf("has_artist = %v, %v", has, err)
	}
	if err := out[0].GetByIndex(4, &n); err != nil || n != 0 {
		t.Errorf("ARRAY_LENGTH(tags) = %d, %v", n, err)
	}
	// The data API: a message comes back as Type.proto_type in
	// bytes_value, an enum as Type.enum_type in int_value, and both are
	// taken as parameters.
	data := btpb.NewBigtableClient(conn)
	prep, err := data.PrepareQuery(ctx, &btpb.PrepareQueryRequest{InstanceName: "projects/p/instances/proto-inst",
		Query:      "SELECT " + a + " AS album, " + a + ".genre AS genre, " + a + ".* FROM Music WHERE " + a + ".genre = @g AND " + a + " IS NOT NULL",
		ParamTypes: map[string]*btpb.Type{"g": protoEnumType("bundle_name", "package_name.Genre")}})
	if err != nil {
		t.Fatalf("PrepareQuery: %v", err)
	}
	cols := prep.GetMetadata().GetProtoSchema().GetColumns()
	if p := cols[0].GetType().GetProtoType(); p.GetSchemaBundleId() != "bundle_name" || p.GetMessageName() != "package_name.Album" {
		t.Errorf("album column type = %v", cols[0].GetType())
	}
	if e := cols[1].GetType().GetEnumType(); e.GetEnumName() != "package_name.Genre" {
		t.Errorf("genre column type = %v", cols[1].GetType())
	}
	var names []string
	for _, col := range cols[2:] {
		names = append(names, col.GetName())
	}
	if strings.Join(names, ",") != "title,artist,release_year,genre,tags" {
		t.Errorf("expression.* columns = %v", names)
	}
	stream, err := data.ExecuteQuery(ctx, &btpb.ExecuteQueryRequest{InstanceName: "projects/p/instances/proto-inst", PreparedQuery: prep.GetPreparedQuery(),
		Params: map[string]*btpb.Value{"g": {Type: protoEnumType("bundle_name", "package_name.Genre"), Kind: &btpb.Value_IntValue{IntValue: 1}}}})
	if err != nil {
		t.Fatal(err)
	}
	var vals []*btpb.Value
	for {
		resp, err := stream.Recv()
		if err != nil {
			break
		}
		if b := resp.GetResults().GetProtoRowsBatch().GetBatchData(); len(b) > 0 {
			var pr btpb.ProtoRows
			if err := proto.Unmarshal(b, &pr); err != nil {
				t.Fatal(err)
			}
			vals = append(vals, pr.GetValues()...)
		}
	}
	if len(vals) != len(cols) {
		t.Fatalf("ExecuteQuery gave %d values, want one row of %d", len(vals), len(cols))
	}
	m := dynamicpb.NewMessage(md)
	if err := proto.Unmarshal(vals[0].GetBytesValue(), m); err != nil || m.Get(md.Fields().ByName("title")).String() != "First" {
		t.Errorf("album = %v, %v", m, err)
	}
	if vals[1].GetIntValue() != 1 {
		t.Errorf("genre = %v", vals[1])
	}
	// Refusals: an unknown bundle or message, a missing field, a message
	// compared, bytes that are not the message, an enum name not defined.
	for sql, want := range map[string]codes.Code{
		"SELECT CAST(album_details['album'] AS nope.package_name.Album).title FROM Music":       codes.InvalidArgument,
		"SELECT CAST(album_details['album'] AS bundle_name.package_name.Nope).title FROM Music": codes.InvalidArgument,
		"SELECT " + a + ".nope FROM Music":                                                      codes.InvalidArgument,
		"SELECT _key FROM Music WHERE " + a + " = " + a:                                         codes.InvalidArgument,
		"SELECT _key FROM Music WHERE " + a + ".genre = 'POLKA'":                                codes.InvalidArgument,
		"SELECT " + a + ".title FROM Music GROUP BY " + a:                                       codes.InvalidArgument,
	} {
		if _, err := c.PrepareStatement(ctx, sql, nil); status.Code(err) != want {
			t.Errorf("%s = %v, want %v", sql, err, want)
		}
	}
	m2 := bigtable.NewMutation()
	m2.Set("album_details", "album", bigtable.Timestamp(1000), []byte{0xff, 0xff})
	if err := c.Open("Music").Apply(ctx, "r9", m2); err != nil {
		t.Fatal(err)
	}
	ps, err := c.PrepareStatement(ctx, "SELECT SAFE_CAST(album_details['album'] AS bundle_name.package_name.Album).title FROM Music WHERE _key = 'r9'", nil)
	if err != nil {
		t.Fatal(err)
	}
	bs, _ := ps.Bind(nil)
	var safe []bigtable.ResultRow
	if err := bs.Execute(ctx, func(r bigtable.ResultRow) bool { safe = append(safe, r); return true }); err != nil || len(safe) != 1 || str(safe[0], 0) != "NULL" {
		t.Errorf("SAFE_CAST of bytes that are not an Album = %v, %v", safe, err)
	}
	del := bigtable.NewMutation()
	del.DeleteRow()
	if err := c.Open("Music").Apply(ctx, "r9", del); err != nil {
		t.Fatal(err)
	}
	// A logical view reading the bundle keeps it from being deleted, and
	// an update that is not backward compatible fails.
	if err := iac.CreateLogicalView(ctx, "proto-inst", &bigtable.LogicalViewInfo{LogicalViewID: "titles",
		Query: "SELECT _key, " + a + ".title AS title FROM Music"}); err != nil {
		t.Fatalf("CreateLogicalView: %v", err)
	}
	out = run("SELECT title FROM titles", nil, nil)
	if len(out) != 3 {
		t.Errorf("the view gave %d rows, want 3", len(out))
	}
	if err := ac.DeleteSchemaBundle(ctx, "Music", "bundle_name"); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("DeleteSchemaBundle of a bundle a view uses = %v, want FailedPrecondition", err)
	}
	dropped, _ := musicFiles(t, true)
	rawDropped, _ := proto.Marshal(dropped)
	upd := bigtable.UpdateSchemaBundleConf{SchemaBundleConf: bigtable.SchemaBundleConf{TableID: "Music", SchemaBundleID: "bundle_name",
		ProtoSchema: &bigtable.ProtoSchemaInfo{ProtoDescriptors: rawDropped}}, IgnoreWarnings: true}
	if err := ac.UpdateSchemaBundle(ctx, upd); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("an incompatible update of a bundle a view uses = %v, want FailedPrecondition", err)
	}
	if err := iac.DeleteLogicalView(ctx, "proto-inst", "titles"); err != nil {
		t.Fatal(err)
	}
	upd.IgnoreWarnings = false
	if err := ac.UpdateSchemaBundle(ctx, upd); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("an incompatible update = %v, want FailedPrecondition", err)
	}
	upd.IgnoreWarnings = true
	if err := ac.UpdateSchemaBundle(ctx, upd); err != nil {
		t.Errorf("an incompatible update with ignore_warnings = %v", err)
	}
	if _, err := c.PrepareStatement(ctx, "SELECT "+a+".release_year FROM Music", nil); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a field the bundle no longer has = %v, want InvalidArgument", err)
	}
	if err := ac.DeleteSchemaBundle(ctx, "Music", "bundle_name"); err != nil {
		t.Errorf("DeleteSchemaBundle: %v", err)
	}
	for i := 0; i < 10; i++ {
		if err := ac.CreateSchemaBundle(ctx, &bigtable.SchemaBundleConf{TableID: "Music", SchemaBundleID: "b" + string(rune('0'+i)),
			ProtoSchema: &bigtable.ProtoSchemaInfo{ProtoDescriptors: raw}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := ac.CreateSchemaBundle(ctx, &bigtable.SchemaBundleConf{TableID: "Music", SchemaBundleID: "b10",
		ProtoSchema: &bigtable.ProtoSchemaInfo{ProtoDescriptors: raw}}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("an eleventh bundle = %v, want FailedPrecondition", err)
	}
}
