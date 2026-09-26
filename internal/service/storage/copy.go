package storage

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Compose, copy, rewrite and move (#495). Bytes are content-addressed, so a
// copy or a rewrite points the new generation at the same blob; only compose
// writes new bytes, and its CRC32C, computed over the joined bytes, is the
// combination of the components'.

const (
	rewritePrefix   = "rewrite/"
	rewriteTokenTTL = 7 * 24 * time.Hour
	rewriteUnit     = 1 << 20
	maxComponents   = 32
)

// objectFromMeta builds a new generation's record from metadata the client
// sent, over a base record whose fields fill what the metadata leaves out.
func applyDestination(o *objectRecord, body map[string]any) error {
	if len(body) == 0 {
		return nil
	}
	if err := checkObjectBody(body); err != nil {
		return err
	}
	return applyObjectBody(o, body, false)
}

// commitNew makes o a new live generation at its bucket and name, under dst
// preconditions, retiring the version it replaces (#498). also, if set, runs
// in the same transaction, to remove the sources of a move or a compose.
func (s *Server) commitNew(o *objectRecord, pre objectPreconditions, also func(tx Tx, b bucketRecord, now time.Time) error) error {
	now := s.now()
	o.Generation, o.Metageneration, o.Created, o.Updated = s.nextGeneration(), 1, now, now
	o.Deleted = time.Time{}
	return s.meta.Update(func(tx Tx) error {
		b, ok, err := s.getBucket(tx, o.Bucket)
		if err != nil {
			return err
		} else if !ok {
			return notFound("The specified bucket does not exist.")
		}
		if o.StorageClass == "" {
			o.StorageClass, _ = b.Fields["storageClass"].(string)
		}
		cur, exists, err := getObject(tx, o.Bucket, o.Name)
		if err != nil {
			return err
		}
		if err := pre.check(cur, exists, false); err != nil {
			return err
		}
		if also != nil {
			if err := also(tx, b, now); err != nil {
				return err
			}
		}
		if err := retireLive(tx, b, o.Name, now); err != nil {
			return err
		}
		return putObject(tx, *o)
	})
}

// sourceObject reads a copy's source under ifSource* preconditions and
// sourceGeneration, which may name a noncurrent version (#498); live says
// whether it is the live one.
func (s *Server) sourceObject(q url.Values, bucket, name string) (o objectRecord, live bool, err error) {
	pre, err := parseObjectPreconditions(q, "ifSource")
	if err != nil {
		return objectRecord{}, false, err
	}
	err = s.meta.View(func(tx Tx) error {
		if _, ok, gerr := s.getBucket(tx, bucket); gerr != nil {
			return gerr
		} else if !ok {
			return notFound("The specified bucket does not exist.")
		}
		var exists bool
		var gerr error
		if o, live, exists, gerr = findVersion(tx, bucket, name, q.Get("sourceGeneration")); gerr != nil {
			return gerr
		}
		if !exists {
			return notFound("No such object: %s/%s", bucket, name)
		}
		return pre.check(o, true, false)
	})
	return o, live, err
}

func refuseCopyParams(q url.Values) error {
	for _, p := range []string{"destinationPredefinedAcl", "destinationKmsKeyName", "kmsKeyName", "dropContextGroups"} {
		if q.Get(p) != "" {
			return badRequest("%s is not supported by CloudBurrow; it is refused rather than ignored", p)
		}
	}
	return nil
}

// copyPath is b/{srcBucket}/o/{srcObject}/{verb}To/b/{dstBucket}/o/{dstObject}.
func copyNames(r *http.Request) (srcB, srcO, dstB, dstO string) {
	return pathVar(r, jsonPrefix, 1), pathVar(r, jsonPrefix, 3), pathVar(r, jsonPrefix, 6), pathVar(r, jsonPrefix, 8)
}

func (s *Server) objectsCopy(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if err := refuseCopyParams(q); err != nil {
		writeError(w, err)
		return
	}
	srcB, srcO, dstB, dstO := copyNames(r)
	body, err := readBody(r)
	if err != nil {
		writeError(w, err)
		return
	}
	src, _, err := s.sourceObject(q, srcB, srcO)
	if err != nil {
		writeError(w, err)
		return
	}
	pre, err := parseObjectPreconditions(q, "if")
	if err != nil {
		writeError(w, err)
		return
	}
	o := src
	o.Bucket, o.Name, o.StorageClass = dstB, dstO, ""
	if len(body) > 0 {
		// A copy with a body takes its metadata from the body.
		o.ContentType, o.ContentEncoding, o.ContentDisposition, o.ContentLanguage, o.CacheControl, o.Metadata = "", "", "", "", "", nil
		if err := applyDestination(&o, body); err != nil {
			writeError(w, err)
			return
		}
		if o.ContentType == "" {
			o.ContentType = src.ContentType
		}
	}
	if err := validObjectName(dstO); err != nil {
		writeError(w, err)
		return
	}
	if err := s.commitNew(&o, pre, nil); err != nil {
		writeError(w, err)
		return
	}
	writeResponse(w, r, http.StatusOK, s.objectJSON(r, o))
}

type rewriteState struct {
	Token     string         `json:"token"`
	Source    objectRecord   `json:"source"`
	DstBucket string         `json:"dstBucket"`
	DstName   string         `json:"dstName"`
	Body      map[string]any `json:"body,omitempty"`
	// StorageClass is what a rewrite exists to change; empty keeps the
	// destination bucket's default.
	StorageClass string    `json:"storageClass,omitempty"`
	Done         int64     `json:"done"`
	Created      time.Time `json:"created"`
	Used         bool      `json:"used"`
}

func (s *Server) objectsRewrite(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if err := refuseCopyParams(q); err != nil {
		writeError(w, err)
		return
	}
	per := int64(0)
	if v := q.Get("maxBytesRewrittenPerCall"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 || n%rewriteUnit != 0 {
			writeError(w, badRequest("Invalid argument: maxBytesRewrittenPerCall must be a positive multiple of 1048576"))
			return
		}
		per = n
	}
	srcB, srcO, dstB, dstO := copyNames(r)
	body, err := readBody(r)
	if err != nil {
		writeError(w, err)
		return
	}
	pre, err := parseObjectPreconditions(q, "if")
	if err != nil {
		writeError(w, err)
		return
	}
	var st rewriteState
	if tok := q.Get("rewriteToken"); tok != "" {
		var found bool
		if err := s.meta.View(func(tx Tx) error {
			raw, ok := tx.Get(rewritePrefix + tok)
			found = ok
			if !ok {
				return nil
			}
			return json.Unmarshal(raw, &st)
		}); err != nil {
			writeError(w, err)
			return
		}
		if !found || st.Used || s.now().Sub(st.Created) >= rewriteTokenTTL {
			writeError(w, errorf(http.StatusGone, "gone", "The rewrite token is not valid: it has been used or has expired"))
			return
		}
		if st.DstBucket != dstB || st.DstName != dstO || st.Source.Bucket != srcB || st.Source.Name != srcO {
			writeError(w, badRequest("The rewrite token belongs to a different rewrite"))
			return
		}
	} else {
		src, _, err := s.sourceObject(q, srcB, srcO)
		if err != nil {
			writeError(w, err)
			return
		}
		// storageClass is what a rewrite exists to change; the rest of the
		// body is ordinary destination metadata.
		class := ""
		if sc, ok := body["storageClass"]; ok {
			class = strings.ToUpper(str(sc))
			delete(body, "storageClass")
			if class != "" && !storageClasses[class] {
				writeError(w, badRequest("Invalid argument: storageClass %v", sc))
				return
			}
		}
		if err := checkObjectBody(body); err != nil {
			writeError(w, err)
			return
		}
		st = rewriteState{Token: newUploadID(), Source: src, DstBucket: dstB, DstName: dstO, Body: body, StorageClass: class, Created: s.now()}
	}
	step := st.Source.Size - st.Done
	if per > 0 && step > per {
		step = per
	}
	st.Done += step
	resp := map[string]any{"kind": "storage#rewriteResponse", "totalBytesRewritten": strconv.FormatInt(st.Done, 10),
		"objectSize": strconv.FormatInt(st.Source.Size, 10)}
	if st.Done < st.Source.Size {
		if err := s.meta.Update(func(tx Tx) error {
			raw, _ := json.Marshal(st)
			tx.Put(rewritePrefix+st.Token, raw)
			return nil
		}); err != nil {
			writeError(w, err)
			return
		}
		resp["done"] = false
		resp["rewriteToken"] = st.Token
		writeResponse(w, r, http.StatusOK, resp)
		return
	}
	o := st.Source
	o.Bucket, o.Name, o.StorageClass = st.DstBucket, st.DstName, st.StorageClass
	if len(st.Body) > 0 {
		o.ContentType, o.ContentEncoding, o.ContentDisposition, o.ContentLanguage, o.CacheControl, o.Metadata = "", "", "", "", "", nil
		if err := applyDestination(&o, st.Body); err != nil {
			writeError(w, err)
			return
		}
		if o.ContentType == "" {
			o.ContentType = st.Source.ContentType
		}
	}
	if err := s.commitNew(&o, pre, nil); err != nil {
		writeError(w, err)
		return
	}
	st.Used = true
	_ = s.meta.Update(func(tx Tx) error {
		raw, _ := json.Marshal(st)
		tx.Put(rewritePrefix+st.Token, raw)
		return nil
	})
	resp["done"] = true
	resp["resource"] = s.objectJSON(r, o)
	writeResponse(w, r, http.StatusOK, resp)
}

type composeRequest struct {
	Destination   map[string]any `json:"destination"`
	SourceObjects []struct {
		Name                string `json:"name"`
		Generation          string `json:"generation"`
		ObjectPreconditions struct {
			IfGenerationMatch string `json:"ifGenerationMatch"`
		} `json:"objectPreconditions"`
	} `json:"sourceObjects"`
	KMSKeyName        string   `json:"kmsKeyName"`
	DropContextGroups []string `json:"dropContextGroups"`
}

func (s *Server) objectsCompose(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if err := refuseCopyParams(q); err != nil {
		writeError(w, err)
		return
	}
	bucket, name := pathVar(r, jsonPrefix, 1), pathVar(r, jsonPrefix, 3)
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeError(w, badRequest("the request body could not be read"))
		return
	}
	var req composeRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeError(w, errorf(http.StatusBadRequest, "parseError", "Parse Error: the compose request is not valid JSON"))
		return
	}
	if req.KMSKeyName != "" {
		writeError(w, badRequest("kmsKeyName is not supported by CloudBurrow; it is refused rather than ignored"))
		return
	}
	if len(req.DropContextGroups) > 0 {
		writeError(w, badRequest("dropContextGroups is not supported by CloudBurrow; it is refused rather than ignored"))
		return
	}
	if n := len(req.SourceObjects); n < 1 || n > maxComponents {
		writeError(w, badRequest("A compose request takes 1 to %d source objects, not %d", maxComponents, n))
		return
	}
	pre, err := parseObjectPreconditions(q, "if")
	if err != nil {
		writeError(w, err)
		return
	}
	var sources []objectRecord
	components := 0
	if err := s.meta.View(func(tx Tx) error {
		if _, ok, gerr := s.getBucket(tx, bucket); gerr != nil {
			return gerr
		} else if !ok {
			return notFound("The specified bucket does not exist.")
		}
		for _, so := range req.SourceObjects {
			o, _, exists, gerr := findVersion(tx, bucket, so.Name, so.Generation)
			if gerr != nil {
				return gerr
			}
			if !exists {
				return notFound("No such object: %s/%s", bucket, so.Name)
			}
			if m := so.ObjectPreconditions.IfGenerationMatch; m != "" && m != strconv.FormatInt(o.Generation, 10) {
				return preconditionFailed("At least one of the pre-conditions you specified did not hold.")
			}
			if len(sources) > 0 && o.StorageClass != sources[0].StorageClass {
				return badRequest("The source objects of a compose must share a storage class")
			}
			sources = append(sources, o)
			if o.ComponentCount > 0 {
				components += o.ComponentCount
			} else {
				components++
			}
		}
		return nil
	}); err != nil {
		writeError(w, err)
		return
	}
	readers := make([]io.Reader, 0, len(sources))
	closers := make([]io.Closer, 0, len(sources))
	defer func() {
		for _, c := range closers {
			_ = c.Close()
		}
	}()
	for _, o := range sources {
		f, err := s.blobs.Open(o.Blob)
		if err != nil {
			writeError(w, err)
			return
		}
		readers, closers = append(readers, f), append(closers, f)
	}
	blob, err := s.blobs.Write(io.MultiReader(readers...))
	if err != nil {
		writeError(w, err)
		return
	}
	// A composite object has no MD5 (composite-objects docs); its CRC32C is
	// the components' combined, which is the CRC32C of the joined bytes.
	o := objectRecord{Bucket: bucket, Name: name, Blob: blob.ID, Size: blob.Size, CRC32C: blob.CRC32C,
		ComponentCount: components, StorageClass: sources[0].StorageClass}
	if err := validObjectName(name); err != nil {
		writeError(w, err)
		return
	}
	if err := applyDestination(&o, req.Destination); err != nil {
		writeError(w, err)
		return
	}
	if o.ContentType == "" {
		o.ContentType = "application/octet-stream"
	}
	var drop func(Tx, bucketRecord, time.Time) error
	if q.Get("deleteSourceObjects") == "true" {
		drop = func(tx Tx, b bucketRecord, now time.Time) error {
			for _, src := range sources {
				if src.Name != name {
					if err := retireLive(tx, b, src.Name, now); err != nil {
						return err
					}
				}
			}
			return nil
		}
	}
	if err := s.commitNew(&o, pre, drop); err != nil {
		writeError(w, err)
		return
	}
	writeResponse(w, r, http.StatusOK, s.objectJSON(r, o))
}

// objectsMove is an atomic rename within one bucket: the source goes and the
// destination appears in one transaction.
func (s *Server) objectsMove(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	bucket, src, dst := pathVar(r, jsonPrefix, 1), pathVar(r, jsonPrefix, 3), pathVar(r, jsonPrefix, 6)
	if err := validObjectName(dst); err != nil {
		writeError(w, err)
		return
	}
	o, live, err := s.sourceObject(q, bucket, src)
	if err != nil {
		writeError(w, err)
		return
	}
	pre, err := parseObjectPreconditions(q, "if")
	if err != nil {
		writeError(w, err)
		return
	}
	moved := o
	o.Name = dst
	// The source goes as a delete would: a live source is retired, a
	// noncurrent one named by generation is removed.
	remove := func(tx Tx, b bucketRecord, now time.Time) error {
		if live {
			return retireLive(tx, b, src, now)
		}
		return deleteVersion(tx, moved)
	}
	if err := s.commitNew(&o, pre, remove); err != nil {
		writeError(w, err)
		return
	}
	writeResponse(w, r, http.StatusOK, s.objectJSON(r, o))
}
