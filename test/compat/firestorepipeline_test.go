//go:build compat

package compat

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"cloud.google.com/go/firestore"
	adminpb "cloud.google.com/go/firestore/apiv1/admin/adminpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// enterpriseDatabase creates the Enterprise edition database id through
// the official Admin client, deleting it when the test ends.
func enterpriseDatabase(t *testing.T, h *Harness, id string) string {
	t.Helper()
	c := firestoreAdmin(t, h)
	ctx := h.Context()
	parent := "projects/" + h.Project()
	op, err := c.CreateDatabase(ctx, &adminpb.CreateDatabaseRequest{Parent: parent, DatabaseId: id,
		Database: &adminpb.Database{LocationId: "nam5", Type: adminpb.Database_FIRESTORE_NATIVE, DatabaseEdition: adminpb.Database_ENTERPRISE}})
	if err != nil {
		t.Fatalf("CreateDatabase (ENTERPRISE): %v", err)
	}
	db, err := op.Wait(ctx)
	if err != nil || db.DatabaseEdition != adminpb.Database_ENTERPRISE {
		t.Fatalf("the created database = %v, %v: want ENTERPRISE", db, err)
	}
	t.Cleanup(func() {
		if op, err := c.DeleteDatabase(h.Context(), &adminpb.DeleteDatabaseRequest{Name: db.Name}); err == nil {
			_, _ = op.Wait(h.Context())
		}
	})
	return db.Name
}

