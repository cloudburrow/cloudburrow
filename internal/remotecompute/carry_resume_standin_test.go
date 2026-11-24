package remotecompute

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// envRemoteHost2 names a second stand-in host, for a move from one VM to
// another (in the walk-through, an AWS VM to an Azure one).
const envRemoteHost2 = "CLOUDBURROW_TEST_REMOTE_HOST_2"

// carryFixture is an instance of one container, on its own network at a
// fixed address, with a labelled volume holding data.
type carryFixture struct {
	t        *testing.T
	ctx      context.Context
	image    string
	instance string
	net, ctr string
	vol      string
}

func newCarryFixture(t *testing.T, ctx context.Context) *carryFixture {
	image := os.Getenv(envRemoteImage)
	if image == "" {
		t.Skipf("%s names no image on the host", envRemoteImage)
	}
	if exec.Command("docker", "image", "inspect", image).Run() != nil {
		t.Skipf("%s is not on this machine's Docker; the test does not pull it", image)
	}
	instance := fmt.Sprintf("mvres-%d", time.Now().UnixNano()%1e6)
	return &carryFixture{t: t, ctx: ctx, image: image, instance: instance, net: instance + "-net", ctr: instance + "-app", vol: instance + "-data"}
}

func (f *carryFixture) must(side DockerEndpoint, args ...string) string {
	f.t.Helper()
	c := &Carry{}
	out, err := c.run(f.ctx, side, args...)
	if err != nil {
		f.t.Fatal(err)
	}
	return out
}

func (f *carryFixture) cleanup(side DockerEndpoint) {
	bg := context.Background()
	c := &Carry{}
	_, _ = c.run(bg, side, "rm", "-f", "-v", f.ctr)
	_, _ = c.run(bg, side, "volume", "rm", "-f", f.vol)
	_, _ = c.run(bg, side, "network", "rm", f.net)
	out, _ := c.run(bg, side, "image", "ls", "-q", "--no-trunc", "cloudburrow.local/move-"+f.instance+"-"+f.ctr)
	for _, id := range strings.Fields(out) {
		_, _ = c.run(bg, side, "rmi", "-f", id)
	}
}

func (f *carryFixture) start(side DockerEndpoint) {
	f.must(side, "network", "create", "--subnet", "10.231.78.0/24", f.net)
	f.must(side, "volume", "create", "--label", "io.cloudburrow.instance="+f.instance, f.vol)
	// 4 MiB of data, so the copy is long enough to be cut off half way.
	f.must(side, "run", "-d", "--name", f.ctr, "--label", "io.cloudburrow.instance="+f.instance, "--network", f.net, "--ip", "10.231.78.9",
		"-v", f.vol+":/data", f.image, "sh", "-c",
		"[ -f /data/state ] || { echo kept > /data/state; head -c 4194304 /dev/urandom > /data/blob; md5sum /data/blob > /data/blob.md5; }; exec sleep 100000")
	time.Sleep(time.Second)
}

func (f *carryFixture) check(side DockerEndpoint, where string) {
	f.t.Helper()
	out := f.must(side, "exec", f.ctr, "sh", "-c", "cat /data/state; md5sum -c /data/blob.md5; ip -4 -o addr show eth0")
	for _, want := range []string{"kept", "/data/blob: OK", "10.231.78.9/24"} {
		if !strings.Contains(out, want) {
			f.t.Fatalf("on %s the moved container lacks %q:\n%s", where, want, out)
		}
	}
}

