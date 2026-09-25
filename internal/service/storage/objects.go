package storage

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Objects (#491): media and multipart uploads, metadata and media reads over
// the JSON API and the XML API, byte ranges, and generations. Every finalize
// creates a new generation with metageneration 1; on a versioned bucket the
// one it replaces becomes noncurrent (#498, versions.go).

type objectRecord struct {
	Bucket             string            `json:"bucket"`
	Name               string            `json:"name"`
	Generation         int64             `json:"generation"`
	Metageneration     int64             `json:"metageneration"`
	Blob               string            `json:"blob"`
	Size               int64             `json:"size"`
	MD5                []byte            `json:"md5"`
	CRC32C             uint32            `json:"crc32c"`
	ContentType        string            `json:"contentType,omitempty"`
	ContentEncoding    string            `json:"contentEncoding,omitempty"`
	ContentDisposition string            `json:"contentDisposition,omitempty"`
	ContentLanguage    string            `json:"contentLanguage,omitempty"`
	CacheControl       string            `json:"cacheControl,omitempty"`
	Metadata           map[string]string `json:"metadata,omitempty"`
	StorageClass       string            `json:"storageClass"`
	CustomTime         time.Time         `json:"customTime,omitempty"`
	ComponentCount     int               `json:"componentCount,omitempty"`
	Created            time.Time         `json:"created"`
	Updated            time.Time         `json:"updated"`
	// Deleted is when a noncurrent version stopped being live (#498).
	Deleted time.Time `json:"deleted,omitempty"`
	// SoftDeleted and HardDelete bound a soft-deleted version's
	// restorable life, in the bucket generation it was deleted from (#499).
	SoftDeleted      time.Time `json:"softDeleted,omitempty"`
	HardDelete       time.Time `json:"hardDelete,omitempty"`
	BucketGeneration int64     `json:"bucketGeneration,omitempty"`
	// Holds and retention (#500). RetentionFrom restarts the retention
	// clock when an event-based hold is released; zero means Created.
	TemporaryHold  bool             `json:"temporaryHold,omitempty"`
	EventBasedHold bool             `json:"eventBasedHold,omitempty"`
	Retention      *objectRetention `json:"retention,omitempty"`
	RetentionFrom  time.Time        `json:"retentionFrom,omitempty"`
	// ClassUpdated is when a lifecycle rule last changed the storage
	// class (#501); zero means Created.
	ClassUpdated time.Time `json:"classUpdated,omitempty"`
}

func objectKey(bucket, name string) string { return objectPrefix + bucket + "/" + name }

func getObject(tx Tx, bucket, name string) (objectRecord, bool, error) {
	var o objectRecord
	raw, ok := tx.Get(objectKey(bucket, name))
	if !ok {
		return o, false, nil
	}
	return o, true, json.Unmarshal(raw, &o)
}

func putObject(tx Tx, o objectRecord) error {
	raw, err := json.Marshal(o)
	if err != nil {
		return err
	}
	tx.Put(objectKey(o.Bucket, o.Name), raw)
	return nil
}

// nextGeneration is a new generation, unique and increasing: the clock in
// microseconds, as Google's generations are, but never repeating one.
func (s *Server) nextGeneration() int64 {
	s.genMu.Lock()
	defer s.genMu.Unlock()
	g := s.now().UnixMicro()
	if g <= s.lastGen {
		g = s.lastGen + 1
	}
	s.lastGen = g
	return g
}

func crcBase64(c uint32) string {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, c)
	return base64.StdEncoding.EncodeToString(b)
}

// objectJSON renders o, reading its bucket's retention period; inside a
// transaction use objectJSONWith.
func (s *Server) objectJSON(r *http.Request, o objectRecord) map[string]any {
	var period time.Duration
	_ = s.meta.View(func(tx Tx) error {
		if b, ok, err := s.getBucket(tx, o.Bucket); err == nil && ok {
			period = retentionPeriod(b)
		}
		return nil
	})
	return s.objectJSONWith(r, o, period)
}

