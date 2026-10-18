//go:build soak

package soak

import (
	"context"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/bigquery"
	cloudtasks "cloud.google.com/go/cloudtasks/apiv2"
	taskspb "cloud.google.com/go/cloudtasks/apiv2/cloudtaskspb"
	"cloud.google.com/go/firestore"
	"cloud.google.com/go/logging"
	"cloud.google.com/go/logging/logadmin"
	"cloud.google.com/go/pubsub/v2"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	secretmanager "cloud.google.com/go/secretmanager/apiv1"
	smpb "cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"cloud.google.com/go/storage"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	mrpb "google.golang.org/genproto/googleapis/api/monitoredres"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func grpcOpts(addr string) []option.ClientOption {
	return []option.ClientOption{option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials()))}
}

// --- Storage ---------------------------------------------------------------

// objectSize draws a size from 1 KB to 10 MB, weighted as real buckets are:
// mostly small objects, a few large ones.
func objectSize() int {
	switch r := rand.Float64(); {
	case r < 0.50:
		return 1 << 10
	case r < 0.75:
		return 64 << 10
	case r < 0.95:
		return 1 << 20
	default:
		return 10 << 20
	}
}

// runStorage: four workers, each over a ring of eight object names it
// overwrites, so the bucket's size is bounded and the state directory's
// growth is a leak, not data. Each call is an upload (45%), a download that
// checks the bytes (40%) or a listing (15%).
func runStorage(ctx context.Context, e *env, wg *sync.WaitGroup) (func(), error) {
	addr := endpoint(e.t, envStorage)
	if addr == "" {
		return nil, nil
	}
	os.Setenv("STORAGE_EMULATOR_HOST", addr)
	sc, cancel := call()
	defer cancel()
	c, err := storage.NewClient(sc, option.WithoutAuthentication(), option.WithEndpoint(addr+"/storage/v1/"))
	if err != nil {
		return nil, err
	}
	bh := c.Bucket(e.project + "-soak")
	if err := bh.Create(sc, e.project, nil); err != nil {
		return nil, err
	}
	for w := 0; w < 4; w++ {
		prefix := fmt.Sprintf("w%d/", w)
		crcs := map[string]uint32{} // this worker's objects, by name
		lens := map[string]int{}
		n := 0
		e.every(ctx, wg, 4, func(context.Context) {
			ctx, cancel := call()
			defer cancel()
			r := rand.Float64()
			switch {
			case r < 0.45 || len(crcs) == 0:
				name := prefix + strconv.Itoa(n%8)
				n++
				size := objectSize()
				data := make([]byte, size)
				for i := 0; i < size; i += 4096 {
					data[i] = byte(rand.IntN(256))
				}
				_ = e.rec.do("storage.upload", func() error {
					ow := bh.Object(name).NewWriter(ctx)
					ow.ChunkSize = 0
					if _, err := ow.Write(data); err != nil {
						_ = ow.Close()
						return err
					}
					return ow.Close()
				})
				crcs[name] = crc32.ChecksumIEEE(data)
				lens[name] = size
			case r < 0.85:
				var name string
				for k := range crcs {
					name = k
					break
				}
				_ = e.rec.do("storage.download", func() error {
					rd, err := bh.Object(name).NewReader(ctx)
					if err != nil {
						return err
					}
					defer rd.Close()
					b, err := io.ReadAll(rd)
					if err != nil {
						return err
					}
					if len(b) != lens[name] || crc32.ChecksumIEEE(b) != crcs[name] {
						return fmt.Errorf("object %s: read %d bytes that differ from the %d written", name, len(b), lens[name])
					}
					return nil
				})
			default:
				_ = e.rec.do("storage.list", func() error {
					it := bh.Objects(ctx, &storage.Query{Prefix: prefix})
					for {
						_, err := it.Next()
						if errors.Is(err, iterator.Done) {
							return nil
						}
						if err != nil {
							return err
						}
					}
				})
			}
		})
	}
	return func() {
		ctx, cancel := call()
		defer cancel()
		it := bh.Objects(ctx, nil)
		for {
			a, err := it.Next()
			if err != nil {
				break
			}
			_ = bh.Object(a.Name).Delete(ctx)
		}
		_ = bh.Delete(ctx)
		_ = c.Close()
	}, nil
}

