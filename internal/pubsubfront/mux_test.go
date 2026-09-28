package pubsubfront

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
	"google.golang.org/protobuf/proto"
)

// headerBlock is an HPACK block of the given pairs.
func headerBlock(t *testing.T, kv ...string) []byte {
	t.Helper()
	var b bytes.Buffer
	enc := hpack.NewEncoder(&b)
	for i := 0; i < len(kv); i += 2 {
		if err := enc.WriteField(hpack.HeaderField{Name: kv[i], Value: kv[i+1]}); err != nil {
			t.Fatal(err)
		}
	}
	return b.Bytes()
}

// classifyBytes runs classify on a connection whose client sends in, and
// returns the choice, everything the chosen server reads, and what the
// client was sent.
func classifyBytes(t *testing.T, in []byte) (destination, []byte, []byte) {
	t.Helper()
	srv, cli := net.Pipe()
	defer cli.Close()
	sent := make(chan []byte, 1)
	go func() {
		// The preface, then the router's SETTINGS is read before the rest
		// is written, as net.Pipe does not buffer.
		_, _ = cli.Write(in[:len(clientPreface)])
		b := make([]byte, len(emptySettings))
		_, _ = io.ReadFull(cli, b)
		sent <- b
		_, _ = cli.Write(in[len(clientPreface):])
		_ = cli.Close()
	}()
	to, conn := classify(srv)
	if conn == nil {
		t.Fatal("classify closed the connection")
	}
	got, _ := io.ReadAll(conn)
	return to, got, <-sent
}

func frameBytes(t *testing.T, write func(*http2.Framer) error) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := write(http2.NewFramer(&b, nil)); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// A connection's first request decides, whatever frames come before it, and
// whether its header block is split, padded or prioritised; the chosen server
// reads every byte the client sent but the acknowledgement of the router's
// SETTINGS, wherever it comes.
func TestClassifyByTheFirstRequest(t *testing.T) {
	settings := frameBytes(t, func(fr *http2.Framer) error {
		return fr.WriteSettings(http2.Setting{ID: http2.SettingInitialWindowSize, Val: 1 << 20})
	})
	window := frameBytes(t, func(fr *http2.Framer) error { return fr.WriteWindowUpdate(0, 1<<20) })
	ack := frameBytes(t, func(fr *http2.Framer) error { return fr.WriteSettingsAck() })
	grpcBlock := headerBlock(t, ":method", "POST", ":scheme", "http", ":path", "/google.pubsub.v1.Publisher/Publish",
		":authority", "front", "content-type", "application/grpc", "te", "trailers")
	restBlock := headerBlock(t, ":method", "GET", ":scheme", "http", ":path", "/v1/projects/p/topics", ":authority", "front")
	whole := func(block []byte) []byte {
		return frameBytes(t, func(fr *http2.Framer) error {
			return fr.WriteHeaders(http2.HeadersFrameParam{StreamID: 1, BlockFragment: block, EndHeaders: true, EndStream: true})
		})
	}
	split := frameBytes(t, func(fr *http2.Framer) error {
		if err := fr.WriteHeaders(http2.HeadersFrameParam{StreamID: 1, BlockFragment: grpcBlock[:5], PadLength: 7,
			Priority: http2.PriorityParam{Weight: 15}}); err != nil {
			return err
		}
		if err := fr.WriteContinuation(1, false, grpcBlock[5:9]); err != nil {
			return err
		}
		return fr.WriteContinuation(1, true, grpcBlock[9:])
	})
	data := frameBytes(t, func(fr *http2.Framer) error { return fr.WriteData(1, true, []byte("body")) })
	cat := func(parts ...[]byte) []byte { return bytes.Join(parts, nil) }
	pre := []byte(clientPreface)
	for _, c := range []struct {
		name     string
		in, want []byte
		to       destination
	}{
		{"gRPC, acknowledged first", cat(pre, settings, window, ack, whole(grpcBlock), data),
			cat(pre, settings, window, whole(grpcBlock), data), toGRPC},
		{"gRPC, acknowledged after the request", cat(pre, settings, whole(grpcBlock), data, ack, ack),
			cat(pre, settings, whole(grpcBlock), data, ack), toGRPC},
		{"gRPC, split, padded and prioritised", cat(pre, settings, ack, split, data),
			cat(pre, settings, split, data), toGRPC},
		{"REST", cat(pre, settings, ack, whole(restBlock)), cat(pre, settings, whole(restBlock)), toHTTP},
		{"no request", cat(pre, settings), cat(pre, settings), toHTTP},
	} {
		t.Run(c.name, func(t *testing.T) {
			to, got, sent := classifyBytes(t, c.in)
			if !bytes.Equal(sent, emptySettings) {
				t.Errorf("the client was sent %x, want an empty SETTINGS", sent)
			}
			if to != c.to {
				t.Errorf("routed to %v, want %v", to, c.to)
			}
			if !bytes.Equal(got, c.want) {
				t.Errorf("the server reads\n%x\nwant\n%x", got, c.want)
			}
		})
	}
}

