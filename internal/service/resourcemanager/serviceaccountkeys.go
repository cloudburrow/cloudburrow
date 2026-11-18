package resourcemanager

// Service account keys (#1747): google.iam.admin.v1.IAM's
// CreateServiceAccountKey, UploadServiceAccountKey, ListServiceAccountKeys,
// GetServiceAccountKey, DeleteServiceAccountKey, DisableServiceAccountKey
// and EnableServiceAccountKey, as Google documents them
// (docs.cloud.google.com/iam/docs/keys-create-delete, .../keys-upload,
// .../iam/quotas, read 2026-11-17):
//
//   - an account has at most 10 user-managed keys, disabled ones included;
//     an 11th is FAILED_PRECONDITION;
//   - a created key is an RSA 2048 key in a JSON credentials file
//     (TYPE_GOOGLE_CREDENTIALS_FILE, the default), returned once and never
//     kept: only its public half, as a self-signed X.509 certificate, is;
//     the file's token_uri is this instance's token endpoint, so a client
//     that signs in with it never reaches Google;
//   - an uploaded key is an X.509 certificate of an RSA 2048 public key;
//   - a user-managed key is valid until 9999-12-31T23:59:59Z (no
//     iam.serviceAccountKeyExpiryHours constraint is modelled);
//   - every account also has a system-managed key, rotated after two
//     weeks, which signBlob and signJwt sign with: listed with keyTypes
//     SYSTEM_MANAGED, and not deletable.
//
// The private key of a system-managed key is kept, as Google keeps it; it
// is CloudBurrow's own test material and never leaves the store but as a
// signature. A created key may instead be a PKCS#12 file
// (TYPE_PKCS12_FILE, password "notasecret", pkcs12.go) and an RSA 1024 key
// (KEY_ALG_RSA_1024), the REST reference's other values (#1750).

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/url"
	"strings"
	"time"

	"cloud.google.com/go/iam/admin/apiv1/adminpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

const (
	maxUserKeys       = 10
	systemKeyLifetime = 14 * 24 * time.Hour
)

// userKeyExpiry is when a user-managed key expires: never, as Google
// writes it.
var userKeyExpiry = time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)

// saKey is one key of an account.
type saKey struct {
	ID          string    `json:"id"`
	System      bool      `json:"system,omitempty"`
	Uploaded    bool      `json:"uploaded,omitempty"`
	CertPEM     string    `json:"cert"`
	PrivatePEM  string    `json:"private,omitempty"` // system-managed keys only
	ValidAfter  time.Time `json:"validAfter"`
	ValidBefore time.Time `json:"validBefore"`
	Disabled    bool      `json:"disabled,omitempty"`
	// Deleted keys are kept, hidden, so the token endpoint refuses an
	// assertion signed with one rather than taking it as a stranger's.
	Deleted bool `json:"deleted,omitempty"`
}

func (k *saKey) privateKey() (*rsa.PrivateKey, error) {
	b, _ := pem.Decode([]byte(k.PrivatePEM))
	if b == nil {
		return nil, apierror.Internal(nil, "key %s has no private key", k.ID)
	}
	key, err := x509.ParsePKCS1PrivateKey(b.Bytes)
	if err != nil {
		return nil, apierror.Internal(err, "decode key %s", k.ID)
	}
	return key, nil
}

