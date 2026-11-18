package firestoreadmin

import (
	"context"
	"net"
	"net/http"
	"strings"
	"testing"

	adminpb "cloud.google.com/go/firestore/apiv1/admin/adminpb"
	firestorepb "cloud.google.com/go/firestore/apiv1/firestorepb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// writeEmulator is a Write stream that answers each request with a stream
// token, and keeps the writes it was sent and the metadata it saw.
type writeEmulator struct {
	firestorepb.UnimplementedFirestoreServer
	writes chan *firestorepb.Write
	auth   chan string
}

func (w *writeEmulator) Write(s firestorepb.Firestore_WriteServer) error {
	md, _ := metadata.FromIncomingContext(s.Context())
	w.auth <- strings.Join(md.Get("authorization"), ",")
	for {
		req, err := s.Recv()
		if err != nil {
			return nil
		}
		for _, x := range req.GetWrites() {
			w.writes <- x
		}
		if err := s.Send(&firestorepb.WriteResponse{StreamId: "s", StreamToken: []byte("t")}); err != nil {
			return err
		}
	}
}

// TestEditionGuardWriteStream (#1738): the guard passes a Write stream on,
// both ways, and ends a Standard database's with the refusal Commit gets
// when a write carries an Enterprise-only value, which the emulator never
// sees; an Enterprise database's goes through.
func TestEditionGuardWriteStream(t *testing.T) {
	emu := &writeEmulator{writes: make(chan *firestorepb.Write, 16), auth: make(chan string, 16)}
	el, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	es := grpc.NewServer()
	firestorepb.RegisterFirestoreServer(es, emu)
	go func() { _ = es.Serve(el) }()
	t.Cleanup(es.Stop)

	a := New(el.Addr().String())
	a.databases["projects/p/databases/(default)"] = &adminpb.Database{Name: "projects/p/databases/(default)", DatabaseEdition: adminpb.Database_STANDARD}
	a.databases["projects/p/databases/ent"] = &adminpb.Database{Name: "projects/p/databases/ent", DatabaseEdition: adminpb.Database_ENTERPRISE}
	g := a.EditionGuard()
	fl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var protocols http.Protocols
	protocols.SetUnencryptedHTTP2(true)
	fs := &http.Server{Protocols: &protocols, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !g.Claims(r) {
			http.Error(w, "not claimed", http.StatusTeapot)
			return
		}
		g.ServeHTTP(w, r)
	})}
	go func() { _ = fs.Serve(fl) }()
	t.Cleanup(func() { _ = fs.Close() })

	conn, err := grpc.NewClient(fl.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	c := firestorepb.NewFirestoreClient(conn)

	update := func(db string, v *firestorepb.Value) *firestorepb.Write {
		return &firestorepb.Write{Operation: &firestorepb.Write_Update{Update: &firestorepb.Document{
			Name: "projects/p/databases/" + db + "/documents/c/d", Fields: map[string]*firestorepb.Value{"v": v}}}}
	}
	stream := func(db string, v *firestorepb.Value) error {
		ctx := metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer owner")
		s, err := c.Write(ctx)
		if err != nil {
			return err
		}
		if err := s.Send(&firestorepb.WriteRequest{Database: "projects/p/databases/" + db}); err != nil {
			return err
		}
		if r, err := s.Recv(); err != nil || r.GetStreamId() != "s" {
			t.Fatalf("handshake: %v, %v", r, err)
		}
		if got := <-emu.auth; got != "Bearer owner" {
			t.Errorf("the emulator saw authorization %q", got)
		}
		_ = s.Send(&firestorepb.WriteRequest{StreamToken: []byte("t"), Writes: []*firestorepb.Write{update(db, fsInt64)}})
		if _, err := s.Recv(); err != nil {
			return err
		}
		<-emu.writes
		_ = s.Send(&firestorepb.WriteRequest{StreamToken: []byte("t"), Writes: []*firestorepb.Write{update(db, v)}})
		if _, err := s.Recv(); err != nil {
			return err
		}
		<-emu.writes
		_ = s.CloseSend()
		return nil
	}

	err = stream("(default)", fsInt32V)
	if status.Code(err) != codes.InvalidArgument || status.Convert(err).Message() != "Value type INT is not allowed" {
		t.Errorf("Standard, an int32: %v, want INVALID_ARGUMENT", err)
	}
	select {
	case w := <-emu.writes:
		t.Errorf("the emulator was sent the refused write %v", w)
	default:
	}
	if err := stream("(default)", fsArrayOf(fsInt64)); err != nil {
		t.Errorf("Standard, an array: %v", err)
	}
	if err := stream("ent", fsInt32V); err != nil {
		t.Errorf("Enterprise, an int32: %v", err)
	}
}
