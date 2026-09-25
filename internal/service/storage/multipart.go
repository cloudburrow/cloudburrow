package storage

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// XML API multipart uploads (#508), to docs.cloud.google.com/storage/docs/multipart-uploads:
// initiate (POST ?uploads), upload a part (PUT ?partNumber=N&uploadId=, N
// from 1 to 10,000; the same number again replaces it), complete (POST
// ?uploadId= with the parts in order and their ETags; every part but the
// last at least 5 MiB), abort (DELETE ?uploadId=, 204), list parts (GET
// ?uploadId=) and list uploads (GET /{bucket}?uploads). The object has no
// MD5, as documented; its CRC32C is the joined bytes'. Preconditions are
// not supported (400 NotImplemented). An upload no one completes stays
// until it is aborted, or a lifecycle rule's AbortIncompleteMultipartUpload
// removes it (#501).

const (
	mpuPrefix    = "mpu/"
	maxPartCount = 10000
	minPartSize  = 5 << 20
	maxListParts = 1000
	s3NS         = "http://s3.amazonaws.com/doc/2006-03-01/"
)

type mpuPart struct {
	Blob     string    `json:"blob"`
	Size     int64     `json:"size"`
	MD5      string    `json:"md5"` // hex, the part's ETag
	Modified time.Time `json:"modified"`
}

type multipartUpload struct {
	ID        string          `json:"id"`
	Bucket    string          `json:"bucket"`
	Meta      uploadMeta      `json:"meta"`
	Initiated time.Time       `json:"initiated"`
	Parts     map[int]mpuPart `json:"parts"`
}

func mpuKey(bucket, id string) string { return mpuPrefix + bucket + "/" + id }

func getUpload(tx Tx, bucket, id string) (multipartUpload, bool, error) {
	var u multipartUpload
	raw, ok := tx.Get(mpuKey(bucket, id))
	if !ok {
		return u, false, nil
	}
	return u, true, json.Unmarshal(raw, &u)
}

func putUpload(tx Tx, u multipartUpload) error {
	raw, err := json.Marshal(u)
	if err != nil {
		return err
	}
	tx.Put(mpuKey(u.Bucket, u.ID), raw)
	return nil
}

func writeXML(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/xml; charset=UTF-8")
	_, _ = w.Write([]byte(xml.Header))
	_ = xml.NewEncoder(w).Encode(v)
}

// refuseMPUPreconditions: "Preconditions are not supported in the
// requests" (multipart-uploads docs).
func refuseMPUPreconditions(w http.ResponseWriter, r *http.Request) bool {
	for k := range r.Header {
		if strings.HasPrefix(strings.ToLower(k), "x-goog-if-") {
			writeXMLError(w, http.StatusBadRequest, "NotImplemented", "Preconditions are not supported in multipart upload requests ("+k+")")
			return true
		}
	}
	return false
}

func noSuchUpload(w http.ResponseWriter) {
	writeXMLError(w, http.StatusNotFound, "NoSuchUpload", "The requested upload was not found.")
}

func (s *Server) mpuInitiate(w http.ResponseWriter, r *http.Request, bucket, name string) {
	if refuseMPUPreconditions(w, r) {
		return
	}
	meta, err := xmlMeta(r, name)
	if err == nil {
		err = s.bucketExists(bucket)
	}
	if err != nil {
		writeXMLFromError(w, r, err, bucket)
		return
	}
	u := multipartUpload{ID: newUploadID(), Bucket: bucket, Meta: meta, Initiated: s.now(), Parts: map[int]mpuPart{}}
	if err := s.meta.Update(func(tx Tx) error { return putUpload(tx, u) }); err != nil {
		writeXMLFromError(w, r, err, bucket)
		return
	}
	writeXML(w, struct {
		XMLName  xml.Name `xml:"InitiateMultipartUploadResult"`
		XMLNS    string   `xml:"xmlns,attr"`
		Bucket   string   `xml:"Bucket"`
		Key      string   `xml:"Key"`
		UploadID string   `xml:"UploadId"`
	}{XMLNS: s3NS, Bucket: bucket, Key: name, UploadID: u.ID})
}

