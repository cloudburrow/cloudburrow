package storage

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// The XML API subset (#507), to docs.cloud.google.com/storage/docs/xml-api/overview:
// PUT and DELETE of an object, GET (listing) and DELETE of a bucket, and
// resumable uploads started with x-goog-resumable: start (201 with the
// session URI, chunks PUT to it, a DELETE cancels with 204). Errors are
// the XML API's <Error> documents with its codes (reference-status).
// Requests are path-style or virtual-hosted ({bucket}.{host}).
//
// Where a JSON API handler already does the work, the XML request is
// translated to it and its answer translated back, so the two APIs cannot
// drift apart.

// captureWriter records a handler's response for translation.
type captureWriter struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func newCapture() *captureWriter { return &captureWriter{header: http.Header{}, status: http.StatusOK} }

func (c *captureWriter) Header() http.Header         { return c.header }
func (c *captureWriter) Write(b []byte) (int, error) { return c.body.Write(b) }
func (c *captureWriter) WriteHeader(status int)      { c.status = status }

// jsonErrorToXML writes a captured JSON API error as the XML API's.
func jsonErrorToXML(w http.ResponseWriter, r *http.Request, c *captureWriter) {
	var e struct {
		Error struct {
			Message string `json:"message"`
			Errors  []struct {
				Reason string `json:"reason"`
			} `json:"errors"`
		} `json:"error"`
	}
	_ = json.Unmarshal(c.body.Bytes(), &e)
	reason := ""
	if len(e.Error.Errors) > 0 {
		reason = e.Error.Errors[0].Reason
	}
	writeXMLFromError(w, r, &httpError{status: c.status, reason: reason, message: e.Error.Message}, "")
}

// xmlObjectHeaders are what the XML API answers a write with: the new
// generation and its checksums, and no body.
func xmlObjectHeaders(w http.ResponseWriter, o objectRecord) {
	h := w.Header()
	h.Set("ETag", `"`+hexMD5(o)+`"`)
	h.Set("X-Goog-Generation", strconv.FormatInt(o.Generation, 10))
	h.Set("X-Goog-Metageneration", strconv.FormatInt(o.Metageneration, 10))
	hash := "crc32c=" + crcBase64(o.CRC32C)
	if len(o.MD5) > 0 {
		hash += ",md5=" + b64(o.MD5)
	}
	h.Set("X-Goog-Hash", hash)
	h.Set("X-Goog-Stored-Content-Length", strconv.FormatInt(o.Size, 10))
}

// xmlPreconditions reads x-goog-if-generation-match and
// x-goog-if-metageneration-match as the JSON API's preconditions.
func xmlPreconditions(r *http.Request) (objectPreconditions, error) {
	q := url.Values{}
	for h, p := range map[string]string{"X-Goog-If-Generation-Match": "ifGenerationMatch", "X-Goog-If-Metageneration-Match": "ifMetagenerationMatch"} {
		if v := r.Header.Get(h); v != "" {
			q.Set(p, v)
		}
	}
	return parseObjectPreconditions(q, "if")
}

// xmlMeta reads an object's metadata from XML API request headers.
func xmlMeta(r *http.Request, name string) (uploadMeta, error) {
	m := uploadMeta{Name: name, ContentType: r.Header.Get("Content-Type"),
		ContentEncoding: r.Header.Get("Content-Encoding"), ContentDisposition: r.Header.Get("Content-Disposition"),
		ContentLanguage: r.Header.Get("Content-Language"), CacheControl: r.Header.Get("Cache-Control"),
		StorageClass: r.Header.Get("X-Goog-Storage-Class"), CustomTime: r.Header.Get("X-Goog-Custom-Time")}
	for k, v := range r.Header {
		if lk := strings.ToLower(k); strings.HasPrefix(lk, "x-goog-meta-") && len(v) > 0 {
			if m.Metadata == nil {
				m.Metadata = map[string]string{}
			}
			m.Metadata[lk[len("x-goog-meta-"):]] = v[0]
		}
	}
	for _, h := range []string{"X-Goog-Copy-Source", "X-Goog-Acl", "X-Goog-Encryption-Kms-Key-Name", "X-Goog-Encryption-Algorithm"} {
		if r.Header.Get(h) != "" {
			return m, badRequest("The XML API header %s is not supported by CloudBurrow; it is refused rather than ignored", h)
		}
	}
	return m, validObjectName(name)
}

