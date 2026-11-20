//go:build browser

package browser

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	documentai "cloud.google.com/go/documentai/apiv1"
	"cloud.google.com/go/documentai/apiv1/documentaipb"
	"cloud.google.com/go/storage"
	"google.golang.org/api/iterator"
	"google.golang.org/protobuf/encoding/protojson"
)

// TestDocumentAIProcessorVersionsAndEvaluationsThroughThePage (#1953), in
// the run shard: a processor's Evaluate & test tab lists the default
// version's evaluations (none: no served type can be evaluated) and says
// why; Batch process with a chosen version and one page per shard writes
// the two-page PDF's Document as two shards, which the official storage
// client reads back with their shard_info.
func TestDocumentAIProcessorVersionsAndEvaluationsThroughThePage(t *testing.T) {
	needService(t, "documentai")
	sc, _ := storageSDK(t)
	c, err := documentai.NewDocumentProcessorClient(context.Background(), localAIDial(t)...)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	ctx := context.Background()
	project := uniqueProject(t)
	proc, err := c.CreateProcessor(ctx, &documentaipb.CreateProcessorRequest{Parent: "projects/" + project + "/locations/us",
		Processor: &documentaipb.Processor{Type: "OCR_PROCESSOR", DisplayName: "browser-shards"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if op, err := c.DeleteProcessor(context.Background(), &documentaipb.DeleteProcessorRequest{Name: proc.GetName()}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	bucket := project + "-shards"
	bh := sc.Bucket(bucket)
	if err := bh.Create(ctx, project, nil); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	var written []string
	t.Cleanup(func() {
		for _, n := range append(written, "in/two-pages.pdf") {
			_ = bh.Object(n).Delete(context.Background())
		}
		_ = bh.Delete(context.Background())
	})
	pdf, err := os.ReadFile(filepath.Join("..", "..", "internal", "embedding", "testdata", "two-pages.pdf"))
	if err != nil {
		t.Fatal(err)
	}
	w := bh.Object("in/two-pages.pdf").NewWriter(ctx)
	w.ContentType = "application/pdf"
	if _, err := w.Write(pdf); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("upload: %v", err)
	}

	id := proc.GetName()[strings.LastIndex(proc.GetName(), "/")+1:]
	p := open(t)
	p.navigate("/document-ai/processors/" + id + "?project=" + project)
	p.waitFor(settled)
	p.clickText("#view [role=tab]", "Evaluate & test")
	p.waitFor(`[...document.querySelectorAll("#view [role=tabpanel]")].some((s) => !s.hidden && s.textContent.includes("cannot be evaluated"))`)
	var panel string
	p.eval(`[...document.querySelectorAll("#view [role=tabpanel]")].filter((s) => !s.hidden).map((s) => s.textContent).join(" ")`, &panel)
	if !strings.Contains(panel, "pretrained-ocr-v2.0-2023-06-02") {
		t.Errorf("Evaluate & test reads %q", panel)
	}

	got := p.pageAction("Batch process", map[string]string{"input": "gs://" + bucket + "/in/two-pages.pdf", "output": "gs://" + bucket + "/out",
		"version": "pretrained-ocr-v1.2-2022-11-10", "pagesPerShard": "1"}, "")
	if !strings.Contains(got, "Succeeded") {
		t.Fatalf("Batch process answered %q", got)
	}
	it := bh.Objects(ctx, &storage.Query{Prefix: "out/"})
	for {
		a, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		written = append(written, a.Name)
	}
	sort.Strings(written)
	if len(written) != 2 || !strings.HasSuffix(written[0], "/0/two-pages-0.json") || !strings.HasSuffix(written[1], "/0/two-pages-1.json") {
		t.Fatalf("the batch wrote %q", written)
	}
	for i, n := range written {
		rd, err := bh.Object(n).NewReader(ctx)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(rd)
		_ = rd.Close()
		var doc documentaipb.Document
		if err := protojson.Unmarshal(b, &doc); err != nil {
			t.Fatal(err)
		}
		if doc.GetShardInfo().GetShardIndex() != int64(i) || doc.GetShardInfo().GetShardCount() != 2 || len(doc.GetPages()) != 1 ||
			doc.GetPages()[0].GetPageNumber() != int32(i+1) {
			t.Errorf("shard %d: shard_info %v, %d pages", i, doc.GetShardInfo(), len(doc.GetPages()))
		}
	}
}
