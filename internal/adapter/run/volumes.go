package run

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"strconv"
	"strings"

	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// Volumes, volume mounts and the service account (#1297).
//
// Two volume types have a faithful Kubernetes counterpart and are rendered:
// a Secret Manager secret, mounted from the Kubernetes Secret that holds its
// versions (the one secretKeyRef env vars resolve to), and an in-memory
// emptyDir. A mount's subPath is Kubernetes' subPath (#1346). A Cloud SQL
// volume is an emptyDir the Cloud SQL Auth Proxy writes its sockets into
// (cloudsql.go). An NFS volume (#1448) is the export the caller names,
// mounted by the node's kubelet: a pod's inline nfs volume where the pod is
// ours (a job's task, a worker pool's instance), and for a service, whose
// pod spec Knative validates and which refuses an inline nfs volume, a
// PersistentVolume of the export with a claim bound to it (nfsClaims). An
// instance has no Filestore, so the server is whatever NFS server the
// cluster's node can reach. A Cloud Storage FUSE volume (#1474) is the
// node directory a gcsfuse pod mounted the instance's bucket on, mounted
// with HostToContainer propagation (gcsfuse.go).
//
// The service account is stored and returned, not acted on: every workload
// reaches the one metadata server, which names the instance's own account.
// So are VPC access, the encryption key, the execution environment and
// session affinity (#1346): Google documents each as a setting, and none has
// an effect here, so each is stored and returned and nothing claims more.

// annPodConfig records the Cloud Run volumes, each container's mounts and
// the service account, as a RevisionTemplate's JSON, so they read back as
// set rather than as the Kubernetes objects they rendered to.
const annPodConfig = "cloudburrow.dev/pod-config"

// checkVolumes validates volumes and mounts before anything renders, so a
// mistake is the caller's InvalidArgument rather than an apply failure.
func checkVolumes(tmpl podTemplate) error {
	declared := map[string]*runpb.Volume{}
	for _, v := range tmpl.GetVolumes() {
		if !serviceIDRE.MatchString(v.GetName()) || len(v.GetName()) > 63 {
			return apierror.InvalidArgument("volume name %q must be lowercase letters, digits and hyphens", v.GetName())
		}
		if declared[v.GetName()] != nil {
			return apierror.InvalidArgument("volume %q is declared twice", v.GetName())
		}
		declared[v.GetName()] = v
		if s := v.GetSecret(); s != nil && s.GetSecret() == "" {
			return apierror.InvalidArgument("volume %q: secret.secret is required", v.GetName())
		}
		if n := v.GetNfs(); n != nil && n.GetServer() == "" {
			return apierror.InvalidArgument("volume %q: nfs.server is required", v.GetName())
		}
		if v.GetGcs() != nil {
			if err := checkGCSVolume(v); err != nil {
				return err
			}
		}
		if v.GetVolumeType() == nil {
			return apierror.InvalidArgument("volume %q sets no volume type", v.GetName())
		}
	}
	for _, c := range tmpl.GetContainers() {
		for _, m := range c.GetVolumeMounts() {
			if declared[m.GetName()] == nil {
				return apierror.InvalidArgument("volumeMount %q names no declared volume", m.GetName())
			}
			if !path.IsAbs(m.GetMountPath()) || path.Clean(m.GetMountPath()) == "/" {
				return apierror.InvalidArgument("volumeMount %q: mountPath %q must be an absolute path other than /",
					m.GetName(), m.GetMountPath())
			}
			if sp := m.GetSubPath(); sp != "" && (path.IsAbs(sp) || path.Clean(sp) != sp ||
				sp == ".." || strings.HasPrefix(sp, "../")) {
				return apierror.InvalidArgument("volumeMount %q: subPath %q must be a relative path inside the volume",
					m.GetName(), sp)
			}
		}
	}
	return nil
}

