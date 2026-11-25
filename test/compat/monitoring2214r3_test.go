//go:build compat

package compat

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	aiplatform "cloud.google.com/go/aiplatform/apiv1"
	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	aiplatformbeta "cloud.google.com/go/aiplatform/apiv1beta1"
	betapb "cloud.google.com/go/aiplatform/apiv1beta1/aiplatformpb"
	"cloud.google.com/go/bigquery"
	"cloud.google.com/go/logging"
	"cloud.google.com/go/logging/logadmin"
	monitoring "cloud.google.com/go/monitoring/apiv3/v2"
	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"cloud.google.com/go/pubsub/v2"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/structpb"
)

// mailMessage is one message of the console's Mail page (GET /api/mail).
type mailMessage struct {
	ID, From, Subject, Source, Text string
	To                              []string
}

// waitMail returns the first message to addr whose subject has subject,
// with its text, from the console's Mail page (the local mail sink).
func waitMail(t *testing.T, console, addr, subject string, d time.Duration) mailMessage {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		code, body := consoleDo(t, console, http.MethodGet, "/api/mail", "")
		if code != http.StatusOK {
			t.Fatalf("GET /api/mail = %d: %s", code, body)
		}
		var list struct {
			SMTP     string        `json:"smtp"`
			Messages []mailMessage `json:"messages"`
		}
		if err := json.Unmarshal([]byte(body), &list); err != nil {
			t.Fatalf("GET /api/mail: %v", err)
		}
		for _, m := range list.Messages {
			if strings.Contains(m.Subject, subject) && strings.Contains(strings.Join(m.To, ","), addr) {
				code, body := consoleDo(t, console, http.MethodGet, "/api/mail/"+m.ID, "")
				if code != http.StatusOK || json.Unmarshal([]byte(body), &m) != nil {
					t.Fatalf("GET /api/mail/%s = %d: %s", m.ID, code, body)
				}
				return m
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no mail to %s with %q in its subject in %s; the sink holds %d messages", addr, subject, d, len(list.Messages))
		}
		time.Sleep(time.Second)
	}
}

