package main

// Cloud Run: Write a function's inline source editor (#1852). Google's
// Write a function opens a code editor over the function's files, starting
// from the chosen runtime's sample (index.js and package.json for Node.js,
// main.py and requirements.txt for Python, function.go and go.mod for Go),
// instead of asking for an archive. The editor's files are zipped and
// handed to Create function's own path, which uploads them as gcloud
// functions deploy does and builds them with Google's buildpacks.

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"

	runadapter "github.com/cloudburrow/cloudburrow/internal/adapter/run"
	"github.com/cloudburrow/cloudburrow/internal/console"
)

const runSourceFilesField = "source"

// Google's samples, by language.
var runFunctionSamples = map[string]struct {
	entryPoint string
	files      map[string]string
}{
	"nodejs": {"helloHttp", map[string]string{
		"index.js": `const functions = require('@google-cloud/functions-framework');

functions.http('helloHttp', (req, res) => {
  res.send(` + "`Hello ${req.query.name || req.body.name || 'World'}!`" + `);
});
`,
		"package.json": `{
  "dependencies": {
    "@google-cloud/functions-framework": "^3.0.0"
  }
}
`}},
	"python": {"hello_http", map[string]string{
		"main.py": `import functions_framework


@functions_framework.http
def hello_http(request):
    request_json = request.get_json(silent=True)
    request_args = request.args

    if request_json and "name" in request_json:
        name = request_json["name"]
    elif request_args and "name" in request_args:
        name = request_args["name"]
    else:
        name = "World"
    return f"Hello {name}!"
`,
		"requirements.txt": "functions-framework==3.*\n"}},
	"go": {"HelloHTTP", map[string]string{
		"function.go": `package helloworld

import (
	"encoding/json"
	"fmt"
	"html"
	"net/http"

	"github.com/GoogleCloudPlatform/functions-framework-go/functions"
)

func init() {
	functions.HTTP("HelloHTTP", helloHTTP)
}

func helloHTTP(w http.ResponseWriter, r *http.Request) {
	var d struct {
		Name string ` + "`json:\"name\"`" + `
	}
	if err := json.NewDecoder(r.Body).Decode(&d); err != nil || d.Name == "" {
		fmt.Fprint(w, "Hello, World!")
		return
	}
	fmt.Fprintf(w, "Hello, %s!", html.EscapeString(d.Name))
}
`,
		"go.mod": "module example.com/gcf\n\ngo 1.21\n\nrequire github.com/GoogleCloudPlatform/functions-framework-go v1.8.1\n"}},
}

// runFunctionLanguage is a runtime's language: nodejs22 is nodejs.
func runFunctionLanguage(runtime string) string {
	return strings.TrimRight(runtime, "0123456789")
}

// runFunctionTemplates are the editor's starting files, by runtime.
func runFunctionTemplates() map[string]map[string]string {
	out := map[string]map[string]string{}
	for _, r := range runadapter.Runtimes {
		if s, ok := runFunctionSamples[runFunctionLanguage(r.Name)]; ok {
			out[r.Name] = s.files
		}
	}
	return out
}

// inlineSourceFields are Create function's fields with the source editor
// in place of the .zip.
func inlineSourceFields(fields []console.Field) []console.Field {
	out := make([]console.Field, 0, len(fields))
	runtime := "nodejs22"
	for _, f := range fields {
		if f.Name == "runtime" && f.Default != "" {
			runtime = f.Default
		}
	}
	for _, f := range fields {
		switch f.Name {
		case "file":
			def, _ := json.Marshal(runFunctionTemplates()[runtime])
			f = console.Field{Name: runSourceFilesField, Label: "Source code", Type: "sourcefiles", Required: true,
				Section: "Source", Default: string(def), Follows: "runtime", Templates: runFunctionTemplates(),
				Help: "The function's files, edited inline; choosing a runtime starts from its sample until a file is " +
					"edited. They are uploaded as gcloud functions deploy uploads a source directory, and built here " +
					"with Google's buildpacks (linux/amd64 images only)."}
		case "entryPoint":
			f.Default = runFunctionSamples[runFunctionLanguage(runtime)].entryPoint
			f.Help = "The function the Functions Framework serves: the samples' are helloHttp (Node.js), " +
				"hello_http (Python) and HelloHTTP (Go)."
		}
		out = append(out, f)
	}
	return out
}

// zipSourceFiles is the editor's files as the .zip Create function takes.
func zipSourceFiles(value string) ([]byte, error) {
	var files map[string]string
	if err := json.Unmarshal([]byte(value), &files); err != nil {
		return nil, fmt.Errorf("source code: expected files and their contents: %v", err)
	}
	if len(files) == 0 {
		return nil, errors.New("source code: the function needs at least one file")
	}
	names := make([]string, 0, len(files))
	for n := range files {
		clean := path.Clean(strings.TrimSpace(n))
		if n == "" || clean != n || path.IsAbs(n) || strings.HasPrefix(clean, "../") || clean == ".." {
			return nil, fmt.Errorf("source code: %q is not a file name inside the source", n)
		}
		names = append(names, n)
	}
	sort.Strings(names)
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, n := range names {
		w, err := zw.Create(n)
		if err != nil {
			return nil, err
		}
		if _, err := w.Write([]byte(files[n])); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// writeFunctionInline creates the function from the editor's files.
func (p runProvider) writeFunctionInline(ctx context.Context, project string, values map[string]string) error {
	data, err := zipSourceFiles(values[runSourceFilesField])
	if err != nil {
		return err
	}
	_, err = p.writeFunction(ctx, project, values, console.UploadedFile{Name: "source.zip",
		ContentType: "application/zip", Reader: bytes.NewReader(data)})
	return err
}
