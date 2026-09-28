package pubsubfront

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptrace"
	"strings"
	"testing"

	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/protobuf/proto"
)

// h2cClient speaks plain-text HTTP/2 with prior knowledge only, as `curl
// --http2-prior-knowledge` does.
func h2cClient() *http.Client {
	var p http.Protocols
	p.SetUnencryptedHTTP2(true)
	return &http.Client{Transport: &http.Transport{Protocols: &p}}
}

// #909: a REST client speaking plain-text HTTP/2 with prior knowledge
// reaches the REST side, with the REST rules, on the port an official gRPC
// client uses at the same time: its reads answer, a create is sent on with
// the 31-day default, and a GET, which has no content-type, is REST too.
func TestRESTOverH2CPriorKnowledge(t *testing.T) {
	up := newRESTUpstream(t)
	fx := newFixtureREST(t, up.addr())
	c := h2cClient()
	do := func(method, path, body string) (*http.Response, map[string]any) {
		t.Helper()
		req, err := http.NewRequest(method, "http://"+fx.addr+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := c.Do(req)
		if err != nil {
			t.Fatalf("%s %s over h2c: %v", method, path, err)
		}
		defer resp.Body.Close()
		out := map[string]any{}
		b, _ := io.ReadAll(resp.Body)
		_ = json.Unmarshal(b, &out)
		return resp, out
	}
	fx.topic(t, "grpc-first")
	resp, body := do(http.MethodGet, "/v1/projects/"+project+"/topics", "")
	if resp.StatusCode != http.StatusOK || resp.ProtoMajor != 2 || body["from"] != "upstream" {
		t.Errorf("GET over h2c = HTTP/%d %d %v, want the upstream's answer over HTTP/2", resp.ProtoMajor, resp.StatusCode, body)
	}
	resp, _ = do(http.MethodPut, "/v1/projects/"+project+"/subscriptions/h2c", `{"topic":"projects/p/topics/t"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT over h2c = %d", resp.StatusCode)
	}
	resp, body = do(http.MethodPut, "/v1/projects/"+project+"/subscriptions/short", `{"topic":"projects/p/topics/t","expirationPolicy":{"ttl":"3600s"}}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("a 1-hour ttl over h2c = %d %v, want 400", resp.StatusCode, body)
	}
	got := up.calls()
	if len(got) != 2 || got[1].method != http.MethodPut || !bytes.Contains(got[1].body, []byte(`"2678400s"`)) {
		t.Errorf("the REST upstream saw %v; want the GET and the create with the 31-day default", got)
	}
	if !fx.tracked(subName("h2c")) {
		t.Error("a subscription created over h2c is not tracked")
	}
	// gRPC is unaffected, before and after, on its own connections.
	if _, err := fx.client.TopicAdminClient.GetTopic(context.Background(),
		&pubsubpb.GetTopicRequest{Topic: "projects/" + project + "/topics/grpc-first"}); err != nil {
		t.Errorf("gRPC after h2c REST: %v", err)
	}
}

// Each request is routed by its own content type, not its connection's
// first: on one HTTP/2 connection, as CloudBurrow's host tunnel pools them, a
// REST GET, a gRPC call and a REST GET again are each served by their side.
func TestOneHTTP2ConnectionCarriesGRPCAndREST(t *testing.T) {
	up := newRESTUpstream(t)
	fx := newFixtureREST(t, up.addr())
	topic := fx.topic(t, "shared")
	c := h2cClient()
	var conns []bool
	trace := &httptrace.ClientTrace{GotConn: func(i httptrace.GotConnInfo) { conns = append(conns, i.Reused) }}
	get := func() {
		t.Helper()
		req, _ := http.NewRequestWithContext(httptrace.WithClientTrace(context.Background(), trace),
			http.MethodGet, "http://"+fx.addr+"/v1/projects/"+project+"/topics", nil)
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK || !bytes.Contains(b, []byte("upstream")) {
			t.Errorf("REST GET = %d %s, want the REST upstream's answer", resp.StatusCode, b)
		}
	}
	get()
	msg, err := proto.Marshal(&pubsubpb.GetTopicRequest{Topic: topic})
	if err != nil {
		t.Fatal(err)
	}
	framed := append([]byte{0, 0, 0, 0, 0}, msg...)
	binary.BigEndian.PutUint32(framed[1:5], uint32(len(msg)))
	req, _ := http.NewRequestWithContext(httptrace.WithClientTrace(context.Background(), trace),
		http.MethodPost, "http://"+fx.addr+"/google.pubsub.v1.Publisher/GetTopic", bytes.NewReader(framed))
	req.Header.Set("Content-Type", "application/grpc")
	req.Header.Set("Te", "trailers")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	var got pubsubpb.Topic
	if resp.Trailer.Get("Grpc-Status") != "0" || len(b) < 5 || proto.Unmarshal(b[5:], &got) != nil || got.GetName() != topic {
		t.Errorf("gRPC GetTopic = status %q, %d bytes, topic %q; want %s", resp.Trailer.Get("Grpc-Status"), len(b), got.GetName(), topic)
	}
	get()
	if len(conns) != 3 || conns[1] != true || conns[2] != true {
		t.Errorf("connections reused = %v; want one connection for all three", conns)
	}
	if n := len(up.calls()); n != 2 {
		t.Errorf("the REST upstream saw %d calls, want the two GETs", n)
	}
}

// A request's content type decides: gRPC's, with any codec, is gRPC;
// JSON, gRPC-Web or none is REST.
func TestIsGRPCContentType(t *testing.T) {
	for ct, want := range map[string]bool{
		"application/grpc": true, "application/grpc+proto": true, "application/grpc;charset=utf-8": true,
		"application/json": false, "": false, "application/grpc-web": false, "application/grpcx": false,
	} {
		if got := isGRPCContentType(ct); got != want {
			t.Errorf("isGRPCContentType(%q) = %v, want %v", ct, got, want)
		}
	}
}