// objectJSONWith renders o under a bucket retention period.
func (s *Server) objectJSONWith(r *http.Request, o objectRecord, period time.Duration) map[string]any {
	gen := strconv.FormatInt(o.Generation, 10)
	out := map[string]any{
		"kind":           "storage#object",
		"id":             o.Bucket + "/" + o.Name + "/" + gen,
		"selfLink":       selfLink(r, o.Bucket, o.Name),
		"mediaLink":      mediaLink(r, o.Bucket, o.Name, o.Generation),
		"name":           o.Name,
		"bucket":         o.Bucket,
		"generation":     gen,
		"metageneration": strconv.FormatInt(o.Metageneration, 10),
		"contentType":    o.ContentType,
		"storageClass":   o.StorageClass,
		"size":           strconv.FormatInt(o.Size, 10),

		"crc32c":                  crcBase64(o.CRC32C),
		"etag":                    objectETag(o),
		"timeCreated":             rfc3339(o.Created),
		"updated":                 rfc3339(o.Updated),
		"timeStorageClassUpdated": rfc3339(classUpdated(o)),
		// Every object is finalized when it is created: appendable uploads,
		// which finalize later, are not built.
		"timeFinalized": rfc3339(o.Created),
	}
	for k, v := range map[string]string{"contentEncoding": o.ContentEncoding, "contentDisposition": o.ContentDisposition,
		"contentLanguage": o.ContentLanguage, "cacheControl": o.CacheControl} {
		if v != "" {
			out[k] = v
		}
	}
	if len(o.Metadata) > 0 {
		out["metadata"] = o.Metadata
	}
	if !o.CustomTime.IsZero() {
		out["customTime"] = o.CustomTime.UTC().Format(time.RFC3339Nano)
	}
	// A composite object has no MD5 (#495).
	if len(o.MD5) > 0 {
		out["md5Hash"] = base64.StdEncoding.EncodeToString(o.MD5)
	}
	if o.ComponentCount > 0 {
		out["componentCount"] = o.ComponentCount
	}
	if !o.Deleted.IsZero() {
		out["timeDeleted"] = rfc3339(o.Deleted)
	}
	if !o.SoftDeleted.IsZero() {
		out["softDeleteTime"], out["hardDeleteTime"] = rfc3339(o.SoftDeleted), rfc3339(o.HardDelete)
	}
	protectionJSON(out, o, period)
	return out
}

func classUpdated(o objectRecord) time.Time {
	if !o.ClassUpdated.IsZero() {
		return o.ClassUpdated
	}
	return o.Created
}

func hexMD5(o objectRecord) string { return fmt.Sprintf("%x", o.MD5) }

// objectETag changes with the generation and the metageneration, as Google's
// does; its encoding is opaque to clients.
func objectETag(o objectRecord) string {
	b := binary.AppendUvarint([]byte{0x08}, uint64(o.Generation))
	b = binary.AppendUvarint(append(b, 0x10), uint64(o.Metageneration))
	return base64.StdEncoding.EncodeToString(b)
}

// objectPreconditions are the four generation and metageneration conditions.
type objectPreconditions struct {
	genMatch, genNotMatch, metaMatch, metaNotMatch *int64
}

func parseObjectPreconditions(q url.Values, prefix string) (objectPreconditions, error) {
	var p objectPreconditions
	for name, dst := range map[string]**int64{
		prefix + "GenerationMatch": &p.genMatch, prefix + "GenerationNotMatch": &p.genNotMatch,
		prefix + "MetagenerationMatch": &p.metaMatch, prefix + "MetagenerationNotMatch": &p.metaNotMatch,
	} {
		if v := q.Get(name); v != "" {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return p, badRequest("Invalid argument: %s=%q is not a number", name, v)
			}
			*dst = &n
		}
	}
	return p, nil
}

