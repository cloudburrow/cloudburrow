package sparkproc

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fakeEngine struct {
	got Job
	res Result
	err error
}

func (f *fakeEngine) Run(_ context.Context, job Job) (Result, error) { f.got = job; return f.res, f.err }

// TestClientServer: a job and its result cross the runner's HTTP API; a
// runner that cannot run Spark answers an *Unavailable.
func TestClientServer(t *testing.T) {
	e := &fakeEngine{res: Result{Out: map[string]string{"n": "3"}}}
	srv := httptest.NewServer(NewServer(e))
	defer srv.Close()
	c := NewClient(srv.URL, nil)
	res, err := c.Run(context.Background(), Job{SparkJobID: "j", Code: "print(1)", Params: map[string]string{"x": `"a"`},
		PyFiles: []File{{URI: "gs://b/h.py", Data: []byte("x = 1")}}})
	if err != nil || res.Out["n"] != "3" || e.got.Code != "print(1)" || string(e.got.PyFiles[0].Data) != "x = 1" {
		t.Fatalf("run: %+v %v (engine saw %+v)", res, err, e.got)
	}
	e.err = &Unavailable{Msg: "no docker"}
	_, err = c.Run(context.Background(), Job{})
	var u *Unavailable
	if !errors.As(err, &u) || u.Msg != "no docker" {
		t.Errorf("unavailable: %v", err)
	}
	if _, err := NewClient("", nil).Run(context.Background(), Job{}); !errors.As(err, &u) {
		t.Errorf("no address: %v", err)
	}
}

// TestWriteJobAndSubmitArgs: the job's files are written where
// spark-submit is told they are, the connector's endpoints are the relay's
// names, and properties are passed as --conf.
func TestWriteJobAndSubmitArgs(t *testing.T) {
	dir := t.TempDir()
	job := Job{SparkJobID: "bqsp-1", Project: "p", Language: "PYTHON", Main: &File{URI: "gs://b/dir/main.py", Data: []byte("main")},
		PyFiles: []File{{URI: "gs://b/h.py", Data: []byte("h")}}, Files: []File{{URI: "gs://b/f.txt", Data: []byte("f")}},
		Properties: map[string]string{"spark.cb.marker": "m1"}}
	a, err := writeJob(dir, job)
	if err != nil {
		t.Fatal(err)
	}
	if a.main != "/cb/main/main.py" || len(a.pyFiles) != 1 || a.pyFiles[0] != "/cb/py/0/h.py" || a.files[0] != "/cb/files/0/f.txt" {
		t.Errorf("args %+v", a)
	}
	for _, rel := range []string{"main/main.py", "py/0/h.py", "lib/bigquery/spark/procedure.py", "relay.py", "run.sh"} {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(rel))); err != nil {
			t.Errorf("%s: %v", rel, err)
		}
	}
	args := strings.Join(submitArgs(job, a), " ")
	for _, want := range []string{"--master local[*]", "--py-files /cb/py/0/h.py", "--conf spark.cb.marker=m1",
		"bigQueryStorageGrpcEndpoint=bigquerystorage.cloudburrow.internal:8443", "bigQueryHttpEndpoint=http://host.docker.internal:8080",
		"--jars /cb-connector/spark-bigquery.jar", "/cb/main/main.py"} {
		if !strings.Contains(args, want) {
			t.Errorf("spark-submit %s: no %q", args, want)
		}
	}
	jar := Job{SparkJobID: "j", Language: "JAVA", MainClass: "com.example.Main", Jars: []File{{URI: "gs://b/app.jar", Data: []byte("j")}}}
	a, _ = writeJob(t.TempDir(), jar)
	if args := submitArgs(jar, a); args[len(args)-1] != "/cb/jars/0/app.jar" || !strings.Contains(strings.Join(args, " "), "--class com.example.Main") {
		t.Errorf("java: %v", args)
	}
	if err := writeTLS(filepath.Join(dir, "tls")); err != nil {
		t.Fatal(err)
	}
}

// TestFailure: the error of a failed application is its traceback's last
// line, or the Java exception py4j carries.
func TestFailure(t *testing.T) {
	py := []string{"Traceback (most recent call last):", `  File "/cb/main/main.py", line 1, in <module>`, "ValueError: kaboom",
		"26/11/21 09:12:01 INFO ShutdownHookManager: Shutdown hook called"}
	if got := failure(py); got != "ValueError: kaboom" {
		t.Errorf("python: %q", got)
	}
	java := []string{"py4j.protocol.Py4JJavaError: An error occurred while calling o34.load.", ": com.example.BigQueryException: 404 Not Found",
		"\tat com.example.X(X.java:1)"}
	if got := failure(java); got != "com.example.BigQueryException: 404 Not Found" {
		t.Errorf("py4j: %q", got)
	}
	if got := failure(nil); got == "" {
		t.Error("no lines: empty")
	}
}
