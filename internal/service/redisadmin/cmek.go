package redisadmin

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"regexp"
	"time"

	redispb "cloud.google.com/go/redis/apiv1/redispb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// Customer-managed encryption keys (#1463). An instance created with
// customer_managed_key has a data key of its own, 256-bit AES, wrapped
// (Encrypt) by that Cloud KMS key and kept only wrapped, as Memorystore
// documents CMEK. What Memorystore encrypts with the key is "customer
// data that are stored in persistent storage": the instance's kept
// metadata of its AUTH string and of in-transit encryption (the CA's
// private key) is kept sealed with the data key (AES-GCM), never in the
// clear; and the server's RDB snapshots on its volume are served only
// while the key can be used. Exports are not: Memorystore protects an
// export with the destination bucket's own CMEK.
//
// The key is checked, by unwrapping the data key, when the instance is
// created, when CloudBurrow starts it again, and every keyEvery. If the
// key (its primary version, or the version that wrapped the data key) is
// disabled or destroyed, or cannot be used, the instance is suspended as
// Memorystore suspends one: no READY state (STATE_UNSPECIFIED, which the
// console shows as "No state"), suspension_reasons
// CUSTOMER_MANAGED_KEY_ISSUE, and its server stopped (its volume kept),
// so no client reaches the data. Once the key can be used again the
// instance resumes on its own: its server runs again (REPAIRING), with
// its data, and is READY.

// KeyService is Cloud KMS: Encrypt and Decrypt with a CryptoKey.
type KeyService interface {
	Encrypt(ctx context.Context, key string, plaintext []byte) ([]byte, error)
	Decrypt(ctx context.Context, key string, ciphertext []byte) ([]byte, error)
}

// Suspender stops a server and keeps its data: a suspended instance's
// servers. A Provisioner that is one is used to suspend.
type Suspender interface {
	Suspend(ctx context.Context, server string) error
}

// UseKMS makes customer_managed_key work, over k: without it, an
// instance with one is refused, as Cloud KMS is not enabled.
func (s *Server) UseKMS(k KeyService) {
	if s.created != nil {
		s.created.kms = k
		go s.created.watchKeys(s.created.bg, keyEvery)
	}
}

// keyEvery is how often a CMEK instance's key is checked.
var keyEvery = 2 * time.Second

var cryptoKeyRE = regexp.MustCompile(`^projects/([^/]+)/locations/([^/]+)/keyRings/([^/]+)/cryptoKeys/([^/]+)$`)

// sealed is what CMEK keeps sealed with the data key.
type sealed struct {
	AuthString   string `json:"authString,omitempty"`
	TransitCAKey string `json:"transitCaKey,omitempty"`
}

// checkKey is Google's rule for customer_managed_key: a CryptoKey in the
// instance's region.
func checkKey(key, region string) error {
	m := cryptoKeyRE.FindStringSubmatch(key)
	if m == nil {
		return apierror.InvalidArgument("customer_managed_key must be projects/{project}/locations/{location}/keyRings/{key_ring}/cryptoKeys/{crypto_key}, not %q", key)
	}
	if m[2] != region {
		return apierror.InvalidArgument("customer_managed_key %s must be in the instance's region %s: the key, key ring and instance must be in the same region", key, region)
	}
	return nil
}

// newDataKey makes r's data key and wraps it with its CryptoKey.
func (c *instances) newDataKey(ctx context.Context, r *record) error {
	if c.kms == nil {
		return apierror.FailedPrecondition("customer_managed_key needs Cloud KMS: run up with kms in --services")
	}
	dek := make([]byte, 32)
	if _, err := rand.Read(dek); err != nil {
		return apierror.Internal(err, "generate the data key")
	}
	wrapped, err := c.kms.Encrypt(ctx, r.CustomerManagedKey, dek)
	if err != nil {
		return keyError(r.CustomerManagedKey, err)
	}
	r.WrappedKey, r.dek = wrapped, dek
	return nil
}

// keyError is why a CryptoKey cannot be used, as Google says it.
func keyError(key string, err error) error {
	var ae *apierror.Error
	msg := err.Error()
	if errors.As(err, &ae) {
		msg = ae.Message
	}
	return apierror.FailedPrecondition("the customer-managed key %s cannot be used: %s", key, msg)
}

// unwrap is r's data key, unwrapped by its CryptoKey: an error when the
// key cannot be used.
func (c *instances) unwrap(ctx context.Context, key string, wrapped []byte) ([]byte, error) {
	if c.kms == nil {
		return nil, errors.New("Cloud KMS is not enabled")
	}
	return c.kms.Decrypt(ctx, key, wrapped)
}