// check applies the conditions to the live object, if any. A generation of 0
// is "no live object", so ifGenerationMatch=0 means "only if absent". A
// failed NotMatch on a read is 304; anything else that fails is 412.
func (p objectPreconditions) check(o objectRecord, exists, read bool) error {
	gen, meta := int64(0), int64(0)
	if exists {
		gen, meta = o.Generation, o.Metageneration
	}
	fail := preconditionFailed("At least one of the pre-conditions you specified did not hold.")
	if p.genMatch != nil && *p.genMatch != gen {
		return fail
	}
	if p.metaMatch != nil && (!exists || *p.metaMatch != meta) {
		return fail
	}
	notModified := p.genNotMatch != nil && *p.genNotMatch == gen || p.metaNotMatch != nil && exists && *p.metaNotMatch == meta
	if notModified {
		if read {
			return errorf(http.StatusNotModified, "notModified", "not modified")
		}
		return fail
	}
	return nil
}

// uploadMeta is what an upload request says about the object.
type uploadMeta struct {
	Name               string            `json:"name"`
	ContentType        string            `json:"contentType"`
	ContentEncoding    string            `json:"contentEncoding"`
	ContentDisposition string            `json:"contentDisposition"`
	ContentLanguage    string            `json:"contentLanguage"`
	CacheControl       string            `json:"cacheControl"`
	Metadata           map[string]string `json:"metadata"`
	MD5Hash            string            `json:"md5Hash"`
	CRC32C             string            `json:"crc32c"`
	StorageClass       string            `json:"storageClass"`
	CustomTime         string            `json:"customTime"`
	// protection is the body's holds and retention, for applyProtection.
	protection map[string]any
}

// uploadFields says how each Object property is treated in an upload's
// metadata: kept, output-only (ignored), or refused by name.
var uploadFields = map[string]string{
	"name": "kept", "contentType": "kept", "contentEncoding": "kept", "contentDisposition": "kept",
	"contentLanguage": "kept", "cacheControl": "kept", "metadata": "kept", "md5Hash": "kept", "crc32c": "kept",
	"storageClass": "kept",
	"bucket":       "output", "id": "output", "kind": "output", "selfLink": "output", "mediaLink": "output",
	"generation": "output", "metageneration": "output", "size": "output", "etag": "output",
	"timeCreated": "output", "updated": "output", "timeStorageClassUpdated": "output", "timeFinalized": "output",
	"componentCount": "output", "timeDeleted": "output", "softDeleteTime": "output", "hardDeleteTime": "output",
	"restoreToken": "output",
	"acl":          "ACL methods are not implemented", "owner": "output",
	"temporaryHold": "kept", "eventBasedHold": "kept", "retention": "kept", "retentionExpirationTime": "output",
	"customTime": "kept", "kmsKeyName": "customer-managed keys are not implemented",
	"customerEncryption": "customer-supplied keys are not implemented", "contexts": "#492",
}

func parseUploadMeta(raw []byte) (uploadMeta, error) {
	var m uploadMeta
	if len(bytes.TrimSpace(raw)) == 0 {
		return m, nil
	}
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return m, errorf(http.StatusBadRequest, "parseError", "Parse Error: the object metadata is not a JSON object")
	}
	for k, v := range generic {
		how, ok := uploadFields[k]
		switch {
		case !ok:
			return m, badRequest("Invalid argument: %s is not an Object field", k)
		case how == "kept", how == "output", empty(v):
		default:
			return m, badRequest("The object field %q is not supported by CloudBurrow yet (%s); it is refused rather than dropped", k, how)
		}
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return m, badRequest("Invalid argument: %v", err)
	}
	m.protection = map[string]any{}
	for _, k := range []string{"temporaryHold", "eventBasedHold", "retention"} {
		if v, ok := generic[k]; ok {
			m.protection[k] = v
		}
	}
	return m, nil
}

