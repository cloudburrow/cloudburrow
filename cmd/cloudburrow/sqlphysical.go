package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/components"
	"github.com/cloudburrow/cloudburrow/internal/service/sqladmin"
)

// A physical external sync (#1570): an XtraBackup backup of the source is
// prepared and copied back into the replica's data directory by a Job of
// the official percona/percona-xtrabackup image of the replica's major
// version (2.4 for MySQL 5.7, #1524; 8.0 for 8.0), on the replica's volume. The backup's files, when they are in
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
// backup: there is no XtraBackup image of its version. MySQL 5.7's,
// XtraBackup 2.4, is linux/amd64 only, as its server's image is, so it
// runs wherever a MYSQL_5_7 instance can be created (sqllegacy.go).
func (p *sqlProvisioner) PhysicalUnsupported(spec sqladmin.Spec) string {
	if components.XtraBackupImage(spec.DatabaseVersion) == "" {
		return "there is no Percona XtraBackup image for " + spec.DatabaseVersion + "; MYSQL_5_7 and MYSQL_8_0 are served"
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
	return components.SQLInstancePrefix + sqlServerOf(ext.Instance) + "-data"
}

func (p *sqlProvisioner) PhysicalRestore(ctx context.Context, spec sqladmin.Spec, b sqladmin.PhysicalBackup) error {
	if why := p.PhysicalUnsupported(spec); why != "" {
		return fmt.Errorf("%s", why)
	}
	if !p.persistent {
		return p.physicalRestoreEphemeral(ctx, spec, b)
	}
	ext := spec.External
	deploy := nodeName(spec.ServerName(), spec.PrimaryNode())
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
  ` + physicalVersionCheck + `
  xtrabackup --backup $nsvc --datadir=/var/lib/mysql/data --target-dir="$B" \
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

// physicalVersionCheck sets nsvc to --no-server-version-check when the
// image's XtraBackup has it (8.0's), and to nothing otherwise: XtraBackup
// 2.4 (#1524) is built for MySQL 5.7, the only server it backs up here.
const physicalVersionCheck = `nsvc=; if xtrabackup --help 2>/dev/null | grep -q -- --no-server-version-check; then nsvc=--no-server-version-check; fi`

// physicalInitSQL sets the root accounts back to the instance's password,
// and gives the restored copy CloudBurrow's own account (#1993), unlogged,
// so the server CloudBurrow reaches answers again. version is the
// replica's: MySQL 5.7 (#1524) or 8.0, whose statements are the same.
func physicalInitSQL(version, rootPassword string) string {
	pw := "'" + strings.NewReplacer(`\`, `\\`, `'`, `''`).Replace(rootPassword) + "'"
	var sb strings.Builder
	sb.WriteString("SET sql_log_bin = 0;\n")
	for _, host := range []string{"%", "localhost"} {
		u := "'root'@'" + host + "'"
		fmt.Fprintf(&sb, "CREATE USER IF NOT EXISTS %s IDENTIFIED BY %s;\nALTER USER %s IDENTIFIED BY %s;\nGRANT ALL PRIVILEGES ON *.* TO %s WITH GRANT OPTION;\n",
			u, pw, u, pw, u)
	}
	sb.WriteString(strings.TrimPrefix(sqladmin.MySQLSystemAccountsSQL(version, rootPassword), "SET SESSION sql_log_bin = 0;\n"))
	return sb.String()
}

// physicalJob is the XtraBackup Job's manifest.
func physicalJob(namespace, instance, job string, spec sqladmin.Spec, take bool) string {
	ext := spec.External
	env := map[string]string{"CLOUDSQL_INIT_SQL": physicalInitSQL(spec.DatabaseVersion, spec.RootPassword)}
	volumes := fmt.Sprintf("        - name: replica\n          persistentVolumeClaim:\n            claimName: %s\n",
		nodeName(spec.ServerName(), spec.PrimaryNode())+"-data")
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

// An ephemeral instance's physical sync (#1614). The server `up` runs for
// --services cloudsql-mysql keeps its data in its container's writable
// layer, with no volume, so the Job above, which mounts the source's
// volume, cannot take its backup. Instead:
//
//   - the replica's server, idle and empty until the sync, is started on
//     its volume (a created instance's server has one in either mode,
//     #1740) with a start (physicalWrapper) that swaps a restored data
//     directory in before mysqld starts;
//   - XtraBackup runs in an ephemeral container (kubectl's debug
//     containers) added to the replica's pod with that volume mounted:
//     it prepares the backup and copies it back beside the live data
//     directory, then the replica's mysqld is stopped, and its container,
//     started again on the same volume, swaps the copy in and starts
//     mysqld on it with the --init-file;
//   - when the sync takes the backup, a second ephemeral container is added
//     to the source server's pod, sharing its process namespace
//     (targetContainerName): the source's data files, in its container's
//     own filesystem, are reached through /proc/PID/root of its mysqld,
//     linked at /var/lib/mysql so that the paths the server reports (its
//     binary log's) hold, and xtrabackup --backup --stream=xbstream, which
//     connects over the pod's loopback, streams the backup through the
//     host into the replica's ephemeral container, which unpacks it.
//
// Ephemeral containers stay in their pod's spec, ended, until the pod goes.

// physicalSwapMarker, beside the replica's data directory, says a restored
// copy is ready in physicalRestoredDir for the server's next start.
const (
	physicalSwapMarker  = "cloudsql-physical-swap"
	physicalRestoredDir = "physical-restored"
)

// physicalWrapper starts an ephemeral instance's physically synced
// replica's server: a restored copy, when one is ready, replaces the data
// directory first, and an --init-file, when the restore left one, is
// passed to mysqld. The image's entrypoint then starts mysqld with the
// backend's arguments.
const physicalWrapper = `set -e
cd /var/lib/mysql
if [ -f ` + physicalSwapMarker + ` ] && [ -d ` + physicalRestoredDir + ` ]; then
  rm -rf data && mv ` + physicalRestoredDir + ` data && rm -f ` + physicalSwapMarker + `
  echo "swapped in the restored data directory"
fi
if [ -s ` + physicalInitFile + ` ]; then set -- "$@" --init-file=/var/lib/mysql/` + physicalInitFile + `; fi
exec docker-entrypoint.sh "$@"
`

// physicalStart is physicalWrapper for a server of version: MySQL 5.7's
// (#1524) starts under cloudSQLMySQL57Command's open-files limit, which
// this command replaces, as mysqld 5.7 sizes its tables from it.
func physicalStart(version string) string {
	if sqladmin.IsLegacyMySQL(version) {
		return strings.Replace(physicalWrapper, "set -e\n", "set -e\nulimit -n 1048576\n", 1)
	}
	return physicalWrapper
}

// scratchPhysical is whether spec's server is an ephemeral instance's
// physically synced MySQL replica, which physicalWrapper starts.
func (p *sqlProvisioner) scratchPhysical(spec sqladmin.Spec) bool {
	ext := spec.External
	return !p.persistent && spec.MySQL() && ext != nil && ext.Physical
}

// physicalEphemeralScript is physicalScript for the ephemeral container:
// the backup, already in /replica/physical-backup, is prepared and copied
// back into physicalRestoredDir, beside the live data directory.
const physicalEphemeralScript = `set -euo pipefail
B=/replica/` + physicalBackupDir + `
R=/replica/` + physicalRestoredDir + `
if [ -n "${CLOUDSQL_TAKEN:-}" ] && [ -f "$B/xtrabackup_tablespaces" ]; then
  ` + physicalUnexternal + `
fi
if [ -n "$(find "$B" -name '*.zst' -print -quit)" ]; then
  xtrabackup --decompress --remove-original --target-dir="$B"
fi
xtrabackup --prepare --target-dir="$B"
gtid=$(cut -f3- "$B/xtrabackup_binlog_info" | tr -d '\n ')
rm -rf "$R" && mkdir -p "$R"
xtrabackup --copy-back --target-dir="$B" --datadir="$R"
rm -f "$R/auto.cnf" "$R/mysqld-auto.cnf"
printf '%s' "$gtid" > /replica/` + physicalGTIDFile + `
printf '%s\n' "$CLOUDSQL_INIT_SQL" > /replica/` + physicalInitFile + `
rm -rf "$B"
rm -f /replica/cloudsql-physical-replicating
chown -R 999:999 "$R" /replica/` + physicalGTIDFile + ` /replica/` + physicalInitFile + `
touch /replica/` + physicalSwapMarker + `
echo "restored the backup at GTIDs $gtid"
`

// physicalUnexternal drops, from a backup the sync took through the
// source's /proc/PID/root, the tablespaces XtraBackup listed as external:
// it resolves each file's real path to tell the data directory's own
// tablespaces from external ones, and a path through another container's
// root does not resolve (realpath loops on the /proc link), so every
// file-per-table tablespace is listed under its absolute path, which
// --copy-back would write outside the data directory. Measured 2026-11-17
// (#1614): the restored server then answers "Tablespace is missing". A
// CloudBurrow server has no tablespace outside its data directory.
const physicalUnexternal = `sed -i -E 's#\{"file_name":"/var/lib/mysql/data/[^"]*","tablespace_name":"[^"]*","tablespace_type":"[^"]*"\},?##g; s#,\]#]#g' "$B/xtrabackup_tablespaces"`

// physicalStreamScript takes the backup in the source's ephemeral
// container, onto stdout as an xbstream.
const physicalStreamScript = `set -euo pipefail
pid=
for d in /proc/[0-9]*; do
  if [ "$(cat "$d/comm" 2>/dev/null)" = mysqld ]; then pid=${d#/proc/}; break; fi
done
[ -n "$pid" ] || { echo "no mysqld in the source's pod" >&2; exit 1; }
rm -rf /var/lib/mysql && mkdir -p /var/lib && ln -s "/proc/$pid/root/var/lib/mysql" /var/lib/mysql
mkdir -p /tmp/xb
` + physicalVersionCheck + `
xtrabackup --backup --stream=xbstream $nsvc --datadir=/var/lib/mysql/data --target-dir=/tmp/xb \
  --host=127.0.0.1 --port=3306 --user="$1" --password="$2"
`

// podOf is the running pod of the Deployment the app label app names, and
// its server's container: not one being deleted, as the pod a rollout
// replaced still runs for a while, and, when volume is set, one that has
// that volume (the template the rollout applied).
func (p *sqlProvisioner) podOf(ctx context.Context, app, volume string) (pod, container string, err error) {
	for {
		out, err := p.inst.Kubectl(ctx, "-n", p.inst.Namespace, "get", "pods", "-l", "app="+app,
			"--field-selector=status.phase=Running", "-o",
			`jsonpath={range .items[*]}{.metadata.name} {.spec.containers[0].name} {.metadata.deletionTimestamp}|{.spec.volumes[*].name}{"\n"}{end}`)
		if err == nil {
			for _, line := range strings.Split(out, "\n") {
				head, vols, _ := strings.Cut(line, "|")
				f := strings.Fields(head)
				if len(f) == 2 && (volume == "" || slices.Contains(strings.Fields(vols), volume)) {
					return f[0], f[1], nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return "", "", fmt.Errorf("no running pod of %s: %v %w", app, err, ctx.Err())
		case <-time.After(time.Second):
		}
	}
}

// addDebugContainer adds the ephemeral container name, of the XtraBackup
// image, idle, to pod, and waits for it to run. target, when set, is the
// container whose processes it sees; dataMount mounts the pod's data
// volume at /replica.
func (p *sqlProvisioner) addDebugContainer(ctx context.Context, pod, name, image, target string, dataMount bool) error {
	c := map[string]any{"name": name, "image": image, "command": []string{"sleep", "86400"},
		"securityContext": map[string]any{"runAsUser": 0, "capabilities": map[string]any{"add": []string{"SYS_PTRACE", "DAC_READ_SEARCH"}}}}
	if target != "" {
		c["targetContainerName"] = target
	}
	if dataMount {
		c["volumeMounts"] = []map[string]any{{"name": "data", "mountPath": "/replica"}}
	}
	patch, err := json.Marshal(map[string]any{"spec": map[string]any{"ephemeralContainers": []any{c}}})
	if err != nil {
		return err
	}
	if _, err := p.inst.Kubectl(ctx, "-n", p.inst.Namespace, "patch", "pod", pod, "--subresource=ephemeralcontainers",
		"--type=strategic", "-p", string(patch)); err != nil {
		return fmt.Errorf("add the XtraBackup container to %s: %w", pod, err)
	}
	for {
		out, _ := p.inst.Kubectl(ctx, "-n", p.inst.Namespace, "get", "pod", pod, "-o",
			`jsonpath={.status.ephemeralContainerStatuses[?(@.name=="`+name+`")].state}`)
		switch {
		case strings.Contains(out, "running"):
			return nil
		case strings.Contains(out, "terminated"):
			return fmt.Errorf("the XtraBackup container of %s ended: %s", pod, out)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("the XtraBackup container of %s did not start (%s): %w", pod, out, ctx.Err())
		case <-time.After(2 * time.Second):
		}
	}
}

// execContainer runs argv in container of pod.
func (p *sqlProvisioner) execContainer(ctx context.Context, pod, container string, stdin io.Reader, stdout io.Writer, argv ...string) error {
	kube := kubeRunner(p.cfg.KubeconfigPath(), p.cfg.Cluster.Namespace)
	args := []string{"exec"}
	if stdin != nil {
		args = append(args, "-i")
	}
	args = append(append(args, pod, "-c", container, "--"), argv...)
	var stderr bytes.Buffer
	if err := kube.Pipe(ctx, stdin, stdout, &stderr, args...); err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(tail(stderr.String(), 2000)))
	}
	return nil
}

func (p *sqlProvisioner) physicalRestoreEphemeral(ctx context.Context, spec sqladmin.Spec, b sqladmin.PhysicalBackup) error {
	ext := spec.External
	deploy := nodeName(spec.ServerName(), spec.PrimaryNode())
	take := len(b.Files) == 0
	srcApp := ""
	if take {
		if ext.Instance == "" {
			return fmt.Errorf("the source is not a CloudBurrow instance's server, and dumpFilePath names no backup")
		}
		// The source's Service selects its primary's server.
		svc := primaryHost(spec)
		app, err := p.inst.Kubectl(ctx, "-n", p.inst.Namespace, "get", "service", svc, "-o", "jsonpath={.spec.selector.app}")
		if err != nil || strings.TrimSpace(app) == "" {
			return fmt.Errorf("the source's server %s: %v", svc, err)
		}
		srcApp = strings.TrimSpace(app)
	}
	// The replica's server, started on its volume with physicalWrapper.
	if err := p.inst.Apply(ctx, p.nodeBackend(spec, spec.PrimaryNode()).Manifest(p.inst.Namespace, p.inst.Instance)); err != nil {
		return err
	}
	if err := p.rolloutDeployment(ctx, deploy); err != nil {
		return err
	}
	pod, server, err := p.podOf(ctx, deploy, "data")
	if err != nil {
		return err
	}
	image := components.XtraBackupImage(spec.DatabaseVersion)
	xb := fmt.Sprintf("xtrabackup-%d", time.Now().Unix())
	if err := p.addDebugContainer(ctx, pod, xb, image, "", true); err != nil {
		return err
	}
	dir := "/replica/" + physicalBackupDir
	var out bytes.Buffer
	if err := p.execContainer(ctx, pod, xb, nil, &out, "sh", "-c", `rm -rf "$1" && mkdir -p "$1"`, "sh", dir); err != nil {
		return fmt.Errorf("make the backup's folder: %w %s", err, out.String())
	}
	if take {
		spod, scont, err := p.podOf(ctx, srcApp, "")
		if err != nil {
			return err
		}
		sxb := xb + "-src"
		if err := p.addDebugContainer(ctx, spod, sxb, image, scont, false); err != nil {
			return err
		}
		pr, pw := io.Pipe()
		var rout bytes.Buffer
		done := make(chan error, 1)
		go func() {
			err := p.execContainer(ctx, spod, sxb, nil, pw, "bash", "-c", physicalStreamScript, "bash", ext.User, ext.Password)
			pw.CloseWithError(err)
			done <- err
		}()
		rerr := p.execContainer(ctx, pod, xb, pr, &rout, "xbstream", "-x", "-C", dir)
		pr.CloseWithError(rerr)
		if err := <-done; err != nil {
			return fmt.Errorf("take the backup of the source: %w", err)
		}
		if rerr != nil {
			return fmt.Errorf("unpack the backup on the replica: %w %s", rerr, rout.String())
		}
	} else {
		for _, f := range b.Files {
			clean := path.Clean("/" + f)
			if clean == "/" {
				continue
			}
			data, err := b.Read(ctx, f)
			if err != nil {
				return fmt.Errorf("read %s: %w", f, err)
			}
			out.Reset()
			if err := p.execContainer(ctx, pod, xb, bytes.NewReader(data), &out, "sh", "-c", `mkdir -p "$(dirname "$1")" && cat > "$1"`,
				"sh", dir+clean); err != nil {
				return fmt.Errorf("write %s: %w %s", f, err, strings.TrimSpace(out.String()))
			}
		}
	}
	out.Reset()
	env := []string{"env", "CLOUDSQL_INIT_SQL=" + physicalInitSQL(spec.DatabaseVersion, spec.RootPassword)}
	if take {
		env = append(env, "CLOUDSQL_TAKEN=1")
	}
	if err := p.execContainer(ctx, pod, xb, nil, &out, append(env, "bash", "-c", physicalEphemeralScript)...); err != nil {
		return fmt.Errorf("prepare the backup: %w %s", err, strings.TrimSpace(tail(out.String(), 2000)))
	}
	// mysqld stops; its container starts again on the same volume and
	// swaps the restored copy in.
	out.Reset()
	if err := p.execContainer(ctx, pod, server, nil, &out, "sh", "-c", "kill -TERM 1"); err != nil {
		return fmt.Errorf("stop the replica's server: %w %s", err, out.String())
	}
	// The wait is bounded on its own, well inside the operation's: a
	// server that does not come up on the restored data fails the sync in
	// minutes, with its own log, instead of silently holding the operation
	// (nightly run 44342114886, #1813: 20 minutes, and no log of why).
	wctx, cancel := context.WithTimeout(ctx, physicalStartTimeout)
	defer cancel()
	for {
		out.Reset()
		// mysqladmin ping succeeds once the server answers, even when it
		// refuses the user. It pings over TCP, as mysqlRootEnv's clients
		// do: on CI's runners the server listened on
		// /var/lib/mysql/mysql.sock and the client's default socket,
		// /var/run/mysqld/mysqld.sock, was missing, so a ping over it never
		// answered and the sync waited out physicalStartTimeout on a
		// server that was up a second after its restart (nightly runs
		// 44562270227 and 44766672852, #1813).
		err := p.execContainer(wctx, pod, server, nil, &out, "sh", "-c", "test ! -e /var/lib/mysql/"+physicalSwapMarker+
			" && mysqladmin -h127.0.0.1 -P3306 --protocol=TCP --connect-timeout=2 ping >/dev/null 2>&1")
		if err == nil {
			return nil
		}
		select {
		case <-wctx.Done():
			return fmt.Errorf("the replica's server did not start on the restored data within %s: %w\n%s",
				physicalStartTimeout, wctx.Err(), p.serverReport(ctx, pod, server))
		case <-time.After(2 * time.Second):
		}
	}
}

// physicalStartTimeout bounds the wait for the replica's server to answer
// on the restored data after its restart. Measured 2026-11-18 on the
// compat test's backup: about a second.
const physicalStartTimeout = 10 * time.Minute

// serverReport is what the pod's server container says about itself: its
// state, its restarts, whether the swap marker is still there, and the
// tail of its log (and of its previous run's, after a restart), for an
// error that otherwise would not say why the server did not come up.
func (p *sqlProvisioner) serverReport(ctx context.Context, pod, server string) string {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	var sb strings.Builder
	state, err := p.inst.Kubectl(ctx, "-n", p.inst.Namespace, "get", "pod", pod, "-o",
		`jsonpath={.status.containerStatuses[?(@.name=="`+server+`")]}`)
	fmt.Fprintf(&sb, "container %s of %s: %s", server, pod, strings.TrimSpace(state))
	if err != nil {
		fmt.Fprintf(&sb, " (%v)", err)
	}
	var out bytes.Buffer
	if err := p.execContainer(ctx, pod, server, nil, &out, "sh", "-c",
		"cd /var/lib/mysql && ls -la . && ls data | head -50"); err == nil {
		fmt.Fprintf(&sb, "\n/var/lib/mysql:\n%s", strings.TrimSpace(tail(out.String(), 3000)))
	}
	for _, prev := range []bool{true, false} {
		args := []string{"-n", p.inst.Namespace, "logs", pod, "-c", server, "--tail=60"}
		label := "log"
		if prev {
			args, label = append(args, "--previous"), "previous log"
		}
		if l, err := p.inst.Kubectl(ctx, args...); err == nil && strings.TrimSpace(l) != "" {
			fmt.Fprintf(&sb, "\n%s:\n%s", label, strings.TrimSpace(tail(l, 6000)))
		}
	}
	return sb.String()
}
