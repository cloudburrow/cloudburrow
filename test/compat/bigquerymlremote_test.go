//go:build compat

package compat

import (
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"os"
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"
	connection "cloud.google.com/go/bigquery/connection/apiv1"
	"cloud.google.com/go/bigquery/connection/apiv1/connectionpb"
	"google.golang.org/api/option"
)

// remoteModelSetup makes a CLOUD_RESOURCE connection in the US through the
// official Connection API client, and a dataset: the connection's
// `project.us.id` and the dataset's name.
func remoteModelSetup(t *testing.T, h *Harness, c *bigquery.Client, suffix string) (string, string) {
	t.Helper()
	endpoint := h.Endpoint(EnvBigQuery)
	project := strings.TrimSpace(os.Getenv(EnvBigQueryProject))
	if project == "" {
		t.Skipf("%s is not set", EnvBigQueryProject)
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		t.Fatalf("BigQuery endpoint %q: %v", endpoint, err)
	}
	ctx := h.Context()
	cc, err := connection.NewRESTClient(ctx, option.WithEndpoint(u.Scheme+"://"+u.Host), option.WithoutAuthentication())
	if err != nil {
		t.Fatalf("connection.NewRESTClient: %v", err)
	}
	t.Cleanup(func() { _ = cc.Close() })
	id := strings.ReplaceAll(h.Project(), "-", "_") + "_" + suffix
	cn, err := cc.CreateConnection(ctx, &connectionpb.CreateConnectionRequest{Parent: "projects/" + project + "/locations/us", ConnectionId: id,
		Connection: &connectionpb.Connection{Properties: &connectionpb.Connection_CloudResource{CloudResource: &connectionpb.CloudResourceProperties{}}}})
	if err != nil {
		t.Fatalf("CreateConnection: %v", err)
	}
	t.Cleanup(func() { _ = cc.DeleteConnection(ctx, &connectionpb.DeleteConnectionRequest{Name: cn.Name}) })
	// The caller's bigquery.connections.use and the connection's service
	// account's Vertex AI User role (#1765).
	grantRemoteModel(t, h, cc, project, cn)
	if suffix == "gen" {
		// A connection of another kind, and one in another location, are
		// refused by CREATE MODEL (TestBigQueryMLRemoteGenerateText).
		for _, req := range []*connectionpb.CreateConnectionRequest{
			{Parent: "projects/" + project + "/locations/us", ConnectionId: id + "_sql",
				Connection: &connectionpb.Connection{Properties: &connectionpb.Connection_CloudSql{CloudSql: &connectionpb.CloudSqlProperties{
					InstanceId: project + ":us-central1:db", Database: "app", Type: connectionpb.CloudSqlProperties_POSTGRES}}}},
			{Parent: "projects/" + project + "/locations/eu", ConnectionId: id + "_eu",
				Connection: &connectionpb.Connection{Properties: &connectionpb.Connection_CloudResource{CloudResource: &connectionpb.CloudResourceProperties{}}}},
		} {
			o, err := cc.CreateConnection(ctx, req)
			if err != nil {
				t.Fatalf("CreateConnection %s: %v", req.ConnectionId, err)
			}
			t.Cleanup(func() { _ = cc.DeleteConnection(ctx, &connectionpb.DeleteConnectionRequest{Name: o.Name}) })
		}
	}
	ds := mlDataset(t, h, c, map[string]string{
		"prompts": "SELECT 1 AS id, 'What is the capital of France? Answer with one word.' AS prompt UNION ALL " +
			"SELECT 2, 'What is 2+2? Answer with a number.' UNION ALL SELECT 3, CAST(NULL AS STRING)",
		"docs": "SELECT 1 AS id, 'How do I bake sourdough bread?' AS content UNION ALL " +
			"SELECT 2, 'Mix flour, water and starter, let it rise, then bake.' UNION ALL SELECT 3, 'Interest rates rose by a quarter point.'",
	})
	return project + ".us." + id, ds
}

