package bigqueryfront

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// engineIdleTimeout is how long the engine's server keeps an idle
// connection open: its http.Server's IdleTimeout (patch 0010,
// third_party/bigquery-emulator, #1192). TestEngineIdleTimeoutMatchesPatch
// keeps the two in step.
const engineIdleTimeout = 15 * time.Second

// idleConnTimeout is how long the front keeps an idle connection to the
// engine: well below engineIdleTimeout, so the front always closes an idle
// connection before the engine does (#1626).
//
// Kept longer, a connection the engine had just closed could carry the
// front's next request: the request went out, the answer was a reset or
// EOF, and the front answered the client 502. The client then retried
// jobs.insert, and an insert the engine had in fact run ran twice (#1622).
// Go's transport cannot tell that case from an engine that took the
// request and died, so it does not retry a POST there, and neither does
// the front; it is avoided instead. The engine's timer starts when it
// finishes writing an answer, the front's only once it has read it, so the
// front's always ends later than its length: a third leaves room for that.
const idleConnTimeout = engineIdleTimeout / 3

// maxReplayBody bounds the request body the front holds whole so that Go's
// transport can send it again (replayable). A jobs.insert, a query or a
// tabledata.insertAll is far smaller; a larger body, such as a load's
// upload, is streamed as before.
const maxReplayBody = 1 << 20

// engineTransport is the transport from the front to the engine on the
// pod's loopback: idle connections closed after idle, dialed with dial
// (nil: the default dialer), and each request with a small body made
// replayable.
func engineTransport(idle time.Duration, dial func(ctx context.Context, network, addr string) (net.Conn, error)) http.RoundTripper {
	t := &http.Transport{
		// The emulator's bodies pass through as it wrote them.
		DisableCompression:  true,
		MaxIdleConnsPerHost: 32,
		IdleConnTimeout:     idle,
		DialContext:         dial,
	}
	if dial == nil {
		t.DialContext = (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	}
	return replayable{next: t}
}

// replayable gives a request whose body is small a GetBody, so that Go's
// transport applies its safe-retry rules to it as it does to a GET: it
// sends the request again on another connection when the one it chose
// failed before any byte of the request was written, or had been closed by
// the engine before the request went out (net/http's shouldRetryRequest).
// In both the engine cannot have seen the request. ReverseProxy's outgoing
// request has the client's body and no GetBody, so without this a POST on
// a connection the engine had reset was answered 502 even then (#1626).
type replayable struct{ next http.RoundTripper }

func (t replayable) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.GetBody != nil || r.Body == nil || r.Body == http.NoBody || r.ContentLength <= 0 || r.ContentLength > maxReplayBody {
		return t.next.RoundTrip(r)
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, r.ContentLength+1))
	_ = r.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("reading the request body: %w", err)
	}
	if int64(len(body)) != r.ContentLength {
		return nil, fmt.Errorf("the request body is %d bytes, not the %d its Content-Length says", len(body), r.ContentLength)
	}
	out := r.Clone(r.Context())
	out.Body = io.NopCloser(bytes.NewReader(body))
	out.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
	return t.next.RoundTrip(out)
}
