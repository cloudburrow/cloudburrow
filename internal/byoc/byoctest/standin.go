package byoctest

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/byoc"
)

// BootStandIn boots the Docker-in-Docker sshd stand-in host (the container
// scripts/remote-compute-standin.sh starts) as the VM whose cloud-init is
// userData, as cloud-init would on the VM: user (the cloud's default
// user, "ubuntu" on AWS and "cloudburrow" on Azure) gets CloudBurrow's
// public key, sshd gets the VM's host key, and the auto-stop file its
// minutes. It reaches the stand-in through this machine's own Docker.
func BootStandIn(ctx context.Context, container, user, userData string) error {
	priv, pub, key, autoStop, err := parseCloudInit(userData)
	if err != nil {
		return err
	}
	if strings.ContainsAny(user, " \n:'\"") || user == "" {
		return fmt.Errorf("invalid user %q", user)
	}
	script := fmt.Sprintf(`set -e
id %[1]s >/dev/null 2>&1 || { adduser -D -s /bin/sh %[1]s; addgroup %[1]s docker; sed -i 's/^%[1]s:!/%[1]s:*/' /etc/shadow; }
install -d -m 700 -o %[1]s -g %[1]s /home/%[1]s/.ssh
printf '%%s\n' %[2]s > /home/%[1]s/.ssh/authorized_keys
chown %[1]s:%[1]s /home/%[1]s/.ssh/authorized_keys; chmod 600 /home/%[1]s/.ssh/authorized_keys
cat > /etc/ssh/ssh_host_ed25519_key; chmod 600 /etc/ssh/ssh_host_ed25519_key
printf '%%s\n' %[3]s > /etc/ssh/ssh_host_ed25519_key.pub
install -d -m 755 /var/lib/cloudburrow
printf '%%s\n' %[4]d > %[5]s; chown %[1]s %[5]s
kill -HUP "$(cat /var/run/sshd.pid)"`, user, shellQuote(key), shellQuote(pub), autoStop, byoc.AutoStopFile)
	cmd := exec.CommandContext(ctx, "docker", "exec", "-i", container, "sh", "-c", script)
	cmd.Env = append(os.Environ(), "DOCKER_HOST=", "DOCKER_CONTEXT=")
	cmd.Stdin = strings.NewReader(priv)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("booting the stand-in %s as the VM: %v %s", container, err, out)
	}
	time.Sleep(time.Second) // sshd re-executes on HUP
	return nil
}

var reAutoStop = regexp.MustCompile(`printf '%d\\n' (\d+) > ` + regexp.QuoteMeta(byoc.AutoStopFile))

// parseCloudInit reads the host key, CloudBurrow's authorized key and the
// auto-stop minutes out of a cloud-init byoc.RenderCloudInit made.
func parseCloudInit(userData string) (priv, pub, authorized string, autoStop int, err error) {
	var b strings.Builder
	lines := strings.Split(userData, "\n")
	in := ""
	for _, line := range lines {
		switch {
		case line == "ssh_authorized_keys:":
			in = "keys"
		case strings.HasPrefix(line, "  ed25519_private: |"):
			in = "priv"
		case strings.HasPrefix(line, "  ed25519_public: "):
			pub = unquote(strings.TrimPrefix(line, "  ed25519_public: "))
			in = ""
		case in == "keys" && strings.HasPrefix(line, "  - "):
			authorized = unquote(strings.TrimPrefix(line, "  - "))
		case in == "priv" && strings.HasPrefix(line, "    "):
			b.WriteString(strings.TrimPrefix(line, "    ") + "\n")
		default:
			in = ""
		}
	}
	priv = b.String()
	if m := reAutoStop.FindStringSubmatch(userData); m != nil {
		fmt.Sscan(m[1], &autoStop)
	}
	if priv == "" || pub == "" || authorized == "" {
		return "", "", "", 0, fmt.Errorf("the cloud-init carries no host key or authorized key")
	}
	return priv, pub, authorized, autoStop, nil
}

func unquote(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && s[0] == '\'' && s[len(s)-1] == '\'' {
		return strings.ReplaceAll(s[1:len(s)-1], "''", "'")
	}
	return s
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
