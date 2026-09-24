package main

import (
	"bytes"
	"io"
	"sync"
	"time"
)

// stampedWriter prefixes each line with an RFC 3339 UTC timestamp, in the
// same form kubectl's --timestamps uses, so `cloudburrow logs` reads the
// in-process services' output and a pod's with one parser, and --since
// applies to both.
//
// Tasks, Secret Manager and the metadata server log only to `up`'s own
// output (#280). That output is up.log in the instance directory: written
// directly when detached, and teed there by a foreground `up`.
type stampedWriter struct {
	mu      sync.Mutex
	w       io.Writer
	now     func() time.Time
	partial bool // the last write ended mid-line
}

func newStampedWriter(w io.Writer) *stampedWriter {
	return &stampedWriter{w: w, now: time.Now}
}

func (s *stampedWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(p)
	var out bytes.Buffer
	for len(p) > 0 {
		if !s.partial {
			out.WriteString(s.now().UTC().Format(time.RFC3339Nano))
			out.WriteByte(' ')
		}
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			out.Write(p)
			s.partial = true
			break
		}
		out.Write(p[:i+1])
		p = p[i+1:]
		s.partial = false
	}
	if _, err := s.w.Write(out.Bytes()); err != nil {
		return 0, err
	}
	// The caller's byte count: the stamps are ours, not theirs.
	return n, nil
}