// expectedHashes are the checksums a client said the bytes have, from the
// metadata, Content-MD5 or x-goog-hash.
func expectedHashes(r *http.Request, m uploadMeta) (md5sum []byte, crc *uint32, err error) {
	dec := func(what, v string) ([]byte, error) {
		b, derr := base64.StdEncoding.DecodeString(v)
		if derr != nil {
			return nil, badRequest("Invalid argument: %s %q is not base64", what, v)
		}
		return b, nil
	}
	setCRC := func(b []byte) error {
		if len(b) != 4 {
			return badRequest("Invalid argument: a CRC32C is 4 bytes")
		}
		c := binary.BigEndian.Uint32(b)
		crc = &c
		return nil
	}
	if m.MD5Hash != "" {
		if md5sum, err = dec("md5Hash", m.MD5Hash); err != nil {
			return
		}
	}
	if m.CRC32C != "" {
		b, derr := dec("crc32c", m.CRC32C)
		if derr != nil {
			return nil, nil, derr
		}
		if err = setCRC(b); err != nil {
			return
		}
	}
	if v := r.Header.Get("Content-MD5"); v != "" {
		if md5sum, err = dec("Content-MD5", v); err != nil {
			return
		}
	}
	for _, part := range strings.Split(r.Header.Get("X-Goog-Hash"), ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		b, derr := dec("x-goog-hash "+k, v)
		if derr != nil {
			return nil, nil, derr
		}
		switch k {
		case "md5":
			md5sum = b
		case "crc32c":
			if err = setCRC(b); err != nil {
				return
			}
		}
	}
	return md5sum, crc, nil
}

func (s *Server) objectsInsertUpload(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	bucket := pathVar(r, uploadPrefix, 1)
	if strings.HasPrefix(r.URL.EscapedPath(), resumablePrefix) {
		bucket = pathVar(r, resumablePrefix, 1)
	}
	for _, p := range []string{"predefinedAcl", "kmsKeyName"} {
		if q.Get(p) != "" {
			writeError(w, errorf(http.StatusNotImplemented, "notImplemented", "%s is not implemented", p))
			return
		}
	}
	pre, err := parseObjectPreconditions(q, "if")
	if err != nil {
		writeError(w, err)
		return
	}
	var meta uploadMeta
	var media io.Reader
	switch q.Get("uploadType") {
	case "media":
		meta.Name = q.Get("name")
		meta.ContentType = r.Header.Get("Content-Type")
		media = r.Body
	case "multipart":
		mt, params, perr := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if perr != nil || !strings.HasPrefix(mt, "multipart/") || params["boundary"] == "" {
			writeError(w, badRequest("A multipart upload needs a multipart/related body with a boundary"))
			return
		}
		mr := multipart.NewReader(r.Body, params["boundary"])
		first, perr := mr.NextPart()
		if perr != nil {
			writeError(w, badRequest("The multipart body has no metadata part"))
			return
		}
		raw, _ := io.ReadAll(io.LimitReader(first, 1<<20))
		if meta, err = parseUploadMeta(raw); err != nil {
			writeError(w, err)
			return
		}
		second, perr := mr.NextPart()
		if perr != nil {
			writeError(w, badRequest("The multipart body has no media part"))
			return
		}
		if meta.ContentType == "" {
			meta.ContentType = second.Header.Get("Content-Type")
		}
		if n := q.Get("name"); meta.Name == "" {
			meta.Name = n
		}
		media = second
	case "":
		writeError(w, required("uploadType"))
		return
	default:
		writeError(w, badRequest("Invalid argument: uploadType=%s", q.Get("uploadType")))
		return
	}
	if meta.Name == "" {
		writeError(w, required("name"))
		return
	}
	if err := validObjectName(meta.Name); err != nil {
		writeError(w, err)
		return
	}
	wantMD5, wantCRC, err := expectedHashes(r, meta)
	if err != nil {
		writeError(w, err)
		return
	}
	if err := s.bucketExists(bucket); err != nil {
		writeError(w, err)
		return
	}
	blob, err := s.blobs.Write(media)
	if err != nil {
		writeError(w, err)
		return
	}
	o, err := s.finalizeObject(bucket, meta, blob, pre, wantMD5, wantCRC)
	if err != nil {
		writeError(w, err)
		return
	}
	writeResponse(w, r, http.StatusOK, s.objectJSON(r, o))
}

func (s *Server) bucketExists(bucket string) error {
	return s.meta.View(func(tx Tx) error {
		if _, ok, err := s.getBucket(tx, bucket); err != nil {
			return err
		} else if !ok {
			return notFound("The specified bucket does not exist.")
		}
		return nil
	})
}

