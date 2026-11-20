package vertexai

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/documentai/apiv1/documentaipb"
	"google.golang.org/grpc/codes"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// TestDocumentAIProcessorVersions: the pretrained versions, the default's
// change and Google's refusals.
func TestDocumentAIProcessorVersions(t *testing.T) {
	ctx := context.Background()
	d := newDocAI(t, "")
	p := createProcessor(t, d, "OCR_PROCESSOR", "ocr")
	list, err := d.ListProcessorVersions(ctx, &documentaipb.ListProcessorVersionsRequest{Parent: p.GetName()})
	if err != nil || len(list.GetProcessorVersions()) != 3 || !strings.HasSuffix(list.ProcessorVersions[0].GetName(), "pretrained-ocr-v2.1-2024-08-07") ||
		!list.ProcessorVersions[0].GetGoogleManaged() || list.ProcessorVersions[0].GetState() != documentaipb.ProcessorVersion_DEPLOYED {
		t.Fatalf("ListProcessorVersions: %v %v", list, err)
	}
	if v, err := d.GetProcessorVersion(ctx, &documentaipb.GetProcessorVersionRequest{Name: p.GetName() + "/processorVersions/stable"}); err != nil ||
		!strings.HasSuffix(v.GetName(), "v2.1-2024-08-07") {
		t.Errorf("stable: %v %v", v, err)
	}
	if _, err := d.GetProcessorVersion(ctx, &documentaipb.GetProcessorVersionRequest{Name: p.GetName() + "/processorVersions/rc"}); code(err) != codes.NotFound {
		t.Errorf("rc: %v", err)
	}
	v12 := p.GetName() + "/processorVersions/pretrained-ocr-v1.2-2022-11-10"
	op, err := d.SetDefaultProcessorVersion(ctx, &documentaipb.SetDefaultProcessorVersionRequest{Processor: p.GetName(), DefaultProcessorVersion: v12})
	if err != nil || !op.GetDone() {
		t.Fatalf("SetDefaultProcessorVersion: %v %v", op, err)
	}
	if got, _ := d.GetProcessor(ctx, &documentaipb.GetProcessorRequest{Name: p.GetName()}); got.GetDefaultProcessorVersion() != v12 {
		t.Errorf("default: %s", got.GetDefaultProcessorVersion())
	}
	if _, err := d.SetDefaultProcessorVersion(ctx, &documentaipb.SetDefaultProcessorVersionRequest{Processor: p.GetName(),
		DefaultProcessorVersion: p.GetName() + "/processorVersions/nope"}); code(err) != codes.NotFound {
		t.Errorf("unknown default: %v", err)
	}
	for _, f := range []func() error{
		func() error {
			_, err := d.DeployProcessorVersion(ctx, &documentaipb.DeployProcessorVersionRequest{Name: v12})
			return err
		},
		func() error {
			_, err := d.UndeployProcessorVersion(ctx, &documentaipb.UndeployProcessorVersionRequest{Name: v12})
			return err
		},
		func() error {
			_, err := d.DeleteProcessorVersion(ctx, &documentaipb.DeleteProcessorVersionRequest{Name: list.ProcessorVersions[0].GetName()})
			return err
		},
	} {
		if err := f(); code(err) != codes.FailedPrecondition {
			t.Errorf("pretrained change: %v", err)
		}
	}
	if _, err := d.TrainProcessorVersion(ctx, &documentaipb.TrainProcessorVersionRequest{Parent: p.GetName()}); code(err) != codes.InvalidArgument {
		t.Errorf("train: %v", err)
	}
}

// fakeBucket is Cloud Storage in memory.
type fakeBucket struct {
	mu   sync.Mutex
	objs map[string][]byte
	ct   map[string]string
}

func (f *fakeBucket) Read(_ context.Context, bucket, object string, _ int64) ([]byte, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.objs[bucket+"/"+object]
	if !ok {
		return nil, "", apierror.NotFound("gs://%s/%s not found", bucket, object)
	}
	return b, f.ct[bucket+"/"+object], nil
}

func (f *fakeBucket) Write(_ context.Context, bucket, object, ct string, data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objs[bucket+"/"+object], f.ct[bucket+"/"+object] = data, ct
	return nil
}

func (f *fakeBucket) List(_ context.Context, bucket, prefix string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for k := range f.objs {
		if b, o, _ := strings.Cut(k, "/"); b == bucket && strings.HasPrefix(o, prefix) {
			out = append(out, o)
		}
	}
	return out, nil
}

// TestDocumentAIBatchProcess: gs:// in, Documents written under the
// operation's ID, statuses in the metadata.
func TestDocumentAIBatchProcess(t *testing.T) {
	ctx := context.Background()
	scan, err := os.ReadFile("../../ocr/testdata/scan.png")
	if err != nil {
		t.Fatal(err)
	}
	d := newDocAI(t, "")
	fb := &fakeBucket{objs: map[string][]byte{"in/docs/scan.png": scan}, ct: map[string]string{"in/docs/scan.png": "image/png"}}
	d.s.UseStorage(fb)
	p := createProcessor(t, d, "OCR_PROCESSOR", "ocr")
	out := &documentaipb.DocumentOutputConfig{Destination: &documentaipb.DocumentOutputConfig_GcsOutputConfig_{
		GcsOutputConfig: &documentaipb.DocumentOutputConfig_GcsOutputConfig{GcsUri: "gs://out/results"}}}
	if _, err := d.BatchProcessDocuments(ctx, &documentaipb.BatchProcessRequest{Name: p.GetName(), DocumentOutputConfig: out}); code(err) != codes.InvalidArgument {
		t.Errorf("no input: %v", err)
	}
	op, err := d.BatchProcessDocuments(ctx, &documentaipb.BatchProcessRequest{Name: p.GetName(), DocumentOutputConfig: out,
		InputDocuments: &documentaipb.BatchDocumentsInputConfig{Source: &documentaipb.BatchDocumentsInputConfig_GcsPrefix{
			GcsPrefix: &documentaipb.GcsPrefix{GcsUriPrefix: "gs://in/docs/"}}}})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Minute)
	for {
		got, _ := d.s.ops.Get(op.GetName())
		if got.Done() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("batch did not finish")
		}
		time.Sleep(50 * time.Millisecond)
	}
	got, _ := d.s.ops.Get(op.GetName())
	meta := got.Metadata.(*documentaipb.BatchProcessMetadata)
	id := op.GetName()[strings.LastIndex(op.GetName(), "/")+1:]
	if meta.GetState() != documentaipb.BatchProcessMetadata_SUCCEEDED || len(meta.GetIndividualProcessStatuses()) != 1 ||
		meta.IndividualProcessStatuses[0].GetOutputGcsDestination() != "gs://out/results/"+id+"/0" {
		t.Fatalf("metadata: %v", meta)
	}
	b, _, err := fb.Read(ctx, "out", "results/"+id+"/0/scan-0.json", 0)
	if err != nil || !strings.Contains(string(b), "INVOICE") {
		t.Fatalf("output: %v %.200s", err, b)
	}
}
