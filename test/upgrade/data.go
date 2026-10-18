//go:build upgrade

package upgrade

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"
	"cloud.google.com/go/bigtable"
	tasks "cloud.google.com/go/cloudtasks/apiv2"
	taskspb "cloud.google.com/go/cloudtasks/apiv2/cloudtaskspb"
	"cloud.google.com/go/datastore"
	"cloud.google.com/go/firestore"
	kms "cloud.google.com/go/kms/apiv1"
	kmspb "cloud.google.com/go/kms/apiv1/kmspb"
	"cloud.google.com/go/logging"
	"cloud.google.com/go/logging/logadmin"
	pubsub "cloud.google.com/go/pubsub/v2"
	pubsubpb "cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	rm "cloud.google.com/go/resourcemanager/apiv3"
	rmpb "cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"
	scheduler "cloud.google.com/go/scheduler/apiv1"
	schedulerpb "cloud.google.com/go/scheduler/apiv1/schedulerpb"
	secretmanager "cloud.google.com/go/secretmanager/apiv1"
	secretmanagerpb "cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"cloud.google.com/go/spanner"
	spannerdb "cloud.google.com/go/spanner/admin/database/apiv1"
	spannerdbpb "cloud.google.com/go/spanner/admin/database/apiv1/databasepb"
	spannerinst "cloud.google.com/go/spanner/admin/instance/apiv1"
	spannerinstpb "cloud.google.com/go/spanner/admin/instance/apiv1/instancepb"
	"cloud.google.com/go/storage"
	_ "github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5"
	"github.com/redis/go-redis/v9"
	"google.golang.org/api/iterator"
)

// The fixed data set, seeded by the older release and read back by this
// one.
const (
	bucketName    = "cb-upgrade-1605"
	topicID       = "upgrade-topic"
	subID         = "upgrade-sub"
	queueID       = "upgrade-queue"
	taskID        = "upgrade-task"
	secretID      = "upgrade-secret"
	keyRingID     = "upgrade-ring"
	cryptoKeyID   = "upgrade-key"
	plaintext     = "the plaintext an older release encrypted"
	jobID         = "upgrade-job"
	projectID2    = "cb-upgrade-proj"
	logID         = "upgrade-log"
	datasetID     = "upgrade_ds"
	tableID       = "t"
	spannerInst   = "upgrade-inst"
	spannerDB     = "upgrade-db"
	bigtableTable = "upgrade-t"
	redisKey      = "upgrade:key"
	sqlTable      = "upgrade_rows"
	location      = "us-central1"
)

// seededCiphertext is what seed encrypted with the Cloud KMS key.
var seededCiphertext []byte

// objects are the Cloud Storage objects, by name.
var objects = map[string]string{"a.txt": "hello from an older release", "dir/b.json": `{"n": 1}`}

// instance is one running CloudBurrow, as the test reaches it.
type instance struct {
	env     env
	project string
	// pgAddr and mysqlDSN are found from the instance's log and env.
	pgAddr string
}