// xmlPutObject is PUT /{bucket}/{object}: a whole object in the body.
func (s *Server) xmlPutObject(w http.ResponseWriter, r *http.Request, bucket, name string) {
	meta, err := xmlMeta(r, name)
	if err == nil {
		err = s.bucketExists(bucket)
	}
	var pre objectPreconditions
	if err == nil {
		pre, err = xmlPreconditions(r)
	}
	var wantMD5 []byte
	var wantCRC *uint32
	if err == nil {
		wantMD5, wantCRC, err = expectedHashes(r, meta)
	}
	if err != nil {
		writeXMLFromError(w, r, err, bucket)
		return
	}
	blob, err := s.blobs.Write(r.Body)
	if err != nil {
		writeXMLFromError(w, r, err, bucket)
		return
	}
	o, err := s.finalizeObject(bucket, meta, blob, pre, wantMD5, wantCRC)
	if err != nil {
		writeXMLFromError(w, r, err, bucket)
		return
	}
	xmlObjectHeaders(w, o)
	w.WriteHeader(http.StatusOK)
}

// viaJSON runs a JSON API handler for an XML request, at jsonPath with
// extra query parameters, and translates an error into the XML API's.
// ok is the status the XML API answers success with.
func (s *Server) viaJSON(w http.ResponseWriter, r *http.Request, h http.HandlerFunc, jsonPath string, q url.Values, ok int) {
	u := *r.URL
	u.RawPath = jsonPath
	u.Path, _ = url.PathUnescape(jsonPath)
	u.RawQuery = q.Encode()
	jr := r.Clone(r.Context())
	jr.URL = &u
	c := newCapture()
	h(c, jr)
	if c.status >= 400 {
		jsonErrorToXML(w, r, c)
		return
	}
	w.WriteHeader(ok)
}

// xmlList is GET /{bucket}: a ListBucketResult with prefix, delimiter,
// marker and max-keys (xml-api reference, list objects).
func (s *Server) xmlList(w http.ResponseWriter, r *http.Request, bucket string) {
	q := r.URL.Query()
	prefix, delim, marker := q.Get("prefix"), q.Get("delimiter"), q.Get("marker")
	max := 1000
	if v := q.Get("max-keys"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeXMLError(w, http.StatusBadRequest, "InvalidArgument", "max-keys must be a non-negative integer")
			return
		}
		if n < max {
			max = n
		}
	}
	type content struct {
		Key            string `xml:"Key"`
		Generation     int64  `xml:"Generation"`
		MetaGeneration int64  `xml:"MetaGeneration"`
		LastModified   string `xml:"LastModified"`
		ETag           string `xml:"ETag"`
		Size           int64  `xml:"Size"`
		StorageClass   string `xml:"StorageClass"`
	}
	type commonPrefix struct {
		Prefix string `xml:"Prefix"`
	}
	type result struct {
		XMLName        xml.Name       `xml:"ListBucketResult"`
		XMLNS          string         `xml:"xmlns,attr"`
		Name           string         `xml:"Name"`
		Prefix         string         `xml:"Prefix"`
		Marker         string         `xml:"Marker"`
		NextMarker     string         `xml:"NextMarker,omitempty"`
		Delimiter      string         `xml:"Delimiter,omitempty"`
		MaxKeys        int            `xml:"MaxKeys"`
		IsTruncated    bool           `xml:"IsTruncated"`
		Contents       []content      `xml:"Contents"`
		CommonPrefixes []commonPrefix `xml:"CommonPrefixes"`
	}
	res := result{XMLNS: "http://doc.s3.amazonaws.com/2006-03-01", Name: bucket, Prefix: prefix, Marker: marker, Delimiter: delim, MaxKeys: max}
	err := s.meta.View(func(tx Tx) error {
		if _, ok, err := s.getBucket(tx, bucket); err != nil {
			return err
		} else if !ok {
			return notFound("The specified bucket does not exist.")
		}
		seen := map[string]bool{}
		last := ""
		for _, name := range listNames(tx, bucket, prefix, listLive) {
			if marker != "" && name <= marker {
				continue
			}
			if delim != "" {
				if i := strings.Index(name[len(prefix):], delim); i >= 0 {
					p := name[:len(prefix)+i+len(delim)]
					if seen[p] || marker != "" && strings.HasPrefix(marker, p) {
						continue
					}
					if len(res.Contents)+len(res.CommonPrefixes) >= max {
						res.IsTruncated, res.NextMarker = true, last
						return nil
					}
					seen[p] = true
					res.CommonPrefixes = append(res.CommonPrefixes, commonPrefix{p})
					last = p
					continue
				}
			}
			if len(res.Contents)+len(res.CommonPrefixes) >= max {
				res.IsTruncated, res.NextMarker = true, last
				return nil
			}
			o, ok, err := getObject(tx, bucket, name)
			if err != nil || !ok {
				return err
			}
			res.Contents = append(res.Contents, content{Key: o.Name, Generation: o.Generation, MetaGeneration: o.Metageneration,
				LastModified: o.Updated.UTC().Format(time.RFC3339Nano), ETag: `"` + hexMD5(o) + `"`, Size: o.Size, StorageClass: o.StorageClass})
			last = name
		}
		return nil
	})
	if err != nil {
		writeXMLFromError(w, r, err, bucket)
		return
	}
	w.Header().Set("Content-Type", "application/xml; charset=UTF-8")
	_, _ = w.Write([]byte(xml.Header))
	_ = xml.NewEncoder(w).Encode(res)
}

