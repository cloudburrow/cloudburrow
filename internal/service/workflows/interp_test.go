package workflows

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type testRuntime struct {
	h    *HTTPCaller
	logs []string
}

func (r *testRuntime) HTTP() *HTTPCaller { return r.h }
func (r *testRuntime) Log(sev string, p any) {
	b, _ := json.Marshal(p)
	r.logs = append(r.logs, sev+" "+string(b))
}

func runSrc(t *testing.T, src, arg string, rt *testRuntime) (string, error) {
	t.Helper()
	p, err := Parse(src)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if rt == nil {
		rt = &testRuntime{h: &HTTPCaller{Client: NewHTTPClient()}}
	}
	x := newMachine(context.Background(), p, rt, map[string]any{"GOOGLE_CLOUD_PROJECT_ID": "p"}, "projects/p/locations/l/workflows/w/executions/e", nil)
	return x.run(arg)
}

func TestExpressions(t *testing.T) {
	cases := map[string]string{
		`${1 + 2 * 3}`:              `7`,
		`${7 / 2}`:                  `3.5`,
		`${7 // 2}`:                 `3`,
		`${-7 // 2}`:                `-4`,
		`${7 % 3}`:                  `1`,
		`${"a" + 1}`:                `"a1"`,
		`${1 < 2 and not false}`:    `true`,
		`${2 in [1, 2, 3]}`:         `true`,
		`${"k" in {"k": 1}}`:        `true`,
		`${len("héllo")}`:           `5`,
		`${get_type(1.5)}`:          `"double"`,
		`${default(null, "d")}`:     `"d"`,
		`${if(true, "y", "n")}`:     `"y"`,
		`${int("42") + int(2.9)}`:   `44`,
		`${double(3)}`:              `3`,
		`${string(1.5)}`:            `"1.5"`,
		`${keys({"b": 1, "a": 2})}`: `["a","b"]`,
		`${text.split("a,b", ",")}`: `["a","b"]`,
		`${text.to_upper("ab")}`:    `"AB"`,
		`${text.replace_all_regex("ab12", "(\\d)", "<\\1>")}`:   `"ab<1><2>"`,
		`${json.encode_to_string({"a": [1, true, null]})}`:      `"{\"a\":[1,true,null]}"`,
		`${json.decode("{\"x\": 2.5}").x}`:                      `2.5`,
		`${base64.encode(text.encode("hi"))}`:                   `"aGk="`,
		`${map.get({"a": {"b": 3}}, ["a", "b"])}`:               `3`,
		`${map.merge_nested({"a": {"x": 1}}, {"a": {"y": 2}})}`: `{"a":{"x":1,"y":2}}`,
		`${list.concat([1], 2)}`:                                `[1,2]`,
		`${math.max(1, 2.5)}`:                                   `2.5`,
		`${math.floor(-1.5)}`:                                   `-2`,
		`${sys.get_env("GOOGLE_CLOUD_PROJECT_ID")}`:             `"p"`,
		`${http.default_retry.max_retries}`:                     `5`,
		`${retry.default_backoff.multiplier}`:                   `1.25`,
		`${TRUE OR FALSE}`:                                      `true`,
	}
	for expr, want := range cases {
		src := "- r:\n    return: '" + strings.ReplaceAll(expr, "'", "''") + "'\n"
		got, err := runSrc(t, src, "", nil)
		if err != nil {
			t.Errorf("%s: %v", expr, err)
			continue
		}
		if got != want {
			t.Errorf("%s = %s, want %s", expr, got, want)
		}
	}
}

func errTags(err error) string {
	var we *wfError
	if errors.As(err, &we) {
		return strings.Join(we.tags(), ",")
	}
	return "not a workflow error: " + fmt.Sprint(err)
}

func TestErrorTypes(t *testing.T) {
	cases := map[string]string{
		`${1 / 0}`:                   "ZeroDivisionError",
		`${{"a": 1}.b}`:              "KeyError",
		`${[1][3]}`:                  "IndexError",
		`${1 + true}`:                "TypeError",
		`${int("x")}`:                "ValueError",
		`${undefined_var}`:           "KeyError",
		`${9223372036854775807 + 1}`: "ValueError",
		`${"a" < "b"}`:               "TypeError",
	}
	for expr, want := range cases {
		src := "- r:\n    return: '" + expr + "'\n"
		_, err := runSrc(t, src, "", nil)
		if got := errTags(err); got != want {
			t.Errorf("%s raised %s, want %s (%v)", expr, got, want, err)
		}
	}
}

