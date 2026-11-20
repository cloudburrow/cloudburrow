package callfront

import (
	"bytes"
	"encoding/binary"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	pb "cloud.google.com/go/datastore/apiv1/datastorepb"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// TestDatastoreReadsAtAnEarlierTimeAreRefused (#1942): a read at a past
// read time is claimed, UNIMPLEMENTED; one at about now, or none, passes.
func TestDatastoreReadsAtAnEarlierTimeAreRefused(t *testing.T) {
	d := newDatastoreReadTimes()
	lookup := func(at time.Time) bool {
		b, _ := proto.Marshal(&pb.LookupRequest{ProjectId: "p", ReadOptions: &pb.ReadOptions{
			ConsistencyType: &pb.ReadOptions_ReadTime{ReadTime: timestamppb.New(at)}}})
		frame := make([]byte, 5, 5+len(b))
		binary.BigEndian.PutUint32(frame[1:], uint32(len(b)))
		r := httptest.NewRequest("POST", datastoreServicePrefix+"Lookup", bytes.NewReader(append(frame, b...)))
		r.Header.Set("Content-Type", "application/grpc")
		return d.Claims(r)
	}
	if lookup(time.Now()) {
		t.Error("a Lookup at now was claimed")
	}
	if !lookup(time.Now().Add(-time.Minute)) {
		t.Error("a Lookup a minute back was not claimed")
	}
	body := `{"query":{},"readOptions":{"readTime":"` + time.Now().Add(-time.Hour).UTC().Format(time.RFC3339) + `"}}`
	r := httptest.NewRequest("POST", "/v1/projects/p:runQuery", strings.NewReader(body))
	if !d.Claims(r) {
		t.Fatal("a REST runQuery an hour back was not claimed")
	}
	w := httptest.NewRecorder()
	d.ServeHTTP(w, r)
	if w.Code != 501 || !strings.Contains(w.Body.String(), "UNIMPLEMENTED") {
		t.Errorf("refusal = %d %s", w.Code, w.Body)
	}
	if d.Claims(httptest.NewRequest("POST", "/v1/projects/p:runQuery", strings.NewReader(`{"query":{}}`))) {
		t.Error("a runQuery without a read time was claimed")
	}
}