func (k *saKey) proto(a *serviceAccount, public adminpb.ServiceAccountPublicKeyType) *adminpb.ServiceAccountKey {
	out := &adminpb.ServiceAccountKey{
		Name:            a.name() + "/keys/" + k.ID,
		KeyAlgorithm:    adminpb.ServiceAccountKeyAlgorithm_KEY_ALG_RSA_2048,
		ValidAfterTime:  timestamppb.New(k.ValidAfter),
		ValidBeforeTime: timestamppb.New(k.ValidBefore),
		KeyOrigin:       adminpb.ServiceAccountKeyOrigin_GOOGLE_PROVIDED,
		KeyType:         adminpb.ListServiceAccountKeysRequest_USER_MANAGED,
		Disabled:        k.Disabled,
	}
	if pub := k.publicKey(); pub != nil && pub.N.BitLen() <= 1024 {
		out.KeyAlgorithm = adminpb.ServiceAccountKeyAlgorithm_KEY_ALG_RSA_1024
	}
	if k.Uploaded {
		out.KeyOrigin = adminpb.ServiceAccountKeyOrigin_USER_PROVIDED
	}
	if k.System {
		out.KeyType = adminpb.ListServiceAccountKeysRequest_SYSTEM_MANAGED
	}
	switch public {
	case adminpb.ServiceAccountPublicKeyType_TYPE_X509_PEM_FILE:
		out.PublicKeyData = []byte(k.CertPEM)
	case adminpb.ServiceAccountPublicKeyType_TYPE_RAW_PUBLIC_KEY:
		if pub := k.publicKey(); pub != nil {
			der, _ := x509.MarshalPKIXPublicKey(pub)
			out.PublicKeyData = pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
		}
	}
	return out
}

func (k *saKey) publicKey() *rsa.PublicKey {
	b, _ := pem.Decode([]byte(k.CertPEM))
	if b == nil {
		return nil
	}
	c, err := x509.ParseCertificate(b.Bytes)
	if err != nil {
		return nil
	}
	pub, _ := c.PublicKey.(*rsa.PublicKey)
	return pub
}

func newKeyID() string {
	b := make([]byte, 20)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// newKey generates an RSA 2048 key and its self-signed certificate.
func newKey(a *serviceAccount, now time.Time, until time.Time) (*rsa.PrivateKey, *saKey, error) {
	return newKeyBits(a, now, until, 2048)
}

// newKeyBits generates an RSA key of bits and its self-signed certificate.
func newKeyBits(a *serviceAccount, now time.Time, until time.Time, bits int) (*rsa.PrivateKey, *saKey, error) {
	priv, err := rsa.GenerateKey(rand.Reader, bits)
	if err != nil {
		return nil, nil, apierror.Internal(err, "generate key")
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 62))
	tmpl := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: a.email()},
		NotBefore: now.UTC().Truncate(time.Second), NotAfter: until, KeyUsage: x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		return nil, nil, apierror.Internal(err, "certify key")
	}
	k := &saKey{ID: newKeyID(), CertPEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		ValidAfter: tmpl.NotBefore, ValidBefore: until}
	return priv, k, nil
}

// systemKey is the account's current system-managed key, nil when it has
// expired.
func (a *serviceAccount) systemKey(now time.Time) *saKey {
	for i := len(a.Keys) - 1; i >= 0; i-- {
		if k := &a.Keys[i]; k.System && now.Before(k.ValidBefore) {
			return k
		}
	}
	return nil
}

// rotateSystemKey replaces the account's system-managed keys with a new one.
func (a *serviceAccount) rotateSystemKey(now time.Time) error {
	priv, k, err := newKey(a, now, now.UTC().Truncate(time.Second).Add(systemKeyLifetime))
	if err != nil {
		return err
	}
	k.System = true
	k.PrivatePEM = string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(priv)}))
	kept := a.Keys[:0]
	for _, old := range a.Keys {
		if !old.System {
			kept = append(kept, old)
		}
	}
	a.Keys = append(kept, *k)
	return nil
}

func (a *serviceAccount) userKeys() int {
	n := 0
	for _, k := range a.Keys {
		if !k.System && !k.Deleted {
			n++
		}
	}
	return n
}

// SetServiceAccountTokenURI is the token endpoint a created key's JSON file
// names: this instance's, so the file never sends a client to Google.
func (r *Registry) SetServiceAccountTokenURI(uri string) {
	r.mu.Lock()
	r.saTokenURI = uri
	r.unlock()
}

