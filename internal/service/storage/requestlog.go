package storage

import (
	"crypto/rand"
	"encoding/hex"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/resource"
)

// The request log (#1979): one Request per served request, the single
// record Cloud Storage's Data Access and Admin Activity audit log entries
// and the Storage Insights activity views are both made from, on the host.
//
// The hot path pays for a byte count, a permission lookup and an append
// to a bounded buffer under a mutex; it never wakes another goroutine. A
// worker takes the buffer every requestDrainInterval (and a read of the log
// takes it first), resolves each bucket's project, number and location
// (cached) and appends the records to a bounded ring the host reads at
// GET /_cloudburrow/events?requests=1&after=N, as it reads the calls. When
// the buffer is full a record is dropped and counted, never waited for: a
// request is never slowed by its own log. Admin paths (/_cloudburrow/...)
// are not logged.

// Request is one served Cloud Storage request.
type Request struct {
	Seq  uint64    `json:"seq"`
	Time time.Time `json:"time"` // when the response was sent
	// Method is the JSON API method (storage.objects.get); an XML API or
	// gRPC request is named by the JSON method it is served by.
	Method string `json:"method"`
	// Permissions are those the request is checked for, which say which
	// audit log type it writes (Google: "generated based on the IAM
	// permissions checked during a request").
	Permissions []string `json:"permissions,omitempty"`
	Bucket      string   `json:"bucket,omitempty"`
	Object      string   `json:"object,omitempty"`
	// DestBucket and DestObject are a copy's or rewrite's destination.
	DestBucket       string `json:"destBucket,omitempty"`
	DestObject       string `json:"destObject,omitempty"`
	Project          string `json:"project,omitempty"`
	ProjectNumber    int64  `json:"projectNumber,omitempty"`
	Location         string `json:"location,omitempty"`
	BucketGeneration int64  `json:"bucketGeneration,omitempty"`
	Status           int    `json:"status"`
	DurationMicros   int64  `json:"durationMicros"`
	RequestBytes     int64  `json:"requestBytes"`
	ResponseBytes    int64  `json:"responseBytes"`
	RequestID        string `json:"requestId"`
	Host             string `json:"host,omitempty"`
	HTTPMethod       string `json:"httpMethod"`
	Referrer         string `json:"referrer,omitempty"`
	UserAgent        string `json:"userAgent,omitempty"`
	CallerIP         string `json:"callerIp,omitempty"`
	// Surface is "json", "xml" or "grpc".
	Surface string `json:"surface"`
}

const (
	// requestQueueSize bounds the records waiting for the worker, and
	// requestRingSize those kept for the host to read.
	requestQueueSize = 8192
	requestRingSize  = 16384
	// requestDrainInterval is how often the worker takes the buffer.
	requestDrainInterval = 100 * time.Millisecond
	// bucketInfoTTL is how long a bucket's project and location are reused.
	bucketInfoTTL = 5 * time.Second
)

type bucketInfo struct {
	project    string
	number     int64
	location   string
	generation int64
	at         time.Time
}

// requestLog is the server's request log.
type requestLog struct {
	once    sync.Once
	started sync.Once
	stop    chan struct{}
	dropped atomic.Uint64
	nonce   string

	// qmu guards queue, the records the worker has not taken.
	qmu   sync.Mutex
	queue []Request

	// mu guards the ring, and the worker's work: seq, buckets.
	mu      sync.Mutex
	seq     uint64
	ring    []Request
	buckets map[string]bucketInfo
}

func (l *requestLog) init() {
	l.once.Do(func() {
		l.stop = make(chan struct{})
		l.buckets = map[string]bucketInfo{}
		var b [4]byte
		_, _ = rand.Read(b[:])
		l.nonce = hex.EncodeToString(b[:])
	})
}

// logRequest buffers one record without waiting.
func (s *Server) logRequest(rq Request) {
	l := &s.requests
	l.init()
	l.started.Do(func() { go s.requestWorker() })
	l.qmu.Lock()
	if len(l.queue) < requestQueueSize {
		l.queue = append(l.queue, rq)
		l.qmu.Unlock()
		return
	}
	l.qmu.Unlock()
	l.dropped.Add(1)
}

func (s *Server) stopRequestLog() {
	l := &s.requests
	l.init()
	l.mu.Lock()
	defer l.mu.Unlock()
	select {
	case <-l.stop:
	default:
		close(l.stop)
	}
}

func (s *Server) requestWorker() {
	t := time.NewTicker(requestDrainInterval)
	defer t.Stop()
	for {
		select {
		case <-s.requests.stop:
			return
		case <-t.C:
			s.drainRequests()
		}
	}
}

// drainRequests moves the buffered records to the ring.
func (s *Server) drainRequests() {
	l := &s.requests
	l.qmu.Lock()
	batch := l.queue
	l.queue = make([]Request, 0, min(len(batch), requestQueueSize))
	l.qmu.Unlock()
	if len(batch) == 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, rq := range batch {
		s.enrich(&rq)
		l.seq++
		rq.Seq = l.seq
		rq.RequestID = l.nonce + strconv.FormatUint(l.seq, 36)
		l.ring = append(l.ring, rq)
	}
	if len(l.ring) > requestRingSize {
		l.ring = append(l.ring[:0], l.ring[len(l.ring)-requestRingSize/2:]...)
	}
}

