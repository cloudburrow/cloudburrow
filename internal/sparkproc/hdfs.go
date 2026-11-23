package sparkproc

// A Dataproc cluster's HDFS (#2186), as a Dataproc image runs it: the
// NameNode on the master (RPC CLUSTER-m:8020, web UI 9870, "HDFS
// NameNode" on the Component Gateway) and a DataNode on each primary
// worker (on the master of a single node cluster), each in a container of
// the official apache/hive image (Apache Hadoop 3.3.6, by digest: the
// Hadoop the cluster's YARN, Hive and Tez run on) sharing its node's
// network, with fs.defaultFS hdfs://CLUSTER-m for every node, driver and
// job. The NameNode's metadata and the DataNodes' blocks are on their
// node's own disk (the container's file system, at Dataproc's
// /hadoop/dfs/name and /hadoop/dfs/data), each DataNode's capacity the
// node's boot disk size: so HDFS keeps its data while the cluster is
// stopped and started again, and loses it when the cluster is deleted, as
// a Dataproc cluster's persistent disks do. The cluster's hdfs:
// properties are hdfs-site.xml's.
//
// What Dataproc keeps in HDFS is there: YARN's MapReduce staging and
// JobHistory directories (/tmp/hadoop-yarn/staging), a Spark job's
// staging directory (/user/USER/.sparkStaging), Tez's and Hive's scratch
// directories, and the Hive warehouse (/user/hive/warehouse).
//
// Scaling the primary workers down decommissions each removed node's
// DataNode first (dfs.hosts.exclude, then dfsadmin -refreshNodes), so its
// blocks are replicated to the nodes that stay.
//
// With the RANGER component, the NameNode loads Ranger's HDFS plugin as
// enable-hdfs-plugin.sh installs it: RangerHdfsAuthorizer the inode
// attribute provider, its policies the hadoop-dataproc service's, polled
// from the master's Ranger Admin, its audit to Solr's ranger_audits.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// The NameNode's RPC and web ports on the master.
const (
	nameNodePort    = "8020"
	nameNodeWebPort = "9870"
	journalNodePort = "8485"
)

// DefaultBootDiskGB is a node's boot disk size when its disk config sets
// none (DiskConfig.boot_disk_size_gb: "default is 500GB").
const DefaultBootDiskGB = 500

// rangerHdfsPlugin is where the Ranger Admin image has the HDFS plugin's
// lib directory.
const rangerHdfsPlugin = "/opt/ranger/ranger-2.8.0-hdfs-plugin/lib"

// hdfsEnabled says whether the cluster runs HDFS: every cluster that runs
// YARN.
func (c *Cluster) hdfsEnabled() bool { return c.yarnEnabled() }

// ha says whether the cluster is a High Availability cluster: three
// masters, CLUSTER-m-0, -m-1 and -m-2.
func (c *Cluster) ha() bool { return c.spec.Masters == 3 }

// masterAlias is master i's host name: CLUSTER-m, or CLUSTER-m-i on a High
// Availability cluster.
func (c *Cluster) masterAlias(i int) string {
	if !c.ha() {
		return c.spec.Name + "-m"
	}
	return fmt.Sprintf("%s-m-%d", c.spec.Name, i)
}

// masterNode is master i's node container: the master's own, or the
// container another master of a High Availability cluster is.
func (c *Cluster) masterNode(i int) string {
	if i == 0 {
		return c.master
	}
	return fmt.Sprintf("%s-%d", c.master, i)
}

// masterPart is the component name of master i's daemon name (its
// container component(masterPart(i, name))).
func masterPart(i int, name string) string {
	if i == 0 {
		return name
	}
	return fmt.Sprintf("%d-%s", i, name)
}

// nodeLabel labels a container with the node (host name) it is part of,
// so a node's containers stop and start together, as a VM's daemons do.
func nodeLabel(alias string) []string {
	return []string{"--label", "cloudburrow.dataproc.node=" + alias}
}