// anomalyEntries are the model_monitoring_anomaly entries of project whose
// jsonPayload field key is value.
func anomalyEntries(t *testing.T, ctx context.Context, logAddr, project, key, value string, d time.Duration) []*logging.Entry {
	t.Helper()
	ac, err := logadmin.NewClient(ctx, "projects/"+project, option.WithEndpoint(logAddr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	defer ac.Close()
	filter := `logName="projects/` + project + `/logs/aiplatform.googleapis.com%2Fmodel_monitoring_anomaly" AND jsonPayload.` + key + `="` + value + `"`
	for deadline := time.Now().Add(d); ; time.Sleep(time.Second) {
		var out []*logging.Entry
		it := ac.Entries(ctx, logadmin.Filter(filter))
		for {
			e, err := it.Next()
			if err == iterator.Done {
				break
			}
			if err != nil {
				t.Fatalf("logadmin Entries: %v", err)
			}
			out = append(out, e)
		}
		if len(out) > 0 || time.Now().After(deadline) {
			return out
		}
	}
}

// payloadOf is an entry's jsonPayload as a map.
func payloadOf(e *logging.Entry) map[string]any {
	if s, ok := e.Payload.(*structpb.Struct); ok {
		return s.AsMap()
	}
	return nil
}

// tfRecordFile is a TFRecord file of tf.train.Example records, each a row
// of int64 and string features.
func tfRecordFile(rows []map[string]any) []byte {
	crc := func(b []byte) uint32 {
		c := crc32.Checksum(b, crc32.MakeTable(crc32.Castagnoli))
		return ((c >> 15) | (c << 17)) + 0xa282ead8
	}
	var out []byte
	for _, r := range rows {
		var features []byte
		for k, v := range r {
			var list, feature []byte
			switch x := v.(type) {
			case string:
				list = protowire.AppendString(protowire.AppendTag(nil, 1, protowire.BytesType), x)
				feature = protowire.AppendBytes(protowire.AppendTag(nil, 1, protowire.BytesType), list) // bytes_list
			case int:
				list = protowire.AppendVarint(protowire.AppendTag(nil, 1, protowire.VarintType), uint64(x))
				feature = protowire.AppendBytes(protowire.AppendTag(nil, 3, protowire.BytesType), list) // int64_list
			}
			entry := protowire.AppendString(protowire.AppendTag(nil, 1, protowire.BytesType), k)
			entry = protowire.AppendBytes(protowire.AppendTag(entry, 2, protowire.BytesType), feature)
			features = protowire.AppendBytes(protowire.AppendTag(features, 1, protowire.BytesType), entry)
		}
		ex := protowire.AppendBytes(protowire.AppendTag(nil, 1, protowire.BytesType), features)
		head := binary.LittleEndian.AppendUint64(nil, uint64(len(ex)))
		out = append(out, head...)
		out = binary.LittleEndian.AppendUint32(out, crc(head))
		out = append(out, ex...)
		out = binary.LittleEndian.AppendUint32(out, crc(ex))
	}
	return out
}

// covers: google.cloud.aiplatform.v1beta1.ModelMonitoringService/CreateModelMonitor, google.cloud.aiplatform.v1beta1.ModelMonitoringService/CreateModelMonitoringJob, google.cloud.aiplatform.v1beta1.ModelMonitoringService/GetModelMonitoringJob, google.cloud.aiplatform.v1beta1.ModelMonitoringService/SearchModelMonitoringStats, google.cloud.aiplatform.v1beta1.ModelMonitoringService/DeleteModelMonitor, google.monitoring.v3.NotificationChannelService/CreateNotificationChannel, google.monitoring.v3.NotificationChannelService/SendNotificationChannelVerificationCode, google.monitoring.v3.NotificationChannelService/VerifyNotificationChannel
//
// TestAgentPlatformModelMonitoringInputsAndAlerts (#2214), through the
// official aiplatform v1beta1, bigquery, monitoring, pubsub and logadmin Go
// clients: a ModelMonitor's training dataset is a BigQuery table, a job's
// target dataset a TFRecord file of tf.train.Example records in Cloud
// Storage and another job's a managed tabular dataset. The monitor's
// notification spec names a Pub/Sub, a webhook and an email notification
// channel, an email address and Cloud Logging: the drifted run publishes
// its alerts to the topic, posts them to the local webhook, mails both
// addresses through the local mail sink (read back from the console's
// Mail page) and writes a model_monitoring_anomaly entry. The email
// channel is verified with the code its verification email carries.
func TestAgentPlatformModelMonitoringInputsAndAlerts(t *testing.T) {
	h := New(t)
	endpoint := runShardEndpoint(h, envRunLocalAI, EnvPrediction)
	shardEndpoint(h, "CLOUDBURROW_TEST_RUN_STORAGE", EnvStorage)
	shardEndpoint(h, "CLOUDBURROW_TEST_RUN_BIGQUERY", EnvBigQuery)
	logAddr := runShardEndpoint(h, "CLOUDBURROW_TEST_RUN_LOGGING", EnvLogging)
	pubsubAddr := runShardEndpoint(h, "CLOUDBURROW_TEST_RUN_PUBSUB", EnvPubSub)
	h.Endpoint(EnvMonitoring)
	console := consoleAddr(t, h)
	if strings.TrimSpace(endpoint) == "" {
		t.Fatal("no Agent Platform endpoint")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	t.Cleanup(cancel)
	opts := gardenOpts(h, EnvPrediction)
	mm, err := aiplatformbeta.NewModelMonitoringClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mm.Close() })
	dc, err := aiplatform.NewDatasetClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dc.Close() })
	nc, err := monitoring.NewNotificationChannelClient(ctx, monitoringOpts(h)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = nc.Close() })
	mc, _, _ := modelRegistryClients(t, h)
	sc := storageClient(t, h)
	bq, bqProject := bigqueryClient(t, h)
	parent := runParent(h)
	project := h.Project()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano()%1e6)

	// The training data: a BigQuery table.
	ds := bq.Dataset("mm_inputs_" + suffix)
	if err := ds.Create(ctx, nil); err != nil {
		t.Fatalf("create dataset: %v", err)
	}
	t.Cleanup(func() { _ = ds.DeleteWithContents(context.Background()) })
	type trainRow struct {
		Age     int64  `bigquery:"age"`
		Country string `bigquery:"country"`
	}
	schema, _ := bigquery.InferSchema(trainRow{})
	tbl := ds.Table("train")
	if err := tbl.Create(ctx, &bigquery.TableMetadata{Schema: schema}); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if err := tbl.Inserter().Put(ctx, []trainRow{{30, "us"}, {31, "us"}, {32, "fr"}, {33, "fr"}}); err != nil {
		t.Fatalf("insert rows: %v", err)
	}
	tableURI := "bq://" + bqProject + ".mm_inputs_" + suffix + ".train"

	// The serving data: TFRecord in Cloud Storage, and a managed dataset.
	bkt := project + "-mm-inputs-" + suffix
	if err := sc.Bucket(bkt).Create(ctx, project, nil); err != nil && status.Code(err) != codes.AlreadyExists && !strings.Contains(err.Error(), "409") {
		t.Fatalf("create bucket: %v", err)
	}
	w := sc.Bucket(bkt).Object("serve.tfrecord").NewWriter(ctx)
	if _, err := w.Write(tfRecordFile([]map[string]any{{"age": 60, "country": "us"}, {"age": 61, "country": "us"}, {"age": 62, "country": "us"}})); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("write serve.tfrecord: %v", err)
	}
	putArtifact(t, ctx, sc, bkt, "dataset.csv", "age,country\n70,us\n71,us\n72,us\n")
	meta, _ := structpb.NewValue(map[string]any{"inputConfig": map[string]any{"gcsSource": map[string]any{"uri": []any{"gs://" + bkt + "/dataset.csv"}}}})
	dop, err := dc.CreateDataset(ctx, &aiplatformpb.CreateDatasetRequest{Parent: parent, Dataset: &aiplatformpb.Dataset{DisplayName: "mm-" + suffix,
		MetadataSchemaUri: "gs://google-cloud-aiplatform/schema/dataset/metadata/tabular_1.0.0.yaml", Metadata: meta}})
	if err != nil {
		t.Fatalf("CreateDataset: %v", err)
	}
	managed, err := dop.Wait(ctx)
	if err != nil {
		t.Fatalf("CreateDataset operation: %v", err)
	}
	t.Cleanup(func() {
		if op, err := dc.DeleteDataset(context.Background(), &aiplatformpb.DeleteDatasetRequest{Name: managed.GetName()}); err == nil {
			_ = op.Wait(context.Background())
		}
	})

	// The notification channels: Pub/Sub, a local webhook, email.
	t.Setenv("PUBSUB_EMULATOR_HOST", pubsubAddr)
	ps, err := pubsub.NewClient(ctx, project)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ps.Close() })
	topic, sub := "projects/"+project+"/topics/mm-alerts-"+suffix, "projects/"+project+"/subscriptions/mm-alerts-"+suffix
	if _, err := ps.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: topic}); err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}
	t.Cleanup(func() {
		_ = ps.TopicAdminClient.DeleteTopic(context.Background(), &pubsubpb.DeleteTopicRequest{Topic: topic})
	})
	if _, err := ps.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{Name: sub, Topic: topic}); err != nil {
		t.Fatalf("CreateSubscription: %v", err)
	}
	t.Cleanup(func() {
		_ = ps.SubscriptionAdminClient.DeleteSubscription(context.Background(), &pubsubpb.DeleteSubscriptionRequest{Subscription: sub})
	})
	hook := newWebhook(t)
	mkChannel := func(c *monitoringpb.NotificationChannel) string {
		t.Helper()
		got, err := nc.CreateNotificationChannel(ctx, &monitoringpb.CreateNotificationChannelRequest{Name: "projects/" + project, NotificationChannel: c})
		if err != nil {
			t.Fatalf("CreateNotificationChannel %s: %v", c.GetType(), err)
		}
		t.Cleanup(func() {
			_ = nc.DeleteNotificationChannel(context.Background(), &monitoringpb.DeleteNotificationChannelRequest{Name: got.GetName(), Force: true})
		})
		return got.GetName()
	}
	pubsubCh := mkChannel(&monitoringpb.NotificationChannel{Type: "pubsub", DisplayName: "mm topic", Labels: map[string]string{"topic": topic}})
	hookCh := mkChannel(&monitoringpb.NotificationChannel{Type: "webhook_tokenauth", DisplayName: "mm hook", Labels: map[string]string{"url": hook.srv.URL + "/mm?token=t"}})
	channelAddr := "oncall-" + suffix + "@example.com"
	userAddr := "owner-" + suffix + "@example.com"
	mailCh := mkChannel(&monitoringpb.NotificationChannel{Type: "email", DisplayName: "mm oncall", Labels: map[string]string{"email_address": channelAddr}})

	// The email channel's verification code arrives by mail.
	if err := nc.SendNotificationChannelVerificationCode(ctx, &monitoringpb.SendNotificationChannelVerificationCodeRequest{Name: mailCh}); err != nil {
		t.Fatalf("SendNotificationChannelVerificationCode: %v", err)
	}
	vm := waitMail(t, console, channelAddr, "Verify your notification channel", time.Minute)
	code := regexp.MustCompile(`G-\d{6}`).FindString(vm.Text)
	if code == "" {
		t.Fatalf("the verification email carries no code: %q", vm.Text)
	}
	if v, err := nc.VerifyNotificationChannel(ctx, &monitoringpb.VerifyNotificationChannelRequest{Name: mailCh, Code: code}); err != nil ||
		v.GetVerificationStatus() != monitoringpb.NotificationChannel_VERIFIED {
		t.Errorf("VerifyNotificationChannel with the mailed code = %v, %v", v, err)
	}

	up, err := mc.UploadModel(ctx, &aiplatformpb.UploadModelRequest{Parent: parent, ModelId: "mm-inputs-" + suffix,
		Model: &aiplatformpb.Model{DisplayName: "mm inputs", ContainerSpec: &aiplatformpb.ModelContainerSpec{ImageUri: "us-docker.pkg.dev/p/r/serve:1"}}})
	if err != nil {
		t.Fatalf("UploadModel: %v", err)
	}
	if _, err := up.Wait(ctx); err != nil {
		t.Fatalf("UploadModel operation: %v", err)
	}
	model := parent + "/models/mm-inputs-" + suffix
	t.Cleanup(func() {
		if op, err := mc.DeleteModel(context.Background(), &aiplatformpb.DeleteModelRequest{Name: model}); err == nil {
			_ = op.Wait(context.Background())
		}
	})

	cond := &betapb.ModelMonitoringAlertCondition{Condition: &betapb.ModelMonitoringAlertCondition_Threshold{Threshold: 0.2}}
	input := func(loc *betapb.ModelMonitoringInput_ModelMonitoringDataset) *betapb.ModelMonitoringInput {
		return &betapb.ModelMonitoringInput{Dataset: &betapb.ModelMonitoringInput_ColumnizedDataset{ColumnizedDataset: loc}}
	}
	monitor := &betapb.ModelMonitor{DisplayName: "mm inputs " + suffix,
		ModelMonitoringTarget: &betapb.ModelMonitor_ModelMonitoringTarget{Source: &betapb.ModelMonitor_ModelMonitoringTarget_VertexModel{
			VertexModel: &betapb.ModelMonitor_ModelMonitoringTarget_VertexModelSource{Model: model, ModelVersionId: "1"}}},
		TrainingDataset: input(&betapb.ModelMonitoringInput_ModelMonitoringDataset{DataLocation: &betapb.ModelMonitoringInput_ModelMonitoringDataset_BigquerySource{
			BigquerySource: &betapb.ModelMonitoringInput_ModelMonitoringDataset_ModelMonitoringBigQuerySource{
				Connection: &betapb.ModelMonitoringInput_ModelMonitoringDataset_ModelMonitoringBigQuerySource_TableUri{TableUri: tableURI}}}}),
		ModelMonitoringSchema: &betapb.ModelMonitoringSchema{FeatureFields: []*betapb.ModelMonitoringSchema_FieldSchema{
			{Name: "age", DataType: "integer"}, {Name: "country", DataType: "categorical"}}},
		DefaultObjective: &betapb.ModelMonitor_TabularObjective{TabularObjective: &betapb.ModelMonitoringObjectiveSpec_TabularObjective{
			FeatureDriftSpec: &betapb.ModelMonitoringObjectiveSpec_DataDriftSpec{CategoricalMetricType: "l_infinity", NumericMetricType: "jensen_shannon_divergence",
				DefaultCategoricalAlertCondition: cond, DefaultNumericAlertCondition: cond}}},
		NotificationSpec: &betapb.ModelMonitoringNotificationSpec{
			EmailConfig: &betapb.ModelMonitoringNotificationSpec_EmailConfig{UserEmails: []string{userAddr}},
			NotificationChannelConfigs: []*betapb.ModelMonitoringNotificationSpec_NotificationChannelConfig{
				{NotificationChannel: pubsubCh}, {NotificationChannel: hookCh}, {NotificationChannel: mailCh}},
			EnableCloudLogging: true},
	}
	op, err := mm.CreateModelMonitor(ctx, &betapb.CreateModelMonitorRequest{Parent: parent, ModelMonitor: monitor, ModelMonitorId: "inputs-" + suffix})
	if err != nil {
		t.Fatalf("CreateModelMonitor: %v", err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatalf("CreateModelMonitor operation: %v", err)
	}
	name := parent + "/modelMonitors/inputs-" + suffix
	t.Cleanup(func() {
		if op, err := mm.DeleteModelMonitor(context.Background(), &betapb.DeleteModelMonitorRequest{Name: name, Force: true}); err == nil {
			_ = op.Wait(context.Background())
		}
	})

	run := func(id string, target *betapb.ModelMonitoringInput) *betapb.ModelMonitoringJob {
		t.Helper()
		job, err := mm.CreateModelMonitoringJob(ctx, &betapb.CreateModelMonitoringJobRequest{Parent: name, ModelMonitoringJobId: id,
			ModelMonitoringJob: &betapb.ModelMonitoringJob{ModelMonitoringSpec: &betapb.ModelMonitoringSpec{ObjectiveSpec: &betapb.ModelMonitoringObjectiveSpec{
				TargetDataset: target}}}})
		if err != nil {
			t.Fatalf("CreateModelMonitoringJob %s: %v", id, err)
		}
		for deadline := time.Now().Add(3 * time.Minute); ; time.Sleep(500 * time.Millisecond) {
			job, err = mm.GetModelMonitoringJob(ctx, &betapb.GetModelMonitoringJobRequest{Name: job.GetName()})
			if err != nil {
				t.Fatalf("GetModelMonitoringJob: %v", err)
			}
			if s := job.GetState(); s != betapb.JobState_JOB_STATE_PENDING && s != betapb.JobState_JOB_STATE_RUNNING || time.Now().After(deadline) {
				break
			}
		}
		if job.GetState() != betapb.JobState_JOB_STATE_SUCCEEDED {
			t.Fatalf("job %s ended %v: %v", id, job.GetState(), job.GetJobExecutionDetail())
		}
		return job
	}
	tfJob := run("tfrecord-"+suffix, input(&betapb.ModelMonitoringInput_ModelMonitoringDataset{DataLocation: &betapb.ModelMonitoringInput_ModelMonitoringDataset_GcsSource{
		GcsSource: &betapb.ModelMonitoringInput_ModelMonitoringDataset_ModelMonitoringGcsSource{GcsUri: "gs://" + bkt + "/serve.tfrecord",
			Format: betapb.ModelMonitoringInput_ModelMonitoringDataset_ModelMonitoringGcsSource_TF_RECORD}}}))
	if d := tfJob.GetJobExecutionDetail(); len(d.GetBaselineDatasets()) != 1 || d.GetBaselineDatasets()[0].GetLocation() != tableURI ||
		len(d.GetTargetDatasets()) != 1 || d.GetTargetDatasets()[0].GetLocation() != "gs://"+bkt+"/serve.tfrecord" {
		t.Errorf("the TFRecord job's datasets = %v", d)
	}
	stats := map[string]*betapb.ModelMonitoringStatsDataPoint{}
	for it := mm.SearchModelMonitoringStats(ctx, &betapb.SearchModelMonitoringStatsRequest{ModelMonitor: name,
		StatsFilter: &betapb.SearchModelMonitoringStatsFilter{Filter: &betapb.SearchModelMonitoringStatsFilter_TabularStatsFilter_{
			TabularStatsFilter: &betapb.SearchModelMonitoringStatsFilter_TabularStatsFilter{ModelMonitoringJob: tfJob.GetName()}}}}); ; {
		s, err := it.Next()
		if err != nil {
			break
		}
		if ts := s.GetTabularStats(); len(ts.GetDataPoints()) > 0 {
			stats[ts.GetStatsName()] = ts.GetDataPoints()[0]
		}
	}
	// BigQuery's 2 us of 4 against TFRecord's 3 of 3: L-infinity 0.5.
	if p := stats["country"]; p.GetCurrentStats().GetDistributionValue().GetDistributionDeviation() != 0.5 || !p.GetHasAnomaly() {
		t.Errorf("country's statistics from BigQuery and TFRecord = %v; want 0.5, an anomaly", p)
	}
	if p := stats["age"]; !p.GetHasAnomaly() {
		t.Errorf("age's statistics = %v; want an anomaly", p)
	}

	// The alerts, delivered.
	var pubsubAlert map[string]any
	for deadline := time.Now().Add(time.Minute); pubsubAlert == nil && time.Now().Before(deadline); {
		pctx, pcancel := context.WithTimeout(ctx, 5*time.Second)
		resp, err := ps.SubscriptionAdminClient.Pull(pctx, &pubsubpb.PullRequest{Subscription: sub, MaxMessages: 10})
		pcancel()
		if err != nil && status.Code(err) != codes.DeadlineExceeded {
			t.Fatalf("Pull: %v", err)
		}
		for _, m := range resp.GetReceivedMessages() {
			var p map[string]any
			if json.Unmarshal(m.GetMessage().GetData(), &p) == nil && p["modelMonitoringJob"] == tfJob.GetName() {
				pubsubAlert = p
			}
			_ = ps.SubscriptionAdminClient.Acknowledge(ctx, &pubsubpb.AcknowledgeRequest{Subscription: sub, AckIds: []string{m.GetAckId()}})
		}
	}
	if alerts, _ := pubsubAlert["alerts"].([]any); len(alerts) < 2 {
		t.Errorf("the Pub/Sub channel's message = %v; want the job's two alerts", pubsubAlert)
	}
	var hooked map[string]any
	for deadline := time.Now().Add(time.Minute); hooked == nil && time.Now().Before(deadline); time.Sleep(time.Second) {
		hook.mu.Lock()
		for _, b := range hook.bodies {
			if b["modelMonitoringJob"] == tfJob.GetName() {
				hooked = b
			}
		}
		hook.mu.Unlock()
	}
	if hooked == nil {
		t.Error("the webhook channel received no alert")
	}
	subject := "Model Monitoring alert: tfrecord-" + suffix
	// The address is mailed by Model Monitoring, the email channel by
	// Cloud Monitoring.
	for addr, source := range map[string]string{userAddr: "model-monitoring", channelAddr: "monitoring"} {
		m := waitMail(t, console, addr, subject, time.Minute)
		if !strings.Contains(m.Text, "country") || !strings.Contains(m.Text, "above the threshold 0.2") || !strings.Contains(m.Text, tfJob.GetName()) || m.Source != source {
			t.Errorf("the alert email to %s = %+v", addr, m)
		}
	}
	entries := anomalyEntries(t, ctx, logAddr, project, "modelMonitoringJob", tfJob.GetName(), time.Minute)
	if len(entries) != 1 {
		t.Fatalf("%d model_monitoring_anomaly entries for %s, want 1", len(entries), tfJob.GetName())
	}
	p := payloadOf(entries[0])
	if entries[0].Severity != logging.Warning || p["anomalyObjective"] != "raw-feature-drift" || p["totalAnomaliesCount"] != 2.0 ||
		p["@type"] != "type.googleapis.com/google.cloud.aiplatform.logging.ModelMonitoringAnomaliesLogEntry" {
		t.Errorf("the anomaly entry = %v %v", entries[0].Severity, p)
	}

	// A managed dataset as the target.
	dsJob := run("dataset-"+suffix, input(&betapb.ModelMonitoringInput_ModelMonitoringDataset{DataLocation: &betapb.ModelMonitoringInput_ModelMonitoringDataset_VertexDataset{
		VertexDataset: managed.GetName()}}))
	if d := dsJob.GetJobExecutionDetail(); len(d.GetTargetDatasets()) != 1 || d.GetTargetDatasets()[0].GetLocation() != managed.GetName() {
		t.Errorf("the managed dataset job's target = %v", d.GetTargetDatasets())
	}
	if len(anomalyEntries(t, ctx, logAddr, project, "modelMonitoringJob", dsJob.GetName(), time.Minute)) != 1 {
		t.Errorf("the managed dataset job wrote no anomaly entry")
	}
}

