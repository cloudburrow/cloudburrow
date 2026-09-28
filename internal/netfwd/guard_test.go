package netfwd

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// fakeEmulator serves what the Pub/Sub, Firestore and Datastore emulators
// serve on one port (measured, #725): REST over HTTP/1.1 and gRPC over
// prior-knowledge h2c. gRPC is grpc-go's own server, not its ServeHTTP
// adapter, so the frames the proxy passes on are what a real gRPC server
// writes.
type fakeEmulator struct {
	addr string

	restHits atomic.Int32
	grpcHits atomic.Int32
	mu       sync.Mutex
	hosts    []string // Host or :authority of every request that arrived

	// pullDone receives the error each StreamingPull handler returns
	// with, so a test sees a cancel reach the emulator.
	pullDone chan error

	pubsubpb.UnimplementedPublisherServer
	pubsubpb.UnimplementedSubscriberServer
}

func startFakeEmulator(t *testing.T) *fakeEmulator {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	e := &fakeEmulator{addr: ln.Addr().String(), pullDone: make(chan error, 4)}

	gs := grpc.NewServer()
	pubsubpb.RegisterPublisherServer(gs, e)
	pubsubpb.RegisterSubscriberServer(gs, e)

	rest := http.NewServeMux()
	rest.HandleFunc("/v1/projects/p/topics", func(w http.ResponseWriter, r *http.Request) {
		e.restHits.Add(1)
		e.saw(r.Host)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"topics":[{"name":"projects/p/topics/t"}]}`)
	})
	// A chunked response written in two parts, the second only once the
	// test says so: a proxy that buffered would deliver nothing until then.
	release := make(chan struct{})
	rest.HandleFunc("/stream", func(w http.ResponseWriter, r *http.Request) {
		e.restHits.Add(1)
		fmt.Fprint(w, "first\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		fmt.Fprint(w, "second\n")
	})
	rest.HandleFunc("/release", func(w http.ResponseWriter, r *http.Request) { close(release) })
	hs := &http.Server{Handler: rest}

	h1, h2 := splitPreface(ln)
	go func() { _ = gs.Serve(h2) }()
	go func() { _ = hs.Serve(h1) }()
	t.Cleanup(func() {
		gs.Stop()
		_ = hs.Close()
		_ = ln.Close()
	})
	return e
}

func (e *fakeEmulator) saw(host string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.hosts = append(e.hosts, host)
}

func (e *fakeEmulator) seen() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.hosts...)
}

func (e *fakeEmulator) authority(ctx context.Context) {
	e.grpcHits.Add(1)
	md, _ := metadata.FromIncomingContext(ctx)
	if a := md.Get(":authority"); len(a) > 0 {
		e.saw(a[0])
	}
}

// GetTopic answers topic "t" and NotFound for any other: NotFound goes out
// as a Trailers-Only response, the shape a proxy most easily breaks.
func (e *fakeEmulator) GetTopic(ctx context.Context, r *pubsubpb.GetTopicRequest) (*pubsubpb.Topic, error) {
	e.authority(ctx)
	if r.GetTopic() != "projects/p/topics/t" {
		return nil, status.Errorf(codes.NotFound, "Topic not found")
	}
	return &pubsubpb.Topic{Name: r.GetTopic()}, nil
}

// StreamingPull behaves as the emulator's does: it holds the stream open,
// delivers messages as they exist, and reads acknowledgements from the same
// stream while it does.
func (e *fakeEmulator) StreamingPull(s pubsubpb.Subscriber_StreamingPullServer) error {
	e.authority(s.Context())
	first, err := s.Recv()
	if err != nil {
		e.pullDone <- err
		return err
	}
	if err := s.Send(&pubsubpb.StreamingPullResponse{ReceivedMessages: []*pubsubpb.ReceivedMessage{
		{AckId: "ack-1", Message: &pubsubpb.PubsubMessage{Data: []byte("hello from " + first.GetSubscription())}},
	}}); err != nil {
		e.pullDone <- err
		return err
	}
	for {
		req, err := s.Recv()
		if err != nil {
			e.pullDone <- err
			return err
		}
		// Each acknowledgement is answered with a message naming it, so the
		// test sees the client's half of the stream arrive while the
		// server's half is still open.
		for _, id := range req.GetAckIds() {
			if err := s.Send(&pubsubpb.StreamingPullResponse{ReceivedMessages: []*pubsubpb.ReceivedMessage{
				{AckId: id + "-next", Message: &pubsubpb.PubsubMessage{Data: []byte("acked " + id)}},
			}}); err != nil {
				e.pullDone <- err
				return err
			}
		}
	}
}

// splitPreface hands connections that open with the HTTP/2 client preface to
// one listener and the rest to another, as an emulator's single port does.
func splitPreface(ln net.Listener) (h1, h2 net.Listener) {
	a := &chanListener{addr: ln.Addr(), c: make(chan net.Conn), done: make(chan struct{})}
	b := &chanListener{addr: ln.Addr(), c: make(chan net.Conn), done: make(chan struct{})}
	go func() {
		defer a.Close()
		defer b.Close()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				br := bufio.NewReader(c)
				p, _ := br.Peek(len(http2.ClientPreface))
				pc := &peekedConn{Conn: c, r: br}
				dst := a
				if string(p) == http2.ClientPreface {
					dst = b
				}
				select {
				case dst.c <- pc:
				case <-dst.done:
					c.Close()
				}
			}()
		}
	}()
	return a, b
}

type peekedConn struct {
	net.Conn
	r io.Reader
}

func (c *peekedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

type chanListener struct {
	addr net.Addr
	c    chan net.Conn
	once sync.Once
	done chan struct{}
}

func (l *chanListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.c:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *chanListener) Close() error   { l.once.Do(func() { close(l.done) }); return nil }
func (l *chanListener) Addr() net.Addr { return l.addr }

// startTestGuard puts the guard in front of the fake emulator and returns
// the address clients use.
func startTestGuard(t *testing.T, upstream string) string {
	t.Helper()
	g, err := startGuard("127.0.0.1:0", upstream, t.Logf, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(g.close)
	return g.ln.Addr().String()
}

func grpcConn(t *testing.T, addr string, opts ...grpc.DialOption) *grpc.ClientConn {
	t.Helper()
	cc, err := grpc.NewClient(addr, append([]grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cc.Close() })
	return cc
}

// h2cClient speaks prior-knowledge cleartext HTTP/2, which is how gRPC
// arrives and which no browser can send.
func h2cClient() *http.Client {
	return &http.Client{Transport: &http2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
	}}
}

// The acceptance case for #725: a DNS-rebound page's Host is refused on a
// guarded tunnel, over HTTP/1.1 and over a non-gRPC h2c request, and
// nothing reaches the emulator.
func TestGuardRefusesAForeignHost(t *testing.T) {
	t.Parallel()
	emu := startFakeEmulator(t)
	addr := startTestGuard(t, emu.addr)

	t.Run("HTTP/1.1 REST", func(t *testing.T) {
		for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
			req, _ := http.NewRequest(method, "http://"+addr+"/v1/projects/p/topics", nil)
			req.Host = "attacker.example:8085"
			req.Header.Set("Origin", "http://attacker.example:8085")
			req.Header.Set("Sec-Fetch-Site", "same-origin")
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(res.Body)
			res.Body.Close()
			if res.StatusCode != http.StatusMisdirectedRequest {
				t.Errorf("%s with Host attacker.example: status %d, want 421", method, res.StatusCode)
			}
			if !strings.Contains(string(body), `"attacker.example:8085"`) {
				t.Errorf("the refusal should name the host; body %q", body)
			}
		}
	})

	t.Run("h2c non-gRPC", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, "http://"+addr+"/v1/projects/p/topics", nil)
		req.Host = "attacker.example"
		res, err := h2cClient().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.ProtoMajor != 2 || res.StatusCode != http.StatusMisdirectedRequest {
			t.Errorf("h2c GET with :authority attacker.example: %s %d, want HTTP/2 421", res.Proto, res.StatusCode)
		}
	})

	if n, m := emu.restHits.Load(), emu.grpcHits.Load(); n != 0 || m != 0 {
		t.Errorf("the emulator saw %d REST and %d gRPC requests for a refused host; want none", n, m)
	}
}

// gRPC is not checked, as on every listener CloudBurrow serves: no browser
// can send cleartext HTTP/2, and a gRPC client's :authority is whatever it
// dialed. A call with a foreign :authority reaches the emulator, unary and
// streaming, with that authority unchanged.
func TestGuardPassesGRPCWhateverItsAuthority(t *testing.T) {
	t.Parallel()
	emu := startFakeEmulator(t)
	addr := startTestGuard(t, emu.addr)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cc := grpcConn(t, addr, grpc.WithAuthority("attacker.example:8085"))
	topic, err := pubsubpb.NewPublisherClient(cc).GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: "projects/p/topics/t"})
	if err != nil || topic.GetName() != "projects/p/topics/t" {
		t.Errorf("gRPC with :authority attacker.example: %v, %v; want the topic", topic, err)
	}
	stream, err := pubsubpb.NewSubscriberClient(cc).StreamingPull(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&pubsubpb.StreamingPullRequest{Subscription: "projects/p/subscriptions/s"}); err != nil {
		t.Fatal(err)
	}
	if got, err := stream.Recv(); err != nil || len(got.GetReceivedMessages()) != 1 {
		t.Errorf("StreamingPull with :authority attacker.example: %v, %v; want a message", got, err)
	}
	if seen := strings.Join(emu.seen(), " "); strings.Count(seen, "attacker.example:8085") != 2 {
		t.Errorf("the emulator should see :authority attacker.example:8085 on both calls; saw %s", seen)
	}
}

// Every name CloudBurrow hands out passes, and the emulator sees the name
// the client used, as it would through a raw tunnel.
func TestGuardPassesAllowedHosts(t *testing.T) {
	t.Parallel()
	emu := startFakeEmulator(t)
	addr := startTestGuard(t, emu.addr)
	_, port, _ := net.SplitHostPort(addr)

	hosts := []string{
		addr, // 127.0.0.1:<port>, what `cloudburrow env` exports
		"localhost:" + port,
		"[::1]:" + port,
		"host.docker.internal:" + port,
		"cloudburrow-host.cloudburrow.svc.cluster.local:" + port,
	}
	for _, h := range hosts {
		req, _ := http.NewRequest(http.MethodGet, "http://"+addr+"/v1/projects/p/topics", nil)
		req.Host = h
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != http.StatusOK || !strings.Contains(string(body), "projects/p/topics/t") {
			t.Errorf("HTTP/1.1 with Host %s: %d %q, want 200 and the topic list", h, res.StatusCode, body)
		}

		cc := grpcConn(t, addr, grpc.WithAuthority(h))
		topic, err := pubsubpb.NewPublisherClient(cc).GetTopic(context.Background(), &pubsubpb.GetTopicRequest{Topic: "projects/p/topics/t"})
		if err != nil || topic.GetName() != "projects/p/topics/t" {
			t.Errorf("gRPC with :authority %s: %v, %v", h, topic, err)
		}
	}
	seen := strings.Join(emu.seen(), " ")
	for _, h := range hosts {
		if strings.Count(seen, h) < 2 {
			t.Errorf("the emulator should see %s as Host and as :authority; saw %s", h, seen)
		}
	}
}

// gRPC through the guard behaves as it does on a raw tunnel: an error
// status sent Trailers-Only keeps its code, and a bidirectional stream
// carries both halves at once.
func TestGuardCarriesGRPC(t *testing.T) {
	t.Parallel()
	emu := startFakeEmulator(t)
	addr := startTestGuard(t, emu.addr)
	cc := grpcConn(t, addr) // :authority 127.0.0.1:<port>, as PUBSUB_EMULATOR_HOST gives it

	t.Run("unary error keeps its code", func(t *testing.T) {
		_, err := pubsubpb.NewPublisherClient(cc).GetTopic(context.Background(), &pubsubpb.GetTopicRequest{Topic: "projects/p/topics/missing"})
		if status.Code(err) != codes.NotFound || status.Convert(err).Message() != "Topic not found" {
			t.Errorf("GetTopic of a missing topic: %v, want NotFound \"Topic not found\"", err)
		}
	})

	t.Run("StreamingPull", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		stream, err := pubsubpb.NewSubscriberClient(cc).StreamingPull(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := stream.Send(&pubsubpb.StreamingPullRequest{Subscription: "projects/p/subscriptions/s", StreamAckDeadlineSeconds: 10}); err != nil {
			t.Fatal(err)
		}
		got, err := stream.Recv()
		if err != nil {
			t.Fatal(err)
		}
		if m := got.GetReceivedMessages(); len(m) != 1 || string(m[0].GetMessage().GetData()) != "hello from projects/p/subscriptions/s" {
			t.Fatalf("first message: %v", got)
		}
		// Acknowledge on the same stream while it stays open, three times:
		// each answer proves the client's half reached the emulator while
		// the server's half was still streaming.
		for i := 1; i <= 3; i++ {
			id := fmt.Sprintf("ack-%d", i)
			if i > 1 {
				id = fmt.Sprintf("ack-%d-next", i-1)
			}
			if err := stream.Send(&pubsubpb.StreamingPullRequest{AckIds: []string{id}}); err != nil {
				t.Fatal(err)
			}
			got, err := stream.Recv()
			if err != nil {
				t.Fatal(err)
			}
			if m := got.GetReceivedMessages(); len(m) != 1 || string(m[0].GetMessage().GetData()) != "acked "+id {
				t.Fatalf("answer to %s: %v", id, got)
			}
		}
		// The client going away reaches the emulator, as it would through
		// kubectl: the handler's stream ends rather than hanging.
		cancel()
		select {
		case err := <-emu.pullDone:
			if status.Code(err) != codes.Canceled && !errors.Is(err, context.Canceled) {
				t.Errorf("the emulator's stream ended with %v, want a cancellation", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("the emulator's StreamingPull was not cancelled when the client went away")
		}
	})
}

// A chunked REST response is passed on as the emulator writes it, not
// buffered until it ends.
func TestGuardStreamsHTTP1(t *testing.T) {
	t.Parallel()
	emu := startFakeEmulator(t)
	addr := startTestGuard(t, emu.addr)

	res, err := http.Get("http://" + addr + "/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	br := bufio.NewReader(res.Body)
	line, err := br.ReadString('\n')
	if err != nil || line != "first\n" {
		t.Fatalf("first chunk: %q, %v", line, err)
	}
	// The first chunk arrived while the emulator is still holding the
	// response open; now let it finish.
	r2, err := http.Get("http://" + addr + "/release")
	if err != nil {
		t.Fatal(err)
	}
	r2.Body.Close()
	rest, _ := io.ReadAll(br)
	if !bytes.Equal(rest, []byte("second\n")) {
		t.Errorf("rest of the stream: %q", rest)
	}
}

// With nothing behind the guard (kubectl restarting), a gRPC client gets
// Unavailable, which the SDKs retry, and a REST client 502.
func TestGuardReportsADeadTunnel(t *testing.T) {
	t.Parallel()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := ln.Addr().String()
	ln.Close()
	addr := startTestGuard(t, dead)

	res, err := http.Get("http://" + addr + "/v1/projects/p/topics")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusBadGateway {
		t.Errorf("REST with no tunnel: %d, want 502", res.StatusCode)
	}
	_, err = pubsubpb.NewPublisherClient(grpcConn(t, addr)).GetTopic(context.Background(), &pubsubpb.GetTopicRequest{Topic: "projects/p/topics/t"})
	if status.Code(err) != codes.Unavailable {
		t.Errorf("gRPC with no tunnel: %v, want Unavailable", err)
	}
}

// A gRPC error with no messages is "Trailers-Only": one HEADERS frame that
// carries grpc-status and ends the stream. Split into headers and an empty
// end, it reads to grpc-java as a stream that ended without trailers, so
// the frames are checked here, below any client's tolerance.
func TestGuardKeepsTrailersOnly(t *testing.T) {
	t.Parallel()
	emu := startFakeEmulator(t)
	addr := startTestGuard(t, emu.addr)

	for _, c := range []struct {
		authority, topic, status string
	}{
		{addr, "projects/p/topics/missing", "5"},               // the emulator's NotFound, passed on
		{"attacker.example", "projects/p/topics/missing", "5"}, // gRPC is not checked
	} {
		frames := rawGRPC(t, addr, c.authority, "/google.pubsub.v1.Publisher/GetTopic",
			append([]byte{0x0a, byte(len(c.topic))}, c.topic...))
		if len(frames) != 1 {
			t.Errorf("%s: %d frames %v, want one HEADERS ending the stream", c.authority, len(frames), frames)
			continue
		}
		h, ok := frames[0].(*http2.MetaHeadersFrame)
		if !ok || !h.StreamEnded() || h.PseudoValue("status") != "200" {
			t.Errorf("%s: got %v, want one HEADERS with :status 200 ending the stream", c.authority, frames[0])
			continue
		}
		var got string
		for _, f := range h.RegularFields() {
			if f.Name == "grpc-status" {
				got = f.Value
			}
		}
		if got != c.status {
			t.Errorf("%s: grpc-status %q, want %s", c.authority, got, c.status)
		}
	}
}

// rawGRPC sends one unary gRPC call with a hand-built framer and returns
// the response's frames on its stream.
func rawGRPC(t *testing.T, addr, authority, path string, msg []byte) []http2.Frame {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write([]byte(http2.ClientPreface)); err != nil {
		t.Fatal(err)
	}
	fr := http2.NewFramer(c, c)
	fr.ReadMetaHeaders = hpack.NewDecoder(4096, nil)
	_ = fr.WriteSettings()
	var hb bytes.Buffer
	enc := hpack.NewEncoder(&hb)
	for _, kv := range [][2]string{
		{":method", "POST"}, {":scheme", "http"}, {":path", path}, {":authority", authority},
		{"content-type", "application/grpc"}, {"te", "trailers"},
	} {
		_ = enc.WriteField(hpack.HeaderField{Name: kv[0], Value: kv[1]})
	}
	_ = fr.WriteHeaders(http2.HeadersFrameParam{StreamID: 1, BlockFragment: hb.Bytes(), EndHeaders: true})
	_ = fr.WriteData(1, true, append([]byte{0, 0, 0, 0, byte(len(msg))}, msg...))

	var out []http2.Frame
	for {
		f, err := fr.ReadFrame()
		if err != nil {
			t.Fatalf("reading the response: %v (frames so far %v)", err, out)
		}
		if s, ok := f.(*http2.SettingsFrame); ok && !s.IsAck() {
			_ = fr.WriteSettingsAck()
		}
		if f.Header().StreamID != 1 {
			continue
		}
		out = append(out, f)
		fl := f.Header().Flags
		if _, rst := f.(*http2.RSTStreamFrame); rst ||
			(f.Header().Type == http2.FrameHeaders && fl.Has(http2.FlagHeadersEndStream)) ||
			(f.Header().Type == http2.FrameData && fl.Has(http2.FlagDataEndStream)) {
			return out
		}
	}
}

// A Guarded target publishes the guard, not kubectl: kubectl listens on its
// own loopback port, the host address refuses a rebound Host, and the guard
// keeps its address across the supervisor re-establishing kubectl.
func TestAGuardedTunnelPublishesTheGuard(t *testing.T) {
	dir := fakeWorld(t)
	podsJSON(t, dir, fakePod{"pubsub-a", true})
	old := podWatch
	podWatch = 200 * time.Millisecond
	t.Cleanup(func() { podWatch = old })

	f := New(Target{Name: "pubsub", Namespace: "cb", ServicePort: 1, Guarded: true}, filepath.Join(dir, "kubeconfig"), "127.0.0.1")
	f.Logf = t.Logf
	t.Cleanup(func() { _ = f.Stop(context.Background()) })
	if err := f.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	kubeAddr := func() string {
		f.mu.Lock()
		defer f.mu.Unlock()
		return net.JoinHostPort(kubeBind, fmt.Sprint(f.kubePort))
	}
	host, kube := f.HostAddr(), kubeAddr()
	if strings.HasSuffix(kube, ":0") || kube == host {
		t.Fatalf("kubectl listens on %s and the tunnel is published on %s; want two ports", kube, host)
	}
	if c, err := net.Dial("tcp", kube); err != nil {
		t.Fatalf("kubectl is not listening on its own port %s: %v", kube, err)
	} else {
		c.Close()
	}

	refused := func() bool {
		req, _ := http.NewRequest(http.MethodGet, "http://"+host+"/v1/projects/p/topics", nil)
		req.Host = "attacker.example"
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			return false
		}
		res.Body.Close()
		return res.StatusCode == http.StatusMisdirectedRequest
	}
	if !refused() {
		t.Fatalf("a request for attacker.example on %s was not refused with 421", host)
	}

	// The pod goes and another comes: kubectl is re-established on the
	// same port behind the same guard.
	if err := os.Remove(filepath.Join(dir, "pod-pubsub-a")); err != nil {
		t.Fatal(err)
	}
	podsJSON(t, dir, fakePod{"pubsub-b", true})
	waitFor(t, 30*time.Second, "the tunnel re-established to pod/pubsub-b", func() bool {
		got := launches(t, dir)
		return f.Restarts() == 1 && len(got) == 2 && got[1] == "pod/pubsub-b"
	})
	if now := kubeAddr(); f.HostAddr() != host || now != kube {
		t.Errorf("after the restart the tunnel is %s behind %s; want %s behind %s", f.HostAddr(), now, host, kube)
	}
	if !refused() {
		t.Error("after the restart a request for attacker.example was not refused")
	}

	if err := f.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c, err := net.DialTimeout("tcp", host, time.Second); err == nil {
		c.Close()
		t.Errorf("%s still accepts connections after Stop", host)
	}
}

// close drops connections without waiting for their handlers, and a
// handler whose copy it cut short logs the error. None of that may reach
// logf once close has returned: the Forwarder's Logf, t.Logf here, may be
// gone by then (the data race behind #803's failures).
func TestGuardDoesNotLogAfterClose(t *testing.T) {
	t.Parallel()
	emu := startFakeEmulator(t)
	var closed atomic.Bool
	late := make(chan string, 16)
	g, err := startGuard("127.0.0.1:0", emu.addr, func(format string, args ...any) {
		if closed.Load() {
			select {
			case late <- fmt.Sprintf(format, args...):
			default:
			}
		}
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	addr := g.ln.Addr().String()

	// Streams in flight in both protocols, each held open by the emulator
	// mid-body, so close cuts short a copy the handler is still doing.
	res, err := http.Get("http://" + addr + "/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if line, err := bufio.NewReader(res.Body).ReadString('\n'); err != nil || line != "first\n" {
		t.Fatalf("first chunk: %q, %v", line, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream, err := pubsubpb.NewSubscriberClient(grpcConn(t, addr)).StreamingPull(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&pubsubpb.StreamingPullRequest{Subscription: "projects/p/subscriptions/s"}); err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Recv(); err != nil {
		t.Fatal(err)
	}

	g.close()
	closed.Store(true)
	// The emulator's handler ends once the proxy's upstream call does; by
	// then the proxy's handler has seen its copy fail.
	select {
	case <-emu.pullDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the emulator's StreamingPull did not end after the guard closed")
	}
	select {
	case line := <-late:
		t.Errorf("the guard logged after close returned: %s", line)
	case <-time.After(500 * time.Millisecond):
	}
}

// A target's Front sees each allowed REST request before the emulator, and
// no gRPC request and no refused Host (#861).
func TestGuardRunsTheFrontForRESTOnly(t *testing.T) {
	t.Parallel()
	emu := startFakeEmulator(t)
	var fronted atomic.Int32
	front := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fronted.Add(1)
			next.ServeHTTP(w, r)
		})
	}
	g, err := startGuard("127.0.0.1:0", emu.addr, t.Logf, front)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(g.close)
	addr := g.ln.Addr().String()

	res, err := http.Get("http://" + addr + "/v1/projects/p/topics")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK || fronted.Load() != 1 {
		t.Errorf("REST: %d, front ran %d times; want 200 and once", res.StatusCode, fronted.Load())
	}

	req, _ := http.NewRequest(http.MethodGet, "http://"+addr+"/v1/projects/p/topics", nil)
	req.Host = "attacker.example:80"
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode == http.StatusOK || fronted.Load() != 1 {
		t.Errorf("foreign Host: %d, front ran %d times; want refused before the front", res.StatusCode, fronted.Load())
	}

	cc := grpcConn(t, addr)
	if _, err := pubsubpb.NewPublisherClient(cc).GetTopic(context.Background(), &pubsubpb.GetTopicRequest{Topic: "projects/p/topics/t"}); err != nil {
		t.Fatal(err)
	}
	if fronted.Load() != 1 {
		t.Errorf("gRPC went through the front")
	}
}

// A guard given more hosts accepts them, and still refuses any other
// (#881): BigQuery's REST tunnel answers to its Service's in-cluster names
// when that Service is routed to it.
func TestGuardAcceptsTheHostsItIsGiven(t *testing.T) {
	t.Parallel()
	emu := startFakeEmulator(t)
	g, err := startGuard("127.0.0.1:0", emu.addr, t.Logf, nil, "bigquery", "bigquery.cb.svc", "bigquery.cb.svc.cluster.local")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(g.close)
	for host, want := range map[string]int{
		"bigquery.cb.svc.cluster.local:9050": http.StatusOK,
		"bigquery:9050":                      http.StatusOK,
		"bigquery.cb.svc":                    http.StatusOK,
		"bigquery.cb:9050":                   http.StatusMisdirectedRequest,
		"bigquery.example:9050":              http.StatusMisdirectedRequest,
	} {
		req, _ := http.NewRequest(http.MethodGet, "http://"+g.ln.Addr().String()+"/v1/projects/p/topics", nil)
		req.Host = host
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != want {
			t.Errorf("Host %s: %d, want %d", host, res.StatusCode, want)
		}
	}
}