// --- Pub/Sub ---------------------------------------------------------------

// tracker counts deliveries of each published message on one subscription.
type tracker struct {
	mu   sync.Mutex
	seen map[string]int
}

func (k *tracker) add(id string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.seen[id]++
}

func (k *tracker) count(id string) int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.seen[id]
}

// runPubSub: two publishers at 20 messages a second each; one subscription
// read by streaming pull (Subscriber.Receive), one by synchronous Pull and
// Acknowledge. Every message carries a unique id, and at the end each
// subscription must have seen every acknowledged publish; a redelivery is
// counted, since at-least-once allows it, and reported when it happens to a
// message already acknowledged in time.
func runPubSub(ctx context.Context, e *env, wg *sync.WaitGroup) (func(), error) {
	addr := endpoint(e.t, envPubSub)
	if addr == "" {
		return nil, nil
	}
	os.Setenv("PUBSUB_EMULATOR_HOST", addr)
	sc, cancel := call()
	defer cancel()
	c, err := pubsub.NewClient(sc, e.project)
	if err != nil {
		return nil, err
	}
	topic := "projects/" + e.project + "/topics/soak"
	if _, err := c.TopicAdminClient.CreateTopic(sc, &pubsubpb.Topic{Name: topic}); err != nil {
		return nil, err
	}
	subs := map[string]*tracker{}
	for _, s := range []string{"stream", "pull"} {
		name := "projects/" + e.project + "/subscriptions/soak-" + s
		if _, err := c.SubscriptionAdminClient.CreateSubscription(sc, &pubsubpb.Subscription{Name: name, Topic: topic, AckDeadlineSeconds: 30}); err != nil {
			return nil, err
		}
		subs[s] = &tracker{seen: map[string]int{}}
	}

	var pmu sync.Mutex
	published := map[string]bool{}
	pub := c.Publisher(topic)
	for w := 0; w < 2; w++ {
		seq := 0
		e.every(ctx, wg, 20, func(context.Context) {
			ctx, cancel := call()
			defer cancel()
			id := fmt.Sprintf("%d-%d", w, seq)
			seq++
			err := e.rec.do("pubsub.publish", func() error {
				_, err := pub.Publish(ctx, &pubsub.Message{
					Data:       []byte("soak " + id),
					Attributes: map[string]string{"id": id, "at": strconv.FormatInt(time.Now().UnixNano(), 10)},
				}).Get(ctx)
				return err
			})
			if err == nil {
				pmu.Lock()
				published[id] = true
				pmu.Unlock()
			}
		})
	}

	// Streaming pull runs until the run's checks are done, so it drains the
	// tail; its context is its own.
	rctx, rcancel := context.WithCancel(context.Background())
	rdone := make(chan struct{})
	go func() {
		defer close(rdone)
		sub := c.Subscriber("projects/" + e.project + "/subscriptions/soak-stream")
		for rctx.Err() == nil {
			err := sub.Receive(rctx, func(_ context.Context, m *pubsub.Message) {
				if at, err := strconv.ParseInt(m.Attributes["at"], 10, 64); err == nil {
					e.rec.record("pubsub.stream.e2e", time.Unix(0, at), time.Since(time.Unix(0, at)), nil)
				}
				subs["stream"].add(m.Attributes["id"])
				m.Ack()
			})
			if err != nil && rctx.Err() == nil {
				e.rec.fail("pubsub.stream.e2e", errorCode(err), "Receive ended: "+err.Error())
				time.Sleep(time.Second)
			}
		}
	}()

	pullName := "projects/" + e.project + "/subscriptions/soak-pull"
	pullOnce := func(ctx context.Context, group string) int {
		var resp *pubsubpb.PullResponse
		err := e.rec.do(group, func() error {
			var err error
			resp, err = c.SubscriptionAdminClient.Pull(ctx, &pubsubpb.PullRequest{Subscription: pullName, MaxMessages: 100})
			return err
		})
		if err != nil || len(resp.GetReceivedMessages()) == 0 {
			return 0
		}
		var ids []string
		for _, m := range resp.GetReceivedMessages() {
			subs["pull"].add(m.GetMessage().GetAttributes()["id"])
			ids = append(ids, m.GetAckId())
		}
		_ = e.rec.do("pubsub.ack", func() error {
			return c.SubscriptionAdminClient.Acknowledge(ctx, &pubsubpb.AcknowledgeRequest{Subscription: pullName, AckIds: ids})
		})
		return len(ids)
	}
	e.every(ctx, wg, 2, func(context.Context) {
		ctx, cancel := call()
		defer cancel()
		pullOnce(ctx, "pubsub.pull")
	})

	return func() {
		pub.Stop()
		// Drain: each subscription has a minute to deliver what is left.
		deadline := time.Now().Add(time.Minute)
		for time.Now().Before(deadline) {
			ctx, cancel := call()
			n := pullOnce(ctx, "pubsub.pull.drain")
			cancel()
			pmu.Lock()
			missing := 0
			for id := range published {
				if subs["stream"].count(id) == 0 || subs["pull"].count(id) == 0 {
					missing++
				}
			}
			pmu.Unlock()
			if missing == 0 {
				break
			}
			if n == 0 {
				time.Sleep(time.Second)
			}
		}
		rcancel()
		<-rdone
		pmu.Lock()
		defer pmu.Unlock()
		for name, k := range subs {
			lost, dups := 0, 0
			for id := range published {
				switch n := k.count(id); {
				case n == 0:
					lost++
				case n > 1:
					dups += n - 1
				}
			}
			e.t.Logf("pubsub %s: %d published, %d lost, %d redelivered", name, len(published), lost, dups)
			if lost > 0 {
				e.rec.fail("pubsub."+name+".delivery", "lost", fmt.Sprintf("%d of %d acknowledged publishes never arrived", lost, len(published)))
			}
			// Every message is acked well inside its 30 s deadline, so a
			// redelivery is beyond what at-least-once excuses here once it is
			// more than a trace.
			if dups*1000 > len(published) {
				e.rec.fail("pubsub."+name+".delivery", "duplicated", fmt.Sprintf("%d redeliveries of %d acknowledged messages", dups, len(published)))
			}
		}
		ctx, cancel := call()
		defer cancel()
		for s := range subs {
			_ = c.SubscriptionAdminClient.DeleteSubscription(ctx, &pubsubpb.DeleteSubscriptionRequest{Subscription: "projects/" + e.project + "/subscriptions/soak-" + s})
		}
		_ = c.TopicAdminClient.DeleteTopic(ctx, &pubsubpb.DeleteTopicRequest{Topic: topic})
		_ = c.Close()
	}, nil
}

