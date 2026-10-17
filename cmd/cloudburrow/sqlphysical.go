package main

import (
	"bytes"
	"context"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/components"
	"github.com/cloudburrow/cloudburrow/internal/service/sqladmin"
)

// A physical external sync (#1570): an XtraBackup backup of the source is
// prepared and copied back into the replica's data directory by a Job of
// the official percona/percona-xtrabackup image of the replica's major
// version, on the replica's volume. The backup's files, when they are in
// Cloud Storage, are first written onto that volume through the replica's
// own running server's pod; with none, the Job takes the backup itself,
// with the source's volume mounted and the replication user. The replica's
// server then starts on the restored data, with an --init-file that sets
// its root password back to the instance's (the copy has the source's
// accounts), and replicates from the source by GTID auto-positioning from
// the GTID set in xtrabackup_binlog_info (mysqlPhysicalSetup).

// The files on a replica's volume (mounted at /var/lib/mysql in its
// server's pod): the backup while it is restored, the GTID set it was
// taken at, and the init file.
const (
	physicalBackupDir = "physical-backup"
	physicalGTIDFile  = "cloudsql-physical-gtid"
	physicalInitFile  = "cloudsql-physical-init.sql"
	physicalJobPrefix = "sqlxb-"
)

// PhysicalUnsupported is why spec's server cannot be given a physical
// backup: the backup is restored onto the replica's volume, which an
// instance that is not persistent has none of; and, when the sync takes the
// backup, onto the source's volume too.
func (p *sqlProvisioner) PhysicalUnsupported(spec sqladmin.Spec) string {
	if components.XtraBackupImage(spec.DatabaseVersion) == "" {
		return "there is no Percona XtraBackup image for " + spec.DatabaseVersion + "; MYSQL_8_0 and MYSQL_8_4 are served"
	}
	if !p.persistent {
		return "the backup is restored onto the replica's volume, and an instance started without --mode persistent " +
			"keeps its servers' data in their containers: start it with --mode persistent"
	}
	return ""
}

// sourceClaim is the volume of the CloudBurrow server ext names, which a
// Job that takes the backup mounts, or "" for a source outside CloudBurrow.
func sourceClaim(ext *sqladmin.ExternalSource) string {
	switch {
	case ext == nil || ext.Instance == "":
		return ""
	case ext.InstanceUp:
		return components.CloudSQLMySQLBackend(true, components.MySQLCredentials{}).Name + "-data"
	}
	return components.SQLInstancePrefix + ext.Instance + "-data"
}

func (p *sqlProvisioner) PhysicalRestore(ctx context.Context, spec sqladmin.Spec, b sqladmin.PhysicalBackup) error {
	if why := p.PhysicalUnsupported(spec); why != "" {
		return fmt.Errorf("%s", why)
	}
	ext := spec.External
	deploy := nodeName(spec.Name, spec.PrimaryNode())
	take := len(b.Files) == 0
	if take && sourceClaim(ext) == "" {
		return fmt.Errorf("the source is not a CloudBurrow instance's server, and dumpFilePath names no backup")
	}
	if !take {
		if err := p.uploadPhysical(ctx, deploy, b); err != nil {
			return fmt.Errorf("write the backup onto the replica's volume: %w", err)
		}
	}
	// The replica's server stops, so the Job has its volume to itself.
	if _, err := p.inst.Kubectl(ctx, "-n", p.inst.Namespace, "scale", "deployment/"+deploy, "--replicas=0"); err != nil {
		return err
	}
	if _, err := p.inst.Kubectl(ctx, "-n", p.inst.Namespace, "wait", "--for=delete", "pod", "-l", "app="+deploy, "--timeout=180s"); err != nil {
		return fmt.Errorf("stop the replica's server: %w", err)
	}
	job := physicalJobPrefix + spec.Name
	err := p.runPhysicalJob(ctx, job, physicalJob(p.inst.Namespace, p.inst.Instance, job, spec, take))
	if err == nil {
		// The server starts on the restored data with its --init-file
		// (backend), which Start applies again unchanged.
		restored := spec
		next := *ext
		next.PhysicalRestored = true
		restored.External = &next
		err = p.inst.Apply(ctx, p.nodeBackend(restored, restored.PrimaryNode()).Manifest(p.inst.Namespace, p.inst.Instance))
	}
	if _, serr := p.inst.Kubectl(ctx, "-n", p.inst.Namespace, "scale", "deployment/"+deploy, "--replicas=1"); err == nil {
		err = serr
	}
	return err
}

