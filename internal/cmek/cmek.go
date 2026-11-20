// Package cmek is the customer-managed encryption key helper the services
// beyond Cloud Storage share (#1950: Cloud Logging buckets, Spanner
// databases and backups, Document AI processors, Speech-to-Text's config
// and its resources), as Cloud Storage (#1938, internal/service/storage's
// cmek.go) and Cloud SQL (#1538) do it:
//
//   - a key is a CryptoKey of the instance's own Cloud KMS, in the
//     location the service requires, and must be usable: it exists, its
//     purpose is ENCRYPT_DECRYPT and its primary version is ENABLED;
//   - the service's agent (service-PROJECT_NUMBER@gcp-sa-SERVICE.iam...)
//     encrypts and decrypts, and needs
//     roles/cloudkms.cryptoKeyEncrypterDecrypter on the key: with IAM
//     enforcement on (a Checker), the host's evaluator checks it holds
//     cloudkms.cryptoKeyVersions.useToEncrypt or useToDecrypt there, with
//     the key's and its key ring's policies;
//   - what the service stores is really encrypted: Seal gives each value a
//     fresh 256-bit data key, seals the bytes with it (AES-256-GCM), and
//     keeps the data key only as Cloud KMS's Encrypt returned it, with the
//     key version that wrapped it; Open asks Cloud KMS to Decrypt it, so a
//     disabled or destroyed version really refuses the read.
//
// Each service words its errors as Google documents them; an *Error says
// which case it is.
package cmek

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"

	"cloud.google.com/go/iam/apiv1/iampb"
	"cloud.google.com/go/kms/apiv1/kmspb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/cloudburrow/cloudburrow/internal/iamcheck"
	"github.com/cloudburrow/cloudburrow/internal/iampolicy"
	"github.com/cloudburrow/cloudburrow/internal/resource"
)

// KMS is the part of Cloud KMS customer-managed keys use. The host's
// in-process *kms.Server is one through InProcess; Dial is one over gRPC.
type KMS interface {
	Encrypt(context.Context, *kmspb.EncryptRequest) (*kmspb.EncryptResponse, error)
	Decrypt(context.Context, *kmspb.DecryptRequest) (*kmspb.DecryptResponse, error)
	GetCryptoKey(context.Context, *kmspb.GetCryptoKeyRequest) (*kmspb.CryptoKey, error)
	GetIamPolicy(context.Context, *iampb.GetIamPolicyRequest) (*iampb.Policy, error)
}

// Checker is the host's IAM evaluator: iamcheck.Client.Check in the
// cluster, the enforcer's CheckRemote on the host. Nil checks nothing (IAM
// enforcement off).
type Checker func(context.Context, iamcheck.Request) (iamcheck.Decision, error)

// Keys is a service's customer-managed keys.
type Keys struct {
	// KMS is the instance's Cloud KMS, nil (or returning nil) when it is
	// not served: a function, as Cloud KMS may start after the service.
	KMS func() KMS
	// Check checks the service agent's use of a key; nil when IAM is not
	// enforced.
	Check Checker
}

// Kind is which way a key cannot be used.
type Kind int

const (
	// Invalid is a name that is not a CryptoKey's, or in the wrong
	// location.
	Invalid Kind = iota + 1
	// NoKMS is an instance without Cloud KMS.
	NoKMS
	// NotFound is a key that does not exist.
	NotFound
	// Unusable is a key or version that is not ENABLED, or a key whose
	// purpose is not ENCRYPT_DECRYPT.
	Unusable
	// PermissionDenied is a service agent without the role.
	PermissionDenied
	// Unavailable is Cloud KMS not answering.
	Unavailable
)

// Error is a key that cannot be used.
type Error struct {
	Kind Kind
	// Key is the CryptoKey (or version) named.
	Key string
	// Msg is the reason, without the key's name.
	Msg string
}

func (e *Error) Error() string { return e.Key + ": " + e.Msg }

