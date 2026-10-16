package kms

import (
	"context"
	"crypto"
	"crypto/aes"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/subtle"
	"crypto/x509"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/kms/apiv1/kmspb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// Import jobs (#1212), as service.proto and resources.proto document them:
// an ImportJob holds an RSA wrapping key, and ImportCryptoKeyVersion unwraps
// key material sent under its public key into a new version.
//
//   - RSA_OAEP_{3072,4096}_{SHA1,SHA256}_AES_256 is CKM_RSA_AES_KEY_WRAP: an
//     ephemeral AES-256 key wrapped with RSA-OAEP (MGF1 with the same hash,
//     empty label), followed by the key wrapped with that AES key using
//     AES-KWP (RFC 5649).
//   - RSA_OAEP_{3072,4096}_SHA256 wraps the key directly with RSA-OAEP, so it
//     cannot carry an RSA key.
//   - The key is plain bytes for symmetric algorithms and PKCS #8 DER for
//     asymmetric ones, and must be what the algorithm names.
//
// A SOFTWARE job is generated at once, so it is ACTIVE when created, and it
// expires three days after creation (resources.proto ImportJob.expire_time).
// The HPKE methods are UNIMPLEMENTED: they need ML-KEM or X-Wing.
// The codes for refused imports are UNVERIFIED.

const (
	importJobPrefix = "kms/importjob/"
	importJobTTL    = 3 * 24 * time.Hour
)

type importJob struct {
	Name    string    `json:"name"`
	Method  string    `json:"method"`
	Created time.Time `json:"created"`
	// Private is the RSA wrapping key, PKCS #8 DER, stored unencrypted like
	// all key material here.
	Private []byte `json:"private"`
	// ProtectionLevel is empty for SOFTWARE; an HSM job is generated as a
	// SOFTWARE one is, with no attestation (protection.go, #1452).
	ProtectionLevel string `json:"protectionLevel,omitempty"`
}

type importSpec struct {
	bits int
	hash crypto.Hash
	aes  bool
}

var importMethods = map[kmspb.ImportJob_ImportMethod]importSpec{
	kmspb.ImportJob_RSA_OAEP_3072_SHA1_AES_256:   {3072, crypto.SHA1, true},
	kmspb.ImportJob_RSA_OAEP_4096_SHA1_AES_256:   {4096, crypto.SHA1, true},
	kmspb.ImportJob_RSA_OAEP_3072_SHA256_AES_256: {3072, crypto.SHA256, true},
	kmspb.ImportJob_RSA_OAEP_4096_SHA256_AES_256: {4096, crypto.SHA256, true},
	kmspb.ImportJob_RSA_OAEP_3072_SHA256:         {3072, crypto.SHA256, false},
	kmspb.ImportJob_RSA_OAEP_4096_SHA256:         {4096, crypto.SHA256, false},
}

func (j importJob) method() kmspb.ImportJob_ImportMethod {
	return kmspb.ImportJob_ImportMethod(kmspb.ImportJob_ImportMethod_value[j.Method])
}

func (j importJob) state(now time.Time) kmspb.ImportJob_ImportJobState {
	if !now.Before(j.Created.Add(importJobTTL)) {
		return kmspb.ImportJob_EXPIRED
	}
	return kmspb.ImportJob_ACTIVE
}

func (j importJob) key() (*rsa.PrivateKey, error) {
	k, err := x509.ParsePKCS8PrivateKey(j.Private)
	if err != nil {
		return nil, apierror.Wrap(apierror.Internal(err, "stored key of %s", j.Name))
	}
	r, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, apierror.Wrap(apierror.Internal(nil, "stored key of %s is not RSA", j.Name))
	}
	return r, nil
}

func (s *Server) toImportJob(j importJob) (*kmspb.ImportJob, error) {
	k, err := j.key()
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalPKIXPublicKey(&k.PublicKey)
	if err != nil {
		return nil, apierror.Wrap(apierror.Internal(err, "encode the public key of %s", j.Name))
	}
	now := s.now()
	out := &kmspb.ImportJob{
		Name:            j.Name,
		ImportMethod:    j.method(),
		ProtectionLevel: j.protectionLevel(),
		CreateTime:      timestamppb.New(j.Created),
		GenerateTime:    timestamppb.New(j.Created),
		ExpireTime:      timestamppb.New(j.Created.Add(importJobTTL)),
		State:           j.state(now),
		PublicKey:       &kmspb.ImportJob_WrappingPublicKey{Pem: string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))},
	}
	if out.State == kmspb.ImportJob_EXPIRED {
		out.ExpireEventTime = out.ExpireTime
	}
	return out, nil
}

