package redisadmin

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"time"

	redispb "cloud.google.com/go/redis/apiv1/redispb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// Server CA rotation (#1463), as Memorystore documents it for in-transit
// encryption: "five years after you create the instance, Memorystore for
// Redis generates a new CA and makes it available", and "the old CA
// remains valid until its expiration date", ten years after it was
// created, so server_ca_certs lists both and a client installs both. The
// server's certificate is then issued by the new CA when it is next
// issued (it is every 180 days, and each time the server starts), and
// clients that trust every CA listed keep connecting.
//
// Google exposes no API to rotate sooner: a customer "file[s] a support
// ticket to request a CA rotation". RotateServerCA is that request, which
// CloudBurrow's console makes: a new CA at once, listed beside the old
// one, and the server started again with a certificate of the new CA (a
// transient connection drop, as a certificate rotation is on Memorystore).

// caRotateAfter is when Memorystore makes a new CA available.
const caRotateAfter = 5 * 365 * 24 * time.Hour

// caCreated is when r's newest CA was created; zero if it does not parse.
func (r *record) caCreated() time.Time {
	b, _ := pem.Decode([]byte(r.TransitCA))
	if b == nil {
		return time.Time{}
	}
	cert, err := x509.ParseCertificate(b.Bytes)
	if err != nil {
		return time.Time{}
	}
	return cert.NotBefore.Add(time.Hour)
}

// rotateCA gives r a new CA, keeping the old ones listed until they
// expire. Call with mu held.
func (c *instances) rotateCA(r *record) error {
	cert, key, err := newCA(r.Name, c.now())
	if err != nil {
		return err
	}
	next := *r
	next.PreviousCAs = r.caPEM()
	next.TransitCA, next.TransitCAKey = cert, key
	if err := c.save(&next); err != nil {
		return err
	}
	*r = next
	return nil
}

// RotateServerCA rotates a created instance's server CA now: what a
// support ticket asks Memorystore for. The instance must use in-transit
// encryption and be READY. Its server is started again, with a
// certificate of the new CA.
func (s *Server) RotateServerCA(ctx context.Context, name string) error {
	isDefault, err := s.find(name)
	if err != nil {
		return err
	}
	if isDefault {
		return apierror.FailedPrecondition("instance %s is the server `cloudburrow up` runs, which has no in-transit encryption", name)
	}
	c := s.created
	c.mu.Lock()
	defer c.mu.Unlock()
	r := c.byID[name]
	if !r.tlsOn() {
		return apierror.FailedPrecondition("instance %s has no in-transit encryption: there is no server CA to rotate", name)
	}
	if _, pending := c.busy[name]; pending || r.state != redispb.Instance_READY {
		return apierror.FailedPrecondition("instance %s is %s: a server CA is rotated only when it is READY", name, r.state)
	}
	if err := c.rotateCA(r); err != nil {
		return apierror.Internal(err, "rotate the server CA of %s", name)
	}
	c.restart(r)
	return nil
}

// restart runs r's servers again with its spec: a new certificate. Call
// with mu held.
func (c *instances) restart(r *record) {
	c.busy[r.Name] = "certificate rotation"
	spec := r.spec()
	go c.run(r.Name, "", spec)
}

// rotateDue rotates the CA of every READY instance whose CA is five years
// old, as Memorystore does.
func (c *instances) rotateDue() {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	for name, r := range c.byID {
		if _, pending := c.busy[name]; pending || !r.tlsOn() || r.state != redispb.Instance_READY {
			continue
		}
		if created := r.caCreated(); created.IsZero() || now.Sub(created) < caRotateAfter {
			continue
		}
		if err := c.rotateCA(r); err == nil {
			c.restart(r)
		}
	}
}
