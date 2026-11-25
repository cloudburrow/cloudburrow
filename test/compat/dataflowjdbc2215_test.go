//go:build compat

package compat

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	computeapi "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	"cloud.google.com/go/dataflow/apiv1beta3/dataflowpb"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"cloud.google.com/go/storage"
	sqladmin "google.golang.org/api/sqladmin/v1"
	"google.golang.org/protobuf/proto"
)

// The PostgreSQL JDBC driver the JDBC templates are given (driverJars),
// from Maven Central (BSD-2-Clause), checked against its SHA-256.
const (
	pgJDBCURL    = "https://repo1.maven.org/maven2/org/postgresql/postgresql/42.6.1/postgresql-42.6.1.jar"
	pgJDBCSHA256 = "cb0774fd7d09b239e21e9d04b3744a40877bff463a8d58c3f407cf7d976c3557"
)

// jdbcSource is a Cloud SQL for PostgreSQL instance with a private IP on
// the project's default network, holding the table people (name, age):
// the source the JDBC templates read, as Google's guide to them has one
// ("The relational database must be accessible from the subnet where
// Dataflow runs").
type jdbcSource struct {
	instance, privateIP string
	db                  *sql.DB
}

// newJDBCSource makes the instance (through sqladmin/v1, on a VPC_PEERING
// range of default made through compute/apiv1) and its table (through
// the official Go connector) in project p, the jobs' (BigQuery's one
// project), deleted when the test ends.
func newJDBCSource(t *testing.T, h *Harness, ctx context.Context, p, suffix string) jdbcSource {
	t.Helper()
	opts := sqlComputeOpts(t, h)
	svc := sqlAdminService(t, h)
	gaddrs, err := computeapi.NewGlobalAddressesRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	rng := "df-jdbc-psa-" + suffix
	op, err := gaddrs.Insert(ctx, &computepb.InsertGlobalAddressRequest{Project: p, AddressResource: &computepb.Address{
		Name: proto.String(rng), AddressType: proto.String("INTERNAL"), Purpose: proto.String("VPC_PEERING"),
		PrefixLength: proto.Int32(24), Network: proto.String("global/networks/default")}})
	if err != nil {
		t.Fatal(err)
	}
	if err := op.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	name := "df-jdbc-" + suffix
	in := createSQLInstance(t, svc, p, &sqladmin.DatabaseInstance{Name: name, DatabaseVersion: "POSTGRES_17", Region: "us-central1",
		Settings: &sqladmin.Settings{Tier: "db-custom-1-3840", IpConfiguration: &sqladmin.IpConfiguration{Ipv4Enabled: true,
			PrivateNetwork: "projects/" + p + "/global/networks/default", AllocatedIpRange: rng}}})
	// The range is released once the instance is gone (createSQLInstance's
	// cleanup runs first).
	t.Cleanup(func() {
		_, _ = gaddrs.Delete(context.Background(), &computepb.DeleteGlobalAddressRequest{Project: p, Address: rng})
	})
	src := jdbcSource{instance: name}
	for _, a := range in.IpAddresses {
		if a.Type == "PRIVATE" {
			src.privateIP = a.IpAddress
		}
	}
	if src.privateIP == "" {
		t.Fatalf("the instance's addresses %+v have no PRIVATE one", in.IpAddresses)
	}
	src.db = sqlEngine{}.root(t, h, in.ConnectionName)
	eventually(t, "create the tables", func() error {
		_, err := src.db.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS people (name TEXT, age INTEGER); TRUNCATE people; "+
			"INSERT INTO people VALUES ('Ada', 36), ('Grace', 45); CREATE TABLE IF NOT EXISTS arrivals (name TEXT, age INTEGER)")
		return err
	})
	return src
}

// url is the source's JDBC connection URL, at its private IP.
func (s jdbcSource) url() string { return "jdbc:postgresql://" + s.privateIP + ":5432/postgres" }

// putJDBCDriver writes the PostgreSQL JDBC driver to bucket, answering
// its gs:// path.
func putJDBCDriver(t *testing.T, ctx context.Context, sc *storage.Client, bucket string) string {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pgJDBCURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("download the PostgreSQL JDBC driver: %v", err)
	}
	b, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("download the PostgreSQL JDBC driver: %v %s", err, resp.Status)
	}
	if sum := sha256.Sum256(b); hex.EncodeToString(sum[:]) != pgJDBCSHA256 {
		t.Fatalf("the PostgreSQL JDBC driver's SHA-256 is %x; want %s", sum, pgJDBCSHA256)
	}
	w := sc.Bucket(bucket).Object("drivers/postgresql.jar").NewWriter(ctx)
	if _, err := io.Copy(w, bytes.NewReader(b)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return "gs://" + bucket + "/drivers/postgresql.jar"
}

