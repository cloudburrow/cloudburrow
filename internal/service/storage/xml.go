package storage

import (
	"encoding/xml"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// xmlError is the XML API's error body
// (docs.cloud.google.com/storage/docs/xml-api/reference-status).
type xmlError struct {
	XMLName xml.Name `xml:"Error"`
	Code    string   `xml:"Code"`
	Message string   `xml:"Message"`
	Details string   `xml:"Details,omitempty"`
}

func writeXMLError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/xml; charset=UTF-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(xml.Header))
	_ = xml.NewEncoder(w).Encode(xmlError{Code: code, Message: message})
}

// serveXML is the XML API. Path-style requests name the bucket in the first
// segment; virtual-hosted ones in the host. Object GET and HEAD are #491;
// object PUT and DELETE, bucket GET (listing) and DELETE, and resumable
// uploads are #507 (xmlops.go).
func (s *Server) serveXML(w http.ResponseWriter, r *http.Request) {
	bucket, object := s.xmlTarget(r)
	q := r.URL.Query()
	switch {
	case bucket != "" && q.Get("upload_id") != "":
		s.xmlResumableChunk(w, r, q.Get("upload_id"))
		return
	case bucket != "" && object != "" && (r.Method == http.MethodGet || r.Method == http.MethodHead):
		o, err := s.lookupObject(r, bucket, object, true)
		if err != nil {
			writeXMLFromError(w, r, err, bucket)
			return
		}
		s.serveMedia(w, r, o, true)
		return
	case bucket != "" && object != "" && r.Method == http.MethodPut:
		s.xmlPutObject(w, r, bucket, object)
		return
	case bucket != "" && object != "" && r.Method == http.MethodPost && strings.EqualFold(r.Header.Get("X-Goog-Resumable"), "start"):
		s.xmlResumableStart(w, r, bucket, object)
		return
	case bucket != "" && object != "" && r.Method == http.MethodDelete:
		dq := url.Values{}
		if g := q.Get("generation"); g != "" {
			dq.Set("generation", g)
		}
		s.viaJSON(w, r, s.objectsDelete, jsonPrefix+"b/"+escape(bucket)+"/o/"+escape(object), dq, http.StatusNoContent)
		return
	case bucket != "" && object == "" && r.Method == http.MethodGet:
		s.xmlList(w, r, bucket)
		return
	case bucket != "" && object == "" && r.Method == http.MethodDelete:
		s.viaJSON(w, r, s.bucketsDelete, jsonPrefix+"b/"+escape(bucket), url.Values{}, http.StatusNoContent)
		return
	}
	what := "the XML API " + r.Method
	switch {
	case bucket == "":
		what += " on the service"
	case object == "":
		what += " on bucket " + bucket
	default:
		what += " on " + bucket + "/" + object
	}
	writeXMLError(w, http.StatusNotImplemented, "NotImplemented", what+" is not implemented yet")
}

// xmlTarget reads the bucket and object an XML request names.
func (s *Server) xmlTarget(r *http.Request) (bucket, object string) {
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	path := strings.TrimPrefix(r.URL.Path, "/")
	for _, base := range s.hosts {
		if b, ok := strings.CutSuffix(host, "."+base); ok && b != "" {
			return b, path
		}
	}
	bucket, object, _ = strings.Cut(path, "/")
	return bucket, object
}

// escape percent-encodes a bucket or object name as one path segment.
func escape(s string) string { return url.PathEscape(s) }

// writeXMLFromError writes a JSON API error in the XML API's terms: a
// missing bucket is NoSuchBucket, a missing object NoSuchKey
// (docs.cloud.google.com/storage/docs/xml-api/reference-status).
func writeXMLFromError(w http.ResponseWriter, r *http.Request, err error, bucket string) {
	e, ok := err.(*httpError)
	if !ok {
		writeXMLError(w, http.StatusInternalServerError, "InternalError", "internal error")
		return
	}
	code := map[int]string{
		http.StatusBadRequest: "InvalidArgument", http.StatusPreconditionFailed: "PreconditionFailed",
		http.StatusRequestedRangeNotSatisfiable: "InvalidRange", http.StatusForbidden: "AccessDenied",
		http.StatusConflict: "Conflict", http.StatusGone: "InvalidArgument",
	}[e.status]
	if e.status == http.StatusConflict && strings.Contains(e.message, "not empty") {
		code = "BucketNotEmpty"
	}
	switch {
	case e.status == http.StatusNotModified:
		w.WriteHeader(http.StatusNotModified)
		return
	case e.status == http.StatusNotFound && strings.Contains(e.message, "bucket"):
		code = "NoSuchBucket"
	case e.status == http.StatusNotFound:
		code = "NoSuchKey"
	case code == "":
		code = "InternalError"
	}
	if r.Method == http.MethodHead {
		w.WriteHeader(e.status)
		return
	}
	writeXMLError(w, e.status, code, e.message)
}