// renderVolumes writes a pod spec's volumes list at the pod-spec indent.
func renderVolumes(b *strings.Builder, volumes []*runpb.Volume, o containerRender) error {
	if len(volumes) == 0 {
		return nil
	}
	b.WriteString("      volumes:\n")
	for i, v := range volumes {
		fmt.Fprintf(b, "        - name: %s\n", v.GetName())
		switch {
		case v.GetCloudSqlInstance() != nil:
			if err := checkCloudSQLVolume(fmt.Sprintf("volumes[%d]", i), v, o.cloudSQL); err != nil {
				return err
			}
			// The proxy's sockets, shared with the containers that mount it.
			b.WriteString("          emptyDir: {}\n")
		case v.GetSecret() != nil:
			s := v.GetSecret()
			if o.secrets == nil {
				return apierror.FailedPrecondition(
					"volume %q mounts secret %q, but Secret Manager is not enabled on this instance", v.GetName(), s.GetSecret())
			}
			items := s.GetItems()
			if len(items) == 0 {
				// Cloud Run's default: the latest version, in a file named
				// after the secret.
				id := s.GetSecret()
				if i := strings.LastIndex(id, "/"); i >= 0 {
					id = id[i+1:]
				}
				items = []*runpb.VersionToPath{{Path: id, Version: "latest"}}
			}
			var secretName string
			var lines strings.Builder
			for _, it := range items {
				name, key, err := o.secrets.ResolveSecretRef(o.project, s.GetSecret(), it.GetVersion())
				if err != nil {
					return err
				}
				secretName = name
				fmt.Fprintf(&lines, "              - key: %s\n                path: %q\n", key, it.GetPath())
				if it.GetMode() != 0 {
					fmt.Fprintf(&lines, "                mode: %d\n", it.GetMode())
				}
			}
			fmt.Fprintf(b, "          secret:\n            secretName: %s\n", secretName)
			if m := s.GetDefaultMode(); m != 0 {
				fmt.Fprintf(b, "            defaultMode: %d\n", m)
			}
			b.WriteString("            items:\n")
			b.WriteString(lines.String())
		case v.GetNfs() != nil:
			n := v.GetNfs()
			if !o.batch {
				fmt.Fprintf(b, "          persistentVolumeClaim:\n            claimName: %s\n", nfsClaimName(n))
				if n.GetReadOnly() {
					b.WriteString("            readOnly: true\n")
				}
				continue
			}
			fmt.Fprintf(b, "          nfs:\n            server: %q\n            path: %q\n", n.GetServer(), nfsPath(n))
			if n.GetReadOnly() {
				b.WriteString("            readOnly: true\n")
			}
		case v.GetGcs() != nil:
			// The node directory its gcsfuse pod mounted the bucket on: a
			// pod's hostPath where the pod is ours, and for a service, whose
			// hostPath mounts Knative makes read-only, a PersistentVolume of
			// the directory with a claim bound to it (gcsClaims).
			g := v.GetGcs()
			if !o.batch {
				fmt.Fprintf(b, "          persistentVolumeClaim:\n            claimName: %s\n", gcsMountName(g))
				if g.GetReadOnly() {
					b.WriteString("            readOnly: true\n")
				}
				continue
			}
			fmt.Fprintf(b, "          hostPath:\n            path: %s\n            type: Directory\n", gcsHostPath(g))
		case v.GetEmptyDir() != nil:
			e := v.GetEmptyDir()
			if e.GetMedium() != runpb.EmptyDirVolumeSource_MEMORY && e.GetSizeLimit() == "" {
				b.WriteString("          emptyDir: {}\n")
				continue
			}
			b.WriteString("          emptyDir:\n")
			if e.GetMedium() == runpb.EmptyDirVolumeSource_MEMORY {
				b.WriteString("            medium: Memory\n")
			}
			if e.GetSizeLimit() != "" {
				fmt.Fprintf(b, "            sizeLimit: %q\n", e.GetSizeLimit())
			}
		}
	}
	return nil
}

// renderVolumeMounts writes one container's volumeMounts. Knative requires
// a secret mount to be read-only, as Cloud Run's is, and a read-only NFS
// volume's claim is mounted read-only too.
func renderVolumeMounts(b *strings.Builder, mounts []*runpb.VolumeMount, volumes []*runpb.Volume) {
	if len(mounts) == 0 {
		return
	}
	secret, gcs := map[string]bool{}, map[string]bool{}
	for _, v := range volumes {
		secret[v.GetName()] = v.GetSecret() != nil || v.GetNfs().GetReadOnly() || v.GetGcs().GetReadOnly()
		gcs[v.GetName()] = v.GetGcs() != nil
	}
	b.WriteString("          volumeMounts:\n")
	for _, m := range mounts {
		fmt.Fprintf(b, "            - name: %s\n              mountPath: %q\n", m.GetName(), m.GetMountPath())
		if sp := m.GetSubPath(); sp != "" {
			fmt.Fprintf(b, "              subPath: %q\n", sp)
		}
		if secret[m.GetName()] {
			b.WriteString("              readOnly: true\n")
		}
		if gcs[m.GetName()] {
			// The FUSE mount is made on the node after the pod may start.
			b.WriteString("              mountPropagation: HostToContainer\n")
		}
	}
}

