package main

// Cloud Run: the service account, Secret Manager files, in-memory, NFS and Cloud Storage
// (#1448) volumes on the deploy and job forms, and their edit forms (#1297).
//
// The adapter mounts a Secret Manager volume from the Kubernetes Secret
// holding the secret's versions and an in-memory emptyDir as a tmpfs, and
// stores and returns the service account (docs/compatibility.md:
// TestRunServiceVolumesAndServiceAccount, TestRunJobMountsSecretAndMemoryVolumes).

import (
	"fmt"
	"path"
	"strings"

	runpb "cloud.google.com/go/run/apiv2/runpb"

	runadapter "github.com/cloudburrow/cloudburrow/internal/adapter/run"
	"github.com/cloudburrow/cloudburrow/internal/console"
)

// runAccountText is the account a Cloud Run workload runs as, which its
// metadata server names to it (#1388): the template's serviceAccount, or the
// project's Compute Engine default account when none is set.
func runAccountText(serviceAccount, project string) string {
	if serviceAccount != "" {
		return serviceAccount
	}
	if project == "" {
		return ""
	}
	return runadapter.DefaultComputeAccount(project) + " (Compute Engine default)"
}

// runRevisionAccount is runAccountText for a Knative revision template,
// whose annotations record the service account.
func runRevisionAccount(annotations map[string]string, project string) string {
	return runAccountText(runadapter.ReadPodConfig(annotations, 0).GetServiceAccount(), project)
}

// runVolumeFields are the service account and volume fields.
func runVolumeFields(section string) []console.Field {
	return []console.Field{
		{Name: "serviceAccount", Label: "Service account", Type: "text", Section: section,
			Help: "Optional. The email of the identity the workload runs as; empty is the project's Compute Engine " +
				"default account. The workload's metadata server names it, in its email and its identity tokens; " +
				"no permission is checked locally."},
		{Name: "secretFiles", Label: "Files from Secret Manager", Type: "map", Section: section,
			Help: "Optional. One /dir/file=secret per line, or /dir/file=secret:version; the version is latest " +
				"when not given. The file holds the version's payload, read-only. Files in one directory come from one secret."},
		{Name: "memoryVolumes", Label: "In-memory volumes", Type: "map", Section: section,
			Help: `Optional. One /mount/path=size per line, as Kubernetes quantities ("64Mi"), or /mount/path= for no limit. ` +
				"The volume is a writable tmpfs that counts toward the container's memory."},
		{Name: "cloudSqlInstances", Label: "Cloud SQL connections", Type: "text", Section: section,
			Help: "Optional. Comma-separated connection names, project:us-central1:postgres or project:us-central1:mysql, " +
				"as gcloud's --add-cloudsql-instances takes them. Each is a Unix socket at /cloudsql/<connection name>, " +
				"served by the Cloud SQL Auth Proxy beside the container (#1346). It is the volume Cloud Run v1's " +
				"run.googleapis.com/cloudsql-instances annotation makes, so a service gcloud deployed shows its instances here."},
		{Name: "nfsVolumes", Label: "NFS volumes", Type: "map", Section: section,
			Help: "Optional. One /mount/path=server:/export per line, or /mount/path=server:/export:ro for read-only, " +
				"as gcloud's --add-volume type=nfs takes them (#1448). The node mounts the export, so the server is any " +
				"NFS server the instance's cluster reaches: a local instance has no Filestore."},
		{Name: "gcsVolumes", Label: "Cloud Storage volumes", Type: "map", Section: section,
			Help: "Optional. One /mount/path=bucket per line, /mount/path=bucket:ro for read-only, and gcsfuse mount " +
				"options after semicolons (bucket;only-dir=logs;implicit-dirs), as gcloud's --add-volume type=cloud-storage " +
				"takes them (#1474). The bucket is this instance's Cloud Storage, mounted by Cloud Storage FUSE."},
	}
}

// podFields is what a service's RevisionTemplate and a job's TaskTemplate
// share for the form.
type podFields struct {
	serviceAccount *string
	volumes        *[]*runpb.Volume
}

