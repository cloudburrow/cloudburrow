//go:build compat

package compat

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"
	"google.golang.org/api/iterator"
)

// TestBigQueryAIAgg (#1777): AI.AGG through the official
// cloud.google.com/go/bigquery client on the instance's local generation
// model: one STRING per GROUP BY group, one value without GROUP BY, a
// STRUCT with output_schema, DISTINCT, NULL inputs ignored, the
// a group larger than one call's
// context batched and its batches' answers combined, the error ratio when
// every call fails, an instruction given as a named or a positional query
// parameter (#1810), and the calls refused before anything runs. The
// model's words are recorded, not asserted.
//
// covers: bigquery.jobs.query
func TestBigQueryAIAgg(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvLocalAI)
	c, _ := bigqueryClient(t, h)
	_, project := remoteConnectionClient(t, h)
	grantProject(t, h, remoteProjectsClient(t, h), project, "roles/aiplatform.user", callerMember(project))
	ds := mlDataset(t, h, c, map[string]string{
		"reviews": "SELECT 'Alpha' AS movie, 'A wonderful film, I loved every minute.' AS review UNION ALL " +
			"SELECT 'Alpha', 'Beautiful and moving, the best film this year.' UNION ALL " +
			"SELECT 'Alpha', CAST(NULL AS STRING) UNION ALL " +
			"SELECT 'Beta', 'Boring and far too long.' UNION ALL " +
			"SELECT 'Beta', 'Terrible acting; I walked out.' UNION ALL " +
			"SELECT 'Beta', 'Terrible acting; I walked out.'",
	})

	rows := aiRows(t, h, c, fmt.Sprintf("SELECT movie, AI.AGG(review, 'In one short sentence, what is the overall sentiment of these movie reviews?', "+
		"endpoint => '%s') AS sentiment, COUNT(*) AS n FROM `%s.reviews` GROUP BY movie ORDER BY movie", localModel, ds))
	if len(rows) != 2 || rows[0]["movie"] != "Alpha" || rows[1]["movie"] != "Beta" || rows[0]["n"] != int64(3) {
		t.Fatalf("GROUP BY: %v", rows)
	}
	for _, r := range rows {
		if s, ok := r["sentiment"].(string); !ok || strings.TrimSpace(s) == "" {
			t.Errorf("AI.AGG of %v is not a STRING: %#v", r["movie"], r["sentiment"])
		}
	}
	t.Logf("GROUP BY (model output recorded, not asserted): %v", rows)

	// Without GROUP BY, one value; with GROUP BY 1 and an alias, the same
	// groups; DISTINCT reads each review once.
	rows = aiRows(t, h, c, fmt.Sprintf("SELECT AI.AGG(DISTINCT review, 'List the movie reviews below, one per line.', endpoint => '%s') AS s "+
		"FROM `%s.reviews`", localModel, ds))
	if len(rows) != 1 || rows[0]["s"] == nil {
		t.Fatalf("no GROUP BY: %v", rows)
	}
	t.Logf("DISTINCT, no GROUP BY (recorded): %v", rows[0]["s"])
	rows = aiRows(t, h, c, fmt.Sprintf("SELECT movie AS m, AI.AGG(review, 'Summarize.', endpoint => '%s') IS NOT NULL AS has FROM `%s.reviews` "+
		"GROUP BY 1 HAVING m = 'Beta'", localModel, ds))
	if len(rows) != 1 || rows[0]["m"] != "Beta" || rows[0]["has"] != true {
		t.Errorf("GROUP BY 1 with HAVING: %v", rows)
	}

	// output_schema: a STRUCT of the schema's fields.
	rows = aiRows(t, h, c, fmt.Sprintf("SELECT movie, a.positive, a.summary FROM (SELECT movie, AI.AGG(review, "+
		"'Is the overall opinion of these reviews positive? Also give a short summary.', endpoint => '%s', "+
		"output_schema => 'positive BOOL NOT NULL, summary STRING NOT NULL') AS a FROM `%s.reviews` GROUP BY movie) ORDER BY movie", localModel, ds))
	if len(rows) != 2 {
		t.Fatalf("output_schema: %v", rows)
	}
	for _, r := range rows {
		if _, ok := r["positive"].(bool); !ok {
			t.Errorf("output_schema positive is not a BOOL: %v", r)
		}
		if _, ok := r["summary"].(string); !ok {
			t.Errorf("output_schema summary is not a STRING: %v", r)
		}
	}
	t.Logf("output_schema (recorded): %v", rows)

	// A group larger than one call's context (about 5,600 bytes locally):
	// batched, each batch answered, the answers combined.
	rows = aiRows(t, h, c, fmt.Sprintf("SELECT AI.AGG(line, 'How many of these lines mention a fruit? Answer with a number only.', endpoint => '%s') AS s "+
		"FROM UNNEST(ARRAY(SELECT CONCAT('Line ', CAST(n AS STRING), ': ', IF(MOD(n, 2) = 0, 'I ate an apple and a pear today with my friends at the park.', "+
		"'The weather was cloudy and the bus arrived late again this morning.')) FROM UNNEST(GENERATE_ARRAY(1, 160)) AS n)) AS line", localModel))
	if len(rows) != 1 || rows[0]["s"] == nil {
		t.Errorf("a batched group: %v", rows)
	}
	t.Logf("batched group (recorded): %v", rows)

	// The instruction as a query parameter, "as either a string or query
	// parameter" (#1810): named, beside another named parameter the query
	// uses elsewhere, and positional, with one before and one after it. The
	// job shows the client's own query and parameters.
	named := fmt.Sprintf("SELECT movie, AI.AGG(review, @instruction, endpoint => '%s') AS s FROM `%s.reviews` WHERE movie = @movie GROUP BY movie", localModel, ds)
	rows, job := aiParamRows(t, h, c, named, []bigquery.QueryParameter{{Name: "instruction", Value: "In one short sentence, summarize these movie reviews."},
		{Name: "movie", Value: "Beta"}})
	if len(rows) != 1 || rows[0]["movie"] != "Beta" {
		t.Fatalf("named parameter: %v", rows)
	}
	if s, ok := rows[0]["s"].(string); !ok || strings.TrimSpace(s) == "" {
		t.Errorf("named parameter: AI.AGG is not a STRING: %#v", rows[0]["s"])
	}
	if cfg, ok := job.(*bigquery.QueryConfig); !ok || cfg.Q != named || len(cfg.Parameters) != 2 {
		t.Errorf("named parameter: the job's configuration %#v", job)
	}
	t.Logf("named parameter (recorded): %v", rows)
	positional := fmt.Sprintf("SELECT movie, AI.AGG(CONCAT(?, review), ?, endpoint => '%s') AS s FROM `%s.reviews` WHERE movie = ? GROUP BY movie", localModel, ds)
	rows, job = aiParamRows(t, h, c, positional, []bigquery.QueryParameter{{Value: "Review: "}, {Value: "In one short sentence, summarize these movie reviews."},
		{Value: "Alpha"}})
	if len(rows) != 1 || rows[0]["movie"] != "Alpha" {
		t.Fatalf("positional parameters: %v", rows)
	}
	if s, ok := rows[0]["s"].(string); !ok || strings.TrimSpace(s) == "" {
		t.Errorf("positional parameters: AI.AGG is not a STRING: %#v", rows[0]["s"])
	}
	if cfg, ok := job.(*bigquery.QueryConfig); !ok || cfg.Q != positional || len(cfg.Parameters) != 3 || cfg.Parameters[0].Name != "" {
		t.Errorf("positional parameters: the job's configuration %#v", job)
	}
	t.Logf("positional parameters (recorded): %v", rows)
	for _, tc := range []struct {
		value any
		want  string
	}{{int64(3), "must be a STRING"}, {bigquery.NullString{}, "NULL"}} {
		q := c.Query(fmt.Sprintf("SELECT AI.AGG(review, @instruction, endpoint => '%s') FROM `%s.reviews`", localModel, ds))
		q.Parameters = []bigquery.QueryParameter{{Name: "instruction", Value: tc.value}}
		if _, err := q.Read(h.Context()); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("instruction parameter %#v: %v, want %q", tc.value, err, tc.want)
		}
	}

	for sql, want := range map[string]string{
		// gemini-3.5-flash-lite has no alias locally: every call fails.
		fmt.Sprintf("SELECT AI.AGG(review, 'Summarize.') FROM `%s.reviews`", ds):                                                  "exceeded their allowed error ratio",
		fmt.Sprintf("SELECT AI.AGG(review) FROM `%s.reviews`", ds):                                                                "instruction",
		fmt.Sprintf("SELECT AI.AGG(review, 'x', endpoint => '%s', model_params => JSON '{}') FROM `%s.reviews`", localModel, ds):  "Unknown argument model_params",
		fmt.Sprintf("SELECT AI.AGG(review, 'x', endpoint => '%s') OVER () FROM `%s.reviews`", localModel, ds):                     "501",
		fmt.Sprintf("SELECT movie FROM `%s.reviews` WHERE AI.AGG(review, 'x', endpoint => '%s') = 'a'", ds, localModel):           "not allowed in WHERE",
		fmt.Sprintf("SELECT AI.AGG(review, 'x', endpoint => '%s', output_schema => 'a DATE') FROM `%s.reviews`", localModel, ds):  "output_schema",
		fmt.Sprintf("SELECT AI.AGG(review, 'x', endpoint => '%s', max_error_ratio => 2.0) FROM `%s.reviews`", localModel, ds):     "max_error_ratio",
		fmt.Sprintf("SELECT AI.AGG(review, CAST(NULL AS STRING), endpoint => '%s') FROM `%s.reviews`", localModel, ds):            "NULL",
		fmt.Sprintf("SELECT AI.AGG(review, 'x', endpoint => '%s', connection_id => 'us.nope') FROM `%s.reviews`", localModel, ds): "Not found: Connection",
	} {
		if err := mlRun(h, c, sql); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", sql, err, want)
		}
	}
}

// aiParamRows runs sql with params and returns its rows and the job's
// configuration as jobs.get shows it.
func aiParamRows(t *testing.T, h *Harness, c *bigquery.Client, sql string, params []bigquery.QueryParameter) ([]map[string]bigquery.Value, bigquery.JobConfig) {
	t.Helper()
	q := c.Query(sql)
	q.Parameters = params
	job, err := q.Run(h.Context())
	if err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	it, err := job.Read(h.Context())
	if err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	var out []map[string]bigquery.Value
	for {
		row := map[string]bigquery.Value{}
		err := it.Next(&row)
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		out = append(out, row)
	}
	got, err := c.JobFromID(h.Context(), job.ID())
	if err != nil {
		t.Fatalf("jobs.get %s: %v", job.ID(), err)
	}
	cfg, err := got.Config()
	if err != nil {
		t.Fatalf("job config: %v", err)
	}
	return out, cfg
}
