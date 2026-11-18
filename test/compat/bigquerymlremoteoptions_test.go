//go:build compat

package compat

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
)

// TestBigQueryMLRemoteGenerationOptions (#1797): ML.GENERATE_TEXT's and
// AI.GENERATE_TEXT's generation options reach the local runtime and change
// its output, through the official cloud.google.com/go/bigquery client:
// max_output_tokens 8 caps the answer that 256 lets run on; temperature 0
// (BigQuery's default) answers the same prompt identically twice;
// stop_sequences end the answer before the sequence; out-of-range values
// (max_output_tokens 0, top_p 1.5, a top_k of 41) are refused 400, and
// top_k of a Gemini endpoint is refused.
//
// Constrained decoding (#1779): AI.GENERATE_INT asked to describe a
// spider at length still answers with nothing but an integer (the model's
// raw text, in full_response, parses whole as one), because the front sends
// the type as a responseSchema and the runtime constrains the decode to it.
//
// covers: bigquery.jobs.query
func TestBigQueryMLRemoteGenerationOptions(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvLocalAI)
	c, _ := bigqueryClient(t, h)
	conn, ds := remoteModelSetup(t, h, c, "genopt")
	run := func(sql string) error { return mlRun(h, c, sql) }
	if err := run(fmt.Sprintf("CREATE MODEL `%s.gen` REMOTE WITH CONNECTION `%s` OPTIONS(ENDPOINT = '%s')", ds, conn, localModel)); err != nil {
		t.Fatalf("CREATE MODEL REMOTE: %v", err)
	}
	text := func(sql string) string {
		t.Helper()
		rows := mlQuery(t, h, c, sql)
		if len(rows) != 1 {
			t.Fatalf("%s: %d rows", sql, len(rows))
		}
		if st, _ := rows[0]["ml_generate_text_status"].(string); st != "" {
			t.Fatalf("%s: status %q", sql, st)
		}
		s, _ := rows[0]["ml_generate_text_llm_result"].(string)
		return s
	}
	const essay = "(SELECT 'Write a 300-word essay about the sea.' AS prompt)"
	short := text(fmt.Sprintf("SELECT ml_generate_text_llm_result, ml_generate_text_status FROM ML.GENERATE_TEXT(MODEL `%s.gen`, %s, "+
		"STRUCT(TRUE AS flatten_json_output, 8 AS max_output_tokens))", ds, essay))
	long := text(fmt.Sprintf("SELECT ml_generate_text_llm_result, ml_generate_text_status FROM ML.GENERATE_TEXT(MODEL `%s.gen`, %s, "+
		"STRUCT(TRUE AS flatten_json_output, 256 AS max_output_tokens))", ds, essay))
	t.Logf("8 tokens: %q; 256 tokens: %d words (recorded)", short, len(strings.Fields(long)))
	if n := len(strings.Fields(short)); n == 0 || n > 10 || len(strings.Fields(long)) <= 3*n {
		t.Errorf("max_output_tokens 8 gave %d words, 256 gave %d", n, len(strings.Fields(long)))
	}

	const colour = "(SELECT 'Invent a name for a new colour. Answer with the name only.' AS prompt)"
	q := fmt.Sprintf("SELECT ml_generate_text_llm_result, ml_generate_text_status FROM ML.GENERATE_TEXT(MODEL `%s.gen`, %s, "+
		"STRUCT(TRUE AS flatten_json_output, 0.0 AS temperature, 24 AS max_output_tokens))", ds, colour)
	if a, b := text(q), text(q); a != b {
		t.Errorf("temperature 0 answered %q, then %q", a, b)
	}

	counted := text(fmt.Sprintf("SELECT ml_generate_text_llm_result, ml_generate_text_status FROM ML.GENERATE_TEXT(MODEL `%s.gen`, "+
		"(SELECT 'Count from 1 to 10, separated by commas. Answer with the numbers only.' AS prompt), "+
		"STRUCT(TRUE AS flatten_json_output, ['5'] AS stop_sequences))", ds))
	t.Logf("stop at 5 (recorded): %q", counted)
	if strings.Contains(counted, "5") || !strings.Contains(counted, "4") {
		t.Errorf("stop_sequences ['5']: %q", counted)
	}

	rows := mlQuery(t, h, c, fmt.Sprintf("SELECT result, status FROM AI.GENERATE_TEXT(MODEL `%s.gen`, %s, STRUCT(6 AS max_output_tokens, 0.9 AS top_p))", ds, essay))
	if len(rows) != 1 || rows[0]["status"] != "" || len(strings.Fields(fmt.Sprint(rows[0]["result"]))) > 8 {
		t.Errorf("AI.GENERATE_TEXT with max_output_tokens 6: %v", rows)
	}

	rows = mlQuery(t, h, c, fmt.Sprintf("SELECT AI.GENERATE_INT('Describe a spider in detail, then say how many legs it has.', endpoint => '%s', connection_id => '%s').full_response AS fr", localModel, conn))
	var fr struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	if len(rows) != 1 || json.Unmarshal([]byte(fmt.Sprint(rows[0]["fr"])), &fr) != nil || len(fr.Candidates) != 1 || len(fr.Candidates[0].Content.Parts) == 0 {
		t.Fatalf("AI.GENERATE_INT full_response: %v", rows)
	}
	raw := strings.TrimSpace(fr.Candidates[0].Content.Parts[0].Text)
	if _, err := strconv.ParseInt(raw, 10, 64); err != nil {
		t.Errorf("AI.GENERATE_INT's raw answer is not just an integer: %q", raw)
	}
	t.Logf("AI.GENERATE_INT raw answer (recorded): %q", raw)

	for opt, want := range map[string]string{
		"0 AS max_output_tokens": "max_output_tokens", "1.5 AS top_p": "top_p", "41 AS top_k": "top_k", "-0.5 AS temperature": "temperature",
	} {
		err := run(fmt.Sprintf("SELECT * FROM ML.GENERATE_TEXT(MODEL `%s.gen`, (SELECT 'x' AS prompt), STRUCT(%s))", ds, opt))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", opt, err)
		}
	}
	if err := run(fmt.Sprintf("CREATE MODEL `%s.gemini` REMOTE WITH CONNECTION `%s` OPTIONS(ENDPOINT = 'gemini-2.0-flash')", ds, conn)); err != nil {
		t.Fatalf("CREATE MODEL gemini: %v", err)
	}
	if err := run(fmt.Sprintf("SELECT * FROM ML.GENERATE_TEXT(MODEL `%s.gemini`, (SELECT 'x' AS prompt), STRUCT(4 AS top_k))", ds)); err == nil ||
		!strings.Contains(err.Error(), "top_k") {
		t.Errorf("top_k of a Gemini endpoint: %v", err)
	}
}
