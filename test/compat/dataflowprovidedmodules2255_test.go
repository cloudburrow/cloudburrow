//go:build compat

package compat

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	computeapi "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	"cloud.google.com/go/dataflow/apiv1beta3/dataflowpb"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	sqladmin "google.golang.org/api/sqladmin/v1"
	"google.golang.org/protobuf/proto"
)

// newMySQLSource is newJDBCSource's Cloud SQL for MySQL instance: a
// private IP on the project's default network, holding the table people
// (name, age).
func newMySQLSource(t *testing.T, h *Harness, ctx context.Context, p, suffix string) jdbcSource {
	t.Helper()
	opts := sqlComputeOpts(t, h)
	svc := sqlAdminService(t, h)
	gaddrs, err := computeapi.NewGlobalAddressesRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	rng := "df-mysql-psa-" + suffix
	op, err := gaddrs.Insert(ctx, &computepb.InsertGlobalAddressRequest{Project: p, AddressResource: &computepb.Address{
		Name: proto.String(rng), AddressType: proto.String("INTERNAL"), Purpose: proto.String("VPC_PEERING"),
		PrefixLength: proto.Int32(24), Network: proto.String("global/networks/default")}})
	if err != nil {
		t.Fatal(err)
	}
	if err := op.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	name := "df-mysql-" + suffix
	in := createSQLInstance(t, svc, p, &sqladmin.DatabaseInstance{Name: name, DatabaseVersion: "MYSQL_8_4", Region: "us-central1", RootPassword: "Root-pass1",
		Settings: &sqladmin.Settings{Tier: "db-custom-1-3840", IpConfiguration: &sqladmin.IpConfiguration{Ipv4Enabled: true,
			PrivateNetwork: "projects/" + p + "/global/networks/default", AllocatedIpRange: rng}}})
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
	src.db = sqlEngine{mysql: true}.root(t, h, in.ConnectionName)
	for _, stmt := range []string{"CREATE DATABASE IF NOT EXISTS orchard", "CREATE TABLE IF NOT EXISTS orchard.people (name VARCHAR(64), age INT)",
		"DELETE FROM orchard.people", "INSERT INTO orchard.people VALUES ('Ada', 36), ('Grace', 45)"} {
		eventually(t, stmt, func() error {
			_, err := src.db.ExecContext(ctx, stmt)
			return err
		})
	}
	return src
}