// finalizeObject makes stored bytes the live generation of an object: it
// checks the checksums the client sent, applies the storage class and the
// preconditions, and commits in one transaction. Every upload kind ends
// here (#491, #493).
func (s *Server) finalizeObject(bucket string, meta uploadMeta, blob Blob, pre objectPreconditions, wantMD5 []byte, wantCRC *uint32) (objectRecord, error) {
	if wantMD5 != nil && !bytes.Equal(wantMD5, blob.MD5) {
		return objectRecord{}, badRequest("Provided MD5 hash %q doesn't match calculated MD5 hash %q.",
			base64.StdEncoding.EncodeToString(wantMD5), base64.StdEncoding.EncodeToString(blob.MD5))
	}
	if wantCRC != nil && *wantCRC != blob.CRC32C {
		return objectRecord{}, badRequest("Provided CRC32C %q doesn't match calculated CRC32C %q.", crcBase64(*wantCRC), crcBase64(blob.CRC32C))
	}
	class := strings.ToUpper(meta.StorageClass)
	if class != "" && !storageClasses[class] {
		return objectRecord{}, badRequest("Invalid argument: storageClass %s", meta.StorageClass)
	}
	now := s.now()
	o := objectRecord{Bucket: bucket, Name: meta.Name, Generation: s.nextGeneration(), Metageneration: 1,
		Blob: blob.ID, Size: blob.Size, MD5: blob.MD5, CRC32C: blob.CRC32C,
		ContentType: meta.ContentType, ContentEncoding: meta.ContentEncoding, ContentDisposition: meta.ContentDisposition,
		ContentLanguage: meta.ContentLanguage, CacheControl: meta.CacheControl, Metadata: meta.Metadata,
		StorageClass: class, Created: now, Updated: now}
	if o.ContentType == "" {
		o.ContentType = "application/octet-stream"
	}
	if meta.CustomTime != "" {
		t, err := time.Parse(time.RFC3339Nano, meta.CustomTime)
		if err != nil {
			return objectRecord{}, badRequest("Invalid argument: customTime %q is not RFC 3339", meta.CustomTime)
		}
		o.CustomTime = t
	}
	if err := applyProtection(&o, objectRecord{}, meta.protection, false, false, now); err != nil {
		return objectRecord{}, err
	}
	err := s.meta.Update(func(tx Tx) error {
		b, ok, err := s.getBucket(tx, bucket)
		if err != nil {
			return err
		} else if !ok {
			return notFound("The specified bucket does not exist.")
		}
		if o.StorageClass == "" {
			o.StorageClass, _ = b.Fields["storageClass"].(string)
		}
		if err := checkNewObject(b, &o); err != nil {
			return err
		}
		cur, exists, err := getObject(tx, bucket, meta.Name)
		if err != nil {
			return err
		}
		if err := pre.check(cur, exists, false); err != nil {
			return err
		}
		if err := retireLive(tx, b, meta.Name, now, o.Generation); err != nil {
			return err
		}
		return finalized(tx, o, cur, exists, now)
	})
	return o, err
}

// validObjectName follows docs.cloud.google.com/storage/docs/objects#naming:
// 1-1024 bytes of UTF-8, no carriage return or line feed, not "." or "..".
func validObjectName(name string) error {
	switch {
	case len(name) == 0 || len(name) > 1024:
		return badRequest("Invalid argument: an object name is 1-1024 bytes")
	case strings.ContainsAny(name, "\r\n"):
		return badRequest("Invalid argument: an object name cannot contain a carriage return or line feed")
	case name == "." || name == "..":
		return badRequest("Invalid argument: an object name cannot be %q", name)
	case strings.HasPrefix(name, ".well-known/acme-challenge/"):
		return badRequest("Invalid argument: object names beginning .well-known/acme-challenge/ are reserved")
	}
	return nil
}

