//go:build browser

package browser

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	speech "cloud.google.com/go/speech/apiv2"
	"cloud.google.com/go/speech/apiv2/speechpb"
	"github.com/chromedp/chromedp"
)

// TestSpeechAdaptationThroughThePage (#1930), in the run shard: Create
// custom class and Create phrase set make the resources GetCustomClass and
// GetPhraseSet read back, items and phrases with their boosts; Edit on a
// custom class's page changes its items; Delete marks a phrase set DELETED,
// and Undelete on its row makes it ACTIVE again.
func TestSpeechAdaptationThroughThePage(t *testing.T) {
	needService(t, "speech-custom-classes")
	needService(t, "speech-phrase-sets")
	c, err := speech.NewClient(context.Background(), localAIDial(t)...)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	ctx := context.Background()
	project := uniqueProject(t)
	q := "?project=" + project
	n := time.Now().UnixNano() % 1e6
	cid, pid := fmt.Sprintf("browser-cc-%d", n), fmt.Sprintf("browser-ps-%d", n)
	cname := "projects/" + project + "/locations/global/customClasses/" + cid
	pname := "projects/" + project + "/locations/global/phraseSets/" + pid
	t.Cleanup(func() {
		_, _ = c.DeleteCustomClass(context.Background(), &speechpb.DeleteCustomClassRequest{Name: cname, AllowMissing: true})
		_, _ = c.DeletePhraseSet(context.Background(), &speechpb.DeletePhraseSetRequest{Name: pname, AllowMissing: true})
	})

	p := open(t)
	p.navigate("/speech/custom-classes/create" + q)
	p.waitFor(`document.querySelector("#f-items") !== null`)
	p.submitForm(map[string]string{"name": cid, "displayName": "Pets", "items": "cat\ndog"}, "name")
	p.waitFor(`location.pathname !== "/speech/custom-classes/create"`)
	cc, err := c.GetCustomClass(ctx, &speechpb.GetCustomClassRequest{Name: cname})
	if err != nil || cc.GetDisplayName() != "Pets" || len(cc.GetItems()) != 2 || cc.GetItems()[1].GetValue() != "dog" {
		t.Fatalf("GetCustomClass after Create custom class: %v %v", cc, err)
	}
	p.navigate("/speech/custom-classes/" + cid + q)
	p.waitFor(settled + ` && document.querySelector("#view").textContent.includes("dog")`)
	p.pageAction("Edit", map[string]string{"items": "cat\ndog\nrabbit"}, "")
	if cc, err = c.GetCustomClass(ctx, &speechpb.GetCustomClassRequest{Name: cname}); err != nil || len(cc.GetItems()) != 3 {
		t.Errorf("GetCustomClass after Edit: %v %v", cc, err)
	}

	p.navigate("/speech/phrase-sets/create" + q)
	p.waitFor(`document.querySelector("#f-phrases") !== null`)
	p.submitForm(map[string]string{"name": pid, "phrases": "CloudBurrow | 15\nSweetTech", "boost": "5"}, "name")
	p.waitFor(`location.pathname !== "/speech/phrase-sets/create"`)
	ps, err := c.GetPhraseSet(ctx, &speechpb.GetPhraseSetRequest{Name: pname})
	if err != nil || ps.GetBoost() != 5 || len(ps.GetPhrases()) != 2 || ps.GetPhrases()[0].GetValue() != "CloudBurrow" || ps.GetPhrases()[0].GetBoost() != 15 {
		t.Fatalf("GetPhraseSet after Create phrase set: %v %v", ps, err)
	}
	p.navigate("/speech/phrase-sets/" + pid + q)
	p.waitFor(settled + ` && document.querySelector("#view").textContent.includes("SweetTech")`)
	p.clickText(p.findAction("Delete"), "Delete")
	p.confirmByName(pid)
	p.waitFor(`document.querySelector(".modal") === null`)
	if ps, err = c.GetPhraseSet(ctx, &speechpb.GetPhraseSetRequest{Name: pname}); err != nil || ps.GetState() != speechpb.PhraseSet_DELETED {
		t.Fatalf("GetPhraseSet after Delete: %v %v", ps.GetState(), err)
	}
	p.navigate("/speech/phrase-sets" + q)
	p.waitFor(rowReady(pid))
	p.eval(`document.querySelector("#view tbody tr .overflow-trigger:not(:disabled)").click()`, nil)
	p.clickText("#view .overflow-item", "Undelete")
	p.waitFor(`document.querySelector(".snackbar") !== null`)
	if ps, err = c.GetPhraseSet(ctx, &speechpb.GetPhraseSetRequest{Name: pname}); err != nil || ps.GetState() != speechpb.PhraseSet_ACTIVE {
		t.Errorf("GetPhraseSet after Undelete: %v %v", ps.GetState(), err)
	}
}