// xmlResumableStart is POST /{bucket}/{object} with x-goog-resumable:
// start: 201 Created, with the session URI in Location
// (performing-resumable-uploads, XML API).
func (s *Server) xmlResumableStart(w http.ResponseWriter, r *http.Request, bucket, name string) {
	meta, err := xmlMeta(r, name)
	if err == nil {
		err = s.bucketExists(bucket)
	}
	if err == nil {
		_, err = xmlPreconditions(r)
	}
	if err != nil {
		writeXMLFromError(w, r, err, bucket)
		return
	}
	u := uploadSession{ID: newUploadID(), Bucket: bucket, Meta: meta, Created: s.now(), Origin: r.Header.Get("Origin"), XML: true}
	pre := url.Values{}
	for h, p := range map[string]string{"X-Goog-If-Generation-Match": "ifGenerationMatch", "X-Goog-If-Metageneration-Match": "ifMetagenerationMatch"} {
		if v := r.Header.Get(h); v != "" {
			pre.Set(p, v)
		}
	}
	u.Query = pre.Encode()
	if err := s.meta.Update(func(tx Tx) error { return putSession(tx, u) }); err != nil {
		writeXMLFromError(w, r, err, bucket)
		return
	}
	w.Header().Set("Location", baseURL(r)+"/"+escape(bucket)+"/"+escapePath(name)+"?upload_id="+url.QueryEscape(u.ID))
	w.Header().Set("X-GUploader-UploadID", u.ID)
	w.WriteHeader(http.StatusCreated)
}

// xmlResumableChunk sends a chunk, status query or cancel to the shared
// session logic and answers in the XML API's terms: a cancel is 204, the
// finished upload 200 with the object's headers and no body.
func (s *Server) xmlResumableChunk(w http.ResponseWriter, r *http.Request, id string) {
	c := newCapture()
	s.resumableChunk(c, r, id)
	switch {
	case c.status == 499:
		w.WriteHeader(http.StatusNoContent)
	case c.status >= 400:
		jsonErrorToXML(w, r, c)
	case c.status == http.StatusOK && c.header.Get("X-Http-Status-Code-Override") == "":
		var o struct {
			Generation, Metageneration, Size, MD5Hash, CRC32C string
		}
		var done objectRecord
		_ = json.Unmarshal(c.body.Bytes(), &o)
		done.Generation, _ = strconv.ParseInt(o.Generation, 10, 64)
		done.Metageneration, _ = strconv.ParseInt(o.Metageneration, 10, 64)
		done.Size, _ = strconv.ParseInt(o.Size, 10, 64)
		done.MD5, _ = unb64(o.MD5Hash)
		if crc, err := unb64(o.CRC32C); err == nil && len(crc) == 4 {
			done.CRC32C = uint32(crc[0])<<24 | uint32(crc[1])<<16 | uint32(crc[2])<<8 | uint32(crc[3])
		}
		xmlObjectHeaders(w, done)
		w.WriteHeader(http.StatusOK)
	default:
		for k, v := range c.header {
			w.Header()[k] = v
		}
		w.WriteHeader(c.status)
		_, _ = w.Write(c.body.Bytes())
	}
}

// escapePath percent-encodes an object name for a URL path, keeping "/".
func escapePath(name string) string {
	parts := strings.Split(name, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func unb64(s string) ([]byte, error) { return base64.StdEncoding.DecodeString(s) }