// uploadPhysical writes the backup's files into the replica's volume,
// through its running server's pod, one file at a time: the server's
// image has a shell and cat, and nothing more is needed.
func (p *sqlProvisioner) uploadPhysical(ctx context.Context, deploy string, b sqladmin.PhysicalBackup) error {
	exec := podExec(p.cfg, deploy)
	dir := "/var/lib/mysql/" + physicalBackupDir
	if err := exec(ctx, nil, &bytes.Buffer{}, "sh", "-c", `rm -rf "$1" && mkdir -p "$1"`, "sh", dir); err != nil {
		return err
	}
	for _, f := range b.Files {
		clean := path.Clean("/" + f)
		if clean == "/" {
			continue
		}
		data, err := b.Read(ctx, f)
		if err != nil {
			return fmt.Errorf("read %s: %w", f, err)
		}
		var out bytes.Buffer
		if err := exec(ctx, bytes.NewReader(data), &out, "sh", "-c", `mkdir -p "$(dirname "$1")" && cat > "$1"`, "sh", dir+clean); err != nil {
			return fmt.Errorf("write %s: %w %s", f, err, strings.TrimSpace(out.String()))
		}
	}
	return nil
}

// runPhysicalJob applies the Job, waits for it to succeed or fail, and
// removes it.
func (p *sqlProvisioner) runPhysicalJob(ctx context.Context, job, manifest string) error {
	_, _ = p.inst.Kubectl(ctx, "-n", p.inst.Namespace, "delete", "job/"+job, "--ignore-not-found", "--wait=true")
	if err := p.inst.Apply(ctx, manifest); err != nil {
		return err
	}
	defer func() {
		_, _ = p.inst.Kubectl(context.WithoutCancel(ctx), "-n", p.inst.Namespace, "delete", "job/"+job, "--ignore-not-found", "--wait=false")
	}()
	for {
		out, err := p.inst.Kubectl(ctx, "-n", p.inst.Namespace, "get", "job/"+job, "-o", "jsonpath={.status.succeeded},{.status.failed}")
		if err == nil {
			ok, failed, _ := strings.Cut(strings.TrimSpace(out), ",")
			if ok != "" && ok != "0" {
				return nil
			}
			if failed != "" && failed != "0" {
				logs, _ := p.inst.Kubectl(ctx, "-n", p.inst.Namespace, "logs", "job/"+job, "--tail=15")
				return fmt.Errorf("the XtraBackup job failed: %s", strings.TrimSpace(logs))
			}
		}
		select {
		case <-ctx.Done():
			logs, _ := p.inst.Kubectl(context.WithoutCancel(ctx), "-n", p.inst.Namespace, "logs", "job/"+job, "--tail=15")
			return fmt.Errorf("the XtraBackup job did not finish: %w %s", ctx.Err(), strings.TrimSpace(logs))
		case <-time.After(2 * time.Second):
		}
	}
}

// physicalScript takes the backup when CLOUDSQL_TAKE_BACKUP is set, with
// the source's volume where its server has it, /var/lib/mysql: XtraBackup
// copies the binary log from the path the server reports. It then
// prepares the backup and copies it back into the replica's emptied data
// directory, without the source's server UUID (auto.cnf) and persisted
// variables (mysqld-auto.cnf), which are the source's own.
const physicalScript = `set -euo pipefail
B=/replica/` + physicalBackupDir + `
if [ -n "${CLOUDSQL_TAKE_BACKUP:-}" ]; then
  rm -rf "$B" && mkdir -p "$B"
  xtrabackup --backup --no-server-version-check --datadir=/var/lib/mysql/data --target-dir="$B" \
    --host="$CLOUDSQL_PRIMARY_HOST" --port="${CLOUDSQL_PRIMARY_PORT:-3306}" --user="$CLOUDSQL_PRIMARY_USER" --password="$CLOUDSQL_REPLICATION_PASSWORD"
fi
if [ -n "$(find "$B" -name '*.zst' -print -quit)" ]; then
  xtrabackup --decompress --remove-original --target-dir="$B"
fi
xtrabackup --prepare --target-dir="$B"
gtid=$(cut -f3- "$B/xtrabackup_binlog_info" | tr -d '\n ')
rm -rf /replica/data && mkdir -p /replica/data
xtrabackup --copy-back --target-dir="$B" --datadir=/replica/data
rm -f /replica/data/auto.cnf /replica/data/mysqld-auto.cnf
printf '%s' "$gtid" > /replica/` + physicalGTIDFile + `
printf '%s\n' "$CLOUDSQL_INIT_SQL" > /replica/` + physicalInitFile + `
rm -rf "$B"
rm -f /replica/cloudsql-physical-replicating
chown -R 999:999 /replica
echo "restored the backup at GTIDs $gtid"
`