// TestSpeechBatchTranscriptionThroughThePage (#1930), in the run shard:
// Transcriptions' New transcription with a Cloud storage file sends it to
// BatchRecognize. A missing object is refused with BatchRecognize's
// per-file NOT_FOUND; where the local models are available
// (CLOUDBURROW_TEST_LOCALAI), the JFK clip uploaded with the official
// Cloud Storage client is transcribed and listed.
func TestSpeechBatchTranscriptionThroughThePage(t *testing.T) {
	needService(t, "speech-transcriptions")
	sc, _ := storageSDK(t)
	c, err := speech.NewClient(context.Background(), localAIDial(t)...)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	ctx := context.Background()
	project := uniqueProject(t)
	q := "?project=" + project
	rid := fmt.Sprintf("browser-batch-%d", time.Now().UnixNano()%1e6)
	op, err := c.CreateRecognizer(ctx, &speechpb.CreateRecognizerRequest{Parent: "projects/" + project + "/locations/global",
		RecognizerId: rid, Recognizer: &speechpb.Recognizer{DefaultRecognitionConfig: &speechpb.RecognitionConfig{Model: "chirp_3",
			LanguageCodes: []string{"en-US"}, DecodingConfig: &speechpb.RecognitionConfig_AutoDecodingConfig{AutoDecodingConfig: &speechpb.AutoDetectDecodingConfig{}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = c.DeleteRecognizer(context.Background(), &speechpb.DeleteRecognizerRequest{Name: "projects/" + project + "/locations/global/recognizers/" + rid})
	})
	bucket := project + "-audio"
	bh := sc.Bucket(bucket)
	if err := bh.Create(ctx, project, nil); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	t.Cleanup(func() {
		_ = bh.Object("jfk.flac").Delete(context.Background())
		_ = bh.Delete(context.Background())
	})

	p := open(t)
	p.navigate("/speech/transcriptions" + q)
	p.waitFor(settled)
	p.openCloudStorageTranscription()
	p.setField(".modal.is-open #f-uri", "gs://"+bucket+"/missing.flac")
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	if msg := p.modalRefusal(); !strings.Contains(msg, "not found") {
		t.Errorf("a missing object said %q", msg)
	}
	p.forgive() // the refusal's 400, provoked on purpose
	p.eval(`document.querySelector(".modal.is-open .modal-actions button:not([type=submit])")?.click()`, nil)
	p.waitFor(`document.querySelector(".modal") === null`)

	if os.Getenv(envLocalAI) == "" {
		return
	}
	clip, err := os.ReadFile(filepath.Join("..", "..", "internal", "speech", "testdata", "jfk.flac"))
	if err != nil {
		t.Fatal(err)
	}
	w := bh.Object("jfk.flac").NewWriter(ctx)
	w.ContentType = "audio/flac"
	if _, err := w.Write(clip); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("upload: %v", err)
	}
	p.openCloudStorageTranscription()
	p.setField(".modal.is-open #f-uri", "gs://"+bucket+"/jfk.flac")
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(`document.querySelector("#action-result-table") !== null`)
	var rows string
	p.eval(`document.querySelector("#action-result-table").textContent`, &rows)
	p.clickText(".modal.is-open .modal-actions button", "Close")
	if !strings.Contains(strings.ToLower(rows), "your country") {
		t.Errorf("Transcribe from Cloud Storage shows %q", rows)
	}
	p.navigate("/speech/transcriptions" + q)
	p.waitFor(rowReady("gs://" + bucket + "/jfk.flac"))
}

// TestSpeechAdaptationScreensMeetWCAG (#1930): Custom classes and Phrase
// sets, their create forms and a resource's page, and Transcriptions' batch
// form, in the run shard.
func TestSpeechAdaptationScreensMeetWCAG(t *testing.T) {
	needService(t, "speech-custom-classes")
	needService(t, "speech-phrase-sets")
	c, err := speech.NewClient(context.Background(), localAIDial(t)...)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	ctx := context.Background()
	project := uniqueProject(t)
	parent := "projects/" + project + "/locations/global"
	cop, err := c.CreateCustomClass(ctx, &speechpb.CreateCustomClassRequest{Parent: parent, CustomClassId: "a11y-class",
		CustomClass: &speechpb.CustomClass{Items: []*speechpb.CustomClass_ClassItem{{Value: "burrow"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cop.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	pop, err := c.CreatePhraseSet(ctx, &speechpb.CreatePhraseSetRequest{Parent: parent, PhraseSetId: "a11y-set",
		PhraseSet: &speechpb.PhraseSet{Boost: 10, Phrases: []*speechpb.PhraseSet_Phrase{{Value: "CloudBurrow"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pop.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	ropt, err := c.CreateRecognizer(ctx, &speechpb.CreateRecognizerRequest{Parent: parent, RecognizerId: "a11y-batch",
		Recognizer: &speechpb.Recognizer{DefaultRecognitionConfig: &speechpb.RecognitionConfig{Model: "chirp", LanguageCodes: []string{"en-US"},
			DecodingConfig: &speechpb.RecognitionConfig_AutoDecodingConfig{AutoDecodingConfig: &speechpb.AutoDetectDecodingConfig{}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ropt.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = c.DeleteCustomClass(context.Background(), &speechpb.DeleteCustomClassRequest{Name: parent + "/customClasses/a11y-class"})
		_, _ = c.DeletePhraseSet(context.Background(), &speechpb.DeletePhraseSetRequest{Name: parent + "/phraseSets/a11y-set"})
		_, _ = c.DeleteRecognizer(context.Background(), &speechpb.DeleteRecognizerRequest{Name: parent + "/recognizers/a11y-batch"})
	})
	batch := screen("New transcription from Cloud storage dialog", "/speech/transcriptions", project)
	batch.ready = settled + ` && document.querySelector("#list-action-transcribe") !== null`
	batch.then = `document.querySelector("#list-action-transcribe").click()`
	pages := listAndCreate("custom classes", "/speech/custom-classes", project)
	pages = append(pages, screen("custom class", "/speech/custom-classes/a11y-class", project))
	pages = append(pages, listAndCreate("phrase sets", "/speech/phrase-sets", project)...)
	pages = append(pages, screen("phrase set", "/speech/phrase-sets/a11y-set", project), batch)
	auditScreens(t, pages)
}
