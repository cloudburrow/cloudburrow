package storage

// Customer-managed encryption keys (#1938), as
// https://docs.cloud.google.com/storage/docs/encryption/customer-managed-keys
// and .../using-customer-managed-keys describe them:
//
//   - an object is written with a Cloud KMS key named by kmsKeyName on
//     objects.insert (simple, multipart and resumable uploads),
//     destinationKmsKeyName on objects.rewrite and objects.copy, kmsKeyName
//     on objects.compose, x-goog-encryption-kms-key-name on an XML API PUT,
//     or else by its bucket's encryption.defaultKmsKeyName; a request's key
//     wins over the bucket's default, and a customer-supplied key wins over
//     both;
//   - the object reports the key version it was encrypted with as
//     kmsKeyName (.../cryptoKeys/KEY/cryptoKeyVersions/N); a new write uses
//     the key's primary version, and a rotated key's older versions keep
//     opening the objects they encrypted;
//   - while the version an object was written with is disabled or
//     destroyed, its bytes cannot be read, copied or rewritten, and with a
//     primary version that is not enabled the key cannot encrypt; metadata,
//     listing and deletion still work;
//   - the project's Cloud Storage service agent,
//     service-PROJECT_NUMBER@gs-project-accounts.iam.gserviceaccount.com,
//     encrypts and decrypts, and needs
//     roles/cloudkms.cryptoKeyEncrypterDecrypter on the key: with
//     `up --iam-enforcement=on` the host's evaluator checks it holds
//     cloudkms.cryptoKeyVersions.useToEncrypt or useToDecrypt there, with
//     the key's and its key ring's policies;
//   - the key ring is in the bucket's location (us for US, the region for a
//     region, the dual-region's code for one);
//   - JSON API listings leave out a CMEK object's crc32c and md5Hash.
//
// The bytes are really encrypted at rest: every object written with a key
// gets a fresh 256-bit data key, the bytes are sealed with it in 64 KiB
// AES-256-GCM segments (sealedblob.go), and the data key is kept only as
// the instance's Cloud KMS Encrypt returned it. A read asks Cloud KMS to
// Decrypt it, so a disabled or destroyed version really refuses the read.
//
// Choices Google does not document, pinned by unit tests (UNVERIFIED): the
// error codes and messages. A key that cannot be used is 400 (the key's
// state, a missing key), a service agent without the role 403, Cloud KMS
// not answering 503. Docs re-read 2026-11-19, still unanswered:
// https://docs.cloud.google.com/storage/docs/encryption/customer-managed-keys

import (
	"context"
	"crypto/rand"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"cloud.google.com/go/iam/apiv1/iampb"
	"cloud.google.com/go/kms/apiv1/kmspb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/cloudburrow/cloudburrow/internal/iamcheck"
	"github.com/cloudburrow/cloudburrow/internal/iampolicy"
	"github.com/cloudburrow/cloudburrow/internal/resource"
)

// KeyService is the part of Cloud KMS customer-managed keys use, as the
// instance's Cloud KMS serves it over gRPC.
type KeyService interface {
	Encrypt(ctx context.Context, in *kmspb.EncryptRequest, opts ...grpc.CallOption) (*kmspb.EncryptResponse, error)
	Decrypt(ctx context.Context, in *kmspb.DecryptRequest, opts ...grpc.CallOption) (*kmspb.DecryptResponse, error)
	GetIamPolicy(ctx context.Context, in *iampb.GetIamPolicyRequest, opts ...grpc.CallOption) (*iampb.Policy, error)
}

// kmsClient is KeyService over one connection.
type kmsClient struct {
	kmspb.KeyManagementServiceClient
	iam iampb.IAMPolicyClient
}

func (c kmsClient) GetIamPolicy(ctx context.Context, in *iampb.GetIamPolicyRequest, opts ...grpc.CallOption) (*iampb.Policy, error) {
	return c.iam.GetIamPolicy(ctx, in, opts...)
}