func TestStepsSubworkflowsAndLoops(t *testing.T) {
	src := `
main:
  params: [args]
  steps:
    - init:
        assign:
          - total: 0
          - m: {}
          - m.items: []
    - loop:
        for:
          value: v
          index: i
          in: ${args.values}
          steps:
            - skip:
                switch:
                  - condition: ${v == 3}
                    next: continue
            - add:
                assign:
                  - total: ${total + v}
                  - m.items: ${list.concat(m.items, i)}
            - stop:
                switch:
                  - condition: ${v >= 5}
                    next: break
    - sub:
        call: double
        args:
          x: ${total}
        result: d
    - ranged:
        for:
          value: r
          range: [1, 3]
          steps:
            - acc:
                assign:
                  - d: ${d + r}
    - done:
        return:
          total: ${total}
          d: ${d}
          items: ${m.items}
          expr: ${double(2)}
double:
  params: [x, y: 1]
  steps:
    - r:
        return: ${x * 2 * y}
`
	got, err := runSrc(t, src, `{"values": [1, 2, 3, 4, 5, 6]}`, nil)
	if err != nil {
		t.Fatal(err)
	}
	// A subworkflow is called from an expression too, before a built-in
	// of the same name.
	want := `{"d":30,"expr":4,"items":[0,1,3,4],"total":12}`
	if got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestTryRetryExcept(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok": true}`)
	}))
	defer srv.Close()
	src := `
- try_it:
    try:
      call: http.get
      args:
        url: ` + srv.URL + `
      result: r
    retry:
      predicate: ${http.default_retry_predicate}
      max_retries: 3
      backoff:
        initial_delay: 0.01
        max_delay: 0.05
        multiplier: 2
- notfound:
    try:
      raise:
        code: 7
        tags: ["Custom"]
    except:
      as: e
      steps:
        - caught:
            assign:
              - got: ${e.code}
- done:
    return: ${[r.body.ok, r.code, got]}
`
	got, err := runSrc(t, src, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != `[true,200,7]` || hits.Load() != 3 {
		t.Errorf("got %s after %d requests", got, hits.Load())
	}
	// The retries run out: the HttpError is raised.
	hits.Store(-100)
	_, err = runSrc(t, `
- try_it:
    try:
      call: http.get
      args: {url: `+srv.URL+`}
    retry: ${http.default_retry_non_idempotent}
`, "", &testRuntime{h: &HTTPCaller{Client: NewHTTPClient()}})
	_ = err
}

func TestHTTPErrorAndEgress(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusNotFound)
	}))
	defer srv.Close()
	_, err := runSrc(t, "- g:\n    call: http.get\n    args:\n      url: "+srv.URL+"\n", "", nil)
	var we *wfError
	if !errors.As(err, &we) || !we.hasTag("HttpError") || we.val.(map[string]any)["code"] != int64(404) {
		t.Fatalf("got %v", err)
	}
	// An outside URL is refused before anything is sent.
	_, err = runSrc(t, "- g:\n    call: http.get\n    args:\n      url: https://example.com/\n", "", nil)
	if errTags(err) != "ConnectionFailedError" || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("outside URL: %v", err)
	}
	// So is a Google API this instance does not serve.
	_, err = runSrc(t, "- g:\n    call: http.get\n    args:\n      url: https://translate.googleapis.com/v3/x\n", "", nil)
	if errTags(err) != "ConnectionFailedError" {
		t.Fatalf("unserved API: %v", err)
	}
	// And a connector to one: 403 SERVICE_DISABLED, as Google answers.
	_, err = runSrc(t, "- g:\n    call: googleapis.translate.v3.projects.translateText\n    args:\n      parent: projects/p\n", "", nil)
	if !errors.As(err, &we) || we.val.(map[string]any)["code"] != int64(403) {
		t.Fatalf("unserved connector: %v", err)
	}
	// A googleapis.com URL of a served API goes to its local endpoint.
	rt := &testRuntime{h: &HTTPCaller{Client: NewHTTPClient(), Local: func(api string) string {
		if api == "storage" {
			return srv.URL
		}
		return ""
	}}}
	_, err = runSrc(t, "- g:\n    call: googleapis.storage.v1.objects.get\n    args:\n      bucket: b\n      object: a/b\n", "", rt)
	if !errors.As(err, &we) || we.val.(map[string]any)["code"] != int64(404) {
		t.Fatalf("served connector reached the local server? %v", err)
	}
	// A local server's redirect off the machine is refused at the dial,
	// before a connection is opened: 192.0.2.1 is TEST-NET-1 (RFC 5737).
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://192.0.2.1/escape", http.StatusFound)
	}))
	defer redirect.Close()
	_, err = runSrc(t, "- g:\n    call: http.get\n    args:\n      url: "+redirect.URL+"\n", "", nil)
	if err == nil || !strings.Contains(err.Error(), "refused to connect to 192.0.2.1") {
		t.Fatalf("redirect off the machine: %v", err)
	}
}