func (s *Server) mpuPut(w http.ResponseWriter, r *http.Request, bucket, name, id string) {
	if refuseMPUPreconditions(w, r) {
		return
	}
	n, err := strconv.Atoi(r.URL.Query().Get("partNumber"))
	if err != nil || n < 1 || n > maxPartCount {
		writeXMLError(w, http.StatusBadRequest, "InvalidArgument", fmt.Sprintf("partNumber must be 1 to %d", maxPartCount))
		return
	}
	var u multipartUpload
	var found bool
	_ = s.meta.View(func(tx Tx) error {
		var err error
		u, found, err = getUpload(tx, bucket, id)
		return err
	})
	if !found || u.Meta.Name != name {
		noSuchUpload(w)
		return
	}
	blob, err := s.blobs.Write(r.Body)
	if err != nil {
		writeXMLFromError(w, r, err, bucket)
		return
	}
	if want := r.Header.Get("Content-MD5"); want != "" && want != b64(blob.MD5) {
		writeXMLError(w, http.StatusBadRequest, "BadDigest", "The Content-MD5 you specified did not match what was received.")
		return
	}
	p := mpuPart{Blob: blob.ID, Size: blob.Size, MD5: hex.EncodeToString(blob.MD5), Modified: s.now()}
	err = s.meta.Update(func(tx Tx) error {
		cur, ok, err := getUpload(tx, bucket, id)
		if err != nil {
			return err
		} else if !ok {
			return notFound("The requested upload was not found.")
		}
		cur.Parts[n] = p
		return putUpload(tx, cur)
	})
	if err != nil {
		noSuchUpload(w)
		return
	}
	w.Header().Set("ETag", `"`+p.MD5+`"`)
	w.Header().Set("X-Goog-Hash", "crc32c="+crcBase64(blob.CRC32C)+",md5="+b64(blob.MD5))
	w.WriteHeader(http.StatusOK)
}

func (s *Server) mpuComplete(w http.ResponseWriter, r *http.Request, bucket, name, id string) {
	if refuseMPUPreconditions(w, r) {
		return
	}
	var req struct {
		Parts []struct {
			PartNumber int    `xml:"PartNumber"`
			ETag       string `xml:"ETag"`
		} `xml:"Part"`
	}
	raw, _ := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err := xml.Unmarshal(raw, &req); err != nil || len(req.Parts) == 0 {
		writeXMLError(w, http.StatusBadRequest, "MalformedXML", "The body is not a CompleteMultipartUpload with at least one Part.")
		return
	}
	var u multipartUpload
	var found bool
	_ = s.meta.View(func(tx Tx) error {
		var err error
		u, found, err = getUpload(tx, bucket, id)
		return err
	})
	if !found || u.Meta.Name != name {
		noSuchUpload(w)
		return
	}
	// UNVERIFIED: the codes for a part out of order, a bad ETag and a small
	// part are S3's (InvalidPartOrder, InvalidPart, EntityTooSmall); no
	// Cloud Storage page states them.
	readers := make([]io.Reader, 0, len(req.Parts))
	closers := make([]io.Closer, 0, len(req.Parts))
	defer func() {
		for _, c := range closers {
			_ = c.Close()
		}
	}()
	prev := 0
	for _, rp := range req.Parts {
		if rp.PartNumber <= prev {
			writeXMLError(w, http.StatusBadRequest, "InvalidPartOrder", "The parts must be listed in ascending order of part number.")
			return
		}
		prev = rp.PartNumber
	}
	hashes := md5.New()
	for i, rp := range req.Parts {
		p, ok := u.Parts[rp.PartNumber]
		if !ok || strings.Trim(rp.ETag, `"`) != p.MD5 {
			writeXMLError(w, http.StatusBadRequest, "InvalidPart", fmt.Sprintf("Part %d was not uploaded, or its ETag does not match.", rp.PartNumber))
			return
		}
		if i < len(req.Parts)-1 && p.Size < minPartSize {
			writeXMLError(w, http.StatusBadRequest, "EntityTooSmall", fmt.Sprintf("Part %d is %d bytes; every part but the last must be at least 5 MiB.", rp.PartNumber, p.Size))
			return
		}
		f, err := s.blobs.Open(p.Blob)
		if err != nil {
			writeXMLFromError(w, r, err, bucket)
			return
		}
		readers, closers = append(readers, f), append(closers, f)
		sum, _ := hex.DecodeString(p.MD5)
		hashes.Write(sum)
	}
	blob, err := s.blobs.Write(io.MultiReader(readers...))
	if err != nil {
		writeXMLFromError(w, r, err, bucket)
		return
	}
	// "MD5 hashes don't exist for objects uploaded using this method."
	blob.MD5 = nil
	o, err := s.finalizeObject(bucket, u.Meta, blob, objectPreconditions{}, nil, nil)
	if err != nil {
		writeXMLFromError(w, r, err, bucket)
		return
	}
	_ = s.meta.Update(func(tx Tx) error { tx.Delete(mpuKey(bucket, id)); return nil })
	// The ETag is S3's composite form, the MD5 of the parts' MD5s and the
	// part count (UNVERIFIED for Cloud Storage).
	etag := fmt.Sprintf(`"%x-%d"`, hashes.Sum(nil), len(req.Parts))
	w.Header().Set("X-Goog-Generation", strconv.FormatInt(o.Generation, 10))
	w.Header().Set("X-Goog-Hash", "crc32c="+crcBase64(o.CRC32C))
	writeXML(w, struct {
		XMLName  xml.Name `xml:"CompleteMultipartUploadResult"`
		XMLNS    string   `xml:"xmlns,attr"`
		Location string   `xml:"Location"`
		Bucket   string   `xml:"Bucket"`
		Key      string   `xml:"Key"`
		ETag     string   `xml:"ETag"`
	}{XMLNS: s3NS, Location: baseURL(r) + "/" + escape(bucket) + "/" + escapePath(name), Bucket: bucket, Key: name, ETag: etag})
}