// HTTP/1.1 is routed to net/http before anything is written to it, even a
// request shorter than the HTTP/2 preface.
func TestClassifyHTTP1(t *testing.T) {
	srv, cli := net.Pipe()
	defer cli.Close()
	req := "GET / HTTP/1.1\r\n\r\n"
	go func() { _, _ = cli.Write([]byte(req)) }()
	to, conn := classify(srv)
	if to != toHTTP || conn == nil {
		t.Fatalf("an HTTP/1.1 request routed to %v (conn %v)", to, conn)
	}
	b := make([]byte, len(req))
	if _, err := io.ReadFull(conn, b); err != nil || string(b) != req {
		t.Errorf("net/http reads %q, %v; want the request", b, err)
	}
}

// grpcOverH2C sends a raw unary gRPC call on c, the h2c client.
func grpcOverH2C(t *testing.T, c *http.Client, addr, method string, m proto.Message) (*http.Response, []byte) {
	t.Helper()
	msg, err := proto.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	framed := append([]byte{0, 0, 0, 0, 0}, msg...)
	binary.BigEndian.PutUint32(framed[1:5], uint32(len(msg)))
	req, _ := http.NewRequest(http.MethodPost, "http://"+addr+method, bytes.NewReader(framed))
	req.Header.Set("Content-Type", "application/grpc")
	req.Header.Set("Te", "trailers")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp, b
}

// #950: a connection whose first request is gRPC is served by grpc-go's own
// transport, which answers a later non-gRPC request on it 415, as it did
// before #909; net/http's, which the connection whose first request is REST
// gets (TestOneHTTP2ConnectionCarriesGRPCAndREST), would have served it.
// Go's HTTP/2 client sends its request before it reads the server's
// SETTINGS, so the router's is acknowledged after it (ackFilter).
func TestAConnectionOpenedByGRPCIsGRPCGos(t *testing.T) {
	up := newRESTUpstream(t)
	fx := newFixtureREST(t, up.addr())
	topic := fx.topic(t, "native")
	c := h2cClient()
	resp, b := grpcOverH2C(t, c, fx.addr, "/google.pubsub.v1.Publisher/GetTopic", &pubsubpb.GetTopicRequest{Topic: topic})
	var got pubsubpb.Topic
	if resp.Trailer.Get("Grpc-Status") != "0" || len(b) < 5 || proto.Unmarshal(b[5:], &got) != nil || got.GetName() != topic {
		t.Fatalf("gRPC GetTopic = status %q, %d bytes; want %s", resp.Trailer.Get("Grpc-Status"), len(b), topic)
	}
	// A second call on the same connection: the transport is healthy after
	// the acknowledgement it never sent was taken out.
	resp, _ = grpcOverH2C(t, c, fx.addr, "/google.pubsub.v1.Publisher/GetTopic", &pubsubpb.GetTopicRequest{Topic: topic})
	if resp.Trailer.Get("Grpc-Status") != "0" {
		t.Fatalf("a second gRPC call = status %q", resp.Trailer.Get("Grpc-Status"))
	}
	rest, err := c.Get("http://" + fx.addr + "/v1/projects/" + project + "/topics")
	if err != nil {
		t.Fatal(err)
	}
	_ = rest.Body.Close()
	if rest.StatusCode != http.StatusUnsupportedMediaType {
		t.Errorf("REST on a connection gRPC opened = %d, want grpc-go's 415", rest.StatusCode)
	}
	if n := len(up.calls()); n != 0 {
		t.Errorf("the REST upstream saw %d calls, want none", n)
	}
}