// DialKMS is the KeyService of the Cloud KMS at addr (host:port, plaintext
// gRPC, as the instance serves it to the cluster through cloudburrow-host).
func DialKMS(addr string) (KeyService, io.Closer, error) {
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, nil, err
	}
	return kmsClient{kmspb.NewKeyManagementServiceClient(conn), iampb.NewIAMPolicyClient(conn)}, conn, nil
}

var (
	cryptoKeyRE        = regexp.MustCompile(`^projects/([^/]+)/locations/([^/]+)/keyRings/[^/]+/cryptoKeys/[^/]+$`)
	cryptoKeyVersionRE = regexp.MustCompile(`^(projects/[^/]+/locations/[^/]+/keyRings/[^/]+/cryptoKeys/[^/]+)/cryptoKeyVersions/[0-9]+$`)
)

// sealedKey is how an object's bytes are encrypted: the key version and
// the data key Cloud KMS wrapped. The zero value is Google-managed
// encryption.
type sealedKey struct {
	Version string
	Wrapped []byte
}

// validKMSKeyName checks a kmsKeyName names a CryptoKey.
func validKMSKeyName(field, name string) error {
	if !cryptoKeyRE.MatchString(name) {
		return badRequest("Invalid argument: %s %q must be projects/PROJECT/locations/LOCATION/keyRings/KEY_RING/cryptoKeys/KEY", field, name)
	}
	return nil
}

// kmsLocationFor is the Cloud KMS location a bucket's keys are in.
func kmsLocationFor(bucketLocation string) string { return strings.ToLower(bucketLocation) }

// checkKeyLocation refuses a key outside the bucket's location.
func checkKeyLocation(key, bucketLocation string) error {
	m := cryptoKeyRE.FindStringSubmatch(key)
	if m == nil || bucketLocation == "" {
		return nil
	}
	if want := kmsLocationFor(bucketLocation); m[2] != want {
		return badRequest("The Cloud KMS key %s is in location %s, but the bucket is in %s: the key ring must be in the same location as the data it encrypts (%s).",
			key, m[2], strings.ToUpper(bucketLocation), want)
	}
	return nil
}

// serviceAgent is the Cloud Storage service agent of project.
func serviceAgent(project string) string {
	return "service-" + strconv.FormatInt(resource.ProjectNumber(project), 10) + "@gs-project-accounts.iam.gserviceaccount.com"
}

// kmsError is Cloud Storage's answer for a Cloud KMS refusal.
func kmsError(key string, err error) error {
	st, _ := grpcstatus.FromError(err)
	switch st.Code() {
	case codes.NotFound:
		return badRequest("The Cloud KMS key %s does not exist.", key)
	case codes.FailedPrecondition:
		return badRequest("The Cloud KMS key %s cannot be used: %s", key, st.Message())
	case codes.PermissionDenied:
		return errKMSPermission(key)
	case codes.InvalidArgument:
		return badRequest("The Cloud KMS key %s cannot be used: %s", key, st.Message())
	case codes.Unavailable, codes.DeadlineExceeded, codes.Canceled, codes.Unknown:
		return errorf(http.StatusServiceUnavailable, "backendError", "Cloud KMS did not answer for %s: %s", key, st.Message())
	}
	return errorf(http.StatusInternalServerError, "backendError", "Cloud KMS refused %s: %s", key, st.Message())
}

func errKMSPermission(key string) error {
	return errorf(http.StatusForbidden, "forbidden",
		"Permission denied on Cloud KMS key %s. Please ensure that your Cloud Storage service account has been authorized to use this key.", key)
}

// keyService is the configured Cloud KMS, or a refusal.
func (s *Server) keyService(key string) (KeyService, error) {
	if s.kms == nil {
		return nil, badRequest("Cloud KMS is not served by this CloudBurrow instance, so the key %s cannot be used: enable the kms service.", key)
	}
	return s.kms, nil
}

