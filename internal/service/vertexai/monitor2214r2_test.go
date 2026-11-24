package vertexai

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	betapb "cloud.google.com/go/aiplatform/apiv1beta1/aiplatformpb"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// tfExample is a serialized tf.train.Example of string, float and int
// features.
func tfExample(strs map[string]string, floats map[string]float32, ints map[string]int64) []byte {
	entry := func(key string, feature []byte) []byte {
		var e []byte
		e = protowire.AppendTag(e, 1, protowire.BytesType)
		e = protowire.AppendString(e, key)
		e = protowire.AppendTag(e, 2, protowire.BytesType)
		return protowire.AppendBytes(e, feature)
	}
	var features []byte
	add := func(key string, kind protowire.Number, list []byte) {
		var f []byte
		f = protowire.AppendTag(f, kind, protowire.BytesType)
		f = protowire.AppendBytes(f, list)
		features = protowire.AppendTag(features, 1, protowire.BytesType)
		features = protowire.AppendBytes(features, entry(key, f))
	}
	for k, v := range strs {
		var l []byte
		l = protowire.AppendTag(l, 1, protowire.BytesType)
		add(k, 1, protowire.AppendString(l, v))
	}
	for k, v := range floats {
		var packed []byte
		packed = binary.LittleEndian.AppendUint32(packed, math.Float32bits(v))
		var l []byte
		l = protowire.AppendTag(l, 1, protowire.BytesType)
		add(k, 2, protowire.AppendBytes(l, packed))
	}
	for k, v := range ints {
		var l []byte
		l = protowire.AppendTag(l, 1, protowire.VarintType)
		add(k, 3, protowire.AppendVarint(l, uint64(v)))
	}
	var ex []byte
	ex = protowire.AppendTag(ex, 1, protowire.BytesType)
	return protowire.AppendBytes(ex, features)
}

func tfRecords(recs ...[]byte) []byte {
	var out []byte
	for _, r := range recs {
		head := binary.LittleEndian.AppendUint64(nil, uint64(len(r)))
		out = append(out, head...)
		out = binary.LittleEndian.AppendUint32(out, maskedCRC(head))
		out = append(out, r...)
		out = binary.LittleEndian.AppendUint32(out, maskedCRC(r))
	}
	return out
}

func TestParseTFRecordExamples(t *testing.T) {
	data := tfRecords(
		tfExample(map[string]string{"plan": "basic"}, map[string]float32{"spend": 1.5}, map[string]int64{"age": 30}),
		tfExample(map[string]string{"plan": "pro"}, map[string]float32{"spend": -2}, map[string]int64{"age": -4}))
	rows, err := parseRows(data, "tf-record")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0]["plan"] != "basic" || rows[0]["spend"] != 1.5 || rows[0]["age"] != 30.0 || rows[1]["age"] != -4.0 || rows[1]["spend"] != -2.0 {
		t.Fatalf("rows = %v", rows)
	}
	bad := append([]byte{}, data...)
	bad[len(bad)-1] ^= 0xff
	if _, err := parseRows(bad, "tf-record"); err == nil || !strings.Contains(err.Error(), "CRC") {
		t.Fatalf("a corrupt record = %v, want a CRC error", err)
	}
	if _, err := parseRows([]byte("plain text"), "tf-record"); err == nil {
		t.Fatal("text was read as TFRecord")
	}
}