// --- Cloud Tasks -----------------------------------------------------------

// runTasks: two workers enqueue ten HTTP tasks a second each, at a loopback
// target that answers 200. The dispatcher runs in the `up` process on this
// machine, so it reaches the target directly. At the end every task must
// have arrived exactly once: a 200 is a success, and a second delivery of a
// task that succeeded is a bug.
func runTasks(ctx context.Context, e *env, wg *sync.WaitGroup) (func(), error) {
	addr := endpoint(e.t, envTasks)
	if addr == "" {
		return nil, nil
	}
	var mu sync.Mutex
	got := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		id, at, _ := strings.Cut(string(b), " ")
		if ns, err := strconv.ParseInt(at, 10, 64); err == nil {
			e.rec.record("tasks.e2e", time.Unix(0, ns), time.Since(time.Unix(0, ns)), nil)
		}
		mu.Lock()
		got[id]++
		mu.Unlock()
	}))
	sc, cancel := call()
	defer cancel()
	c, err := cloudtasks.NewClient(sc, grpcOpts(addr)...)
	if err != nil {
		return nil, err
	}
	loc := "projects/" + e.project + "/locations/us-central1"
	q, err := c.CreateQueue(sc, &taskspb.CreateQueueRequest{Parent: loc, Queue: &taskspb.Queue{Name: loc + "/queues/soak"}})
	if err != nil {
		return nil, err
	}
	var cmu sync.Mutex
	created := map[string]bool{}
	for w := 0; w < 2; w++ {
		seq := 0
		e.every(ctx, wg, 10, func(context.Context) {
			ctx, cancel := call()
			defer cancel()
			id := fmt.Sprintf("%d-%d", w, seq)
			seq++
			err := e.rec.do("tasks.create", func() error {
				_, err := c.CreateTask(ctx, &taskspb.CreateTaskRequest{Parent: q.GetName(), Task: &taskspb.Task{
					MessageType: &taskspb.Task_HttpRequest{HttpRequest: &taskspb.HttpRequest{
						Url: srv.URL + "/task", HttpMethod: taskspb.HttpMethod_POST,
						Body: []byte(id + " " + strconv.FormatInt(time.Now().UnixNano(), 10)),
					}},
				}})
				return err
			})
			if err == nil {
				cmu.Lock()
				created[id] = true
				cmu.Unlock()
			}
		})
	}
	return func() {
		deadline := time.Now().Add(2 * time.Minute)
		for {
			mu.Lock()
			missing := 0
			for id := range created {
				if got[id] == 0 {
					missing++
				}
			}
			mu.Unlock()
			if missing == 0 || time.Now().After(deadline) {
				break
			}
			time.Sleep(time.Second)
		}
		time.Sleep(5 * time.Second) // a late second delivery
		mu.Lock()
		lost, twice := 0, 0
		for id := range created {
			switch got[id] {
			case 0:
				lost++
			case 1:
			default:
				twice++
			}
		}
		mu.Unlock()
		e.t.Logf("tasks: %d created, %d never delivered, %d delivered more than once", len(created), lost, twice)
		if lost > 0 {
			e.rec.fail("tasks.delivery", "lost", fmt.Sprintf("%d of %d tasks never delivered", lost, len(created)))
		}
		if twice > 0 {
			e.rec.fail("tasks.delivery", "duplicated", fmt.Sprintf("%d of %d tasks delivered more than once", twice, len(created)))
		}
		ctx, cancel := call()
		defer cancel()
		_ = c.DeleteQueue(ctx, &taskspb.DeleteQueueRequest{Name: q.GetName()})
		_ = c.Close()
		srv.Close()
	}, nil
}