// Code is the gRPC code Google answers for e on its gRPC APIs.
func (e *Error) Code() codes.Code {
	switch e.Kind {
	case Invalid:
		return codes.InvalidArgument
	case PermissionDenied:
		return codes.PermissionDenied
	case Unavailable:
		return codes.Unavailable
	}
	return codes.FailedPrecondition
}

// As is err's *Error, or nil.
func As(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return nil
}

var (
	keyRE     = regexp.MustCompile(`^projects/([^/]+)/locations/([^/]+)/keyRings/[^/]+/cryptoKeys/[^/]+$`)
	versionRE = regexp.MustCompile(`^(projects/[^/]+/locations/[^/]+/keyRings/[^/]+/cryptoKeys/[^/]+)/cryptoKeyVersions/[0-9]+$`)
)

// Location is a CryptoKey name's location, and false when name is not
// projects/*/locations/*/keyRings/*/cryptoKeys/*.
func Location(name string) (string, bool) {
	m := keyRE.FindStringSubmatch(name)
	if m == nil {
		return "", false
	}
	return m[2], true
}

// KeyOf is the CryptoKey a version belongs to, "" when version is not a
// CryptoKeyVersion's name.
func KeyOf(version string) string {
	if m := versionRE.FindStringSubmatch(version); m != nil {
		return m[1]
	}
	return ""
}

// CheckName checks name is a CryptoKey's and, when want is set, in that
// location; field names the request field.
func CheckName(field, name, want string) error {
	loc, ok := Location(name)
	if !ok {
		return &Error{Kind: Invalid, Key: name, Msg: fmt.Sprintf("%s must be projects/{project}/locations/{location}/keyRings/{key_ring}/cryptoKeys/{crypto_key}", field)}
	}
	if want != "" && loc != want {
		return &Error{Kind: Invalid, Key: name, Msg: fmt.Sprintf("%s is in location %s; it must be in %s", field, loc, want)}
	}
	return nil
}

// ServiceAgent is project's service agent at domain
// ("gcp-sa-logging.iam.gserviceaccount.com"):
// service-PROJECT_NUMBER@domain.
func ServiceAgent(project, domain string) string {
	return "service-" + strconv.FormatInt(resource.ProjectNumber(project), 10) + "@" + domain
}

func (k Keys) kms(key string) (KMS, error) {
	if k.KMS != nil {
		if s := k.KMS(); s != nil {
			return s, nil
		}
	}
	return nil, &Error{Kind: NoKMS, Key: key, Msg: "Cloud KMS is not served by this CloudBurrow instance: start it with kms in --services"}
}

// kmsError is a Cloud KMS refusal of key as an *Error.
func kmsError(key string, err error) error {
	st, _ := status.FromError(err)
	switch st.Code() {
	case codes.NotFound:
		return &Error{Kind: NotFound, Key: key, Msg: "the Cloud KMS key does not exist: " + st.Message()}
	case codes.PermissionDenied:
		return &Error{Kind: PermissionDenied, Key: key, Msg: st.Message()}
	case codes.Unavailable, codes.DeadlineExceeded, codes.Canceled:
		return &Error{Kind: Unavailable, Key: key, Msg: "Cloud KMS did not answer: " + st.Message()}
	case codes.InvalidArgument:
		return &Error{Kind: Invalid, Key: key, Msg: st.Message()}
	}
	return &Error{Kind: Unusable, Key: key, Msg: st.Message()}
}

// Usable checks key exists, encrypts and decrypts, and has an ENABLED
// primary version, and that agent (of project) may use it to encrypt.
func (k Keys) Usable(ctx context.Context, project, agent, key string) error {
	s, err := k.kms(key)
	if err != nil {
		return err
	}
	ck, err := s.GetCryptoKey(ctx, &kmspb.GetCryptoKeyRequest{Name: key})
	if err != nil {
		return kmsError(key, err)
	}
	if ck.GetPurpose() != kmspb.CryptoKey_ENCRYPT_DECRYPT {
		return &Error{Kind: Unusable, Key: key, Msg: "the key's purpose is " + ck.GetPurpose().String() + ", not ENCRYPT_DECRYPT"}
	}
	if p := ck.GetPrimary(); p == nil || p.GetState() != kmspb.CryptoKeyVersion_ENABLED {
		state := "missing"
		if p != nil {
			state = p.GetState().String()
		}
		return &Error{Kind: Unusable, Key: key, Msg: "the key's primary version is " + state + ", not ENABLED"}
	}
	return k.checkAgent(ctx, s, project, agent, key, "cloudkms.cryptoKeyVersions.useToEncrypt")
}

