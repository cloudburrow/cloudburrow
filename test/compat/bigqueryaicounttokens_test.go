//go:build compat

package compat

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"google.golang.org/genai"
)

// TestBigQueryAICountTokens (#1962): AI.COUNT_TOKENS through the official
// cloud.google.com/go/bigquery client, as
// https://docs.cloud.google.com/bigquery/docs/reference/standard-sql/bigqueryml-syntax-ai-count-tokens
// documents it (read 2026-11-19): AI.COUNT_TOKENS(INPUT [, endpoint =>
// ENDPOINT]) answers STRUCT<result INT64, full_response JSON>, the
// countTokens response ({"promptTokensDetails":[{"modality":"TEXT",
// "tokenCount":N}],"totalTokens":N}) with its totalTokens as result, counted
// by the instance's local model's tokenizer and equal to the official genai
// SDK's Models.CountTokens of the same text; a NULL input, and an API error
// (the default endpoint with no alias is the local endpoint's NOT_FOUND),
// are NULL in both fields; the caller needs the Vertex AI User role
// (UNVERIFIED, docs re-read 2026-11-19, still unanswered: the page above
// names no permission; AI.GENERATE's is applied).
//
// covers: bigquery.jobs.query
func TestBigQueryAICountTokens(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvLocalAI)
	c, _ := bigqueryClient(t, h)
	_, project := remoteConnectionClient(t, h)
	pc := remoteProjectsClient(t, h)
	ds := mlDataset(t, h, c, map[string]string{
		"reviews": "SELECT 1 AS id, \"Token count isn't always equal to word count.\" AS review UNION ALL " +
			"SELECT 2, 'A much longer review: the plot wandered, the acting was uneven, but the music carried every scene to the end.' UNION ALL " +
			"SELECT 3, CAST(NULL AS STRING)",
	})
	q := fmt.Sprintf("SELECT id, AI.COUNT_TOKENS(review, endpoint => '%s').* FROM `%s.reviews` ORDER BY id", localModel, ds)
	if err := mlRun(h, c, q); err == nil || !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "aiplatform") {
		t.Errorf("no Vertex AI User role: %v", err)
	}
	grantProject(t, h, pc, project, "roles/aiplatform.user", callerMember(project))

	rows := mlQuery(t, h, c, q)
	if len(rows) != 3 {
		t.Fatalf("rows: %v", rows)
	}
	counts := make([]int64, 2)
	for i, r := range rows[:2] {
		n, ok := r["result"].(int64)
		var full struct {
			TotalTokens         int64 `json:"totalTokens"`
			PromptTokensDetails []struct {
				Modality   string `json:"modality"`
				TokenCount int64  `json:"tokenCount"`
			} `json:"promptTokensDetails"`
		}
		raw, _ := r["full_response"].(string)
		if !ok || n <= 0 || json.Unmarshal([]byte(raw), &full) != nil || full.TotalTokens != n ||
			len(full.PromptTokensDetails) != 1 || full.PromptTokensDetails[0].Modality != "TEXT" || full.PromptTokensDetails[0].TokenCount != n {
			t.Errorf("row %d: %v", i+1, r)
		}
		counts[i] = n
	}
	if counts[1] <= counts[0] {
		t.Errorf("the longer review counted %d tokens, the shorter %d", counts[1], counts[0])
	}
	if rows[2]["result"] != nil || rows[2]["full_response"] != nil {
		t.Errorf("a NULL input was counted: %v", rows[2])
	}

	// The same count the official genai SDK's countTokens gives.
	cl := genaiClient(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	ct, err := cl.Models.CountTokens(ctx, localModel, genai.Text("Token count isn't always equal to word count."), nil)
	if err != nil {
		t.Fatalf("CountTokens: %v", err)
	}
	if int64(ct.TotalTokens) != counts[0] {
		t.Errorf("AI.COUNT_TOKENS %d, the SDK's CountTokens %d", counts[0], ct.TotalTokens)
	}
	t.Logf("token counts (recorded): %v", counts)

	// The default endpoint (AI.GENERATE's, no alias): an API error, NULL.
	rows = mlQuery(t, h, c, "SELECT AI.COUNT_TOKENS('hi').result AS result, AI.COUNT_TOKENS('hi').full_response AS full_resp")
	if len(rows) != 1 || rows[0]["result"] != nil || rows[0]["full_resp"] != nil {
		t.Errorf("the default endpoint with no alias: %v", rows)
	}
	// INPUT is a STRING.
	if err := mlRun(h, c, fmt.Sprintf("SELECT AI.COUNT_TOKENS(('a', 'b'), endpoint => '%s')", localModel)); err == nil || !strings.Contains(err.Error(), "STRING") {
		t.Errorf("a STRUCT input: %v", err)
	}
}

// TestBigQueryAICountTokensArguments (#1962): AI.COUNT_TOKENS' arguments,
// checked by the BigQuery front before any model is asked, so this needs
// no local model: INPUT is required and positional, endpoint is named, and
// the page documents no other argument
// (UNVERIFIED, docs re-read 2026-11-19, still unanswered:
// https://docs.cloud.google.com/bigquery/docs/reference/standard-sql/bigqueryml-syntax-ai-count-tokens:
// the errors' wording, which the page does not quote).
//
// covers: bigquery.jobs.query
func TestBigQueryAICountTokensArguments(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	for sql, want := range map[string]string{
		"SELECT AI.COUNT_TOKENS(endpoint => 'gemini-2.5-flash')":         "requires its input argument",
		"SELECT AI.COUNT_TOKENS('hi', connection_id => 'us.c')":          "Unknown argument connection_id",
		"SELECT AI.COUNT_TOKENS('hi', 'gemini-2.5-flash')":               "positional",
		"SELECT AI.COUNT_TOKENS('hi', endpoint => 'a', endpoint => 'b')": "twice",
		"SELECT AI.COUNT_TOKENS('hi', model_params => JSON '{}')":        "Unknown argument model_params",
	} {
		if err := mlRun(h, c, sql); err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "400") {
			t.Errorf("%s: %v; want 400 naming %q", sql, err, want)
		}
	}
}