// The High Availability NameNodes' IDs in the cluster's nameservice.
var haNameNodes = []string{"nn0", "nn1"}

// defaultFS is the cluster's fs.defaultFS: the NameNode on the master,
// or a High Availability cluster's nameservice (hdfs://CLUSTER).
func (c *Cluster) defaultFS() string {
	if !c.hdfsEnabled() {
		return "file:///"
	}
	if c.ha() {
		return "hdfs://" + c.spec.Name
	}
	return "hdfs://" + c.nodeAlias(-1)
}

// zkQuorum is a High Availability cluster's ZooKeeper ensemble, one
// server on each master.
func (c *Cluster) zkQuorum() string {
	var q []string
	for i := 0; i < 3; i++ {
		q = append(q, c.masterAlias(i)+":2181")
	}
	return strings.Join(q, ",")
}

// replication is dfs.replication as Dataproc sets it: 2, or 1 when the
// cluster has fewer than two primary workers.
func (c *Cluster) replication() int {
	if c.spec.Workers < 2 {
		return 1
	}
	return 2
}

// hdfsSite is the cluster's hdfs-site.xml: the NameNode on the master
// (on a High Availability cluster, the nameservice CLUSTER of NameNodes
// nn0 on -m-0 and nn1 on -m-1, their edits on the JournalNodes of the
// three masters, failing over automatically through ZooKeeper), the name
// and data directories at Dataproc's paths, dfs.replication, the
// excluded (decommissioned) hosts' file, the Ranger plugin's properties
// with RANGER, and the cluster's hdfs: properties over them.
func (c *Cluster) hdfsSite() map[string]string {
	master := c.nodeAlias(-1)
	props := map[string]string{
		"dfs.namenode.rpc-address":                             master + ":" + nameNodePort,
		"dfs.namenode.rpc-bind-host":                           "0.0.0.0",
		"dfs.namenode.servicerpc-bind-host":                    "0.0.0.0",
		"dfs.namenode.http-address":                            master + ":" + nameNodeWebPort,
		"dfs.namenode.http-bind-host":                          "0.0.0.0",
		"dfs.namenode.name.dir":                                "file:///hadoop/dfs/name",
		"dfs.datanode.data.dir":                                "file:///hadoop/dfs/data",
		"dfs.replication":                                      strconv.Itoa(c.replication()),
		"dfs.hosts.exclude":                                    "/cb-conf/nodes_exclude",
		"dfs.namenode.datanode.registration.ip-hostname-check": "false",
		"dfs.namenode.safemode.extension":                      "5000",
		"dfs.namenode.decommission.interval":                   "5s",
		// HDFS checks no permissions, as a Dataproc image's hdfs-site.xml
		// has it, also with Kerberos (Google's kerberized data lake
		// example, GoogleCloudPlatform/professional-services
		// examples/kerberized_data_lake, sets dfs.permissions.enabled true
		// itself in an initialization action), unless Ranger's plugin is
		// installed (below).
		"dfs.permissions.enabled": "false",
	}
	if c.ha() {
		ns := c.spec.Name
		delete(props, "dfs.namenode.rpc-address")
		delete(props, "dfs.namenode.http-address")
		var journals []string
		for i := 0; i < 3; i++ {
			journals = append(journals, c.masterAlias(i)+":"+journalNodePort)
		}
		props["dfs.nameservices"] = ns
		props["dfs.ha.namenodes."+ns] = strings.Join(haNameNodes, ",")
		for i, id := range haNameNodes {
			props["dfs.namenode.rpc-address."+ns+"."+id] = c.masterAlias(i) + ":" + nameNodePort
			props["dfs.namenode.http-address."+ns+"."+id] = c.masterAlias(i) + ":" + nameNodeWebPort
		}
		props["dfs.namenode.shared.edits.dir"] = "qjournal://" + strings.Join(journals, ";") + "/" + ns
		props["dfs.journalnode.edits.dir"] = "/hadoop/dfs/journalnode"
		props["dfs.client.failover.proxy.provider."+ns] = "org.apache.hadoop.hdfs.server.namenode.ha.ConfiguredFailoverProxyProvider"
		props["dfs.ha.automatic-failover.enabled"] = "true"
		// A stopped master cannot be reached to fence it; the
		// JournalNodes' quorum keeps a second writer out.
		props["dfs.ha.fencing.methods"] = "shell(/bin/true)"
	}
	if c.has(ComponentRanger) {
		// enable-hdfs-plugin.sh's hdfs-site-changes.cfg.
		props["dfs.permissions.enabled"] = "true"
		props["dfs.permissions"] = "true"
		props["dfs.namenode.inode.attributes.provider.class"] = "org.apache.ranger.authorization.hadoop.RangerHdfsAuthorizer"
		props["dfs.permissions.ContentSummary.subAccess"] = "true"
	}
	for k, v := range c.confProps("hdfs") {
		props[k] = v
	}
	return props
}

