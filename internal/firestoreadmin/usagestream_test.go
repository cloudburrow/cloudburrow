package firestoreadmin

import (
	"bytes"
	"encoding/binary"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"cloud.google.com/go/firestore/apiv1/firestorepb"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func frame(t *testing.T, m proto.Message) []byte {
	t.Helper()
	b, err := proto.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]byte, 5, 5+len(b))
	binary.BigEndian.PutUint32(out[1:], uint32(len(b)))
	return append(out, b...)
}

// TestListenStreamCountsReads (#2050): each document a listener delivers
// is a read once per version (LOOKUP for a documents target, QUERY for a query), a removal
// from the results is a read, a delete is not, and a query that becomes
// current with no results costs one read.
func TestListenStreamCountsReads(t *testing.T) {
	db := "projects/p/databases/(default)"
	var req bytes.Buffer
	for _, m := range []*firestorepb.ListenRequest{
		{Database: db, TargetChange: &firestorepb.ListenRequest_AddTarget{AddTarget: &firestorepb.Target{TargetId: 1,
			TargetType: &firestorepb.Target_Query{Query: &firestorepb.Target_QueryTarget{Parent: db + "/documents"}}}}},
		{Database: db, TargetChange: &firestorepb.ListenRequest_AddTarget{AddTarget: &firestorepb.Target{TargetId: 2,
			TargetType: &firestorepb.Target_Documents{Documents: &firestorepb.Target_DocumentsTarget{Documents: []string{db + "/documents/c/a"}}}}}},
		{Database: db, TargetChange: &firestorepb.ListenRequest_AddTarget{AddTarget: &firestorepb.Target{TargetId: 3,
			TargetType: &firestorepb.Target_Query{Query: &firestorepb.Target_QueryTarget{Parent: db + "/documents"}}}}},
	} {
		req.Write(frame(t, m))
	}
	doc := &firestorepb.Document{Name: db + "/documents/c/a", UpdateTime: timestamppb.New(time.Unix(1, 0))}
	doc2 := &firestorepb.Document{Name: doc.Name, UpdateTime: timestamppb.New(time.Unix(2, 0))}
	var res bytes.Buffer
	for _, m := range []*firestorepb.ListenResponse{
		{ResponseType: &firestorepb.ListenResponse_DocumentChange{DocumentChange: &firestorepb.DocumentChange{Document: doc, TargetIds: []int32{1}}}},
		{ResponseType: &firestorepb.ListenResponse_DocumentChange{DocumentChange: &firestorepb.DocumentChange{Document: doc, TargetIds: []int32{2}}}},
		{ResponseType: &firestorepb.ListenResponse_TargetChange{TargetChange: &firestorepb.TargetChange{TargetChangeType: firestorepb.TargetChange_CURRENT, TargetIds: []int32{1, 2, 3}}}},
		// The emulator's reset resends the same version: not a read.
		{ResponseType: &firestorepb.ListenResponse_TargetChange{TargetChange: &firestorepb.TargetChange{TargetChangeType: firestorepb.TargetChange_RESET, TargetIds: []int32{1}}}},
		{ResponseType: &firestorepb.ListenResponse_DocumentChange{DocumentChange: &firestorepb.DocumentChange{Document: doc, TargetIds: []int32{1}}}},
		{ResponseType: &firestorepb.ListenResponse_DocumentChange{DocumentChange: &firestorepb.DocumentChange{Document: doc2, TargetIds: []int32{1}}}},
		{ResponseType: &firestorepb.ListenResponse_DocumentRemove{DocumentRemove: &firestorepb.DocumentRemove{Document: doc.Name, RemovedTargetIds: []int32{1}}}},
		{ResponseType: &firestorepb.ListenResponse_DocumentDelete{DocumentDelete: &firestorepb.DocumentDelete{Document: doc.Name, RemovedTargetIds: []int32{1}}}},
		{ResponseType: &firestorepb.ListenResponse_TargetChange{TargetChange: &firestorepb.TargetChange{TargetChangeType: firestorepb.TargetChange_CURRENT, TargetIds: []int32{1, 3}}}},
	} {
		res.Write(frame(t, m))
	}
	u := New("").Usage()
	h := u.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.ReadAll(r.Body); err != nil {
			t.Error(err)
		}
		// Written in pieces, as a stream arrives.
		b := res.Bytes()
		for len(b) > 0 {
			n := min(7, len(b))
			_, _ = w.Write(b[:n])
			b = b[n:]
		}
	}))
	r := httptest.NewRequest(http.MethodPost, "/google.firestore.v1.Firestore/Listen", &req)
	r.Header.Set("Content-Type", "application/grpc")
	h.ServeHTTP(httptest.NewRecorder(), r)
	got := u.Minutes(u.now().Add(-1e9))
	if len(got) != 1 {
		t.Fatalf("minutes = %+v", got)
	}
	reads := got[0].Databases[db].Reads
	// QUERY: two changes, one removal, and target 3's empty result.
	if reads["QUERY"] != 4 || reads["LOOKUP"] != 1 {
		t.Errorf("reads = %v, want QUERY 4, LOOKUP 1", reads)
	}
}

// TestFrameReaderHoldsPartialFrames: a message passes on only once whole,
// after onMsg saw it.
func TestFrameReaderHoldsPartialFrames(t *testing.T) {
	m := &firestorepb.WriteRequest{Database: "projects/p/databases/(default)"}
	f := frame(t, m)
	all := append(append([]byte(nil), f...), f...)
	var seen int
	fr := &frameReader{rc: io.NopCloser(&slowReader{b: all}), onMsg: func(b []byte) {
		var got firestorepb.WriteRequest
		if proto.Unmarshal(b, &got) != nil || got.Database != m.Database {
			t.Errorf("onMsg saw %x", b)
		}
		seen++
	}}
	out, err := io.ReadAll(fr)
	if err != nil || !bytes.Equal(out, all) || seen != 2 {
		t.Errorf("read %x, %v, seen %d; want both frames, each seen", out, err, seen)
	}
}

type slowReader struct{ b []byte }

func (s *slowReader) Read(p []byte) (int, error) {
	if len(s.b) == 0 {
		return 0, io.EOF
	}
	n := copy(p[:min(3, len(p))], s.b)
	s.b = s.b[n:]
	return n, nil
}