// covers: google.dataflow.v1beta3.TemplatesService/LaunchTemplate, google.dataflow.v1beta3.FlexTemplatesService/LaunchFlexTemplate
//
// TestDataflowProvidedJdbcToBigQuery (#2215): Google's JDBC templates.
// JDBC to BigQuery
// templates (templates/provided/jdbc-to-bigquery), the classic
// Jdbc_to_BigQuery and the Flex Jdbc_to_BigQuery_Flex (v2 module
// jdbc-and-googlecloud), read a Cloud SQL for PostgreSQL instance at its
// private IP, from the workers on the project's default network, with
// the PostgreSQL driver from Cloud Storage, into BigQuery tables. A job
// whose workers run on another network (environment.network) does not
// reach the instance, and fails. The Flex Templates JDBC to Pub/Sub
// (Jdbc_to_PubSub, templates/provided/jdbc-to-pubsub) publishes the
// query's rows as JSON messages, and Pub/Sub to JDBC (Pubsub_to_Jdbc,
// templates/provided/pubsub-to-jdbc) streams a subscription's JSON
// messages into the instance through a prepared statement, and drains.
func TestDataflowProvidedJdbcToBigQuery(t *testing.T) {
	h, _, c := dataflowEnv(t)
	h.Endpoint(EnvStorage)
	h.Endpoint(EnvBigQuery)
	bq, project := bigqueryClient(t, h)
	sc := storageClient(t, h)
	// The first use of the Flex Template builds its module with Maven.
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Minute)
	defer cancel()
	suffix := fmt.Sprint(time.Now().UnixNano() % 1e6)
	src := newJDBCSource(t, h, ctx, project, suffix)
	bucket := fmt.Sprintf("%s-dataflow-jdbc-%s", h.Project(), suffix)
	dataflowBucket(t, ctx, sc, h.Project(), bucket)
	driver := putJDBCDriver(t, ctx, sc, bucket)
	ds := "provided_jdbc_" + suffix
	if err := bq.Dataset(ds).Create(ctx, nil); err != nil {
		t.Fatal(err)
	}
	table := func(name string) string {
		t.Helper()
		schema := bigquery.Schema{{Name: "name", Type: bigquery.StringFieldType}, {Name: "age", Type: bigquery.IntegerFieldType}}
		if err := bq.Dataset(ds).Table(name).Create(ctx, &bigquery.TableMetadata{Schema: schema}); err != nil {
			t.Fatal(err)
		}
		return project + ":" + ds + "." + name
	}
	params := func(out string) map[string]string {
		return map[string]string{"driverJars": driver, "driverClassName": "org.postgresql.Driver", "connectionURL": src.url(),
			"username": "postgres", "password": "x", "query": "SELECT name, age FROM people", "outputTable": out,
			"bigQueryLoadingTemporaryDirectory": "gs://" + bucket + "/bqtmp"}
	}
	launch := func(t *testing.T, job string, p map[string]string, env *dataflowpb.RuntimeEnvironment) string {
		t.Helper()
		resp, err := c.tmpl.LaunchTemplate(ctx, &dataflowpb.LaunchTemplateRequest{ProjectId: project, Location: "us-central1",
			Template:         &dataflowpb.LaunchTemplateRequest_GcsPath{GcsPath: "gs://dataflow-templates-us-central1/latest/Jdbc_to_BigQuery"},
			LaunchParameters: &dataflowpb.LaunchTemplateParameters{JobName: job + "-" + suffix, Parameters: p, Environment: env}})
		if err != nil {
			t.Fatalf("LaunchTemplate(Jdbc_to_BigQuery): %v", err)
		}
		return resp.GetJob().GetId()
	}

	t.Run("Jdbc_to_BigQuery", func(t *testing.T) {
		out := table("classic")
		done(t, ctx, c, project, "Jdbc_to_BigQuery", launch(t, "provided-jdbc", params(out),
			&dataflowpb.RuntimeEnvironment{TempLocation: "gs://" + bucket + "/tmp"}))
		if got := providedRows(t, ctx, bq, "SELECT name, age FROM `"+project+"."+ds+".classic` ORDER BY name"); got != "Ada=36,Grace=45" {
			t.Errorf("the table has %s; want Ada=36,Grace=45", got)
		}
	})

	t.Run("OtherNetwork", func(t *testing.T) {
		h := h.For(t)
		// A network with no private services access to the instance: its
		// workers have no route to the private IP.
		network := "df-jdbc-other-" + suffix
		computeNetwork(t, sqlComputeEndpoint(h), project, network, "10.93.0.0/24", "")
		out := table("other")
		id := launch(t, "provided-jdbc-other", params(out), &dataflowpb.RuntimeEnvironment{TempLocation: "gs://" + bucket + "/tmp",
			Network: network, Subnetwork: "regions/us-central1/subnetworks/" + network + "-s"})
		j := waitDataflowJob(t, ctx, c, project, id, dataflowpb.JobState_JOB_STATE_DONE, dataflowpb.JobState_JOB_STATE_FAILED)
		if j.CurrentState != dataflowpb.JobState_JOB_STATE_FAILED {
			t.Fatalf("the job on %s is %s; want FAILED, as its workers cannot reach %s:\n%s", network, j.CurrentState, src.privateIP,
				jobMessages(t, ctx, c, project, id))
		}
		if got := providedRows(t, ctx, bq, "SELECT name, age FROM `"+project+"."+ds+".other` ORDER BY name"); got != "" {
			t.Errorf("the table has %s; want no rows", got)
		}
	})

	t.Run("Jdbc_to_BigQuery_Flex", func(t *testing.T) {
		out := table("flex")
		p := params(out)
		resp, err := c.flex.LaunchFlexTemplate(ctx, &dataflowpb.LaunchFlexTemplateRequest{ProjectId: project, Location: "us-central1",
			LaunchParameter: &dataflowpb.LaunchFlexTemplateParameter{JobName: "provided-jdbc-flex-" + suffix,
				Template:    &dataflowpb.LaunchFlexTemplateParameter_ContainerSpecGcsPath{ContainerSpecGcsPath: "gs://dataflow-templates-us-central1/latest/flex/Jdbc_to_BigQuery_Flex"},
				Parameters:  p,
				Environment: &dataflowpb.FlexTemplateRuntimeEnvironment{TempLocation: "gs://" + bucket + "/tmp"}}})
		if err != nil {
			t.Fatalf("LaunchFlexTemplate(Jdbc_to_BigQuery_Flex): %v", err)
		}
		id := resp.GetJob().GetId()
		j := waitDataflowJob(t, ctx, c, project, id, dataflowpb.JobState_JOB_STATE_DONE, dataflowpb.JobState_JOB_STATE_FAILED)
		if j.CurrentState != dataflowpb.JobState_JOB_STATE_DONE {
			t.Fatalf("Jdbc_to_BigQuery_Flex's job is %s:\n%s", j.CurrentState, jobMessages(t, ctx, c, project, id))
		}
		if l := j.GetLabels()["goog-dataflow-provided-template-type"]; l != "flex" {
			t.Errorf("the job's goog-dataflow-provided-template-type label is %q; want flex", l)
		}
		if got := providedRows(t, ctx, bq, "SELECT name, age FROM `"+project+"."+ds+".flex` ORDER BY name"); got != "Ada=36,Grace=45" {
			t.Errorf("the table has %s; want Ada=36,Grace=45", got)
		}
	})
	ps := pubsubClient(t, h)
	flexLaunch := func(t *testing.T, name, job string, p map[string]string) string {
		t.Helper()
		resp, err := c.flex.LaunchFlexTemplate(ctx, &dataflowpb.LaunchFlexTemplateRequest{ProjectId: project, Location: "us-central1",
			LaunchParameter: &dataflowpb.LaunchFlexTemplateParameter{JobName: job + "-" + suffix,
				Template:    &dataflowpb.LaunchFlexTemplateParameter_ContainerSpecGcsPath{ContainerSpecGcsPath: "gs://dataflow-templates-us-central1/latest/flex/" + name},
				Parameters:  p,
				Environment: &dataflowpb.FlexTemplateRuntimeEnvironment{TempLocation: "gs://" + bucket + "/tmp"}}})
		if err != nil {
			t.Fatalf("LaunchFlexTemplate(%s): %v", name, err)
		}
		return resp.GetJob().GetId()
	}
	topicSub := func(t *testing.T, name string) (string, string) {
		t.Helper()
		topic := fmt.Sprintf("projects/%s/topics/%s-%s", project, name, suffix)
		sub := fmt.Sprintf("projects/%s/subscriptions/%s-%s", project, name, suffix)
		if _, err := ps.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: topic}); err != nil {
			t.Fatal(err)
		}
		if _, err := ps.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{Name: sub, Topic: topic}); err != nil {
			t.Fatal(err)
		}
		return topic, sub
	}
	conn := map[string]string{"driverJars": driver, "driverClassName": "org.postgresql.Driver", "connectionUrl": src.url(),
		"username": "postgres", "password": "x"}

	t.Run("Jdbc_to_PubSub", func(t *testing.T) {
		topic, sub := topicSub(t, "jdbc-out")
		p := map[string]string{"query": "SELECT name, age FROM people", "outputTopic": topic}
		for k, v := range conn {
			p[k] = v
		}
		done(t, ctx, c, project, "Jdbc_to_PubSub", flexLaunch(t, "Jdbc_to_PubSub", "provided-jdbc-ps", p))
		var got []string
		for deadline := time.Now().Add(time.Minute); len(got) < 2 && time.Now().Before(deadline); {
			r, err := ps.SubscriptionAdminClient.Pull(ctx, &pubsubpb.PullRequest{Subscription: sub, MaxMessages: 10})
			if err != nil {
				t.Fatal(err)
			}
			for _, m := range r.GetReceivedMessages() {
				var row map[string]any
				if err := json.Unmarshal(m.GetMessage().GetData(), &row); err != nil {
					t.Fatalf("a message %q is not a JSON row: %v", m.GetMessage().GetData(), err)
				}
				got = append(got, fmt.Sprintf("%v=%v", row["name"], row["age"]))
			}
		}
		sort.Strings(got)
		if want := "Ada=36,Grace=45"; strings.Join(got, ",") != want {
			t.Errorf("the topic has %v; want %s", got, want)
		}
	})

	t.Run("Pubsub_to_Jdbc", func(t *testing.T) {
		topic, sub := topicSub(t, "jdbc-in")
		dead, _ := topicSub(t, "jdbc-dead")
		p := map[string]string{"inputSubscription": sub, "statement": "INSERT INTO arrivals (name, age) VALUES (?, ?)",
			"outputDeadletterTopic": dead}
		for k, v := range conn {
			p[k] = v
		}
		id := flexLaunch(t, "Pubsub_to_Jdbc", "provided-ps-jdbc", p)
		j := waitDataflowJob(t, ctx, c, project, id, dataflowpb.JobState_JOB_STATE_RUNNING, dataflowpb.JobState_JOB_STATE_FAILED)
		if j.CurrentState != dataflowpb.JobState_JOB_STATE_RUNNING || j.Type != dataflowpb.JobType_JOB_TYPE_STREAMING {
			t.Fatalf("Pubsub_to_Jdbc's job is %s %s:\n%s", j.CurrentState, j.Type, jobMessages(t, ctx, c, project, id))
		}
		rows := func() string {
			r, err := src.db.QueryContext(ctx, "SELECT DISTINCT name, age FROM arrivals ORDER BY name")
			if err != nil {
				return err.Error()
			}
			defer r.Close()
			var out []string
			for r.Next() {
				var n string
				var a int
				_ = r.Scan(&n, &a)
				out = append(out, fmt.Sprintf("%s=%d", n, a))
			}
			return strings.Join(out, ",")
		}
		want := "Ada=36,Grace=45"
		for deadline := time.Now().Add(10 * time.Minute); ; {
			for _, m := range []string{`{"name": "Ada", "age": 36}`, `{"name": "Grace", "age": 45}`} {
				if _, err := ps.TopicAdminClient.Publish(ctx, &pubsubpb.PublishRequest{Topic: topic, Messages: []*pubsubpb.PubsubMessage{{Data: []byte(m)}}}); err != nil {
					t.Fatal(err)
				}
			}
			time.Sleep(20 * time.Second)
			got := rows()
			if got == want {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("the table arrivals has %q; want %s:\n%s", got, want, jobMessages(t, ctx, c, project, id))
			}
		}
		if _, err := c.jobs.UpdateJob(ctx, &dataflowpb.UpdateJobRequest{ProjectId: project, Location: "us-central1", JobId: id,
			Job: &dataflowpb.Job{RequestedState: dataflowpb.JobState_JOB_STATE_DRAINED}}); err != nil {
			t.Fatalf("drain: %v", err)
		}
		if j := waitDataflowJob(t, ctx, c, project, id, dataflowpb.JobState_JOB_STATE_DRAINED, dataflowpb.JobState_JOB_STATE_FAILED, dataflowpb.JobState_JOB_STATE_CANCELLED); j.CurrentState != dataflowpb.JobState_JOB_STATE_DRAINED {
			t.Fatalf("the job ended %s:\n%s", j.CurrentState, jobMessages(t, ctx, c, project, id))
		}
	})
}