// rangerHdfsConf is the NameNode's configuration for Ranger's HDFS
// plugin, as enable-hdfs-plugin.sh writes it: the plugin's service
// (hadoop-dataproc), the Admin server on the master's 6080, and the audit
// log to Solr's ranger_audits. An access no Ranger policy decides falls
// back to HDFS's own permissions (xasecure.add-hadoop-authorization).
func (c *Cluster) rangerHdfsConf() map[string]map[string]string {
	return map[string]map[string]string{
		"ranger-hdfs-security.xml": {
			"ranger.plugin.hdfs.service.name":                            "hadoop-dataproc",
			"ranger.plugin.hdfs.policy.source.impl":                      "org.apache.ranger.admin.client.RangerAdminRESTClient",
			"ranger.plugin.hdfs.policy.rest.url":                         "http://" + c.nodeAlias(-1) + ":6080",
			"ranger.plugin.hdfs.policy.pollIntervalMs":                   "5000",
			"ranger.plugin.hdfs.policy.cache.dir":                        "/tmp/ranger-hdfs-policycache",
			"ranger.plugin.hdfs.policy.rest.client.connection.timeoutMs": "120000",
			"ranger.plugin.hdfs.policy.rest.client.read.timeoutMs":       "30000",
			"xasecure.add-hadoop-authorization":                          "true",
		},
		"ranger-hdfs-audit.xml": {
			"xasecure.audit.is.enabled":                           "true",
			"xasecure.audit.destination.solr":                     "true",
			"xasecure.audit.destination.solr.urls":                "http://localhost:8983/solr/ranger_audits",
			"xasecure.audit.destination.solr.batch.filespool.dir": "/tmp/ranger-hdfs-audit-spool",
		},
	}
}

// hdfsMounts are the mounts of an HDFS daemon's container on the node
// alias: the cluster's configuration, the Cloud Storage connector, the
// daemons' script, and the node's disk at /hadoop.
func (c *Cluster) hdfsMounts(gcs, alias string) []string {
	return []string{"-v", filepath.Join(c.dir, "conf") + ":/cb-conf:ro", "-v", gcs + ":/cb-gcs/gcs-connector.jar:ro",
		"-v", filepath.Join(c.dir, "hdfs.sh") + ":/cb/hdfs.sh:ro", "-v", c.disk(alias) + ":/hadoop"}
}

// nameNode is the NameNode's container (nn0's on a High Availability
// cluster).
func (c *Cluster) nameNode() string { return c.component("hdfs") }

// nameNodeOf is master i's NameNode container.
func (c *Cluster) nameNodeOf(i int) string { return c.component(masterPart(i, "hdfs")) }

// nameNodeUp is the line a NameNode logs once its RPC server listens.
var nameNodeUp = regexp.MustCompile(`IPC Server listener on ` + nameNodePort)