// applyRunVolumes sets the form's service account, volumes and mounts on a
// template and its one container. A field the request did not send leaves
// that part as it was, so an older form edits nothing it did not show.
func applyRunVolumes(p podFields, c *runpb.Container, values map[string]string) error {
	if v, ok := values["serviceAccount"]; ok {
		*p.serviceAccount = strings.TrimSpace(v)
	}
	rawSecret, sentSecret := values["secretFiles"]
	rawMemory, sentMemory := values["memoryVolumes"]
	rawSQL, sentSQL := values["cloudSqlInstances"]
	rawNFS, sentNFS := values["nfsVolumes"]
	rawGCS, sentGCS := values["gcsVolumes"]
	if !sentSecret && !sentMemory && !sentSQL && !sentNFS && !sentGCS {
		return nil
	}
	var keep []*runpb.Volume
	kept := map[string]bool{}
	for _, v := range *p.volumes {
		if (v.GetSecret() != nil && sentSecret) || (v.GetEmptyDir() != nil && sentMemory) ||
			(v.GetCloudSqlInstance() != nil && sentSQL) || (v.GetNfs() != nil && sentNFS) ||
			(v.GetGcs() != nil && sentGCS) {
			continue
		}
		keep = append(keep, v)
		kept[v.GetName()] = true
	}
	var mounts []*runpb.VolumeMount
	for _, m := range c.GetVolumeMounts() {
		if kept[m.GetName()] {
			mounts = append(mounts, m)
		}
	}
	if sentSecret {
		m, err := console.ParseMap(rawSecret)
		if err != nil {
			return fmt.Errorf("files from Secret Manager: %w", err)
		}
		byDir := map[string]*runpb.SecretVolumeSource{}
		var dirs []string
		for _, file := range sortedKeys(m) {
			ref := strings.TrimSpace(m[file])
			secret, version := ref, "latest"
			if i := strings.LastIndex(ref, ":"); i >= 0 {
				secret, version = ref[:i], ref[i+1:]
			}
			if !path.IsAbs(file) || path.Base(file) == "/" || secret == "" || version == "" {
				return fmt.Errorf("files from Secret Manager: %s=%q is not /dir/file=secret[:version]", file, ref)
			}
			dir := path.Dir(path.Clean(file))
			s := byDir[dir]
			if s == nil {
				s = &runpb.SecretVolumeSource{Secret: secret}
				byDir[dir] = s
				dirs = append(dirs, dir)
			} else if s.Secret != secret {
				return fmt.Errorf("files from Secret Manager: %s holds files from %s and %s; use one secret per directory", dir, s.Secret, secret)
			}
			s.Items = append(s.Items, &runpb.VersionToPath{Path: path.Base(file), Version: version})
		}
		for i, dir := range dirs {
			name := fmt.Sprintf("secret-%d", i+1)
			keep = append(keep, &runpb.Volume{Name: name, VolumeType: &runpb.Volume_Secret{Secret: byDir[dir]}})
			mounts = append(mounts, &runpb.VolumeMount{Name: name, MountPath: dir})
		}
	}
	if sentMemory {
		m, err := console.ParseMap(rawMemory)
		if err != nil {
			return fmt.Errorf("in-memory volumes: %w", err)
		}
		for i, dir := range sortedKeys(m) {
			if !path.IsAbs(dir) || path.Clean(dir) == "/" {
				return fmt.Errorf("in-memory volumes: %q is not an absolute path other than /", dir)
			}
			name := fmt.Sprintf("memory-%d", i+1)
			keep = append(keep, &runpb.Volume{Name: name, VolumeType: &runpb.Volume_EmptyDir{EmptyDir: &runpb.EmptyDirVolumeSource{
				Medium: runpb.EmptyDirVolumeSource_MEMORY, SizeLimit: strings.TrimSpace(m[dir])}}})
			mounts = append(mounts, &runpb.VolumeMount{Name: name, MountPath: path.Clean(dir)})
		}
	}
	if sentSQL {
		var conns []string
		for _, n := range strings.Split(rawSQL, ",") {
			if n = strings.TrimSpace(n); n != "" {
				conns = append(conns, n)
			}
		}
		if len(conns) > 0 {
			keep = append(keep, &runpb.Volume{Name: "cloudsql", VolumeType: &runpb.Volume_CloudSqlInstance{
				CloudSqlInstance: &runpb.CloudSqlInstance{Instances: conns}}})
			mounts = append(mounts, &runpb.VolumeMount{Name: "cloudsql", MountPath: "/cloudsql"})
		}
	}
	if sentNFS {
		m, err := console.ParseMap(rawNFS)
		if err != nil {
			return fmt.Errorf("NFS volumes: %w", err)
		}
		for i, dir := range sortedKeys(m) {
			n, ok := parseRunNFS(m[dir])
			if !path.IsAbs(dir) || path.Clean(dir) == "/" || !ok {
				return fmt.Errorf("NFS volumes: %s=%q is not /mount/path=server:/export[:ro]", dir, m[dir])
			}
			name := fmt.Sprintf("nfs-%d", i+1)
			keep = append(keep, &runpb.Volume{Name: name, VolumeType: &runpb.Volume_Nfs{Nfs: n}})
			mounts = append(mounts, &runpb.VolumeMount{Name: name, MountPath: path.Clean(dir)})
		}
	}
	if sentGCS {
		m, err := console.ParseMap(rawGCS)
		if err != nil {
			return fmt.Errorf("Cloud Storage volumes: %w", err)
		}
		for i, dir := range sortedKeys(m) {
			g, ok := parseRunGCS(m[dir])
			if !path.IsAbs(dir) || path.Clean(dir) == "/" || !ok {
				return fmt.Errorf("Cloud Storage volumes: %s=%q is not /mount/path=bucket[:ro][;option...]", dir, m[dir])
			}
			name := fmt.Sprintf("gcs-%d", i+1)
			keep = append(keep, &runpb.Volume{Name: name, VolumeType: &runpb.Volume_Gcs{Gcs: g}})
			mounts = append(mounts, &runpb.VolumeMount{Name: name, MountPath: path.Clean(dir)})
		}
	}
	for _, mt := range mounts {
		for _, other := range mounts {
			if mt != other && mt.GetMountPath() == other.GetMountPath() {
				return fmt.Errorf("%s is mounted twice", mt.GetMountPath())
			}
		}
	}
	*p.volumes = keep
	c.VolumeMounts = mounts
	return nil
}