func (in instance) storage(ctx context.Context, t *testing.T) *storage.Client {
	c, err := storage.NewClient(ctx, httpOpts(t, in.env.need(t, "STORAGE_EMULATOR_HOST")+"/storage/v1/")...)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// seed writes the data set through the official clients, and returns each
// service's outcome. A service whose seeding failed is not verified.
func seed(ctx context.Context, t *testing.T, in instance) map[string]error {
	var rs []result
	p := in.project
	check(ctx, &rs, "storage", func(ctx context.Context) error {
		c := in.storage(ctx, t)
		defer c.Close()
		if err := c.Bucket(bucketName).Create(ctx, p, &storage.BucketAttrs{Labels: map[string]string{"from": "upgrade"}}); err != nil {
			return err
		}
		for name, body := range objects {
			w := c.Bucket(bucketName).Object(name).NewWriter(ctx)
			w.ContentType = "text/plain"
			w.Metadata = map[string]string{"seeded": "yes"}
			if _, err := io.WriteString(w, body); err != nil {
				return err
			}
			if err := w.Close(); err != nil {
				return err
			}
		}
		return nil
	})
	check(ctx, &rs, "pubsub", func(ctx context.Context) error {
		c, err := pubsub.NewClient(ctx, p, grpcOpts(t, in.env.need(t, "PUBSUB_EMULATOR_HOST"))...)
		if err != nil {
			return err
		}
		defer c.Close()
		topic := "projects/" + p + "/topics/" + topicID
		if _, err := c.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: topic}); err != nil {
			return err
		}
		if _, err := c.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{
			Name: "projects/" + p + "/subscriptions/" + subID, Topic: topic, AckDeadlineSeconds: 30}); err != nil {
			return err
		}
		pub := c.Publisher(topicID)
		defer pub.Stop()
		for i := range 3 {
			if _, err := pub.Publish(ctx, &pubsub.Message{Data: fmt.Appendf(nil, "backlog %d", i)}).Get(ctx); err != nil {
				return err
			}
		}
		return nil
	})
	check(ctx, &rs, "tasks", func(ctx context.Context) error {
		c, err := tasks.NewClient(ctx, grpcOpts(t, in.env.need(t, "CLOUDBURROW_TASKS_ENDPOINT"))...)
		if err != nil {
			return err
		}
		defer c.Close()
		parent := "projects/" + p + "/locations/" + location
		q, err := c.CreateQueue(ctx, &taskspb.CreateQueueRequest{Parent: parent, Queue: &taskspb.Queue{
			Name: parent + "/queues/" + queueID, RetryConfig: &taskspb.RetryConfig{MaxAttempts: 7}}})
		if err != nil {
			return err
		}
		// Paused, so nothing dispatches the task while it waits.
		if _, err := c.PauseQueue(ctx, &taskspb.PauseQueueRequest{Name: q.Name}); err != nil {
			return err
		}
		_, err = c.CreateTask(ctx, &taskspb.CreateTaskRequest{Parent: q.Name, Task: &taskspb.Task{
			Name: q.Name + "/tasks/" + taskID,
			MessageType: &taskspb.Task_HttpRequest{HttpRequest: &taskspb.HttpRequest{
				Url: "http://upgrade.invalid/work", HttpMethod: taskspb.HttpMethod_POST, Body: []byte("payload")}}}})
		return err
	})
	check(ctx, &rs, "secretmanager", func(ctx context.Context) error {
		c, err := secretmanager.NewClient(ctx, grpcOpts(t, in.env.need(t, "CLOUDBURROW_SECRETMANAGER_ENDPOINT"))...)
		if err != nil {
			return err
		}
		defer c.Close()
		s, err := c.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{Parent: "projects/" + p, SecretId: secretID,
			Secret: &secretmanagerpb.Secret{Replication: &secretmanagerpb.Replication{
				Replication: &secretmanagerpb.Replication_Automatic_{Automatic: &secretmanagerpb.Replication_Automatic{}}},
				Labels: map[string]string{"from": "upgrade"}}})
		if err != nil {
			return err
		}
		for _, v := range []string{"first", "second", "third"} {
			if _, err := c.AddSecretVersion(ctx, &secretmanagerpb.AddSecretVersionRequest{Parent: s.Name,
				Payload: &secretmanagerpb.SecretPayload{Data: []byte(v)}}); err != nil {
				return err
			}
		}
		if _, err := c.DisableSecretVersion(ctx, &secretmanagerpb.DisableSecretVersionRequest{Name: s.Name + "/versions/1"}); err != nil {
			return err
		}
		_, err = c.DestroySecretVersion(ctx, &secretmanagerpb.DestroySecretVersionRequest{Name: s.Name + "/versions/2"})
		return err
	})
	check(ctx, &rs, "kms", func(ctx context.Context) error {
		c, err := kms.NewKeyManagementClient(ctx, grpcOpts(t, in.env.need(t, "CLOUDBURROW_KMS_ENDPOINT"))...)
		if err != nil {
			return err
		}
		defer c.Close()
		parent := "projects/" + p + "/locations/global"
		ring, err := c.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: parent, KeyRingId: keyRingID})
		if err != nil {
			return err
		}
		key, err := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.Name, CryptoKeyId: cryptoKeyID,
			CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}})
		if err != nil {
			return err
		}
		enc, err := c.Encrypt(ctx, &kmspb.EncryptRequest{Name: key.Name, Plaintext: []byte(plaintext)})
		if err != nil {
			return err
		}
		// The ciphertext is kept, to decrypt after the upgrade: here, and in
		// Cloud Storage for a check by hand from another process.
		seededCiphertext = enc.Ciphertext
		sc := in.storage(ctx, t)
		defer sc.Close()
		w := sc.Bucket(bucketName).Object("kms-ciphertext").NewWriter(ctx)
		if _, err := w.Write(enc.Ciphertext); err != nil {
			return err
		}
		return w.Close()
	})
	check(ctx, &rs, "scheduler", func(ctx context.Context) error {
		c, err := scheduler.NewCloudSchedulerClient(ctx, grpcOpts(t, in.env.need(t, "CLOUDBURROW_SCHEDULER_ENDPOINT"))...)
		if err != nil {
			return err
		}
		defer c.Close()
		parent := "projects/" + p + "/locations/" + location
		j, err := c.CreateJob(ctx, &schedulerpb.CreateJobRequest{Parent: parent, Job: &schedulerpb.Job{
			Name: parent + "/jobs/" + jobID, Schedule: "0 3 * * *", TimeZone: "Etc/UTC", Description: "from an older release",
			Target: &schedulerpb.Job_HttpTarget{HttpTarget: &schedulerpb.HttpTarget{Uri: "http://upgrade.invalid/cron"}}}})
		if err != nil {
			return err
		}
		_, err = c.PauseJob(ctx, &schedulerpb.PauseJobRequest{Name: j.Name})
		return err
	})
	check(ctx, &rs, "projects", func(ctx context.Context) error {
		c, err := rm.NewProjectsRESTClient(ctx, httpOpts(t, "http://"+in.env.need(t, "CLOUDBURROW_RESOURCEMANAGER_ENDPOINT"))...)
		if err != nil {
			return err
		}
		defer c.Close()
		op, err := c.CreateProject(ctx, &rmpb.CreateProjectRequest{Project: &rmpb.Project{ProjectId: projectID2, DisplayName: "Upgrade"}})
		if err != nil {
			return err
		}
		_, err = op.Wait(ctx)
		return err
	})
	check(ctx, &rs, "logging", func(ctx context.Context) error {
		c, err := logging.NewClient(ctx, "projects/"+p, grpcOpts(t, in.env.need(t, "CLOUDBURROW_LOGGING_ENDPOINT"))...)
		if err != nil {
			return err
		}
		defer c.Close()
		l := c.Logger(logID)
		for i := range 2 {
			if err := l.LogSync(ctx, logging.Entry{Payload: fmt.Sprintf("entry %d", i), Severity: logging.Warning}); err != nil {
				return err
			}
		}
		return nil
	})
	check(ctx, &rs, "bigquery", func(ctx context.Context) error {
		c, err := bigquery.NewClient(ctx, p, httpOpts(t, in.env.need(t, "CLOUDBURROW_BIGQUERY_ENDPOINT"))...)
		if err != nil {
			return err
		}
		defer c.Close()
		if err := c.Dataset(datasetID).Create(ctx, &bigquery.DatasetMetadata{Location: "US"}); err != nil {
			return err
		}
		if err := c.Dataset(datasetID).Table(tableID).Create(ctx, &bigquery.TableMetadata{Schema: bigquery.Schema{
			{Name: "name", Type: bigquery.StringFieldType}, {Name: "n", Type: bigquery.IntegerFieldType}}}); err != nil {
			return err
		}
		return runQuery(ctx, c, fmt.Sprintf("INSERT INTO `%s.%s.%s` (name, n) VALUES ('a', 1), ('b', 2)", p, datasetID, tableID))
	})
	check(ctx, &rs, "firestore", func(ctx context.Context) error {
		in.env.need(t, "FIRESTORE_EMULATOR_HOST")
		t.Setenv("FIRESTORE_EMULATOR_HOST", in.env["FIRESTORE_EMULATOR_HOST"])
		c, err := firestore.NewClient(ctx, p)
		if err != nil {
			return err
		}
		defer c.Close()
		_, err = c.Collection("upgrade").Doc("one").Set(ctx, map[string]any{"n": 1, "s": "older"})
		return err
	})
	check(ctx, &rs, "datastore", func(ctx context.Context) error {
		in.env.need(t, "DATASTORE_EMULATOR_HOST")
		t.Setenv("DATASTORE_EMULATOR_HOST", in.env["DATASTORE_EMULATOR_HOST"])
		c, err := datastore.NewClient(ctx, p)
		if err != nil {
			return err
		}
		defer c.Close()
		_, err = c.Put(ctx, datastore.NameKey("Upgrade", "one", nil), &dsEntity{N: 1, S: "older"})
		return err
	})
	check(ctx, &rs, "spanner", func(ctx context.Context) error { return seedSpanner(ctx, t, in) })
	check(ctx, &rs, "bigtable", func(ctx context.Context) error {
		in.env.need(t, "BIGTABLE_EMULATOR_HOST")
		t.Setenv("BIGTABLE_EMULATOR_HOST", in.env["BIGTABLE_EMULATOR_HOST"])
		ac, err := bigtable.NewAdminClient(ctx, p, "upgrade")
		if err != nil {
			return err
		}
		defer ac.Close()
		if err := ac.CreateTable(ctx, bigtableTable); err != nil {
			return err
		}
		if err := ac.CreateColumnFamily(ctx, bigtableTable, "f"); err != nil {
			return err
		}
		c, err := bigtable.NewClient(ctx, p, "upgrade")
		if err != nil {
			return err
		}
		defer c.Close()
		m := bigtable.NewMutation()
		m.Set("f", "c", bigtable.Now(), []byte("older"))
		return c.Open(bigtableTable).Apply(ctx, "r1", m)
	})
	check(ctx, &rs, "cloudsql", func(ctx context.Context) error {
		if in.pgAddr == "" {
			return errors.New("no PostgreSQL address in the instance's log")
		}
		conn, err := pgx.Connect(ctx, "postgres://cloudburrow@"+in.pgAddr+"/cloudburrow?sslmode=disable")
		if err != nil {
			return err
		}
		defer conn.Close(ctx)
		_, err = conn.Exec(ctx, "CREATE TABLE "+sqlTable+" (id int PRIMARY KEY, v text); INSERT INTO "+sqlTable+" VALUES (1, 'one'), (2, 'two')")
		return err
	})
	check(ctx, &rs, "cloudsql-mysql", func(ctx context.Context) error {
		db, err := sql.Open("mysql", in.mysqlDSN(t))
		if err != nil {
			return err
		}
		defer db.Close()
		if _, err := db.ExecContext(ctx, "CREATE TABLE "+sqlTable+" (id int PRIMARY KEY, v text)"); err != nil {
			return err
		}
		_, err = db.ExecContext(ctx, "INSERT INTO "+sqlTable+" VALUES (1, 'one'), (2, 'two')")
		return err
	})
	check(ctx, &rs, "memorystore", func(ctx context.Context) error {
		c := redis.NewClient(&redis.Options{Addr: in.env.need(t, "REDIS_HOST") + ":" + in.env.need(t, "REDIS_PORT")})
		defer c.Close()
		return c.Set(ctx, redisKey, "older", 0).Err()
	})
	out := map[string]error{}
	for _, r := range rs {
		t.Logf("seed %s", r)
		out[r.service] = r.err
	}
	return out
}

