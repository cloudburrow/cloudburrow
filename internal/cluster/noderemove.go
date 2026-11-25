package cluster

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Deleting a cluster, or one of its nodes, never fails because a pod had an
// NFS mount, as on Google (a Filestore share, #2192). A process blocked in
// I/O on a hard NFS mount, its server gone or slow, is in uninterruptible
// sleep, so the node container cannot be killed: Docker answers "could not
// kill container: tried to kill container, but did not receive an exit
// event". Before a node goes, its kubelet is stopped, so that it mounts
// nothing again, and every NFS mount is force-unmounted lazily: MNT_FORCE
// aborts the RPCs in flight, so the blocked processes get EIO and can be
// killed, and MNT_DETACH unmounts it even while it is busy. A removal that
// still fails is retried while the processes exit.

// releaseScript stops the node's kubelet and force-unmounts, lazily, every
// NFS mount, deepest first.
const releaseScript = `systemctl stop kubelet >/dev/null 2>&1; ` +
	`awk '$3 ~ /^nfs/ {print $2}' /proc/mounts | sort -r | while read -r m; do umount -f -l "$m" >/dev/null 2>&1; done; true`

// releaseTimeout bounds releasing one node.
const releaseTimeout = 30 * time.Second

// removeAttempts and removeDelay bound retrying a node's removal: the n-th
// retry waits n times removeDelay. A variable so tests do not wait.
var (
	removeAttempts = 6
	removeDelay    = 5 * time.Second
)

// releaseNodes releases each node's NFS mounts. It is best effort: a node
// already gone, or without a shell, is simply removed.
func releaseNodes(ctx context.Context, r Runner, nodes []string) {
	for _, n := range nodes {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
		_, _ = r.Run(rctx, "docker", "exec", n, "sh", "-c", releaseScript)
		cancel()
	}
}

// removeNodeContainer releases a node's NFS mounts and removes its
// container, retrying while its processes exit. A container already gone is
// removed.
func removeNodeContainer(ctx context.Context, r Runner, name string) error {
	releaseNodes(ctx, r, []string{name})
	var err error
	for attempt := 0; attempt < removeAttempts; attempt++ {
		if attempt > 0 {
			if werr := waitRetry(ctx, attempt); werr != nil {
				return fmt.Errorf("remove node %s: %w (last: %v)", name, werr, err)
			}
			releaseNodes(ctx, r, []string{name})
		}
		if _, err = r.Run(ctx, "docker", "rm", "-f", "-v", name); err == nil || noSuchContainer(err) {
			return nil
		}
	}
	return err
}

// deleteKind runs kind delete cluster after releasing every node's NFS
// mounts; when it fails, it removes the nodes left, retrying each, and runs
// it again, so that kind forgets the cluster.
func (c *Cluster) deleteKind(ctx context.Context) error {
	if nodes, err := c.nodes(ctx); err == nil {
		releaseNodes(ctx, c.runner, nodes)
	}
	_, err := c.runner.Run(ctx, "kind", "delete", "cluster", "--name", c.opts.Name)
	if err == nil {
		return nil
	}
	left, lerr := c.nodes(ctx)
	if lerr != nil {
		return err
	}
	for _, n := range left {
		if rerr := removeNodeContainer(ctx, c.runner, n); rerr != nil {
			return fmt.Errorf("%w; then: %v", err, rerr)
		}
	}
	_, err = c.runner.Run(ctx, "kind", "delete", "cluster", "--name", c.opts.Name)
	return err
}

func waitRetry(ctx context.Context, attempt int) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(time.Duration(attempt) * removeDelay):
		return nil
	}
}

func noSuchContainer(err error) bool {
	return strings.Contains(err.Error(), "No such container")
}