// runVolumeDefaults are the volume fields' prefill from a template's
// volumes and its one container's mounts.
func runVolumeDefaults(volumes []*runpb.Volume, mounts []*runpb.VolumeMount) (secretFiles, memoryVolumes string) {
	byName := map[string]*runpb.Volume{}
	for _, v := range volumes {
		byName[v.GetName()] = v
	}
	secret, memory := map[string]string{}, map[string]string{}
	for _, m := range mounts {
		v := byName[m.GetName()]
		switch {
		case v.GetSecret() != nil:
			s := v.GetSecret()
			items := s.GetItems()
			if len(items) == 0 {
				id := s.GetSecret()[strings.LastIndex(s.GetSecret(), "/")+1:]
				items = []*runpb.VersionToPath{{Path: id, Version: "latest"}}
			}
			for _, it := range items {
				secret[path.Join(m.GetMountPath(), it.GetPath())] = formatRunSecretRef(s.GetSecret(), it.GetVersion())
			}
		case v.GetEmptyDir() != nil:
			memory[m.GetMountPath()] = v.GetEmptyDir().GetSizeLimit()
		}
	}
	return console.FormatMap(secret), console.FormatMap(memory)
}

// runCloudSQLDefault is the Cloud SQL connections field's prefill: every
// Cloud SQL volume's connection names.
func runCloudSQLDefault(volumes []*runpb.Volume) string {
	var conns []string
	for _, v := range volumes {
		conns = append(conns, v.GetCloudSqlInstance().GetInstances()...)
	}
	return strings.Join(conns, ", ")
}

// parseRunNFS reads server:/export or server:/export:ro.
func parseRunNFS(ref string) (*runpb.NFSVolumeSource, bool) {
	ref = strings.TrimSpace(ref)
	readOnly := false
	if r, ok := strings.CutSuffix(ref, ":ro"); ok {
		ref, readOnly = r, true
	}
	server, export, ok := strings.Cut(ref, ":")
	if !ok || server == "" || !path.IsAbs(export) {
		return nil, false
	}
	return &runpb.NFSVolumeSource{Server: server, Path: export, ReadOnly: readOnly}, true
}

// runNFSDefault is the NFS volumes field's prefill.
func runNFSDefault(volumes []*runpb.Volume, mounts []*runpb.VolumeMount) string {
	byName := map[string]*runpb.NFSVolumeSource{}
	for _, v := range volumes {
		if n := v.GetNfs(); n != nil {
			byName[v.GetName()] = n
		}
	}
	out := map[string]string{}
	for _, m := range mounts {
		if n := byName[m.GetName()]; n != nil {
			p := n.GetPath()
			if p == "" {
				p = "/"
			}
			ref := n.GetServer() + ":" + p
			if n.GetReadOnly() {
				ref += ":ro"
			}
			out[m.GetMountPath()] = ref
		}
	}
	return console.FormatMap(out)
}

// parseRunGCS reads bucket, bucket:ro, and either followed by gcsfuse mount
// options after semicolons.
func parseRunGCS(ref string) (*runpb.GCSVolumeSource, bool) {
	parts := strings.Split(strings.TrimSpace(ref), ";")
	bucket, readOnly := strings.TrimSpace(parts[0]), false
	if b, ok := strings.CutSuffix(bucket, ":ro"); ok {
		bucket, readOnly = b, true
	}
	if bucket == "" || strings.ContainsAny(bucket, ":/ ") {
		return nil, false
	}
	g := &runpb.GCSVolumeSource{Bucket: bucket, ReadOnly: readOnly}
	for _, o := range parts[1:] {
		if o = strings.TrimSpace(o); o != "" {
			g.MountOptions = append(g.MountOptions, o)
		}
	}
	return g, true
}

// runGCSDefault is the Cloud Storage volumes field's prefill.
func runGCSDefault(volumes []*runpb.Volume, mounts []*runpb.VolumeMount) string {
	byName := map[string]*runpb.GCSVolumeSource{}
	for _, v := range volumes {
		if g := v.GetGcs(); g != nil {
			byName[v.GetName()] = g
		}
	}
	out := map[string]string{}
	for _, m := range mounts {
		if g := byName[m.GetName()]; g != nil {
			ref := g.GetBucket()
			if g.GetReadOnly() {
				ref += ":ro"
			}
			for _, o := range g.GetMountOptions() {
				ref += ";" + o
			}
			out[m.GetMountPath()] = ref
		}
	}
	return console.FormatMap(out)
}