// checkAgent checks, with IAM enforcement on, that project's service
// agent holds permission on key, with the key's and its key ring's own
// policies.
func (s *Server) checkAgent(ctx context.Context, k KeyService, project, key, permission string) error {
	if !s.enforcing() {
		return nil
	}
	ring := key[:strings.Index(key, "/cryptoKeys/")]
	var own []iamcheck.Policy
	for _, res := range []string{key, ring} {
		p, err := k.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: res, Options: &iampb.GetPolicyOptions{RequestedPolicyVersion: 3}})
		if err != nil {
			return kmsError(key, err)
		}
		stored := kmsPolicy(p)
		own = append(own, iamcheck.Policy{Resource: "//cloudkms.googleapis.com/" + res, Policy: &stored})
	}
	m := cryptoKeyRE.FindStringSubmatch(key)
	d, err := s.iamCheck.Check(ctx, iamcheck.Request{Signer: serviceAgent(project), Service: "cloudkms.googleapis.com",
		Permissions: []string{permission}, Resource: key, Type: "cloudkms.googleapis.com/CryptoKey", Project: m[1], Own: own})
	if err != nil {
		return errorf(http.StatusServiceUnavailable, "backendError", "IAM check unavailable: %v", err)
	}
	if !d.Holds(permission) {
		return errKMSPermission(key)
	}
	return nil
}

// sealWrite stores r encrypted under key (a CryptoKey's name) for an
// object of project's bucket: the blob holds only ciphertext, and the
// returned Blob's size and checksums are the plaintext's.
func (s *Server) sealWrite(ctx context.Context, project, key string, r io.Reader) (Blob, sealedKey, error) {
	k, err := s.keyService(key)
	if err != nil {
		return Blob{}, sealedKey{}, err
	}
	if err := s.checkAgent(ctx, k, project, key, "cloudkms.cryptoKeyVersions.useToEncrypt"); err != nil {
		return Blob{}, sealedKey{}, err
	}
	dek := make([]byte, 32)
	if _, err := rand.Read(dek); err != nil {
		return Blob{}, sealedKey{}, err
	}
	resp, err := k.Encrypt(ctx, &kmspb.EncryptRequest{Name: key, Plaintext: dek})
	if err != nil {
		return Blob{}, sealedKey{}, kmsError(key, err)
	}
	plain := newHashes()
	sr, err := newSealReader(io.TeeReader(r, plain), dek)
	if err != nil {
		return Blob{}, sealedKey{}, err
	}
	blob, err := s.blobs.Write(sr)
	if err != nil {
		return Blob{}, sealedKey{}, err
	}
	p := plain.blob()
	blob.Size, blob.MD5, blob.CRC32C = p.Size, p.MD5, p.CRC32C
	return blob, sealedKey{Version: resp.GetName(), Wrapped: resp.GetCiphertext()}, nil
}

// writeBytes stores r as an object's bytes, sealed when key is set.
func (s *Server) writeBytes(ctx context.Context, project, key string, r io.Reader) (Blob, sealedKey, error) {
	if key == "" {
		b, err := s.blobs.Write(r)
		return b, sealedKey{}, err
	}
	return s.sealWrite(ctx, project, key, r)
}

