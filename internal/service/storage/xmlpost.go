package storage

import (
	"bytes"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// The XML API's service GET, bucket PUT and form POST (#1258), to
// docs.cloud.google.com/storage/docs/xml-api/get-service,
// .../xml-api/put-bucket-create and .../xml-api/post-object-form.
//
// The project a service GET lists, or a bucket PUT creates in, is the
// x-goog-project-id header; without it, the project of the HMAC key named
// in an Authorization header (the interoperability default project); and
// without either the request is 400 MissingSecurityHeader.
//
// A form POST stores the form's file field under its key field. With a
// policy field the policy document is checked as Google documents it: the
// expiration, and every condition (exact-match objects, eq, starts-with and
// content-length-range), and every form field must be named by a condition
// (x-goog-signature, signature, GoogleAccessId, policy, file and x-ignore-*
// excepted). A signature on the policy is verified as a signed URL's is
// (#509), failing closed: x-goog-signature with GOOG4-RSA-SHA256 or
// GOOG4-HMAC-SHA256 over the base64 policy, or V2's signature with
// GoogleAccessId. With neither a policy nor a signature the upload is
// served unsigned, as every unsigned request is (ADR-0004).

// xmlProject is the project a service request acts in.
func (s *Server) xmlProject(r *http.Request) (string, bool) {
	if p := r.Header.Get("X-Goog-Project-Id"); p != "" {
		return p, true
	}
	id := authAccessID(r.Header.Get("Authorization"))
	if id == "" {
		return "", false
	}
	project := ""
	_ = s.meta.View(func(tx Tx) error {
		for _, key := range tx.List(hmacPrefix) {
			if !strings.HasSuffix(key, "/"+id) {
				continue
			}
			raw, _ := tx.Get(key)
			var k hmacKey
			if json.Unmarshal(raw, &k) == nil && k.AccessID == id {
				project = k.Project
			}
		}
		return nil
	})
	return project, project != ""
}

// authAccessID is the access ID in a GOOG1/AWS (V2) or GOOG4/AWS4 (V4)
// HMAC Authorization header.
func authAccessID(h string) string {
	alg, rest, _ := strings.Cut(h, " ")
	switch alg {
	case "GOOG1", "AWS":
		id, _, _ := strings.Cut(rest, ":")
		return strings.TrimSpace(id)
	case "GOOG4-HMAC-SHA256", "AWS4-HMAC-SHA256":
		for _, part := range strings.Split(rest, ",") {
			if v, ok := strings.CutPrefix(strings.TrimSpace(part), "Credential="); ok {
				id, _, _ := strings.Cut(v, "/")
				return id
			}
		}
	}
	return ""
}

func missingProject(w http.ResponseWriter) {
	// UNVERIFIED: the code and Details are what Google answers a service
	// Docs re-read 2026-10-17, still unanswered: https://docs.cloud.google.com/storage/docs/xml-api/get-service
	// request with no project, as far as we know; not checked against
	// production.
	w.Header().Set("Content-Type", "application/xml; charset=UTF-8")
	w.WriteHeader(http.StatusBadRequest)
	_, _ = w.Write([]byte(xml.Header))
	_ = xml.NewEncoder(w).Encode(xmlError{Code: "MissingSecurityHeader",
		Message: "Your request was missing a required header.", Details: "x-goog-project-id"})
}

// ownerID is the opaque canonical ID a listing names the owner by.
func ownerID(project string) string {
	sum := sha256.Sum256([]byte("project-owners-" + project))
	return hex.EncodeToString(sum[:])
}

// xmlListBuckets is GET / on the service: the project's buckets as a
// ListAllMyBucketsResult.
func (s *Server) xmlListBuckets(w http.ResponseWriter, r *http.Request) {
	project, ok := s.xmlProject(r)
	if !ok {
		missingProject(w)
		return
	}
	type bucketEntry struct {
		Name         string `xml:"Name"`
		CreationDate string `xml:"CreationDate"`
	}
	type owner struct {
		ID          string `xml:"ID"`
		DisplayName string `xml:"DisplayName"`
	}
	type result struct {
		XMLName xml.Name      `xml:"ListAllMyBucketsResult"`
		XMLNS   string        `xml:"xmlns,attr"`
		Owner   owner         `xml:"Owner"`
		Buckets []bucketEntry `xml:"Buckets>Bucket"`
	}
	res := result{XMLNS: "http://doc.s3.amazonaws.com/2006-03-01", Owner: owner{ID: ownerID(project)}}
	err := s.meta.View(func(tx Tx) error {
		for _, key := range tx.List(bucketPrefix) {
			b, ok, err := s.getBucket(tx, strings.TrimPrefix(key, bucketPrefix))
			if err != nil {
				return err
			}
			if ok && b.Project == project {
				res.Buckets = append(res.Buckets, bucketEntry{b.Name, rfc3339(b.Created)})
			}
		}
		return nil
	})
	if err != nil {
		writeXMLFromError(w, r, err, "")
		return
	}
	w.Header().Set("Content-Type", "application/xml; charset=UTF-8")
	_, _ = w.Write([]byte(xml.Header))
	_ = xml.NewEncoder(w).Encode(res)
}

// xmlCannedBucketACL maps the XML API's x-goog-acl to a JSON API
// predefinedAcl (xml-api reference, canned ACLs).
var xmlCannedBucketACL = map[string]string{"private": "private", "project-private": "projectPrivate",
	"public-read": "publicRead", "public-read-write": "publicReadWrite", "authenticated-read": "authenticatedRead"}

var xmlCannedObjectACL = map[string]string{"private": "private", "project-private": "projectPrivate",
	"public-read": "publicRead", "authenticated-read": "authenticatedRead",
	"bucket-owner-read": "bucketOwnerRead", "bucket-owner-full-control": "bucketOwnerFullControl"}

// xmlCreateBucket is PUT /{bucket} with no subresource: an optional
// CreateBucketConfiguration body (LocationConstraint, StorageClass), with
// x-goog-project-id, x-goog-acl and x-goog-bucket-object-lock-enabled.
// It runs buckets.insert, so the two APIs create the same bucket.
func (s *Server) xmlCreateBucket(w http.ResponseWriter, r *http.Request, bucket string) {
	project, ok := s.xmlProject(r)
	if !ok {
		missingProject(w)
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeXMLError(w, http.StatusBadRequest, "InvalidArgument", "the request body could not be read")
		return
	}
	var cfg struct {
		XMLName            xml.Name
		LocationConstraint string     `xml:"LocationConstraint"`
		StorageClass       string     `xml:"StorageClass"`
		Other              []xml.Name `xml:",any"`
	}
	body := map[string]any{"name": bucket}
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := xml.Unmarshal(raw, &cfg); err != nil || cfg.XMLName.Local != "CreateBucketConfiguration" {
			writeXMLError(w, http.StatusBadRequest, "MalformedXML", "The XML you provided was not well-formed or did not validate against our published schema.")
			return
		}
		if len(cfg.Other) > 0 {
			writeXMLError(w, http.StatusBadRequest, "InvalidArgument",
				"CreateBucketConfiguration element <"+cfg.Other[0].Local+"> is not supported by CloudBurrow; it is refused rather than ignored")
			return
		}
		if cfg.LocationConstraint != "" {
			body["location"] = cfg.LocationConstraint
		}
		if cfg.StorageClass != "" {
			body["storageClass"] = cfg.StorageClass
		}
	}
	q := url.Values{"project": {project}}
	if v := r.Header.Get("X-Goog-Acl"); v != "" {
		p, ok := xmlCannedBucketACL[v]
		if !ok {
			writeXMLError(w, http.StatusBadRequest, "InvalidArgument", "Invalid canned ACL x-goog-acl: "+v)
			return
		}
		q.Set("predefinedAcl", p)
	}
	if strings.EqualFold(r.Header.Get("X-Goog-Bucket-Object-Lock-Enabled"), "true") {
		q.Set("enableObjectRetention", "true")
	}
	var existing bucketRecord
	var exists bool
	_ = s.meta.View(func(tx Tx) error {
		existing, exists, err = s.getBucket(tx, bucket)
		return err
	})
	if exists {
		if existing.Project == project {
			writeXMLError(w, http.StatusConflict, "BucketAlreadyOwnedByYou", "Your previous request to create the named bucket succeeded and you already own it.")
		} else {
			writeXMLError(w, http.StatusConflict, "BucketAlreadyExists", "The requested bucket name is not available. The bucket namespace is shared by all users of the system. Please select a different name and try again.")
		}
		return
	}
	js, _ := json.Marshal(body)
	jr := r.Clone(r.Context())
	jr.Body = io.NopCloser(bytes.NewReader(js))
	jr.ContentLength = int64(len(js))
	jr.Header.Set("Content-Type", "application/json")
	s.viaJSON(w, jr, s.bucketsInsert, jsonPrefix+"b", q, http.StatusOK)
}

