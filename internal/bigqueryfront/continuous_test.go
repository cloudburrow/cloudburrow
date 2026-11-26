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
	}
	for q, code := range refused {
		_, err := parseContinuous(q)
		e, isRefusal := err.(*cqRefusal)
		if !isRefusal || e.code != code {
			t.Errorf("%s: %v, want refused %d", q, err, code)
		}
	}
}

// The documented examples of stateful continuous queries
// (continuous-query-joins, window-aggregations, continuous-queries#join-agg-window-example).
const (
	cqJoinExample = "INSERT INTO `real_time_taxi_streaming.matched_rides` SELECT requests.timestamp AS request_time, requests.request_id, " +
		"taxis.taxi_id, ST_DISTANCE(ST_GEOGPOINT(requests.longitude, requests.latitude), ST_GEOGPOINT(taxis.longitude, taxis.latitude)) AS distance_in_meters, " +
		"taxis.timestamp AS taxi_available_time FROM APPENDS (TABLE `real_time_taxi_streaming.ride_requests`, CURRENT_TIMESTAMP() - INTERVAL 10 MINUTE) AS requests " +
		"INNER JOIN APPENDS (TABLE `real_time_taxi_streaming.taxirides`, CURRENT_TIMESTAMP() - INTERVAL 10 MINUTE) AS taxis ON requests.geohash = taxis.geohash " +
		"WHERE taxis.ride_status = 'available' AND taxis._CHANGE_TIMESTAMP BETWEEN(requests._CHANGE_TIMESTAMP - INTERVAL 5 MINUTE) AND requests._CHANGE_TIMESTAMP " +
		"AND ST_DWITHIN(ST_GEOGPOINT(requests.longitude, requests.latitude), ST_GEOGPOINT(taxis.longitude, taxis.latitude), 2000 -- Distance in meters\n);"
	cqWindowExample = "INSERT INTO `real_time_taxi_streaming.driver_stats`\n\nWITH ride_completions AS ( SELECT _CHANGE_TIMESTAMP as bq_changed_ts, " +
		"CAST(timestamp AS DATE) AS ride_date, taxi_id, meter_reading, passenger_count FROM APPENDS(TABLE `real_time_taxi_streaming.taxirides`, " +
		"CURRENT_TIMESTAMP() - INTERVAL 10 MINUTE) WHERE ride_status = 'dropoff')\n SELECT ride_date, window_end, taxi_id, COUNT(taxi_id) AS total_rides_per_half_hour, " +
		"ROUND(AVG(meter_reading),2) AS avg_fare_per_half_hour, SUM(passenger_count) AS total_passengers_per_half_hour FROM " +
		"tumble(TABLE ride_completions,\"bq_changed_ts\",INTERVAL 30 MINUTE) GROUP BY window_end, ride_date, taxi_id"
	cqJoinWindowExample = "INSERT INTO `real_time_taxi_streaming.neighborhood_taxi_health` WITH potential_matches AS ( SELECT requests._CHANGE_TIMESTAMP AS bq_changed_ts, " +
		"requests.geohash, requests.latitude, requests.longitude, ST_DISTANCE(ST_GEOGPOINT(requests.longitude, requests.latitude), " +
		"ST_GEOGPOINT(taxis.longitude, taxis.latitude)) AS distance_in_meters FROM APPENDS(TABLE `real_time_taxi_streaming.ride_requests`, " +
		"CURRENT_TIMESTAMP() - INTERVAL 10 MINUTE) AS requests INNER JOIN APPENDS(TABLE `real_time_taxi_streaming.taxirides`, " +
		"CURRENT_TIMESTAMP() - INTERVAL 10 MINUTE) AS taxis ON requests.geohash = taxis.geohash WHERE taxis.ride_status = 'available' " +
		"AND taxis._CHANGE_TIMESTAMP BETWEEN (requests._CHANGE_TIMESTAMP - INTERVAL 5 MINUTE) AND requests._CHANGE_TIMESTAMP " +
		"AND ST_Dwithin(ST_GEOGPOINT(requests.longitude, requests.latitude), ST_GEOGPOINT(taxis.longitude, taxis.latitude), 2000 -- Distance in meters\n) ) " +
		"SELECT window_end, geohash, ROUND(AVG(latitude), 6) AS avg_latitude, ROUND(AVG(longitude), 6) AS avg_longitude, COUNT(*) AS taxi_demand_volume, " +
		"ROUND(AVG(distance_in_meters), 2) AS avg_proximity_meters, ROUND(MIN(distance_in_meters), 2) AS min_proximity_meters, " +
		"ROUND(MAX(distance_in_meters), 2) AS max_proximity_meters, ROUND(STDDEV(distance_in_meters), 2) AS proximity_stddev " +
		"FROM TUMBLE(TABLE potential_matches, \"bq_changed_ts\", INTERVAL 5 MINUTE) GROUP BY window_end, geohash;"
)

