package privateca

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/pem"
	"regexp"
	"strings"
	"time"

	"cloud.google.com/go/security/privateca/apiv1/privatecapb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// ObjectWriter uploads an object to the instance's Cloud Storage.
type ObjectWriter interface {
	WriteObject(ctx context.Context, bucket, name, contentType string, data []byte) error
}

// UseStorage gives the server the instance's Cloud Storage: a CA with a
// gcs_bucket publishes "content, such as the CA certificate and CRLs" to
// that bucket, and its access URLs are the objects' URLs at addr (host:port
// of the storage endpoint).
func (s *Server) UseStorage(w ObjectWriter, addr func() string) {
	s.objects, s.storageAddr = w, addr
}

// bucketRE is a Cloud Storage bucket name: gcs_bucket "must be a bucket
// name, without any prefixes (such as `gs://`) or suffixes (such as
// `.googleapis.com`)".
var bucketRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{1,220}[a-z0-9]$`)

func checkBucket(b string) error {
	if b == "" {
		return nil
	}
	if !bucketRE.MatchString(b) {
		return apierror.InvalidArgument("certificate_authority.gcs_bucket %q must be a bucket name, without any prefixes (such as gs://) or suffixes (such as .googleapis.com)", b)
	}
	return nil
}

// contentID is the folder of a CA's content in its bucket, stable for the
// CA: Google names it with an ID of its own under the bucket.
func contentID(ca *privatecapb.CertificateAuthority) string {
	sum := sha256.Sum256([]byte(ca.GetName() + "\x00" + ca.GetCreateTime().AsTime().UTC().Format(time.RFC3339Nano)))
	return hex.EncodeToString(sum[:10])
}

// bucketBase is the URL a gcs_bucket CA's content is under, or "".
func (s *Server) bucketBase(ca *privatecapb.CertificateAuthority) string {
	if s.storageAddr == nil || s.storageAddr() == "" {
		return ""
	}
	return "http://" + s.storageAddr() + "/" + ca.GetGcsBucket() + "/" + contentID(ca)
}

// publish uploads a gcs_bucket CA's CA certificate and CRL, as its pool
// publishes them, in the pool's encoding. The caller holds mu.
func (s *Server) publish(caName string) error {
	c, ok := s.cas[caName]
	if !ok || c.CA.GetGcsBucket() == "" || s.objects == nil {
		return nil
	}
	u := c.CA.GetAccessUrls()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	bucket, dir := c.CA.GetGcsBucket(), contentID(c.CA)
	pool := s.pools[poolOf(caName)]
	asDER := pool != nil && pool.Pool.GetPublishingOptions().GetEncodingFormat() == privatecapb.CaPool_PublishingOptions_DER
	if u.GetCaCertificateAccessUrl() != "" && c.Cert != nil {
		body, ctype := []byte(c.CA.GetPemCaCertificates()[0]), "application/x-pem-file"
		if asDER {
			body, ctype = c.Cert.Raw, "application/pkix-cert"
		}
		if err := s.objects.WriteObject(ctx, bucket, dir+"/ca.crt", ctype, body); err != nil {
			return apierror.FailedPrecondition("publish the CA certificate of %s to gs://%s: %v", caName, bucket, err)
		}
	}
	if len(u.GetCrlAccessUrls()) > 0 {
		if crl := s.crls[s.crlFor(caName)]; crl != nil {
			body, ctype := []byte(crl.GetPemCrl()), "application/x-pem-file"
			if asDER {
				if blk, _ := pem.Decode(body); blk != nil {
					body, ctype = blk.Bytes, "application/pkix-crl"
				}
			}
			if err := s.objects.WriteObject(ctx, bucket, dir+"/crl.crl", ctype, body); err != nil {
				return apierror.FailedPrecondition("publish the CRL of %s to gs://%s: %v", caName, bucket, err)
			}
		}
	}
	return nil
}

func poolOf(caName string) string {
	pool, _, _ := strings.Cut(caName, "/certificateAuthorities/")
	return pool
}
