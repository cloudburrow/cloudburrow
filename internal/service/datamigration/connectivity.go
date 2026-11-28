package datamigration

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"strings"

	dmspb "cloud.google.com/go/clouddms/apiv1/clouddmspb"
	"golang.org/x/crypto/ssh"
	"google.golang.org/protobuf/proto"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// Connectivity to a source (#2257), as Google's REST reference describes
// it (https://docs.cloud.google.com/database-migration/docs/reference/rest/v1/projects.locations.connectionProfiles,
// read 2026-11-25):
//
//   - SslConfig: REQUIRED "Mandates SSL encryption for all connections.
//     This does not require certificate verification."; SERVER_ONLY "Only
//     'caCertificate' specified", the source's certificate verified
//     against it; SERVER_CLIENT "Both server ('caCertificate'), and client
//     ('clientKey', 'clientCertificate') specified", the destination
//     presenting its certificate too ("If this field is used then the
//     'clientKey' field is mandatory"); NONE "Connection is not encrypted".
//     The destinations' servers verify the CA (sslmode=verify-ca,
//     --ssl-mode=VERIFY_CA, the replication channel's SOURCE_SSL_CA), not
//     the host name (UNVERIFIED: Google's guides say the instance
//     "authenticates the source", naming no host name check for MySQL or
//     PostgreSQL; docs re-read 2026-11-25, still unanswered:
//     https://docs.cloud.google.com/database-migration/docs/mysql/create-source-connection-profile).
//   - forwardSshConnectivity (hostname, username, port "default value is
//     22", password or privateKey, input only): the destinations' servers
//     reach the source's host and port through an SSH tunnel to the host,
//     which CloudBurrow runs as a pod of the instance's cluster
//     (internal/dmstunnel).
//   - A migration job's reverseSshConnectivity (vmIp, vmPort, vm, vpc):
//     the destination connects to vmIp:vmPort, the forwarding port of the
//     bastion the source's side tunnels to (generateSshScript writes the
//     script that sets it up).

// overlayJSON is m's JSON, the overlay's fields included.
func overlayJSON(m proto.Message) map[string]any {
	b, err := overlay.Marshal(m)
	if err != nil {
		return nil
	}
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return out
}

// editJSON changes m through its JSON, the overlay's fields included.
func editJSON(m proto.Message, f func(map[string]any)) error {
	j := overlayJSON(m)
	if j == nil {
		return fmt.Errorf("datamigration: %T has no JSON form", m)
	}
	f(j)
	b, err := json.Marshal(j)
	if err != nil {
		return err
	}
	proto.Reset(m)
	return overlay.Unmarshal(b, m)
}

// decodeField decodes m's JSON field key into out, reporting whether m
// sets it.
func decodeField(m proto.Message, key string, out any) bool {
	j := overlayJSON(m)
	v, ok := j[key]
	if !ok {
		return false
	}
	b, _ := json.Marshal(v)
	return json.Unmarshal(b, out) == nil
}

// forwardSSH is the JSON of forwardSshConnectivity.
type forwardSSH struct {
	Hostname   string `json:"hostname"`
	Username   string `json:"username"`
	Port       int    `json:"port"`
	Password   string `json:"password"`
	PrivateKey string `json:"privateKey"`
}

// sshOf is a mysql or postgresql profile's forward SSH tunnel, nil for
// none.
func sshOf(m proto.Message) *SSHTunnel {
	var f forwardSSH
	if !decodeField(m, "forwardSshConnectivity", &f) {
		return nil
	}
	port := f.Port
	if port == 0 {
		port = 22
	}
	return &SSHTunnel{Host: f.Hostname, Port: port, User: f.Username, Password: f.Password, PrivateKey: f.PrivateKey}
}