// --- Secret Manager --------------------------------------------------------

// runSecrets: two workers at five calls a second over one secret: mostly
// AccessSecretVersion of latest (90%), some AddSecretVersion (5%), whose
// payload the next access must return, and some ListSecretVersions (5%).
func runSecrets(ctx context.Context, e *env, wg *sync.WaitGroup) (func(), error) {
	addr := endpoint(e.t, envSecrets)
	if addr == "" {
		return nil, nil
	}
	sc, cancel := call()
	defer cancel()
	c, err := secretmanager.NewClient(sc, grpcOpts(addr)...)
	if err != nil {
		return nil, err
	}
	s, err := c.CreateSecret(sc, &smpb.CreateSecretRequest{Parent: "projects/" + e.project, SecretId: "soak",
		Secret: &smpb.Secret{Replication: &smpb.Replication{Replication: &smpb.Replication_Automatic_{Automatic: &smpb.Replication_Automatic{}}}}})
	if err != nil {
		return nil, err
	}
	if _, err := c.AddSecretVersion(sc, &smpb.AddSecretVersionRequest{Parent: s.GetName(), Payload: &smpb.SecretPayload{Data: []byte("v0")}}); err != nil {
		return nil, err
	}
	for w := 0; w < 2; w++ {
		e.every(ctx, wg, 5, func(context.Context) {
			ctx, cancel := call()
			defer cancel()
			switch r := rand.Float64(); {
			case r < 0.90:
				_ = e.rec.do("secrets.access", func() error {
					v, err := c.AccessSecretVersion(ctx, &smpb.AccessSecretVersionRequest{Name: s.GetName() + "/versions/latest"})
					if err == nil && len(v.GetPayload().GetData()) == 0 {
						return errors.New("latest version has an empty payload")
					}
					return err
				})
			case r < 0.95:
				_ = e.rec.do("secrets.add", func() error {
					_, err := c.AddSecretVersion(ctx, &smpb.AddSecretVersionRequest{Parent: s.GetName(),
						Payload: &smpb.SecretPayload{Data: []byte(time.Now().String())}})
					return err
				})
			default:
				_ = e.rec.do("secrets.list", func() error {
					it := c.ListSecretVersions(ctx, &smpb.ListSecretVersionsRequest{Parent: s.GetName(), PageSize: 50})
					_, err := it.Next()
					if errors.Is(err, iterator.Done) {
						return nil
					}
					return err
				})
			}
		})
	}
	return func() {
		ctx, cancel := call()
		defer cancel()
		_ = c.DeleteSecret(ctx, &smpb.DeleteSecretRequest{Name: s.GetName()})
		_ = c.Close()
	}, nil
}

