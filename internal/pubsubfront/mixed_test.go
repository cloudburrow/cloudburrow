package pubsubfront

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"net"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
	"google.golang.org/protobuf/proto"
)

// rawH2 is a client connection to the front spoken frame by frame.
type rawH2 struct {
	t   *testing.T
	c   net.Conn
	fr  *http2.Framer
	enc *hpack.Encoder
	hb  bytes.Buffer
	dec *hpack.Decoder
}

func dialRawH2(t *testing.T, addr string) *rawH2 {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	r := &rawH2{t: t, c: c, dec: hpack.NewDecoder(4096, nil)}
	r.enc = hpack.NewEncoder(&r.hb)
	if _, err := c.Write([]byte(clientPreface)); err != nil {
		t.Fatal(err)
	}
	r.fr = http2.NewFramer(c, bufio.NewReader(c))
	if err := r.fr.WriteSettings(); err != nil {
		t.Fatal(err)
	}
	return r
}

// request opens stream id with the given headers, indexed in the client's
// HPACK table as Go's encoder does, and sends body, if any, as one DATA
// frame ending the stream.
func (r *rawH2) request(id uint32, body []byte, kv ...string) {
	r.t.Helper()
	r.hb.Reset()
	for i := 0; i < len(kv); i += 2 {
		_ = r.enc.WriteField(hpack.HeaderField{Name: kv[i], Value: kv[i+1]})
	}
	if err := r.fr.WriteHeaders(http2.HeadersFrameParam{StreamID: id, BlockFragment: r.hb.Bytes(),
		EndHeaders: true, EndStream: body == nil}); err != nil {
		r.t.Fatal(err)
	}
	if body != nil {
		if err := r.fr.WriteData(id, true, body); err != nil {
			r.t.Fatal(err)
		}
	}
}

func grpcHeaders(method string) []string {
	return []string{":method", "POST", ":scheme", "http", ":path", method, ":authority", "front",
		"content-type", "application/grpc", "te", "trailers"}
}

func grpcBody(t *testing.T, m proto.Message) []byte {
	t.Helper()
	msg, err := proto.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	framed := append([]byte{0, 0, 0, 0, 0}, msg...)
	binary.BigEndian.PutUint32(framed[1:5], uint32(len(msg)))
	return framed
}

// read reads frames until until says stop, answering SETTINGS, and
// returns each stream's :status and grpc-status.
func (r *rawH2) read(until func(http2.Frame) bool) (status, grpcStatus map[uint32]string) {
	r.t.Helper()
	status, grpcStatus = map[uint32]string{}, map[uint32]string{}
	for {
		f, err := r.fr.ReadFrame()
		if err != nil {
			r.t.Fatalf("read: %v (statuses %v)", err, status)
		}
		switch f := f.(type) {
		case *http2.SettingsFrame:
			if !f.IsAck() {
				_ = r.fr.WriteSettingsAck()
			}
		case *http2.HeadersFrame:
			fields, err := r.dec.DecodeFull(f.HeaderBlockFragment())
			if err != nil {
				r.t.Fatal(err)
			}
			for _, h := range fields {
				switch h.Name {
				case ":status":
					status[f.StreamID] = h.Value
				case "grpc-status":
					grpcStatus[f.StreamID] = h.Value
				}
			}
		}
		if until(f) {
			return status, grpcStatus
		}
	}
}