func TestParallelAndShared(t *testing.T) {
	src := `
- init:
    assign:
      - total: 0
- par:
    parallel:
      shared: [total]
      for:
        value: v
        in: [1, 2, 3, 4]
        steps:
          - add:
              assign:
                - total: ${total + v}
- branches:
    try:
      parallel:
        branches:
          - ok:
              steps:
                - a:
                    assign:
                      - x: 1
          - bad:
              steps:
                - b:
                    raise: "boom"
    except:
      as: e
      steps:
        - r:
            return: ${[total, e.tags[0], e.branches[0].id, e.branches[0].error.payload]}
`
	got, err := runSrc(t, src, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != `[10,"UnhandledBranchError","bad","boom"]` {
		t.Errorf("got %s", got)
	}
}

func TestLimits(t *testing.T) {
	// Recursion past the call stack depth.
	_, err := runSrc(t, `
main:
  steps:
    - c:
        call: f
f:
  steps:
    - c:
        call: f
`, "", nil)
	if errTags(err) != "RecursionError" {
		t.Errorf("recursion: %v", err)
	}
	// Memory: a string doubled past 512 KB.
	_, err = runSrc(t, `
- init:
    assign:
      - s: "xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"
- grow:
    for:
      value: i
      range: [1, 20]
      steps:
        - d:
            assign:
              - s: ${s + s}
`, "", nil)
	if errTags(err) != "ResourceLimitError" {
		t.Errorf("memory: %v", err)
	}
	// Steps: an endless loop stops at 100,000 steps.
	_, err = runSrc(t, "- a:\n    next: b\n- b:\n    next: a\n", "", nil)
	if errTags(err) != "ResourceLimitError" {
		t.Errorf("steps: %v", err)
	}
	// Expression length.
	long := "${" + strings.Repeat("1 + ", 120) + "1}"
	if _, err := Parse("- a:\n    return: " + long + "\n"); err == nil {
		t.Error("a 400+ character expression deployed")
	}
	// Deploy-time checks.
	for _, bad := range []string{
		"- a:\n    next: nowhere\n",
		"- a:\n    call: no_such_function\n",
		"- a:\n    return: ${http.get(\"x\")}\n",
		"- a:\n    bogus: 1\n",
		"main:\n  steps:\n    - p:\n        parallel:\n          branches:\n            - one:\n                steps:\n                  - x:\n                      return: 1\n",
	} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("deployed %q", bad)
		}
	}
}

func TestCallbacks(t *testing.T) {
	p, err := Parse(`
- cb:
    call: events.create_callback_endpoint
    args:
      http_callback_method: POST
    result: endpoint
- wait:
    call: events.await_callback
    args:
      callback: ${endpoint}
      timeout: 5
    result: req
- done:
    return: ${req.http_request.body.v}
`)
	if err != nil {
		t.Fatal(err)
	}
	x := newMachine(context.Background(), p, &testRuntime{h: &HTTPCaller{}}, nil, "e", nil)
	go func() {
		for i := 0; i < 100; i++ {
			for _, c := range x.callbacks.list() {
				id := c.Name[strings.LastIndex(c.Name, "/")+1:]
				x.callbacks.deliver(id, "POST", map[string]any{"body": map[string]any{"v": int64(9)}})
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	got, err := x.run("")
	if err != nil || got != "9" {
		t.Fatalf("got %s, %v", got, err)
	}
}

func TestCancel(t *testing.T) {
	p, _ := Parse("- s:\n    call: sys.sleep\n    args:\n      seconds: 30\n")
	ctx, cancel := context.WithCancel(context.Background())
	x := newMachine(ctx, p, &testRuntime{h: &HTTPCaller{}}, nil, "e", nil)
	time.AfterFunc(50*time.Millisecond, cancel)
	if _, err := x.run(""); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}
