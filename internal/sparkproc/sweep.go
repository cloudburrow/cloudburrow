package sparkproc

import (
	"context"
	"os/exec"
	"strings"
)

// InstanceLabel labels every container and network the runner makes
// with the instance it was made for, as #2152 labels the instance's
// images: io.cloudburrow.instance=<name>.
const InstanceLabel = "io.cloudburrow.instance"

// instanceLabel is the label=value pair of d's instance.
func (d *Docker) instanceLabel() string { return InstanceLabel + "=" + d.Instance }

// RemoveInstance removes every container (with its anonymous volumes)
// and then every network labelled for instance: what a workload's
// cleanup left when the process running it ended first, such as a
// cancelled batch's relay when `cloudburrow stop` ended `up`. It returns
// the containers and networks removed. Nothing without the label is
// touched; an empty instance removes nothing.
func RemoveInstance(ctx context.Context, instance string) (containers, networks []string) {
	if instance == "" {
		return nil, nil
	}
	if _, err := exec.LookPath("docker"); err != nil {
		return nil, nil
	}
	filter := "label=" + InstanceLabel + "=" + instance
	run := func(args ...string) string {
		out, err := exec.CommandContext(ctx, "docker", args...).Output()
		if err != nil {
			return ""
		}
		return string(out)
	}
	for _, id := range strings.Fields(run("ps", "-aq", "--filter", filter)) {
		if err := exec.CommandContext(ctx, "docker", "rm", "-f", "-v", id).Run(); err == nil {
			containers = append(containers, id)
		}
	}
	for _, id := range strings.Fields(run("network", "ls", "-q", "--filter", filter)) {
		if err := exec.CommandContext(ctx, "docker", "network", "rm", id).Run(); err == nil {
			networks = append(networks, id)
		}
	}
	return containers, networks
}