func TestMonitoringReadsTFRecordBigQueryAndDatasets(t *testing.T) {
	s, runner := newPipelineServer(t)
	ctx := context.Background()
	_ = runner.WriteObject(ctx, "b", "train.tfrecord", "", tfRecords(tfExample(map[string]string{"plan": "basic"}, nil, nil)))
	in := &betapb.ModelMonitoringInput{Dataset: &betapb.ModelMonitoringInput_ColumnizedDataset{ColumnizedDataset: &betapb.ModelMonitoringInput_ModelMonitoringDataset{
		DataLocation: &betapb.ModelMonitoringInput_ModelMonitoringDataset_GcsSource{GcsSource: &betapb.ModelMonitoringInput_ModelMonitoringDataset_ModelMonitoringGcsSource{
			GcsUri: "gs://b/train.tfrecord", Format: betapb.ModelMonitoringInput_ModelMonitoringDataset_ModelMonitoringGcsSource_TF_RECORD}}}}}
	rows, _, err := s.readInput(ctx, runner, in, nil, "projects/p/locations/us-central1/models/m", time.Now())
	if err != nil || len(rows) != 1 || rows[0]["plan"] != "basic" {
		t.Fatalf("TF_RECORD rows = %v, %v", rows, err)
	}
	// BigQuery with no BigQuery on the instance is FAILED_PRECONDITION.
	bq := &betapb.ModelMonitoringInput{Dataset: &betapb.ModelMonitoringInput_ColumnizedDataset{ColumnizedDataset: &betapb.ModelMonitoringInput_ModelMonitoringDataset{
		DataLocation: &betapb.ModelMonitoringInput_ModelMonitoringDataset_BigquerySource{BigquerySource: &betapb.ModelMonitoringInput_ModelMonitoringDataset_ModelMonitoringBigQuerySource{
			Connection: &betapb.ModelMonitoringInput_ModelMonitoringDataset_ModelMonitoringBigQuerySource_TableUri{TableUri: "bq://p.d.t"}}}}}}
	if _, _, err := s.readInput(ctx, runner, bq, nil, "projects/p/locations/us-central1/models/m", time.Now()); err == nil || !strings.Contains(err.Error(), "BigQuery is not served") {
		t.Fatalf("BigQuery without BigQuery = %v", err)
	}
	// A managed dataset that does not exist is NOT_FOUND.
	if _, err := s.datasetRows(ctx, runner, "projects/p/locations/us-central1/datasets/404"); err == nil {
		t.Fatal("a missing dataset was read")
	}
}

func TestBigQueryQueryRows(t *testing.T) {
	bq := bqFunc(func(method, path string, body []byte) (int, []byte, error) {
		if method == "POST" {
			return 200, []byte(`{"jobComplete":false,"jobReference":{"jobId":"j1","location":"US"}}`), nil
		}
		if strings.Contains(path, "pageToken=p2") {
			return 200, []byte(`{"jobComplete":true,"rows":[{"f":[{"v":"2"},{"v":"b"}]}]}`), nil
		}
		return 200, []byte(`{"jobComplete":true,"schema":{"fields":[{"name":"n","type":"INTEGER"},{"name":"s","type":"STRING"}]},` +
			`"rows":[{"f":[{"v":"1"},{"v":"a"}]}],"pageToken":"p2","jobReference":{"jobId":"j1","location":"US"}}`), nil
	})
	rows, err := bigQueryQueryRows(context.Background(), bq, "p", "SELECT 1")
	if err != nil || len(rows) != 2 || rows[0]["n"] != 1.0 || rows[1]["s"] != "b" {
		t.Fatalf("rows = %v, %v", rows, err)
	}
}

type bqFunc func(method, path string, body []byte) (int, []byte, error)

func (f bqFunc) BigQueryREST(_ context.Context, method, path string, body []byte) (int, []byte, error) {
	return f(method, path, body)
}
func (bqFunc) ReadObject(context.Context, string, string, int64) ([]byte, error) { return nil, nil }
func (bqFunc) WriteObject(context.Context, string, string, string, []byte) error { return nil }
func (bqFunc) ListObjects(context.Context, string, string) ([]string, error)     { return nil, nil }

func TestAttributionScoresAreMeanAbsolute(t *testing.T) {
	calls := 0
	explain := func(insts []*structpb.Value) (*aiplatformpb.ExplainResponse, error) {
		calls++
		out := &aiplatformpb.ExplainResponse{}
		for _, in := range insts {
			f := in.GetStructValue().GetFields()
			attr, _ := structpb.NewStruct(map[string]any{"x": -f["x"].GetNumberValue(), "y": []any{1.0, 2.0}})
			out.Explanations = append(out.Explanations, &aiplatformpb.Explanation{Attributions: []*aiplatformpb.Attribution{{FeatureAttributions: structpb.NewStructValue(attr)}}})
		}
		return out, nil
	}
	rows := []map[string]any{{"x": 1.0, "y": 0.0, "z": "ignored"}, {"x": 3.0, "y": 0.0}}
	got, n, err := attributionScores(rows, []string{"x", "y"}, explain)
	if err != nil || n != 2 || got["x"] != 2 || got["y"] != 3 || calls != 1 {
		t.Fatalf("scores = %v (%d rows, %d calls), %v", got, n, calls, err)
	}
}

type fakeNotifier struct {
	mu       sync.Mutex
	channels [][]string
	payloads []string
	emails   []string
}

func (f *fakeNotifier) NotifyChannels(_ context.Context, _ string, names []string, payload []byte, _, _ string) []AlertDelivery {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.channels = append(f.channels, names)
	f.payloads = append(f.payloads, string(payload))
	return nil
}

func (f *fakeNotifier) SendEmail(_ context.Context, project, _, _ string, to []string, subject, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.emails = append(f.emails, project+"|"+strings.Join(to, ",")+"|"+subject+"|"+body)
}