// openObject opens o's bytes as plaintext: an object encrypted with a
// customer-managed key needs Cloud KMS to unwrap its data key.
func (s *Server) openObject(ctx context.Context, o objectRecord) (io.ReadSeekCloser, error) {
	if o.KMSKeyVersion == "" {
		return s.blobs.Open(o.Blob)
	}
	m := cryptoKeyVersionRE.FindStringSubmatch(o.KMSKeyVersion)
	if m == nil {
		return nil, errorf(http.StatusInternalServerError, "backendError", "the object's key version %q is not a CryptoKeyVersion", o.KMSKeyVersion)
	}
	key := m[1]
	k, err := s.keyService(key)
	if err != nil {
		return nil, err
	}
	project := ""
	_ = s.meta.View(func(tx Tx) error {
		if b, ok, _ := s.getBucket(tx, o.Bucket); ok {
			project = b.Project
		}
		return nil
	})
	if err := s.checkAgent(ctx, k, project, key, "cloudkms.cryptoKeyVersions.useToDecrypt"); err != nil {
		return nil, err
	}
	resp, err := k.Decrypt(ctx, &kmspb.DecryptRequest{Name: key, Ciphertext: o.WrappedKey})
	if err != nil {
		if grpcstatus.Code(err) == codes.FailedPrecondition {
			return nil, badRequest("The Cloud KMS key version %s that encrypts this object cannot be used: %s", o.KMSKeyVersion, grpcstatus.Convert(err).Message())
		}
		return nil, kmsError(key, err)
	}
	f, err := s.blobs.Open(o.Blob)
	if err != nil {
		return nil, err
	}
	rs, err := newSealedReader(f, resp.GetPlaintext(), o.Size)
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return rs, nil
}

// reseal is the bytes and key of o copied to an object encrypted with key
// ("" for Google-managed) in project's bucket: the same blob when neither
// is customer-managed, else the plaintext sealed anew.
func (s *Server) reseal(ctx context.Context, o objectRecord, project, key string) (Blob, sealedKey, error) {
	if o.KMSKeyVersion == "" && key == "" {
		return Blob{ID: o.Blob, Size: o.Size, MD5: o.MD5, CRC32C: o.CRC32C}, sealedKey{}, nil
	}
	f, err := s.openObject(ctx, o)
	if err != nil {
		return Blob{}, sealedKey{}, err
	}
	defer f.Close()
	b, sk, err := s.writeBytes(ctx, project, key, f)
	if err != nil {
		return Blob{}, sealedKey{}, err
	}
	b.MD5 = o.MD5
	return b, sk, nil
}

// encryptionTypes are the bucket's enforcement configs, by Object
// encryption type, and the JSON field each is configured by.
var encryptionTypes = []struct{ kind, field, xml string }{
	{"googleManaged", "googleManagedEncryptionEnforcementConfig", "GoogleManagedEncryptionEnforcement"},
	{"customerManaged", "customerManagedEncryptionEnforcementConfig", "CustomerManagedEncryptionEnforcement"},
	{"customerSupplied", "customerSuppliedEncryptionEnforcementConfig", "CustomerSuppliedEncryptionEnforcement"},
}

// bucketEncryption is a bucket's encryption object, or nil.
func bucketEncryption(b bucketRecord) map[string]any {
	m, _ := b.Fields["encryption"].(map[string]any)
	return m
}

// defaultKMSKey is the bucket's encryption.defaultKmsKeyName.
func defaultKMSKey(b bucketRecord) string {
	s, _ := bucketEncryption(b)["defaultKmsKeyName"].(string)
	return s
}

// restricted reports whether new objects of kind may not be created in b.
func restricted(b bucketRecord, kind string) bool {
	for _, t := range encryptionTypes {
		if t.kind == kind {
			c, _ := bucketEncryption(b)[t.field].(map[string]any)
			mode, _ := c["restrictionMode"].(string)
			return mode == "FullyRestricted"
		}
	}
	return false
}

// encryptionKind is the encryption type of an object written with key and
// csek.
func encryptionKind(key string, csek []byte) string {
	switch {
	case csek != nil:
		return "customerSupplied"
	case key != "":
		return "customerManaged"
	}
	return "googleManaged"
}

var kindNames = map[string]string{"googleManaged": "Google-managed encryption", "customerManaged": "customer-managed encryption keys",
	"customerSupplied": "customer-supplied encryption keys"}