// startHDFS starts the NameNode on the master and waits for its RPC
// server; on a High Availability cluster, the other two masters' nodes,
// a ZooKeeper server and a JournalNode on each master, and the NameNodes
// on -m-0 and -m-1, each with its ZKFailoverController. The cluster's
// HDFS is ready (waitHDFS) once the DataNodes report.
func (c *Cluster) startHDFS(ctx context.Context) error {
	e := c.d.Ecosystem
	gcs, err := e.GCS(ctx)
	if err != nil {
		return &Unavailable{Msg: "Google's Cloud Storage connector for Hadoop is not available: " + err.Error()}
	}
	var ranger []string
	if c.has(ComponentRanger) {
		if e.Ranger == nil {
			return &Unavailable{Msg: "the Spark runner serves no Ranger Admin image"}
		}
		image, err := e.Ranger(ctx)
		if err != nil {
			return &Unavailable{Msg: "build the Ranger Admin image from the Apache Ranger 2.8.0 source release: " + err.Error()}
		}
		ranger = []string{"--mount", "type=image,source=" + image + ",target=/cb-ranger", "-e", "CB_RANGER_HDFS=/cb-ranger" + rangerHdfsPlugin}
	}
	nameNodes := 1
	if c.ha() {
		nameNodes = len(haNameNodes)
		if err := c.startHAQuorum(ctx, gcs); err != nil {
			return err
		}
	}
	for i := 0; i < nameNodes; i++ {
		alias := c.masterAlias(i)
		if err := os.MkdirAll(c.disk(alias), 0o755); err != nil {
			return err
		}
		args := append(append(append([]string{"--user", "0:0"}, nodeLabel(alias)...), c.hdfsMounts(gcs, alias)...), ranger...)
		args = append(args, "-e", "CB_CLUSTER_ID=CID-"+c.spec.ID)
		if c.ha() {
			role := "active"
			if i > 0 {
				role = "standby"
			}
			args = append(args, "-e", "CB_HA="+role)
		}
		args = append(args, "--entrypoint", "/bin/bash", e.Hive, "/cb/hdfs.sh", "namenode")
		if err := c.runComponent(ctx, masterPart(i, "hdfs"), c.masterNode(i), args...); err != nil {
			return err
		}
		if err := c.waitLog(ctx, c.nameNodeOf(i), nameNodeUp, 5*time.Minute); err != nil {
			return err
		}
	}
	return nil
}

// waitHDFS waits for the NameNode to be out of safe mode and to have
// made the cluster's HDFS directories.
func (c *Cluster) waitHDFS(ctx context.Context) error {
	return c.waitLog(ctx, c.nameNode(), regexp.MustCompile(`CLOUDBURROW HDFS READY`), 5*time.Minute)
}

