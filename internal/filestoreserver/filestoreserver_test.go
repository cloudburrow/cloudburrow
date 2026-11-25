package filestoreserver

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/service/filestore"
)

func TestConfigServesTheSharesAsFilestoreClientsMountThem(t *testing.T) {
	conf := Config(filestore.Spec{Shares: []filestore.Share{{Name: "vol1"}}})
	for _, want := range []string{"Protocols = 3;", "Mount_Path_Pseudo = true;", "Enable_UDP = false;",
		"Path = /export/fs/vol1/data;", "Pseudo = /vol1;", "Access_Type = RW;", "Squash = No_Root_Squash;"} {
		if !strings.Contains(conf, want) {
			t.Errorf("config lacks %q:\n%s", want, conf)
		}
	}
	conf = Config(filestore.Spec{NFSv41: true, Shares: []filestore.Share{{Name: "data", Exports: []filestore.Export{
		{IPRanges: []string{"10.0.0.0/24", "172.18.0.5"}, ReadOnly: true, RootSquash: true, AnonUID: 1000, AnonGID: 1001}}}}})
	for _, want := range []string{"Protocols = 3, 4;", "Access_Type = None;", "Clients = 10.0.0.0/24, 172.18.0.5;",
		"Access_Type = RO;", "Squash = Root_Squash;", "Anonymous_Uid = 1000;", "Anonymous_Gid = 1001;"} {
		if !strings.Contains(conf, want) {
			t.Errorf("config lacks %q:\n%s", want, conf)
		}
	}
}

func TestPickIPTakesAFreeAddressInTheTopQuarter(t *testing.T) {
	ip, err := pickIP([]string{"fc00::/64", "172.26.0.0/16"}, "fs-abc", map[string]bool{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(ip, "172.26.") {
		t.Fatalf("ip = %s", ip)
	}
	var b3 int
	if _, err := fmtSscan(ip, &b3); err != nil || b3 < 192 {
		t.Fatalf("ip %s is not in the top quarter", ip)
	}
	again, _ := pickIP([]string{"172.26.0.0/16"}, "fs-abc", map[string]bool{ip: true})
	if again == ip || again == "" {
		t.Fatalf("a used address was picked again: %s", again)
	}
	if _, err := pickIP([]string{"fc00::/64"}, "k", nil); err == nil {
		t.Fatal("no IPv4 subnet was accepted")
	}
}

func fmtSscan(ip string, third *int) (int, error) {
	parts := strings.Split(ip, ".")
	n := 0
	for _, c := range parts[2] {
		n = n*10 + int(c-'0')
	}
	*third = n
	return 1, nil
}

func TestNamesAreCheckedBeforeTheyReachAShell(t *testing.T) {
	for _, bad := range []string{"a;b", "../x", "a b", ""} {
		if checkNames(bad, "fs-01") == nil {
			t.Errorf("share %q accepted", bad)
		}
	}
	if checkNames("vol1", "fs-0a", "snap-$(x)") == nil {
		t.Error("a bad key was accepted")
	}
}

func TestRulesSendEachAddressOnToItsServer(t *testing.T) {
	got := Rules(map[string]string{"10.0.0.2": "172.18.200.7", "10.0.1.2": "172.18.200.9"})
	want := "*nat\n:CLOUDBURROW-FILESTORE - [0:0]\n" +
		"-A CLOUDBURROW-FILESTORE -d 10.0.0.2/32 -j DNAT --to-destination 172.18.200.7\n" +
		"-A CLOUDBURROW-FILESTORE -d 10.0.1.2/32 -j DNAT --to-destination 172.18.200.9\nCOMMIT\n"
	if got != want {
		t.Fatalf("Rules = %q, want %q", got, want)
	}
	for c, want := range map[string]bool{"cloudburrow-x": true, "cloudburrow-x-gke-0a1b2c3d": true, "cloudburrow-x-gke-zz": false, "cloudburrow-xy": false, "cloudburrow-y-gke-0a1b2c3d": false} {
		if ownCluster("cloudburrow-x", c) != want {
			t.Errorf("ownCluster(%q) = %v", c, !want)
		}
	}
}

func TestEncryptedBackupsOpenOnlyWithTheirKeyAndWhole(t *testing.T) {
	dek := make([]byte, 32)
	dek[0] = 7
	plain := strings.Repeat("filestore backup ", 9000) // more than two chunks
	var buf strings.Builder
	w, err := newSealingWriter(&buf, dek)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(plain)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	sealed := buf.String()
	if strings.Contains(sealed, "filestore backup") {
		t.Fatal("the backup is in the clear")
	}
	read := func(s string, key []byte) (string, error) {
		r, err := newOpeningReader(strings.NewReader(s), key)
		if err != nil {
			return "", err
		}
		b, err := io.ReadAll(r)
		return string(b), err
	}
	if got, err := read(sealed, dek); err != nil || got != plain {
		t.Fatalf("round trip: %v (%d bytes)", err, len(got))
	}
	other := make([]byte, 32)
	if _, err := read(sealed, other); err == nil {
		t.Fatal("another key opened the backup")
	}
	if _, err := read(sealed[:len(sealed)-chunkSize/2], dek); err == nil {
		t.Fatal("a cut backup opened")
	}
}

// A node killed while a process waits on an NFS server, which an exec can
// no longer join, may still mount the address, so its tombstone stays; a
// node Docker reports stopped or gone mounts nothing.
func TestAStuckNodeStillMountsItsShares(t *testing.T) {
	for msg, want := range map[string]bool{
		"docker exec -i: exit status 127: ":                                          true,
		"OCI runtime exec failed: error executing setns process: exit status 1":      true,
		"Error response from daemon: container 5c4e4d391bae is not running":          false,
		"Error response from daemon: No such container: cloudburrow-ci-gke-1-5044-1": false,
	} {
		if got := stuckNode(errors.New(msg)); got != want {
			t.Errorf("stuckNode(%q) = %v, want %v", msg, got, want)
		}
	}
}