// TestFirestorePipelines (#1500): pipelines run on an Enterprise edition
// database, through the official Go client's pipeline API: the front reads
// the emulator's documents and evaluates the stages itself. A Standard
// database (`(default)`) refuses them INVALID_ARGUMENT with Google's
// message, and a stage that is not served is refused naming it.
//
// covers: google.firestore.v1.Firestore/ExecutePipeline
func TestFirestorePipelines(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvFirestore)
	ctx := h.Context()
	enterpriseDatabase(t, h, "piped-ent")
	t.Setenv("FIRESTORE_EMULATOR_HOST", h.Endpoint(EnvFirestore))
	c, err := firestore.NewClientWithDatabase(ctx, h.Project(), "piped-ent")
	if err != nil {
		t.Fatalf("NewClientWithDatabase: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	books := c.Collection("books")
	for id, d := range map[string]map[string]any{
		"a": {"title": "Alpha", "genre": "sf", "rating": 5, "tags": []string{"x", "y"}},
		"b": {"title": "Beta", "genre": "sf", "rating": 3, "tags": []string{"y"}},
		"c": {"title": "Gamma", "genre": "poetry", "rating": 4},
	} {
		if _, err := books.Doc(id).Set(ctx, d); err != nil {
			t.Fatalf("Set %s: %v", id, err)
		}
	}
	if _, err := c.Collection("shelves").Doc("s1").Collection("books").Doc("d").Set(ctx,
		map[string]any{"title": "Delta", "genre": "poetry", "rating": 1}); err != nil {
		t.Fatal(err)
	}
	names := func(rs []*firestore.PipelineResult) string {
		var out []string
		for _, r := range rs {
			out = append(out, r.Data()["title"].(string))
		}
		return strings.Join(out, ",")
	}

	got, err := c.Pipeline().Collection("books").
		Where(firestore.And(firestore.GreaterThan(firestore.FieldOf("rating"), 3), firestore.Equal(firestore.FieldOf("genre"), "sf"))).
		Execute(ctx).Results().GetAll()
	if err != nil {
		t.Fatalf("collection, where: %v", err)
	}
	if names(got) != "Alpha" || got[0].Ref() == nil || got[0].Ref().Path != books.Doc("a").Path {
		t.Errorf("where rating > 3 and genre == sf = %s (%v), want Alpha, books/a", names(got), got)
	}

	got, err = c.Pipeline().CollectionGroup("books").
		Sort(firestore.Orders(firestore.Ascending(firestore.FieldOf("rating")))).Offset(1).Limit(2).
		Execute(ctx).Results().GetAll()
	if err != nil || names(got) != "Beta,Gamma" {
		t.Errorf("collection group, sort, offset, limit = %s, %v; want Beta,Gamma", names(got), err)
	}

	got, err = c.Pipeline().Collection("books").
		Aggregate(firestore.Accumulators(firestore.CountAll().As("n"), firestore.Sum("rating").As("total")),
			firestore.WithAggregateGroups("genre")).
		Sort(firestore.Orders(firestore.Descending(firestore.FieldOf("genre")))).
		Execute(ctx).Results().GetAll()
	if err != nil || len(got) != 2 || got[0].Data()["genre"] != "sf" || got[0].Data()["n"] != int64(2) || got[0].Data()["total"] != int64(8) {
		t.Errorf("aggregate by genre = %v, %v; want sf: n 2, total 8 first", got, err)
	}

	got, err = c.Pipeline().Collection("books").
		AddFields(firestore.Selectables(firestore.StringConcat(firestore.FieldOf("title"), "!").As("shout"))).
		RemoveFields(firestore.Fields("tags")).
		Unnest(firestore.Array("p", "q").As("letter")).
		Where(firestore.Equal(firestore.FieldOf("letter"), "q")).
		Sort(firestore.Orders(firestore.Ascending(firestore.FieldOf("title")))).
		Execute(ctx).Results().GetAll()
	if err != nil || len(got) != 3 || got[0].Data()["shout"] != "Alpha!" || got[0].Data()["tags"] != nil {
		t.Errorf("add fields, remove fields, unnest = %v, %v", got, err)
	}

	got, err = c.Pipeline().Collection("books").Select(firestore.Fields("genre")).Distinct(firestore.Fields("genre")).
		Execute(ctx).Results().GetAll()
	if err != nil || len(got) != 2 {
		t.Errorf("distinct genre = %v, %v; want 2", got, err)
	}

	snap := c.Pipeline().Collection("books").Execute(ctx, firestore.WithExplainMode(firestore.ExplainModeAnalyze))
	if rs, err := snap.Results().GetAll(); err != nil || len(rs) != 3 {
		t.Fatalf("analyze = %d results, %v", len(rs), err)
	}
	if text, err := snap.ExplainStats().Text(); err != nil || !strings.Contains(text, "results returned: 3") {
		t.Errorf("explain stats = %q, %v", text, err)
	}

	_, err = c.Pipeline().Collection("books").RawStage("search", nil).Execute(ctx).Results().GetAll()
	if status.Code(err) != codes.Unimplemented || !strings.Contains(err.Error(), `"search"`) {
		t.Errorf("a search stage = %v, want UNIMPLEMENTED naming it", err)
	}

	// Over REST, the stream is a JSON array of the responses.
	body := `{"structuredPipeline":{"pipeline":{"stages":[{"name":"collection","args":[{"referenceValue":"/books"}]},` +
		`{"name":"limit","args":[{"integerValue":"2"}]}]}}}`
	resp, err := http.Post("http://"+h.Endpoint(EnvFirestore)+"/v1/projects/"+h.Project()+"/databases/piped-ent/documents:executePipeline",
		"application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("executePipeline over REST: %v", err)
	}
	var rest []struct {
		Results []struct {
			Name string `json:"name"`
		} `json:"results"`
		ExecutionTime string `json:"executionTime"`
	}
	err = json.NewDecoder(resp.Body).Decode(&rest)
	_ = resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK || len(rest) != 1 || len(rest[0].Results) != 2 || rest[0].ExecutionTime == "" {
		t.Errorf("executePipeline over REST = %d %+v, %v; want one response with 2 results", resp.StatusCode, rest, err)
	}

	def := firestoreClient(t, h)
	_, err = def.Pipeline().Collection("books").Execute(ctx).Results().GetAll()
	wantRefused(t, "ExecutePipeline on (default)", err, codes.InvalidArgument,
		"ExecutePipeline requires the Database Edition to be `enterprise`.")
}

// TestFirestoreAdminUserCreds (#1500): user credentials on an Enterprise
// edition database, through the official Admin client: created ENABLED
// with a server-generated password returned once, got and listed without
// it, disabled and enabled, their password reset, and deleted. On a
// Standard database (`(default)`) they are refused FAILED_PRECONDITION.
// Nothing authenticates with them: the MongoDB wire protocol is not
// served.
//
// covers: google.firestore.admin.v1.FirestoreAdmin/CreateUserCreds, google.firestore.admin.v1.FirestoreAdmin/GetUserCreds, google.firestore.admin.v1.FirestoreAdmin/ListUserCreds, google.firestore.admin.v1.FirestoreAdmin/EnableUserCreds
// covers: google.firestore.admin.v1.FirestoreAdmin/DisableUserCreds, google.firestore.admin.v1.FirestoreAdmin/ResetUserPassword, google.firestore.admin.v1.FirestoreAdmin/DeleteUserCreds
func TestFirestoreAdminUserCreds(t *testing.T) {
	h := New(t)
	c := firestoreAdmin(t, h)
	ctx := h.Context()
	db := enterpriseDatabase(t, h, "creds-ent")

	uc, err := c.CreateUserCreds(ctx, &adminpb.CreateUserCredsRequest{Parent: db, UserCredsId: "app-user", UserCreds: &adminpb.UserCreds{}})
	if err != nil {
		t.Fatalf("CreateUserCreds: %v", err)
	}
	if uc.Name != db+"/userCreds/app-user" || uc.State != adminpb.UserCreds_ENABLED || uc.SecurePassword == "" ||
		uc.CreateTime == nil || !strings.HasPrefix(uc.GetResourceIdentity().GetPrincipal(), "principal://firestore.googleapis.com/") {
		t.Errorf("created = %v", uc)
	}
	if _, err := c.CreateUserCreds(ctx, &adminpb.CreateUserCredsRequest{Parent: db, UserCredsId: "app-user", UserCreds: &adminpb.UserCreds{}}); status.Code(err) != codes.AlreadyExists {
		t.Errorf("creating it again: %v, want ALREADY_EXISTS", err)
	}
	if _, err := c.CreateUserCreds(ctx, &adminpb.CreateUserCredsRequest{Parent: db, UserCredsId: "Bad_ID", UserCreds: &adminpb.UserCreds{}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("an invalid ID: %v, want INVALID_ARGUMENT", err)
	}
	got, err := c.GetUserCreds(ctx, &adminpb.GetUserCredsRequest{Name: uc.Name})
	if err != nil || got.SecurePassword != "" || got.State != adminpb.UserCreds_ENABLED {
		t.Errorf("GetUserCreds = %v, %v: want ENABLED, no password", got, err)
	}
	list, err := c.ListUserCreds(ctx, &adminpb.ListUserCredsRequest{Parent: db})
	if err != nil || len(list.UserCreds) != 1 || list.UserCreds[0].Name != uc.Name {
		t.Errorf("ListUserCreds = %v, %v", list, err)
	}
	if d, err := c.DisableUserCreds(ctx, &adminpb.DisableUserCredsRequest{Name: uc.Name}); err != nil || d.State != adminpb.UserCreds_DISABLED {
		t.Errorf("DisableUserCreds = %v, %v", d, err)
	}
	if e, err := c.EnableUserCreds(ctx, &adminpb.EnableUserCredsRequest{Name: uc.Name}); err != nil || e.State != adminpb.UserCreds_ENABLED {
		t.Errorf("EnableUserCreds = %v, %v", e, err)
	}
	r, err := c.ResetUserPassword(ctx, &adminpb.ResetUserPasswordRequest{Name: uc.Name})
	if err != nil || r.SecurePassword == "" || r.SecurePassword == uc.SecurePassword {
		t.Errorf("ResetUserPassword: %v; want a new password", err)
	}
	if err := c.DeleteUserCreds(ctx, &adminpb.DeleteUserCredsRequest{Name: uc.Name}); err != nil {
		t.Fatalf("DeleteUserCreds: %v", err)
	}
	if _, err := c.GetUserCreds(ctx, &adminpb.GetUserCredsRequest{Name: uc.Name}); status.Code(err) != codes.NotFound {
		t.Errorf("GetUserCreds after delete: %v, want NOT_FOUND", err)
	}
	std := "projects/" + h.Project() + "/databases/(default)"
	if _, err := c.ListUserCreds(ctx, &adminpb.ListUserCredsRequest{Parent: std}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("ListUserCreds on (default): %v, want FAILED_PRECONDITION", err)
	}
}
