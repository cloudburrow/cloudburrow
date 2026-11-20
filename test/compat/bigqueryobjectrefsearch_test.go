//go:build compat

package compat

import (
	"context"
	"fmt"
	"image/color"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	"cloud.google.com/go/bigquery/connection/apiv1/connectionpb"
)

// TestBigQueryObjectRefEmbeddingsAndSearch (#1983): ObjectRef inputs to
// the embedding and search functions, through the official
// cloud.google.com/go bigquery, storage and connection clients, on the
// instance's local embedder with EmbeddingGemma 2 loaded (the console's
// Local models API) and, for AI.AGG and AI.GENERATE_TABLE, its local
// generator:
//   - AI.EMBED of an ObjectRef: one 768-value embedding; on the text-only
//     embeddinggemma-300m, the row's status;
//   - AI.SIMILARITY of an ObjectRef and a STRING: an image of a red circle
//     is nearer "a red circle" than "a blue square";
//   - an autonomous embedding column over an ObjectRef source column,
//     filled by the background job;
//   - VECTOR_SEARCH over that ObjectRef column with a STRING query value,
//     an ObjectRef query value and a query table of ObjectRefs (the
//     documented form), and AI.SEARCH with a STRING and an ObjectRef
//     query value; rerank over ObjectRefs refused, as documented;
//   - AI.COUNT_TOKENS of an ObjectRef refused (the page: STRING only);
//   - AI.AGG over ObjectRefs and AI.GENERATE_TABLE of a STRUCT prompt with
//     an ObjectRef, on the local generator. The model's words are
//     recorded, not asserted.
//
// covers: bigquery.jobs.query
func TestBigQueryObjectRefEmbeddingsAndSearch(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvEmbeddings)
	shardEndpoint(h, "CLOUDBURROW_TEST_BQML_STORAGE", EnvStorage)
	addr := consoleAddr(t, h)
	lctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	loadEmbeddingModel2(t, lctx, addr)

	c, _ := bigqueryClient(t, h)
	cc, project := remoteConnectionClient(t, h)
	conn, ds := remoteModelSetup(t, h, c, "orefs")
	ctx := h.Context()
	cn, err := cc.GetConnection(ctx, &connectionpb.GetConnectionRequest{Name: "projects/" + project + "/locations/us/connections/" + strings.Split(conn, ".")[2]})
	if err != nil {
		t.Fatalf("GetConnection: %v", err)
	}
	grantObjectRefs(t, h, cc, project, cn)
	bh, _ := objRefBucket(t, h, "orefs")
	grantBucket(t, h, bh, cn.GetCloudResource().GetServiceAccountId(), "roles/storage.objectViewer")
	shapes := []struct {
		name, kind string
		c          color.RGBA
	}{
		{"circle", "circle", color.RGBA{220, 20, 20, 255}},
		{"square", "square", color.RGBA{20, 40, 220, 255}},
		{"triangle", "triangle", color.RGBA{20, 180, 40, 255}},
	}
	uri := map[string]string{}
	for _, s := range shapes {
		w := bh.Object("shapes/" + s.name + ".png").NewWriter(ctx)
		w.ContentType = "image/png"
		if _, err := w.Write(shapeImage(t, s.kind, s.c)); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("write %s: %v", s.name, err)
		}
		uri[s.name] = "gs://" + bh.BucketName() + "/shapes/" + s.name + ".png"
	}
	ref := func(name string) string { return fmt.Sprintf("OBJ.MAKE_REF('%s', '%s')", uri[name], conn) }
	embed := fmt.Sprintf("connection_id => '%s', endpoint => '%s'", conn, embeddingModel2)

	// AI.EMBED of an ObjectRef.
	rows := mlQuery(t, h, c, fmt.Sprintf("SELECT ARRAY_LENGTH(e.result) AS n, e.status AS status FROM (SELECT AI.EMBED(%s, %s) AS e)", ref("circle"), embed))
	if len(rows) != 1 || rows[0]["n"] != int64(768) || rows[0]["status"] != "" {
		t.Fatalf("AI.EMBED of an ObjectRef: %v", rows)
	}
	rows = mlQuery(t, h, c, fmt.Sprintf("SELECT e.result IS NULL AS none, e.status AS status FROM (SELECT AI.EMBED(%s, model => 'embeddinggemma-300m') AS e)", ref("circle")))
	if len(rows) != 1 || rows[0]["none"] != true || !strings.Contains(fmt.Sprint(rows[0]["status"]), "text only") {
		t.Fatalf("AI.EMBED of an ObjectRef on embeddinggemma-300m: %v", rows)
	}

	// AI.SIMILARITY of an ObjectRef and a STRING.
	rows = mlQuery(t, h, c, fmt.Sprintf("SELECT AI.SIMILARITY(content1 => %s, content2 => 'a red circle', %s) AS same, "+
		"AI.SIMILARITY(content1 => %s, content2 => 'a blue square', %s) AS other", ref("circle"), embed, ref("circle"), embed))
	same, _ := rows[0]["same"].(float64)
	other, _ := rows[0]["other"].(float64)
	if len(rows) != 1 || same == 0 || same <= other {
		t.Fatalf("AI.SIMILARITY of an ObjectRef: %v", rows)
	}

	// An autonomous embedding column over an ObjectRef source.
	if err := mlRun(h, c, fmt.Sprintf("CREATE TABLE `%s.images` (name STRING, image_ref STRUCT<uri STRING, version STRING, authorizer STRING, details JSON>, "+
		"image_embedding STRUCT<result ARRAY<FLOAT64>, status STRING> GENERATED ALWAYS AS (AI.EMBED(image_ref, %s)) STORED OPTIONS (asynchronous = TRUE))", ds, embed)); err != nil {
		t.Fatalf("CREATE TABLE with an ObjectRef source: %v", err)
	}
	if err := mlRun(h, c, fmt.Sprintf("INSERT INTO `%s.images` (name, image_ref) VALUES ('circle', %s), ('square', %s), ('triangle', %s)",
		ds, ref("circle"), ref("square"), ref("triangle"))); err != nil {
		t.Fatalf("INSERT ObjectRefs: %v", err)
	}
	deadline := time.Now().Add(3 * time.Minute)
	for {
		rows = mlQuery(t, h, c, fmt.Sprintf("SELECT COUNTIF(ARRAY_LENGTH(image_embedding.result) = 768 AND image_embedding.status = '') AS done, "+
			"STRING_AGG(image_embedding.status) AS statuses FROM `%s.images`", ds))
		if rows[0]["done"] == int64(3) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the ObjectRef embeddings were not generated: %v", rows)
		}
		time.Sleep(time.Second)
	}

	nearest := func(what, sql string) []map[string]bigquery.Value {
		t.Helper()
		rows := mlQuery(t, h, c, sql)
		if len(rows) == 0 {
			t.Fatalf("%s: no rows", what)
		}
		return rows
	}
	// VECTOR_SEARCH over the ObjectRef column: a STRING query value, an
	// ObjectRef query value, and a query table of ObjectRefs.
	for _, s := range []string{"circle", "square"} {
		text := map[string]string{"circle": "a red circle", "square": "a blue square"}[s]
		rows = nearest("VECTOR_SEARCH, STRING", fmt.Sprintf("SELECT base.name AS name, distance FROM VECTOR_SEARCH(TABLE `%s.images`, 'image_ref', "+
			"query_value => '%s', top_k => 1, distance_type => 'COSINE')", ds, text))
		if rows[0]["name"] != s {
			t.Errorf("VECTOR_SEARCH for %q: %v", text, rows)
		}
		rows = nearest("VECTOR_SEARCH, ObjectRef", fmt.Sprintf("SELECT base.name AS name, distance FROM VECTOR_SEARCH(TABLE `%s.images`, 'image_ref', "+
			"query_value => %s, top_k => 1, distance_type => 'COSINE')", ds, ref(s)))
		if d, _ := rows[0]["distance"].(float64); rows[0]["name"] != s || d > 1e-3 {
			t.Errorf("VECTOR_SEARCH for the %s's ObjectRef: %v", s, rows)
		}
	}
	rows = nearest("VECTOR_SEARCH, query table", fmt.Sprintf("SELECT query.q AS q, base.name AS name FROM VECTOR_SEARCH(TABLE `%s.images`, 'image_ref', "+
		"(SELECT name AS q, image_ref FROM `%s.images`), top_k => 1, distance_type => 'COSINE') ORDER BY q", ds, ds))
	if len(rows) != 3 {
		t.Fatalf("VECTOR_SEARCH with a query table of ObjectRefs: %v", rows)
	}
	for _, r := range rows {
		if r["q"] != r["name"] {
			t.Errorf("VECTOR_SEARCH with a query table of ObjectRefs: %v", rows)
		}
	}

	// AI.SEARCH: a STRING query value (the documented "table of images
	// with a text query") and an ObjectRef.
	rows = nearest("AI.SEARCH, STRING", fmt.Sprintf("SELECT base.name AS name, distance FROM AI.SEARCH(TABLE `%s.images`, 'image_ref', 'a green triangle', "+
		"top_k => 1, distance_type => 'COSINE')", ds))
	if rows[0]["name"] != "triangle" {
		t.Errorf("AI.SEARCH for a green triangle: %v", rows)
	}
	rows = nearest("AI.SEARCH, ObjectRef", fmt.Sprintf("SELECT base.name AS name, distance FROM AI.SEARCH(TABLE `%s.images`, 'image_ref', %s, top_k => 1)", ds, ref("square")))
	if rows[0]["name"] != "square" {
		t.Errorf("AI.SEARCH for the square's ObjectRef: %v", rows)
	}
	if err := mlRun(h, c, fmt.Sprintf("SELECT * FROM AI.SEARCH(TABLE `%s.images`, 'image_ref', 'a circle', top_k => 1, rerank => TRUE)", ds)); err == nil ||
		!strings.Contains(err.Error(), "ObjectRef") {
		t.Errorf("AI.SEARCH rerank over ObjectRefs: %v", err)
	}

	t.Run("AI.AGG and AI.GENERATE_TABLE", func(t *testing.T) {
		h := h.For(t)
		h.Endpoint(EnvLocalAI)
		// AI.COUNT_TOKENS runs as the caller (TestBigQueryAICountTokens).
		grantProject(t, h, remoteProjectsClient(t, h), project, "roles/aiplatform.user", callerMember(project))
		if err := mlRun(h, c, fmt.Sprintf("SELECT AI.COUNT_TOKENS(%s, endpoint => '%s')", ref("circle"), localModel)); err == nil ||
			!strings.Contains(err.Error(), "STRING") {
			t.Errorf("AI.COUNT_TOKENS of an ObjectRef: %v", err)
		}
		rows := mlQuery(t, h, c, fmt.Sprintf("SELECT AI.AGG(image_ref, 'List the colour of the shape in each image.', connection_id => '%s', endpoint => '%s') AS colours "+
			"FROM `%s.images`", conn, localModel, ds))
		if len(rows) != 1 || strings.TrimSpace(fmt.Sprint(rows[0]["colours"])) == "" || rows[0]["colours"] == nil {
			t.Fatalf("AI.AGG of ObjectRefs: %v", rows)
		}
		t.Logf("AI.AGG of three images (recorded, not asserted): %q", rows[0]["colours"])
		if err := mlRun(h, c, fmt.Sprintf("CREATE MODEL `%s.gen` REMOTE WITH CONNECTION `%s` OPTIONS(ENDPOINT = '%s')", ds, conn, localModel)); err != nil {
			t.Fatalf("CREATE MODEL: %v", err)
		}
		rows = mlQuery(t, h, c, fmt.Sprintf("SELECT name, colour, status FROM AI.GENERATE_TABLE(MODEL `%s.gen`, "+
			"(SELECT name, ('What is the colour of the shape in this image? ', image_ref) AS prompt FROM `%s.images`), "+
			"STRUCT('colour STRING' AS output_schema)) ORDER BY name", ds, ds))
		if len(rows) != 3 {
			t.Fatalf("AI.GENERATE_TABLE of ObjectRef prompts: %v", rows)
		}
		for _, r := range rows {
			if r["status"] != "" || strings.TrimSpace(fmt.Sprint(r["colour"])) == "" {
				t.Errorf("AI.GENERATE_TABLE row: %v", r)
			}
			t.Logf("AI.GENERATE_TABLE %v (recorded, not asserted): %q", r["name"], r["colour"])
		}
	})
}