// --- Firestore -------------------------------------------------------------

// runFirestore: three workers at ten calls a second over 200 documents:
// gets (50%), sets (40%) and a range query (10%); and one listener on the
// collection, which must stay attached for the whole run: at the end a
// sentinel write must reach it within ten seconds.
func runFirestore(ctx context.Context, e *env, wg *sync.WaitGroup) (func(), error) {
	addr := endpoint(e.t, envFirestore)
	if addr == "" {
		return nil, nil
	}
	os.Setenv("FIRESTORE_EMULATOR_HOST", addr)
	sc, cancel := call()
	defer cancel()
	c, err := firestore.NewClient(sc, e.project)
	if err != nil {
		return nil, err
	}
	col := c.Collection("soak")

	lctx, lcancel := context.WithCancel(context.Background())
	sentinel := make(chan struct{}, 1)
	ldone := make(chan struct{})
	go func() {
		defer close(ldone)
		for lctx.Err() == nil {
			it := col.Snapshots(lctx)
			for {
				snap, err := it.Next()
				if err != nil {
					if lctx.Err() == nil {
						e.rec.fail("firestore.listen", errorCode(err), "listener ended: "+err.Error())
						time.Sleep(time.Second)
					}
					break
				}
				e.rec.record("firestore.listen", time.Now(), 0, nil)
				for _, ch := range snap.Changes {
					if ch.Doc.Ref.ID == "sentinel" {
						select {
						case sentinel <- struct{}{}:
						default:
						}
					}
				}
			}
			it.Stop()
		}
	}()

	for w := 0; w < 3; w++ {
		e.every(ctx, wg, 10, func(context.Context) {
			ctx, cancel := call()
			defer cancel()
			doc := col.Doc("d" + strconv.Itoa(rand.IntN(200)))
			switch r := rand.Float64(); {
			case r < 0.50:
				_ = e.rec.do("firestore.get", func() error {
					_, err := doc.Get(ctx)
					if status := errorCode(err); err != nil && status == "gRPC NotFound" {
						return nil // not written yet
					}
					return err
				})
			case r < 0.90:
				_ = e.rec.do("firestore.set", func() error {
					_, err := doc.Set(ctx, map[string]any{"n": rand.IntN(1000), "at": time.Now(), "w": w})
					return err
				})
			default:
				_ = e.rec.do("firestore.query", func() error {
					it := col.Where("n", ">", rand.IntN(1000)).Limit(20).Documents(ctx)
					defer it.Stop()
					for {
						_, err := it.Next()
						if errors.Is(err, iterator.Done) {
							return nil
						}
						if err != nil {
							return err
						}
					}
				})
			}
		})
	}
	return func() {
		ctx, cancel := call()
		defer cancel()
		// Drain anything a worker's last write queued, then write the sentinel.
		for len(sentinel) > 0 {
			<-sentinel
		}
		if _, err := col.Doc("sentinel").Set(ctx, map[string]any{"at": time.Now()}); err != nil {
			e.rec.fail("firestore.listen", errorCode(err), "sentinel write: "+err.Error())
		} else {
			select {
			case <-sentinel:
			case <-time.After(10 * time.Second):
				e.rec.fail("firestore.listen", "stale", "the listener did not see a write within 10 s at the end of the run")
			}
		}
		lcancel()
		<-ldone
		_ = c.Close()
	}, nil
}