func seal(dek, plain []byte) ([]byte, error) {
	gcm, err := newGCM(dek)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, plain, nil), nil
}

func open(dek, box []byte) ([]byte, error) {
	gcm, err := newGCM(dek)
	if err != nil {
		return nil, err
	}
	if len(box) < gcm.NonceSize() {
		return nil, errors.New("the sealed metadata is too short")
	}
	return gcm.Open(nil, box[:gcm.NonceSize()], box[gcm.NonceSize():], nil)
}

func newGCM(dek []byte) (cipher.AEAD, error) {
	b, err := aes.NewCipher(dek)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(b)
}

// kept is r as it is written: with CMEK, its secrets sealed with the
// data key and not in the clear.
func (r *record) kept() (*record, error) {
	if r.CustomerManagedKey == "" {
		return r, nil
	}
	if r.dek == nil {
		return nil, errors.New("the instance's data key is not unwrapped")
	}
	b, err := json.Marshal(sealed{AuthString: r.AuthString, TransitCAKey: r.TransitCAKey})
	if err != nil {
		return nil, err
	}
	out := *r
	if out.Sealed, err = seal(r.dek, b); err != nil {
		return nil, err
	}
	out.AuthString, out.TransitCAKey = "", ""
	return &out, nil
}

// unseal restores r's secrets with its data key dek.
func (r *record) unseal(dek []byte) error {
	r.dek = dek
	if len(r.Sealed) == 0 {
		return nil
	}
	b, err := open(dek, r.Sealed)
	if err != nil {
		return fmt.Errorf("open the instance's sealed metadata: %w", err)
	}
	var s sealed
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	r.AuthString, r.TransitCAKey = s.AuthString, s.TransitCAKey
	return nil
}

// suspend marks r suspended, why, and stops its servers. Call with mu
// held.
func (c *instances) suspend(r *record, why string) {
	if r.suspended {
		r.message = why
		return
	}
	r.suspended = true
	r.state, r.message = redispb.Instance_STATE_UNSPECIFIED, why
	r.closeEndpoints()
	if sp, ok := c.prov.(Suspender); ok {
		servers := r.servers()
		go func() {
			for _, s := range servers {
				if err := sp.Suspend(c.bg, s); err != nil {
					log.Printf("memorystore: suspend %s: %v", s, err)
				}
			}
		}()
	}
}

// watchKeys checks every CMEK instance's key until ctx ends.
func (c *instances) watchKeys(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		c.checkKeys(ctx)
	}
}

// checkKeys suspends each CMEK instance whose key cannot be used, and
// resumes each suspended one whose key can be again.
func (c *instances) checkKeys(ctx context.Context) {
	type target struct {
		name, key string
		wrapped   []byte
	}
	var ts []target
	c.mu.Lock()
	for name, r := range c.byID {
		if _, pending := c.busy[name]; pending || r.CustomerManagedKey == "" || r.state == redispb.Instance_DELETING {
			continue
		}
		ts = append(ts, target{name, r.CustomerManagedKey, r.WrappedKey})
	}
	c.mu.Unlock()
	for _, tg := range ts {
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		dek, err := c.unwrap(cctx, tg.key, tg.wrapped)
		cancel()
		c.mu.Lock()
		r, ok := c.byID[tg.name]
		if _, pending := c.busy[tg.name]; !ok || pending {
			c.mu.Unlock()
			continue
		}
		if err != nil {
			c.suspend(r, "the instance is suspended: its customer-managed key "+tg.key+" cannot be used: "+keyError(tg.key, err).(*apierror.Error).Message)
			c.mu.Unlock()
			continue
		}
		if !r.suspended {
			c.mu.Unlock()
			continue
		}
		if err := r.unseal(dek); err != nil {
			r.message = err.Error()
			c.mu.Unlock()
			continue
		}
		// Resume: the server runs again over its kept volume.
		r.suspended = false
		r.state, r.message = redispb.Instance_REPAIRING, "the customer-managed key can be used again: the instance is resuming"
		c.busy[tg.name] = "resume"
		spec := r.spec()
		c.mu.Unlock()
		go c.run(tg.name, "", spec)
	}
}

// renderCMEK sets inst's customer_managed_key and suspension_reasons.
func (r *record) renderCMEK(inst *redispb.Instance) {
	inst.CustomerManagedKey = r.CustomerManagedKey
	if r.suspended {
		inst.SuspensionReasons = []redispb.Instance_SuspensionReason{redispb.Instance_CUSTOMER_MANAGED_KEY_ISSUE}
	}
}
