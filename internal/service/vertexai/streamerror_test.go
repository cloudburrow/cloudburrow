package vertexai

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"google.golang.org/genai"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// lastEvent returns the data of the final server-sent event in body.
func lastEvent(t *testing.T, body string) []byte {
	t.Helper()
	var last []byte
	sc := bufio.NewScanner(strings.NewReader(body))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		if d, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
			last = []byte(d)
		}
	}
	if last == nil {
		t.Fatalf("no event in %q", body)
	}
	return last
}

// failingGenerator sends one chunk and then fails, so the failure arrives
// after the 200 and can only be an event.
func failingGenerator() *ScriptedGenerator {
	return &ScriptedGenerator{ID: testModel, Chunks: []string{"partial"}, Err: errors.New("runtime died mid-generation")}
}

// checkStatusEvent decodes a stream's error event strictly as Google's JSON
// mapping of google.rpc.Status, which is the body apierror.WriteJSON sends,
// and checks it carries the canonical code's name: INTERNAL, not Go's
// Internal (#1471).
func checkStatusEvent(t *testing.T, data []byte, wantMsg string) {
	t.Helper()
	var got struct {
		Error struct {
			Code    int               `json:"code"`
			Message string            `json:"message"`
			Status  string            `json:"status"`
			Details []json.RawMessage `json:"details"`
		} `json:"error"`
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&got); err != nil {
		t.Fatalf("event %s is not Google's status mapping: %v", data, err)
	}
	if got.Error.Code != 500 || got.Error.Status != "INTERNAL" || !strings.Contains(got.Error.Message, wantMsg) {
		t.Errorf("event = %s; want code 500, status INTERNAL and %q", data, wantMsg)
	}
	if want := apierror.MarshalJSON(apierror.Internal(nil, "%s", got.Error.Message)); !bytes.Equal(data, want) {
		t.Errorf("event = %s; want WriteJSON's body %s", data, want)
	}
}

func TestStreamGenerateContentFailureEventHasCanonicalStatus(t *testing.T) {
	srv := newTestServer(t, failingGenerator())

	resp, body := post(t, srv, vertexPath(testModel, "streamGenerateContent")+"?alt=sse", helloBody)
	if resp.StatusCode != 200 || !strings.Contains(body, "partial") {
		t.Fatalf("status = %d, want 200 and the first chunk: %s", resp.StatusCode, body)
	}
	checkStatusEvent(t, lastEvent(t, body), "runtime died mid-generation")
}

// The event decodes into the genai SDK's own APIError, whose Status is what a
// caller maps back to a code: it must be a google.rpc.Code name. (genai Go
// v1.73 yields a data: event that carries an error as an empty response rather
// than an error, so the test decodes the event as the SDK's responseWithError
// does for a bare error line.)
func TestStreamFailureEventDecodesAsTheSDKsAPIError(t *testing.T) {
	srv := newTestServer(t, failingGenerator())
	_, body := post(t, srv, vertexPath(testModel, "streamGenerateContent")+"?alt=sse", helloBody)
	var got struct {
		Error *genai.APIError `json:"error"`
	}
	if err := json.Unmarshal(lastEvent(t, body), &got); err != nil || got.Error == nil {
		t.Fatalf("event %s: %v", lastEvent(t, body), err)
	}
	if got.Error.Code != 500 || got.Error.Status != "INTERNAL" || !strings.Contains(got.Error.Message, "runtime died") {
		t.Errorf("APIError = %+v; want 500 INTERNAL naming the failure", *got.Error)
	}
}

func TestStreamRawPredictFailureEventHasCanonicalStatus(t *testing.T) {
	srv := newTestServer(t, failingGenerator())

	resp, body := post(t, srv, vertexPath(testModel, "streamRawPredict"), helloBody)
	if resp.StatusCode != 200 || !strings.Contains(body, "partial") {
		t.Fatalf("status = %d, want 200 and the first chunk: %s", resp.StatusCode, body)
	}
	checkStatusEvent(t, lastEvent(t, body), "runtime died mid-generation")
}