// --- BigQuery --------------------------------------------------------------

type event struct {
	ID     int64     `bigquery:"id"`
	Worker string    `bigquery:"worker"`
	At     time.Time `bigquery:"at"`
	V      float64   `bigquery:"v"`
}

// runBigQuery: one worker streams ten rows twice a second; one queries the
// table every two seconds, alternating a COUNT and a GROUP BY. At the end
// the table must hold exactly the rows streamed.
func runBigQuery(ctx context.Context, e *env, wg *sync.WaitGroup) (func(), error) {
	addr := endpoint(e.t, envBigQuery)
	if addr == "" {
		return nil, nil
	}
	project := os.Getenv(envBQProject)
	if project == "" {
		e.t.Logf("%s is not set; bigquery does not run", envBQProject)
		return nil, nil
	}
	sc, cancel := call()
	defer cancel()
	c, err := bigquery.NewClient(sc, project, option.WithEndpoint(addr), option.WithoutAuthentication())
	if err != nil {
		return nil, err
	}
	ds := c.Dataset("soak_" + e.suffix)
	if err := ds.Create(sc, nil); err != nil {
		return nil, err
	}
	schema, err := bigquery.InferSchema(event{})
	if err != nil {
		return nil, err
	}
	tbl := ds.Table("events")
	if err := tbl.Create(sc, &bigquery.TableMetadata{Schema: schema}); err != nil {
		return nil, err
	}
	var rows int64
	var rmu sync.Mutex
	next := int64(0)
	e.every(ctx, wg, 2, func(context.Context) {
		ctx, cancel := call()
		defer cancel()
		batch := make([]*bigquery.ValuesSaver, 10)
		for i := range batch {
			batch[i] = &bigquery.ValuesSaver{Schema: schema, InsertID: strconv.FormatInt(next, 10),
				Row: []bigquery.Value{next, "w0", time.Now(), rand.Float64()}}
			next++
		}
		if e.rec.do("bigquery.insert", func() error { return tbl.Inserter().Put(ctx, batch) }) == nil {
			rmu.Lock()
			rows += int64(len(batch))
			rmu.Unlock()
		}
	})
	fq := "`" + project + "." + ds.DatasetID + ".events`"
	i := 0
	count := func(ctx context.Context) (int64, error) {
		it, err := c.Query("SELECT COUNT(*) AS n FROM " + fq).Read(ctx)
		if err != nil {
			return 0, err
		}
		var r struct {
			N int64 `bigquery:"n"`
		}
		return r.N, it.Next(&r)
	}
	e.every(ctx, wg, 0.5, func(context.Context) {
		ctx, cancel := call()
		defer cancel()
		i++
		if i%2 == 0 {
			_ = e.rec.do("bigquery.query.count", func() error { _, err := count(ctx); return err })
			return
		}
		_ = e.rec.do("bigquery.query.groupby", func() error {
			it, err := c.Query("SELECT worker, COUNT(*) AS n, AVG(v) AS v FROM " + fq + " GROUP BY worker").Read(ctx)
			if err != nil {
				return err
			}
			for {
				var r []bigquery.Value
				err := it.Next(&r)
				if errors.Is(err, iterator.Done) {
					return nil
				}
				if err != nil {
					return err
				}
			}
		})
	})
	return func() {
		ctx, cancel := call()
		defer cancel()
		n, err := count(ctx)
		rmu.Lock()
		want := rows
		rmu.Unlock()
		e.t.Logf("bigquery: %d rows streamed, %d counted", want, n)
		if err != nil {
			e.rec.fail("bigquery.rows", errorCode(err), "final count: "+err.Error())
		} else if n != want {
			e.rec.fail("bigquery.rows", "mismatch", fmt.Sprintf("table holds %d rows, %d were streamed", n, want))
		}
		_ = ds.DeleteWithContents(ctx)
		_ = c.Close()
	}, nil
}