// checkAgent checks agent holds permission on key, with the key's and
// its key ring's own policies, when IAM is enforced.
func (k Keys) checkAgent(ctx context.Context, s KMS, project, agent, key, permission string) error {
	if k.Check == nil {
		return nil
	}
	ring := key[:strings.Index(key, "/cryptoKeys/")]
	var own []iamcheck.Policy
	for _, res := range []string{key, ring} {
		p, err := s.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: res, Options: &iampb.GetPolicyOptions{RequestedPolicyVersion: 3}})
		if err != nil {
			return kmsError(key, err)
		}
		stored := policyOf(p)
		own = append(own, iamcheck.Policy{Resource: "//cloudkms.googleapis.com/" + res, Policy: &stored})
	}
	m := keyRE.FindStringSubmatch(key)
	d, err := k.Check(ctx, iamcheck.Request{Signer: agent, Service: "cloudkms.googleapis.com", Permissions: []string{permission},
		Resource: key, Type: "cloudkms.googleapis.com/CryptoKey", Project: m[1], Own: own})
	if err != nil {
		return &Error{Kind: Unavailable, Key: key, Msg: "the IAM check did not answer: " + err.Error()}
	}
	if !d.Holds(permission) {
		return &Error{Kind: PermissionDenied, Key: key, Msg: "the service account " + agent + " does not have " + permission +
			" on the key: grant it roles/cloudkms.cryptoKeyEncrypterDecrypter"}
	}
	return nil
}

// Sealed is a value encrypted with a customer-managed key: the key version
// that wrapped its data key, the wrapped data key, and the ciphertext
// (nonce first).
type Sealed struct {
	KeyVersion string `json:"kmsKeyVersion"`
	Wrapped    []byte `json:"wrappedKey"`
	Ciphertext []byte `json:"ciphertext,omitempty"`
}

// Key is the CryptoKey s was sealed with.
func (s Sealed) Key() string { return KeyOf(s.KeyVersion) }

// Seal encrypts plaintext under key for agent of project; aad binds it to
// what it is (a resource's name). A nil plaintext only wraps a data key, so
// Open checks the key.
func (k Keys) Seal(ctx context.Context, project, agent, key string, plaintext, aad []byte) (Sealed, error) {
	s, err := k.kms(key)
	if err != nil {
		return Sealed{}, err
	}
	if err := k.checkAgent(ctx, s, project, agent, key, "cloudkms.cryptoKeyVersions.useToEncrypt"); err != nil {
		return Sealed{}, err
	}
	dek := make([]byte, 32)
	if _, err := rand.Read(dek); err != nil {
		return Sealed{}, err
	}
	resp, err := s.Encrypt(ctx, &kmspb.EncryptRequest{Name: key, Plaintext: dek, AdditionalAuthenticatedData: aad})
	if err != nil {
		return Sealed{}, kmsError(key, err)
	}
	out := Sealed{KeyVersion: resp.GetName(), Wrapped: resp.GetCiphertext()}
	if plaintext != nil {
		if out.Ciphertext, err = seal(dek, plaintext, aad); err != nil {
			return Sealed{}, err
		}
	}
	return out, nil
}

