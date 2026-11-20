package compute

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
)

// serveBatch answers a batch request (multipart/mixed of application/http
// parts, https://cloud.google.com/compute/docs/api/how-tos/batch), which
// gcloud compute sends its calls as: each part is served as its own
// request, and answered in a part with the same Content-ID.
func (s *Server) serveBatch(w http.ResponseWriter, r *http.Request) {
	mt, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || !strings.HasPrefix(mt, "multipart/") || params["boundary"] == "" {
		writeError(w, invalid("A batch request is multipart/mixed."))
		return
	}
	mr := multipart.NewReader(r.Body, params["boundary"])
	var out bytes.Buffer
	mw := multipart.NewWriter(&out)
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			writeError(w, invalid("The batch request could not be read: %v", err))
			return
		}
		id := part.Header.Get("Content-ID")
		inner, err := http.ReadRequest(bufio.NewReader(part))
		if err != nil {
			writeError(w, invalid("A batch part is not an HTTP request: %v", err))
			return
		}
		body, _ := io.ReadAll(inner.Body)
		u := inner.URL
		path := u.Path
		if i := strings.Index(path, Prefix); i > 0 {
			path = path[i:]
		}
		req := httptest.NewRequest(inner.Method, path+"?"+u.RawQuery, bytes.NewReader(body))
		req.Host = r.Host
		req.Header = inner.Header
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		h := textproto.MIMEHeader{}
		h.Set("Content-Type", "application/http")
		if id != "" {
			h.Set("Content-ID", "<response-"+strings.Trim(id, "<>")+">")
		}
		pw, _ := mw.CreatePart(h)
		fmt.Fprintf(pw, "HTTP/1.1 %d %s\r\nContent-Type: application/json; charset=UTF-8\r\n\r\n", rec.Code, http.StatusText(rec.Code))
		_, _ = pw.Write(rec.Body.Bytes())
	}
	_ = mw.Close()
	w.Header().Set("Content-Type", "multipart/mixed; boundary="+mw.Boundary())
	_, _ = w.Write(out.Bytes())
}
