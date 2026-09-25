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
// segment; virtual-hosted ones in the host. Nothing is built yet.
func (s *Server) serveXML(w http.ResponseWriter, r *http.Request) {
	bucket, object := s.xmlTarget(r)
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
