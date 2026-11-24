package dataform

// SSH git remotes (#2105): git_remote_settings.ssh_authentication_config,
// "authentication fields for remote uris using SSH protocol": the Secret
// Manager secret version holding the SSH private key, and the remote
// host's public key, which git's ssh verifies the host against
// (https://docs.cloud.google.com/dataform/docs/connect-repository, "Connect
// a remote repository with SSH"). git runs with core.sshCommand set to the
// host's ssh with that key alone, a known_hosts of that host key alone,
// strict host key checking and no user configuration.
//
// The remote is whatever git host the user configures: a host on this
// machine, on their network or on the internet, as on Google's Dataform.
// Nothing here assumes a loopback host; git's ssh dials the URL's host
// and port, and verifies the host against the configured key alone.

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	dfpb "cloud.google.com/go/dataform/apiv1/dataformpb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// scpLikeRE is git's scp-like SSH syntax, user@host:path.
var scpLikeRE = regexp.MustCompile(`^([A-Za-z0-9._-]+)@([A-Za-z0-9.-]+|\[[0-9a-fA-F:]+\]):([^/].*)$`)

// sshRemote is an SSH remote URL's host and port ("" for 22).
func sshRemote(u string) (host, port string, ok bool) {
	if strings.HasPrefix(u, "ssh://") || strings.HasPrefix(u, "git+ssh://") {
		p, err := url.Parse(u)
		if err != nil || p.Hostname() == "" || strings.Trim(p.Path, "/") == "" {
			return "", "", false
		}
		return p.Hostname(), p.Port(), true
	}
	if m := scpLikeRE.FindStringSubmatch(u); m != nil {
		return strings.Trim(m[2], "[]"), "", true
	}
	return "", "", false
}

// hostKeyRE is an OpenSSH public key line: its type and base64 key.
var hostKeyRE = regexp.MustCompile(`^(ssh-ed25519|ssh-rsa|ecdsa-sha2-nistp256|ecdsa-sha2-nistp384|ecdsa-sha2-nistp521|sk-ssh-ed25519@openssh\.com|sk-ecdsa-sha2-nistp256@openssh\.com)\s+([A-Za-z0-9+/]+={0,3})(\s+.*)?$`)

// checkSSH validates an SSH remote's URL and authentication config.
func checkSSH(gr *dfpb.Repository_GitRemoteSettings) error {
	c := gr.GetSshAuthenticationConfig()
	host, _, isSSH := sshRemote(gr.GetUrl())
	if c == nil {
		if isSSH {
			return apierror.InvalidArgument("git_remote_settings.url %q is an SSH URL: set ssh_authentication_config", gr.GetUrl())
		}
		return nil
	}
	if !isSSH {
		return apierror.InvalidArgument("git_remote_settings.url %q must be an SSH URL (ssh://HOST/PATH or USER@HOST:PATH) with ssh_authentication_config", gr.GetUrl())
	}
	if host == "" {
		return apierror.InvalidArgument("git_remote_settings.url %q has no host", gr.GetUrl())
	}
	if gr.GetAuthenticationTokenSecretVersion() != "" || gr.GetGitRepositoryLink() != "" {
		return apierror.InvalidArgument("git_remote_settings.ssh_authentication_config cannot be set with authentication_token_secret_version or git_repository_link")
	}
	if v := c.GetUserPrivateKeySecretVersion(); !secretVersRE.MatchString(v) {
		return apierror.InvalidArgument("git_remote_settings.ssh_authentication_config.user_private_key_secret_version %q must be projects/*/secrets/*/versions/*", v)
	}
	m := hostKeyRE.FindStringSubmatch(strings.TrimSpace(c.GetHostPublicKey()))
	if m == nil {
		return apierror.InvalidArgument("git_remote_settings.ssh_authentication_config.host_public_key must be an OpenSSH public key (TYPE BASE64)")
	}
	if _, err := base64.StdEncoding.DecodeString(m[2]); err != nil {
		return apierror.InvalidArgument("git_remote_settings.ssh_authentication_config.host_public_key: the key is not base64")
	}
	return nil
}

// sshConf writes the remote's private key and known host for repo, and
// returns git's -c settings using them.
func (s *Server) sshConf(repo, remoteURL string, c *dfpb.Repository_GitRemoteSettings_SshAuthenticationConfig, key []byte) ([]string, error) {
	host, port, _ := sshRemote(remoteURL)
	sum := sha256.Sum256([]byte(repo))
	dir := filepath.Join(s.opts.DataDir, "ssh", hex.EncodeToString(sum[:8]))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	keyFile, known := filepath.Join(dir, "id"), filepath.Join(dir, "known_hosts")
	k := strings.TrimSpace(string(key)) + "\n"
	if err := os.WriteFile(keyFile, []byte(k), 0o600); err != nil {
		return nil, err
	}
	m := hostKeyRE.FindStringSubmatch(strings.TrimSpace(c.GetHostPublicKey()))
	name := host
	if port != "" && port != "22" {
		name = "[" + host + "]:" + port
	}
	if err := os.WriteFile(known, []byte(name+" "+m[1]+" "+m[2]+"\n"), 0o600); err != nil {
		return nil, err
	}
	cmd := "ssh -F /dev/null -i " + shellQuote(keyFile) + " -o IdentitiesOnly=yes -o IdentityAgent=none -o BatchMode=yes" +
		" -o StrictHostKeyChecking=yes -o UserKnownHostsFile=" + shellQuote(known) + " -o GlobalKnownHostsFile=/dev/null"
	return []string{"core.sshCommand=" + cmd}, nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