// lookupObject reads the object a request names: the live generation, or the
// version ?generation= asks for, live or noncurrent (#498).
func (s *Server) lookupObject(r *http.Request, bucket, name string, read bool) (objectRecord, error) {
	q := r.URL.Query()
	pre, err := parseObjectPreconditions(q, "if")
	if err != nil {
		return objectRecord{}, err
	}
	var o objectRecord
	err = s.meta.View(func(tx Tx) error {
		if _, ok, gerr := s.getBucket(tx, bucket); gerr != nil {
			return gerr
		} else if !ok {
			return notFound("The specified bucket does not exist.")
		}
		var exists bool
		var gerr error
		if o, _, exists, gerr = findVersion(tx, bucket, name, q.Get("generation")); gerr != nil {
			return gerr
		}
		if !exists {
			return notFound("No such object: %s/%s", bucket, name)
		}
		if err := pre.check(o, true, read); err != nil {
			return err
		}
		return headerPreconditions(r, o)
	})
	return o, err
}

func (s *Server) objectsGet(w http.ResponseWriter, r *http.Request) {
	bucket, name := pathVar(r, jsonPrefix, 1), pathVar(r, jsonPrefix, 3)
	if r.URL.Query().Get("softDeleted") == "true" {
		s.getSoftDeletedObject(w, r, bucket, name)
		return
	}
	o, err := s.lookupObject(r, bucket, name, true)
	if err != nil {
		writeError(w, err)
		return
	}
	if r.URL.Query().Get("alt") == "media" {
		s.serveMedia(w, r, o, false)
		return
	}
	writeResponse(w, r, http.StatusOK, s.objectJSON(r, o))
}

// serveDownload is /download/storage/v1/b/{bucket}/o/{object}.
func (s *Server) serveDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeError(w, notFound("no Cloud Storage method is bound to %s %s", r.Method, r.URL.Path))
		return
	}
	bucket, name := pathVar(r, downloadPrefix, 1), pathVar(r, downloadPrefix, 3)
	if r.URL.Query().Get("softDeleted") == "true" {
		writeError(w, badRequest("A soft-deleted object cannot be read; restore it first"))
		return
	}
	o, err := s.lookupObject(r, bucket, name, true)
	if err != nil {
		writeError(w, err)
		return
	}
	s.serveMedia(w, r, o, false)
}

