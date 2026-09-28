package pubsubfront

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"net"
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
