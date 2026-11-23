package sqladmin

import (
	"crypto/tls"
	"database/sql"
	"time"

	"github.com/go-sql-driver/mysql"
)

// openSourceMySQL opens the external MySQL source at addr as user, the way
// the verifications and demotion's GTID check reach it: with TLS when the
// server offers it, unverified (the source representation instance names
// no CA), and in the clear when it offers none (MySQL 5.6 here makes no
// certificate), as the driver's "preferred" does.
//
// The TLS configuration also offers the RSA key exchange suites, which Go
// leaves out of its defaults since 1.22: MySQL 5.7.44's server (the
// mysql:5.7 image's, on Oracle Linux 7's OpenSSL 1.0.2) negotiates no
// ECDHE suite, and without them it refused the handshake ("remote error:
// tls: handshake failure", main run 45047408927's TestCloudSQLMySQL57),
// where a Cloud SQL replica, whose OpenSSL offers them, connects.
func openSourceMySQL(user, password, addr string) (*sql.DB, error) {
	cfg := mysql.NewConfig()
	cfg.User, cfg.Passwd, cfg.Net, cfg.Addr = user, password, "tcp", addr
	cfg.Timeout = 10 * time.Second
	cfg.AllowNativePasswords = true
	cfg.TLS = sourceTLSConfig()
	cfg.AllowFallbackToPlaintext = true
	conn, err := mysql.NewConnector(cfg)
	if err != nil {
		return nil, err
	}
	return sql.OpenDB(conn), nil
}

// sourceTLSConfig is the TLS configuration of a connection to an external
// MySQL source: Go's default suites, then the RSA key exchange ones.
func sourceTLSConfig() *tls.Config {
	var suites []uint16
	for _, s := range tls.CipherSuites() {
		suites = append(suites, s.ID)
	}
	suites = append(suites,
		tls.TLS_RSA_WITH_AES_128_GCM_SHA256, tls.TLS_RSA_WITH_AES_256_GCM_SHA384,
		tls.TLS_RSA_WITH_AES_128_CBC_SHA, tls.TLS_RSA_WITH_AES_256_CBC_SHA)
	return &tls.Config{InsecureSkipVerify: true, CipherSuites: suites} //nolint:gosec // the source names no CA to verify against
}
