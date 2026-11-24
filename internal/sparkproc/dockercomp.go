package sparkproc

// The DOCKER optional component (#2162): a Docker daemon on every node,
// as the component page documents
// (https://docs.cloud.google.com/dataproc/docs/concepts/components/docker,
// read 2026-11-23), the release notes' Docker of the image version
// (24.0 on 2.1 and 2.2, 28.1 on 2.3 and 3.0). Each node's daemon is a
// container of the official docker:VERSION-dind-rootless image sharing
// the node's network: rootless dockerd (RootlessKit, the daemon and its
// containers in a user namespace of the image's unprivileged user), which
// runs without --privileged on Docker Desktop given seccomp, AppArmor and
// the /proc and /sys masks unconfined and /dev/net/tun (RootlessKit's
// network). Its API is tcp://localhost:2375 in the node's network, which
// only the cluster's internal network reaches; the node's containers, its
// NodeManager's and the jobs' drivers have the image's docker CLI at
// /usr/bin/docker and DOCKER_HOST (a driver's: the master's daemon, as a
// Dataproc job's driver runs on the master).

import (
	"context"
	"regexp"
	"time"
)

// ComponentDocker is the DOCKER optional component.
const ComponentDocker = "DOCKER"

// dockerPort is a node daemon's API port in the node's network.
const dockerPort = "2375"

// dockerImage is the cluster's dind-rootless image, "" when not served.
func (c *Cluster) dockerImage() string {
	e := c.d.Ecosystem
	if !c.has(ComponentDocker) || e == nil || e.Docker == nil {
		return ""
	}
	return e.Docker(c.spec.ImageVersion)
}

// dockerClient are the docker run arguments that give a container the
// docker CLI and DOCKER_HOST: the daemon of the network it shares (local)
// or the master's.
func (c *Cluster) dockerClient(local bool) []string {
	image := c.dockerImage()
	if image == "" {
		return nil
	}
	host := "localhost"
	if !local {
		host = c.nodeAlias(-1)
	}
	return []string{"--mount", "type=image,source=" + image + ",target=/usr/bin/docker,image-subpath=usr/local/bin/docker",
		"-e", "DOCKER_HOST=tcp://" + host + ":" + dockerPort}
}

// dockerd is the daemon container of node.
func dockerd(node string) string { return node + "-docker" }

// startDockerd starts node's Docker daemon and waits for its API.
func (c *Cluster) startDockerd(ctx context.Context, node string) error {
	image := c.dockerImage()
	if image == "" {
		return nil
	}
	name := dockerd(node)
	args := []string{"run", "-d", "--name", name, "--label", c.label, "--label", "cloudburrow.dataproc.cluster=" + c.spec.ID,
		"--network", "container:" + node, "--device", "/dev/net/tun",
		"--security-opt", "seccomp=unconfined", "--security-opt", "apparmor=unconfined", "--security-opt", "systempaths=unconfined",
		"-e", "DOCKER_TLS_CERTDIR=", image}
	if out, err := c.d.docker(ctx, args...); err != nil {
		return &Unavailable{Msg: "start the Docker daemon of " + node + ": " + out}
	}
	return c.waitLog(ctx, name, regexp.MustCompile(`API listen on \[::\]:`+dockerPort+`|API listen on 0\.0\.0\.0:`+dockerPort), 3*time.Minute)
}

// dockerdsLocked are the cluster's Docker daemon containers.
func (c *Cluster) dockerdsLocked() []string {
	if c.dockerImage() == "" {
		return nil
	}
	out := []string{dockerd(c.master)}
	for _, w := range c.workers {
		out = append(out, dockerd(w))
	}
	return out
}
