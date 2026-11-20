//go:build browser

package browser

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	database "cloud.google.com/go/spanner/admin/database/apiv1"
	"cloud.google.com/go/spanner/admin/database/apiv1/databasepb"
	instance "cloud.google.com/go/spanner/admin/instance/apiv1"
	"cloud.google.com/go/spanner/admin/instance/apiv1/instancepb"
	sp "cloud.google.com/go/spanner/apiv1"
	"cloud.google.com/go/spanner/apiv1/spannerpb"
	"github.com/chromedp/chromedp"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/known/structpb"
)

// TestSpannerProtoColumnsThroughTheDataTab (#2017), in a shard whose
// instance serves Spanner: a database made with the official client and a
// proto bundle (message cb.music.SingerInfo, enum cb.music.Genre) has a
// table whose Data tab shows a PROTO value as the message's JSON and an
// ENUM by its value name. Insert row takes the message's JSON in a
// textarea and the enum from a select of its value names; Edit row is
// prefilled with them and changes both; each is read back through the
// official client as the serialised message and the enum's number. The
// Insert row dialog meets the WCAG audit.
func TestSpannerProtoColumnsThroughTheDataTab(t *testing.T) {
	needService(t, "spanner")
	addr := strings.TrimSpace(os.Getenv(envSpanner))
	if addr == "" {
		t.Skipf("%s is not set: the database is made with the official client", envSpanner)
	}
	project := uniqueProject(t)
	q := "?project=" + project
	inst := fmt.Sprintf("browser-proto-%d", time.Now().UnixNano()%1e8)
	ctx := context.Background()
	opts := []option.ClientOption{option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials()))}

	ia, err := instance.NewInstanceAdminClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer ia.Close()
	iop, err := ia.CreateInstance(ctx, &instancepb.CreateInstanceRequest{Parent: "projects/" + project, InstanceId: inst,
		Instance: &instancepb.Instance{Config: "projects/" + project + "/instanceConfigs/emulator-config", DisplayName: "Proto columns", NodeCount: 1}})
	if err == nil {
		_, err = iop.Wait(ctx)
	}
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	t.Cleanup(func() {
		_ = ia.DeleteInstance(context.Background(), &instancepb.DeleteInstanceRequest{Name: "projects/" + project + "/instances/" + inst})
	})
	da, err := database.NewDatabaseAdminClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer da.Close()
	opt := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum()
	raw, _ := proto.Marshal(&descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{{
		Name: proto.String("cb/music.proto"), Package: proto.String("cb.music"), Syntax: proto.String("proto2"),
		MessageType: []*descriptorpb.DescriptorProto{{Name: proto.String("SingerInfo"), Field: []*descriptorpb.FieldDescriptorProto{
			{Name: proto.String("singer_id"), JsonName: proto.String("singerId"), Number: proto.Int32(1), Label: opt, Type: descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum()},
			{Name: proto.String("nationality"), JsonName: proto.String("nationality"), Number: proto.Int32(2), Label: opt, Type: descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum()},
		}}},
		EnumType: []*descriptorpb.EnumDescriptorProto{{Name: proto.String("Genre"), Value: []*descriptorpb.EnumValueDescriptorProto{
			{Name: proto.String("POP"), Number: proto.Int32(0)}, {Name: proto.String("JAZZ"), Number: proto.Int32(1)}, {Name: proto.String("ROCK"), Number: proto.Int32(2)},
		}}},
	}}})
	dop, err := da.CreateDatabase(ctx, &databasepb.CreateDatabaseRequest{
		Parent: "projects/" + project + "/instances/" + inst, CreateStatement: "CREATE DATABASE protodb",
		ExtraStatements: []string{"CREATE PROTO BUNDLE (cb.music.SingerInfo, cb.music.Genre)",
			"CREATE TABLE Singers (Id INT64 NOT NULL, Info cb.music.SingerInfo, Genre cb.music.Genre) PRIMARY KEY (Id)"},
		ProtoDescriptors: raw})
	if err == nil {
		_, err = dop.Wait(ctx)
	}
	if err != nil {
		t.Fatalf("CreateDatabase with a proto bundle: %v", err)
	}
	db := "projects/" + project + "/instances/" + inst + "/databases/protodb"
	dc, err := sp.NewClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer dc.Close()
	sess, err := dc.CreateSession(ctx, &spannerpb.CreateSessionRequest{Database: db})
	if err != nil {
		t.Fatal(err)
	}
	b64 := base64.StdEncoding.EncodeToString
	if _, err := dc.Commit(ctx, &spannerpb.CommitRequest{Session: sess.GetName(),
		Transaction: &spannerpb.CommitRequest_SingleUseTransaction{SingleUseTransaction: &spannerpb.TransactionOptions{
			Mode: &spannerpb.TransactionOptions_ReadWrite_{ReadWrite: &spannerpb.TransactionOptions_ReadWrite{}}}},
		Mutations: []*spannerpb.Mutation{{Operation: &spannerpb.Mutation_Insert{Insert: &spannerpb.Mutation_Write{
			Table: "Singers", Columns: []string{"Id", "Info", "Genre"},
			Values: []*structpb.ListValue{{Values: []*structpb.Value{structpb.NewStringValue("1"),
				structpb.NewStringValue(b64([]byte{0x08, 0x07, 0x12, 0x02, 'F', 'R'})), structpb.NewStringValue("2")}}},
		}}}}}); err != nil {
		t.Fatalf("Commit the first row: %v", err)
	}
	read := func(id string) (nationality, genre string) {
		t.Helper()
		rs, err := dc.ExecuteSql(ctx, &spannerpb.ExecuteSqlRequest{Session: sess.GetName(),
			Sql:        "SELECT Info.nationality, CAST(Genre AS INT64) FROM Singers WHERE Id = @id",
			Params:     &structpb.Struct{Fields: map[string]*structpb.Value{"id": structpb.NewStringValue(id)}},
			ParamTypes: map[string]*spannerpb.Type{"id": {Code: spannerpb.TypeCode_INT64}}})
		if err != nil || len(rs.GetRows()) != 1 {
			t.Fatalf("reading row %s with the official client: %v (%d rows)", id, err, len(rs.GetRows()))
		}
		v := rs.GetRows()[0].GetValues()
		return v[0].GetStringValue(), v[1].GetStringValue()
	}

	page := "/spanner/" + inst + "/protodb/Singers" + q + "&tab=data"
	p := open(t)
	p.navigate(page)
	p.waitFor(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes('"nationality":"FR"') && r.textContent.includes("ROCK"))`)
	p.waitFor(`document.querySelector(".modal") === null`)
	p.clickText("#view .page-actions button", "Insert row")
	p.waitFor(`document.querySelector(".modal.is-open #f-col_Info") !== null`)
	var kinds []string
	p.eval(`[document.querySelector(".modal.is-open #f-col_Info").tagName, document.querySelector(".modal.is-open #f-col_Genre").tagName]`, &kinds)
	if !slices.Equal(kinds, []string{"TEXTAREA", "SELECT"}) {
		t.Errorf("Insert row draws Info and Genre as %v, want a textarea and a select", kinds)
	}
	var options []string
	p.eval(`[...document.querySelectorAll(".modal.is-open #f-col_Genre option")].map((o) => o.value)`, &options)
	if !slices.Equal(options, []string{"", "POP", "JAZZ", "ROCK"}) {
		t.Errorf("the Genre select offers %v, want NULL and POP, JAZZ, ROCK", options)
	}
	setField(p, ".modal.is-open", "f-col_Id", "2")
	setField(p, ".modal.is-open", "f-col_Info", `{"singerId": "8", "nationality": "JP"}`)
	setField(p, ".modal.is-open", "f-col_Genre", "JAZZ")
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal") === null`)
	if n, g := read("2"); n != "JP" || g != "1" {
		t.Errorf("after Insert row the official client reads nationality %q, genre %q; want JP and 1 (JAZZ)", n, g)
	}
	rows := readDataPage(t, "spanner", project, inst, "protodb", "Singers").rows("data")
	if rows["2"]["Genre"] != "JAZZ" || !strings.Contains(rows["2"]["Info"], `"nationality":"JP"`) {
		t.Errorf("the Data tab reads row 2 as %v, want Genre JAZZ and Info's JSON", rows["2"])
	}

	p.navigate(page)
	p.waitFor(`document.querySelector('button[aria-label="Actions for 2"]') !== null`)
	p.run(chromedp.Click(chromedp.CSS(`button[aria-label="Actions for 2"]`)))
	p.clickText(`.overflow-menu:not([hidden]) [role="menuitem"]`, "Edit row")
	p.waitFor(`document.querySelector(".modal.is-open #f-col_Info") !== null`)
	var prefill []string
	p.eval(`[document.querySelector(".modal.is-open #f-col_Info").value, document.querySelector(".modal.is-open #f-col_Genre").value]`, &prefill)
	if len(prefill) != 2 || !strings.Contains(prefill[0], `"nationality":"JP"`) || prefill[1] != "JAZZ" {
		t.Errorf("Edit row is prefilled with %q, want Info's JSON and JAZZ", prefill)
	}
	setField(p, ".modal.is-open", "f-col_Info", `{"singerId": "8", "nationality": "KR"}`)
	setField(p, ".modal.is-open", "f-col_Genre", "POP")
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal") === null`)
	if n, g := read("2"); n != "KR" || g != "0" {
		t.Errorf("after Edit row the official client reads nationality %q, genre %q; want KR and 0 (POP)", n, g)
	}

	auditScreens(t, []a11yPage{
		{"Spanner Data tab with PROTO and ENUM columns", page, `[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes("ROCK"))`, ""},
		{"Spanner Insert row with PROTO and ENUM fields", page, `document.querySelector("#view .page-actions") !== null`,
			`[...document.querySelectorAll("#view .page-actions button")].find((b) => b.textContent.trim() === "Insert row").click()`},
	})
}
