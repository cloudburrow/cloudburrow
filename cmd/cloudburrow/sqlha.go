package main

// The servers of a Cloud SQL high-availability instance (#1408). A
// REGIONAL instance is two Deployments: sqlinst-NAME, the server it was
// created with, and sqlha-NAME, its second. One is the primary, which the
// instance's Service, sqlinst-NAME, selects, so the host tunnel, the
// connector and the instance's read replicas reach it; the other is a
// standby streaming from that Service as cloudsqlstandby, which the
// primary's synchronous_standby_names names with synchronous_commit on: a
// commit returns once the standby has flushed it, Cloud SQL's guarantee
// that a failover loses no committed transaction. Read replicas stream as
// walreceiver, so they never stand in for the standby.
//
// A failover selects the standby in the Service, stops the primary's
// server, promotes the standby, and rebuilds the old primary from it as the
// new standby. The promoted server's Deployment is not changed, so it does
// not restart: its epoch is the one its volume records, and it keeps its
// data across later restarts.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/components"
	"github.com/cloudburrow/cloudburrow/internal/service/sqladmin"
)

// nodeName is the Deployment of server node (0 or 1) of instance name.
func nodeName(name string, node int) string {
	if node == 1 {
		return components.SQLStandbyPrefix + name
	}
	return components.SQLInstancePrefix + name
}

// nodeBackend is server node of spec: its own name and, for node 0, the
// instance's Service, which selects the primary's server; a server that
// has been a standby keeps the standby's start, at its epoch.
func (p *sqlProvisioner) nodeBackend(spec sqladmin.Spec, node int) components.Backend {
	b := p.backend(spec)
	b.Name = nodeName(spec.Name, node)
	if node == 0 {
		b.Selects = nodeName(spec.Name, spec.PrimaryNode())
	}
	if e := spec.HAEpochs[node]; e > 0 && !spec.MySQL() {
		b = components.CloudSQLStandbyBackend(b, components.SQLInstancePrefix+spec.Name, e)
	}
	return b
}

// applyService applies the instance's Service alone, selecting the
// primary's server.
func (p *sqlProvisioner) applyService(ctx context.Context, spec sqladmin.Spec) error {
	m := p.nodeBackend(spec, 0).Manifest(p.inst.Namespace, p.inst.Instance)
	const sep = "---\napiVersion: v1\nkind: Service\n"
	i := strings.Index(m, sep)
	if i < 0 {
		return fmt.Errorf("instance %s: no Service in its manifest", spec.Name)
	}
	return p.inst.Apply(ctx, m[i+4:])
}