// keyCredentialsFile is the JSON layout of a service account key file.
type keyCredentialsFile struct {
	Type                string `json:"type"`
	ProjectID           string `json:"project_id"`
	PrivateKeyID        string `json:"private_key_id"`
	PrivateKey          string `json:"private_key"`
	ClientEmail         string `json:"client_email"`
	ClientID            string `json:"client_id"`
	AuthURI             string `json:"auth_uri"`
	TokenURI            string `json:"token_uri"`
	AuthProviderCertURL string `json:"auth_provider_x509_cert_url"`
	ClientCertURL       string `json:"client_x509_cert_url"`
	UniverseDomain      string `json:"universe_domain"`
}

// CreateServiceAccountKey makes a user-managed key and returns its private
// half, once.
func (r *Registry) CreateServiceAccountKey(req *adminpb.CreateServiceAccountKeyRequest) (*adminpb.ServiceAccountKey, error) {
	keyType := req.GetPrivateKeyType()
	switch keyType {
	case adminpb.ServiceAccountPrivateKeyType_TYPE_UNSPECIFIED:
		keyType = adminpb.ServiceAccountPrivateKeyType_TYPE_GOOGLE_CREDENTIALS_FILE
	case adminpb.ServiceAccountPrivateKeyType_TYPE_GOOGLE_CREDENTIALS_FILE, adminpb.ServiceAccountPrivateKeyType_TYPE_PKCS12_FILE:
	default:
		return nil, apierror.InvalidArgument("Invalid private key type %s.", keyType)
	}
	bits := 2048
	switch req.GetKeyAlgorithm() {
	case adminpb.ServiceAccountKeyAlgorithm_KEY_ALG_UNSPECIFIED, adminpb.ServiceAccountKeyAlgorithm_KEY_ALG_RSA_2048:
	case adminpb.ServiceAccountKeyAlgorithm_KEY_ALG_RSA_1024:
		bits = 1024
	default:
		return nil, apierror.InvalidArgument("Invalid key algorithm %s.", req.GetKeyAlgorithm())
	}
	r.mu.Lock()
	defer r.unlock()
	a, err := r.findSALocked(req.GetName(), false)
	if err != nil {
		return nil, err
	}
	if a.userKeys() >= maxUserKeys {
		return nil, apierror.FailedPrecondition("Precondition check failed.")
	}
	priv, k, err := newKeyBits(a, r.now(), userKeyExpiry, bits)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, apierror.Internal(err, "encode key")
	}
	var file []byte
	if keyType == adminpb.ServiceAccountPrivateKeyType_TYPE_PKCS12_FILE {
		cert, _ := pem.Decode([]byte(k.CertPEM))
		if file, err = encodePKCS12(der, cert.Bytes); err != nil {
			return nil, apierror.Internal(err, "encode the PKCS#12 key file")
		}
	} else if file, err = r.keyFileLocked(a, k, der); err != nil {
		return nil, err
	}
	a.Keys = append(a.Keys, *k)
	if err := r.putSALocked(a); err != nil {
		return nil, err
	}
	out := k.proto(a, adminpb.ServiceAccountPublicKeyType_TYPE_NONE)
	out.PrivateKeyType = keyType
	out.PrivateKeyData = file
	return out, nil
}

// keyFileLocked is a JSON key file of a key whose PKCS#8 private half is
// der. The caller holds r.mu.
func (r *Registry) keyFileLocked(a *serviceAccount, k *saKey, der []byte) ([]byte, error) {
	base := strings.TrimSuffix(r.saTokenURI, "/token")
	file, err := json.MarshalIndent(keyCredentialsFile{
		Type: "service_account", ProjectID: a.Project, PrivateKeyID: k.ID,
		PrivateKey:  string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
		ClientEmail: a.email(), ClientID: a.UniqueID, AuthURI: r.saTokenURI, TokenURI: r.saTokenURI,
		AuthProviderCertURL: base + "/certs", ClientCertURL: base + "/robot/v1/metadata/x509/" + strings.ReplaceAll(url.PathEscape(a.email()), "@", "%40"), UniverseDomain: "googleapis.com",
	}, "", "  ")
	if err != nil {
		return nil, apierror.Internal(err, "encode key file")
	}
	return file, nil
}

