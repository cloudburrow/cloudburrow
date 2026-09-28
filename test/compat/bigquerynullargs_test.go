//go:build compat

package compat

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
)

// TestBigQueryNullArgumentsOfArrayFunctions (#1109): in BigQuery "If an
// operand is NULL, the function result is NULL" (function call rules), and
// ARRAY_CONCAT "returns NULL if any input argument is NULL". So
// ARRAY_TO_STRING of a NULL array, delimiter or null_text is NULL, as are
// ARRAY_REVERSE and ARRAY_CONCAT of a NULL array and the NET functions of
// a NULL, through jobs.query and a query job, nested and with a
// parameter; a non-NULL call reads as before. JSON_OBJECT of a NULL key,
// an error in BigQuery too, is answered promptly with a 400 the client
// does not retry, never a 500. Measured first through the front with this
// client: SELECT ARRAY_TO_STRING(CAST(NULL AS ARRAY<STRING>), ',') was
// answered 500 internalError "runtime error: invalid memory address or nil
// pointer dereference", which the client retried until its deadline. The
// engine CloudBurrow builds returns NULL itself since #1121 (googlesqlite
// patch 0011); the front no longer rewrites these calls.
func TestBigQueryNullArgumentsOfArrayFunctions(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ctx := h.Context()
	one := func(sql string, insert bool, params ...bigquery.QueryParameter) (bigquery.Value, error) {
		qctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		q := c.Query(sql)
		q.Parameters = params
		var it *bigquery.RowIterator
		var err error
		if insert {
			var job *bigquery.Job
			if job, err = q.Run(qctx); err == nil {
				it, err = job.Read(qctx)
			}
		} else {
			it, err = q.Read(qctx)
		}
		if err != nil {
			return nil, err
		}
		var row []bigquery.Value
		if err := it.Next(&row); err != nil {
			return nil, err
		}
		if err := it.Next(&row); !errors.Is(err, iterator.Done) {
			return nil, fmt.Errorf("more than one row: %v", err)
		}
		return row[0], nil
	}
	for _, insert := range []bool{false, true} {
		for _, tc := range []struct {
			sql  string
			want bigquery.Value
		}{
			{"SELECT ARRAY_TO_STRING(CAST(NULL AS ARRAY<STRING>), ',')", nil},
			{"SELECT ARRAY_TO_STRING(['a', NULL, 'b'], CAST(NULL AS STRING))", nil},
			{"SELECT ARRAY_TO_STRING(['a', NULL, 'b'], '-', CAST(NULL AS STRING))", nil},
			{"SELECT SAFE.ARRAY_TO_STRING(CAST(NULL AS ARRAY<STRING>), ',')", nil},
			{"SELECT array_to_string ( ARRAY_REVERSE(CAST(NULL AS ARRAY<STRING>)), ',' )", nil},
			{"SELECT ARRAY_TO_STRING(ARRAY_REVERSE(['a', 'b']), ',', 'n')", "b,a"},
			{"SELECT ARRAY_TO_STRING(['a', NULL, 'b'], '-')", "a-b"},
			{"SELECT ARRAY_TO_STRING(['a', NULL, 'b'], '-', 'n')", "a-n-b"},
			{"SELECT ARRAY_REVERSE(CAST(NULL AS ARRAY<INT64>)) IS NULL", true},
			{"SELECT ARRAY_CONCAT([1], CAST(NULL AS ARRAY<INT64>)) IS NULL", true},
			{"SELECT ARRAY_LENGTH(ARRAY_CONCAT([1], [2, 3]))", int64(3)},
			{"SELECT NET.IPV4_TO_INT64(CAST(NULL AS BYTES)) IS NULL", true},
			{"SELECT NET.IP_TO_STRING(CAST(NULL AS BYTES)) IS NULL", true},
			{"SELECT NET.REG_DOMAIN(CAST(NULL AS STRING)) IS NULL", true},
			{"SELECT ARRAY_TO_STRING((SELECT ARRAY_AGG(x ORDER BY x) FROM UNNEST(['b', 'a']) x), '+')", "a+b"},
			{"SELECT 'ARRAY_TO_STRING(NULL, 1)'", "ARRAY_TO_STRING(NULL, 1)"},
		} {
			got, err := one(tc.sql, insert)
			if err != nil || got != tc.want {
				t.Errorf("%s (query job %v): %v %v, want %v", tc.sql, insert, got, err, tc.want)
			}
		}
		// A parameter.
		got, err := one("SELECT ARRAY_TO_STRING(@a, @d)", insert, bigquery.QueryParameter{Name: "a", Value: []string{"x", "y"}},
			bigquery.QueryParameter{Name: "d", Value: bigquery.NullString{}})
		if err != nil || got != nil {
			t.Errorf("ARRAY_TO_STRING of a NULL parameter (query job %v): %v %v", insert, got, err)
		}
		// JSON_OBJECT of a NULL key, an error in BigQuery too: a prompt
		// 400, never a 500.
		start := time.Now()
		_, err = one("SELECT JSON_OBJECT(CAST(NULL AS STRING), 1)", insert)
		var ge *googleapi.Error
		if err == nil || errors.As(err, &ge) && ge.Code >= 500 || time.Since(start) > 15*time.Second {
			t.Errorf("JSON_OBJECT of a NULL key (query job %v): %v after %v, want a prompt 400", insert, err, time.Since(start))
		}
	}
}