// psql runs a script in deployment's server as postgres.
func (p *sqlProvisioner) psql(ctx context.Context, deployment, script string) (string, error) {
	var out strings.Builder
	err := nodeExec(p.cfg, deployment)(ctx, strings.NewReader(script), &out,
		"psql", "-v", "ON_ERROR_STOP=1", "-X", "-qtA", "-U", "postgres", "-d", "postgres")
	if err != nil {
		return "", fmt.Errorf("%s: %w %s", deployment, err, strings.TrimSpace(out.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

// pgStandbySource lets the standby connect to the primary for
// replication: the replication role and its pg_hba.conf line, as a read
// replica's primary has. The role's commit is local: no standby may be
// streaming yet to acknowledge it.
const pgStandbySource = `SET synchronous_commit TO local;
DO $$BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = '` + components.SQLReplicaUser + `') THEN
    CREATE ROLE ` + components.SQLReplicaUser + ` WITH REPLICATION LOGIN;
  END IF;
END$$;
`

// standby makes spec's standby, for a REGIONAL instance, or removes it,
// for a ZONAL one, and sets the primary's synchronous replication to
// match.
func (p *sqlProvisioner) standby(ctx context.Context, spec sqladmin.Spec) error {
	if spec.MySQL() {
		return nil
	}
	primary := nodeName(spec.Name, spec.PrimaryNode())
	sb := nodeName(spec.Name, 1-spec.PrimaryNode())
	if !spec.Regional() {
		if spec.HAEpochs == [2]int{} {
			return nil // never had a standby
		}
		if _, err := p.psql(ctx, primary, "ALTER SYSTEM RESET synchronous_standby_names;\nSELECT pg_reload_conf();\n"); err != nil {
			return err
		}
		obj := []string{"deployment/" + sb, "persistentvolumeclaim/" + sb + "-data"}
		if spec.PrimaryNode() == 0 {
			obj = append(obj, "service/"+sb)
		}
		_, err := p.inst.Kubectl(ctx, append(append([]string{"-n", p.inst.Namespace, "delete"}, obj...), "--ignore-not-found", "--wait=true")...)
		return err
	}
	if _, err := p.psql(ctx, primary, pgStandbySource); err != nil {
		return fmt.Errorf("prepare %s as the standby's source: %w", primary, err)
	}
	{
		const line = "host replication " + components.SQLReplicaUser + " all trust"
		const add = `hba=$(psql -U postgres -XtAc 'SHOW hba_file') && { grep -qxF '` + line + `' "$hba" || ` +
			`{ echo '` + line + `' >> "$hba" && psql -U postgres -XtAc 'SELECT pg_reload_conf()'; }; }`
		var o strings.Builder
		if err := nodeExec(p.cfg, primary)(ctx, nil, &o, "sh", "-c", add); err != nil {
			return fmt.Errorf("let the standby connect to %s: %w %s", primary, err, o.String())
		}
	}
	b := p.nodeBackend(spec, 1-spec.PrimaryNode())
	if err := p.inst.Apply(ctx, b.Manifest(p.inst.Namespace, p.inst.Instance)); err != nil {
		return err
	}
	if err := p.rolloutDeployment(ctx, sb); err != nil {
		return err
	}
	if _, err := p.psql(ctx, primary, "ALTER SYSTEM SET synchronous_standby_names = '"+components.SQLStandbyApplicationName+
		"';\nALTER SYSTEM SET synchronous_commit = 'on';\nSELECT pg_reload_conf();\n"); err != nil {
		return err
	}
	// Done once the standby streams synchronously.
	for {
		st, err := p.psql(ctx, primary, "SELECT sync_state FROM pg_stat_replication WHERE application_name = '"+
			components.SQLStandbyApplicationName+"' AND state = 'streaming';\n")
		if err == nil && strings.Contains(st, "sync") && !strings.Contains(st, "async") {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("the standby of %s does not stream synchronously (%q): %w", spec.Name, st, ctx.Err())
		case <-time.After(time.Second):
		}
	}
}

// Failover makes the standby the primary and rebuilds the old primary as
// the new standby (#1408).
func (p *sqlProvisioner) Failover(ctx context.Context, from, to sqladmin.Spec) error {
	if to.MySQL() {
		return fmt.Errorf("a MySQL instance has no standby")
	}
	oldPrimary := nodeName(from.Name, from.PrimaryNode())
	newPrimary := nodeName(to.Name, to.PrimaryNode())
	// The Service first: a client reconnecting once the old primary has
	// stopped reaches the new one.
	if err := p.applyService(ctx, to); err != nil {
		return err
	}
	if _, err := p.inst.Kubectl(ctx, "-n", p.inst.Namespace, "scale", "deployment/"+oldPrimary, "--replicas=0"); err != nil {
		return err
	}
	if _, err := p.inst.Kubectl(ctx, "-n", p.inst.Namespace, "wait", "--for=delete", "pod", "-l", "app="+oldPrimary,
		"--timeout=120s"); err != nil {
		return fmt.Errorf("stop the primary %s: %w", oldPrimary, err)
	}
	// Every transaction the old primary committed, the standby had
	// flushed; promotion replays what it has and ends recovery.
	if out, err := p.psql(ctx, newPrimary, "SELECT pg_promote(true, 60);\n"); err != nil || out != "t" {
		return fmt.Errorf("promote the standby %s: %v %s", newPrimary, err, out)
	}
	// Start applies the old primary's server again, at its new epoch: it
	// rebuilds from the new primary and streams as its standby.
	return p.Start(ctx, to)
}