// startHAQuorum starts what a High Availability cluster's NameNodes
// need: the -m-1 and -m-2 nodes, and on each master a ZooKeeper server
// (the ensemble ZKFC elects the active NameNode through, and the
// ZOOKEEPER component) and a JournalNode.
func (c *Cluster) startHAQuorum(ctx context.Context, gcs string) error {
	e := c.d.Ecosystem
	if e.Zookeeper == "" {
		return &Unavailable{Msg: "the Spark runner serves no ZooKeeper image, which a High Availability cluster's masters run"}
	}
	for i := 1; i < 3; i++ {
		alias := c.masterAlias(i)
		args := append([]string{"run", "-d", "--name", c.masterNode(i), "--hostname", alias, "--network-alias", alias,
			"--label", c.label, "--label", "cloudburrow.dataproc.cluster=" + c.spec.ID}, nodeLabel(alias)...)
		args = append(args, "--network", c.network, "--entrypoint", "sleep", e.Hive, "infinity")
		if out, err := c.d.docker(ctx, args...); err != nil {
			return &Unavailable{Msg: "start the cluster's node " + alias + ": " + out}
		}
		c.mu.Lock()
		c.masters = append(c.masters, c.masterNode(i))
		c.mu.Unlock()
	}
	var servers []string
	for i := 0; i < 3; i++ {
		servers = append(servers, fmt.Sprintf("server.%d=%s:2888:3888;2181", i+1, c.masterAlias(i)))
	}
	for i := 0; i < 3; i++ {
		args := append(nodeLabel(c.masterAlias(i)), "-e", fmt.Sprintf("ZOO_MY_ID=%d", i+1), "-e", "ZOO_SERVERS="+strings.Join(servers, " "),
			"-e", "ZOO_CFG_EXTRA=quorumListenOnAllIPs=true", "-e", "ZOO_4LW_COMMANDS_WHITELIST=ruok,stat,srvr,mntr", e.Zookeeper)
		if err := c.runComponent(ctx, masterPart(i, "zookeeper"), c.masterNode(i), args...); err != nil {
			return err
		}
	}
	for i := 0; i < 3; i++ {
		alias := c.masterAlias(i)
		if err := os.MkdirAll(c.disk(alias), 0o755); err != nil {
			return err
		}
		args := append(append([]string{"--user", "0:0"}, nodeLabel(alias)...), c.hdfsMounts(gcs, alias)...)
		args = append(args, "--entrypoint", "/bin/bash", e.Hive, "/cb/hdfs.sh", "journalnode")
		if err := c.runComponent(ctx, masterPart(i, "journalnode"), c.masterNode(i), args...); err != nil {
			return err
		}
	}
	for i := 0; i < 3; i++ {
		if err := c.waitLog(ctx, c.component(masterPart(i, "zookeeper")), regexp.MustCompile(`LEADING|FOLLOWING`), 3*time.Minute); err != nil {
			return err
		}
		if err := c.waitLog(ctx, c.component(masterPart(i, "journalnode")), regexp.MustCompile(`IPC Server listener on `+journalNodePort), 3*time.Minute); err != nil {
			return err
		}
	}
	return nil
}

// hdfsFirstLocked are the containers a stopped cluster's HDFS starts
// from, in order: the nodes, the ZooKeeper servers and JournalNodes of a
// High Availability cluster, the NameNodes, and the DataNodes.
func (c *Cluster) hdfsFirstLocked() []string {
	out := append(append([]string{c.relay, c.master}, c.masters...), c.workers...)
	if c.ha() {
		for i := 0; i < 3; i++ {
			out = append(out, c.component(masterPart(i, "zookeeper")), c.component(masterPart(i, "journalnode")))
		}
		for i := range haNameNodes {
			out = append(out, c.nameNodeOf(i))
		}
	} else {
		out = append(out, c.nameNode())
	}
	return append(out, c.dataNodesLocked()...)
}

// dataNode is the DataNode container of the node container node.
func dataNode(node string) string { return node + "-dn" }

// localSSDGB is a local SSD's fixed size: "The 375GB size of each local
// SSD is fixed" (Dataproc's local SSDs page).
const localSSDGB = 375

// localSSDs is the number of local SSDs the node of a DataNode has
// mounted for HDFS and YARN's scratch: the primary workers'
// numLocalSsds (a single node cluster's master's), unless
// dataproc:dataproc.localssd.mount.enable is false.
func (c *Cluster) localSSDs() int {
	if c.spec.LocalSSDsUnmounted {
		return 0
	}
	return c.spec.LocalSSDs
}

// ssdMounts are the node alias's local SSDs, each at /mnt/N (N from 1),
// as Dataproc mounts them.
func (c *Cluster) ssdMounts(alias string) []string {
	var out []string
	for i := 1; i <= c.localSSDs(); i++ {
		dir := filepath.Join(c.disk(alias), fmt.Sprintf("local-ssd-%d", i))
		_ = os.MkdirAll(dir, 0o755)
		out = append(out, "-v", dir+fmt.Sprintf(":/mnt/%d", i))
	}
	return out
}