type dsEntity struct {
	N int
	S string
}

func (in instance) mysqlDSN(t *testing.T) string {
	return fmt.Sprintf("%s:%s@tcp(%s:%s)/%s", in.env.need(t, "MYSQL_USER"), in.env.need(t, "MYSQL_PASSWORD"),
		in.env.need(t, "MYSQL_HOST"), in.env.need(t, "MYSQL_PORT"), in.env.need(t, "MYSQL_DATABASE"))
}

func runQuery(ctx context.Context, c *bigquery.Client, q string) error {
	job, err := c.Query(q).Run(ctx)
	if err != nil {
		return err
	}
	st, err := job.Wait(ctx)
	if err != nil {
		return err
	}
	return st.Err()
}

func seedSpanner(ctx context.Context, t *testing.T, in instance) error {
	host := in.env.need(t, "SPANNER_EMULATOR_HOST")
	p := in.project
	ic, err := spannerinst.NewInstanceAdminClient(ctx, grpcOpts(t, host)...)
	if err != nil {
		return err
	}
	defer ic.Close()
	op, err := ic.CreateInstance(ctx, &spannerinstpb.CreateInstanceRequest{Parent: "projects/" + p, InstanceId: spannerInst,
		Instance: &spannerinstpb.Instance{Config: "projects/" + p + "/instanceConfigs/emulator-config", DisplayName: "upgrade", NodeCount: 1}})
	if err != nil {
		return err
	}
	if _, err := op.Wait(ctx); err != nil {
		return err
	}
	dc, err := spannerdb.NewDatabaseAdminClient(ctx, grpcOpts(t, host)...)
	if err != nil {
		return err
	}
	defer dc.Close()
	dop, err := dc.CreateDatabase(ctx, &spannerdbpb.CreateDatabaseRequest{Parent: "projects/" + p + "/instances/" + spannerInst,
		CreateStatement: "CREATE DATABASE `" + spannerDB + "`", ExtraStatements: []string{"CREATE TABLE T (id INT64, v STRING(MAX)) PRIMARY KEY (id)"}})
	if err != nil {
		return err
	}
	if _, err := dop.Wait(ctx); err != nil {
		return err
	}
	c, err := spanner.NewClient(ctx, spannerDBName(p), grpcOpts(t, host)...)
	if err != nil {
		return err
	}
	defer c.Close()
	_, err = c.Apply(ctx, []*spanner.Mutation{spanner.Insert("T", []string{"id", "v"}, []any{int64(1), "older"})})
	return err
}

