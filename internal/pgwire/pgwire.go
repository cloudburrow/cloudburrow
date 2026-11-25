// Package pgwire is the little of PostgreSQL's wire protocol a proxy needs
// to sign a client in to a server on its behalf (#2208): reading the
// client's startup message and its password, and answering the server's
// authentication request (cleartext, MD5 or SCRAM-SHA-256) with a role's
// password. AlloyDB's connectors' server-side proxy uses it for an IAM
// connection, whose token it has checked, as Google's proxy signs one in.
package pgwire

import (
	"bufio"
	"crypto/hmac"
	"crypto/md5" //nolint:gosec // PostgreSQL's md5 method is MD5 by definition.
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// CancelRequest is the code of a startup message that cancels a query.
const CancelRequest = 80877102

// ReadStartup reads an untyped startup message, its length included.
func ReadStartup(r *bufio.Reader) ([]byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := int(binary.BigEndian.Uint32(hdr[:]))
	if n < 8 || n > 1<<16 {
		return nil, errors.New("bad startup message length")
	}
	msg := make([]byte, n)
	copy(msg, hdr[:])
	if _, err := io.ReadFull(r, msg[4:]); err != nil {
		return nil, err
	}
	return msg, nil
}

// Code is a startup message's protocol version or request code.
func Code(startup []byte) uint32 { return binary.BigEndian.Uint32(startup[4:8]) }

// ReadMsg reads one typed message: its type and body.
func ReadMsg(r *bufio.Reader) (byte, []byte, error) {
	t, err := r.ReadByte()
	if err != nil {
		return 0, nil, err
	}
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := int(binary.BigEndian.Uint32(hdr[:]))
	if n < 4 || n > 1<<24 {
		return 0, nil, errors.New("bad message length")
	}
	body := make([]byte, n-4)
	_, err = io.ReadFull(r, body)
	return t, body, err
}

// Msg is a typed message.
func Msg(t byte, body []byte) []byte {
	out := make([]byte, 5, 5+len(body))
	out[0] = t
	binary.BigEndian.PutUint32(out[1:], uint32(4+len(body)))
	return append(out, body...)
}

// Auth is an authentication request of code.
func Auth(code uint32, extra []byte) []byte {
	return Msg('R', append(binary.BigEndian.AppendUint32(nil, code), extra...))
}

// Fatal is a FATAL ErrorResponse, as the server sends a refused sign-in.
func Fatal(code, msg string) []byte {
	var b []byte
	for _, f := range [][2]string{{"S", "FATAL"}, {"V", "FATAL"}, {"C", code}, {"M", msg}} {
		b = append(b, f[0][0])
		b = append(append(b, f[1]...), 0)
	}
	return Msg('E', append(b, 0))
}

// StartupParams are a startup message's parameters.
func StartupParams(msg []byte) map[string]string {
	out := map[string]string{}
	parts := strings.Split(string(msg[8:]), "\x00")
	for i := 0; i+1 < len(parts); i += 2 {
		if parts[i] == "" {
			break
		}
		out[parts[i]] = parts[i+1]
	}
	return out
}

// ReadPassword asks the client for its password in cleartext, as a server
// does for the password method, and returns it.
func ReadPassword(client io.Writer, r *bufio.Reader) (string, error) {
	if _, err := client.Write(Auth(3, nil)); err != nil {
		return "", err
	}
	t, body, err := ReadMsg(r)
	if err != nil {
		return "", err
	}
	if t != 'p' {
		return "", errors.New("expected a password message")
	}
	return strings.TrimRight(string(body), "\x00"), nil
}

// SignIn sends startup to the server (up, read through ur) and answers its
// authentication requests as role with password. Every message up to and
// including AuthenticationOk is passed to client after the sign-in; a
// refusal (an ErrorResponse) is passed to client and returned as an error.
func SignIn(up io.Writer, ur *bufio.Reader, startup []byte, role, password string, client io.Writer) error {
	if _, err := up.Write(startup); err != nil {
		return err
	}
	var scram *scramClient
	for {
		t, body, err := ReadMsg(ur)
		if err != nil {
			return err
		}
		if t == 'E' {
			_, _ = client.Write(Msg(t, body))
			return errors.New("the server refused the sign-in")
		}
		if t != 'R' || len(body) < 4 {
			if _, err := client.Write(Msg(t, body)); err != nil {
				return err
			}
			continue
		}
		var reply []byte
		switch code := binary.BigEndian.Uint32(body[:4]); code {
		case 0: // AuthenticationOk
			_, err := client.Write(Msg(t, body))
			return err
		case 3: // cleartext
			reply = Msg('p', append([]byte(password), 0))
		case 5: // md5
			if len(body) < 8 {
				return errors.New("bad md5 request")
			}
			reply = Msg('p', append([]byte(md5Password(role, password, body[4:8])), 0))
		case 10: // SASL
			if !strings.Contains(string(body[4:]), "SCRAM-SHA-256\x00") {
				return errors.New("the server offers no SCRAM-SHA-256")
			}
			scram = newScram(password)
			first := scram.first()
			b := append([]byte("SCRAM-SHA-256\x00"), binary.BigEndian.AppendUint32(nil, uint32(len(first)))...)
			reply = Msg('p', append(b, first...))
		case 11: // SASLContinue
			if scram == nil {
				return errors.New("SASLContinue before SASL")
			}
			final, err := scram.final(string(body[4:]))
			if err != nil {
				return err
			}
			reply = Msg('p', []byte(final))
		case 12: // SASLFinal
			continue
		default:
			return fmt.Errorf("authentication method %d is not served", code)
		}
		if _, err := up.Write(reply); err != nil {
			return err
		}
	}
}

func md5Password(user, password string, salt []byte) string {
	inner := md5hex([]byte(password + user))
	return "md5" + md5hex(append([]byte(inner), salt...))
}

func md5hex(b []byte) string {
	sum := md5.Sum(b) //nolint:gosec
	return hex.EncodeToString(sum[:])
}

// scramClient is SCRAM-SHA-256's client side (RFC 5802, RFC 7677), as
// PostgreSQL's SASL authentication takes it.
type scramClient struct {
	password, cnonce, firstBare string
}

func newScram(password string) *scramClient {
	b := make([]byte, 18)
	_, _ = rand.Read(b)
	return &scramClient{password: password, cnonce: base64.RawStdEncoding.EncodeToString(b)}
}

func (c *scramClient) first() string {
	c.firstBare = "n=,r=" + c.cnonce
	return "n,," + c.firstBare
}

func (c *scramClient) final(serverFirst string) (string, error) {
	var nonce, salt string
	iter := 0
	for _, kv := range strings.Split(serverFirst, ",") {
		switch {
		case strings.HasPrefix(kv, "r="):
			nonce = kv[2:]
		case strings.HasPrefix(kv, "s="):
			salt = kv[2:]
		case strings.HasPrefix(kv, "i="):
			iter, _ = strconv.Atoi(kv[2:])
		}
	}
	if !strings.HasPrefix(nonce, c.cnonce) || iter <= 0 {
		return "", errors.New("bad SCRAM server-first message")
	}
	saltB, err := base64.StdEncoding.DecodeString(salt)
	if err != nil {
		return "", err
	}
	salted, err := pbkdf2.Key(sha256.New, c.password, saltB, iter, 32)
	if err != nil {
		return "", err
	}
	clientKey := hmacSHA256(salted, "Client Key")
	stored := sha256.Sum256(clientKey)
	withoutProof := "c=biws,r=" + nonce
	authMsg := c.firstBare + "," + serverFirst + "," + withoutProof
	sig := hmacSHA256(stored[:], authMsg)
	proof := make([]byte, len(clientKey))
	for i := range clientKey {
		proof[i] = clientKey[i] ^ sig[i]
	}
	return withoutProof + ",p=" + base64.StdEncoding.EncodeToString(proof), nil
}

func hmacSHA256(key []byte, msg string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(msg))
	return m.Sum(nil)
}
