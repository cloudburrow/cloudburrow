package pubsubfront

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"

	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
)

// readCalls reads the call log the way the CLI does, through CallsPath.
func (fx *fixture) readCalls(t *testing.T, after uint64) []Call {
	t.Helper()
	resp, err := http.Get("http://" + fx.addr + CallsPath + "?after=" + strconv.FormatUint(after, 10))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var page struct{ Calls []Call }
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		t.Fatal(err)
	}
	return page.Calls
}

// Every Pub/Sub call through the front, gRPC and REST, is in the call log
// with its method, resource, code and duration, and the traceparent it came
// with (#1266); CloudBurrow's own methods are not.
func TestCallLogRecordsGRPCAndREST(t *testing.T) {
	up := newRESTUpstream(t)
	fx := newFixtureREST(t, up.addr())
	ctx := metadata.AppendToOutgoingContext(context.Background(),
		"traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	topic := "projects/" + project + "/topics/logged"
	if _, err := fx.client.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: topic}); err != nil {
		t.Fatal(err)
	}
	_, err := fx.client.TopicAdminClient.GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: "projects/" + project + "/topics/missing"})
	if err == nil {
		t.Fatal("GetTopic of a missing topic succeeded")
	}
	fx.advance(t, 0)
	if code, _ := fx.rest(t, http.MethodGet, "/v1/projects/"+project+"/topics/logged", ""); code != http.StatusOK {
		t.Fatalf("REST GET: %d", code)
	}

	calls := fx.readCalls(t, 0)
	if len(calls) != 3 {
		t.Fatalf("calls = %+v, want 3 (CreateTopic, GetTopic, the REST GET; not the clock)", calls)
	}
	create, get, rest := calls[0], calls[1], calls[2]
	if create.Method != publisher+"CreateTopic" || create.Code != int(codes.OK) || create.Resource != topic ||
		create.Transport != "grpc" || create.Duration <= 0 || create.Time.IsZero() ||
		create.TraceParent != "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01" {
		t.Errorf("CreateTopic recorded as %+v", create)
	}
	if get.Method != publisher+"GetTopic" || get.Code != int(codes.NotFound) {
		t.Errorf("GetTopic recorded as %+v", get)
	}
	if rest.Method != "GET /v1/projects/"+project+"/topics/logged" || rest.Transport != "http" ||
		rest.Code != http.StatusOK || rest.Resource != topic {
		t.Errorf("REST GET recorded as %+v", rest)
	}
	if !(create.Seq < get.Seq && get.Seq < rest.Seq) {
		t.Errorf("sequence not increasing: %d %d %d", create.Seq, get.Seq, rest.Seq)
	}
	if after := fx.readCalls(t, get.Seq); len(after) != 1 || after[0].Seq != rest.Seq {
		t.Errorf("calls after %d = %+v, want the REST GET alone", get.Seq, after)
	}
}

// A StreamingPull is recorded as a stream once it ends, with its
// subscription.
func TestCallLogRecordsStreamingPull(t *testing.T) {
	fx := newFixture(t)
	topic := fx.topic(t, "streamed")
	sub := subName("streamed")
	if _, err := fx.create(t, &pubsubpb.Subscription{Name: sub, Topic: topic}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_ = fx.client.Subscriber(sub).Receive(ctx, nil)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, c := range fx.readCalls(t, 0) {
			if c.Method == subscriber+"StreamingPull" {
				if !c.Stream || c.Resource != sub {
					t.Errorf("StreamingPull recorded as %+v", c)
				}
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("no StreamingPull in the call log")
}

// The ring keeps the last callRingSize calls, oldest first.
func TestCallRingBounded(t *testing.T) {
	var r callRing
	for i := 0; i < callRingSize+5; i++ {
		r.add(Call{Method: "m"})
	}
	all := r.after(0)
	if len(all) != callRingSize || all[0].Seq != 6 || all[len(all)-1].Seq != callRingSize+5 {
		t.Fatalf("kept %d calls, %d..%d", len(all), all[0].Seq, all[len(all)-1].Seq)
	}
}

func TestFirstName(t *testing.T) {
	b, _ := proto.Marshal(&pubsubpb.PublishRequest{Topic: "projects/p/topics/t", Messages: []*pubsubpb.PubsubMessage{{Data: []byte("x")}}})
	if got := firstName(b); got != "projects/p/topics/t" {
		t.Errorf("firstName(Publish) = %q", got)
	}
	b, _ = proto.Marshal(&pubsubpb.AcknowledgeRequest{AckIds: []string{"projects/x"}})
	if got := firstName(b); got != "" {
		t.Errorf("firstName of a request with no name in field 1 = %q, want none", got)
	}
}