func (s *Server) mpuAbort(w http.ResponseWriter, r *http.Request, bucket, name, id string) {
	err := s.meta.Update(func(tx Tx) error {
		u, ok, err := getUpload(tx, bucket, id)
		if err != nil {
			return err
		} else if !ok || u.Meta.Name != name {
			return notFound("The requested upload was not found.")
		}
		tx.Delete(mpuKey(bucket, id))
		return nil
	})
	if err != nil {
		noSuchUpload(w)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) mpuListParts(w http.ResponseWriter, r *http.Request, bucket, name, id string) {
	q := r.URL.Query()
	max := maxListParts
	if v := q.Get("max-parts"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeXMLError(w, http.StatusBadRequest, "InvalidArgument", "max-parts must be a non-negative integer")
			return
		}
		if n < max {
			max = n
		}
	}
	marker, _ := strconv.Atoi(q.Get("part-number-marker"))
	var u multipartUpload
	var found bool
	_ = s.meta.View(func(tx Tx) error {
		var err error
		u, found, err = getUpload(tx, bucket, id)
		return err
	})
	if !found || u.Meta.Name != name {
		noSuchUpload(w)
		return
	}
	nums := make([]int, 0, len(u.Parts))
	for n := range u.Parts {
		if n > marker {
			nums = append(nums, n)
		}
	}
	sort.Ints(nums)
	type part struct {
		PartNumber   int    `xml:"PartNumber"`
		LastModified string `xml:"LastModified"`
		ETag         string `xml:"ETag"`
		Size         int64  `xml:"Size"`
	}
	res := struct {
		XMLName              xml.Name `xml:"ListPartsResult"`
		XMLNS                string   `xml:"xmlns,attr"`
		Bucket               string   `xml:"Bucket"`
		Key                  string   `xml:"Key"`
		UploadID             string   `xml:"UploadId"`
		PartNumberMarker     int      `xml:"PartNumberMarker"`
		NextPartNumberMarker int      `xml:"NextPartNumberMarker,omitempty"`
		MaxParts             int      `xml:"MaxParts"`
		IsTruncated          bool     `xml:"IsTruncated"`
		Parts                []part   `xml:"Part"`
	}{XMLNS: s3NS, Bucket: bucket, Key: name, UploadID: id, PartNumberMarker: marker, MaxParts: max}
	for _, n := range nums {
		if len(res.Parts) == max {
			res.IsTruncated = true
			res.NextPartNumberMarker = res.Parts[len(res.Parts)-1].PartNumber
			break
		}
		p := u.Parts[n]
		res.Parts = append(res.Parts, part{PartNumber: n, LastModified: p.Modified.UTC().Format(time.RFC3339Nano), ETag: `"` + p.MD5 + `"`, Size: p.Size})
	}
	writeXML(w, res)
}