// covers: google.dataflow.v1beta3.FlexTemplatesService/LaunchFlexTemplate
//
// TestDataflowProvidedFlexModules (#2255): the Flex Templates of three
// more v2 modules of DataflowTemplates, each module built with Maven on
// its first template's use, launched at Google's path and run to their
// output:
//   - PostgreSQL to BigQuery (PostgreSQL_to_BigQuery, module
//     postgresql-to-googlecloud, templates/provided/postgresql-to-bigquery)
//     loads a query's rows from a Cloud SQL for PostgreSQL instance's
//     private IP, with the driver the template has, into BigQuery;
//   - MySQL to BigQuery (MySQL_to_BigQuery, module mysql-to-googlecloud,
//     templates/provided/mysql-to-bigquery) does so from Cloud SQL for
//     MySQL;
//   - Streaming Data Generator (Streaming_Data_Generator, module
//     streaming-data-generator, templates/provided/streaming-data-
//     generator) publishes fake GAME_EVENT messages to a topic at its qps,
//     up to its messagesLimit.
func TestDataflowProvidedFlexModules(t *testing.T) {
	h, _, c := dataflowEnv(t)
	h.Endpoint(EnvStorage)
	h.Endpoint(EnvBigQuery)
	h.Endpoint(EnvPubSub)
	bq, project := bigqueryClient(t, h)
	sc := storageClient(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Minute)
	defer cancel()
	const base = "gs://dataflow-templates-us-central1/latest/flex/"
	suffix := fmt.Sprint(time.Now().UnixNano() % 1e6)
	bucket := fmt.Sprintf("%s-dataflow-provmod-%s", project, suffix)
	dataflowBucket(t, ctx, sc, project, bucket)
	launch := func(t *testing.T, name, job string, p map[string]string) string {
		t.Helper()
		resp, err := c.flex.LaunchFlexTemplate(ctx, &dataflowpb.LaunchFlexTemplateRequest{ProjectId: project, Location: "us-central1",
			LaunchParameter: &dataflowpb.LaunchFlexTemplateParameter{JobName: job + "-" + suffix,
				Template:    &dataflowpb.LaunchFlexTemplateParameter_ContainerSpecGcsPath{ContainerSpecGcsPath: base + name},
				Parameters:  p,
				Environment: &dataflowpb.FlexTemplateRuntimeEnvironment{TempLocation: "gs://" + bucket + "/tmp"}}})
		if err != nil {
			t.Fatalf("LaunchFlexTemplate(%s): %v", name, err)
		}
		return resp.GetJob().GetId()
	}
	ds := "provided_modules_" + suffix
	if err := bq.Dataset(ds).Create(ctx, nil); err != nil {
		t.Fatal(err)
	}
	table := func(t *testing.T, name string) string {
		t.Helper()
		schema := bigquery.Schema{{Name: "name", Type: bigquery.StringFieldType}, {Name: "age", Type: bigquery.IntegerFieldType}}
		if err := bq.Dataset(ds).Table(name).Create(ctx, &bigquery.TableMetadata{Schema: schema}); err != nil {
			t.Fatal(err)
		}
		return project + ":" + ds + "." + name
	}

	t.Run("PostgreSQL_to_BigQuery", func(t *testing.T) {
		h := h.For(t)
		src := newJDBCSource(t, h, ctx, project, suffix)
		out := table(t, "postgres")
		done(t, ctx, c, project, "PostgreSQL_to_BigQuery", launch(t, "PostgreSQL_to_BigQuery", "provided-pg-bq", map[string]string{
			"connectionURL": src.url(), "username": "postgres", "password": "x", "query": "SELECT name, age FROM people",
			"outputTable": out, "bigQueryLoadingTemporaryDirectory": "gs://" + bucket + "/pgbqtmp"}))
		if got := providedRows(t, ctx, bq, "SELECT name, age FROM `"+project+"."+ds+".postgres` ORDER BY name"); got != "Ada=36,Grace=45" {
			t.Errorf("the table has %s; want Ada=36,Grace=45", got)
		}
	})

	t.Run("MySQL_to_BigQuery", func(t *testing.T) {
		h := h.For(t)
		src := newMySQLSource(t, h, ctx, project, suffix)
		out := table(t, "mysql")
		done(t, ctx, c, project, "MySQL_to_BigQuery", launch(t, "MySQL_to_BigQuery", "provided-mysql-bq", map[string]string{
			"connectionURL": "jdbc:mysql://" + src.privateIP + ":3306/orchard", "username": "root", "password": "Root-pass1",
			"query": "SELECT name, age FROM people", "outputTable": out, "bigQueryLoadingTemporaryDirectory": "gs://" + bucket + "/mybqtmp"}))
		if got := providedRows(t, ctx, bq, "SELECT name, age FROM `"+project+"."+ds+".mysql` ORDER BY name"); got != "Ada=36,Grace=45" {
			t.Errorf("the table has %s; want Ada=36,Grace=45", got)
		}
	})

	t.Run("Streaming_Data_Generator", func(t *testing.T) {
		h := h.For(t)
		ps := pubsubClient(t, h)
		topic := fmt.Sprintf("projects/%s/topics/provgen-%s", project, suffix)
		sub := fmt.Sprintf("projects/%s/subscriptions/provgen-%s", project, suffix)
		if _, err := ps.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: topic}); err != nil {
			t.Fatal(err)
		}
		if _, err := ps.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{Name: sub, Topic: topic}); err != nil {
			t.Fatal(err)
		}
		id := launch(t, "Streaming_Data_Generator", "provided-generator", map[string]string{"schemaTemplate": "GAME_EVENT",
			"qps": "5", "messagesLimit": "6", "sinkType": "PUBSUB", "topic": topic})
		var got []string
		for deadline := time.Now().Add(8 * time.Minute); len(got) < 6; {
			pctx, pcancel := context.WithTimeout(ctx, 20*time.Second)
			resp, err := ps.SubscriptionAdminClient.Pull(pctx, &pubsubpb.PullRequest{Subscription: sub, MaxMessages: 10})
			pcancel()
			if err == nil {
				var ack []string
				for _, m := range resp.GetReceivedMessages() {
					got = append(got, string(m.GetMessage().GetData()))
					ack = append(ack, m.GetAckId())
				}
				if len(ack) > 0 {
					if err := ps.SubscriptionAdminClient.Acknowledge(ctx, &pubsubpb.AcknowledgeRequest{Subscription: sub, AckIds: ack}); err != nil {
						t.Fatal(err)
					}
				}
			}
			if time.Now().After(deadline) {
				t.Fatalf("the topic got %d messages; want 6:\n%s", len(got), jobMessages(t, ctx, c, project, id))
			}
		}
		sort.Strings(got)
		for _, m := range got {
			if !strings.Contains(m, `"eventId"`) || !strings.Contains(m, `"score"`) {
				t.Errorf("a message is not a GAME_EVENT: %s", m)
			}
		}
		if len(got) != 6 {
			t.Errorf("the topic got %d messages; want messagesLimit's 6", len(got))
		}
		// The generator's bounded sequence ends, and so does its job; a job
		// still running is cancelled.
		j, err := c.jobs.GetJob(ctx, &dataflowpb.GetJobRequest{ProjectId: project, Location: "us-central1", JobId: id})
		if err != nil {
			t.Fatal(err)
		}
		if j.CurrentState == dataflowpb.JobState_JOB_STATE_FAILED {
			t.Fatalf("the generator's job failed:\n%s", jobMessages(t, ctx, c, project, id))
		}
		if j.CurrentState == dataflowpb.JobState_JOB_STATE_RUNNING {
			if _, err := c.jobs.UpdateJob(ctx, &dataflowpb.UpdateJobRequest{ProjectId: project, Location: "us-central1", JobId: id,
				Job: &dataflowpb.Job{RequestedState: dataflowpb.JobState_JOB_STATE_CANCELLED}}); err != nil {
				t.Fatal(err)
			}
		}
	})
}