// checkEnforced refuses a new object of b of kind its enforcement config
// restricts.
func checkEnforced(b bucketRecord, kind string) error {
	if restricted(b, kind) {
		return badRequest("Creating a new object in bucket %s with %s is not allowed: the bucket's encryption enforcement config restricts it (%sEncryptionEnforcementConfig.restrictionMode is FullyRestricted).",
			b.Name, kindNames[kind], kind)
	}
	return nil
}

// resolveKey is the key a new object of b is written with, given the
// request's kmsKeyName and customer-supplied key, after the bucket's
// enforcement config: the request's key, else the bucket's default unless
// the request supplies its own key.
func resolveKey(b bucketRecord, requested string, csek []byte) (string, error) {
	if requested != "" {
		if err := validKMSKeyName("kmsKeyName", requested); err != nil {
			return "", err
		}
		if csek != nil {
			return "", badRequest("A request cannot name both a Cloud KMS key (kmsKeyName) and a customer-supplied encryption key.")
		}
	}
	key := requested
	if key == "" && csek == nil {
		key = defaultKMSKey(b)
	}
	if err := checkEnforced(b, encryptionKind(key, csek)); err != nil {
		return "", err
	}
	if requested != "" {
		loc, _ := b.Fields["location"].(string)
		if err := checkKeyLocation(requested, loc); err != nil {
			return "", err
		}
	}
	return key, nil
}

// writeTarget is a new object's bucket project and key, resolved before its
// bytes are written: the bucket must exist.
type writeTarget struct {
	project, key string
}

func (s *Server) writeTarget(bucket, requested string, csek []byte) (writeTarget, error) {
	var t writeTarget
	err := s.meta.View(func(tx Tx) error {
		b, ok, err := s.getBucket(tx, bucket)
		if err != nil {
			return err
		}
		if !ok {
			return notFound("The specified bucket does not exist.")
		}
		t.project = b.Project
		t.key, err = resolveKey(b, requested, csek)
		return err
	})
	return t, err
}

// write stores r for the target.
func (t writeTarget) write(ctx context.Context, s *Server, r io.Reader) (Blob, sealedKey, error) {
	return s.writeBytes(ctx, t.project, t.key, r)
}

// checkBucketEncryption checks a bucket's encryption as it would be, next,
// against what it was, old (nil on create), and stamps each enforcement
// config's effectiveTime when its mode changes. An omitted config keeps
// its old value (enforce-encryption-types: "If you omit an encryption
// type, the existing configuration is retained").
func checkBucketEncryption(next, old map[string]any, now string) error {
	enc, _ := next["encryption"].(map[string]any)
	oldEnc, _ := old["encryption"].(map[string]any)
	if enc == nil && oldEnc == nil {
		return nil
	}
	if enc == nil {
		enc = map[string]any{}
	}
	for k := range enc {
		known := k == "defaultKmsKeyName"
		for _, t := range encryptionTypes {
			known = known || k == t.field
		}
		if !known {
			return badRequest("Invalid argument: encryption.%s is not a field of Bucket.Encryption", k)
		}
	}
	if v, ok := enc["defaultKmsKeyName"]; ok && v != nil {
		key, isStr := v.(string)
		if !isStr {
			return badRequest("Invalid argument: encryption.defaultKmsKeyName must be a string")
		}
		if key == "" {
			delete(enc, "defaultKmsKeyName")
		} else {
			if err := validKMSKeyName("encryption.defaultKmsKeyName", key); err != nil {
				return err
			}
			loc, _ := next["location"].(string)
			if err := checkKeyLocation(key, loc); err != nil {
				return err
			}
		}
	}
	allowed := 0
	for _, t := range encryptionTypes {
		v, set := enc[t.field]
		prev, _ := oldEnc[t.field].(map[string]any)
		if !set || v == nil {
			if prev != nil {
				enc[t.field] = prev
			}
			if prevMode, _ := prev["restrictionMode"].(string); prevMode != "FullyRestricted" {
				allowed++
			}
			continue
		}
		c, isMap := v.(map[string]any)
		if !isMap {
			return badRequest("Invalid argument: encryption.%s must be an object", t.field)
		}
		mode, _ := c["restrictionMode"].(string)
		switch mode {
		case "NotRestricted", "FullyRestricted":
		case "":
			mode = "NotRestricted"
		default:
			return badRequest("Invalid argument: encryption.%s.restrictionMode %v must be NotRestricted or FullyRestricted", t.field, c["restrictionMode"])
		}
		prevMode, _ := prev["restrictionMode"].(string)
		eff, _ := prev["effectiveTime"].(string)
		if prev == nil || prevMode != mode || eff == "" {
			eff = now
		}
		enc[t.field] = map[string]any{"restrictionMode": mode, "effectiveTime": eff}
		if mode != "FullyRestricted" {
			allowed++
		}
	}
	if allowed == 0 {
		return badRequest("Invalid argument: a bucket's encryption enforcement config must allow at least one encryption type.")
	}
	if key, _ := enc["defaultKmsKeyName"].(string); key != "" {
		b := bucketRecord{Fields: map[string]any{"encryption": enc}}
		if restricted(b, "customerManaged") && restricted(b, "customerSupplied") {
			return badRequest("Invalid argument: a bucket with a default Cloud KMS key must allow customer-managed or customer-supplied encryption keys.")
		}
	}
	if len(enc) == 0 {
		delete(next, "encryption")
		return nil
	}
	next["encryption"] = enc
	return nil
}

