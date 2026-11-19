package main

// MySQL 5.7 and 5.6 (#1524): their official images are linux/amd64
// only, so an instance of either is served where the cluster's node runs linux/amd64
// binaries, natively or through a binfmt_misc handler registered with
// the F (fix binary) flag, whose interpreter then works inside every
// container: the Rosetta or qemu-x86_64 handler Docker Desktop registers
// on Apple silicon (measured 2026-11-18).

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

// legacyProbe prints the node's machine, then, for each x86-64 handler
// registered, its name, whether it is enabled and its flags.
const legacyProbe = `uname -m
for f in x86_64 rosetta qemu-x86_64; do
  h=/proc/sys/fs/binfmt_misc/$f
  [ -r "$h" ] && echo "$f $(head -n 1 "$h") $(grep '^flags:' "$h" | cut -d' ' -f2)"
done; true`

// LegacyUnrunnable is why the node cannot run version's linux/amd64-only
// image, or "" when it can. The node is asked once.
func (p *sqlProvisioner) LegacyUnrunnable(version string) string {
	p.legacyOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		node := p.cfg.ClusterName() + "-control-plane"
		out, err := exec.CommandContext(ctx, "docker", "exec", node, "sh", "-c", legacyProbe).Output()
		if err != nil {
			p.legacyWhy = "the cluster's node, " + node + ", could not be asked whether it runs linux/amd64 binaries: " + err.Error()
			return
		}
		p.legacyWhy = amd64Unrunnable(string(out))
	})
	if p.legacyWhy == "" {
		return ""
	}
	image := "mysql:5.7, 5.7.44"
	if version == "MYSQL_5_6" {
		image = "mysql:5.6, 5.6.51"
	}
	return version + "'s official image (" + image + ") is linux/amd64 only, and " + p.legacyWhy
}

// amd64Unrunnable reads legacyProbe's output: "" when the node runs
// linux/amd64 binaries, or why not.
func amd64Unrunnable(probe string) string {
	lines := strings.Split(strings.TrimSpace(probe), "\n")
	machine := strings.TrimSpace(lines[0])
	if machine == "x86_64" || machine == "amd64" {
		return ""
	}
	for _, l := range lines[1:] {
		f := strings.Fields(l)
		if len(f) == 3 && f[1] == "enabled" && strings.Contains(f[2], "F") {
			return ""
		}
	}
	return "the cluster's node is " + machine + " with no linux/amd64 emulation registered in binfmt_misc with the F flag " +
		"(Docker Desktop's \"Use Rosetta for x86_64/amd64 emulation\", or qemu-user-static's binfmt handlers)"
}