func (s *Server) objectsDelete(w http.ResponseWriter, r *http.Request) {
	bucket, name := pathVar(r, jsonPrefix, 1), pathVar(r, jsonPrefix, 3)
	q := r.URL.Query()
	pre, err := parseObjectPreconditions(q, "if")
	if err != nil {
		writeError(w, err)
		return
	}
	err = s.meta.Update(func(tx Tx) error {
		b, ok, gerr := s.getBucket(tx, bucket)
		if gerr != nil {
			return gerr
		} else if !ok {
			return notFound("The specified bucket does not exist.")
		}
		gen := q.Get("generation")
		o, live, exists, gerr := findVersion(tx, bucket, name, gen)
		if gerr != nil {
			return gerr
		}
		if !exists {
			return notFound("No such object: %s/%s", bucket, name)
		}
		if err := pre.check(o, true, false); err != nil {
			return err
		}
		if err := headerPreconditions(r, o); err != nil {
			return err
		}
		// A delete that names a generation removes that version; one that
		// does not retires the live version, which a versioned bucket keeps
		// as noncurrent (object-versioning docs). Either way what leaves the
		// bucket is soft-deleted under its policy (#499).
		now := s.now()
		if gen != "" {
			if err := protect(b, o, now, false); err != nil {
				return err
			}
		}
		switch {
		case gen == "":
			return retireLive(tx, b, name, now, 0)
		case live:
			tx.Delete(objectKey(bucket, name))
		default:
			if err := deleteVersion(tx, o); err != nil {
				return err
			}
		}
		return removed(tx, b, o, now, 0)
	})
	if err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// serveMedia writes an object's bytes, honouring one byte range. xml adds the
// XML API's headers. The x-goog-* headers are what the Go client reads the
// object's identity and checksums from.
func (s *Server) serveMedia(w http.ResponseWriter, r *http.Request, o objectRecord, xml bool) {
	h := w.Header()
	h.Set("Content-Type", o.ContentType)
	h.Set("X-Goog-Generation", strconv.FormatInt(o.Generation, 10))
	h.Set("X-Goog-Metageneration", strconv.FormatInt(o.Metageneration, 10))
	h.Set("X-Goog-Stored-Content-Length", strconv.FormatInt(o.Size, 10))
	enc := o.ContentEncoding
	if enc == "" {
		enc = "identity"
	}
	h.Set("X-Goog-Stored-Content-Encoding", enc)
	if o.ContentEncoding != "" {
		h.Set("Content-Encoding", o.ContentEncoding)
	}
	hash := "crc32c=" + crcBase64(o.CRC32C)
	if len(o.MD5) > 0 {
		hash += ",md5=" + base64.StdEncoding.EncodeToString(o.MD5)
	}
	h.Set("X-Goog-Hash", hash)
	if o.ComponentCount > 0 {
		h.Set("X-Goog-Component-Count", strconv.Itoa(o.ComponentCount))
	}
	h.Set("X-Goog-Storage-Class", o.StorageClass)
	h.Set("Last-Modified", o.Updated.UTC().Format(http.TimeFormat))
	h.Set("ETag", `"`+hexMD5(o)+`"`)
	h.Set("Accept-Ranges", "bytes")
	for k, v := range o.Metadata {
		h.Set("X-Goog-Meta-"+k, v)
	}
	for k, v := range map[string]string{"Cache-Control": o.CacheControl, "Content-Disposition": o.ContentDisposition, "Content-Language": o.ContentLanguage} {
		if v != "" {
			h.Set(k, v)
		}
	}
	start, end, partial, ok := parseRange(r.Header.Get("Range"), o.Size)
	if !ok {
		h.Set("Content-Range", fmt.Sprintf("bytes */%d", o.Size))
		if xml {
			writeXMLError(w, http.StatusRequestedRangeNotSatisfiable, "InvalidRange", "The requested range cannot be satisfied.")
			return
		}
		writeError(w, errorf(http.StatusRequestedRangeNotSatisfiable, "requestedRangeNotSatisfiable", "The requested range cannot be satisfied."))
		return
	}
	status := http.StatusOK
	if partial {
		status = http.StatusPartialContent
		h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end-1, o.Size))
	}
	h.Set("Content-Length", strconv.FormatInt(end-start, 10))
	if r.Method == http.MethodHead {
		w.WriteHeader(status)
		return
	}
	f, err := s.blobs.Open(o.Blob)
	if err != nil {
		writeError(w, err)
		return
	}
	defer f.Close()
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(status)
	_, _ = io.CopyN(w, f, end-start)
}

// parseRange reads one "bytes=" range against size: a-b, a- or -N. It
// returns the half-open [start,end), whether it is partial, and false when
// unsatisfiable. A header it does not understand, or several ranges, serves
// the whole object, as HTTP allows.
func parseRange(h string, size int64) (start, end int64, partial, ok bool) {
	spec, found := strings.CutPrefix(h, "bytes=")
	if h == "" || !found || strings.Contains(spec, ",") {
		return 0, size, false, true
	}
	a, b, found := strings.Cut(spec, "-")
	if !found {
		return 0, size, false, true
	}
	switch {
	case a == "":
		n, err := strconv.ParseInt(b, 10, 64)
		if err != nil || n <= 0 {
			return 0, size, false, n != 0 || err != nil
		}
		if n > size {
			n = size
		}
		if size == 0 {
			return 0, 0, false, false
		}
		return size - n, size, true, true
	default:
		s, err := strconv.ParseInt(a, 10, 64)
		if err != nil {
			return 0, size, false, true
		}
		if s >= size {
			return 0, 0, false, false
		}
		e := size
		if b != "" {
			last, err := strconv.ParseInt(b, 10, 64)
			if err != nil || last < s {
				return 0, size, false, true
			}
			if last+1 < size {
				e = last + 1
			}
		}
		return s, e, true, true
	}
}

// metadataOnlyInsert is POST /storage/v1/b/{bucket}/o: an upload sent to the
// metadata URL, which Google answers 400 wrongUrlForUpload.
func (s *Server) metadataOnlyInsert(w http.ResponseWriter, r *http.Request) {
	writeError(w, errorf(http.StatusBadRequest, "wrongUrlForUpload",
		"Upload requests must include an uploadType URL parameter and a URL path beginning with /upload/"))
}