// sameKMSKey reports whether a patch or update body's kmsKeyName restates
// o's: its key version, or the key it is a version of.
func sameKMSKey(v any, o objectRecord) bool {
	s, ok := v.(string)
	if !ok || o.KMSKeyVersion == "" {
		return false
	}
	if s == o.KMSKeyVersion {
		return true
	}
	m := cryptoKeyVersionRE.FindStringSubmatch(o.KMSKeyVersion)
	return m != nil && m[1] == s
}

// resealInto gives o, a copy of src, the bytes and key the target needs.
func (s *Server) resealInto(r *http.Request, o *objectRecord, src objectRecord, t writeTarget) error {
	b, sk, err := s.reseal(r.Context(), src, t.project, t.key)
	if err != nil {
		return err
	}
	o.Blob, o.KMSKeyVersion, o.WrappedKey = b.ID, sk.Version, sk.Wrapped
	return nil
}

// keyOfVersion is the CryptoKey a version belongs to, "" for none.
func keyOfVersion(version string) string {
	if m := cryptoKeyVersionRE.FindStringSubmatch(version); m != nil {
		return m[1]
	}
	return ""
}

// deepCopy copies a JSON value, so a merge patch cannot change the
// original's nested objects.
func deepCopy(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = deepCopy(e)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = deepCopy(e)
		}
		return out
	}
	return v
}

// bodyKMSKey is a copy's, rewrite's or compose's destination key: the
// query parameter, else the destination body's kmsKeyName, which is taken
// out of the body.
func bodyKMSKey(param string, body map[string]any) string {
	v, _ := body["kmsKeyName"].(string)
	delete(body, "kmsKeyName")
	if param != "" {
		return param
	}
	return v
}

// kmsPolicy is a policy Cloud KMS returned, as the evaluator reads it.
func kmsPolicy(p *iampb.Policy) iampolicy.Stored {
	var out iampolicy.Stored
	for _, b := range p.GetBindings() {
		sb := iampolicy.Binding{Role: b.GetRole(), Members: b.GetMembers()}
		if c := b.GetCondition(); c != nil {
			sb.Condition = &iampolicy.Condition{Title: c.GetTitle(), Description: c.GetDescription(), Expression: c.GetExpression()}
		}
		out.Bindings = append(out.Bindings, sb)
	}
	return out
}