// checkForwardSSH validates a profile's forwardSshConnectivity.
func checkForwardSSH(kind string, m proto.Message, update bool) error {
	t := sshOf(m)
	if t == nil {
		return nil
	}
	field := "connection_profile." + kind + ".forward_ssh_connectivity"
	switch {
	case t.Host == "":
		return apierror.InvalidArgument("%s.hostname is required", field)
	case t.User == "":
		return apierror.InvalidArgument("%s.username is required", field)
	case t.Port <= 0 || t.Port > 65535:
		return apierror.InvalidArgument("%s.port %d must be between 1 and 65535", field, t.Port)
	case t.Password != "" && t.PrivateKey != "":
		return apierror.InvalidArgument("%s: only one of password and private_key may be set", field)
	case t.Password == "" && t.PrivateKey == "" && !update:
		return apierror.InvalidArgument("%s: password or private_key is required", field)
	}
	if t.PrivateKey != "" {
		if _, err := ssh.ParsePrivateKey([]byte(t.PrivateKey)); err != nil {
			return apierror.InvalidArgument("%s.private_key is not an unencrypted PEM private key: %v", field, err)
		}
	}
	if has(m, "private_connectivity") || has(m, "private_service_connect_connectivity") || has(m, "static_service_ip_connectivity") {
		return apierror.InvalidArgument("connection_profile.%s: forward_ssh_connectivity cannot be set with another connectivity", kind)
	}
	return nil
}

// redactSSH clears forwardSshConnectivity's input-only password and
// private key.
func redactSSH(m proto.Message) {
	if sshOf(m) == nil {
		return
	}
	_ = editJSON(m, func(j map[string]any) {
		if f, ok := j["forwardSshConnectivity"].(map[string]any); ok {
			delete(f, "password")
			delete(f, "privateKey")
		}
	})
}

// keepSSHSecret carries cur's SSH password or private key into next when
// an update does not send one.
func keepSSHSecret(cur, next proto.Message) {
	c, n := sshOf(cur), sshOf(next)
	if c == nil || n == nil || n.Password != "" || n.PrivateKey != "" {
		return
	}
	_ = editJSON(next, func(j map[string]any) {
		f, _ := j["forwardSshConnectivity"].(map[string]any)
		if f == nil {
			return
		}
		if c.Password != "" {
			f["password"] = c.Password
		} else if c.PrivateKey != "" {
			f["privateKey"] = c.PrivateKey
		}
	})
}

// checkCertificates validates a SERVER_ONLY or SERVER_CLIENT SslConfig's
// PEM: a CA certificate the source's is verified against, and a client
// certificate with its key.
func checkCertificates(kind string, ssl *dmspb.SslConfig) error {
	field := "connection_profile." + kind + ".ssl"
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(ssl.GetCaCertificate())) {
		return apierror.InvalidArgument("%s.ca_certificate is not a PEM-encoded x509 certificate", field)
	}
	if ssl.GetClientCertificate() == "" && ssl.GetClientKey() == "" {
		return nil
	}
	if _, err := tls.X509KeyPair([]byte(ssl.GetClientCertificate()), []byte(ssl.GetClientKey())); err != nil {
		return apierror.InvalidArgument("%s.client_certificate and client_key are not a PEM certificate and its unencrypted private key: %v", field, err)
	}
	return nil
}

// sslType is an SslConfig's type, inferred from its certificates when it
// names none, as the reference's "according to 'clientKey',
// 'clientCertificate' and 'caCertificate'" describes.
func sslType(c *dmspb.SslConfig) int32 {
	if t := int32(c.GetType()); t != 0 {
		return t
	}
	switch {
	case c.GetClientCertificate() != "":
		return int32(dmspb.SslConfig_SERVER_CLIENT)
	case c.GetCaCertificate() != "":
		return int32(dmspb.SslConfig_SERVER_ONLY)
	}
	return 0
}

// isPEM reports whether s holds a PEM block.
func isPEM(s string) bool {
	b, _ := pem.Decode([]byte(s))
	return b != nil
}

// reverseSSH is a job's reverseSshConnectivity, nil for none.
func reverseSSH(j *dmspb.MigrationJob) *dmspb.ReverseSshConnectivity {
	if c, ok := j.GetConnectivity().(*dmspb.MigrationJob_ReverseSshConnectivity); ok {
		return c.ReverseSshConnectivity
	}
	return nil
}

func checkReverseSSH(r *dmspb.ReverseSshConnectivity) error {
	switch {
	case r.GetVmIp() == "":
		return apierror.InvalidArgument("migration_job.reverse_ssh_connectivity.vm_ip is required")
	case r.GetVmPort() <= 0 || r.GetVmPort() > 65535:
		return apierror.InvalidArgument("migration_job.reverse_ssh_connectivity.vm_port %d must be between 1 and 65535", r.GetVmPort())
	case strings.ContainsAny(r.GetVmIp(), " /'\""):
		return apierror.InvalidArgument("migration_job.reverse_ssh_connectivity.vm_ip %q is not an address", r.GetVmIp())
	}
	return nil
}