// TestContinuousQueryStatefulSQL (#2193): the JOINs, window aggregations
// and TUMBLE calls BigQuery documents, accepted, and what it documents as
// unsupported, refused.
func TestContinuousQueryStatefulSQL(t *testing.T) {
	const ap = "APPENDS(TABLE d.a, CURRENT_TIMESTAMP() - INTERVAL 1 HOUR)"
	const bp = "APPENDS(TABLE d.b, CURRENT_TIMESTAMP() - INTERVAL 1 HOUR)"
	const bound = " AND b._CHANGE_TIMESTAMP BETWEEN a._CHANGE_TIMESTAMP - INTERVAL 1 MINUTE AND a._CHANGE_TIMESTAMP + INTERVAL 30 SECOND"
	const tumble = "TUMBLE(TABLE d.a, \"_CHANGE_TIMESTAMP\", INTERVAL 1 MINUTE)"
	ok := map[string]int64{ // the JOINs' reach, microseconds
		cqJoinExample:       300e6,
		cqWindowExample:     0,
		cqJoinWindowExample: 300e6,
		"INSERT INTO d.out SELECT a.x, b.y FROM " + ap + " AS a JOIN " + bp + " AS b ON a.k = b.k" + bound:                                                                                                             60e6,
		"INSERT INTO d.out SELECT a.x FROM " + ap + " a INNER JOIN " + bp + " b ON a.k = b.k AND TIMESTAMP_DIFF(a._CHANGE_TIMESTAMP, b._CHANGE_TIMESTAMP, SECOND) BETWEEN -10 AND 10":                                  11e6,
		"INSERT INTO d.out SELECT a.x FROM " + ap + " a JOIN " + bp + " b USING (k) WHERE b._CHANGE_TIMESTAMP >= TIMESTAMP_SUB(a._CHANGE_TIMESTAMP, INTERVAL 2 MINUTE) AND b._CHANGE_TIMESTAMP <= a._CHANGE_TIMESTAMP": 120e6,
		"INSERT INTO d.out SELECT window_start, COUNT(*) AS n, ARRAY_AGG(x LIMIT 10) AS xs, STRING_AGG(s, ',' LIMIT 100) AS ss FROM " + tumble + " GROUP BY window_start":                                              0,
		"INSERT INTO d.out SELECT window_end, k, MAX_BY(x, y) FROM " + tumble + " AS w GROUP BY 1, k":                                                                                                                  0,
		"INSERT INTO d.out SELECT t.window_end AS e, COUNT(*) FROM " + tumble + " AS t GROUP BY e":                                                                                                                     0,
		"INSERT INTO d.out SELECT x, window_end FROM " + tumble:                                                                                                                                                        0,
		"INSERT INTO d.out SELECT a.x FROM " + ap + " AS a, UNNEST(a.arr) AS x":                                                                                                                                        0,
	}
	for q, reach := range ok {
		p, err := parseContinuous(q)
		if err != nil {
			t.Errorf("%s: %v, want it accepted", q, err)
			continue
		}
		if !p.query.stateful() && !strings.Contains(q, "UNNEST") {
			t.Errorf("%s: not stateful", q)
		}
		if got := p.query.lookback(); got != reach {
			t.Errorf("%s: reach %d, want %d", q, got, reach)
		}
	}
	refused := map[string]string{
		"INSERT INTO d.out SELECT a.x FROM " + ap + " AS a LEFT JOIN " + bp + " AS b ON a.k = b.k" + bound:                                       "only INNER JOIN",
		"INSERT INTO d.out SELECT a.x FROM " + ap + " AS a FULL OUTER JOIN " + bp + " AS b ON a.k = b.k" + bound:                                 "only INNER JOIN",
		"INSERT INTO d.out SELECT a.x FROM " + ap + " AS a CROSS JOIN " + bp + " AS b":                                                           "only INNER JOIN",
		"INSERT INTO d.out SELECT a.x FROM " + ap + " AS a, " + bp + " AS b WHERE a.k = b.k" + bound:                                             "comma",
		"INSERT INTO d.out SELECT a.x FROM " + ap + " AS a JOIN " + bp + " AS b ON a.k = b.k":                                                    "time-oriented",
		"INSERT INTO d.out SELECT a.x FROM " + ap + " AS a JOIN " + bp + " AS b ON a.k = b.k AND b.ts BETWEEN a.ts - INTERVAL 1 MINUTE AND a.ts": "time-oriented",
		"INSERT INTO d.out SELECT a.x FROM " + ap + " AS a JOIN " + bp + " AS b ON a.k = b.k AND b._CHANGE_TIMESTAMP >= a._CHANGE_TIMESTAMP":     "time-oriented",
		"INSERT INTO d.out SELECT a.x FROM " + ap + " AS a JOIN d.dim AS b ON a.k = b.k" + bound:                                                 "stream-to-static",
		"INSERT INTO d.out SELECT a.x FROM " + ap + " AS a JOIN APPENDS(TABLE d.b, NULL) AS b ON a.k = b.k" + bound:                              "starting time",
		"EXPORT DATA OPTIONS(format = 'CLOUD_PUBSUB', uri = 'https://pubsub.googleapis.com/projects/p/topics/t') AS (SELECT a.x AS message FROM " + ap +
			" AS a JOIN CHANGES(TABLE d.b, CURRENT_TIMESTAMP() - INTERVAL 1 HOUR) AS b ON a.k = b.k" + bound + ")": "CHANGES",
		"EXPORT DATA OPTIONS(format = 'CLOUD_PUBSUB', uri = 'https://pubsub.googleapis.com/projects/p/topics/t') AS (SELECT TO_JSON_STRING(STRUCT(window_end)) AS message " +
			"FROM TUMBLE(TABLE c, \"_CHANGE_TIMESTAMP\", INTERVAL 1 MINUTE) GROUP BY window_end)": "",
		"INSERT INTO d.out SELECT k, COUNT(*) FROM d.src GROUP BY k":                                                                                            "outside a window",
		"INSERT INTO d.out SELECT COUNT(*) FROM " + ap:                                                                                                          "outside a window",
		"INSERT INTO d.out SELECT window_end, COUNT(*) FROM TUMBLE(TABLE d.a, \"_CHANGE_TIMESTAMP\", INTERVAL 25 HOUR) GROUP BY window_end":                     "24 hours",
		"INSERT INTO d.out SELECT window_end, COUNT(*) FROM TUMBLE(TABLE d.a, \"ts\", INTERVAL 1 HOUR) GROUP BY window_end":                                     "user-defined",
		"INSERT INTO d.out WITH c AS (SELECT ts FROM " + ap + ") SELECT window_end, COUNT(*) FROM TUMBLE(TABLE c, \"ts\", INTERVAL 1 HOUR) GROUP BY window_end": "user-defined",
		"INSERT INTO d.out SELECT window_end, COUNT(*) FROM TUMBLE(TABLE d.a, _CHANGE_TIMESTAMP, INTERVAL 1 HOUR) GROUP BY window_end":                          "STRING literal",
		"INSERT INTO d.out SELECT k, COUNT(*) FROM " + tumble + " GROUP BY k":                                                                                   "GROUP BY has neither",
		"INSERT INTO d.out SELECT COUNT(*) FROM " + tumble:                                                                                                      "GROUP BY has neither",
		"INSERT INTO d.out SELECT window_end, COUNT(DISTINCT k) FROM " + tumble + " GROUP BY window_end":                                                        "DISTINCT",
		"INSERT INTO d.out SELECT window_end, ARRAY_AGG(k) FROM " + tumble + " GROUP BY window_end":                                                             "requires a LIMIT",
		"INSERT INTO d.out SELECT window_end, ARRAY_AGG(k LIMIT 10001) FROM " + tumble + " GROUP BY window_end":                                                 "maximum value of 10000",
		"INSERT INTO d.out SELECT window_end, STRING_AGG(s LIMIT 101) FROM " + tumble + " GROUP BY window_end":                                                  "maximum value of 100",
		"INSERT INTO d.out SELECT window_end, ST_UNION_AGG(g) FROM " + tumble + " GROUP BY window_end":                                                          "ST_UNION_AGG",
		"INSERT INTO d.out SELECT window_end, GROUPING(k) FROM " + tumble + " GROUP BY window_end":                                                              "GROUPING",
		"INSERT INTO d.out SELECT WITH DIFFERENTIAL_PRIVACY OPTIONS(epsilon = 1) COUNT(*) FROM d.a":                                                             "differential privacy",
		"INSERT INTO d.out SELECT window_end, COUNT(*) FROM TUMBLE(TABLE d.a, \"_CHANGE_TIMESTAMP\", INTERVAL 1 MINUTE) GROUP BY window_end; ":                  "",
		"EXPORT DATA OPTIONS(format = 'CLOUD_PUBSUB', uri = 'https://pubsub.googleapis.com/projects/p/topics/t') AS (SELECT CAST(window_end AS STRING) AS message " +
			"FROM TUMBLE(TABLE c, \"_CHANGE_TIMESTAMP\", INTERVAL 1 MINUTE)) ": "",
		"INSERT INTO d.out SELECT window_end, COUNT(*) FROM\n  HOP(TABLE d.a, \"_CHANGE_TIMESTAMP\", INTERVAL 1 MINUTE, INTERVAL 5 MINUTE) GROUP BY window_end": "Table-valued function not found: HOP at [2:3]",
		"INSERT INTO d.out SELECT window_end, COUNT(*) FROM SESSION(TABLE d.a, \"_CHANGE_TIMESTAMP\", INTERVAL 1 MINUTE) GROUP BY window_end":                   "Table-valued function not found: SESSION",
		"EXPORT DATA OPTIONS(format = 'CLOUD_PUBSUB', uri = 'https://pubsub.googleapis.com/projects/p/topics/t') AS (SELECT CAST(window_end AS STRING) AS message " +
			"FROM TUMBLE(TABLE d.a, \"_CHANGE_TIMESTAMP\", INTERVAL 1 MINUTE) UNION ALL SELECT CAST(a AS STRING) FROM CHANGES(TABLE d.b, CURRENT_TIMESTAMP() - INTERVAL 1 HOUR))": "CHANGES",
	}
	for q, want := range refused {
		_, err := parseContinuous(q)
		if want == "" {
			if err != nil {
				t.Errorf("%s: %v, want it accepted", q, err)
			}
			continue
		}
		e, isRefusal := err.(*cqRefusal)
		if !isRefusal || e.code != 400 || !strings.Contains(e.msg, want) {
			t.Errorf("%s: %v, want refused 400 %q", q, err, want)
		}
	}
}