// #963, on the wire: a REST request on a connection grpc-go serves is never
// given to grpc-go. The client is sent a GOAWAY, NO_ERROR, whose last stream
// is the last gRPC call, and a WINDOW_UPDATE giving back the connection
// window the refused request's DATA spent; a gRPC call opened after it is
// refused with it, and the call already open is answered in full. The next
// connection from the same client, gRPC first, is served per request:
// its REST request is answered, with no GOAWAY.
func TestAMixedConnectionIsRefusedWithGoAway(t *testing.T) {
	up := newRESTUpstream(t)
	fx := newFixtureREST(t, up.addr())
	topic := fx.topic(t, "raw")
	get := grpcBody(t, &pubsubpb.GetTopicRequest{Topic: topic})

	r := dialRawH2(t, fx.addr)
	r.request(1, get, grpcHeaders("/google.pubsub.v1.Publisher/GetTopic")...)
	status, grpcStatus := r.read(func(f http2.Frame) bool {
		h, ok := f.(*http2.HeadersFrame)
		return ok && h.StreamID == 1 && h.StreamEnded()
	})
	if status[1] != "200" || grpcStatus[1] != "0" {
		t.Fatalf("gRPC on stream 1 = %v %v", status, grpcStatus)
	}
	// Stream 3 is a REST PUT with a body; stream 5, gRPC, follows it.
	body := []byte(`{"labels":{"k":"v"}}`)
	r.request(3, body, ":method", "PUT", ":scheme", "http", ":path", "/v1/projects/"+project+"/topics/rest",
		":authority", "front", "content-type", "application/json")
	r.request(5, get, grpcHeaders("/google.pubsub.v1.Publisher/GetTopic")...)
	var goaway *http2.GoAwayFrame
	window := uint32(0)
	r.read(func(f http2.Frame) bool {
		switch f := f.(type) {
		case *http2.GoAwayFrame:
			goaway = f
		case *http2.WindowUpdateFrame:
			if f.StreamID == 0 {
				window += f.Increment
			}
		case *http2.HeadersFrame, *http2.DataFrame, *http2.RSTStreamFrame:
			t.Errorf("a refused stream was answered: %v", f)
		}
		return goaway != nil && window >= uint32(len(body)+len(get))
	})
	if goaway.LastStreamID != 1 || goaway.ErrCode != http2.ErrCodeNo {
		t.Errorf("GOAWAY last stream %d, code %v; want 1, NO_ERROR", goaway.LastStreamID, goaway.ErrCode)
	}
	if want := uint32(len(body) + len(get)); window != want {
		t.Errorf("WINDOW_UPDATE gave back %d, want %d, the refused streams' DATA", window, want)
	}
	if n := len(up.calls()); n != 0 {
		t.Errorf("the REST upstream saw %d calls, want none", n)
	}

	// The next connection, gRPC first: served per request.
	r2 := dialRawH2(t, fx.addr)
	r2.request(1, get, grpcHeaders("/google.pubsub.v1.Publisher/GetTopic")...)
	r2.request(3, nil, ":method", "GET", ":scheme", "http", ":path", "/v1/projects/"+project+"/topics", ":authority", "front")
	ended := map[uint32]bool{}
	status, grpcStatus = r2.read(func(f http2.Frame) bool {
		switch f := f.(type) {
		case *http2.GoAwayFrame:
			t.Errorf("the second connection was sent a GOAWAY: %v", f)
			return true
		case *http2.HeadersFrame:
			ended[f.StreamID] = ended[f.StreamID] || f.StreamEnded()
		case *http2.DataFrame:
			ended[f.StreamID] = ended[f.StreamID] || f.StreamEnded()
		}
		return ended[1] && ended[3]
	})
	if status[1] != "200" || grpcStatus[1] != "0" || status[3] != "200" {
		t.Errorf("gRPC then REST on the next connection = %v %v, want both answered", status, grpcStatus)
	}
	if n := len(up.calls()); n != 1 {
		t.Errorf("the REST upstream saw %d calls, want the GET", n)
	}
}