// covers: google.cloud.aiplatform.v1.JobService/CreateModelDeploymentMonitoringJob, google.cloud.aiplatform.v1.JobService/UpdateModelDeploymentMonitoringJob, google.cloud.aiplatform.v1.JobService/GetModelDeploymentMonitoringJob, google.cloud.aiplatform.v1.JobService/SearchModelDeploymentMonitoringStatsAnomalies, google.cloud.aiplatform.v1.JobService/PauseModelDeploymentMonitoringJob, google.cloud.aiplatform.v1.JobService/DeleteModelDeploymentMonitoringJob
//
// TestAgentPlatformDeploymentMonitoringAttributionAndAlerts (#2214), with
// the official aiplatform v1 Go clients: a model with a Sampled Shapley
// explanation spec is deployed, and a ModelDeploymentMonitoringJob with
// explanation_config watches its feature attributions for skew from a
// Cloud Storage training dataset and for drift. UpdateModelDeploymentMonitoringJob
// renames it and gives it an alert config (an email address, Cloud
// Logging, and, where Cloud Monitoring and Pub/Sub are served, a Pub/Sub
// and a webhook channel). With the Agent Platform clock moved past each
// monitor interval, FEATURE_ATTRIBUTION_SKEW and then FEATURE_ATTRIBUTION_DRIFT
// anomalies are searched, the email arrives in the console's Mail page,
// and, where served, the model_monitoring_anomaly entry is in Cloud
// Logging and the channels received the anomalies.
func TestAgentPlatformDeploymentMonitoringAttributionAndAlerts(t *testing.T) {
	h := New(t)
	endpointAddr := runShardEndpoint(h, envRunLocalAI, EnvPrediction)
	scaleShardEndpoint(h, envScaleRun, EnvRun) // a serving container's start: the scale shard (#1821)
	shardEndpoint(h, "CLOUDBURROW_TEST_RUN_STORAGE", EnvStorage)
	console := consoleAddr(t, h)
	// Cloud Logging, Cloud Monitoring and Pub/Sub are checked where the
	// instance serves them, which the scale shard's does not.
	served := func(own, shared string) string {
		if v := strings.TrimSpace(os.Getenv(own)); v != "" {
			return v
		}
		return strings.TrimSpace(os.Getenv(shared))
	}
	logAddr := served("CLOUDBURROW_TEST_RUN_LOGGING", EnvLogging)
	pubsubAddr := served("CLOUDBURROW_TEST_RUN_PUBSUB", EnvPubSub)
	channels := pubsubAddr != "" && strings.TrimSpace(os.Getenv(EnvMonitoring)) != ""
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	buildModelServer(t, ctx)
	image := pushModelServer(t, ctx, h)
	mc, ec, pc := modelRegistryClients(t, h)
	jc, err := aiplatform.NewJobClient(ctx, gardenOpts(h, EnvPrediction)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = jc.Close() })
	sc := storageClient(t, h)
	parent := runParent(h)
	project := h.Project()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano()%1e6)

	bkt := project + "-mdm-attr-" + suffix
	if err := sc.Bucket(bkt).Create(ctx, project, nil); err != nil && status.Code(err) != codes.AlreadyExists && !strings.Contains(err.Error(), "409") {
		t.Fatalf("create bucket: %v", err)
	}
	putArtifact(t, ctx, sc, bkt, "model/model.json", `{"weight":2,"bias":0,"name":"mdm"}`)
	putArtifact(t, ctx, sc, bkt, "train.csv", "x\n1\n2\n3\n4\n")

	modelID := "mdm-attr-" + suffix
	explain := &aiplatformpb.ExplanationSpec{
		Parameters: &aiplatformpb.ExplanationParameters{Method: &aiplatformpb.ExplanationParameters_SampledShapleyAttribution{
			SampledShapleyAttribution: &aiplatformpb.SampledShapleyAttribution{PathCount: 5}}},
		Metadata: &aiplatformpb.ExplanationMetadata{Inputs: map[string]*aiplatformpb.ExplanationMetadata_InputMetadata{"x": {}},
			Outputs: map[string]*aiplatformpb.ExplanationMetadata_OutputMetadata{"y": {}}}}
	up, err := mc.UploadModel(ctx, &aiplatformpb.UploadModelRequest{Parent: parent, ModelId: modelID, Model: &aiplatformpb.Model{
		DisplayName: "mdm attribution", ArtifactUri: "gs://" + bkt + "/model", ExplanationSpec: explain,
		ContainerSpec: &aiplatformpb.ModelContainerSpec{ImageUri: image, PredictRoute: "/v1/models/linear:predict", HealthRoute: "/v1/models/linear",
			Ports: []*aiplatformpb.Port{{ContainerPort: 7080}}}}})
	if err != nil {
		t.Fatalf("UploadModel: %v", err)
	}
	if _, err := up.Wait(ctx); err != nil {
		t.Fatalf("UploadModel operation: %v", err)
	}
	model := parent + "/models/" + modelID
	cop, err := ec.CreateEndpoint(ctx, &aiplatformpb.CreateEndpointRequest{Parent: parent, EndpointId: "mdm-attr-" + suffix, Endpoint: &aiplatformpb.Endpoint{DisplayName: "mdm attribution"}})
	if err != nil {
		t.Fatalf("CreateEndpoint: %v", err)
	}
	ep, err := cop.Wait(ctx)
	if err != nil {
		t.Fatalf("CreateEndpoint operation: %v", err)
	}
	endpoint := ep.GetName()
	t.Cleanup(func() {
		bg, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if e, err := ec.GetEndpoint(bg, &aiplatformpb.GetEndpointRequest{Name: endpoint}); err == nil {
			for _, d := range e.GetDeployedModels() {
				if op, err := ec.UndeployModel(bg, &aiplatformpb.UndeployModelRequest{Endpoint: endpoint, DeployedModelId: d.GetId()}); err == nil {
					_, _ = op.Wait(bg)
				}
			}
		}
		if op, err := ec.DeleteEndpoint(bg, &aiplatformpb.DeleteEndpointRequest{Name: endpoint}); err == nil {
			_ = op.Wait(bg)
		}
		if op, err := mc.DeleteModel(bg, &aiplatformpb.DeleteModelRequest{Name: model}); err == nil {
			_ = op.Wait(bg)
		}
	})
	dop, err := ec.DeployModel(ctx, &aiplatformpb.DeployModelRequest{Endpoint: endpoint, DeployedModel: &aiplatformpb.DeployedModel{
		Model: model, DisplayName: "mdm attribution", PredictionResources: &aiplatformpb.DeployedModel_DedicatedResources{DedicatedResources: &aiplatformpb.DedicatedResources{
			MachineSpec: &aiplatformpb.MachineSpec{MachineType: "n1-standard-2"}, MinReplicaCount: 1, MaxReplicaCount: 1}}},
		TrafficSplit: map[string]int32{"0": 100}})
	if err != nil {
		t.Fatalf("DeployModel: %v", err)
	}
	dr, err := dop.Wait(ctx)
	if err != nil {
		t.Fatalf("DeployModel operation: %v", err)
	}
	dm := dr.GetDeployedModel().GetId()

	attrThreshold := map[string]*aiplatformpb.ThresholdConfig{"x": {Threshold: &aiplatformpb.ThresholdConfig_Value{Value: 0.5}}}
	job := &aiplatformpb.ModelDeploymentMonitoringJob{DisplayName: "mdm-attr-" + suffix, Endpoint: endpoint,
		ModelDeploymentMonitoringObjectiveConfigs: []*aiplatformpb.ModelDeploymentMonitoringObjectiveConfig{{DeployedModelId: dm,
			ObjectiveConfig: &aiplatformpb.ModelMonitoringObjectiveConfig{
				TrainingDataset: &aiplatformpb.ModelMonitoringObjectiveConfig_TrainingDataset{DataFormat: "csv",
					DataSource: &aiplatformpb.ModelMonitoringObjectiveConfig_TrainingDataset_GcsSource{GcsSource: &aiplatformpb.GcsSource{Uris: []string{"gs://" + bkt + "/train.csv"}}}},
				TrainingPredictionSkewDetectionConfig: &aiplatformpb.ModelMonitoringObjectiveConfig_TrainingPredictionSkewDetectionConfig{
					AttributionScoreSkewThresholds: attrThreshold},
				PredictionDriftDetectionConfig: &aiplatformpb.ModelMonitoringObjectiveConfig_PredictionDriftDetectionConfig{
					AttributionScoreDriftThresholds: attrThreshold},
				ExplanationConfig: &aiplatformpb.ModelMonitoringObjectiveConfig_ExplanationConfig{EnableFeatureAttributes: true},
			}}},
		LoggingSamplingStrategy: &aiplatformpb.SamplingStrategy{RandomSampleConfig: &aiplatformpb.SamplingStrategy_RandomSampleConfig{SampleRate: 1}},
		ModelDeploymentMonitoringScheduleConfig: &aiplatformpb.ModelDeploymentMonitoringScheduleConfig{MonitorInterval: durationpb.New(time.Hour),
			MonitorWindow: durationpb.New(2 * time.Hour)}}
	j, err := jc.CreateModelDeploymentMonitoringJob(ctx, &aiplatformpb.CreateModelDeploymentMonitoringJobRequest{Parent: parent, ModelDeploymentMonitoringJob: job})
	if err != nil {
		t.Fatalf("CreateModelDeploymentMonitoringJob: %v", err)
	}
	name := j.GetName()
	t.Cleanup(func() {
		bg := context.Background()
		_ = jc.PauseModelDeploymentMonitoringJob(bg, &aiplatformpb.PauseModelDeploymentMonitoringJobRequest{Name: name})
		if op, err := jc.DeleteModelDeploymentMonitoringJob(bg, &aiplatformpb.DeleteModelDeploymentMonitoringJobRequest{Name: name}); err == nil {
			_ = op.Wait(bg)
		}
	})
	get := func() *aiplatformpb.ModelDeploymentMonitoringJob {
		t.Helper()
		g, err := jc.GetModelDeploymentMonitoringJob(ctx, &aiplatformpb.GetModelDeploymentMonitoringJobRequest{Name: name})
		if err != nil {
			t.Fatalf("GetModelDeploymentMonitoringJob: %v", err)
		}
		return g
	}
	for deadline := time.Now().Add(time.Minute); get().GetState() != aiplatformpb.JobState_JOB_STATE_RUNNING; time.Sleep(time.Second) {
		if time.Now().After(deadline) {
			t.Fatalf("the job is %v, not RUNNING, a minute after it was created", get().GetState())
		}
	}

	// UpdateModelDeploymentMonitoringJob: a new name and the alert config.
	userAddr := "mdm-" + suffix + "@example.com"
	alert := &aiplatformpb.ModelMonitoringAlertConfig{EnableLogging: true,
		Alert: &aiplatformpb.ModelMonitoringAlertConfig_EmailAlertConfig_{EmailAlertConfig: &aiplatformpb.ModelMonitoringAlertConfig_EmailAlertConfig{UserEmails: []string{userAddr}}}}
	var hook *webhook
	var ps *pubsub.Client
	var sub string
	if channels {
		nc, err := monitoring.NewNotificationChannelClient(ctx, monitoringOpts(h)...)
		if err != nil {
			t.Fatal(err)
		}
		defer nc.Close()
		t.Setenv("PUBSUB_EMULATOR_HOST", pubsubAddr)
		if ps, err = pubsub.NewClient(ctx, project); err != nil {
			t.Fatal(err)
		}
		defer ps.Close()
		topic := "projects/" + project + "/topics/mdm-alerts-" + suffix
		sub = "projects/" + project + "/subscriptions/mdm-alerts-" + suffix
		if _, err := ps.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: topic}); err != nil {
			t.Fatalf("CreateTopic: %v", err)
		}
		defer ps.TopicAdminClient.DeleteTopic(context.Background(), &pubsubpb.DeleteTopicRequest{Topic: topic})
		if _, err := ps.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{Name: sub, Topic: topic}); err != nil {
			t.Fatalf("CreateSubscription: %v", err)
		}
		defer ps.SubscriptionAdminClient.DeleteSubscription(context.Background(), &pubsubpb.DeleteSubscriptionRequest{Subscription: sub})
		hook = newWebhook(t)
		for _, c := range []*monitoringpb.NotificationChannel{
			{Type: "pubsub", DisplayName: "mdm topic", Labels: map[string]string{"topic": topic}},
			{Type: "webhook_tokenauth", DisplayName: "mdm hook", Labels: map[string]string{"url": hook.srv.URL + "/mdm?token=t"}}} {
			got, err := nc.CreateNotificationChannel(ctx, &monitoringpb.CreateNotificationChannelRequest{Name: "projects/" + project, NotificationChannel: c})
			if err != nil {
				t.Fatalf("CreateNotificationChannel: %v", err)
			}
			defer nc.DeleteNotificationChannel(context.Background(), &monitoringpb.DeleteNotificationChannelRequest{Name: got.GetName(), Force: true})
			alert.NotificationChannels = append(alert.NotificationChannels, got.GetName())
		}
	} else {
		t.Log("Cloud Monitoring or Pub/Sub is not served here: the notification channels are not checked")
	}
	if _, err := jc.UpdateModelDeploymentMonitoringJob(ctx, &aiplatformpb.UpdateModelDeploymentMonitoringJobRequest{
		ModelDeploymentMonitoringJob: &aiplatformpb.ModelDeploymentMonitoringJob{Name: name, DisplayName: "renamed"},
		UpdateMask:                   &fieldmaskpb.FieldMask{Paths: []string{"state"}}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("an update of state = %v, want INVALID_ARGUMENT", err)
	}
	uop, err := jc.UpdateModelDeploymentMonitoringJob(ctx, &aiplatformpb.UpdateModelDeploymentMonitoringJobRequest{
		ModelDeploymentMonitoringJob: &aiplatformpb.ModelDeploymentMonitoringJob{Name: name, DisplayName: "mdm-attr-renamed-" + suffix, ModelMonitoringAlertConfig: alert},
		UpdateMask:                   &fieldmaskpb.FieldMask{Paths: []string{"display_name", "model_monitoring_alert_config"}}})
	if err != nil {
		t.Fatalf("UpdateModelDeploymentMonitoringJob: %v", err)
	}
	updated, err := uop.Wait(ctx)
	if err != nil {
		t.Fatalf("UpdateModelDeploymentMonitoringJob operation: %v", err)
	}
	if updated.GetDisplayName() != "mdm-attr-renamed-"+suffix || !updated.GetModelMonitoringAlertConfig().GetEnableLogging() ||
		get().GetModelMonitoringAlertConfig().GetEmailAlertConfig().GetUserEmails()[0] != userAddr {
		t.Errorf("after the update the job = %v", updated)
	}

	advance := func(d string) {
		t.Helper()
		resp, err := http.Post("http://"+stripScheme(endpointAddr)+"/cloudburrow/v1/clock:advance", "application/json", strings.NewReader(`{"duration": "`+d+`"}`))
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("clock:advance = %d: %s", resp.StatusCode, body)
		}
	}
	predict := func(xs ...float64) {
		t.Helper()
		for _, x := range xs {
			inst, _ := structpb.NewValue(map[string]any{"x": x})
			if _, err := pc.Predict(ctx, &aiplatformpb.PredictRequest{Endpoint: endpoint, Instances: []*structpb.Value{inst}}); err != nil {
				t.Fatalf("Predict: %v", err)
			}
		}
	}
	search := func(obj aiplatformpb.ModelDeploymentMonitoringObjectiveType, d time.Duration) *aiplatformpb.ModelMonitoringStatsAnomalies {
		t.Helper()
		for deadline := time.Now().Add(d); ; time.Sleep(2 * time.Second) {
			it := jc.SearchModelDeploymentMonitoringStatsAnomalies(ctx, &aiplatformpb.SearchModelDeploymentMonitoringStatsAnomaliesRequest{
				ModelDeploymentMonitoringJob: name, DeployedModelId: dm,
				Objectives: []*aiplatformpb.SearchModelDeploymentMonitoringStatsAnomaliesRequest_StatsAnomaliesObjective{{Type: obj}}})
			if s, err := it.Next(); err == nil && s.GetAnomalyCount() > 0 {
				return s
			}
			if time.Now().After(deadline) {
				t.Fatalf("no %v anomaly %s after the run was due; the job: %v", obj, d, get())
			}
		}
	}

	// Run 1: attribution skew against the training data (x = 1..4 against
	// 50..53: attributions of 2x).
	predict(50, 51, 52, 53)
	advance("3601s")
	skew := search(aiplatformpb.ModelDeploymentMonitoringObjectiveType_FEATURE_ATTRIBUTION_SKEW, 3*time.Minute)
	if len(skew.GetFeatureStats()) != 1 || skew.GetFeatureStats()[0].GetFeatureDisplayName() != "x" ||
		skew.GetFeatureStats()[0].GetPredictionStats()[0].GetScore() <= 0.5 {
		t.Errorf("attribution skew = %v; want x's above 0.5", skew)
	}
	m := waitMail(t, console, userAddr, "Model Monitoring anomalies: mdm-attr-renamed-"+suffix, time.Minute)
	if !strings.Contains(m.Text, "FEATURE_ATTRIBUTION_SKEW") || !strings.Contains(m.Text, "alerting threshold 0.5") {
		t.Errorf("the anomaly email = %q", m.Text)
	}

	// Run 2, an interval on: the two-hour window now holds these
	// predictions and the previous one run 1's, so the attributions drift.
	predict(1, 2, 3, 4)
	advance("3601s")
	drift := search(aiplatformpb.ModelDeploymentMonitoringObjectiveType_FEATURE_ATTRIBUTION_DRIFT, 3*time.Minute)
	if len(drift.GetFeatureStats()) != 1 || drift.GetFeatureStats()[0].GetFeatureDisplayName() != "x" {
		t.Errorf("attribution drift = %v; want x", drift)
	}

	if logAddr != "" {
		var objectives []string
		for _, e := range anomalyEntries(t, ctx, logAddr, project, "modelDeploymentMonitoringJob", name, time.Minute) {
			p := payloadOf(e)
			if e.Resource.GetType() != "aiplatform.googleapis.com/ModelDeploymentMonitoringJob" || p["deployedModelId"] != dm {
				t.Errorf("anomaly entry %v %v", e.Resource, p)
			}
			objectives = append(objectives, fmt.Sprint(p["anomalyObjective"]))
		}
		if got := strings.Join(objectives, ","); !strings.Contains(got, "FEATURE_ATTRIBUTION_SKEW") || !strings.Contains(got, "FEATURE_ATTRIBUTION_DRIFT") {
			t.Errorf("the anomaly entries' objectives = %s", got)
		}
	} else {
		t.Log("Cloud Logging is not served here: the anomaly entries are not checked")
	}
	if channels {
		got := false
		for deadline := time.Now().Add(time.Minute); !got && time.Now().Before(deadline); {
			pctx, pcancel := context.WithTimeout(ctx, 5*time.Second)
			resp, _ := ps.SubscriptionAdminClient.Pull(pctx, &pubsubpb.PullRequest{Subscription: sub, MaxMessages: 10})
			pcancel()
			for _, msg := range resp.GetReceivedMessages() {
				var p map[string]any
				got = got || json.Unmarshal(msg.GetMessage().GetData(), &p) == nil && p["modelDeploymentMonitoringJob"] == name
			}
		}
		if !got {
			t.Error("the Pub/Sub channel received no anomalies")
		}
		hook.mu.Lock()
		n := 0
		for _, b := range hook.bodies {
			if b["modelDeploymentMonitoringJob"] == name {
				n++
			}
		}
		hook.mu.Unlock()
		if n < 2 {
			t.Errorf("the webhook received %d runs' anomalies, want 2", n)
		}
	}

	if err := jc.PauseModelDeploymentMonitoringJob(ctx, &aiplatformpb.PauseModelDeploymentMonitoringJobRequest{Name: name}); err != nil {
		t.Fatalf("PauseModelDeploymentMonitoringJob: %v", err)
	}
	op, err := jc.DeleteModelDeploymentMonitoringJob(ctx, &aiplatformpb.DeleteModelDeploymentMonitoringJobRequest{Name: name})
	if err != nil {
		t.Fatalf("DeleteModelDeploymentMonitoringJob: %v", err)
	}
	if err := op.Wait(ctx); err != nil {
		t.Fatalf("DeleteModelDeploymentMonitoringJob operation: %v", err)
	}
}
