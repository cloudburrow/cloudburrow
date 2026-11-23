package bigqueryfront

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestContinuousQuerySQL (#2193): the statements and operators continuous
// queries support and refuse, as continuous-queries-introduction lists
// them.
func TestContinuousQuerySQL(t *testing.T) {
	ok := []string{
		"INSERT INTO d.out (a) SELECT a FROM d.src WHERE a > 0",
		"EXPORT DATA OPTIONS(format = 'CLOUD_PUBSUB', uri = 'https://pubsub.googleapis.com/projects/p/topics/t') AS (SELECT TO_JSON_STRING(STRUCT(a)) AS message FROM d.src)",
		"CREATE TEMP FUNCTION f(x INT64) AS (x * 2); INSERT INTO d.out SELECT f(a) FROM d.src",
		"INSERT INTO d.out SELECT a FROM d.s1 UNION ALL SELECT a FROM d.s2",
		"INSERT INTO d.out SELECT a FROM APPENDS(TABLE d.src, CURRENT_TIMESTAMP() - INTERVAL 10 MINUTE)",
		"INSERT INTO `p.d.out` SELECT a FROM APPENDS(TABLE `p.d.src`, CURRENT_TIMESTAMP() - INTERVAL 10 MINUTE) WHERE a > 0",
		"INSERT INTO d.out SELECT * EXCEPT (b) FROM d.src",
		"INSERT INTO d.out SELECT x FROM d.src, UNNEST(arr) AS x",
		"INSERT INTO d.out SELECT EXTRACT(YEAR FROM ts) FROM d.src",
		"EXPORT DATA OPTIONS(format = 'CLOUD_PUBSUB', uri = 'https://pubsub.googleapis.com/projects/p/topics/t') AS (SELECT CAST(a AS STRING) AS m FROM CHANGES(TABLE d.src, CURRENT_TIMESTAMP() - INTERVAL 1 HOUR))",
	}
	for _, q := range ok {
		if _, err := parseContinuous(q); err != nil {
			t.Errorf("%s: %v, want it accepted", q, err)
		}
	}
	refused := map[string]int{
		"SELECT 1":                                       400,
		"UPDATE d.t SET a = 1 WHERE true":                400,
		"CREATE TABLE d.x (a INT64)":                     400,
		"INSERT INTO d.out SELECT DISTINCT a FROM d.src": 400,
		"INSERT INTO d.out SELECT a FROM d.src PIVOT (SUM(b) FOR c IN ('x'))":                                                                                                 400,
		"INSERT INTO d.out SELECT a FROM d.src TABLESAMPLE SYSTEM (10 PERCENT)":                                                                                               400,
		"INSERT INTO d.out SELECT a FROM d.s1 UNION DISTINCT SELECT a FROM d.s2":                                                                                              400,
		"INSERT INTO d.out SELECT a FROM d.s1 INTERSECT DISTINCT SELECT a FROM d.s2":                                                                                          400,
		"INSERT INTO d.out SELECT a FROM d.src WHERE EXISTS (SELECT 1)":                                                                                                       400,
		"INSERT INTO d.out WITH RECURSIVE r AS (SELECT 1 AS a) SELECT a FROM r":                                                                                               400,
		"INSERT INTO d.out SELECT ROW_NUMBER() OVER () FROM d.src":                                                                                                            400,
		"CREATE TEMP FUNCTION f(x INT64) RETURNS INT64 LANGUAGE js AS 'return x'; INSERT INTO d.out SELECT f(a) FROM d.src":                                                   400,
		"INSERT INTO d.out SELECT * FROM d.INFORMATION_SCHEMA.TABLES":                                                                                                         400,
		"INSERT INTO d.out SELECT * FROM `d.src*`":                                                                                                                            400,
		"INSERT INTO d.out SELECT * FROM ML.PREDICT(MODEL d.m, TABLE d.src)":                                                                                                  400,
		"INSERT INTO d.out SELECT a FROM APPENDS(TABLE d.src, NULL, CURRENT_TIMESTAMP())":                                                                                     400,
		"EXPORT DATA OPTIONS(format = 'CSV', uri = 'gs://b/*.csv') AS SELECT a FROM d.src":                                                                                    400,
		"EXPORT DATA OPTIONS(format = 'CLOUD_BIGTABLE', uri = 'https://bigtable.googleapis.com/projects/p/instances/i/tables/t') AS SELECT * FROM CHANGES(TABLE d.src, NULL)": 400,
		"INSERT INTO d.out VALUES (1)":                                                                                                                                        400,
		"INSERT INTO d.out SELECT a.x FROM d.src a JOIN d.dim b ON a.k = b.k":                                                                                                 501,
		"INSERT INTO d.out SELECT k, COUNT(*) FROM d.src GROUP BY k":                                                                                                          501,
	}
	for q, code := range refused {
		_, err := parseContinuous(q)
		e, isRefusal := err.(*cqRefusal)
		if !isRefusal || e.code != code {
			t.Errorf("%s: %v, want refused %d", q, err, code)
		}
	}
}

// TestContinuousQueryWindow (#2193): each run reads its tables' rows
// appended in its window, the tables' columns only; an APPENDS the query
// calls takes its own start in the first window.
func TestContinuousQueryWindow(t *testing.T) {
	p, err := parseContinuous("INSERT INTO d.out SELECT a FROM src AS s WHERE a > 0")
	if err != nil {
		t.Fatal(err)
	}
	got := p.statement(cqWindow{low: 1000, high: 2000}, "d")
	want := "INSERT INTO d.out SELECT a FROM (SELECT * EXCEPT (_CHANGE_TYPE, _CHANGE_TIMESTAMP) FROM APPENDS(TABLE `d.src`, " +
		"TIMESTAMP_MILLIS(1000), TIMESTAMP_MILLIS(2000))) AS s WHERE a > 0"
	if got != want {
		t.Errorf("window:\n got %s\nwant %s", got, want)
	}
	p, err = parseContinuous("INSERT INTO d.out SELECT a, _CHANGE_TIMESTAMP FROM APPENDS(TABLE d.src, CURRENT_TIMESTAMP() - INTERVAL 1 HOUR)")
	if err != nil {
		t.Fatal(err)
	}
	if got := p.statement(cqWindow{low: 1000, high: 2000, first: true}, ""); !strings.Contains(got,
		"APPENDS(TABLE `d.src`, (CURRENT_TIMESTAMP() - INTERVAL 1 HOUR), TIMESTAMP_MILLIS(2000))") {
		t.Errorf("first window: %s", got)
	}
	if got := p.statement(cqWindow{low: 1000, high: 2000}, ""); !strings.Contains(got, "APPENDS(TABLE `d.src`, TIMESTAMP_MILLIS(1000), TIMESTAMP_MILLIS(2000))") {
		t.Errorf("later window: %s", got)
	}
	p, err = parseContinuous("INSERT INTO d.out SELECT a FROM d.src")
	if err != nil {
		t.Fatal(err)
	}
	if got := p.statement(cqWindow{low: 1, high: 2}, ""); !strings.HasSuffix(got, " AS `src`") {
		t.Errorf("a table without an alias keeps its name: %s", got)
	}
}

// resvCall sends one Reservation API request to s.
func resvCall(t *testing.T, s *bqReservations, method, path, body string) (int, map[string]any) {
	t.Helper()
	r := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	if !s.serve(w, r) {
		t.Fatalf("%s %s: not served", method, path)
	}
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

// TestReservationAPIRules (#2193): the documented rules of reservations,
// assignments and capacity commitments.
func TestReservationAPIRules(t *testing.T) {
	now := time.Date(2026, 11, 23, 0, 0, 0, 0, time.UTC)
	s := &bqReservations{now: func() time.Time { return now }}
	base := "/v1/projects/p/locations/US"
	cases := []struct {
		method, path, body string
		code               int
	}{
		{"POST", base + "/reservations?reservationId=Bad_ID", `{}`, 400},
		{"POST", base + "/reservations?reservationId=r1-", `{}`, 400},
		{"POST", base + "/reservations?reservationId=r1", `{"slotCapacity":"75","edition":"ENTERPRISE"}`, 400},
		{"POST", base + "/reservations?reservationId=r1", `{"slotCapacity":"100","edition":"STANDARD"}`, 400},
		{"POST", base + "/reservations?reservationId=r1", `{"slotCapacity":"100","maxSlots":"50","scalingMode":"ALL_SLOTS","edition":"ENTERPRISE"}`, 400},
		{"POST", base + "/reservations?reservationId=r1", `{"slotCapacity":"100","edition":"ENTERPRISE","unknownField":1}`, 400},
		{"POST", base + "/reservations?reservationId=r1", `{"slotCapacity":"100","edition":"ENTERPRISE"}`, 200},
		{"POST", base + "/reservations?reservationId=r1", `{"slotCapacity":"100","edition":"ENTERPRISE"}`, 409},
		{"POST", base + "/reservations?reservationId=big", `{"slotCapacity":"1000","edition":"ENTERPRISE"}`, 200},
		{"POST", base + "/reservations?reservationId=std", `{"autoscale":{"maxSlots":"100"},"edition":"STANDARD"}`, 200},
		{"POST", base + "/reservations/r1/assignments", `{"assignee":"projects/p"}`, 400},
		{"POST", base + "/reservations/r1/assignments", `{"assignee":"p","jobType":"CONTINUOUS"}`, 400},
		{"POST", base + "/reservations/r1/assignments?assignmentId=a1", `{"assignee":"projects/p","jobType":"CONTINUOUS"}`, 200},
		{"POST", base + "/reservations/r1/assignments", `{"assignee":"projects/q","jobType":"QUERY"}`, 400},
		{"POST", base + "/reservations/big/assignments", `{"assignee":"projects/p","jobType":"CONTINUOUS"}`, 400},
		{"POST", base + "/reservations/big/assignments", `{"assignee":"projects/q","jobType":"CONTINUOUS"}`, 400},
		{"POST", base + "/reservations/std/assignments", `{"assignee":"folders/123","jobType":"QUERY"}`, 400},
		{"POST", base + "/reservations/none/assignments", `{"assignee":"projects/p","jobType":"PIPELINE"}`, 400},
		{"PATCH", base + "/reservations/r1?updateMask=edition", `{"edition":"ENTERPRISE_PLUS"}`, 400},
		{"PATCH", base + "/reservations/r1?updateMask=slotCapacity", `{"slotCapacity":"600"}`, 400},
		{"PATCH", base + "/reservations/r1?updateMask=slotCapacity", `{"slotCapacity":"200"}`, 200},
		{"DELETE", base + "/reservations/r1", ``, 400},
		{"GET", base + ":searchAllAssignments?query=assignee%3Dprojects%2Fp", ``, 200},
		{"DELETE", base + "/reservations/r1/assignments/a1", ``, 200},
		{"DELETE", base + "/reservations/r1", ``, 200},
		{"GET", base + "/reservations/r1", ``, 404},
		{"POST", base + "/capacityCommitments", `{"slotCount":"100"}`, 400},
		{"POST", base + "/capacityCommitments", `{"slotCount":"100","plan":"ANNUAL","renewalPlan":"MONTHLY"}`, 400},
		{"POST", base + "/capacityCommitments?capacityCommitmentId=c1", `{"slotCount":"100","plan":"ANNUAL"}`, 200},
		{"DELETE", base + "/capacityCommitments/c1", ``, 400},
		{"POST", base + "/capacityCommitments/c1:split", `{"slotCount":"50"}`, 200},
		{"POST", base + ":upgradeEdition", `{"reservations":["projects/p/locations/US/reservations/big"],"targetEdition":"STANDARD"}`, 400},
		{"POST", base + ":upgradeEdition", `{"reservations":["projects/p/locations/US/reservations/big"],"targetEdition":"ENTERPRISE"}`, 400},
		{"POST", base + ":upgradeEdition", `{"reservations":["projects/p/locations/US/reservations/big"],"targetEdition":"ENTERPRISE_PLUS"}`, 400},
		{"POST", base + ":upgradeEdition", `{"reservations":["projects/p/locations/US/reservations/gone"],"targetEdition":"ENTERPRISE_PLUS"}`, 404},
		{"POST", base + ":upgradeEdition", `{"reservations":["projects/p/locations/US/reservations/std"]}`, 200},
		{"GET", base + ":upgradeEdition", ``, 405},
	}
	for _, c := range cases {
		code, out := resvCall(t, s, c.method, c.path, c.body)
		if code != c.code {
			t.Errorf("%s %s %s: %d %v, want %d", c.method, c.path, c.body, code, out, c.code)
		}
	}
	code, out := resvCall(t, s, "DELETE", base+"/reservations/r1", ``)
	_ = out
	if code != 404 {
		t.Errorf("deleted twice: %d", code)
	}
	if _, ok := s.continuousReservation("p", "us", nil); ok {
		t.Error("a CONTINUOUS reservation after its assignment was deleted")
	}
	resvCall(t, s, "POST", base+"/reservations?reservationId=cq", `{"slotCapacity":"100","edition":"ENTERPRISE"}`)
	resvCall(t, s, "POST", base+"/reservations/cq/assignments", `{"assignee":"projects/p","jobType":"CONTINUOUS"}`)
	if rv, ok := s.continuousReservation("p", "us", nil); !ok || !strings.HasSuffix(rv.GetName(), "/reservations/cq") {
		t.Errorf("continuous reservation: %v %v", rv, ok)
	}
	if code, out := resvCall(t, s, "DELETE", base+"/reservations/cq", ``); code != 400 || out["error"].(map[string]any)["status"] != "FAILED_PRECONDITION" {
		t.Errorf("delete with assignments: %d %v, want FAILED_PRECONDITION", code, out)
	}
	_ = http.MethodGet
}