// startDataNode starts the DataNode of node (a primary worker's
// container, or a single node cluster's master): its blocks on the
// node's boot disk at /hadoop/dfs/data, its capacity the disk's size, or
// with local SSDs on each SSD at /mnt/N/hadoop/dfs/data, each volume
// 375 GB.
func (c *Cluster) startDataNode(ctx context.Context, node, host string) error {
	e := c.d.Ecosystem
	gcs, err := e.GCS(ctx)
	if err != nil {
		return &Unavailable{Msg: "Google's Cloud Storage connector for Hadoop is not available: " + err.Error()}
	}
	if err := os.MkdirAll(c.disk(host), 0o755); err != nil {
		return err
	}
	args := []string{"run", "-d", "--name", dataNode(node), "--label", c.label, "--label", "cloudburrow.dataproc.cluster=" + c.spec.ID,
		"--network", "container:" + node, "--user", "0:0"}
	args = append(append(append(args, nodeLabel(host)...), c.hdfsMounts(gcs, host)...), c.ssdMounts(host)...)
	args = append(args, "-e", "CB_HOSTNAME="+host)
	props := c.confProps("hdfs")
	if n := c.localSSDs(); n > 0 {
		if _, set := props["dfs.datanode.data.dir"]; !set {
			args = append(args, "-e", "CB_SSDS="+strconv.Itoa(n))
		}
	}
	if _, set := props["dfs.datanode.du.reserved"]; !set {
		disk := c.spec.BootDiskGB
		if disk <= 0 {
			disk = DefaultBootDiskGB
		}
		if c.localSSDs() > 0 {
			disk = localSSDGB
		}
		args = append(args, "-e", "CB_DISK_GB="+strconv.Itoa(disk))
	}
	args = append(args, "--entrypoint", "/bin/bash", e.Hive, "/cb/hdfs.sh", "datanode")
	if out, err := c.d.docker(ctx, args...); err != nil {
		return &Unavailable{Msg: "start the DataNode of " + node + ": " + out}
	}
	return nil
}

// nmLocalDirs is a NodeManager's yarn.nodemanager.local-dirs on the node
// alias: its local SSDs' /mnt/N/hadoop/yarn/nm-local-dir, as Dataproc
// keeps YARN's scratch (shuffle output) on them, or /tmp/yarn-local.
func (c *Cluster) nmLocalDirs() string {
	n := c.localSSDs()
	if n == 0 {
		return "/tmp/yarn-local"
	}
	var dirs []string
	for i := 1; i <= n; i++ {
		dirs = append(dirs, fmt.Sprintf("/mnt/%d/hadoop/yarn/nm-local-dir", i))
	}
	return strings.Join(dirs, ",")
}

// diskRoot is the directory of the instance's clusters' disks.
func (c *Cluster) diskRoot() string {
	if c.d.Disks != "" {
		return c.d.Disks
	}
	return filepath.Join(c.dir, "disks")
}

// disk is the persistent disk of the cluster's node alias: a directory
// of the instance's (Docker.Disks), so HDFS keeps its data across the
// cluster's stop and start and the instance's own, as a Compute Engine
// persistent disk does, until the node or the cluster is deleted.
func (c *Cluster) disk(alias string) string {
	return filepath.Join(c.diskRoot(), filepath.Base(c.spec.ID), alias)
}

// RemoveDisks removes cluster id's disks ("": every cluster's).
func (d *Docker) RemoveDisks(id string) {
	if d.Disks == "" {
		return
	}
	if id == "" {
		d.removeDir(d.Disks)
		return
	}
	d.removeDir(filepath.Join(d.Disks, filepath.Base(id)))
}

// removeDir removes a disk's directory. The HDFS daemons and the
// NodeManagers run as root (Hadoop's login needs a user the image's
// passwd names), so where the daemon maps no user to the host's (Docker
// Engine on Linux) their files are root's: a container of the Hive image
// removes those.
func (d *Docker) removeDir(dir string) {
	if os.RemoveAll(dir) == nil {
		return
	}
	if d.Ecosystem == nil || d.Ecosystem.Hive == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	_, _ = d.docker(ctx, "run", "--rm", "--user", "0:0", "-v", dir+":/cb-disk", "--entrypoint", "find", d.Ecosystem.Hive,
		"/cb-disk", "-mindepth", "1", "-delete")
	_ = os.RemoveAll(dir)
}

