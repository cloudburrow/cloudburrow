//go:build compat

package compat

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	"cloud.google.com/go/pubsub/v2"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
)

// refusedContinuous runs sql as a continuous query and wants it refused
// 400, with want in the error.
func refusedContinuous(t *testing.T, h *Harness, bq *bigquery.Client, what, sql, sa, want string) {
	t.Helper()
	q := bq.Query(sql)
	q.Continuous = true
	if sa != "" {
		q.ConnectionProperties = []*bigquery.ConnectionProperty{{Key: "service_account", Value: sa}}
	}
	_, err := q.Run(h.Context())
	var ge *googleapi.Error
	if !errors.As(err, &ge) || ge.Code != 400 || !strings.Contains(err.Error(), want) {
		t.Errorf("%s: %v, want 400 %q", what, err, want)
	}
}

// waitForWindowStart sleeps until just after the start of the next window
// of size, on this machine's clock (the instance's): rows inserted then
// commit within that window.
func waitForWindowStart(size time.Duration) time.Time {
	now := time.Now()
	next := now.Truncate(size).Add(size)
	time.Sleep(time.Until(next) + 300*time.Millisecond)
	return next
}

// covers: bigquery.jobs.insert, bigquery.jobs.cancel
//
// TestBigQueryContinuousQueryTumblePubSub (#2193): a window aggregation
// over TUMBLE, exported to Pub/Sub, as window-aggregations documents it.
// Rows are inserted at controlled times, one batch in each 6-second
// window: a window's aggregate is published only once a row of a later
// window has been read ("Output materialization"), once per window, with
// window_end at the window's exclusive end, aligned to the window size.
// The documented limits are refused: a window over 24 hours, a user
// timestamp column, ARRAY_AGG without its LIMIT, GROUP BY without a window
// column, aggregation outside a window, and HOP, which BigQuery does not
// have.
func TestBigQueryContinuousQueryTumblePubSub(t *testing.T) {
	h := New(t)
	t.Setenv("PUBSUB_EMULATOR_HOST", runShardEndpoint(h, envSinkPubSub, EnvPubSub))
	bq, project := bigqueryClient(t, h)
	rest, _, _ := reservationClients(t, h)
	ctx := h.Context()
	ps, err := pubsub.NewClient(ctx, h.Project())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ps.Close() })
	ds := bq.Dataset("cqt_" + strings.ReplaceAll(h.Project(), "-", "_"))
	if err := ds.Create(ctx, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ds.DeleteWithContents(h.Context()) })
	if err := ds.Table("clicks").Create(ctx, &bigquery.TableMetadata{Schema: bigquery.Schema{
		{Name: "user", Type: bigquery.StringFieldType}, {Name: "n", Type: bigquery.IntegerFieldType}}}); err != nil {
		t.Fatal(err)
	}
	topic := "projects/" + h.Project() + "/topics/cq-tumble"
	if _, err := ps.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: topic}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ps.TopicAdminClient.DeleteTopic(h.Context(), &pubsubpb.DeleteTopicRequest{Topic: topic}) })
	subName := "projects/" + h.Project() + "/subscriptions/cq-tumble"
	if _, err := ps.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{Name: subName, Topic: topic}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = ps.SubscriptionAdminClient.DeleteSubscription(h.Context(), &pubsubpb.DeleteSubscriptionRequest{Subscription: subName})
	})
	continuousReservation(t, h, rest, project, "cqt-"+strings.ToLower(lastPart(strings.ReplaceAll(h.Project(), "_", "-"))))
	sa := "cq-runner@" + project + ".iam.gserviceaccount.com"
	table := "`" + project + "." + ds.DatasetID + ".clicks`"
	export := "EXPORT DATA OPTIONS (format = 'CLOUD_PUBSUB', uri = 'https://pubsub.googleapis.com/" + topic + "') AS ("
	const size = 6 * time.Second
	sql := export + "SELECT TO_JSON_STRING(STRUCT(UNIX_MILLIS(window_start) AS start_ms, UNIX_MILLIS(window_end) AS end_ms, user, " +
		"COUNT(*) AS clicks, SUM(n) AS total, ARRAY_AGG(n ORDER BY n LIMIT 10) AS ns)) AS message " +
		"FROM TUMBLE(TABLE " + table + ", \"_CHANGE_TIMESTAMP\", INTERVAL 6 SECOND) GROUP BY window_start, window_end, user)"

	tumble := func(col, size string) string {
		return "TUMBLE(TABLE " + table + ", \"" + col + "\", " + size + ")"
	}
	refusedContinuous(t, h, bq, "a 25-hour window", export+"SELECT CAST(COUNT(*) AS STRING) AS message FROM "+
		tumble("_CHANGE_TIMESTAMP", "INTERVAL 25 HOUR")+" GROUP BY window_end)", sa, "24 hours")
	refusedContinuous(t, h, bq, "a user timestamp column", export+"SELECT CAST(COUNT(*) AS STRING) AS message FROM "+
		tumble("ts", "INTERVAL 1 MINUTE")+" GROUP BY window_end)", sa, "_CHANGE_TIMESTAMP")
	refusedContinuous(t, h, bq, "ARRAY_AGG without LIMIT", export+"SELECT TO_JSON_STRING(ARRAY_AGG(n)) AS message FROM "+
		tumble("_CHANGE_TIMESTAMP", "INTERVAL 1 MINUTE")+" GROUP BY window_end)", sa, "LIMIT")
	refusedContinuous(t, h, bq, "GROUP BY without a window column", export+"SELECT user AS message FROM "+
		tumble("_CHANGE_TIMESTAMP", "INTERVAL 1 MINUTE")+" GROUP BY user)", sa, "window_start")
	refusedContinuous(t, h, bq, "aggregation outside a window", export+"SELECT user AS message FROM "+table+" GROUP BY user)", sa, "window")
	refusedContinuous(t, h, bq, "HOP", export+"SELECT user AS message FROM HOP(TABLE "+table+
		", \"_CHANGE_TIMESTAMP\", INTERVAL 1 MINUTE, INTERVAL 5 MINUTE) GROUP BY window_end, user)", sa, "Table-valued function not found: HOP")

	job := startContinuous(t, h, bq, sql, sa)
	stopped := false
	t.Cleanup(func() {
		if !stopped {
			_ = job.Cancel(context.Background())
		}
	})
	insert := func(values string) {
		t.Helper()
		if err := runDML(ctx, bq.Query("INSERT INTO "+table+" (user, n) VALUES "+values)); err != nil {
			t.Fatal(err)
		}
	}
	type agg struct {
		StartMs int64   `json:"start_ms"`
		EndMs   int64   `json:"end_ms"`
		User    string  `json:"user"`
		Clicks  int64   `json:"clicks"`
		Total   int64   `json:"total"`
		Ns      []int64 `json:"ns"`
	}
	var mu sync.Mutex
	var got []agg
	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	received := make(chan struct{})
	go func() {
		defer close(received)
		_ = ps.Subscriber(subName).Receive(rctx, func(_ context.Context, m *pubsub.Message) {
			m.Ack()
			var a agg
			if err := json.Unmarshal(m.Data, &a); err != nil {
				t.Errorf("message %s: %v", m.Data, err)
				return
			}
			mu.Lock()
			got = append(got, a)
			mu.Unlock()
		})
	}()
	snapshot := func() []agg {
		mu.Lock()
		defer mu.Unlock()
		out := append([]agg(nil), got...)
		sort.Slice(out, func(i, j int) bool {
			return out[i].EndMs < out[j].EndMs || out[i].EndMs == out[j].EndMs && out[i].User < out[j].User
		})
		return out
	}

	// Window 1: three clicks of ann, one of bob.
	w1 := waitForWindowStart(size)
	insert("('ann', 3), ('ann', 1), ('bob', 5)")
	insert("('ann', 2)")
	// The window is open: nothing is published while no later row is read.
	time.Sleep(size - 2*time.Second)
	if g := snapshot(); len(g) != 0 {
		t.Fatalf("published %v before any row of a later window, want nothing until the window closes", g)
	}
	// Window 2 (or later): a row of bob closes window 1.
	w2 := waitForWindowStart(size)
	insert("('bob', 7)")
	eventually1874(t, "window 1's aggregates", 60*time.Second, func() bool { return len(snapshot()) >= 2 })
	g := snapshot()
	want1 := []agg{
		{StartMs: w1.UnixMilli(), EndMs: w1.Add(size).UnixMilli(), User: "ann", Clicks: 3, Total: 6, Ns: []int64{1, 2, 3}},
		{StartMs: w1.UnixMilli(), EndMs: w1.Add(size).UnixMilli(), User: "bob", Clicks: 1, Total: 5, Ns: []int64{5}},
	}
	check := func(g, want []agg) {
		t.Helper()
		if len(g) != len(want) {
			t.Fatalf("published %+v, want %+v", g, want)
		}
		for i := range want {
			a, b := g[i], want[i]
			if a.StartMs != b.StartMs || a.EndMs != b.EndMs || a.User != b.User || a.Clicks != b.Clicks || a.Total != b.Total ||
				len(a.Ns) != len(b.Ns) {
				t.Errorf("aggregate %d: %+v, want %+v", i, a, b)
				continue
			}
			for k := range b.Ns {
				if a.Ns[k] != b.Ns[k] {
					t.Errorf("aggregate %d: ns %v, want %v", i, a.Ns, b.Ns)
				}
			}
		}
	}
	check(g, want1)
	// A third window's row closes window 2: bob's one click there, and
	// window 1 is not published again.
	waitForWindowStart(size)
	insert("('cy', 1)")
	eventually1874(t, "window 2's aggregate", 60*time.Second, func() bool { return len(snapshot()) >= 3 })
	time.Sleep(3 * time.Second)
	check(snapshot(), append(want1, agg{StartMs: w2.UnixMilli(), EndMs: w2.Add(size).UnixMilli(), User: "bob", Clicks: 1, Total: 7, Ns: []int64{7}}))
	cancel()
	<-received
	cancelContinuous(t, h, job)
	stopped = true
}