func (s *Server) mpuListUploads(w http.ResponseWriter, r *http.Request, bucket string) {
	q := r.URL.Query()
	prefix := q.Get("prefix")
	max := maxListParts
	if v := q.Get("max-uploads"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 && n < max {
			max = n
		}
	}
	keyMarker, idMarker := q.Get("key-marker"), q.Get("upload-id-marker")
	var ups []multipartUpload
	err := s.meta.View(func(tx Tx) error {
		if _, ok, err := s.getBucket(tx, bucket); err != nil {
			return err
		} else if !ok {
			return notFound("The specified bucket does not exist.")
		}
		for _, k := range tx.List(mpuPrefix + bucket + "/") {
			raw, _ := tx.Get(k)
			var u multipartUpload
			if err := json.Unmarshal(raw, &u); err != nil {
				return err
			}
			if strings.HasPrefix(u.Meta.Name, prefix) {
				ups = append(ups, u)
			}
		}
		return nil
	})
	if err != nil {
		writeXMLFromError(w, r, err, bucket)
		return
	}
	sort.Slice(ups, func(i, j int) bool {
		if ups[i].Meta.Name != ups[j].Meta.Name {
			return ups[i].Meta.Name < ups[j].Meta.Name
		}
		return ups[i].ID < ups[j].ID
	})
	type upload struct {
		Key       string `xml:"Key"`
		UploadID  string `xml:"UploadId"`
		Initiated string `xml:"Initiated"`
	}
	res := struct {
		XMLName            xml.Name `xml:"ListMultipartUploadsResult"`
		XMLNS              string   `xml:"xmlns,attr"`
		Bucket             string   `xml:"Bucket"`
		KeyMarker          string   `xml:"KeyMarker"`
		UploadIDMarker     string   `xml:"UploadIdMarker"`
		NextKeyMarker      string   `xml:"NextKeyMarker,omitempty"`
		NextUploadIDMarker string   `xml:"NextUploadIdMarker,omitempty"`
		Prefix             string   `xml:"Prefix"`
		MaxUploads         int      `xml:"MaxUploads"`
		IsTruncated        bool     `xml:"IsTruncated"`
		Uploads            []upload `xml:"Upload"`
	}{XMLNS: s3NS, Bucket: bucket, KeyMarker: keyMarker, UploadIDMarker: idMarker, Prefix: prefix, MaxUploads: max}
	for _, u := range ups {
		if keyMarker != "" && (u.Meta.Name < keyMarker || u.Meta.Name == keyMarker && u.ID <= idMarker) {
			continue
		}
		if len(res.Uploads) == max {
			last := res.Uploads[len(res.Uploads)-1]
			res.IsTruncated, res.NextKeyMarker, res.NextUploadIDMarker = true, last.Key, last.UploadID
			break
		}
		res.Uploads = append(res.Uploads, upload{Key: u.Meta.Name, UploadID: u.ID, Initiated: u.Initiated.UTC().Format(time.RFC3339Nano)})
	}
	writeXML(w, res)
}

// abortIncompleteUploads is lifecycle's AbortIncompleteMultipartUpload
// (#501): uploads at least age days old whose name matches.
func abortIncompleteUploads(tx Tx, bucket string, c lifecycleCondition, now time.Time) (int, error) {
	n := 0
	for _, k := range tx.List(mpuPrefix + bucket + "/") {
		raw, _ := tx.Get(k)
		var u multipartUpload
		if err := json.Unmarshal(raw, &u); err != nil {
			return n, err
		}
		if c.Age != nil && now.Before(u.Initiated.Add(time.Duration(*c.Age)*24*time.Hour)) {
			continue
		}
		if c.Prefixes != nil && !anyAffix(u.Meta.Name, c.Prefixes, strings.HasPrefix) ||
			c.Suffixes != nil && !anyAffix(u.Meta.Name, c.Suffixes, strings.HasSuffix) {
			continue
		}
		tx.Delete(k)
		n++
	}
	return n, nil
}