// --- Logging ---------------------------------------------------------------

// runLogging: two writers at ten synchronous entries a second each, and one
// reader listing the last minute's entries of the log every two seconds. At
// the end a sentinel entry must be readable.
func runLogging(ctx context.Context, e *env, wg *sync.WaitGroup) (func(), error) {
	addr := endpoint(e.t, envLogging)
	if addr == "" {
		return nil, nil
	}
	parent := "projects/" + e.project
	sc, cancel := call()
	defer cancel()
	lc, err := logging.NewClient(sc, parent, grpcOpts(addr)...)
	if err != nil {
		return nil, err
	}
	ac, err := logadmin.NewClient(sc, parent, grpcOpts(addr)...)
	if err != nil {
		return nil, err
	}
	logID := "soak-" + e.suffix
	lg := lc.Logger(logID, logging.CommonResource(&mrpb.MonitoredResource{Type: "global"}))
	for w := 0; w < 2; w++ {
		seq := 0
		e.every(ctx, wg, 10, func(context.Context) {
			ctx, cancel := call()
			defer cancel()
			seq++
			_ = e.rec.do("logging.write", func() error {
				return lg.LogSync(ctx, logging.Entry{Severity: logging.Info,
					Labels:  map[string]string{"worker": strconv.Itoa(w)},
					Payload: map[string]any{"seq": seq, "msg": "soak entry " + label("w", w)}})
			})
		})
	}
	filter := func(since time.Time) string {
		return fmt.Sprintf(`logName = "%s/logs/%s" AND timestamp >= "%s"`, parent, logID, since.UTC().Format(time.RFC3339))
	}
	e.every(ctx, wg, 0.5, func(context.Context) {
		ctx, cancel := call()
		defer cancel()
		_ = e.rec.do("logging.read", func() error {
			it := ac.Entries(ctx, logadmin.Filter(filter(time.Now().Add(-time.Minute))), logadmin.NewestFirst())
			for i := 0; i < 50; i++ {
				_, err := it.Next()
				if errors.Is(err, iterator.Done) {
					return nil
				}
				if err != nil {
					return err
				}
			}
			return nil
		})
	})
	return func() {
		ctx, cancel := call()
		defer cancel()
		mark := "sentinel-" + e.suffix
		if err := lg.LogSync(ctx, logging.Entry{Payload: mark}); err != nil {
			e.rec.fail("logging.sentinel", errorCode(err), err.Error())
		} else {
			it := ac.Entries(ctx, logadmin.Filter(filter(time.Now().Add(-time.Minute))), logadmin.NewestFirst())
			found := false
			for i := 0; i < 2000 && !found; i++ {
				en, err := it.Next()
				if err != nil {
					break
				}
				found = fmt.Sprint(en.Payload) == mark
			}
			if !found {
				e.rec.fail("logging.sentinel", "lost", "the sentinel entry was not read back")
			}
		}
		_ = ac.DeleteLog(ctx, logID)
		_ = lc.Close()
		_ = ac.Close()
	}, nil
}
