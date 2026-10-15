package calllog

import (
	"testing"

	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/protobuf/proto"
)

// The ring keeps the last RingSize calls, oldest first.
func TestRingBounded(t *testing.T) {
	var r Ring
	for i := 0; i < RingSize+5; i++ {
		r.Add(Call{Method: "m"})
	}
	all := r.After(0)
	if len(all) != RingSize || all[0].Seq != 6 || all[len(all)-1].Seq != RingSize+5 {
		t.Fatalf("kept %d calls, %d..%d", len(all), all[0].Seq, all[len(all)-1].Seq)
	}
	if got := r.After(RingSize + 3); len(got) != 2 {
		t.Errorf("after %d: %d calls, want 2", RingSize+3, len(got))
	}
}

func TestFirstName(t *testing.T) {
	b, _ := proto.Marshal(&pubsubpb.PublishRequest{Topic: "projects/p/topics/t", Messages: []*pubsubpb.PubsubMessage{{Data: []byte("x")}}})
	if got := FirstName(b); got != "projects/p/topics/t" {
		t.Errorf("FirstName(Publish) = %q", got)
	}
	b, _ = proto.Marshal(&pubsubpb.AcknowledgeRequest{AckIds: []string{"projects/x"}})
	if got := FirstName(b); got != "" {
		t.Errorf("FirstName of a request with no name in field 1 = %q, want none", got)
	}
}
