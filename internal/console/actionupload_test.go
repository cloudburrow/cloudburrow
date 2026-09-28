package console

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"
)

// fileActorProvider offers "loadfile", which takes a file, and "plain",
// which does not, on any path; it keeps what each file action was given.
type fileActorProvider struct {
	fakeProvider
	got *[]string
}

func (p fileActorProvider) DetailActions(context.Context, string, []string) []Action {
	return []Action{
		{ID: "loadfile", Label: "Load from a file", Fields: []Field{
			{Name: "file", Label: "File", Type: FileFieldType, Required: true},
			{Name: "format", Label: "Format", Type: "text"},
		}},
		{ID: "plain", Label: "Plain", Fields: []Field{{Name: "x", Label: "X", Type: "text"}}},
	}
}

func (p fileActorProvider) ActAt(context.Context, string, []string, string, map[string]string) error {
	return nil
}

func (p fileActorProvider) ActAtFile(ctx context.Context, project string, path []string, action string, values map[string]string, f UploadedFile) (*Listing, error) {
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	if string(data) == "refuse" {
		return nil, errors.New("the API refused it")
	}
	*p.got = append(*p.got, strings.Join([]string{project, strings.Join(path, "/"), action, values["format"], f.Name, f.ContentType, string(data)}, "|"))
	return &Listing{NameColumn: "Job", Items: []Resource{{Name: "job-1"}}, Total: 1}, nil
}

// actionUpload posts the request part, then (unless file is nil) the file
// part, as the console's form does.
func actionUpload(t *testing.T, url string, request any, filename string, file []byte) (int, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if request != nil {
		w, _ := mw.CreateFormField("request")
		if s, ok := request.(string); ok {
			_, _ = io.WriteString(w, s)
		} else {
			_ = json.NewEncoder(w).Encode(request)
		}
	}
	if file != nil {
		part, _ := mw.CreatePart(map[string][]string{
			"Content-Disposition": {`form-data; name="file"; filename="` + filename + `"`},
			"Content-Type":        {"text/csv"},
		})
		_, _ = part.Write(file)
	}
	_ = mw.Close()
	resp, err := http.Post(url, mw.FormDataContentType(), &buf)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// TestActionUploadStreamsTheFileToAnOfferedAction (#999): an action whose
// form holds a file is posted with it to /api/actions/{service}/upload. The
// provider gets the path, the action's values and the file, named by its
// last segment only, and its rows are the answer; the action is in the
// operations ledger. An action the page does not offer, one that takes no
// file, a body with no request part or no file, a provider with no file
// actions and a refusal are each refused with why, and nothing is recorded
// as done.
func TestActionUploadStreamsTheFileToAnOfferedAction(t *testing.T) {
	var got []string
	p := fileActorProvider{fakeProvider: fakeProvider{id: "bigquery", title: "BigQuery"}, got: &got}
	srv := serve(t, p, fakeProvider{id: "plain", title: "Plain"})
	u := srv.URL + "/api/actions/bigquery/upload?project=p"
	req := map[string]any{"Path": []string{"ds", "t"}, "Action": "loadfile", "Values": map[string]string{"format": "CSV"}}

	code, body := actionUpload(t, u, req, `..\..\people.csv`, []byte("id\n1\n"))
	if code != http.StatusOK || !strings.Contains(body, `"result"`) || !strings.Contains(body, "job-1") {
		t.Fatalf("the upload = %d: %s", code, body)
	}
	if want := "p|ds/t|loadfile|CSV|people.csv|text/csv|id\n1\n"; len(got) != 1 || got[0] != want {
		t.Errorf("the provider got %q, want %q", got, want)
	}
	_, ops := get(t, srv, "/api/operations?project=p", nil)
	if !strings.Contains(ops, `"kind":"loadfile"`) || !strings.Contains(ops, "ds/t") {
		t.Errorf("the action is not in the ledger: %s", ops)
	}

	for _, c := range []struct {
		name    string
		url     string
		request any
		file    []byte
		code    int
		why     string
	}{
		{"not offered", u, map[string]any{"Path": []string{"ds"}, "Action": "dropall"}, []byte("x"), http.StatusBadRequest, "not available"},
		{"no file field", u, map[string]any{"Path": []string{"ds"}, "Action": "plain"}, []byte("x"), http.StatusBadRequest, "takes no file"},
		{"no request", u, nil, []byte("x"), http.StatusBadRequest, "first part is the action's request"},
		{"bad request", u, `{"Path":["ds"],"Action":"loadfile","Extra":1}`, []byte("x"), http.StatusBadRequest, "unknown field"},
		{"no file", u, req, nil, http.StatusBadRequest, "no file part"},
		{"refused", u, req, []byte("refuse"), http.StatusBadRequest, "the API refused it"},
		{"no file actions", srv.URL + "/api/actions/plain/upload?project=p", req, []byte("x"), http.StatusNotImplemented, "no actions that take a file"},
		{"no service", srv.URL + "/api/actions/nope/upload?project=p", req, []byte("x"), http.StatusNotFound, "no such service"},
	} {
		code, body := actionUpload(t, c.url, c.request, "a.csv", c.file)
		if code != c.code || !strings.Contains(body, c.why) {
			t.Errorf("%s: %d %s, want %d naming %q", c.name, code, body, c.code, c.why)
		}
	}
	if len(got) != 1 {
		t.Errorf("a refused upload reached the provider: %q", got)
	}
}

// TestActionUploadIsHeldToTheUploadLimit (#999): a file larger than the
// upload limit in Settings is 413 naming the limit, whether the request
// declares its size or the count of the stream finds it, and the provider
// never completes it.
func TestActionUploadIsHeldToTheUploadLimit(t *testing.T) {
	var got []string
	p := fileActorProvider{fakeProvider: fakeProvider{id: "bigquery", title: "BigQuery"}, got: &got}
	srv := serve(t, p)
	if code, body := sendBody(t, srv, http.MethodPut, "/api/settings", `{"uploadLimitBytes":1024}`); code != http.StatusOK {
		t.Fatalf("settings = %d: %s", code, body)
	}
	u := srv.URL + "/api/actions/bigquery/upload?project=p"
	req := map[string]any{"Path": []string{"ds"}, "Action": "loadfile", "Values": map[string]string{}}
	for _, size := range []int{2048, 300 << 10} {
		code, body := actionUpload(t, u, req, "big.csv", bytes.Repeat([]byte("x"), size))
		if code != http.StatusRequestEntityTooLarge || !strings.Contains(body, "upload limit of 1.0 KiB") {
			t.Errorf("a %d-byte file = %d: %s", size, code, body)
		}
	}
	if len(got) != 0 {
		t.Errorf("an over-limit file was loaded: %q", got)
	}
}

// TestActionUploadRefusesACrossSitePost (#999): the route is behind the
// same guard as every /api/ route, so a page on another site cannot post a
// file to it.
func TestActionUploadRefusesACrossSitePost(t *testing.T) {
	var got []string
	srv := serve(t, fileActorProvider{fakeProvider: fakeProvider{id: "bigquery", title: "BigQuery"}, got: &got})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/actions/bigquery/upload?project=p", strings.NewReader("x"))
	req.Header.Set("Content-Type", "multipart/form-data; boundary=x")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || len(got) != 0 {
		t.Errorf("a cross-site post = %d, loaded %q", resp.StatusCode, got)
	}
}