// isFormPost reports whether a bucket POST is an HTML form upload.
func isFormPost(r *http.Request) bool {
	mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return r.Method == http.MethodPost && mt == "multipart/form-data"
}

// formExempt are the form fields a policy need not name.
func formExempt(name string) bool {
	switch name {
	case "file", "policy", "x-goog-signature", "signature", "googleaccessid":
		return true
	}
	return strings.HasPrefix(name, "x-ignore-")
}

// formKnown are the fields a form POST is served with; any other is
// refused rather than ignored.
func formKnown(name string) bool {
	switch name {
	case "key", "acl", "cache-control", "content-disposition", "content-encoding", "content-type",
		"success_action_status", "success_action_redirect", "x-goog-algorithm", "x-goog-credential", "x-goog-date":
		return true
	}
	return formExempt(name) || strings.HasPrefix(name, "x-goog-meta-")
}

// policyFail is a policy check's failure.
func policyFail(w http.ResponseWriter, msg string, a ...any) {
	// UNVERIFIED: the AccessDenied code for a failed condition or an
	// Docs re-read 2026-10-17, still unanswered: https://docs.cloud.google.com/storage/docs/xml-api/post-object-forms
	// expired policy follows the XML API's interoperable behaviour.
	writeXMLError(w, http.StatusForbidden, "AccessDenied", "Invalid according to Policy: "+fmt.Sprintf(msg, a...))
}