// podConfigAnnotation renders annPodConfig for a template, or "" when it
// sets none of the fields.
func podConfigAnnotation(tmpl podTemplate) string {
	rec := &runpb.RevisionTemplate{ServiceAccount: tmpl.GetServiceAccount(), Volumes: tmpl.GetVolumes(),
		VpcAccess: tmpl.GetVpcAccess(), EncryptionKey: tmpl.GetEncryptionKey(),
		ExecutionEnvironment: tmpl.GetExecutionEnvironment()}
	if rt, ok := tmpl.(*runpb.RevisionTemplate); ok {
		rec.SessionAffinity = rt.GetSessionAffinity()
	}
	set := rec.ServiceAccount != "" || len(rec.Volumes) > 0 || rec.VpcAccess != nil || rec.EncryptionKey != "" ||
		rec.ExecutionEnvironment != runpb.ExecutionEnvironment_EXECUTION_ENVIRONMENT_UNSPECIFIED || rec.SessionAffinity
	for _, c := range tmpl.GetContainers() {
		rec.Containers = append(rec.Containers, &runpb.Container{VolumeMounts: c.GetVolumeMounts()})
		set = set || len(c.GetVolumeMounts()) > 0
	}
	if !set {
		return ""
	}
	raw, _ := protojson.Marshal(rec)
	return string(raw)
}

// readPodConfig merges annPodConfig into a read-back template.
func readPodConfig(ann map[string]string, into *runpb.RevisionTemplate) {
	raw := ann[annPodConfig]
	if raw == "" {
		return
	}
	var rec runpb.RevisionTemplate
	if protojson.Unmarshal([]byte(raw), &rec) != nil {
		return
	}
	into.ServiceAccount = rec.GetServiceAccount()
	into.Volumes = rec.GetVolumes()
	into.VpcAccess = rec.GetVpcAccess()
	into.EncryptionKey = rec.GetEncryptionKey()
	into.ExecutionEnvironment = rec.GetExecutionEnvironment()
	into.SessionAffinity = rec.GetSessionAffinity()
	for i, c := range rec.GetContainers() {
		if i < len(into.Containers) {
			into.Containers[i].VolumeMounts = c.GetVolumeMounts()
		}
	}
}

// nfsPath is the export an NFS volume mounts; Cloud Run's default is the
// server's root.
func nfsPath(n *runpb.NFSVolumeSource) string {
	if n.GetPath() == "" {
		return "/"
	}
	return n.GetPath()
}

// nfsClaimName names the PersistentVolume of one NFS export, and the claim
// bound to it: every service that mounts the same export, read-only or
// not, shares them.
func nfsClaimName(n *runpb.NFSVolumeSource) string {
	h := sha256.Sum256([]byte(n.GetServer() + "\x00" + nfsPath(n) + "\x00" + strconv.FormatBool(n.GetReadOnly())))
	return "run-nfs-" + hex.EncodeToString(h[:6])
}

// nfsClaims renders, as documents to apply before a Knative Service, a
// PersistentVolume and a bound claim for each of its NFS volumes. The
// volume is pre-bound to its claim (claimRef and volumeName, with no
// storage class), so no provisioner is involved; the size is nominal, as
// an NFS export's is not a PersistentVolume's to enforce. Every service
// that mounts the export shares them, and they are removed once the last
// is deleted (volumegc.go); Retain, so the data, the NFS server's, is
// never touched.
func nfsClaims(namespace, instance string, volumes []*runpb.Volume) string {
	var b strings.Builder
	seen := map[string]bool{}
	for _, v := range volumes {
		n := v.GetNfs()
		if n == nil || seen[nfsClaimName(n)] {
			continue
		}
		name := nfsClaimName(n)
		seen[name] = true
		mode := "ReadWriteMany"
		if n.GetReadOnly() {
			mode = "ReadOnlyMany"
		}
		fmt.Fprintf(&b, `apiVersion: v1
kind: PersistentVolume
metadata:
  name: %[1]s
  labels:
    cloudburrow.dev/owned: "true"
    cloudburrow.dev/instance: %[2]q
spec:
  capacity:
    storage: 1Gi
  accessModes: [%[3]s]
  persistentVolumeReclaimPolicy: Retain
  storageClassName: ""
  claimRef:
    namespace: %[4]s
    name: %[1]s
  nfs:
    server: %[5]q
    path: %[6]q
    readOnly: %[7]t
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: %[1]s
  namespace: %[4]s
  labels:
    cloudburrow.dev/owned: "true"
    cloudburrow.dev/instance: %[2]q
spec:
  accessModes: [%[3]s]
  storageClassName: ""
  volumeName: %[1]s
  resources:
    requests:
      storage: 1Gi
---
`, name, instance, mode, namespace, n.GetServer(), nfsPath(n), n.GetReadOnly())
	}
	return b.String()
}