// UploadServiceAccountKey adds a user-managed key from an X.509
// certificate of an RSA 2048 public key.
func (r *Registry) UploadServiceAccountKey(name string, data []byte) (*adminpb.ServiceAccountKey, error) {
	b, _ := pem.Decode(data)
	if b == nil || b.Type != "CERTIFICATE" {
		return nil, apierror.InvalidArgument("The given public key data is not a valid X.509 certificate in PEM format.")
	}
	cert, err := x509.ParseCertificate(b.Bytes)
	if err != nil {
		return nil, apierror.InvalidArgument("The given public key data is not a valid X.509 certificate in PEM format.")
	}
	pub, ok := cert.PublicKey.(*rsa.PublicKey)
	if !ok || pub.N.BitLen() != 2048 {
		return nil, apierror.InvalidArgument("The public key must be an RSA 2048-bit key.")
	}
	r.mu.Lock()
	defer r.unlock()
	a, err := r.findSALocked(name, false)
	if err != nil {
		return nil, err
	}
	if a.userKeys() >= maxUserKeys {
		return nil, apierror.FailedPrecondition("Precondition check failed.")
	}
	for _, k := range a.Keys {
		if !k.Deleted && k.CertPEM == string(pem.EncodeToMemory(b)) {
			return nil, apierror.AlreadyExists("The key already exists for this service account.")
		}
	}
	k := saKey{ID: newKeyID(), Uploaded: true, CertPEM: string(pem.EncodeToMemory(b)),
		ValidAfter: cert.NotBefore.UTC(), ValidBefore: cert.NotAfter.UTC()}
	a.Keys = append(a.Keys, k)
	if err := r.putSALocked(a); err != nil {
		return nil, err
	}
	return k.proto(a, adminpb.ServiceAccountPublicKeyType_TYPE_NONE), nil
}

// ListServiceAccountKeys lists an account's keys of the types asked, both
// when none is.
func (r *Registry) ListServiceAccountKeys(req *adminpb.ListServiceAccountKeysRequest) (*adminpb.ListServiceAccountKeysResponse, error) {
	r.mu.Lock()
	defer r.unlock()
	a, err := r.findSALocked(req.GetName(), false)
	if err != nil {
		return nil, err
	}
	if a.systemKey(r.now()) == nil {
		if err := a.rotateSystemKey(r.now()); err != nil {
			return nil, err
		}
		if err := r.putSALocked(a); err != nil {
			return nil, err
		}
	}
	want := map[adminpb.ListServiceAccountKeysRequest_KeyType]bool{}
	for _, t := range req.GetKeyTypes() {
		want[t] = true
	}
	out := &adminpb.ListServiceAccountKeysResponse{}
	for i := range a.Keys {
		if a.Keys[i].Deleted {
			continue
		}
		k := a.Keys[i].proto(a, adminpb.ServiceAccountPublicKeyType_TYPE_NONE)
		if len(want) > 0 && !want[k.KeyType] {
			continue
		}
		out.Keys = append(out.Keys, k)
	}
	return out, nil
}

// keyLocked resolves "{account name}/keys/{id}".
func (r *Registry) keyLocked(name string) (*serviceAccount, int, error) {
	acct, id, ok := strings.Cut(name, "/keys/")
	if !ok || id == "" || strings.Contains(id, "/") {
		return nil, 0, apierror.InvalidArgument("Invalid key name %q: it must be projects/{project}/serviceAccounts/{account}/keys/{key}.", name)
	}
	a, err := r.findSALocked(acct, false)
	if err != nil {
		return nil, 0, err
	}
	for i := range a.Keys {
		if a.Keys[i].ID == id && !a.Keys[i].Deleted {
			return a, i, nil
		}
	}
	return nil, 0, apierror.NotFound("Unknown service account key %s", id)
}