// physicalInitSQL sets the root accounts back to the instance's password,
// unlogged, so the server CloudBurrow reaches as root answers again.
func physicalInitSQL(rootPassword string) string {
	pw := "'" + strings.NewReplacer(`\`, `\\`, `'`, `''`).Replace(rootPassword) + "'"
	var sb strings.Builder
	sb.WriteString("SET sql_log_bin = 0;\n")
	for _, host := range []string{"%", "localhost"} {
		u := "'root'@'" + host + "'"
		fmt.Fprintf(&sb, "CREATE USER IF NOT EXISTS %s IDENTIFIED BY %s;\nALTER USER %s IDENTIFIED BY %s;\nGRANT ALL PRIVILEGES ON *.* TO %s WITH GRANT OPTION;\n",
			u, pw, u, pw, u)
	}
	return sb.String()
}

// physicalJob is the XtraBackup Job's manifest.
func physicalJob(namespace, instance, job string, spec sqladmin.Spec, take bool) string {
	ext := spec.External
	env := map[string]string{"CLOUDSQL_INIT_SQL": physicalInitSQL(spec.RootPassword)}
	volumes := fmt.Sprintf("        - name: replica\n          persistentVolumeClaim:\n            claimName: %s\n",
		nodeName(spec.Name, spec.PrimaryNode())+"-data")
	mounts := "            - name: replica\n              mountPath: /replica\n"
	if take {
		env["CLOUDSQL_TAKE_BACKUP"] = "1"
		env["CLOUDSQL_PRIMARY_HOST"] = primaryHost(spec)
		env["CLOUDSQL_PRIMARY_USER"] = ext.User
		env["CLOUDSQL_REPLICATION_PASSWORD"] = ext.Password
		volumes += fmt.Sprintf("        - name: source\n          persistentVolumeClaim:\n            claimName: %s\n            readOnly: true\n", sourceClaim(ext))
		mounts += "            - name: source\n              mountPath: /var/lib/mysql\n              readOnly: true\n"
	}
	var envs strings.Builder
	for _, k := range sortedKeys(env) {
		fmt.Fprintf(&envs, "            - name: %s\n              value: %q\n", k, env[k])
	}
	return fmt.Sprintf(`apiVersion: batch/v1
kind: Job
metadata:
  name: %s
  namespace: %s
  labels:
    cloudburrow.dev/owned: "true"
    cloudburrow.dev/instance: %q
spec:
  backoffLimit: 0
  template:
    metadata:
      labels:
        cloudburrow.dev/owned: "true"
    spec:
      restartPolicy: Never
      securityContext:
        runAsUser: 0
      containers:
        - name: xtrabackup
          image: %s
          command: ["bash", "-c", %q]
          env:
%s          volumeMounts:
%s      volumes:
%s`, job, namespace, instance, components.XtraBackupImage(spec.DatabaseVersion), physicalScript, envs.String(), mounts, volumes)
}

// mysqlPhysicalSetup starts a physically restored replica's replication
// from its source once: its GTIDs are reset to the backup's, the source's
// own replication settings, which the copy carries, are dropped, and the
// init file that set root's password is emptied, as the password is set.
const mysqlPhysicalSetup = `set -e
` + mysqlRootEnv + `marker=/var/lib/mysql/cloudsql-physical-replicating
if [ -f "$marker" ]; then exit 0; fi
gtid=$(cat /var/lib/mysql/` + physicalGTIDFile + `)
mysql $MYSQL_LOCAL -e 'STOP REPLICA; RESET REPLICA ALL' >/dev/null 2>&1 || true
mysql $MYSQL_LOCAL -e 'RESET BINARY LOGS AND GTIDS' 2>/dev/null || mysql $MYSQL_LOCAL -e 'RESET MASTER' # MySQL 8.0's name
mysql $MYSQL_LOCAL -e "SET GLOBAL gtid_purged = '$gtid'"
mysql $MYSQL_LOCAL -e "CHANGE REPLICATION SOURCE TO SOURCE_HOST = '$CLOUDSQL_PRIMARY_HOST', SOURCE_PORT = ${CLOUDSQL_PRIMARY_PORT:-3306}, SOURCE_USER = '$CLOUDSQL_PRIMARY_USER', SOURCE_PASSWORD = '$CLOUDSQL_REPLICATION_PASSWORD', SOURCE_AUTO_POSITION = 1, SOURCE_SSL = 1; START REPLICA; SET PERSIST super_read_only = ON"
echo 'SET sql_log_bin = 0;' > /var/lib/mysql/` + physicalInitFile + `
touch "$marker"
`
