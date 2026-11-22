package redisadmin

import (
	"bufio"
	"net"
	"strings"
	"testing"

	memorystorepb "cloud.google.com/go/memorystore/apiv1/memorystorepb"
)

func TestReadCommandCountsBytes(t *testing.T) {
	stream := "\n" + string(respCommand([]string{"SET", "k", "v\r\nx"})) + "PING\r\n"
	br := bufio.NewReader(strings.NewReader(stream))
	var total int64
	var got [][]string
	for {
		args, n, err := readCommand(br)
		if err != nil {
			break
		}
		total += n
		if len(args) > 0 {
			got = append(got, args)
		}
	}
	if total != int64(len(stream)) {
		t.Errorf("bytes counted = %d, want %d", total, len(stream))
	}
	if len(got) != 2 || got[0][2] != "v\r\nx" || got[1][0] != "PING" {
		t.Errorf("commands = %q", got)
	}
}

func TestFullSyncSkipsThePayload(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	go func() {
		br := bufio.NewReader(b)
		for i := 0; i < 2; i++ {
			if _, _, err := readCommand(br); err != nil {
				return
			}
			if i == 0 {
				_, _ = b.Write([]byte("+OK\r\n"))
			}
		}
		_, _ = b.Write([]byte("+FULLRESYNC abc 1234\r\n\n\n$5\r\nREDIS*1\r\n$4\r\nPING\r\n"))
	}()
	br := bufio.NewReader(a)
	off, err := fullSync(a, br)
	if err != nil || off != 1234 {
		t.Fatalf("fullSync = %d, %v; want 1234", off, err)
	}
	args, _, err := readCommand(br)
	if err != nil || len(args) != 1 || args[0] != "PING" {
		t.Errorf("first command after the payload = %q, %v", args, err)
	}
}

func TestSecondaryUsersRefuseWrites(t *testing.T) {
	c := &clusters{valkey: true}
	r := &clusterRecord{Name: "s", Valkey: &valkeyFields{Mode: "CLUSTER", LinkPassword: "pw",
		CIR: &cirConfig{Role: memorystorepb.CrossInstanceReplicationConfig_SECONDARY.String()}}}
	if !r.crrSecondary() {
		t.Fatal("not a secondary")
	}
	users := c.aclUsers(r)
	def := users["default"]
	if len(def) == 0 || def[len(def)-1] != "-@write" {
		t.Errorf("default user = %v, want writes refused", def)
	}
	if link := users[crrUser]; len(link) == 0 || link[len(link)-1] != "+@all" {
		t.Errorf("link user = %v", link)
	}
	r.Valkey.CIR = nil
	users = c.aclUsers(r)
	if def := users["default"]; def[len(def)-1] != "+@all" {
		t.Errorf("default user of a detached instance = %v, want +@all", def)
	}
	if _, ok := users[crrUser]; ok {
		t.Error("a detached instance keeps the link's user")
	}
}
