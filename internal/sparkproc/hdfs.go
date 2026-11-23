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

// defaultFS is the cluster's fs.defaultFS.
func (c *Cluster) defaultFS() string {
	if !c.hdfsEnabled() {
		return "file:///"
	}
	return "hdfs://" + c.nodeAlias(-1)
}

// replication is dfs.replication as Dataproc sets it: 2, or 1 when the
// cluster has fewer than two primary workers.
func (c *Cluster) replication() int {
	if c.spec.Workers < 2 {
		return 1
	}
	return 2
}

// hdfsSite is the cluster's hdfs-site.xml: the NameNode on the master,
// the name and data directories at Dataproc's paths, dfs.replication,
// the excluded (decommissioned) hosts' file, the Ranger plugin's
// properties with RANGER, and the cluster's hdfs: properties over them.
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

// hdfsMounts are the mounts of an HDFS daemon's container.
func (c *Cluster) hdfsMounts(gcs string) []string {
	return []string{"-v", filepath.Join(c.dir, "conf") + ":/cb-conf:ro", "-v", gcs + ":/cb-gcs/gcs-connector.jar:ro",
		"-v", filepath.Join(c.dir, "hdfs.sh") + ":/cb/hdfs.sh:ro"}
}

// nameNode is the NameNode's container.
func (c *Cluster) nameNode() string { return c.component("hdfs") }

// startHDFS starts the NameNode on the master and waits for it to make
// the cluster's HDFS directories.
func (c *Cluster) startHDFS(ctx context.Context) error {
	e := c.d.Ecosystem
	gcs, err := e.GCS(ctx)
	if err != nil {
		return &Unavailable{Msg: "Google's Cloud Storage connector for Hadoop is not available: " + err.Error()}
	}
	args := append([]string{"--user", "0:0"}, c.hdfsMounts(gcs)...)
	if c.has(ComponentRanger) {
		if e.Ranger == nil {
			return &Unavailable{Msg: "the Spark runner serves no Ranger Admin image"}
		}
		image, err := e.Ranger(ctx)
		if err != nil {
			return &Unavailable{Msg: "build the Ranger Admin image from the Apache Ranger 2.8.0 source release: " + err.Error()}
		}
		args = append(args, "--mount", "type=image,source="+image+",target=/cb-ranger", "-e", "CB_RANGER_HDFS=/cb-ranger"+rangerHdfsPlugin)
	}
	args = append(args, "-e", "CB_CLUSTER_ID=CID-"+c.spec.ID, "--entrypoint", "/bin/bash", e.Hive, "/cb/hdfs.sh", "namenode")
	if err := c.runComponent(ctx, "hdfs", "", args...); err != nil {
		return err
	}
	return c.waitLog(ctx, c.nameNode(), regexp.MustCompile(`CLOUDBURROW HDFS READY`), 5*time.Minute)
}

// dataNode is the DataNode container of the node container node.
func dataNode(node string) string { return node + "-dn" }

// startDataNode starts the DataNode of node (a primary worker's
// container, or a single node cluster's master).
func (c *Cluster) startDataNode(ctx context.Context, node, host string) error {
	e := c.d.Ecosystem
	gcs, err := e.GCS(ctx)
	if err != nil {
		return &Unavailable{Msg: "Google's Cloud Storage connector for Hadoop is not available: " + err.Error()}
	}
	args := []string{"run", "-d", "--name", dataNode(node), "--label", c.label, "--label", "cloudburrow.dataproc.cluster=" + c.spec.ID,
		"--network", "container:" + node, "--user", "0:0"}
	args = append(args, c.hdfsMounts(gcs)...)
	args = append(args, "-e", "CB_HOSTNAME="+host)
	if _, set := c.confProps("hdfs")["dfs.datanode.du.reserved"]; !set {
		disk := c.spec.BootDiskGB
		if disk <= 0 {
			disk = DefaultBootDiskGB
		}
		args = append(args, "-e", "CB_DISK_GB="+strconv.Itoa(disk))
	}
	args = append(args, "--entrypoint", "/bin/bash", e.Hive, "/cb/hdfs.sh", "datanode")
	if out, err := c.d.docker(ctx, args...); err != nil {
		return &Unavailable{Msg: "start the DataNode of " + node + ": " + out}
	}
	return nil
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
	if out, err := c.d.docker(ctx, "exec", "-e", "HADOOP_USER_NAME=hdfs", c.nameNode(), "/opt/hadoop/bin/hdfs", "dfsadmin", "-refreshNodes"); err != nil {
		return &Unavailable{Msg: "refresh the NameNode's excluded hosts: " + out}
	}
	deadline := time.Now().Add(max)
	for {
		out, _ := c.d.docker(ctx, "exec", "-e", "HADOOP_USER_NAME=hdfs", c.nameNode(), "/opt/hadoop/bin/hdfs", "dfsadmin", "-report")
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
	_, _ = c.d.docker(ctx, "exec", "-e", "HADOOP_USER_NAME=hdfs", c.nameNode(), "/opt/hadoop/bin/hdfs", "dfsadmin", "-refreshNodes")
}