// postPolicy is a decoded policy document.
type postPolicy struct {
	expires    time.Time
	conditions [][]any // each as [op, field, value...]
	minLen     int64
	maxLen     int64
	hasRange   bool
	named      map[string]bool
}

func parsePostPolicy(b64policy string) (*postPolicy, error) {
	raw, err := base64.StdEncoding.DecodeString(b64policy)
	if err != nil {
		return nil, errors.New("the policy is not base64")
	}
	var doc struct {
		Expiration string            `json:"expiration"`
		Conditions []json.RawMessage `json:"conditions"`
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if err := d.Decode(&doc); err != nil {
		return nil, errors.New("the policy is not a JSON object")
	}
	if doc.Expiration == "" {
		return nil, errors.New("the policy has no expiration")
	}
	p := &postPolicy{named: map[string]bool{}}
	if p.expires, err = time.Parse(time.RFC3339Nano, doc.Expiration); err != nil {
		return nil, fmt.Errorf("the policy expiration %q is not ISO 8601", doc.Expiration)
	}
	for _, c := range doc.Conditions {
		var obj map[string]any
		if json.Unmarshal(c, &obj) == nil {
			if len(obj) != 1 {
				return nil, errors.New("an object condition names exactly one field")
			}
			for k, v := range obj {
				sv, ok := v.(string)
				if !ok {
					return nil, fmt.Errorf("the condition on %q is not a string", k)
				}
				name := strings.ToLower(k)
				p.conditions = append(p.conditions, []any{"eq", name, sv})
				p.named[name] = true
			}
			continue
		}
		var arr []any
		ad := json.NewDecoder(bytes.NewReader(c))
		ad.UseNumber()
		if ad.Decode(&arr) != nil || len(arr) != 3 {
			return nil, errors.New("a condition is an object or a three-element array")
		}
		op, _ := arr[0].(string)
		switch strings.ToLower(op) {
		case "eq", "starts-with":
			field, ok1 := arr[1].(string)
			val, ok2 := arr[2].(string)
			if !ok1 || !ok2 || !strings.HasPrefix(field, "$") {
				return nil, fmt.Errorf("the %s condition is [%q, \"$field\", \"value\"]", op, op)
			}
			name := strings.ToLower(field[1:])
			p.conditions = append(p.conditions, []any{strings.ToLower(op), name, val})
			p.named[name] = true
		case "content-length-range":
			lo, err1 := strconv.ParseInt(fmt.Sprint(arr[1]), 10, 64)
			hi, err2 := strconv.ParseInt(fmt.Sprint(arr[2]), 10, 64)
			if err1 != nil || err2 != nil || lo < 0 || hi < lo {
				return nil, errors.New("content-length-range is [\"content-length-range\", min, max]")
			}
			p.minLen, p.maxLen, p.hasRange = lo, hi, true
		default:
			return nil, fmt.Errorf("unknown condition %q", op)
		}
	}
	return p, nil
}

// verifyPolicySignature checks the policy's signature, as #509 checks a
// signed URL's; nil means serve it.
func (s *Server) verifyPolicySignature(fields map[string]string) error {
	policy := fields["policy"]
	if sig := fields["x-goog-signature"]; sig != "" {
		alg, cred, date := fields["x-goog-algorithm"], fields["x-goog-credential"], fields["x-goog-date"]
		parts := strings.Split(cred, "/")
		if alg == "" || len(parts) != 5 || date == "" {
			return &signatureError{http.StatusBadRequest, "InvalidArgument", "A signed policy needs x-goog-algorithm, x-goog-credential and x-goog-date."}
		}
		if !strings.HasPrefix(date, parts[1]) {
			return &signatureError{http.StatusBadRequest, "InvalidArgument", "x-goog-date is not in the credential's day."}
		}
		ok, err := s.checkV4(alg, parts[0], policy, sig, parts[1], parts[2], parts[3], parts[4])
		if err != nil {
			return err
		}
		if !ok {
			return mismatch()
		}
		return nil
	}
	if sig := fields["signature"]; sig != "" {
		id := fields["googleaccessid"]
		key, ok := s.signingKeys[id]
		if !ok {
			return denied("No signing certificate is registered for %s; register one (storage-server --signing-cert) to verify its signatures.", id)
		}
		raw, err := base64.StdEncoding.DecodeString(sig)
		if err != nil {
			return mismatch()
		}
		sum := sha256.Sum256([]byte(policy))
		if rsa.VerifyPKCS1v15(key, crypto.SHA256, sum[:], raw) != nil {
			return mismatch()
		}
	}
	return nil
}

var filenameVar = regexp.MustCompile(`\$\{filename\}`)

// xmlFormPost is POST /{bucket} with multipart/form-data: an HTML form or
// POST policy upload. The file field is the last one, as the docs require.
func (s *Server) xmlFormPost(w http.ResponseWriter, r *http.Request, bucket string) {
	mr, err := r.MultipartReader()
	if err != nil {
		writeXMLError(w, http.StatusBadRequest, "InvalidArgument", "The form is not multipart/form-data: "+err.Error())
		return
	}
	fields := map[string]string{}
	var order []string
	var part io.Reader
	filename := ""
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			writeXMLError(w, http.StatusBadRequest, "InvalidArgument", "The form could not be read: "+err.Error())
			return
		}
		name := strings.ToLower(p.FormName())
		if name == "file" {
			part, filename = p, p.FileName()
			break
		}
		v, err := io.ReadAll(io.LimitReader(p, 1<<20))
		if err != nil {
			writeXMLError(w, http.StatusBadRequest, "InvalidArgument", "The form could not be read: "+err.Error())
			return
		}
		if _, dup := fields[name]; dup {
			writeXMLError(w, http.StatusBadRequest, "InvalidArgument", "The form field "+name+" is given twice")
			return
		}
		fields[name] = string(v)
		order = append(order, name)
	}
	if part == nil {
		writeXMLError(w, http.StatusBadRequest, "InvalidArgument", "The form has no file field.")
		return
	}
	key, ok := fields["key"]
	if !ok || key == "" {
		writeXMLError(w, http.StatusBadRequest, "InvalidArgument", "The form has no key field.")
		return
	}
	for _, name := range order {
		if !formKnown(name) {
			writeXMLError(w, http.StatusBadRequest, "InvalidArgument",
				"The form field "+name+" is not supported by CloudBurrow; it is refused rather than ignored")
			return
		}
	}
	if err := s.bucketExists(bucket); err != nil {
		writeXMLFromError(w, r, err, bucket)
		return
	}
	var pol *postPolicy
	if b64 := fields["policy"]; b64 != "" {
		if pol, err = parsePostPolicy(b64); err != nil {
			// UNVERIFIED: the code Google gives a malformed policy.
			// Docs re-read 2026-10-17, still unanswered: https://docs.cloud.google.com/storage/docs/xml-api/reference-status
			writeXMLError(w, http.StatusBadRequest, "InvalidPolicyDocument", "Invalid Policy: "+err.Error())
			return
		}
		if err := s.verifyPolicySignature(fields); err != nil {
			var se *signatureError
			if errors.As(err, &se) {
				writeXMLError(w, se.status, se.code, se.msg)
			} else {
				writeXMLError(w, http.StatusForbidden, "AccessDenied", err.Error())
			}
			return
		}
		if !s.now().Before(pol.expires) {
			policyFail(w, "Policy expired.")
			return
		}
		for _, c := range pol.conditions {
			op, name, want := c[0].(string), c[1].(string), c[2].(string)
			got := fields[name]
			if name == "bucket" {
				got = bucket
			}
			if op == "eq" && got != want || op == "starts-with" && !strings.HasPrefix(got, want) {
				policyFail(w, "Policy Condition failed: [%q, \"$%s\", %q]", op, name, want)
				return
			}
		}
		for _, name := range order {
			if !formExempt(name) && !pol.named[name] {
				policyFail(w, "Extra input fields: %s", name)
				return
			}
		}
	} else if fields["x-goog-signature"] != "" || fields["signature"] != "" {
		writeXMLError(w, http.StatusBadRequest, "InvalidArgument", "A signature needs the policy it signs.")
		return
	}
	key = filenameVar.ReplaceAllLiteralString(key, filename)
	meta := uploadMeta{Name: key, ContentType: fields["content-type"], ContentEncoding: fields["content-encoding"],
		ContentDisposition: fields["content-disposition"], CacheControl: fields["cache-control"]}
	for _, name := range order {
		if m, ok := strings.CutPrefix(name, "x-goog-meta-"); ok {
			if meta.Metadata == nil {
				meta.Metadata = map[string]string{}
			}
			meta.Metadata[m] = fields[name]
		}
	}
	if a := fields["acl"]; a != "" {
		p, ok := xmlCannedObjectACL[a]
		if !ok {
			writeXMLError(w, http.StatusBadRequest, "InvalidArgument", "Invalid canned ACL acl: "+a)
			return
		}
		meta.PredefinedACL = p
	}
	if err := validObjectName(key); err != nil {
		writeXMLFromError(w, r, err, bucket)
		return
	}
	body := part
	if pol != nil && pol.hasRange {
		body = io.LimitReader(part, pol.maxLen+1)
	}
	blob, err := s.blobs.Write(body)
	if err != nil {
		writeXMLFromError(w, r, err, bucket)
		return
	}
	if pol != nil && pol.hasRange {
		// UNVERIFIED: the codes follow the XML API's interoperable ones.
		// Docs re-read 2026-10-17, still unanswered: https://docs.cloud.google.com/storage/docs/xml-api/reference-status
		if blob.Size > pol.maxLen {
			writeXMLError(w, http.StatusBadRequest, "EntityTooLarge", fmt.Sprintf("Your proposed upload exceeds the maximum allowed size of %d bytes.", pol.maxLen))
			return
		}
		if blob.Size < pol.minLen {
			writeXMLError(w, http.StatusBadRequest, "EntityTooSmall", fmt.Sprintf("Your proposed upload is smaller than the minimum allowed size of %d bytes.", pol.minLen))
			return
		}
	}
	o, err := s.finalizeObject(bucket, meta, blob, objectPreconditions{}, nil, nil)
	if err != nil {
		writeXMLFromError(w, r, err, bucket)
		return
	}
	xmlObjectHeaders(w, o)
	etag := `"` + hexMD5(o) + `"`
	if redirect := fields["success_action_redirect"]; redirect != "" {
		if u, err := url.Parse(redirect); err == nil && u.IsAbs() {
			q := u.Query()
			q.Set("bucket", bucket)
			q.Set("key", key)
			q.Set("etag", etag)
			u.RawQuery = q.Encode()
			w.Header().Set("Location", u.String())
			w.WriteHeader(http.StatusSeeOther)
			return
		}
	}
	switch fields["success_action_status"] {
	case "200":
		w.WriteHeader(http.StatusOK)
	case "201":
		// UNVERIFIED: the PostResponse body follows the interoperable one.
		// Docs re-read 2026-10-17, still unanswered: https://docs.cloud.google.com/storage/docs/xml-api/post-object-forms
		type postResponse struct {
			XMLName  xml.Name `xml:"PostResponse"`
			Location string   `xml:"Location"`
			Bucket   string   `xml:"Bucket"`
			Key      string   `xml:"Key"`
			ETag     string   `xml:"ETag"`
		}
		loc := baseURL(r) + "/" + escape(bucket) + "/" + escapePath(key)
		w.Header().Set("Location", loc)
		w.Header().Set("Content-Type", "application/xml; charset=UTF-8")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(xml.Header))
		_ = xml.NewEncoder(w).Encode(postResponse{Location: loc, Bucket: bucket, Key: key, ETag: etag})
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}