// enrich fills a record's project, number and location from its bucket.
func (s *Server) enrich(rq *Request) {
	if rq.Bucket == "" {
		if rq.Project != "" {
			rq.ProjectNumber = resource.ProjectNumber(rq.Project)
		}
		return
	}
	l := &s.requests
	now := time.Now()
	info, ok := l.buckets[rq.Bucket]
	if !ok || now.Sub(info.at) > bucketInfoTTL {
		var b bucketRecord
		var found bool
		_ = s.meta.View(func(tx Tx) error {
			var err error
			b, found, err = s.getBucket(tx, rq.Bucket)
			return err
		})
		if found {
			loc, _ := b.Fields["location"].(string)
			info = bucketInfo{project: b.Project, location: loc, generation: b.Generation, at: now}
			info.number = resource.ProjectNumber(b.Project)
			if len(l.buckets) > 10000 {
				clear(l.buckets)
			}
			l.buckets[rq.Bucket] = info
		} else if ok {
			// A deleted bucket keeps the project it had.
			info.at = now
			l.buckets[rq.Bucket] = info
		}
	}
	rq.Project, rq.ProjectNumber, rq.Location, rq.BucketGeneration = info.project, info.number, info.location, info.generation
}

// requestsAfter is the kept records after seq n, and how many were dropped.
func (s *Server) requestsAfter(n uint64) ([]Request, uint64) {
	l := &s.requests
	l.init()
	s.drainRequests()
	l.mu.Lock()
	defer l.mu.Unlock()
	out := []Request{}
	for _, rq := range l.ring {
		if rq.Seq > n {
			out = append(out, rq)
		}
	}
	return out, l.dropped.Load()
}

// noteBucket tells the request log the bucket a request names in its body
// (buckets.insert), which its path does not.
func noteBucket(w http.ResponseWriter, name string) {
	switch rec := w.(type) {
	case *statusRecorder:
		rec.bucket = name
	case *grpcRecorder:
		rec.bucket = name
	}
}

// countingBody counts the bytes read from a request body.
type countingBody struct {
	io.ReadCloser
	n int64
}

func (c *countingBody) Read(p []byte) (int, error) {
	n, err := c.ReadCloser.Read(p)
	c.n += int64(n)
	return n, err
}

// requestPermissions are the permissions a request is checked for.
func (s *Server) requestPermissions(r *http.Request, method, bucket, object string) []string {
	if strings.HasPrefix(method, "xml.") {
		_, p := xmlPermission(r, bucket, object)
		if p == "" {
			return nil
		}
		return []string{p}
	}
	switch method {
	case "storage.objects.copy", "storage.objects.rewrite", "storage.objects.compose":
		// Google: copying and composing objects each write two entries,
		// the read of the source and the write of the destination.
		return []string{"storage.objects.get", "storage.objects.create"}
	case "storage.objects.move":
		return []string{"storage.objects.move"}
	}
	return jsonPermissions[method]
}

// xmlMethod is the JSON method an XML API request is served as.
func xmlMethod(perm, httpMethod string) string {
	switch perm {
	case "storage.objects.get":
		return "storage.objects.get"
	case "storage.objects.list":
		return "storage.objects.list"
	case "storage.objects.create":
		return "storage.objects.insert"
	case "storage.objects.delete":
		return "storage.objects.delete"
	case "storage.buckets.list":
		return "storage.buckets.list"
	case "storage.buckets.get":
		return "storage.buckets.get"
	case "storage.buckets.create":
		return "storage.buckets.insert"
	case "storage.buckets.update":
		return "storage.buckets.update"
	case "storage.buckets.delete":
		return "storage.buckets.delete"
	case "storage.buckets.getIamPolicy", "storage.buckets.setIamPolicy", "storage.objects.getIamPolicy", "storage.objects.setIamPolicy":
		return perm
	}
	return "xml." + httpMethod
}

// newRequest is the record of one served request, before the worker fills
// its bucket's project.
func (s *Server) newRequest(r *http.Request, surface, method, bucket, object string, status int, reqBytes, respBytes int64, start, end time.Time) Request {
	perms := s.requestPermissions(r, method, bucket, object)
	if surface == "xml" && len(perms) == 1 {
		method = xmlMethod(perms[0], r.Method)
	}
	rq := Request{Time: end, Method: method, Permissions: perms, Bucket: bucket, Object: object, Status: status,
		DurationMicros: end.Sub(start).Microseconds(), RequestBytes: reqBytes, ResponseBytes: respBytes, Host: r.Host,
		HTTPMethod: r.Method, Referrer: r.Referer(), UserAgent: r.UserAgent(), Surface: surface}
	if ip, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		rq.CallerIP = ip
	}
	switch method {
	case "storage.objects.copy", "storage.objects.rewrite":
		rq.DestBucket, rq.DestObject = pathVar(r, jsonPrefix, 6), pathVar(r, jsonPrefix, 8)
	case "storage.buckets.insert", "storage.buckets.list", "storage.projects.hmacKeys.list", "storage.projects.hmacKeys.create":
		rq.Project = r.URL.Query().Get("project")
	}
	if rq.Project == "" && strings.HasPrefix(method, "storage.projects.") {
		rq.Project = pathVar(r, jsonPrefix, 1)
	}
	if surface == "xml" && bucket == "" {
		rq.Project = r.Header.Get("X-Goog-Project-Id")
	}
	return rq
}