// A GOAWAY and a WINDOW_UPDATE from the router are written between the
// server's frames, however the server's writes cut them.
func TestRouterFramesGoBetweenTheServers(t *testing.T) {
	srv, cli := net.Pipe()
	defer srv.Close()
	w := &grpcWrites{conn: srv}
	data := func(n int) []byte {
		var b bytes.Buffer
		fr := http2.NewFramer(&b, nil)
		_ = fr.WriteData(1, false, bytes.Repeat([]byte("x"), n))
		return b.Bytes()
	}
	a, b := data(100), data(50)
	got := make(chan []byte, 1)
	go func() {
		var all []byte
		buf := make([]byte, 1024)
		for {
			n, err := cli.Read(buf)
			all = append(all, buf[:n]...)
			if err != nil || len(all) >= len(a)+len(b)+len(goAway(1)) {
				got <- all
				return
			}
		}
	}()
	// The first frame is written in two parts, cut inside its header; the
	// router's frame is queued between them.
	if _, err := w.Write(a[:4]); err != nil {
		t.Fatal(err)
	}
	w.inject(goAway(1))
	time.Sleep(10 * time.Millisecond) // the injector finds no boundary
	if _, err := w.Write(a[4:]); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(b); err != nil {
		t.Fatal(err)
	}
	all := <-got
	fr := http2.NewFramer(nil, bytes.NewReader(all))
	var types []http2.FrameType
	for {
		f, err := fr.ReadFrame()
		if err != nil {
			break
		}
		types = append(types, f.Header().Type)
	}
	want := []http2.FrameType{http2.FrameData, http2.FrameGoAway, http2.FrameData}
	if len(types) != 3 || types[0] != want[0] || types[1] != want[1] || types[2] != want[2] {
		t.Errorf("the client reads frames %v, want %v", types, want)
	}
}

// open opens stream id with the given headers and sends body as one DATA
// frame that leaves the stream open.
func (r *rawH2) open(id uint32, body []byte, kv ...string) {
	r.t.Helper()
	r.writeBlock(id, false, kv...)
	if err := r.fr.WriteData(id, false, body); err != nil {
		r.t.Fatal(err)
	}
}

// trailers ends stream id with a header block of client trailers, and
// returns the block as encoded.
func (r *rawH2) trailers(id uint32, kv ...string) []byte {
	r.t.Helper()
	return r.writeBlock(id, true, kv...)
}

func (r *rawH2) writeBlock(id uint32, end bool, kv ...string) []byte {
	r.t.Helper()
	r.hb.Reset()
	for i := 0; i < len(kv); i += 2 {
		_ = r.enc.WriteField(hpack.HeaderField{Name: kv[i], Value: kv[i+1]})
	}
	block := bytes.Clone(r.hb.Bytes())
	// Split as HTTP/2's default SETTINGS_MAX_FRAME_SIZE has it.
	frag, rest := block, []byte(nil)
	if len(frag) > maxFrame {
		frag, rest = block[:maxFrame], block[maxFrame:]
	}
	if err := r.fr.WriteHeaders(http2.HeadersFrameParam{StreamID: id, BlockFragment: frag,
		EndHeaders: len(rest) == 0, EndStream: end}); err != nil {
		r.t.Fatal(err)
	}
	for len(rest) > 0 {
		frag, rest = rest, nil
		if len(frag) > maxFrame {
			frag, rest = frag[:maxFrame], frag[maxFrame:]
		}
		if err := r.fr.WriteContinuation(id, len(rest) == 0, frag); err != nil {
			r.t.Fatal(err)
		}
	}
	return block
}

