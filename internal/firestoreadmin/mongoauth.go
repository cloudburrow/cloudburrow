package firestoreadmin

// Firestore with MongoDB compatibility (#1703): authentication, as Google
// documents it ("Authenticate and connect to a database",
// https://docs.cloud.google.com/firestore/mongodb-compatibility/docs/connect):
//
//   - SCRAM-SHA-256 (RFC 7677) with the user credentials the Admin API
//     keeps (usercreds.go): the user is the credentials' ID, the
//     authentication database the database's ID;
//   - PLAIN with the user access_token and an access token as the
//     password, from $external, for diagnostics;
//   - MONGODB-OIDC with an access token (Google's callback returns the
//     application default credentials' access token).
//
// The last two take only a live access token this instance minted, and
// each command then needs its permissions in the principal's roles
// (mongoiam.go, #1738).

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"strconv"
	"strings"

	adminpb "cloud.google.com/go/firestore/apiv1/admin/adminpb"

	"github.com/cloudburrow/cloudburrow/internal/firestoreadmin/mql"
)

// scramIterations is MongoDB's default scramSHA256IterationCount.
const scramIterations = 15000

// scramKeys is a password's SCRAM-SHA-256 verifier.
type scramKeys struct {
	Salt       []byte `json:"salt"`
	Iterations int    `json:"iterations"`
	StoredKey  []byte `json:"storedKey"`
	ServerKey  []byte `json:"serverKey"`
}

func hmacSHA256(key []byte, msg string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(msg))
	return h.Sum(nil)
}

// newScramKeys is the verifier of pw, with a fresh salt. The server's
// passwords are base64url, which SASLprep leaves as they are.
func newScramKeys(pw string) *scramKeys {
	salt := make([]byte, 28)
	_, _ = rand.Read(salt)
	salted, err := pbkdf2.Key(sha256.New, pw, salt, scramIterations, sha256.Size)
	if err != nil {
		return nil
	}
	client := hmacSHA256(salted, "Client Key")
	stored := sha256.Sum256(client)
	return &scramKeys{Salt: salt, Iterations: scramIterations, StoredKey: stored[:], ServerKey: hmacSHA256(salted, "Server Key")}
}

func authFailed() *mql.Error {
	return mql.Errorf(18, "AuthenticationFailed", "Authentication failed.")
}

// mongoDB is the database a connection authenticated to.
type mongoDB struct {
	name string // projects/{p}/databases/{d}
	id   string // {d}
}

// mongoCreds finds the enabled user credentials user of the Enterprise
// edition database whose ID is dbID; when dbID is admin, of any such
// database that has them, if only one does.
func (a *Admin) mongoCreds(dbID, user string) (*mongoDB, *scramKeys, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	var found []string
	for name := range a.userCreds {
		m := userCredsNameRe.FindStringSubmatch(name)
		if m == nil || m[4] != user || (m[3] != dbID && dbID != "admin") {
			continue
		}
		found = append(found, name)
	}
	if len(found) != 1 {
		return nil, nil, authFailed()
	}
	c := a.userCreds[found[0]]
	m := userCredsNameRe.FindStringSubmatch(found[0])
	if _, err := a.enterprise(m[1]); err != nil || c.UserCreds.GetState() != adminpb.UserCreds_ENABLED || c.Scram == nil {
		return nil, nil, authFailed()
	}
	return &mongoDB{name: m[1], id: m[3]}, c.Scram, nil
}

// mongoDatabase is the Enterprise edition database dbID that a token
// authenticates to: of the project only one such database of that ID is in.
func (a *Admin) mongoDatabase(dbID string) (*mongoDB, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	var found []string
	for name, db := range a.databases {
		m := dbNameRe.FindStringSubmatch(name)
		if m == nil || db.DeleteTime != nil || db.GetDatabaseEdition() != adminpb.Database_ENTERPRISE {
			continue
		}
		if strings.HasSuffix(name, "/databases/"+dbID) {
			found = append(found, name)
		}
	}
	if len(found) != 1 {
		return nil, authFailed()
	}
	return &mongoDB{name: found[0], id: dbID}, nil
}

// scramConv is a SCRAM-SHA-256 conversation in progress.
type scramConv struct {
	db          *mongoDB
	user        string
	keys        *scramKeys
	clientFirst string // client-first-message-bare
	serverFirst string
	nonce       string
}

func parseScramAttrs(msg string) map[string]string {
	out := map[string]string{}
	for _, p := range strings.Split(msg, ",") {
		if len(p) >= 2 && p[1] == '=' {
			out[p[:1]] = p[2:]
		}
	}
	return out
}

// scramStart answers a client-first message.
func (a *Admin) scramStart(authDB string, payload []byte) (*scramConv, []byte, error) {
	msg := string(payload)
	if !strings.HasPrefix(msg, "n,") {
		return nil, nil, mql.BadValue("SCRAM channel binding is not supported.")
	}
	i := strings.Index(msg[2:], ",")
	if i < 0 {
		return nil, nil, authFailed()
	}
	bare := msg[2+i+1:]
	attrs := parseScramAttrs(bare)
	user := strings.NewReplacer("=2C", ",", "=3D", "=").Replace(attrs["n"])
	if attrs["r"] == "" || user == "" {
		return nil, nil, authFailed()
	}
	db, keys, err := a.mongoCreds(authDB, user)
	if err != nil {
		return nil, nil, err
	}
	nb := make([]byte, 24)
	_, _ = rand.Read(nb)
	nonce := attrs["r"] + base64.StdEncoding.EncodeToString(nb)
	first := "r=" + nonce + ",s=" + base64.StdEncoding.EncodeToString(keys.Salt) + ",i=" + strconv.Itoa(keys.Iterations)
	return &scramConv{db: db, user: user, keys: keys, clientFirst: bare, serverFirst: first, nonce: nonce}, []byte(first), nil
}

// scramFinish checks a client-final message's proof and answers with the
// server's signature.
func (c *scramConv) finish(payload []byte) ([]byte, error) {
	msg := string(payload)
	i := strings.LastIndex(msg, ",p=")
	if i < 0 {
		return nil, authFailed()
	}
	withoutProof := msg[:i]
	attrs := parseScramAttrs(withoutProof)
	if attrs["r"] != c.nonce {
		return nil, authFailed()
	}
	proof, err := base64.StdEncoding.DecodeString(msg[i+3:])
	if err != nil || len(proof) != sha256.Size {
		return nil, authFailed()
	}
	authMsg := c.clientFirst + "," + c.serverFirst + "," + withoutProof
	sig := hmacSHA256(c.keys.StoredKey, authMsg)
	clientKey := make([]byte, len(proof))
	for k := range proof {
		clientKey[k] = proof[k] ^ sig[k]
	}
	stored := sha256.Sum256(clientKey)
	if subtle.ConstantTimeCompare(stored[:], c.keys.StoredKey) != 1 {
		return nil, authFailed()
	}
	return []byte("v=" + base64.StdEncoding.EncodeToString(hmacSHA256(c.keys.ServerKey, authMsg))), nil
}