// TestStandInInterruptedMoveResumes (#2209): a carry cut off half way
// (its context ends during the copy, as when the process is stopped)
// keeps its journal and does not roll back; ResumeCarry finishes it with
// the data whole. A second interrupted carry is undone from its journal
// alone (OpenCarry, Rollback): nothing stays on the target and the source
// runs again.
func TestStandInInterruptedMoveResumes(t *testing.T) {
	tg, sshConfig := standIn(t)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	f := newCarryFixture(t, ctx)
	s, err := Connect(ctx, Options{Target: tg, StateDir: t.TempDir(), Instance: "standin-resume", SSHConfig: sshConfig})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	local, remote := DockerEndpoint{Helper: f.image}, DockerEndpoint{Host: s.DockerHost()}
	defer f.cleanup(local)
	defer f.cleanup(remote)
	f.start(local)
	journal := filepath.Join(t.TempDir(), "carry.json")

	interrupt := func(at string) error {
		t.Helper()
		cctx, stop := context.WithCancel(ctx)
		defer stop()
		var once sync.Once
		_, err := StartCarry(cctx, CarryOptions{Instance: f.instance, From: local, To: remote, Journal: journal,
			Progress: func(step, msg string) {
				t.Logf("%s: %s", step, msg)
				if step == at {
					once.Do(stop)
				}
			}})
		return err
	}
	err = interrupt("volume")
	if err == nil || !Interrupted(context.Background(), errors.Unwrap(err)) && !strings.Contains(err.Error(), "interrupted") {
		t.Fatalf("an interrupted carry = %v", err)
	}
	if _, err := os.Stat(journal); err != nil {
		t.Fatalf("the interrupted carry left no journal: %v", err)
	}
	if st := f.must(local, "inspect", "--format", "{{.State.Running}}", f.ctr); st != "false" {
		t.Fatalf("the source runs again after an interruption (%s): it was rolled back", st)
	}

	var steps []string
	c, err := ResumeCarry(ctx, CarryOptions{Instance: f.instance, From: local, To: remote, Journal: journal,
		Progress: func(step, msg string) { steps = append(steps, step); t.Logf("%s: %s", step, msg) }})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(steps, " "), "resume") || !strings.Contains(strings.Join(steps, " "), "already") && !strings.Contains(strings.Join(steps, " "), "image") {
		t.Fatalf("resume steps %v", steps)
	}
	if err := c.StartTarget(ctx); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second)
	f.check(remote, "the stand-in host after a resume")
	if err := c.RemoveSource(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(journal); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the finished carry kept its journal: %v", err)
	}

	// Back to this machine, cut off at the image copy, then undone from the
	// journal alone.
	back := func(at string) error {
		cctx, stop := context.WithCancel(ctx)
		defer stop()
		var once sync.Once
		_, err := StartCarry(cctx, CarryOptions{Instance: f.instance, From: remote, To: local, Journal: journal,
			Progress: func(step, msg string) {
				if step == at {
					once.Do(stop)
				}
			}})
		return err
	}
	if err := back("volume"); err == nil {
		t.Fatal("the second carry was not interrupted")
	}
	u, err := OpenCarry(CarryOptions{Instance: f.instance, From: remote, To: local, Journal: journal})
	if err != nil {
		t.Fatal(err)
	}
	u.Rollback(ctx)
	if _, err := (&Carry{}).run(ctx, local, "container", "inspect", f.ctr); err == nil {
		t.Fatal("the rolled-back carry left the container on this machine")
	}
	if _, err := (&Carry{}).run(ctx, local, "volume", "inspect", f.vol); err == nil {
		t.Fatal("the rolled-back carry left the volume on this machine")
	}
	if st := f.must(remote, "inspect", "--format", "{{.State.Running}}", f.ctr); st != "true" {
		t.Fatalf("after the rollback the source container is not running (%s)", st)
	}
	f.check(remote, "the stand-in host after the rollback")
}

// TestStandInMoveBetweenTwoHosts (#2209): from this machine to one VM, from
// that VM to another (each a stand-in host; the carry streams between the
// two remote Dockers through this machine), and back to this machine, the
// data and the address kept at each leg.
func TestStandInMoveBetweenTwoHosts(t *testing.T) {
	tg, sshConfig := standIn(t)
	raw2 := os.Getenv(envRemoteHost2)
	if raw2 == "" {
		t.Skipf("%s names no second stand-in host", envRemoteHost2)
	}
	tg2, err := ParseTarget(raw2)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	f := newCarryFixture(t, ctx)
	sa, err := Connect(ctx, Options{Target: tg, StateDir: t.TempDir(), Instance: "standin-a", SSHConfig: sshConfig})
	if err != nil {
		t.Fatal(err)
	}
	defer sa.Close()
	sb, err := Connect(ctx, Options{Target: tg2, StateDir: t.TempDir(), Instance: "standin-b", SSHConfig: sshConfig})
	if err != nil {
		t.Fatal(err)
	}
	defer sb.Close()
	local := DockerEndpoint{Helper: f.image}
	a, b := DockerEndpoint{Host: sa.DockerHost()}, DockerEndpoint{Host: sb.DockerHost()}
	defer f.cleanup(local)
	defer f.cleanup(a)
	defer f.cleanup(b)
	f.start(local)
	leg := func(from, to DockerEndpoint, where string) {
		t.Helper()
		c, err := StartCarry(ctx, CarryOptions{Instance: f.instance, From: from, To: to, Journal: filepath.Join(t.TempDir(), "carry.json"),
			Progress: func(step, msg string) { t.Logf("%s: %s", step, msg) }})
		if err != nil {
			t.Fatal(err)
		}
		if err := c.StartTarget(ctx); err != nil {
			c.Rollback(ctx)
			t.Fatal(err)
		}
		time.Sleep(time.Second)
		f.check(to, where)
		if err := c.RemoveSource(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := (&Carry{}).run(ctx, from, "container", "inspect", f.ctr); err == nil {
			t.Fatalf("the source still has %s after the move to %s", f.ctr, where)
		}
	}
	leg(local, a, "the first host")
	f.must(a, "exec", f.ctr, "sh", "-c", "echo kept on a >> /data/state")
	leg(a, b, "the second host")
	if out := f.must(b, "exec", f.ctr, "cat", "/data/state"); out != "kept\nkept on a" {
		t.Fatalf("on the second host /data/state is %q", out)
	}
	leg(b, local, "this machine again")
}