// GetServiceAccountKey reads one key, with its public half as asked.
func (r *Registry) GetServiceAccountKey(name string, public adminpb.ServiceAccountPublicKeyType) (*adminpb.ServiceAccountKey, error) {
	r.mu.Lock()
	defer r.unlock()
	a, i, err := r.keyLocked(name)
	if err != nil {
		return nil, err
	}
	return a.Keys[i].proto(a, public), nil
}

// DeleteServiceAccountKey deletes a user-managed key.
func (r *Registry) DeleteServiceAccountKey(name string) error {
	r.mu.Lock()
	defer r.unlock()
	a, i, err := r.keyLocked(name)
	if err != nil {
		return err
	}
	if a.Keys[i].System {
		return apierror.FailedPrecondition("A system-managed key cannot be deleted.")
	}
	a.Keys[i].Deleted = true
	return r.putSALocked(a)
}

// SetServiceAccountKeyDisabled disables or enables a user-managed key.
func (r *Registry) SetServiceAccountKeyDisabled(name string, disabled bool) error {
	r.mu.Lock()
	defer r.unlock()
	a, i, err := r.keyLocked(name)
	if err != nil {
		return err
	}
	if a.Keys[i].System {
		return apierror.FailedPrecondition("A system-managed key cannot be disabled or enabled.")
	}
	a.Keys[i].Disabled = disabled
	return r.putSALocked(a)
}

// ServiceAccountKeyState is the public key a key ID names, with the email
// of its account: known is whether any account has the key, usable whether
// the key and its account are live, enabled and the key valid at now. It
// is what the token endpoint accepts a key file's assertion with.
func (r *Registry) ServiceAccountKeyState(keyID string, now time.Time) (pub *rsa.PublicKey, email string, known, usable bool) {
	if keyID == "" {
		return nil, "", false, false
	}
	r.mu.Lock()
	defer r.unlock()
	all, err := r.accountsLocked("")
	if err != nil {
		return nil, "", false, false
	}
	for _, a := range all {
		for _, k := range a.Keys {
			if k.ID != keyID {
				continue
			}
			pub := k.publicKey()
			ok := !a.Deleted && !a.Disabled && !k.Disabled && !k.Deleted && !now.Before(k.ValidAfter) && now.Before(k.ValidBefore) && pub != nil
			return pub, a.email(), true, ok
		}
	}
	return nil, "", false, false
}

// ServiceAccountCert is one public key of an account as its public
// certificate URLs serve it (#1750).
type ServiceAccountCert struct {
	ID      string
	CertPEM string
	Key     *rsa.PublicKey
}

// ServiceAccountPublicCerts are the certificates of a live account's
// enabled, valid keys, its system-managed key first (rotated when it has
// expired, so it is the key signBlob and signJwt sign with), by email or
// unique ID: what https://www.googleapis.com/service_accounts/v1/metadata/
// {x509,jwk,raw}/EMAIL serve on Google. ok is false for an account that
// is not live.
func (r *Registry) ServiceAccountPublicCerts(account string) (certs []ServiceAccountCert, ok bool) {
	r.mu.Lock()
	defer r.unlock()
	a, err := r.findSALocked("projects/-/serviceAccounts/"+account, false)
	if err != nil {
		return nil, false
	}
	now := r.now()
	if a.systemKey(now) == nil {
		if err := a.rotateSystemKey(now); err == nil {
			_ = r.putSALocked(a)
		}
	}
	var sys, user []ServiceAccountCert
	for _, k := range a.Keys {
		if k.Disabled || k.Deleted || now.Before(k.ValidAfter) || !now.Before(k.ValidBefore) {
			continue
		}
		pub := k.publicKey()
		if pub == nil {
			continue
		}
		c := ServiceAccountCert{ID: k.ID, CertPEM: k.CertPEM, Key: pub}
		if k.System {
			sys = append(sys, c)
		} else {
			user = append(user, c)
		}
	}
	return append(sys, user...), true
}