// covers: bigquery.jobs.insert, bigquery.jobs.cancel
//
// TestBigQueryContinuousQueryJoin (#2193): a stream-to-stream INNER JOIN
// into a BigQuery table, as continuous-query-joins documents it: ride
// requests joined to available taxis of the same geohash that became
// available in the minute before the request. Each match is inserted once,
// whichever side arrives last; a taxi that arrives after the request, or
// in another geohash, does not match. LEFT JOIN, a JOIN without a time
// interval and a JOIN to a static table are refused.
func TestBigQueryContinuousQueryJoin(t *testing.T) {
	h := New(t)
	bq, project := bigqueryClient(t, h)
	rest, _, _ := reservationClients(t, h)
	ctx := h.Context()
	ds := bq.Dataset("cqj_" + strings.ReplaceAll(h.Project(), "-", "_"))
	if err := ds.Create(ctx, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ds.DeleteWithContents(h.Context()) })
	tables := map[string]bigquery.Schema{
		"ride_requests": {{Name: "request_id", Type: bigquery.StringFieldType}, {Name: "geohash", Type: bigquery.StringFieldType}},
		"taxirides": {{Name: "taxi_id", Type: bigquery.StringFieldType}, {Name: "geohash", Type: bigquery.StringFieldType},
			{Name: "ride_status", Type: bigquery.StringFieldType}},
		"matched_rides": {{Name: "request_id", Type: bigquery.StringFieldType}, {Name: "taxi_id", Type: bigquery.StringFieldType},
			{Name: "wait_ms", Type: bigquery.IntegerFieldType}},
		"zones": {{Name: "geohash", Type: bigquery.StringFieldType}},
		"neighborhood_taxi_health": {{Name: "window_end", Type: bigquery.TimestampFieldType}, {Name: "geohash", Type: bigquery.StringFieldType},
			{Name: "taxi_demand_volume", Type: bigquery.IntegerFieldType}},
	}
	for n, s := range tables {
		if err := ds.Table(n).Create(ctx, &bigquery.TableMetadata{Schema: s}); err != nil {
			t.Fatal(err)
		}
	}
	continuousReservation(t, h, rest, project, "cqj-"+strings.ToLower(lastPart(strings.ReplaceAll(h.Project(), "_", "-"))))
	name := func(t string) string { return "`" + project + "." + ds.DatasetID + "." + t + "`" }
	requests := "APPENDS(TABLE " + name("ride_requests") + ", CURRENT_TIMESTAMP() - INTERVAL 10 MINUTE) AS requests"
	taxis := "APPENDS(TABLE " + name("taxirides") + ", CURRENT_TIMESTAMP() - INTERVAL 10 MINUTE) AS taxis"
	bound := " AND taxis._CHANGE_TIMESTAMP BETWEEN (requests._CHANGE_TIMESTAMP - INTERVAL 1 MINUTE) AND requests._CHANGE_TIMESTAMP"
	insertInto := "INSERT INTO " + name("matched_rides") + " (request_id, taxi_id, wait_ms) "
	sql := insertInto + "SELECT requests.request_id, taxis.taxi_id, " +
		"TIMESTAMP_DIFF(requests._CHANGE_TIMESTAMP, taxis._CHANGE_TIMESTAMP, MILLISECOND) AS wait_ms FROM " + requests +
		" INNER JOIN " + taxis + " ON requests.geohash = taxis.geohash WHERE taxis.ride_status = 'available'" + bound

	refusedContinuous(t, h, bq, "LEFT JOIN", insertInto+"SELECT requests.request_id, taxis.taxi_id, 0 FROM "+requests+
		" LEFT JOIN "+taxis+" ON requests.geohash = taxis.geohash"+bound, "", "INNER JOIN")
	refusedContinuous(t, h, bq, "no time interval", insertInto+"SELECT requests.request_id, taxis.taxi_id, 0 FROM "+requests+
		" JOIN "+taxis+" ON requests.geohash = taxis.geohash", "", "time-oriented")
	refusedContinuous(t, h, bq, "a static table", insertInto+"SELECT requests.request_id, z.geohash, 0 FROM "+requests+
		" JOIN "+name("zones")+" AS z ON requests.geohash = z.geohash", "", "stream-to-static")

	insert := func(table, values string) {
		t.Helper()
		if err := runDML(ctx, bq.Query("INSERT INTO "+name(table)+" VALUES "+values)); err != nil {
			t.Fatal(err)
		}
	}
	// Before the job: a taxi available in g1, within the APPENDS start.
	insert("taxirides", "('t1', 'g1', 'available'), ('t9', 'g1', 'enroute')")
	job := startContinuous(t, h, bq, sql, "")
	stopped := false
	t.Cleanup(func() {
		if !stopped {
			_ = job.Cancel(context.Background())
		}
	})
	time.Sleep(1500 * time.Millisecond)
	insert("ride_requests", "('r1', 'g1')") // matches t1
	insert("ride_requests", "('r2', 'g2')") // no taxi in g2 yet
	time.Sleep(2500 * time.Millisecond)
	insert("taxirides", "('t2', 'g2', 'available')") // after r2: no match
	time.Sleep(2500 * time.Millisecond)
	insert("ride_requests", "('r3', 'g1'), ('r4', 'g2')") // r3 matches t1, r4 matches t2
	type match struct {
		req, taxi string
		wait      int64
	}
	read := func() []match {
		it, err := bq.Query("SELECT request_id, taxi_id, wait_ms FROM " + name("matched_rides") + " ORDER BY request_id, taxi_id").Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var out []match
		for {
			var r []bigquery.Value
			if err := it.Next(&r); err == iterator.Done {
				break
			} else if err != nil {
				t.Fatal(err)
			}
			out = append(out, match{r[0].(string), r[1].(string), r[2].(int64)})
		}
		return out
	}
	var got []match
	eventually1874(t, "matched rides", 60*time.Second, func() bool { got = read(); return len(got) >= 3 })
	// A few more runs: no pair is inserted twice.
	time.Sleep(4 * time.Second)
	got = read()
	want := []match{{"r1", "t1", 0}, {"r3", "t1", 0}, {"r4", "t2", 0}}
	if len(got) != len(want) {
		t.Fatalf("matched rides %v, want %v", got, want)
	}
	for i, w := range want {
		if got[i].req != w.req || got[i].taxi != w.taxi || got[i].wait < 0 || got[i].wait > 60000 {
			t.Errorf("match %d: %v, want %s with %s within a minute", i, got[i], w.req, w.taxi)
		}
	}
	cancelContinuous(t, h, job)
	stopped = true

	// Google's JOIN with a window aggregation over it
	// (continuous-queries#join-agg-window-example): the demand per
	// geohash in each 6-second window, written once the window closes.
	const size = 6 * time.Second
	health := "INSERT INTO " + name("neighborhood_taxi_health") + " WITH potential_matches AS (SELECT requests._CHANGE_TIMESTAMP AS bq_changed_ts, " +
		"requests.geohash FROM " + requests + " INNER JOIN " + taxis + " ON requests.geohash = taxis.geohash WHERE taxis.ride_status = 'available'" + bound +
		") SELECT window_end, geohash, COUNT(*) AS taxi_demand_volume FROM TUMBLE(TABLE potential_matches, \"bq_changed_ts\", INTERVAL 6 SECOND) " +
		"GROUP BY window_end, geohash"
	job2 := startContinuous(t, h, bq, health, "")
	t.Cleanup(func() { _ = job2.Cancel(context.Background()) })
	wa := waitForWindowStart(size)
	insert("taxirides", "('t5', 'g5', 'available')")
	insert("ride_requests", "('r5', 'g5'), ('r6', 'g5')")
	waitForWindowStart(size)
	insert("ride_requests", "('r7', 'g7')") // closes the window
	type demand struct {
		end    time.Time
		volume int64
	}
	readHealth := func() []demand {
		it, err := bq.Query("SELECT window_end, taxi_demand_volume FROM " + name("neighborhood_taxi_health") + " WHERE geohash = 'g5'").Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var out []demand
		for {
			var r []bigquery.Value
			if err := it.Next(&r); err == iterator.Done {
				break
			} else if err != nil {
				t.Fatal(err)
			}
			out = append(out, demand{r[0].(time.Time), r[1].(int64)})
		}
		return out
	}
	var hg []demand
	eventually1874(t, "the window's demand", 60*time.Second, func() bool { hg = readHealth(); return len(hg) >= 1 })
	time.Sleep(3 * time.Second)
	hg = readHealth()
	if len(hg) != 1 || !hg[0].end.Equal(wa.Add(size)) || hg[0].volume != 2 {
		t.Errorf("g5's demand %v, want 2 in the window ending %v, once", hg, wa.Add(size))
	}
	cancelContinuous(t, h, job2)
}