// Open decrypts sealed for agent of project: Cloud KMS must unwrap its data
// key, which it refuses while the version is disabled or destroyed.
func (k Keys) Open(ctx context.Context, project, agent string, sealed Sealed, aad []byte) ([]byte, error) {
	key := sealed.Key()
	if key == "" {
		return nil, &Error{Kind: Invalid, Key: sealed.KeyVersion, Msg: "not a CryptoKeyVersion"}
	}
	s, err := k.kms(key)
	if err != nil {
		return nil, err
	}
	if err := k.checkAgent(ctx, s, project, agent, key, "cloudkms.cryptoKeyVersions.useToDecrypt"); err != nil {
		return nil, err
	}
	resp, err := s.Decrypt(ctx, &kmspb.DecryptRequest{Name: key, Ciphertext: sealed.Wrapped, AdditionalAuthenticatedData: aad})
	if err != nil {
		e := kmsError(key, err).(*Error)
		if e.Kind == Unusable {
			e.Key = sealed.KeyVersion
		}
		return nil, e
	}
	if sealed.Ciphertext == nil {
		return nil, nil
	}
	return open(resp.GetPlaintext(), sealed.Ciphertext, aad)
}

func seal(dek, plaintext, aad []byte) ([]byte, error) {
	gcm, err := newGCM(dek)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, plaintext, aad), nil
}

func open(dek, ciphertext, aad []byte) ([]byte, error) {
	gcm, err := newGCM(dek)
	if err != nil {
		return nil, err
	}
	if len(ciphertext) < gcm.NonceSize() {
		return nil, errors.New("cmek: a sealed value too short to open")
	}
	pt, err := gcm.Open(nil, ciphertext[:gcm.NonceSize()], ciphertext[gcm.NonceSize():], aad)
	if err != nil {
		return nil, fmt.Errorf("cmek: the sealed value does not open: %w", err)
	}
	if pt == nil {
		pt = []byte{}
	}
	return pt, nil
}

func newGCM(dek []byte) (cipher.AEAD, error) {
	b, err := aes.NewCipher(dek)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(b)
}

// policyOf is a policy Cloud KMS returned, as the evaluator reads it.
func policyOf(p *iampb.Policy) iampolicy.Stored {
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

// InProcessServer is the host's Cloud KMS server.
type InProcessServer interface {
	Encrypt(context.Context, *kmspb.EncryptRequest) (*kmspb.EncryptResponse, error)
	Decrypt(context.Context, *kmspb.DecryptRequest) (*kmspb.DecryptResponse, error)
	GetCryptoKey(context.Context, *kmspb.GetCryptoKeyRequest) (*kmspb.CryptoKey, error)
	IAMPolicy() iampb.IAMPolicyServer
}

type inProcess struct {
	InProcessServer
}

func (p inProcess) GetIamPolicy(ctx context.Context, in *iampb.GetIamPolicyRequest) (*iampb.Policy, error) {
	return p.IAMPolicy().GetIamPolicy(ctx, in)
}

// InProcess is s as a KMS.
func InProcess(s InProcessServer) KMS { return inProcess{s} }

type remote struct {
	kms kmspb.KeyManagementServiceClient
	iam iampb.IAMPolicyClient
}

func (r remote) Encrypt(ctx context.Context, in *kmspb.EncryptRequest) (*kmspb.EncryptResponse, error) {
	return r.kms.Encrypt(ctx, in)
}

func (r remote) Decrypt(ctx context.Context, in *kmspb.DecryptRequest) (*kmspb.DecryptResponse, error) {
	return r.kms.Decrypt(ctx, in)
}

func (r remote) GetCryptoKey(ctx context.Context, in *kmspb.GetCryptoKeyRequest) (*kmspb.CryptoKey, error) {
	return r.kms.GetCryptoKey(ctx, in)
}

func (r remote) GetIamPolicy(ctx context.Context, in *iampb.GetIamPolicyRequest) (*iampb.Policy, error) {
	return r.iam.GetIamPolicy(ctx, in)
}

// Dial is the Cloud KMS at addr (host:port, plaintext gRPC, as the instance
// serves it to the cluster through cloudburrow-host).
func Dial(addr string) (KMS, io.Closer, error) {
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, nil, err
	}
	return remote{kmspb.NewKeyManagementServiceClient(conn), iampb.NewIAMPolicyClient(conn)}, conn, nil
}

// Remote is c's Check, nil when c is not enabled.
func Remote(c *iamcheck.Client) Checker {
	if !c.Enabled() {
		return nil
	}
	return c.Check
}
