package storage

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
)

// The batch endpoint (#496), POST /batch/storage/v1
// (docs.cloud.google.com/storage/docs/batch): a multipart/mixed body whose
// parts are application/http requests, each dispatched through the same
// handler as the direct call and answered in a part of its own, in order,
// with Content-ID response-<id>. Batches carry metadata calls only, so an
// upload or a download inside one is refused, per part.

// maxBatchCalls is Google's documented limit of 100 calls per batch. How
// Google refuses a larger batch is not stated, so the 400 here is UNVERIFIED.
const maxBatchCalls = 100

func (s *Server) serveBatch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, badRequest("The batch endpoint takes POST"))
		return
	}
	mt, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || !strings.HasPrefix(mt, "multipart/") || params["boundary"] == "" {
		writeError(w, badRequest("A batch request is a multipart/mixed body with a boundary"))
		return
	}
	type call struct {
		id  string
		req *http.Request
		err *httpError
	}
	var calls []call
	mr := multipart.NewReader(r.Body, params["boundary"])
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			writeError(w, badRequest("The batch body is not valid multipart: %v", err))
			return
		}
		if len(calls) == maxBatchCalls {
			writeError(w, badRequest("A batch request takes at most %d calls", maxBatchCalls))
			return
		}
		c := call{id: part.Header.Get("Content-ID")}
		raw, _ := io.ReadAll(io.LimitReader(part, 1<<20))
		// A part with no body may end at its headers, without the blank line
		// an HTTP request needs; supply it.
		if !bytes.Contains(raw, []byte("\r\n\r\n")) && !bytes.Contains(raw, []byte("\n\n")) {
			raw = append(bytes.TrimRight(raw, "\r\n"), []byte("\r\n\r\n")...)
		}
		br := bufio.NewReader(bytes.NewReader(raw))
		inner, perr := http.ReadRequest(br)
		if perr != nil {
			c.err = badRequest("A batch part is not an HTTP request: %v", perr)
		} else {
			inner.RemoteAddr, inner.Host = r.RemoteAddr, r.Host
			if inner.URL.Host != "" {
				inner.URL.Host, inner.URL.Scheme = "", ""
			}
			path := inner.URL.EscapedPath()
			switch {
			case strings.HasPrefix(path, uploadPrefix), strings.HasPrefix(path, resumablePrefix):
				c.err = badRequest("An upload cannot be made inside a batch request")
			case strings.HasPrefix(path, downloadPrefix), inner.URL.Query().Get("alt") == "media":
				c.err = badRequest("A download cannot be made inside a batch request")
			case !strings.HasPrefix(path, jsonPrefix):
				c.err = badRequest("A batch part must be a JSON API request, not %s", path)
			}
			body, _ := io.ReadAll(inner.Body)
			// The Python client (batch.py MIMEApplicationHTTP) sends a part's
			// body with no Content-Length, which http.ReadRequest reads as no
			// body; the body is then the rest of the part.
			if inner.Header.Get("Content-Length") == "" && len(inner.TransferEncoding) == 0 {
				rest, _ := io.ReadAll(br)
				body = append(body, bytes.TrimRight(rest, "\r\n")...)
			}
			inner.Body = io.NopCloser(bytes.NewReader(body))
			inner.ContentLength = int64(len(body))
		}
		c.req = inner
		calls = append(calls, c)
	}
	var buf bytes.Buffer
	boundary := batchBoundary()
	mw := multipart.NewWriter(&buf)
	_ = mw.SetBoundary(boundary)
	for _, c := range calls {
		rec := httptest.NewRecorder()
		if c.err != nil {
			writeError(rec, c.err)
		} else {
			s.ServeHTTP(rec, c.req.WithContext(r.Context()))
		}
		hdr := textproto.MIMEHeader{"Content-Type": {"application/http"}}
		if c.id != "" {
			hdr.Set("Content-ID", responseID(c.id))
		}
		pw, _ := mw.CreatePart(hdr)
		resp := rec.Result()
		fmt.Fprintf(pw, "HTTP/1.1 %d %s\r\n", resp.StatusCode, http.StatusText(resp.StatusCode))
		body := rec.Body.Bytes()
		resp.Header.Set("Content-Length", fmt.Sprint(len(body)))
		_ = resp.Header.Write(pw)
		_, _ = io.WriteString(pw, "\r\n")
		_, _ = pw.Write(body)
	}
	_ = mw.Close()
	w.Header().Set("Content-Type", "multipart/mixed; boundary="+boundary)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf.Bytes())
}

// responseID is a request part's Content-ID as the response names it:
// <x> answers <response-x>, and a bare x answers response-x.
func responseID(id string) string {
	if strings.HasPrefix(id, "<") && strings.HasSuffix(id, ">") {
		return "<response-" + id[1:len(id)-1] + ">"
	}
	return "response-" + id
}

func batchBoundary() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "batch_" + hex.EncodeToString(b)
}
