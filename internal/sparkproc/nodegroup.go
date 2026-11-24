package sparkproc

import (
	"context"
	"fmt"
	"strings"
)

// Driver node groups (#2162): a cluster's auxiliary node groups of role
// DRIVER, whose nodes run its jobs' drivers
// (https://docs.cloud.google.com/dataproc/docs/guides/node-groups/dataproc-driver-node-groups,
// read 2026-11-22). Each node is a container of the cluster's image on its
// network, named and reached as its instance; a job placed on it
// (Job.DriverNode) runs its driver container in the node's network, so the
// driver's host is the node.

// driverNodeNetwork are the docker run arguments of a job's driver
// container: on the cluster's network as driver, or in its driver node's
// network, and the driver's host name.
func (c *Cluster) driverNodeNetwork(job Job, driver string) ([]string, string) {
	c.mu.Lock()
	node := c.groupNodes[job.DriverNode]
	c.mu.Unlock()
	if job.DriverNode == "" || node == "" {
		return []string{"--hostname", driver, "--network", c.network}, driver
	}
	return []string{"--network", "container:" + node}, job.DriverNode
}

// SetNodeGroup makes the node group id's nodes the instances named:
// starting the missing ones and removing the rest.
func (c *Cluster) SetNodeGroup(ctx context.Context, id string, instances []string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.gone {
		return fmt.Errorf("the cluster was deleted")
	}
	if c.groups == nil {
		c.groups, c.groupNodes = map[string][]string{}, map[string]string{}
	}
	want := map[string]bool{}
	for _, inst := range instances {
		want[inst] = true
	}
	var keep []string
	for _, inst := range c.groups[id] {
		if want[inst] {
			keep = append(keep, inst)
			continue
		}
		_, _ = c.d.docker(context.Background(), "rm", "-f", "-v", c.groupNodes[inst])
		delete(c.groupNodes, inst)
	}
	have := map[string]bool{}
	for _, inst := range keep {
		have[inst] = true
	}
	for _, inst := range instances {
		if have[inst] {
			continue
		}
		name := c.master[:len(c.master)-2] + "-ng-" + strings.TrimPrefix(inst, c.spec.Name+"-")
		args := []string{"run", "-d", "--name", name, "--hostname", inst, "--network-alias", inst, "--label", c.label,
			"--label", "cloudburrow.dataproc.cluster=" + c.spec.ID, "--network", c.network,
			"--entrypoint", "sleep", c.rt.Image, "infinity"}
		if out, err := c.d.docker(ctx, args...); err != nil {
			return &Unavailable{Msg: "start the node group's node " + inst + ": " + out}
		}
		c.groupNodes[inst] = name
		keep = append(keep, inst)
	}
	c.groups[id] = keep
	return nil
}

// groupContainersLocked are the node groups' containers.
func (c *Cluster) groupContainersLocked() []string {
	var out []string
	for _, id := range sortedGroupIDs(c.groups) {
		for _, inst := range c.groups[id] {
			out = append(out, c.groupNodes[inst])
		}
	}
	return out
}

func sortedGroupIDs(m map[string][]string) []string {
	ids := make([]string, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	for i := 1; i < len(ids); i++ {
		for j := i; j > 0 && ids[j] < ids[j-1]; j-- {
			ids[j], ids[j-1] = ids[j-1], ids[j]
		}
	}
	return ids
}
