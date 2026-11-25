package remotecompute

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestStandInMoveRoundTripKeepsData carries an instance's containers from
// this machine's Docker to the stand-in host and back: a container on its
// own network at a fixed address, with a labelled volume, an anonymous
// volume and a bind mount of a path here, and a published port. Each leg
// stops the source, carries, starts the target, checks the data and the
// address there, and removes the source. A failed leg rolls back. The image
// (CLOUDBURROW_TEST_REMOTE_IMAGE, busybox) must be on both Dockers.
func TestStandInMoveRoundTripKeepsData(t *testing.T) {
	tg, sshConfig := standIn(t)
	image := os.Getenv(envRemoteImage)
	if image == "" {
		t.Skipf("%s names no image on the host", envRemoteImage)
	}
	if exec.Command("docker", "image", "inspect", image).Run() != nil {
		t.Skipf("%s is not on this machine's Docker; the test does not pull it", image)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	s, err := Connect(ctx, Options{Target: tg, StateDir: t.TempDir(), Instance: "standin-move", SSHConfig: sshConfig})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	local, remote := DockerEndpoint{Helper: image}, DockerEndpoint{Host: s.DockerHost(), Helper: image}
	instance := fmt.Sprintf("mvtest-%d", time.Now().UnixNano()%1e6)
	net, ctr, vol := instance+"-net", instance+"-app", instance+"-data"
	on := func(side DockerEndpoint, args ...string) (string, error) {
		c := &Carry{}
		return c.run(ctx, side, args...)
	}
	must := func(side DockerEndpoint, args ...string) string {
		t.Helper()
		out, err := on(side, args...)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	cleanup := func(side DockerEndpoint) {
		bg := context.Background()
		c := &Carry{}
		_, _ = c.run(bg, side, "rm", "-f", "-v", ctr)
		_, _ = c.run(bg, side, "volume", "rm", "-f", vol)
		out, _ := c.run(bg, side, "volume", "ls", "-q", "--filter", "label=io.cloudburrow.instance="+instance)
		for _, v := range strings.Fields(out) {
			_, _ = c.run(bg, side, "volume", "rm", "-f", v)
		}
		_, _ = c.run(bg, side, "network", "rm", net)
		out, _ = c.run(bg, side, "image", "ls", "-q", "--no-trunc", "cloudburrow.local/move-"+instance+"-"+ctr)
		for _, id := range strings.Fields(out) {
			_, _ = c.run(bg, side, "rmi", "-f", id)
		}
	}
	defer cleanup(local)
	defer cleanup(remote)

	// The instance on this machine.
	shared := t.TempDir()
	if err := os.WriteFile(filepath.Join(shared, "model.bin"), []byte("weights from this machine"), 0o644); err != nil {
		t.Fatal(err)
	}
	must(local, "network", "create", "--subnet", "10.231.77.0/24", net)
	must(local, "volume", "create", "--label", "io.cloudburrow.instance="+instance, vol)
	must(local, "run", "-d", "--name", ctr, "--label", "io.cloudburrow.instance="+instance, "--hostname", "app-node",
		"--network", net, "--ip", "10.231.77.9", "-v", vol+":/data", "--volume", "/scratch", "-v", shared+":/model:ro",
		"-p", "127.0.0.1::8080", image, "sh", "-c", "[ -f /data/state ] || echo kept > /data/state; [ -f /scratch/x ] || echo anon > /scratch/x; exec httpd -f -p 8080 -h /data")
	time.Sleep(time.Second)

	check := func(side DockerEndpoint, where string) {
		t.Helper()
		out := must(side, "exec", ctr, "sh", "-c", "cat /data/state /scratch/x /model/model.bin; hostname; ip -4 -o addr show eth0")
		for _, want := range []string{"kept", "anon", "weights from this machine", "app-node", "10.231.77.9/24"} {
			if !strings.Contains(out, want) {
				t.Fatalf("on %s the moved container lacks %q:\n%s", where, want, out)
			}
		}
		if p := must(side, "port", ctr, "8080/tcp"); !strings.HasPrefix(p, "127.0.0.1:") {
			t.Fatalf("on %s the port is published on %q", where, p)
		}
	}
	leg := func(from, to DockerEndpoint, where string) {
		t.Helper()
		var steps []string
		c, err := StartCarry(ctx, CarryOptions{Instance: instance, From: from, To: to,
			Progress: func(step, msg string) { steps = append(steps, step); t.Logf("%s: %s", step, msg) }})
		if err != nil {
			t.Fatal(err)
		}
		if err := c.StartTarget(ctx); err != nil {
			c.Rollback(ctx)
			t.Fatal(err)
		}
		time.Sleep(time.Second)
		check(to, where)
		if err := c.RemoveSource(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := on(from, "container", "inspect", ctr); err == nil {
			t.Fatalf("the source still has %s after the move to %s", ctr, where)
		}
		if !strings.Contains(strings.Join(steps, " "), "stop") || c.Bytes() == 0 {
			t.Fatalf("steps %v, %d bytes", steps, c.Bytes())
		}
	}
	leg(local, remote, "the stand-in host")
	// The data was written to the volumes; a second write on the host comes
	// back too.
	must(remote, "exec", ctr, "sh", "-c", "echo kept >> /data/state")
	leg(DockerEndpoint{Host: remote.Host, Helper: image}, DockerEndpoint{Helper: image}, "this machine again")
	if out := must(local, "exec", ctr, "cat", "/data/state"); out != "kept\nkept" {
		t.Fatalf("after the round trip /data/state is %q", out)
	}

	// A carry that cannot finish rolls back: the target already holds a
	// volume of that name, so nothing is made there and the source runs.
	must(remote, "volume", "create", vol)
	if _, err := StartCarry(ctx, CarryOptions{Instance: instance, From: DockerEndpoint{Helper: image}, To: remote}); err == nil {
		t.Fatal("a carry onto a volume the target already has was not refused")
	}
	if _, err := on(remote, "container", "inspect", ctr); err == nil {
		t.Fatal("the refused carry left a container on the target")
	}
	if st := must(local, "inspect", "--format", "{{.State.Running}}", ctr); st != "true" {
		t.Fatalf("after the rollback the source container is not running (%s)", st)
	}
}
