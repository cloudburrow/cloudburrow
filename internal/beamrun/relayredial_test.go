package beamrun

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestRelayRedial: the relay (assets/relay.py) dials a target that
// closes a connection before a byte has gone either way again, so that a
// server that speaks first (MySQL, behind a tunnel being re-established)
// greets its client rather than the client being cut off; a client that
// speaks first is relayed as before.
func TestRelayRedial(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Fatalf("python3 is not on PATH: the relay runs in Python: %v", err)
	}
	src, err := assets.ReadFile("assets/relay.py")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	relay := filepath.Join(dir, "relay.py")
	if err := os.WriteFile(relay, src, 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(py, "-I", "-c", relayRedialScript, relay).CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "ok 3" {
		t.Fatalf("relay redial: %v\n%s", err, out)
	}
}

const relayRedialScript = `
# Exercises relay.py's dial: a server-first target that drops the first
# two connections before speaking, then greets; and a client-first one.
import asyncio, subprocess, sys, socket, time, os
relay = sys.argv[1]
drops = {"n": 0}
async def flaky(r, w):
    drops["n"] += 1
    if drops["n"] <= 2:
        w.close(); return
    w.write(b"HELLO"); await w.drain()
    d = await r.read(100)
    w.write(b"echo:" + d); await w.drain(); w.close()
async def echo(r, w):
    d = await r.read(100)
    w.write(b"got:" + d); await w.drain(); w.close()
async def main():
    s1 = await asyncio.start_server(flaky, "127.0.0.1", 0)
    s2 = await asyncio.start_server(echo, "127.0.0.1", 0)
    p1 = s1.sockets[0].getsockname()[1]; p2 = s2.sockets[0].getsockname()[1]
    def free():
        s = socket.socket(); s.bind(("127.0.0.1", 0)); p = s.getsockname()[1]; s.close(); return p
    l1, l2 = free(), free()
    proc = await asyncio.create_subprocess_exec(sys.executable, "-I", relay, "127.0.0.1:%d=127.0.0.1:%d" % (l1, p1), "127.0.0.1:%d=127.0.0.1:%d" % (l2, p2), stdout=asyncio.subprocess.PIPE)
    assert (await proc.stdout.readline()).strip() == b"ready"
    r, w = await asyncio.open_connection("127.0.0.1", l1)
    g = await asyncio.wait_for(r.read(5), 10)
    assert g == b"HELLO", g
    w.write(b"x"); await w.drain()
    assert await asyncio.wait_for(r.read(100), 10) == b"echo:x"
    r, w = await asyncio.open_connection("127.0.0.1", l2)
    w.write(b"hi"); await w.drain()
    assert await asyncio.wait_for(r.read(100), 10) == b"got:hi"
    proc.kill()
    print("ok", drops["n"])
asyncio.run(main())
`