func TestMonitoringAlertsAreDelivered(t *testing.T) {
	s := NewServer(nil)
	n := &fakeNotifier{}
	s.SetAlertNotifier(n)
	job := "projects/p/locations/us-central1/modelMonitors/m1/modelMonitoringJobs/7"
	ns := &betapb.ModelMonitoringNotificationSpec{EmailConfig: &betapb.ModelMonitoringNotificationSpec_EmailConfig{UserEmails: []string{"a@example.com"}},
		NotificationChannelConfigs: []*betapb.ModelMonitoringNotificationSpec_NotificationChannelConfig{{NotificationChannel: "projects/p/notificationChannels/1"}}}
	alert := &mmAlert{Alert: &betapb.ModelMonitoringAlert{StatsName: "plan", ObjectiveType: objFeatureDrift, AlertTime: timestamppb.Now(),
		Anomaly: &betapb.ModelMonitoringAnomaly{Anomaly: &betapb.ModelMonitoringAnomaly_TabularAnomaly_{TabularAnomaly: &betapb.ModelMonitoringAnomaly_TabularAnomaly{Summary: "l_infinity of plan is 0.5"}}}}}
	s.notifyMonitorAlerts(context.Background(), job, ns, []*mmAlert{alert})
	if len(n.channels) != 1 || n.channels[0][0] != "projects/p/notificationChannels/1" || !strings.Contains(n.payloads[0], `"statsName":"plan"`) {
		t.Fatalf("channels = %v %v", n.channels, n.payloads)
	}
	if len(n.emails) != 1 || !strings.HasPrefix(n.emails[0], "p|a@example.com|") || !strings.Contains(n.emails[0], "l_infinity of plan is 0.5") {
		t.Fatalf("emails = %v", n.emails)
	}
	// v1: only anomalies (a non-zero threshold exceeded) are sent.
	j := &aiplatformpb.ModelDeploymentMonitoringJob{Name: "projects/p/locations/us-central1/modelDeploymentMonitoringJobs/9", DisplayName: "mdm",
		ModelMonitoringAlertConfig: &aiplatformpb.ModelMonitoringAlertConfig{NotificationChannels: []string{"projects/p/notificationChannels/2"},
			Alert: &aiplatformpb.ModelMonitoringAlertConfig_EmailAlertConfig_{EmailAlertConfig: &aiplatformpb.ModelMonitoringAlertConfig_EmailAlertConfig{UserEmails: []string{"b@example.com"}}}}}
	stats := []*mdmStat{
		{Feature: "x", DeployedModel: "d", Threshold: 0.3, Prediction: &aiplatformpb.FeatureStatsAnomaly{Score: 0.9}},
		{Feature: "y", DeployedModel: "d", Threshold: 0.3, Prediction: &aiplatformpb.FeatureStatsAnomaly{Score: 0.1}},
		{Feature: "z", DeployedModel: "d", Threshold: 0, Prediction: &aiplatformpb.FeatureStatsAnomaly{Score: 0.9}},
	}
	s.notifyMDMAnomalies(context.Background(), j, time.Now(), stats)
	var p struct{ Anomalies []map[string]any }
	_ = json.Unmarshal([]byte(n.payloads[1]), &p)
	if len(p.Anomalies) != 1 || p.Anomalies[0]["featureDisplayName"] != "x" || len(n.emails) != 2 {
		t.Fatalf("v1 payload = %s, emails %v", n.payloads[1], n.emails)
	}
	if err := checkChannels("f", []string{"notificationChannels/1"}); err == nil {
		t.Fatal("a malformed channel name was accepted")
	}
	if err := checkEmails("f", []string{"not-an-address"}); err == nil {
		t.Fatal("a malformed email was accepted")
	}
}

func TestClockAdvanceRunsDueMonitoring(t *testing.T) {
	s := NewServer(nil)
	before := s.notebooks().now()
	for body, want := range map[string]int{`{"duration": "-1s"}`: 400, `{"duration": "soon"}`: 400, `{"duration": "3600s"}`: 200} {
		rec := httptest.NewRecorder()
		s.serveClock(rec, httptest.NewRequest(http.MethodPost, ClockAdvancePath, strings.NewReader(body)))
		if rec.Code != want {
			t.Errorf("%s = %d %s, want %d", body, rec.Code, rec.Body, want)
		}
	}
	if d := s.notebooks().now().Sub(before); d < time.Hour {
		t.Fatalf("the clock moved %v, want an hour", d)
	}
}