// dataNodeUp is the line a DataNode logs once the NameNode registered it.
var dataNodeUp = regexp.MustCompile(`successfully registered with NN`)

// waitDataNodes waits for the DataNodes of nodes to register with the
// NameNode.
func (c *Cluster) waitDataNodes(ctx context.Context, nodes []string) error {
	for _, n := range nodes {
		if err := c.waitLog(ctx, dataNode(n), dataNodeUp, 3*time.Minute); err != nil {
			return err
		}
	}
	return nil
}

// dataNodesLocked are the DataNode containers.
func (c *Cluster) dataNodesLocked() []string {
	if !c.hdfsEnabled() {
		return nil
	}
	var out []string
	for _, n := range c.yarnNodesLocked() {
		out = append(out, dataNode(n))
	}
	return out
}

// decommissionLocked decommissions the DataNodes of hosts (the workers'
// host names) before their nodes are removed: the hosts in the NameNode's
// dfs.hosts.exclude file and the NameNode refreshed, then waiting (at
// most max) for each to be Decommissioned, its blocks replicated to the
// DataNodes that stay.
func (c *Cluster) decommissionLocked(ctx context.Context, hosts []string, max time.Duration) error {
	file := filepath.Join(c.dir, "conf", "nodes_exclude")
	if err := os.WriteFile(file, []byte(strings.Join(hosts, "\n")+"\n"), 0o644); err != nil {
		return err
	}
	if out, err := c.d.docker(ctx, "exec", "-e", "HADOOP_USER_NAME=hdfs", "-e", "HADOOP_CONF_DIR=/cb-conf", c.nameNode(), "/opt/hadoop/bin/hdfs", "dfsadmin", "-refreshNodes"); err != nil {
		return &Unavailable{Msg: "refresh the NameNode's excluded hosts: " + out}
	}
	deadline := time.Now().Add(max)
	for {
		out, _ := c.d.docker(ctx, "exec", "-e", "HADOOP_USER_NAME=hdfs", "-e", "HADOOP_CONF_DIR=/cb-conf", c.nameNode(), "/opt/hadoop/bin/hdfs", "dfsadmin", "-report")
		if allDecommissioned(out, hosts) || time.Now().After(deadline) || ctx.Err() != nil {
			return nil
		}
		time.Sleep(2 * time.Second)
	}
}

// allDecommissioned says whether dfsadmin -report's output shows each of
// hosts Decommissioned (or no longer reported).
func allDecommissioned(report string, hosts []string) bool {
	status := map[string]string{}
	host := ""
	for _, line := range strings.Split(report, "\n") {
		line = strings.TrimSpace(line)
		if v, ok := strings.CutPrefix(line, "Hostname:"); ok {
			host = strings.TrimSpace(v)
		}
		if v, ok := strings.CutPrefix(line, "Decommission Status :"); ok && host != "" {
			status[host] = strings.TrimSpace(v)
		}
	}
	for _, h := range hosts {
		if s, ok := status[h]; ok && s != "Decommissioned" {
			return false
		}
	}
	return true
}

// clearExcludedLocked empties the excluded hosts once their nodes are
// gone, so a node added again under the same name joins HDFS.
func (c *Cluster) clearExcludedLocked(ctx context.Context) {
	_ = os.WriteFile(filepath.Join(c.dir, "conf", "nodes_exclude"), nil, 0o644)
	_, _ = c.d.docker(ctx, "exec", "-e", "HADOOP_USER_NAME=hdfs", "-e", "HADOOP_CONF_DIR=/cb-conf", c.nameNode(), "/opt/hadoop/bin/hdfs", "dfsadmin", "-refreshNodes")
}
