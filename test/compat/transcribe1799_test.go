//go:build compat

package compat

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	"cloud.google.com/go/bigquery/connection/apiv1/connectionpb"
	"cloud.google.com/go/iam"
)

// TestBigQueryMLTranscribeLongAndAMR (#1799): ML.TRANSCRIBE through the
// official cloud.google.com/go/bigquery, storage and
// bigquery/connection/apiv1 clients, for the audio the first round
// refused or did not run there: JFK's sample as AMR (narrowband) and
// AMR-WB files answers his sentence; an 81-second MP3, long audio as
// Speech-to-Text counts it (over the 60 seconds a synchronous Recognize
// takes), is transcribed whole and billed about 81 s (ML.TRANSCRIBE's
// documented limit is 30 minutes; unit TestDecodeLongMP3AAC reads 216 s of
// it, past the 180 seconds CloudBurrow's MP3 decoder stopped at); a stereo MP3
// with SEPARATE_RECOGNITION_PER_CHANNEL is decoded per channel, billed for
// both; and diarization_config is INVALID_ARGUMENT, as the chirp model
// ML.TRANSCRIBE runs has no diarization in Speech-to-Text's supported
// languages table.
//
// covers: bigquery.jobs.query
func TestBigQueryMLTranscribeLongAndAMR(t *testing.T) {
	h := New(t)
	if h.Endpoint(EnvLocalAI) == "" {
		t.Skip("this instance serves no local AI endpoint")
	}
	c, project := bigqueryClient(t, h)
	cc, _ := remoteConnectionClient(t, h)
	sc := storageClient(t, h)
	// 20 minutes: the test took 666 s with a 184-second MP3 in main runs
	// 45147981577 and 45171873407 (448 s of it that MP3), and ran out of
	// its 15 minutes on a slower runner in 45202563361 (10m16s for the MP3
	// alone). With the 81-second MP3 about 415 s, 570 s at that runner's
	// pace.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	t.Cleanup(cancel)
	read := func(path string) []byte {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	amr := read("../../internal/speech/testdata/jfk.amr")
	awb := read("../../internal/speech/testdata/jfk.awb")
	// MP3 frames concatenate: 75 copies of a 1.08-second tone without
	// tags are one 81-second stream, over Speech-to-Text's 60 (whisper
	// reads a tone slowly: 184 s took 7m28s on a runner, 10m16s on a slow
	// one).
	longMP3 := bytes.Repeat(read("../../internal/embedding/testdata/tone-notag-16k.mp3"), 75)
	stereoMP3 := read("../../internal/embedding/testdata/tone-stereo-44k.mp3")

	id := strings.ReplaceAll(h.Project(), "-", "_") + "_speech2"
	cn, err := cc.CreateConnection(ctx, &connectionpb.CreateConnectionRequest{Parent: "projects/" + project + "/locations/us", ConnectionId: id,
		Connection: &connectionpb.Connection{Properties: &connectionpb.Connection_CloudResource{CloudResource: &connectionpb.CloudResourceProperties{}}}})
	if err != nil {
		t.Fatalf("CreateConnection: %v", err)
	}
	t.Cleanup(func() { _ = cc.DeleteConnection(ctx, &connectionpb.DeleteConnectionRequest{Name: cn.Name}) })
	grantRemoteModel(t, h, cc, project, cn)
	conn := project + ".us." + id
	ds := "compat_speech2_" + strings.ReplaceAll(h.Project(), "-", "_")
	if err := c.Dataset(ds).Create(ctx, &bigquery.DatasetMetadata{Location: "US"}); err != nil {
		t.Fatalf("Dataset.Create: %v", err)
	}
	t.Cleanup(func() { _ = c.Dataset(ds).DeleteWithContents(ctx) })
	bh := sc.Bucket(strings.ReplaceAll(h.Project(), "_", "-") + "-speech2")
	if err := bh.Create(ctx, h.Project(), nil); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	t.Cleanup(func() { emptyAndDelete(ctx, bh) })
	for name, o := range map[string]struct {
		ct   string
		data []byte
	}{
		"amr/jfk.amr":       {"audio/amr", amr},
		"amr/jfk.awb":       {"audio/amr-wb", awb},
		"long/tone.mp3":     {"audio/mpeg", longMP3},
		"stereo/stereo.mp3": {"audio/mpeg", stereoMP3},
	} {
		w := bh.Object(name).NewWriter(ctx)
		w.ContentType = o.ct
		if _, err := w.Write(o.data); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	pol, err := bh.IAM().Policy(ctx)
	if err != nil {
		t.Fatalf("bucket IAM Policy: %v", err)
	}
	pol.Add("serviceAccount:"+cn.GetCloudResource().GetServiceAccountId(), iam.RoleName("roles/storage.objectViewer"))
	if err := bh.IAM().SetPolicy(ctx, pol); err != nil {
		t.Fatalf("bucket IAM SetPolicy: %v", err)
	}
	run := func(sql string) error { return mlRunIn(ctx, c, sql) }
	for _, tbl := range []string{"amr", "long", "stereo"} {
		if err := run(fmt.Sprintf("CREATE EXTERNAL TABLE `%s.%s` WITH CONNECTION `%s` OPTIONS(object_metadata = 'SIMPLE', uris = ['gs://%s/%s/*'])",
			ds, tbl, conn, bh.BucketName(), tbl)); err != nil {
			t.Fatalf("CREATE EXTERNAL TABLE %s: %v", tbl, err)
		}
	}
	if err := run(fmt.Sprintf("CREATE MODEL `%s.speech` REMOTE WITH CONNECTION `%s` OPTIONS(REMOTE_SERVICE_TYPE = 'CLOUD_AI_SPEECH_TO_TEXT_V2')", ds, conn)); err != nil {
		t.Fatalf("CREATE MODEL: %v", err)
	}
	query := func(tbl, features string) []map[string]bigquery.Value {
		return mlQueryIn(t, ctx, c, fmt.Sprintf("SELECT uri, transcripts, ml_transcribe_status AS status, "+
			"LAX_INT64(ml_transcribe_result.metadata.total_billed_duration.seconds) AS billed "+
			`FROM ML.TRANSCRIBE(MODEL `+"`%s.speech`, TABLE `%s.%s`"+`, recognition_config => (JSON '{"language_codes": ["en-US"], "model": "chirp", `+
			`"auto_decoding_config": {}%s}')) ORDER BY uri`, ds, ds, tbl, features))
	}

	rows := query("amr", "")
	if len(rows) != 2 {
		t.Fatalf("AMR rows: %v", rows)
	}
	for _, row := range rows {
		text, _ := row["transcripts"].(string)
		t.Logf("AMR row (recorded): %v %q billed %v", row["uri"], text, row["billed"])
		if row["status"] != "" || !strings.Contains(strings.ToLower(text), "what your country can do for you") || row["billed"] != int64(11) {
			t.Errorf("AMR row %v: %v", row["uri"], row)
		}
	}

	start := time.Now()
	rows = query("long", "")
	t.Logf("81-second MP3 (recorded) in %v: %v", time.Since(start).Round(time.Second), rows)
	if billed, _ := rows[0]["billed"].(int64); len(rows) != 1 || rows[0]["status"] != "" || billed < 79 || billed > 83 {
		t.Errorf("81-second MP3 row: %v", rows)
	}

	rows = query("stereo", `, "features": {"multi_channel_mode": "SEPARATE_RECOGNITION_PER_CHANNEL"}`)
	mono := query("stereo", "")
	if len(rows) != 1 || len(mono) != 1 || rows[0]["status"] != "" || mono[0]["status"] != "" {
		t.Fatalf("stereo MP3 rows: %v, %v", rows, mono)
	}
	both, _ := rows[0]["billed"].(int64)
	one, _ := mono[0]["billed"].(int64)
	t.Logf("stereo MP3 (recorded): billed %d s per channel, %d s for one", both, one)
	if one < 1 || both != 2*one {
		t.Errorf("stereo MP3 per channel billed %d s, %d s for the first channel alone", both, one)
	}

	if err := run(fmt.Sprintf(`SELECT * FROM ML.TRANSCRIBE(MODEL `+"`%s.speech`, TABLE `%s.amr`"+`, recognition_config => (JSON '{"language_codes": ["en-US"], `+
		`"model": "chirp", "auto_decoding_config": {}, "features": {"diarization_config": {"min_speaker_count": 2, "max_speaker_count": 2}}}'))`, ds, ds)); err == nil ||
		!strings.Contains(err.Error(), "features.diarizationConfig") {
		t.Errorf("diarization_config on chirp: %v", err)
	}
}
