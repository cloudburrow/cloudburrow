package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// TestBigQueryLoadFromAFileOffersLoadFromCloudStoragesOptions (#999): Load
// from a file is on a dataset's and a table's page, and its form is Load
// from Cloud Storage's with a required file control where the URIs were.
func TestBigQueryLoadFromAFileOffersLoadFromCloudStoragesOptions(t *testing.T) {
	for _, withTable := range []bool{true, false} {
		gcs, file := bigqueryLoadFields(withTable), bigqueryLoadFileFields(withTable)
		if len(gcs) != len(file) {
			t.Fatalf("Load from a file has %d fields, Load from Cloud Storage %d", len(file), len(gcs))
		}
		for i := range gcs {
			if gcs[i].Name == "uris" {
				if f := file[i]; f.Name != "file" || f.Type != console.FileFieldType || !f.Required {
					t.Errorf("the URIs' place holds %+v, want a required file control", f)
				}
				continue
			}
			if !reflect.DeepEqual(gcs[i], file[i]) {
				t.Errorf("field %d is %+v on Load from a file and %+v on Load from Cloud Storage", i, file[i], gcs[i])
			}
		}
	}
	found := false
	for _, a := range (bigqueryProvider{project: "p"}).DetailActions(context.Background(), "p", []string{"ds"}) {
		found = found || (a.ID == actLoadFile && a.Label == "Load from a file")
	}
	if !found {
		t.Errorf("a dataset's page does not offer Load from a file")
	}
}

// TestBigQueryLoadFromAFileSendsTheFileAsTheLoadsData (#999): the file is
// the load's media, sent by the official client with the form's options in
// the job's configuration: a small file as one multipart upload. The job's
// answer is the result row, linked to its page. A table ID is required on a
// dataset's page, and a CSV option on a JSON load is refused before
// anything is sent; the JSON action route refuses Load from a file, whose
// file it cannot carry. A file past one upload chunk is sent as a resumable
// upload, one chunk at a time.
func TestBigQueryLoadFromAFileSendsTheFileAsTheLoadsData(t *testing.T) {
	var mu sync.Mutex
	var uploads []string
	var config map[string]any
	var received int64
	var chunks int
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const done = `{"jobReference":{"projectId":"p","jobId":"job-1","location":"US"},
			"status":{"state":"DONE"},"statistics":{"load":{"outputRows":"2","inputFiles":"1"}}}`
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/jobs") && r.URL.Query().Get("uploadType") == "resumable":
			mu.Lock()
			uploads = append(uploads, "resumable")
			mu.Unlock()
			w.Header().Set("Location", "http://"+r.Host+"/session")
		case r.URL.Path == "/session":
			n, _ := io.Copy(io.Discard, r.Body)
			mu.Lock()
			received += n
			chunks++
			total := received
			mu.Unlock()
			if strings.HasSuffix(r.Header.Get("Content-Range"), "/*") {
				// As Google's upload servers answer a client that sends
				// X-GUploader-No-308, which the Go client does.
				w.Header().Set("Range", fmt.Sprintf("bytes=0-%d", total-1))
				w.Header().Set("X-Http-Status-Code-Override", "308")
				return
			}
			_, _ = io.WriteString(w, done)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/jobs"):
			_, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
			mr := multipart.NewReader(r.Body, params["boundary"])
			meta, _ := mr.NextPart()
			var job struct {
				Configuration map[string]any `json:"configuration"`
			}
			_ = json.NewDecoder(meta).Decode(&job)
			media, _ := mr.NextPart()
			data, _ := io.ReadAll(media)
			mu.Lock()
			uploads = append(uploads, r.URL.Query().Get("uploadType")+" "+string(data))
			config = job.Configuration
			mu.Unlock()
			_, _ = io.WriteString(w, done)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/jobs/job-1"):
			_, _ = io.WriteString(w, done)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"code":404,"message":"not here"}}`)
		}
	}))
	defer api.Close()
	p := bigqueryProvider{endpoint: strings.TrimPrefix(api.URL, "http://"), project: "p"}
	ctx := context.Background()
	file := func(data string) console.UploadedFile {
		return console.UploadedFile{Name: "people.csv", ContentType: "text/csv", Reader: strings.NewReader(data)}
	}

	res, err := p.ActAtFile(ctx, "p", []string{"ds"}, actLoadFile, map[string]string{
		"tableId": "people", "format": "CSV", "skipLeadingRows": "1", "writeDisposition": "WRITE_TRUNCATE",
	}, file("id,name\n1,ann\n2,bob\n"))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"multipart id,name\n1,ann\n2,bob\n"}; !reflect.DeepEqual(uploads, want) {
		t.Errorf("the client sent %q, want %q", uploads, want)
	}
	load, _ := config["load"].(map[string]any)
	dest, _ := load["destinationTable"].(map[string]any)
	if load["sourceFormat"] != "CSV" || load["skipLeadingRows"] != float64(1) || load["writeDisposition"] != "WRITE_TRUNCATE" ||
		dest["datasetId"] != "ds" || dest["tableId"] != "people" || load["sourceUris"] != nil {
		t.Errorf("the job's configuration is %v", config)
	}
	if len(res.Items) != 1 || res.Items[0].Name != "job-1" || res.Items[0].Fields["Output rows"] != "2" ||
		res.Items[0].Link != "/bigquery-jobs/job-1?project=p" {
		t.Errorf("the result is %+v", res)
	}

	// Past one chunk: a resumable upload, in chunks of bigqueryUploadChunk.
	uploads = nil
	big := strings.Repeat("9,x\n", (bigqueryUploadChunk+bigqueryUploadChunk/2)/4)
	if _, err := p.ActAtFile(ctx, "p", []string{"ds", "t"}, actLoadFile, map[string]string{"format": "CSV"}, file(big)); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(uploads, []string{"resumable"}) || received != int64(len(big)) || chunks != 2 {
		t.Errorf("a %d-byte file was sent as %q, %d bytes in %d chunks; want one resumable upload of it in 2 chunks",
			len(big), uploads, received, chunks)
	}

	uploads = nil
	for _, c := range []struct {
		path   []string
		values map[string]string
		why    string
	}{
		{[]string{"ds"}, map[string]string{"format": "CSV"}, "table ID is required"},
		{[]string{"ds", "t"}, map[string]string{"format": "NEWLINE_DELIMITED_JSON", "fieldDelimiter": "|"}, "applies to CSV only"},
		{[]string{"ds", "t"}, map[string]string{"writeDisposition": "WRITE_EMPTY"}, "WRITE_APPEND or WRITE_TRUNCATE"},
	} {
		if _, err := p.ActAtFile(ctx, "p", c.path, actLoadFile, c.values, file("x")); err == nil || !strings.Contains(err.Error(), c.why) {
			t.Errorf("%v %v = %v, want %q", c.path, c.values, err, c.why)
		}
	}
	if _, err := p.ActAtFile(ctx, "other", []string{"ds", "t"}, actLoadFile, nil, file("x")); err == nil {
		t.Error("a load into another project was accepted")
	}
	if len(uploads) != 0 {
		t.Errorf("a refused load sent %q", uploads)
	}
	if _, err := p.ActAtResult(ctx, "p", []string{"ds", "t"}, actLoadFile, nil); err == nil || !strings.Contains(err.Error(), "upload") {
		t.Errorf("Load from a file through the JSON action route = %v", err)
	}
}