func parseImportJob(field, name string) error {
	parts := strings.Split(name, "/")
	if len(parts) != 8 || parts[6] != "importJobs" {
		return badName(field, name, "ImportJob", "want projects/{project}/locations/{location}/keyRings/{key_ring}/importJobs/{import_job}")
	}
	if err := parseKeyRing(field, strings.Join(parts[:6], "/")); err != nil {
		return err
	}
	if !idRE.MatchString(parts[7]) {
		return badName(field, name, "ImportJob", "the import job ID must be 1-63 letters, digits, - or _")
	}
	return nil
}

func (s *Server) CreateImportJob(_ context.Context, req *kmspb.CreateImportJobRequest) (*kmspb.ImportJob, error) {
	if err := parseKeyRing("parent", req.GetParent()); err != nil {
		return nil, apierror.Wrap(err)
	}
	if !idRE.MatchString(req.GetImportJobId()) {
		return nil, apierror.Wrap(apierror.InvalidArgument("import_job_id %q must be 1-63 letters, digits, - or _", req.GetImportJobId()))
	}
	in := req.GetImportJob()
	m := in.GetImportMethod()
	if m == kmspb.ImportJob_IMPORT_METHOD_UNSPECIFIED {
		return nil, apierror.Wrap(apierror.InvalidArgument("import_job.import_method is required"))
	}
	spec, ok := importMethods[m]
	if !ok {
		return nil, apierror.Wrap(apierror.Unimplemented("import_job.import_method %s is not implemented", m))
	}
	if in.GetProtectionLevel() == kmspb.ProtectionLevel_PROTECTION_LEVEL_UNSPECIFIED {
		return nil, apierror.Wrap(apierror.InvalidArgument("import_job.protection_level is required"))
	}
	level, err := protectionLevelOf("import_job.protection_level", in.GetProtectionLevel())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	if in.GetCryptoKeyBackend() != "" {
		return nil, apierror.Wrap(apierror.Unimplemented("import_job.crypto_key_backend is not implemented"))
	}
	name := req.GetParent() + "/importJobs/" + req.GetImportJobId()
	priv, err := rsa.GenerateKey(rand.Reader, spec.bits)
	if err != nil {
		return nil, apierror.Wrap(apierror.Internal(err, "generate a wrapping key"))
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, apierror.Wrap(apierror.Internal(err, "encode a wrapping key"))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var ring keyRing
	if err := s.load(dbKey(ringPrefix, req.GetParent()), &ring, "KeyRing", req.GetParent()); err != nil {
		return nil, err
	}
	var existing importJob
	if found, err := s.get(dbKey(importJobPrefix, name), &existing); err != nil {
		return nil, apierror.Wrap(err)
	} else if found {
		return nil, apierror.Wrap(apierror.AlreadyExists("ImportJob %s already exists", name))
	}
	j := importJob{Name: name, Method: m.String(), Created: s.now().UTC(), Private: der, ProtectionLevel: storedLevel(level)}
	if err := s.put(dbKey(importJobPrefix, name), j); err != nil {
		return nil, apierror.Wrap(err)
	}
	return s.toImportJob(j)
}

func (s *Server) GetImportJob(_ context.Context, req *kmspb.GetImportJobRequest) (*kmspb.ImportJob, error) {
	if err := parseImportJob("name", req.GetName()); err != nil {
		return nil, apierror.Wrap(err)
	}
	var j importJob
	if err := s.load(dbKey(importJobPrefix, req.GetName()), &j, "ImportJob", req.GetName()); err != nil {
		return nil, err
	}
	return s.toImportJob(j)
}

var importJobFilterFields = fieldSupport{"state": {"=", "!="}}

func (s *Server) ListImportJobs(_ context.Context, req *kmspb.ListImportJobsRequest) (*kmspb.ListImportJobsResponse, error) {
	if err := parseKeyRing("parent", req.GetParent()); err != nil {
		return nil, apierror.Wrap(err)
	}
	filter, err := parseFilter(req.GetFilter(), importJobFilterFields)
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	desc, err := parseOrderBy(req.GetOrderBy())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	var ring keyRing
	if err := s.load(dbKey(ringPrefix, req.GetParent()), &ring, "KeyRing", req.GetParent()); err != nil {
		return nil, err
	}
	names, err := s.list(importJobPrefix, req.GetParent())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	jobs := map[string]importJob{}
	var matched []string
	now := s.now()
	for _, n := range names {
		var j importJob
		found, err := s.get(dbKey(importJobPrefix, n), &j)
		if err != nil {
			return nil, apierror.Wrap(err)
		}
		if !found {
			continue
		}
		if filter == nil || filter.match(func(field string) (string, bool) { return j.state(now).String(), field == "state" }) {
			jobs[n] = j
			matched = append(matched, n)
		}
	}
	page, next, err := orderedPage("kms-importjobs:"+req.GetParent()+"|filter="+req.GetFilter(), matched, byName, desc, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, apierror.Wrap(apierror.InvalidArgument("%v", err))
	}
	resp := &kmspb.ListImportJobsResponse{NextPageToken: next, TotalSize: int32(len(matched))}
	for _, n := range page {
		pj, err := s.toImportJob(jobs[n])
		if err != nil {
			return nil, err
		}
		resp.ImportJobs = append(resp.ImportJobs, pj)
	}
	return resp, nil
}

var errBadWrappedKey = apierror.InvalidArgument("wrapped_key could not be unwrapped with the import job's key, or is not in the format its import_method requires")

// unwrap opens wrapped under job j.
func unwrap(j importJob, wrapped []byte) ([]byte, error) {
	priv, err := j.key()
	if err != nil {
		return nil, err
	}
	spec := importMethods[j.method()]
	n := priv.Size()
	if !spec.aes {
		pt, err := rsa.DecryptOAEP(spec.hash.New(), nil, priv, wrapped, nil)
		if err != nil {
			return nil, apierror.Wrap(errBadWrappedKey)
		}
		return pt, nil
	}
	if len(wrapped) <= n {
		return nil, apierror.Wrap(errBadWrappedKey)
	}
	kek, err := rsa.DecryptOAEP(spec.hash.New(), nil, priv, wrapped[:n], nil)
	if err != nil || len(kek) != 32 {
		return nil, apierror.Wrap(errBadWrappedKey)
	}
	pt, err := unwrapKWP(kek, wrapped[n:])
	if err != nil {
		return nil, apierror.Wrap(errBadWrappedKey)
	}
	return pt, nil
}

// unwrapKWP is AES Key Wrap with Padding, RFC 5649 section 4.2.
func unwrapKWP(kek, c []byte) ([]byte, error) {
	block, err := aes.NewCipher(kek)
	if err != nil {
		return nil, err
	}
	if len(c) < 16 || len(c)%8 != 0 {
		return nil, errors.New("bad length")
	}
	n := len(c)/8 - 1
	var a [8]byte
	r := make([]byte, n*8)
	if n == 1 {
		var b [16]byte
		block.Decrypt(b[:], c)
		copy(a[:], b[:8])
		copy(r, b[8:])
	} else {
		copy(a[:], c[:8])
		copy(r, c[8:])
		var b [16]byte
		for j := 5; j >= 0; j-- {
			for i := n; i >= 1; i-- {
				t := uint64(n*j + i)
				binary.BigEndian.PutUint64(b[:8], binary.BigEndian.Uint64(a[:])^t)
				copy(b[8:], r[(i-1)*8:i*8])
				block.Decrypt(b[:], b[:])
				copy(a[:], b[:8])
				copy(r[(i-1)*8:i*8], b[8:])
			}
		}
	}
	if subtle.ConstantTimeCompare(a[:4], []byte{0xA6, 0x59, 0x59, 0xA6}) != 1 {
		return nil, errors.New("bad integrity value")
	}
	mli := int(binary.BigEndian.Uint32(a[4:]))
	if mli > n*8 || mli <= (n-1)*8 {
		return nil, errors.New("bad length indicator")
	}
	for _, p := range r[mli:] {
		if p != 0 {
			return nil, errors.New("bad padding")
		}
	}
	return r[:mli], nil
}

// importedMaterial checks that key is what algorithm a names: bytes of the
// right length for AES and HMAC, PKCS #8 DER of the right type and size for
// the asymmetric ones. Stored material is re-encoded PKCS #8.
func importedMaterial(a algo, key []byte) ([]byte, error) {
	spec := algorithms[a]
	bad := func(why string) error {
		return apierror.Wrap(apierror.InvalidArgument("the imported key is not a valid %s key: %s", a, why))
	}
	if spec.keyLen > 0 {
		if len(key) != spec.keyLen {
			return nil, bad("it must be " + strconv.Itoa(spec.keyLen) + " bytes")
		}
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(key)
	if err != nil {
		return nil, bad("it must be PKCS #8 DER")
	}
	switch k := parsed.(type) {
	case *rsa.PrivateKey:
		if spec.rsaBits == 0 || k.N.BitLen() != spec.rsaBits {
			return nil, bad("wrong key type or size")
		}
	case *ecdsa.PrivateKey:
		if spec.curve == nil || k.Curve != spec.curve {
			return nil, bad("wrong key type or curve")
		}
	case ed25519.PrivateKey:
		if !spec.ed25519 {
			return nil, bad("wrong key type")
		}
	default:
		return nil, bad("unsupported key type")
	}
	return key, nil
}

// ImportCryptoKeyVersion imports wrapped key material into a new version of
// a key, unwrapping it with the named ACTIVE import job. The version is
// ENABLED at once (on Google it passes through PENDING_IMPORT), records its
// import job and import time, and is not made primary. Re-importing into an
// existing version (crypto_key_version) is UNIMPLEMENTED.
func (s *Server) ImportCryptoKeyVersion(_ context.Context, req *kmspb.ImportCryptoKeyVersionRequest) (*kmspb.CryptoKeyVersion, error) {
	if err := parseCryptoKey("parent", req.GetParent()); err != nil {
		return nil, apierror.Wrap(err)
	}
	if err := parseImportJob("import_job", req.GetImportJob()); err != nil {
		return nil, apierror.Wrap(err)
	}
	if req.GetCryptoKeyVersion() != "" {
		return nil, apierror.Wrap(apierror.Unimplemented("crypto_key_version is not implemented: re-import into an existing version is not supported"))
	}
	wrapped := req.GetWrappedKey()
	if len(wrapped) == 0 {
		wrapped = req.GetRsaAesWrappedKey()
	}
	if len(wrapped) == 0 {
		return nil, apierror.Wrap(apierror.InvalidArgument("wrapped_key is required"))
	}
	a := req.GetAlgorithm()
	if a == kmspb.CryptoKeyVersion_CRYPTO_KEY_VERSION_ALGORITHM_UNSPECIFIED {
		return nil, apierror.Wrap(apierror.InvalidArgument("algorithm is required"))
	}
	ap, ok := algorithmPurpose(a)
	if !ok {
		return nil, apierror.Wrap(apierror.Unimplemented("algorithm %s is not implemented", a))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var k cryptoKey
	if err := s.load(dbKey(keyPrefix, req.GetParent()), &k, "CryptoKey", req.GetParent()); err != nil {
		return nil, err
	}
	// The algorithm "does not need to match the version_template", but it
	// must suit the key's purpose (service.proto).
	if ap != k.purpose() {
		return nil, apierror.Wrap(apierror.InvalidArgument("algorithm %s is not valid for CryptoKey %s, whose purpose is %s", a, k.Name, k.purpose()))
	}
	var j importJob
	if err := s.load(dbKey(importJobPrefix, req.GetImportJob()), &j, "ImportJob", req.GetImportJob()); err != nil {
		return nil, err
	}
	// The job's protection level "must match the protection_level of the
	// version_template on the CryptoKey you attempt to import into"
	// (resources.proto ImportJob); the code is UNVERIFIED.
	if j.protectionLevel() != k.protectionLevel() {
		return nil, apierror.Wrap(apierror.InvalidArgument("ImportJob %s is %s, but CryptoKey %s is %s: the protection levels must match", j.Name, j.protectionLevel(), k.Name, k.protectionLevel()))
	}
	now := s.now().UTC()
	if st := j.state(now); st != kmspb.ImportJob_ACTIVE {
		return nil, apierror.Wrap(apierror.FailedPrecondition("ImportJob %s is %s: only an ACTIVE import job can be used", j.Name, st))
	}
	raw, err := unwrap(j, wrapped)
	if err != nil {
		return nil, err
	}
	mat, err := importedMaterial(a, raw)
	if err != nil {
		return nil, err
	}
	n := k.Next
	v := keyVersion{Name: versionName(k.Name, n), Created: now, Material: mat, Algorithm: storedAlgorithm(a),
		ImportJob: j.Name, Imported: now, ProtectionLevel: k.ProtectionLevel}
	if err := s.put(dbKey(versionPrefix, v.Name), v); err != nil {
		return nil, apierror.Wrap(err)
	}
	k.Next = n + 1
	if err := s.put(dbKey(keyPrefix, k.Name), k); err != nil {
		return nil, apierror.Wrap(err)
	}
	return s.toVersion(v), nil
}

// errTrustedKeyWrapping is why the trusted-key methods are UNIMPLEMENTED:
// service.proto ties them to keys with trusted_wrapping_enabled, which is
// supported only at protection level HSM_SINGLE_TENANT, wrapped with an
// AES_WRAPPING key (AES_256_KWP). Neither that protection level nor that
// purpose exists locally, so no version could satisfy their preconditions.
const errTrustedKeyWrapping = "%s is not implemented: trusted key wrapping needs HSM_SINGLE_TENANT protection and an AES_WRAPPING key, which CloudBurrow does not provide"

func (s *Server) ExportTrustedKeyWrappedCryptoKeyVersion(context.Context, *kmspb.ExportTrustedKeyWrappedCryptoKeyVersionRequest) (*kmspb.ExportTrustedKeyWrappedCryptoKeyVersionResponse, error) {
	return nil, apierror.Wrap(apierror.Unimplemented(errTrustedKeyWrapping, "ExportTrustedKeyWrappedCryptoKeyVersion"))
}

func (s *Server) ImportTrustedKeyWrappedCryptoKeyVersion(context.Context, *kmspb.ImportTrustedKeyWrappedCryptoKeyVersionRequest) (*kmspb.CryptoKeyVersion, error) {
	return nil, apierror.Wrap(apierror.Unimplemented(errTrustedKeyWrapping, "ImportTrustedKeyWrappedCryptoKeyVersion"))
}

// WrapForImport wraps key under an import job's wrapping public key (its
// PEM) as method names, as Google's import sample and gcloud's
// --target-key-file do on the client: for the *_AES_256 methods
// (CKM_RSA_AES_KEY_WRAP) an ephemeral AES-256 key wrapped with RSA-OAEP,
// followed by key wrapped with it using AES-KWP (RFC 5649); for the others,
// key wrapped with RSA-OAEP alone. OAEP uses MGF1 with the method's hash and
// an empty label. The console's Import version uses it for plain key
// material (#1219); nothing server-side does.
func WrapForImport(publicPEM string, method kmspb.ImportJob_ImportMethod, key []byte) ([]byte, error) {
	spec, ok := importMethods[method]
	if !ok {
		return nil, errors.New("import method " + method.String() + " is not implemented")
	}
	b, _ := pem.Decode([]byte(publicPEM))
	if b == nil {
		return nil, errors.New("the import job's public key is not PEM")
	}
	parsed, err := x509.ParsePKIXPublicKey(b.Bytes)
	if err != nil {
		return nil, err
	}
	pub, ok := parsed.(*rsa.PublicKey)
	if !ok {
		return nil, errors.New("the import job's public key is not RSA")
	}
	if !spec.aes {
		return rsa.EncryptOAEP(spec.hash.New(), rand.Reader, pub, key, nil)
	}
	kek := make([]byte, 32)
	if _, err := rand.Read(kek); err != nil {
		return nil, err
	}
	wrappedKEK, err := rsa.EncryptOAEP(spec.hash.New(), rand.Reader, pub, kek, nil)
	if err != nil {
		return nil, err
	}
	wrapped, err := wrapKWP(kek, key)
	if err != nil {
		return nil, err
	}
	return append(wrappedKEK, wrapped...), nil
}

// wrapKWP is AES Key Wrap with Padding, RFC 5649 section 4.1.
func wrapKWP(kek, p []byte) ([]byte, error) {
	if len(p) == 0 || uint64(len(p)) > 1<<32-1 {
		return nil, errors.New("the key to wrap must be 1 to 2^32-1 bytes")
	}
	block, err := aes.NewCipher(kek)
	if err != nil {
		return nil, err
	}
	var a [8]byte
	copy(a[:4], []byte{0xA6, 0x59, 0x59, 0xA6})
	binary.BigEndian.PutUint32(a[4:], uint32(len(p)))
	n := (len(p) + 7) / 8
	r := make([]byte, n*8)
	copy(r, p)
	if n == 1 {
		out := make([]byte, 16)
		copy(out, a[:])
		copy(out[8:], r)
		block.Encrypt(out, out)
		return out, nil
	}
	var b [16]byte
	for j := 0; j <= 5; j++ {
		for i := 1; i <= n; i++ {
			copy(b[:8], a[:])
			copy(b[8:], r[(i-1)*8:i*8])
			block.Encrypt(b[:], b[:])
			binary.BigEndian.PutUint64(a[:], binary.BigEndian.Uint64(b[:8])^uint64(n*j+i))
			copy(r[(i-1)*8:i*8], b[8:])
		}
	}
	return append(a[:], r...), nil
}
