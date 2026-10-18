package bigqueryfront

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"time"
)

// The front's calls to the instance's other services (#1628): Resource
// Manager's projects.getAncestry (infoschemahierarchy.go), Cloud Storage's
// buckets.get and media uploads (extract.go, extractwrite.go). Each went
// through http.DefaultClient, which has no timeout of its own, under a
// context of 10 to 60 s, and logged nothing.
//
// Resource Manager is served by the cloudburrow process on the developer's
// machine, which pods reach through the cloudburrow-host Service
// (cmd/cloudburrow/clusterhost.go): on Docker Desktop, its forward to the
// host's loopback. That path does not always answer: measured on a fresh
// instance (--services storage,bigquery, Docker Desktop, arm64,
// 2026-10-18), the first two dials of the front's first getAncestry went
// unanswered for 5 s each, and the third connected at once. A
// JOBS_BY_ORGANIZATION query whose getAncestry stalled so waited out the
// call's 30 s, was answered 500 (unlogged), which the BigQuery client
// retries, and its second attempt ran into the client's 60 s deadline:
// TestBigQueryInformationSchemaJobs's "context deadline exceeded".
//
// So each call goes through serviceClient: a dial is bounded by
// serviceDialTimeout and each attempt by serviceAttemptTimeout (and the
// caller's own deadline); an attempt that failed before any answer, or
// was answered 502, 503 or 504, is sent again, on a new connection,
// serviceAttempts in all and serviceBackoff apart, doubled each time; and
// a call that failed, was retried, or took longer than serviceSlow is
// logged with what it asked and how long it took. Every call here is one
// that may be sent again: getAncestry and buckets.get read, and a media
// upload names its object, so a second writes the same one. Cloud
// Storage's object reads (gcsload.go) stream bodies of any size through a
// client of their own, with its own timeout.

// serviceAttempts is how many times a call is sent at most.
const serviceAttempts = 4

// serviceDialTimeout bounds a dial: the instance's services are on the
// cluster's network or the developer's machine, a dial of which takes
// milliseconds when it is answered at all.
const serviceDialTimeout = 3 * time.Second

// serviceAttemptTimeout bounds one attempt of a call, its answer's body
// read whole, and a second more for each MiB of the request's body (an
// extract's file).
var serviceAttemptTimeout = 8 * time.Second

// serviceBackoff is the wait before the second attempt; it doubles after
// each.
var serviceBackoff = 100 * time.Millisecond

// serviceSlow is how long a call takes before it is logged.
var serviceSlow = 2 * time.Second

// serviceBodyMax bounds the answer's body a call reads.
const serviceBodyMax = 1 << 20

// serviceTransport is the transport of serviceClient: its own idle
// connections, closed well before a peer's server would (#1626), and a
// bounded dial.
var serviceTransport = &http.Transport{
	Proxy:                 nil, // the instance's own services, never through a proxy
	DialContext:           (&net.Dialer{Timeout: serviceDialTimeout, KeepAlive: 30 * time.Second}).DialContext,
	MaxIdleConnsPerHost:   8,
	IdleConnTimeout:       idleConnTimeout,
	ResponseHeaderTimeout: 30 * time.Second,
}

// serviceClient is the client of the front's calls to other services.
var serviceClient = &http.Client{Transport: serviceTransport, Timeout: 10 * time.Minute} // each attempt is bounded too

// frontLogf is the front's log (Wrap's logf), for what serviceCall logs.
var frontLogf atomic.Pointer[func(string, ...any)]

// logFront logs to the front's log, when it has one.
func logFront(format string, args ...any) {
	if p := frontLogf.Load(); p != nil {
		(*p)(format, args...)
	}
}

// serviceCall sends method u with body (nil: none) and contentType to
// another service, as above, and returns the status and body of its
// answer, or the error of its last attempt. what names the call in the
// log.
func serviceCall(ctx context.Context, what, method, u, contentType string, body []byte) (int, []byte, error) {
	start := time.Now()
	var status int
	var out []byte
	var err error
	wait := serviceBackoff
	attempt := 0
	for attempt = 1; attempt <= serviceAttempts; attempt++ {
		status, out, err = serviceAttempt(ctx, method, u, contentType, body)
		if err == nil && status != http.StatusBadGateway && status != http.StatusServiceUnavailable && status != http.StatusGatewayTimeout {
			break
		}
		if ctx.Err() != nil || attempt == serviceAttempts {
			break
		}
		logFront("bigquery front: %s: attempt %d of %d failed after %s: %s; trying again",
			what, attempt, serviceAttempts, time.Since(start).Round(time.Millisecond), describeAttempt(status, err))
		// Not on the connection that failed.
		serviceTransport.CloseIdleConnections()
		select {
		case <-ctx.Done():
		case <-time.After(wait):
		}
		wait *= 2
	}
	attempt = min(attempt, serviceAttempts)
	took := time.Since(start).Round(time.Millisecond)
	switch {
	case err != nil:
		logFront("bigquery front: %s failed after %s and %d attempts: %v", what, took, attempt, err)
	case status/100 == 5:
		logFront("bigquery front: %s was answered %d after %s and %d attempts", what, status, took, attempt)
	case took >= serviceSlow || attempt > 1:
		logFront("bigquery front: %s took %s (%d attempts), answered %d", what, took, attempt, status)
	}
	return status, out, err
}

// serviceAttempt is one attempt of serviceCall.
func serviceAttempt(ctx context.Context, method, u, contentType string, body []byte) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, serviceAttemptTimeout+time.Duration(len(body)>>20)*time.Second)
	defer cancel()
	var rd io.Reader = http.NoBody
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return 0, nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := serviceClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(io.LimitReader(resp.Body, serviceBodyMax))
	if err != nil {
		return 0, nil, fmt.Errorf("reading the answer (%d): %w", resp.StatusCode, err)
	}
	return resp.StatusCode, out, nil
}

// describeAttempt is a failed attempt's status or error, for the log.
func describeAttempt(status int, err error) string {
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return "no answer in time"
		}
		return err.Error()
	}
	return fmt.Sprintf("answered %d", status)
}