// #950: an HTTP/1.1 request asking to switch to h2c is answered 101, as
// stream 1 of the HTTP/2 connection it becomes, which then carries a gRPC
// call too.
func TestUpgradeToH2C(t *testing.T) {
	up := newRESTUpstream(t)
	fx := newFixtureREST(t, up.addr())
	topic := fx.topic(t, "upgraded")
	c, err := net.DialTimeout("tcp", fx.addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	_, err = io.WriteString(c, "GET /v1/projects/"+project+"/topics HTTP/1.1\r\nHost: front\r\n"+
		"Connection: Upgrade, HTTP2-Settings\r\nUpgrade: h2c\r\nHTTP2-Settings: \r\n\r\n")
	if err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols || !strings.EqualFold(resp.Header.Get("Upgrade"), "h2c") {
		t.Fatalf("the upgrade = %d %v, want 101 to h2c", resp.StatusCode, resp.Header)
	}
	if _, err := io.WriteString(c, clientPreface); err != nil {
		t.Fatal(err)
	}
	fr := http2.NewFramer(c, br)
	if err := fr.WriteSettings(); err != nil {
		t.Fatal(err)
	}
	// Stream 3: a gRPC GetTopic on the upgraded connection.
	var hb bytes.Buffer
	enc := hpack.NewEncoder(&hb)
	for _, kv := range [][2]string{{":method", "POST"}, {":scheme", "http"}, {":path", "/google.pubsub.v1.Publisher/GetTopic"},
		{":authority", "front"}, {"content-type", "application/grpc"}, {"te", "trailers"}} {
		_ = enc.WriteField(hpack.HeaderField{Name: kv[0], Value: kv[1]})
	}
	msg, _ := proto.Marshal(&pubsubpb.GetTopicRequest{Topic: topic})
	framed := append([]byte{0, 0, 0, 0, 0}, msg...)
	binary.BigEndian.PutUint32(framed[1:5], uint32(len(msg)))
	if err := fr.WriteHeaders(http2.HeadersFrameParam{StreamID: 3, BlockFragment: hb.Bytes(), EndHeaders: true}); err != nil {
		t.Fatal(err)
	}
	if err := fr.WriteData(3, true, framed); err != nil {
		t.Fatal(err)
	}
	dec := hpack.NewDecoder(4096, nil)
	status := map[uint32]string{}
	body := map[uint32][]byte{}
	grpcStatus := ""
	for done := 0; done < 2; {
		f, err := fr.ReadFrame()
		if err != nil {
			t.Fatalf("read the upgraded connection: %v (statuses %v)", err, status)
		}
		switch f := f.(type) {
		case *http2.SettingsFrame:
			if !f.IsAck() {
				_ = fr.WriteSettingsAck()
			}
		case *http2.HeadersFrame:
			fields, err := dec.DecodeFull(f.HeaderBlockFragment())
			if err != nil {
				t.Fatal(err)
			}
			for _, h := range fields {
				switch h.Name {
				case ":status":
					status[f.StreamID] = h.Value
				case "grpc-status":
					grpcStatus = h.Value
				}
			}
			if f.StreamEnded() {
				done++
			}
		case *http2.DataFrame:
			body[f.StreamID] = append(body[f.StreamID], f.Data()...)
			if f.StreamEnded() {
				done++
			}
		}
	}
	if status[1] != "200" || !bytes.Contains(body[1], []byte("upstream")) {
		t.Errorf("stream 1, the upgraded request = %s %q; want the REST upstream's answer", status[1], body[1])
	}
	var got pubsubpb.Topic
	if status[3] != "200" || grpcStatus != "0" || len(body[3]) < 5 || proto.Unmarshal(body[3][5:], &got) != nil || got.GetName() != topic {
		t.Errorf("stream 3, gRPC GetTopic = %s, grpc-status %q, %d bytes; want %s", status[3], grpcStatus, len(body[3]), topic)
	}
}

// A request that asks for h2c with a malformed HTTP2-Settings is served
// over HTTP/1.1, as though it had not asked.
func TestAMalformedUpgradeIsServedOverHTTP1(t *testing.T) {
	up := newRESTUpstream(t)
	fx := newFixtureREST(t, up.addr())
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://"+fx.addr+"/v1/projects/"+project+"/topics", nil)
	req.Header.Set("Connection", "Upgrade, HTTP2-Settings")
	req.Header.Set("Upgrade", "h2c")
	req.Header.Set("HTTP2-Settings", "not base64!")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.ProtoMajor != 1 || !bytes.Contains(b, []byte("upstream")) {
		t.Errorf("= HTTP/%d %d %s; want the upstream's answer over HTTP/1.1", resp.ProtoMajor, resp.StatusCode, b)
	}
}