// #981, on the wire: the blocks the router keeps from grpc-go (client
// trailers, a refused REST request) add entries to the client's HPACK
// table that grpc-go's decoder never sees; later blocks that name those
// entries still reach grpc-go as blocks it can decode. On one connection:
// a gRPC call; a call ended by client trailers that index a new entry; a
// call whose headers name that entry; a REST request, refused, that
// indexes another; client trailers naming it on the call still open; and
// a gRPC call after the GOAWAY, which is not answered there and is served
// when retried on a new connection. Every gRPC call succeeds, and the
// connection lives on (a PING is answered) with no GOAWAY but the router's.
// Without the re-encoding, grpc-go's decoder fails on stream 5's headers
// (COMPRESSION_ERROR) and closes the connection; without the trailers
// rewritten, grpc-go closes it on stream 3's (PROTOCOL_ERROR).
func TestHPACKStaysInStepOnAMixedConnection(t *testing.T) {
	up := newRESTUpstream(t)
	fx := newFixtureREST(t, up.addr())
	topic := fx.topic(t, "hpack")
	get := grpcBody(t, &pubsubpb.GetTopicRequest{Topic: topic})
	call := append(grpcHeaders("/google.pubsub.v1.Publisher/GetTopic"),
		"x-goog-request-params", "topic="+topic, "x-call", "same on every call")

	r := dialRawH2(t, fx.addr)
	r.request(1, get, call...)
	// Stream 3: ended by trailers that index x-trailer in the client's
	// table; grpc-go never sees them.
	r.open(3, get, call...)
	r.trailers(3, "x-trailer", "t")
	// Stream 5: its headers name x-trailer, and every entry stream 1
	// added; a large value makes the block, and grpc-go's copy of it, take
	// CONTINUATION frames.
	big := strings.Repeat("0123456789abcdef", 2*maxFrame/16)
	r.open(5, get, append(call, "x-trailer", "t", "x-big", big)...)
	// Stream 7: REST, refused; it indexes x-rest.
	body := []byte(`{"labels":{"k":"v"}}`)
	r.request(7, body, ":method", "PUT", ":scheme", "http", ":path", "/v1/projects/"+project+"/topics/rest",
		":authority", "front", "content-type", "application/json", "x-rest", "r")
	// Stream 5 ends with trailers naming the entry only the refused
	// block added.
	if tb := r.trailers(5, "x-rest", "r"); len(tb) != 1 || tb[0]&0x80 == 0 {
		t.Fatalf("the trailers block is %x, want one indexed field", tb)
	}
	// Stream 9: gRPC, reusing the table, above the GOAWAY's last stream.
	r.request(9, get, call...)
	if err := r.fr.WritePing(false, [8]byte{9, 8, 1}); err != nil {
		t.Fatal(err)
	}

	var goaways []*http2.GoAwayFrame
	ended := map[uint32]bool{}
	pong := false
	status, grpcStatus := r.read(func(f http2.Frame) bool {
		switch f := f.(type) {
		case *http2.GoAwayFrame:
			goaways = append(goaways, f)
		case *http2.PingFrame:
			pong = pong || f.IsAck()
		case *http2.HeadersFrame:
			ended[f.StreamID] = ended[f.StreamID] || f.StreamEnded()
		case *http2.DataFrame:
			ended[f.StreamID] = ended[f.StreamID] || f.StreamEnded()
		}
		if f.Header().StreamID > 5 {
			t.Errorf("a refused stream was answered: %v", f)
		}
		return pong && ended[1] && ended[3] && ended[5]
	})
	for _, id := range []uint32{1, 3, 5} {
		if status[id] != "200" || grpcStatus[id] != "0" {
			t.Errorf("gRPC on stream %d = %q, grpc-status %q; want 200, 0", id, status[id], grpcStatus[id])
		}
	}
	if len(goaways) != 1 || goaways[0].LastStreamID != 5 || goaways[0].ErrCode != http2.ErrCodeNo {
		for _, g := range goaways {
			t.Logf("GOAWAY last %d %v %q", g.LastStreamID, g.ErrCode, g.DebugData())
		}
		t.Fatalf("got %d GOAWAYs, want the router's one, last stream 5, NO_ERROR", len(goaways))
	}

	// Stream 9's call, retried on a new connection, as RFC 9113 lets a
	// client do with a stream above the last ID.
	r2 := dialRawH2(t, fx.addr)
	r2.request(1, get, call...)
	status, grpcStatus = r2.read(func(f http2.Frame) bool {
		h, ok := f.(*http2.HeadersFrame)
		return ok && h.StreamID == 1 && h.StreamEnded()
	})
	if status[1] != "200" || grpcStatus[1] != "0" {
		t.Errorf("the retried call = %v %v", status, grpcStatus)
	}
	if n := len(up.calls()); n != 0 {
		t.Errorf("the REST upstream saw %d calls, want none", n)
	}
}