// TestBigQueryMLRemoteGenerateText (#1760): a remote model over a
// CLOUD_RESOURCE connection whose ENDPOINT is the instance's local
// generation model, through the official cloud.google.com/go/bigquery
// client: CREATE MODEL … REMOTE WITH CONNECTION checks the connection
// (a missing one 404, a Cloud SQL one and one in the EU 400; DEFAULT
// is TestBigQueryMLRemoteDefaultConnection), models.get answers remoteModelInfo (the
// generated bigquery/v2 client), ML.GENERATE_TEXT answers
// ml_generate_text_result and ml_generate_text_status, and with
// flatten_json_output ml_generate_text_llm_result, the input's columns
// kept and a NULL prompt left unanswered; a gemini-* endpoint with no
// alias is the local endpoint's NOT_FOUND in the row's status, never
// substituted; temperature is refused 501. The model's words are
// recorded, not asserted.
//
// covers: bigquery.jobs.query, bigquery.models.get
func TestBigQueryMLRemoteGenerateText(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvLocalAI)
	c, _ := bigqueryClient(t, h)
	conn, ds := remoteModelSetup(t, h, c, "gen")
	run := func(sql string) error { return mlRun(h, c, sql) }

	if err := run(fmt.Sprintf("CREATE MODEL `%s.nope` REMOTE WITH CONNECTION `%s` OPTIONS(ENDPOINT = '%s')", ds, conn+"_missing", localModel)); err == nil ||
		!strings.Contains(err.Error(), "Not found: Connection") {
		t.Errorf("a missing connection: %v", err)
	}
	if err := run(fmt.Sprintf("CREATE MODEL `%s.nope` REMOTE WITH CONNECTION `%s` OPTIONS(ENDPOINT = '%s')", ds, conn+"_sql", localModel)); err == nil ||
		!strings.Contains(err.Error(), "CLOUD_RESOURCE") {
		t.Errorf("a Cloud SQL connection: %v", err)
	}
	eu := strings.Replace(conn, ".us.", ".eu.", 1) + "_eu"
	if err := run(fmt.Sprintf("CREATE MODEL `%s.nope` REMOTE WITH CONNECTION `%s` OPTIONS(ENDPOINT = '%s')", ds, eu, localModel)); err == nil ||
		!strings.Contains(err.Error(), "differs from connection location") {
		t.Errorf("a connection in the EU: %v", err)
	}
	if err := run(fmt.Sprintf("CREATE MODEL `%s.gen` REMOTE WITH CONNECTION `%s` OPTIONS(ENDPOINT = '%s')", ds, conn, localModel)); err != nil {
		t.Fatalf("CREATE MODEL REMOTE: %v", err)
	}
	if err := run(fmt.Sprintf("CREATE MODEL `%s.gemini` REMOTE WITH CONNECTION `%s` OPTIONS(ENDPOINT = 'gemini-2.0-flash')", ds, conn)); err != nil {
		t.Fatalf("CREATE MODEL REMOTE gemini: %v", err)
	}
	svc := generatedBigQuery(t, h)
	md, err := svc.Models.Get(c.Project(), ds, "gen").Context(h.Context()).Do()
	if err != nil {
		t.Fatalf("models.get: %v", err)
	}
	if md.RemoteModelInfo == nil || md.RemoteModelInfo.Endpoint != localModel ||
		!strings.HasSuffix(md.RemoteModelInfo.Connection, "/locations/us/connections/"+conn[strings.LastIndex(conn, ".")+1:]) {
		t.Errorf("remoteModelInfo: %+v", md.RemoteModelInfo)
	}

	rows := mlQuery(t, h, c, fmt.Sprintf("SELECT * FROM ML.GENERATE_TEXT(MODEL `%s.gen`, TABLE `%s.prompts`, STRUCT(TRUE AS flatten_json_output)) ORDER BY id", ds, ds))
	if len(rows) != 3 {
		t.Fatalf("flattened rows: %v", rows)
	}
	for _, r := range rows[:2] {
		text, _ := r["ml_generate_text_llm_result"].(string)
		if r["ml_generate_text_status"] != "" || strings.TrimSpace(text) == "" || r["prompt"] == nil {
			t.Errorf("flattened row: %v", r)
		}
		t.Logf("model output (recorded, not asserted): %q", text)
	}
	if _, ok := rows[0]["ml_generate_text_rai_result"]; !ok {
		t.Errorf("no ml_generate_text_rai_result column: %v", rows[0])
	}
	if rows[2]["ml_generate_text_llm_result"] != nil {
		t.Errorf("a NULL prompt was answered: %v", rows[2])
	}

	rows = mlQuery(t, h, c, fmt.Sprintf("SELECT id, ml_generate_text_result, ml_generate_text_status FROM ML.GENERATE_TEXT(MODEL `%s.gen`, "+
		"(SELECT id, prompt FROM `%s.prompts` WHERE id = 2))", ds, ds))
	if len(rows) != 1 || rows[0]["ml_generate_text_status"] != "" {
		t.Fatalf("unflattened: %v", rows)
	}
	var full struct {
		Candidates []struct {
			Content struct {
				Parts []struct{ Text string } `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	raw, _ := rows[0]["ml_generate_text_result"].(string)
	if err := json.Unmarshal([]byte(raw), &full); err != nil || len(full.Candidates) == 0 || len(full.Candidates[0].Content.Parts) == 0 {
		t.Errorf("ml_generate_text_result is not a generateContent response: %q %v", raw, err)
	}

	rows = mlQuery(t, h, c, fmt.Sprintf("SELECT ml_generate_text_llm_result, ml_generate_text_status FROM ML.GENERATE_TEXT(MODEL `%s.gemini`, "+
		"(SELECT 'hi' AS prompt), STRUCT(TRUE AS flatten_json_output))", ds))
	if len(rows) != 1 || rows[0]["ml_generate_text_llm_result"] != nil || !strings.Contains(fmt.Sprint(rows[0]["ml_generate_text_status"]), "NOT_FOUND") {
		t.Errorf("a gemini endpoint with no alias: %v", rows)
	}
	if err := run(fmt.Sprintf("SELECT * FROM ML.GENERATE_TEXT(MODEL `%s.gen`, (SELECT 'x' AS prompt), STRUCT(0.2 AS temperature))", ds)); err == nil ||
		!strings.Contains(err.Error(), "temperature") {
		t.Errorf("temperature: %v", err)
	}
	if err := run(fmt.Sprintf("SELECT * FROM ML.GENERATE_EMBEDDING(MODEL `%s.gen`, (SELECT 'x' AS content))", ds)); err == nil {
		t.Error("ML.GENERATE_EMBEDDING of a generative model ran")
	}
	if err := run(fmt.Sprintf("SELECT * FROM ML.GENERATE_TEXT(MODEL `%s.gen`, (SELECT 'x' AS question))", ds)); err == nil ||
		!strings.Contains(err.Error(), "prompt") {
		t.Errorf("no prompt column: %v", err)
	}
}

// TestBigQueryMLRemoteGenerateEmbedding (#1760): a remote model whose
// ENDPOINT is the instance's local embedding model, through the official
// cloud.google.com/go/bigquery client: ML.GENERATE_EMBEDDING answers
// ml_generate_embedding_result (a unit vector of output_dimensionality),
// ml_generate_embedding_statistics (token_count) and
// ml_generate_embedding_status, the input's columns kept; the bread
// passage is nearer the bread question than the interest-rate one.
//
// covers: bigquery.jobs.query
func TestBigQueryMLRemoteGenerateEmbedding(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvEmbeddings)
	c, _ := bigqueryClient(t, h)
	conn, ds := remoteModelSetup(t, h, c, "emb")
	if err := mlRun(h, c, fmt.Sprintf("CREATE MODEL `%s.emb` REMOTE WITH CONNECTION `%s` OPTIONS(ENDPOINT = 'embeddinggemma-300m-onnx-community')", ds, conn)); err != nil {
		t.Fatalf("CREATE MODEL REMOTE: %v", err)
	}
	rows := mlQuery(t, h, c, fmt.Sprintf("SELECT * FROM ML.GENERATE_EMBEDDING(MODEL `%s.emb`, TABLE `%s.docs`, "+
		"STRUCT(TRUE AS flatten_json_output, 'RETRIEVAL_DOCUMENT' AS task_type, 256 AS output_dimensionality)) ORDER BY id", ds, ds))
	if len(rows) != 3 {
		t.Fatalf("rows: %v", rows)
	}
	var vecs [][]float64
	for _, r := range rows {
		vs, _ := r["ml_generate_embedding_result"].([]bigquery.Value)
		var v []float64
		norm := 0.0
		for _, x := range vs {
			f, _ := x.(float64)
			v = append(v, f)
			norm += f * f
		}
		if len(v) != 256 || math.Abs(norm-1) > 1e-3 || r["ml_generate_embedding_status"] != "" || r["content"] == nil {
			t.Errorf("row %v: %d values, norm %f, status %v", r["id"], len(v), norm, r["ml_generate_embedding_status"])
		}
		if s, _ := r["ml_generate_embedding_statistics"].(string); !strings.Contains(s, "token_count") {
			t.Errorf("statistics: %v", r["ml_generate_embedding_statistics"])
		}
		vecs = append(vecs, v)
	}
	if len(vecs) == 3 && len(vecs[0]) == 256 {
		dot := func(a, b []float64) float64 {
			s := 0.0
			for i := range a {
				s += a[i] * b[i]
			}
			return s
		}
		bread, rates := dot(vecs[0], vecs[1]), dot(vecs[0], vecs[2])
		t.Logf("bread %.4f, rates %.4f", bread, rates)
		if bread <= rates {
			t.Errorf("the bread passage (%.4f) is not nearer the bread question than the rates one (%.4f)", bread, rates)
		}
	}
	if err := mlRun(h, c, fmt.Sprintf("SELECT * FROM ML.GENERATE_TEXT(MODEL `%s.emb`, (SELECT 'x' AS prompt))", ds)); err == nil {
		t.Error("ML.GENERATE_TEXT of an embedding model ran")
	}
}
