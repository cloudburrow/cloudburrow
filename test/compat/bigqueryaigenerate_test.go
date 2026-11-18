//go:build compat

package compat

import (
	"fmt"
	"strings"
	"testing"

	"cloud.google.com/go/bigquery/connection/apiv1/connectionpb"
)

// TestBigQueryAIGenerate (#1765): AI.GENERATE through the official
// cloud.google.com/go/bigquery client, evaluated row by row on the
// instance's local generation model: a STRING prompt column and a STRUCT
// prompt (its fields concatenated) answer STRUCT<result, full_response,
// status>, a NULL prompt's struct is NULL, the default endpoint
// (gemini-3.5-flash-lite, no alias) is the local endpoint's NOT_FOUND in
// status with a NULL result, never substituted; without connection_id the
// caller needs the Vertex AI User role (403 without it), and with one the
// connection's permissions (#1765); output_schema and model_params are
// 501. The model's words are recorded, not asserted.
//
// covers: bigquery.jobs.query
func TestBigQueryAIGenerate(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvLocalAI)
	c, _ := bigqueryClient(t, h)
	cc, project := remoteConnectionClient(t, h)
	pc := remoteProjectsClient(t, h)
	ds := mlDataset(t, h, c, map[string]string{
		"prompts": "SELECT 1 AS id, 'What is the capital of France? Answer with one word.' AS prompt, 'France' AS country UNION ALL " +
			"SELECT 2, 'What is 2+2? Answer with a number.', 'Italy' UNION ALL SELECT 3, CAST(NULL AS STRING), 'Spain'",
	})
	q := fmt.Sprintf("SELECT id, AI.GENERATE(prompt, endpoint => '%s') AS g FROM `%s.prompts` ORDER BY id", localModel, ds)
	if err := mlRun(h, c, q); err == nil || !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "aiplatform") {
		t.Errorf("no Vertex AI User role: %v", err)
	}
	grantProject(t, h, pc, project, "roles/aiplatform.user", callerMember(project))

	rows := mlQuery(t, h, c, fmt.Sprintf("SELECT id, AI.GENERATE(prompt, endpoint => '%s').result AS result, "+
		"AI.GENERATE(prompt, endpoint => '%s').status AS status FROM `%s.prompts` ORDER BY id", localModel, localModel, ds))
	if len(rows) != 3 {
		t.Fatalf("rows: %v", rows)
	}
	for _, r := range rows[:2] {
		text, _ := r["result"].(string)
		if r["status"] != "" || strings.TrimSpace(text) == "" {
			t.Errorf("row: %v", r)
		}
		t.Logf("model output (recorded, not asserted): %q", text)
	}
	if rows[2]["result"] != nil {
		t.Errorf("a NULL prompt was answered: %v", rows[2])
	}

	rows = mlQuery(t, h, c, fmt.Sprintf("SELECT g.result, g.status, JSON_VALUE(g.full_response, '$.candidates[0].content.parts[0].text') AS text FROM "+
		"(SELECT AI.GENERATE(('What is the capital of ', country, '? Answer with one word.'), endpoint => '%s') AS g FROM `%s.prompts` WHERE id = 1)", localModel, ds))
	if len(rows) != 1 || rows[0]["status"] != "" || rows[0]["result"] == nil || rows[0]["text"] != rows[0]["result"] {
		t.Errorf("a STRUCT prompt: %v", rows)
	}
	t.Logf("model output (recorded, not asserted): %v", rows)

	rows = mlQuery(t, h, c, "SELECT AI.GENERATE('hi').result AS result, AI.GENERATE('hi').status AS status")
	if len(rows) != 1 || rows[0]["result"] != nil || !strings.Contains(fmt.Sprint(rows[0]["status"]), "NOT_FOUND") {
		t.Errorf("the default endpoint with no alias: %v", rows)
	}
	for arg, want := range map[string]string{"output_schema => 'a STRING'": "output_schema", `model_params => JSON '{"generationConfig":{"temperature":0}}'`: "model_params"} {
		if err := mlRun(h, c, fmt.Sprintf("SELECT AI.GENERATE('hi', endpoint => '%s', %s)", localModel, arg)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", arg, err)
		}
	}

	// With connection_id: the connection's permissions.
	id := strings.ReplaceAll(h.Project(), "-", "_") + "_aigen"
	cn, err := cc.CreateConnection(h.Context(), &connectionpb.CreateConnectionRequest{Parent: "projects/" + project + "/locations/us", ConnectionId: id,
		Connection: &connectionpb.Connection{Properties: &connectionpb.Connection_CloudResource{CloudResource: &connectionpb.CloudResourceProperties{}}}})
	if err != nil {
		t.Fatalf("CreateConnection: %v", err)
	}
	t.Cleanup(func() { _ = cc.DeleteConnection(h.Context(), &connectionpb.DeleteConnectionRequest{Name: cn.Name}) })
	cq := fmt.Sprintf("SELECT AI.GENERATE('What is 2+2? Answer with a number.', endpoint => '%s', connection_id => 'us.%s').result AS result", localModel, id)
	if err := mlRun(h, c, cq); err == nil || !strings.Contains(err.Error(), "bigquery.connections.use") {
		t.Errorf("a connection the caller may not use: %v", err)
	}
	grantRemoteModel(t, h, cc, project, cn)
	rows = mlQuery(t, h, c, cq)
	if len(rows) != 1 || rows[0]["result"] == nil {
		t.Errorf("with connection_id: %v", rows)
	}
	t.Logf("model output (recorded, not asserted): %v", rows)
}
