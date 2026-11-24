package pgwire

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

// TestSCRAMClientFinal checks the client proof against RFC 7677's
// example exchange (user "user", password "pencil").
func TestSCRAMClientFinal(t *testing.T) {
	c := &scramClient{password: "pencil", cnonce: "rOprNGfwEbeRWgbNEkqO", firstBare: "n=user,r=rOprNGfwEbeRWgbNEkqO"}
	got, err := c.final("r=rOprNGfwEbeRWgbNEkqO%hvYDpWUa2RaTCAfuxFIlj)hNlF$k0,s=W22ZaJ0SNY7soEsUEjb6gQ==,i=4096")
	if err != nil {
		t.Fatal(err)
	}
	want := "c=biws,r=rOprNGfwEbeRWgbNEkqO%hvYDpWUa2RaTCAfuxFIlj)hNlF$k0,p=dHzbZapWIk4jUhN+Ute9ytag9zjfMHgsqmmiz7AndVQ="
	if got != want {
		t.Errorf("client-final\n got %s\nwant %s", got, want)
	}
	if _, err := c.final("r=someone-elses-nonce,s=W22ZaJ0SNY7soEsUEjb6gQ==,i=4096"); err == nil {
		t.Error("a server nonce that does not extend the client's was accepted")
	}
}

// TestMD5Password checks PostgreSQL's md5 answer: "md5" + md5(md5(password
// + user) + salt), as libpq computes it.
func TestMD5Password(t *testing.T) {
	salt := []byte{1, 2, 3, 4}
	got := md5Password("user", "pencil", salt)
	inner := md5hex([]byte("penciluser"))
	if want := "md5" + md5hex(append([]byte(inner), salt...)); got != want || len(got) != 35 {
		t.Errorf("md5Password = %s, want %s", got, want)
	}
}

// TestSignInPassesTheServersAnswers: a cleartext request is answered with
// the password, AuthenticationOk ends the sign-in and is passed on, and an
// ErrorResponse is passed to the client and returned.
func TestSignInPassesTheServersAnswers(t *testing.T) {
	startup := []byte{0, 0, 0, 8, 0, 3, 0, 0}
	server := append(Auth(3, nil), Auth(0, nil)...)
	var up, client bytes.Buffer
	if err := SignIn(&up, bufio.NewReader(bytes.NewReader(server)), startup, "r", "secret", &client); err != nil {
		t.Fatal(err)
	}
	sent := up.Bytes()[len(startup):]
	if sent[0] != 'p' || !strings.Contains(string(sent), "secret\x00") {
		t.Errorf("the password message sent: %q", sent)
	}
	if !bytes.Equal(client.Bytes(), Auth(0, nil)) {
		t.Errorf("the client got %q, want AuthenticationOk", client.Bytes())
	}

	refusal := Fatal("28P01", `password authentication failed for user "r"`)
	up.Reset()
	client.Reset()
	if err := SignIn(&up, bufio.NewReader(bytes.NewReader(append(Auth(3, nil), refusal...))), startup, "r", "bad", &client); err == nil {
		t.Error("a refusal signed in")
	}
	if !bytes.Equal(client.Bytes(), refusal) {
		t.Errorf("the client got %q, want the server's refusal", client.Bytes())
	}
}

func TestStartupParams(t *testing.T) {
	body := binary.BigEndian.AppendUint32(nil, 196608)
	body = append(body, "user\x00alice\x00database\x00db\x00\x00"...)
	msg := append(binary.BigEndian.AppendUint32(nil, uint32(4+len(body))), body...)
	r := bufio.NewReader(bytes.NewReader(msg))
	got, err := ReadStartup(r)
	if err != nil {
		t.Fatal(err)
	}
	if Code(got) != 196608 {
		t.Errorf("code %d", Code(got))
	}
	p := StartupParams(got)
	if p["user"] != "alice" || p["database"] != "db" || len(p) != 2 {
		t.Errorf("params %v", p)
	}
}
