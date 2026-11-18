package main

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"cloud.google.com/go/bigtable"
	btpb "cloud.google.com/go/bigtable/apiv2/bigtablepb"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/durationpb"
)

// sessionlessBigtable is a Bigtable data server whose configuration sends
// every point call through a session, as the front's does, but whose
// sessions never open: a client that takes the session path waits out its
// deadline, and one on the classic path is answered at once.
type sessionlessBigtable struct {
	btpb.UnimplementedBigtableServer
	configs, sessions, reads, writes atomic.Int32
}

func (s *sessionlessBigtable) GetClientConfiguration(context.Context, *btpb.GetClientConfigurationRequest) (*btpb.ClientConfiguration, error) {
	s.configs.Add(1)
	return &btpb.ClientConfiguration{
		SessionConfiguration: &btpb.SessionClientConfiguration{SessionLoad: 1},
		Polling: &btpb.ClientConfiguration_PollingConfiguration_{PollingConfiguration: &btpb.ClientConfiguration_PollingConfiguration{
			PollingInterval: durationpb.New(5 * time.Minute), ValidityDuration: durationpb.New(30 * time.Minute)}},
	}, nil
}

func (*sessionlessBigtable) PingAndWarm(context.Context, *btpb.PingAndWarmRequest) (*btpb.PingAndWarmResponse, error) {
	return &btpb.PingAndWarmResponse{}, nil
}

func (s *sessionlessBigtable) OpenTable(ss btpb.Bigtable_OpenTableServer) error {
	s.sessions.Add(1)
	<-ss.Context().Done()
	return ss.Context().Err()
}

func (s *sessionlessBigtable) ReadRows(*btpb.ReadRowsRequest, btpb.Bigtable_ReadRowsServer) error {
	s.reads.Add(1)
	return nil
}

func (s *sessionlessBigtable) MutateRow(context.Context, *btpb.MutateRowRequest) (*btpb.MutateRowResponse, error) {
	s.writes.Add(1)
	return &btpb.MutateRowResponse{}, nil
}

// TestConsoleBigtableClientIsClassic (#1820): the console's Bigtable data
// clients read and write through the classic data plane, never a session,
// so a console request never waits on a cold session pool: the Go client's
// pool can miss the wake-up of the first caller and leave it waiting its
// whole deadline beside an idle session. Each read here would hang on the
// session path; each must be answered well inside its deadline, from a
// fresh client as the console makes one per request.
func TestConsoleBigtableClientIsClassic(t *testing.T) {
	fake := &sessionlessBigtable{}
	gs := grpc.NewServer()
	btpb.RegisterBigtableServer(gs, fake)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = gs.Serve(l) }()
	t.Cleanup(gs.Stop)

	ctx := context.Background()
	for i := 0; i < 5; i++ {
		c, err := newConsoleBigtableClient(ctx, l.Addr().String(), "demo", "default")
		if err != nil {
			t.Fatal(err)
		}
		tbl := c.Open("events")
		// A while for a configuration poll to land and turn the session
		// path on, were the client polling.
		time.Sleep(100 * time.Millisecond)
		rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		start := time.Now()
		_, rerr := tbl.ReadRow(rctx, "r1")
		werr := tbl.Apply(rctx, "r1", bigtable.NewMutation())
		cancel()
		_ = c.Close()
		if rerr != nil || werr != nil || time.Since(start) > 3*time.Second {
			t.Fatalf("client %d: ReadRow = %v, Apply = %v, after %v", i, rerr, werr, time.Since(start))
		}
	}
	if n := fake.sessions.Load(); n != 0 {
		t.Errorf("the console's clients opened %d sessions, want none", n)
	}
	if n := fake.configs.Load(); n != 0 {
		t.Errorf("the console's clients asked for the session configuration %d times, want never", n)
	}
	if r, w := fake.reads.Load(), fake.writes.Load(); r != 5 || w != 5 {
		t.Errorf("classic ReadRows = %d, MutateRow = %d, want 5 each", r, w)
	}
}