// TestContinuousQueryStatefulRender (#2193): a run reads its sources back
// by its windows and its JOINs' interval, and keeps the windows it closes,
// or the JOIN pairs whose later row it read new.
func TestContinuousQueryStatefulRender(t *testing.T) {
	p, err := parseContinuous("INSERT INTO d.out SELECT window_end, COUNT(*) AS n FROM TUMBLE(TABLE d.a, \"_CHANGE_TIMESTAMP\", INTERVAL 1 MINUTE) GROUP BY window_end")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range p.query.baseSources() {
		s.startMs = 1000
	}
	w := cqWindow{low: 200000, high: 300000, stateful: true, prevWM: 130e6, curWM: 250e6}
	w.read = statefulRead(p.query, w)
	if w.read != 120000 {
		t.Errorf("read from %d, want 120000 (the start of the window open at the last watermark)", w.read)
	}
	got := p.statement(w, "")
	for _, want := range []string{
		"FROM (SELECT * EXCEPT (_CHANGE_TYPE) FROM APPENDS(TABLE `d.a`, TIMESTAMP_MILLIS(120000), TIMESTAMP_MILLIS(300000))) AS _cloudburrow_tumble",
		"TIMESTAMP_MICROS(DIV(UNIX_MICROS(_cloudburrow_tumble.`_CHANGE_TIMESTAMP`), 60000000) * 60000000) AS window_start",
		"WHERE window_end > TIMESTAMP_MICROS(130000000) AND window_end <= TIMESTAMP_MICROS(250000000)) GROUP BY window_end",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("statement %s\nlacks %s", got, want)
		}
	}
	if !closesWindow([]int64{60e6}, 130e6, 180e6) || closesWindow([]int64{60e6}, 130e6, 179e6) || closesWindow([]int64{60e6}, 120e6, 179e6) {
		t.Error("closesWindow: a window [120s, 180s) closes once a row of 180s or later is read")
	}

	p, err = parseContinuous("INSERT INTO d.out SELECT a.x FROM APPENDS(TABLE d.a, CURRENT_TIMESTAMP() - INTERVAL 1 HOUR) AS a " +
		"JOIN APPENDS(TABLE d.b, NULL) AS b ON a.k = b.k AND b._CHANGE_TIMESTAMP BETWEEN a._CHANGE_TIMESTAMP - INTERVAL 1 MINUTE AND a._CHANGE_TIMESTAMP " +
		"WHERE a.x > 0")
	if err == nil {
		t.Fatal("an APPENDS side with a NULL start is accepted")
	}
	p, err = parseContinuous("INSERT INTO d.out SELECT a.x FROM APPENDS(TABLE d.a, CURRENT_TIMESTAMP() - INTERVAL 1 HOUR) AS a " +
		"JOIN APPENDS(TABLE d.b, CURRENT_TIMESTAMP() - INTERVAL 1 HOUR) AS b ON a.k = b.k AND b._CHANGE_TIMESTAMP BETWEEN a._CHANGE_TIMESTAMP - INTERVAL 1 MINUTE AND a._CHANGE_TIMESTAMP " +
		"WHERE a.x > 0")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range p.query.baseSources() {
		s.startMs = 1000
	}
	w = cqWindow{low: 200000, high: 300000, stateful: true, joinOnce: true}
	w.read = statefulRead(p.query, w)
	got = p.statement(w, "")
	for _, want := range []string{
		"APPENDS(TABLE `d.a`, TIMESTAMP_MILLIS(140000), TIMESTAMP_MILLIS(300000)) AS a",
		"WHERE (GREATEST(`a`._CHANGE_TIMESTAMP, `b`._CHANGE_TIMESTAMP) >= TIMESTAMP_MILLIS(200000)) AND ( a.x > 0)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("statement %s\nlacks %s", got, want)
		}
	}
	w.first = true
	w.read = statefulRead(p.query, w)
	if got := p.statement(w, ""); strings.Contains(got, "GREATEST") || !strings.Contains(got, "TIMESTAMP_MILLIS(1000)") {
		t.Errorf("first run: %s, want every pair from the sources' start", got)
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