func spannerDBName(p string) string {
	return "projects/" + p + "/instances/" + spannerInst + "/databases/" + spannerDB
}

// verify reads the data set back through the official clients. It checks
// only the services in want, the ones seeded and expected to have kept
// their state, and returns each one's outcome.
func verify(ctx context.Context, t *testing.T, in instance, want map[string]bool) map[string]error {
	var rs []result
	p := in.project
	run := func(service string, f func(ctx context.Context) error) {
		if want[service] {
			check(ctx, &rs, service, f)
		}
	}
	run("storage", func(ctx context.Context) error {
		c := in.storage(ctx, t)
		defer c.Close()
		attrs, err := c.Bucket(bucketName).Attrs(ctx)
		if err != nil {
			return err
		}
		_ = attrs // the older release's server did not keep bucket labels
		for name, body := range objects {
			o := c.Bucket(bucketName).Object(name)
			r, err := o.NewReader(ctx)
			if err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			got, err := io.ReadAll(r)
			_ = r.Close()
			if err != nil {
				return err
			}
			if string(got) != body {
				return fmt.Errorf("%s: %q, want %q", name, got, body)
			}
			a, err := o.Attrs(ctx)
			if err != nil {
				return err
			}
			if a.ContentType != "text/plain" || a.Metadata["seeded"] != "yes" {
				return fmt.Errorf("%s: content type %q metadata %v", name, a.ContentType, a.Metadata)
			}
		}
		return nil
	})
	run("pubsub", func(ctx context.Context) error {
		c, err := pubsub.NewClient(ctx, p, grpcOpts(t, in.env.need(t, "PUBSUB_EMULATOR_HOST"))...)
		if err != nil {
			return err
		}
		defer c.Close()
		s, err := c.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: "projects/" + p + "/subscriptions/" + subID})
		if err != nil {
			return err
		}
		if s.AckDeadlineSeconds != 30 {
			return fmt.Errorf("ack deadline %d", s.AckDeadlineSeconds)
		}
		return nil
	})
	run("tasks", func(ctx context.Context) error {
		c, err := tasks.NewClient(ctx, grpcOpts(t, in.env.need(t, "CLOUDBURROW_TASKS_ENDPOINT"))...)
		if err != nil {
			return err
		}
		defer c.Close()
		qn := "projects/" + p + "/locations/" + location + "/queues/" + queueID
		q, err := c.GetQueue(ctx, &taskspb.GetQueueRequest{Name: qn})
		if err != nil {
			return err
		}
		if q.State != taskspb.Queue_PAUSED || q.GetRetryConfig().GetMaxAttempts() != 7 {
			return fmt.Errorf("queue state %v retry %v", q.State, q.RetryConfig)
		}
		task, err := c.GetTask(ctx, &taskspb.GetTaskRequest{Name: qn + "/tasks/" + taskID, ResponseView: taskspb.Task_FULL})
		if err != nil {
			return err
		}
		if string(task.GetHttpRequest().GetBody()) != "payload" {
			return fmt.Errorf("task body %q", task.GetHttpRequest().GetBody())
		}
		return nil
	})
	run("secretmanager", func(ctx context.Context) error {
		c, err := secretmanager.NewClient(ctx, grpcOpts(t, in.env.need(t, "CLOUDBURROW_SECRETMANAGER_ENDPOINT"))...)
		if err != nil {
			return err
		}
		defer c.Close()
		name := "projects/" + p + "/secrets/" + secretID
		s, err := c.GetSecret(ctx, &secretmanagerpb.GetSecretRequest{Name: name})
		if err != nil {
			return err
		}
		if s.Labels["from"] != "upgrade" {
			return fmt.Errorf("labels %v", s.Labels)
		}
		states := map[string]secretmanagerpb.SecretVersion_State{}
		it := c.ListSecretVersions(ctx, &secretmanagerpb.ListSecretVersionsRequest{Parent: name})
		for {
			v, err := it.Next()
			if errors.Is(err, iterator.Done) {
				break
			}
			if err != nil {
				return err
			}
			if n := v.Name[len(name)+len("/versions/"):]; n <= "3" && len(n) == 1 {
				states[n] = v.State
			}
		}
		want := map[string]secretmanagerpb.SecretVersion_State{"1": secretmanagerpb.SecretVersion_DISABLED,
			"2": secretmanagerpb.SecretVersion_DESTROYED, "3": secretmanagerpb.SecretVersion_ENABLED}
		if fmt.Sprint(states) != fmt.Sprint(want) {
			return fmt.Errorf("versions %v, want %v", states, want)
		}
		got, err := c.AccessSecretVersion(ctx, &secretmanagerpb.AccessSecretVersionRequest{Name: name + "/versions/3"})
		if err != nil {
			return err
		}
		if string(got.Payload.Data) != "third" {
			return fmt.Errorf("version 3 is %q", got.Payload.Data)
		}
		// A new version after the upgrade is numbered after the old ones.
		v, err := c.AddSecretVersion(ctx, &secretmanagerpb.AddSecretVersionRequest{Parent: name,
			Payload: &secretmanagerpb.SecretPayload{Data: []byte("fourth")}})
		if err != nil {
			return err
		}
		if n := v.Name[strings.LastIndex(v.Name, "/")+1:]; len(n) == 1 && n <= "3" {
			return fmt.Errorf("a version added after the upgrade is %s", v.Name)
		}
		_, err = c.DestroySecretVersion(ctx, &secretmanagerpb.DestroySecretVersionRequest{Name: v.Name})
		return err
	})
	run("kms", func(ctx context.Context) error {
		c, err := kms.NewKeyManagementClient(ctx, grpcOpts(t, in.env.need(t, "CLOUDBURROW_KMS_ENDPOINT"))...)
		if err != nil {
			return err
		}
		defer c.Close()
		ct := seededCiphertext
		if ct == nil {
			sc := in.storage(ctx, t)
			defer sc.Close()
			r, err := sc.Bucket(bucketName).Object("kms-ciphertext").NewReader(ctx)
			if err != nil {
				return fmt.Errorf("read the ciphertext: %w", err)
			}
			ct, _ = io.ReadAll(r)
			_ = r.Close()
		}
		key := "projects/" + p + "/locations/global/keyRings/" + keyRingID + "/cryptoKeys/" + cryptoKeyID
		dec, err := c.Decrypt(ctx, &kmspb.DecryptRequest{Name: key, Ciphertext: ct})
		if err != nil {
			return err
		}
		if !bytes.Equal(dec.Plaintext, []byte(plaintext)) {
			return fmt.Errorf("decrypted %q", dec.Plaintext)
		}
		return nil
	})
	run("scheduler", func(ctx context.Context) error {
		c, err := scheduler.NewCloudSchedulerClient(ctx, grpcOpts(t, in.env.need(t, "CLOUDBURROW_SCHEDULER_ENDPOINT"))...)
		if err != nil {
			return err
		}
		defer c.Close()
		j, err := c.GetJob(ctx, &schedulerpb.GetJobRequest{Name: "projects/" + p + "/locations/" + location + "/jobs/" + jobID})
		if err != nil {
			return err
		}
		if j.State != schedulerpb.Job_PAUSED || j.Schedule != "0 3 * * *" || j.GetHttpTarget().GetUri() != "http://upgrade.invalid/cron" {
			return fmt.Errorf("job %v %q %v", j.State, j.Schedule, j.Target)
		}
		return nil
	})
	run("projects", func(ctx context.Context) error {
		c, err := rm.NewProjectsRESTClient(ctx, httpOpts(t, "http://"+in.env.need(t, "CLOUDBURROW_RESOURCEMANAGER_ENDPOINT"))...)
		if err != nil {
			return err
		}
		defer c.Close()
		pr, err := c.GetProject(ctx, &rmpb.GetProjectRequest{Name: "projects/" + projectID2})
		if err != nil {
			// The v3 API also names a project by its number; search by ID.
			it := c.SearchProjects(ctx, &rmpb.SearchProjectsRequest{Query: "id:" + projectID2})
			pr, err = it.Next()
			if err != nil {
				return err
			}
		}
		if pr.DisplayName != "Upgrade" {
			return fmt.Errorf("project %v", pr)
		}
		return nil
	})
	run("logging", func(ctx context.Context) error {
		c, err := logadmin.NewClient(ctx, "projects/"+p, grpcOpts(t, in.env.need(t, "CLOUDBURROW_LOGGING_ENDPOINT"))...)
		if err != nil {
			return err
		}
		defer c.Close()
		it := c.Entries(ctx, logadmin.Filter(fmt.Sprintf(`logName="projects/%s/logs/%s"`, p, logID)))
		n := 0
		for {
			_, err := it.Next()
			if errors.Is(err, iterator.Done) {
				break
			}
			if err != nil {
				return err
			}
			n++
		}
		if n != 2 {
			return fmt.Errorf("%d entries, want 2", n)
		}
		return nil
	})
	run("bigquery", func(ctx context.Context) error {
		c, err := bigquery.NewClient(ctx, p, httpOpts(t, in.env.need(t, "CLOUDBURROW_BIGQUERY_ENDPOINT"))...)
		if err != nil {
			return err
		}
		defer c.Close()
		it, err := c.Query(fmt.Sprintf("SELECT name FROM `%s.%s.%s` ORDER BY n", p, datasetID, tableID)).Read(ctx)
		if err != nil {
			return err
		}
		var names []string
		for {
			var row []bigquery.Value
			err := it.Next(&row)
			if errors.Is(err, iterator.Done) {
				break
			}
			if err != nil {
				return err
			}
			names = append(names, fmt.Sprint(row[0]))
		}
		if strings.Join(names, ",") != "a,b" {
			return fmt.Errorf("rows %v", names)
		}
		return nil
	})
	run("firestore", func(ctx context.Context) error {
		t.Setenv("FIRESTORE_EMULATOR_HOST", in.env.need(t, "FIRESTORE_EMULATOR_HOST"))
		c, err := firestore.NewClient(ctx, p)
		if err != nil {
			return err
		}
		defer c.Close()
		d, err := c.Collection("upgrade").Doc("one").Get(ctx)
		if err != nil {
			return err
		}
		if d.Data()["s"] != "older" {
			return fmt.Errorf("document %v", d.Data())
		}
		return nil
	})
	run("datastore", func(ctx context.Context) error {
		t.Setenv("DATASTORE_EMULATOR_HOST", in.env.need(t, "DATASTORE_EMULATOR_HOST"))
		c, err := datastore.NewClient(ctx, p)
		if err != nil {
			return err
		}
		defer c.Close()
		var e dsEntity
		if err := c.Get(ctx, datastore.NameKey("Upgrade", "one", nil), &e); err != nil {
			return err
		}
		if e.S != "older" {
			return fmt.Errorf("entity %+v", e)
		}
		return nil
	})
	run("spanner", func(ctx context.Context) error {
		c, err := spanner.NewClient(ctx, spannerDBName(p), grpcOpts(t, in.env.need(t, "SPANNER_EMULATOR_HOST"))...)
		if err != nil {
			return err
		}
		defer c.Close()
		row, err := c.Single().ReadRow(ctx, "T", spanner.Key{int64(1)}, []string{"v"})
		if err != nil {
			return err
		}
		var v string
		if err := row.Column(0, &v); err != nil || v != "older" {
			return fmt.Errorf("row %q %v", v, err)
		}
		return nil
	})
	run("bigtable", func(ctx context.Context) error {
		t.Setenv("BIGTABLE_EMULATOR_HOST", in.env.need(t, "BIGTABLE_EMULATOR_HOST"))
		c, err := bigtable.NewClient(ctx, p, "upgrade")
		if err != nil {
			return err
		}
		defer c.Close()
		row, err := c.Open(bigtableTable).ReadRow(ctx, "r1")
		if err != nil {
			return err
		}
		if len(row["f"]) != 1 || string(row["f"][0].Value) != "older" {
			return fmt.Errorf("row %v", row)
		}
		return nil
	})
	run("cloudsql", func(ctx context.Context) error {
		if in.pgAddr == "" {
			return errors.New("no PostgreSQL address in the instance's log")
		}
		conn, err := pgx.Connect(ctx, "postgres://cloudburrow@"+in.pgAddr+"/cloudburrow?sslmode=disable")
		if err != nil {
			return err
		}
		defer conn.Close(ctx)
		return sqlRows(func() (*sqlScan, error) {
			rows, err := conn.Query(ctx, "SELECT v FROM "+sqlTable+" ORDER BY id")
			if err != nil {
				return nil, err
			}
			s := &sqlScan{}
			for rows.Next() {
				var v string
				if err := rows.Scan(&v); err != nil {
					return nil, err
				}
				s.vs = append(s.vs, v)
			}
			return s, rows.Err()
		})
	})
	run("cloudsql-mysql", func(ctx context.Context) error {
		db, err := sql.Open("mysql", in.mysqlDSN(t))
		if err != nil {
			return err
		}
		defer db.Close()
		return sqlRows(func() (*sqlScan, error) {
			rows, err := db.QueryContext(ctx, "SELECT v FROM "+sqlTable+" ORDER BY id")
			if err != nil {
				return nil, err
			}
			defer rows.Close()
			s := &sqlScan{}
			for rows.Next() {
				var v string
				if err := rows.Scan(&v); err != nil {
					return nil, err
				}
				s.vs = append(s.vs, v)
			}
			return s, rows.Err()
		})
	})
	run("memorystore", func(ctx context.Context) error {
		c := redis.NewClient(&redis.Options{Addr: in.env.need(t, "REDIS_HOST") + ":" + in.env.need(t, "REDIS_PORT")})
		defer c.Close()
		v, err := c.Get(ctx, redisKey).Result()
		if err != nil {
			return err
		}
		if v != "older" {
			return fmt.Errorf("%s = %q", redisKey, v)
		}
		return nil
	})
	out := map[string]error{}
	for _, r := range rs {
		t.Logf("verify %s", r)
		out[r.service] = r.err
	}
	return out
}

type sqlScan struct{ vs []string }

func sqlRows(q func() (*sqlScan, error)) error {
	s, err := q()
	if err != nil {
		return err
	}
	if strings.Join(s.vs, ",") != "one,two" {
		return fmt.Errorf("rows %v", s.vs)
	}
	return nil
}

// sortedKeys is m's keys, sorted, for stable logs.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
