package pubsubfront

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/protobuf/types/known/durationpb"
)

// #926: an answer over the size the front holds whole reaches the client. A
// list of subscriptions is rewritten as it streams, each naming its real
// push endpoint and its kept policy, with every other field as it came; a
// single subscription that large is passed on as it came.
func TestRESTOversizedAnswersPassThrough(t *testing.T) {
	up := newRESTUpstream(t)
	fx := newFixtureOpts(t, true, up.addr())
	real := "https://example.test/push"
	pad := strings.Repeat("x", 1000)
	const n = 5000
	var list bytes.Buffer
	list.WriteString(`{"subscriptions": [`)
	for i := range n {
		if i > 0 {
			list.WriteString(",")
		}
		name := subName(fmt.Sprintf("s%d", i))
		fmt.Fprintf(&list, `{"name":%q,"topic":"projects/p/topics/t","labels":{"pad":%q},"pushConfig":{"pushEndpoint":%q}}`,
			name, pad, fx.front.relayEndpoint(name, real))
	}
	list.WriteString(`], "nextPageToken": "next"}`)
	if list.Len() <= maxRESTBody {
		t.Fatalf("the list is %d bytes, not over %d", list.Len(), maxRESTBody)
	}
	up.answer(http.MethodGet, "/v1/projects/"+project+"/subscriptions", list.String())
	fx.front.setPolicy(subName("s4999"), &pubsubpb.ExpirationPolicy{Ttl: durationpb.New(DefaultTTL * 2)})

	resp, err := http.Get("http://" + fx.addr + "/v1/projects/" + project + "/subscriptions")
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET the list = %d, %v", resp.StatusCode, err)
	}
	var got struct {
		Subscriptions []map[string]any `json:"subscriptions"`
		NextPageToken string           `json:"nextPageToken"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("the list read back is not JSON: %v", err)
	}
	if len(got.Subscriptions) != n || got.NextPageToken != "next" {
		t.Fatalf("the list read back has %d subscriptions and token %q", len(got.Subscriptions), got.NextPageToken)
	}
	for _, i := range []int{0, n / 2, n - 1} {
		s := got.Subscriptions[i]
		if answeredEndpoint(s) != real || s["labels"].(map[string]any)["pad"] != pad {
			t.Errorf("subscription %d reads %v", i, s["pushConfig"])
		}
	}
	if p, _ := got.Subscriptions[n-1]["expirationPolicy"].(map[string]any); p["ttl"] != "5356800s" {
		t.Errorf("the last subscription's policy reads %v, want the kept one", p)
	}

	one := fmt.Sprintf(`{"name":%q,"labels":{"pad":%q}}`, subName("big"), strings.Repeat("y", maxRESTBody))
	up.answer(http.MethodGet, "/v1/projects/"+project+"/subscriptions/big", one)
	resp, err = http.Get("http://" + fx.addr + "/v1/projects/" + project + "/subscriptions/big")
	if err != nil {
		t.Fatal(err)
	}
	b, err = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK || string(b) != one {
		t.Errorf("GET an oversized subscription = %d, %d bytes, %v; want it as the emulator sent it", resp.StatusCode, len(b), err)
	}

	// An oversized answer that is not a list ends in a failed read, never a
	// short answer that looks whole.
	up.answer(http.MethodGet, "/v1/projects/"+project+"/subscriptions", `["`+strings.Repeat("z", maxRESTBody)+`"]`)
	resp, err = http.Get("http://" + fx.addr + "/v1/projects/" + project + "/subscriptions")
	if err == nil {
		_, err = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
	}
	if err == nil {
		t.Error("an oversized answer that is not a list was read whole")
	}
}

// The streamed rewrite leaves an answer it has nothing to change equal, as
// JSON, to what came.
func TestRewriteListKeepsEveryField(t *testing.T) {
	fx := newFixtureWith(t, true)
	in := `{"subscriptions":[{"name":"a","pushConfig":{}},{"name":"b","ackDeadlineSeconds":10}],"nextPageToken":"t","other":{"k":[1,2]}}`
	var out bytes.Buffer
	if err := fx.front.rewriteList(&out, strings.NewReader(in)); err != nil {
		t.Fatal(err)
	}
	if out.String() != in {
		t.Errorf("rewritten as %s", out.String())
	}
	for _, bad := range []string{`[]`, `{"subscriptions":{}}`, `{"subscriptions":[{]}`, `{"subscriptions":[]`} {
		if err := fx.front.rewriteList(io.Discard, strings.NewReader(bad)); err == nil {
			t.Errorf("rewriteList(%s) succeeded", bad)
		}
	}
}
