package run

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"time"

	runpb "cloud.google.com/go/run/apiv2/runpb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// Cloud Storage FUSE volumes (#1474).
//
// Cloud Run mounts a `volumes[].gcs` bucket into the container with
// Cloud Storage FUSE, read-write unless readOnly, passing mountOptions to
// gcsfuse. Here the bucket is the instance's Cloud Storage, never Google's:
// a gcsfuse pod per distinct mount (bucket, read-only, options) runs
// Google's own gcsfuse, pointed at the instance's storage server with
// --custom-endpoint and --anonymous-access, and mounts the bucket on the
// kind node under gcsFuseHostRoot, with Bidirectional propagation so the
// mount is the node's. A revision, a job's task or a worker pool's pod then
// mounts that host directory with HostToContainer propagation, so it sees
// the FUSE mount made under it. Knative refuses a privileged container and a
// CSI volume would need GKE's token flow, which is why the mount is made at
// the node rather than in the workload's pod. A service mounts the directory
// through a PersistentVolume and claim (gcsClaims), as Knative makes a
// hostPath mount read-only.
//
// The mount pod is applied, and the bucket seen mounted, before the
// workload that mounts it: otherwise the workload could write into the bare
// host directory, under the mount that then hides it. The pod is kept when
// the workload is deleted, as an NFS volume's claim is: another workload may
// mount the same bucket, and the data is the bucket's.

// GCSFuseImage is the Cloud Storage FUSE CSI driver's sidecar mounter
// v1.24.9-gke.2, Google's image, pinned by the index digest (linux/amd64 and
// linux/arm64). It carries gcsfuse 3.12.0 at /gcsfuse, which is all that is
// run from it: the sidecar mounter's own entrypoint is not used.
const GCSFuseImage = "gke.gcr.io/gcs-fuse-csi-driver-sidecar-mounter:v1.24.9-gke.2@sha256:c3115b88a34e2675b93eaa4e6245fa5ae82bd9f63bcf2dae3ff148d8a902c16b"

// gcsFuseDriver is the CSI driver a v1 (Knative) volume names for Cloud
// Storage FUSE.
const gcsFuseDriver = "gcsfuse.run.googleapis.com"

// gcsFuseHostRoot is the node directory the buckets are mounted under.
const gcsFuseHostRoot = "/var/lib/cloudburrow/gcsfuse"

// gcsFuseMounted is what gcsfuse logs, in the foreground, once the bucket
// is mounted.
const gcsFuseMounted = "successfully mounted"

// gcsFuseWait bounds how long a workload waits for its buckets to mount,
// within the deadline a client gives the call that deploys it.
var gcsFuseWait = 45 * time.Second

// gcsFuseOptions are the mount options passed to FUSE itself (`-o`); every
// other option is a gcsfuse flag, as GKE's driver passes them.
var gcsFuseOptions = map[string]bool{
	"ro": true, "rw": true, "exec": true, "noexec": true, "suid": true, "nosuid": true,
	"dev": true, "nodev": true, "atime": true, "noatime": true, "allow_other": true,
	"default_permissions": true, "sync": true, "async": true, "dirsync": true,
}

// gcsFuseRefused are the gcsfuse flags a mount option may not set: each
// would point gcsfuse away from the instance's storage, give it credentials,
// or stop it serving in the foreground.
var gcsFuseRefused = map[string]bool{
	"custom-endpoint": true, "anonymous-access": true, "key-file": true, "token-url": true,
	"reuse-token-from-url": true, "foreground": true, "config-file": true, "client-protocol": true,
	"enable-google-lib-auth": true, "impersonate-service-account": true, "billing-project": true,
	"log-file": true,
}

var gcsFuseOptionRE = regexp.MustCompile(`^[a-z][a-z0-9_-]*(=[^\s]*)?$`)

// checkGCSVolume validates a Cloud Storage volume's bucket and options.
func checkGCSVolume(v *runpb.Volume) error {
	g := v.GetGcs()
	if g.GetBucket() == "" {
		return apierror.InvalidArgument("volume %q: gcs.bucket is required", v.GetName())
	}
	_, err := gcsFuseArgs(g)
	if err != nil {
		return apierror.InvalidArgument("volume %q: %v", v.GetName(), err)
	}
	return nil
}

// gcsFuseArgs are gcsfuse's flags for a volume's mountOptions, before the
// endpoint and the mount point.
func gcsFuseArgs(g *runpb.GCSVolumeSource) ([]string, error) {
	var args []string
	if g.GetReadOnly() {
		args = append(args, "-o", "ro")
	}
	for _, raw := range g.GetMountOptions() {
		for _, opt := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ';' }) {
			opt = strings.TrimPrefix(strings.TrimSpace(opt), "--")
			if opt == "" {
				continue
			}
			if !gcsFuseOptionRE.MatchString(opt) {
				return nil, fmt.Errorf("mount option %q is not KEY or KEY=VALUE", opt)
			}
			key, _, _ := strings.Cut(opt, "=")
			switch {
			case gcsFuseRefused[key]:
				return nil, fmt.Errorf("mount option %q is set by the instance and cannot be changed", key)
			case gcsFuseOptions[key]:
				args = append(args, "-o", opt)
			default:
				args = append(args, "--"+opt)
			}
		}
	}
	return args, nil
}

// gcsMountName names the gcsfuse pod of one mount, and its node directory.
// Every workload that mounts the same bucket with the same options shares
// them.
func gcsMountName(g *runpb.GCSVolumeSource) string {
	h := sha256.Sum256([]byte(g.GetBucket() + "\x00" + fmt.Sprint(g.GetReadOnly()) + "\x00" +
		strings.Join(g.GetMountOptions(), "\x00")))
	return "run-gcsfuse-" + hex.EncodeToString(h[:6])
}

// gcsHostPath is the node directory a volume's bucket is mounted on.
func gcsHostPath(g *runpb.GCSVolumeSource) string {
	return gcsFuseHostRoot + "/" + gcsMountName(g)
}

// gcsStorageEndpoint is the storage server's in-cluster URL, from the
// STORAGE_EMULATOR_HOST every workload is given, or "" when Cloud Storage
// is not enabled.
func gcsStorageEndpoint(injected []injectedEnv) string {
	for _, e := range injected {
		if e.name != "STORAGE_EMULATOR_HOST" || e.value == "" {
			continue
		}
		v := e.value
		if !strings.Contains(v, "://") {
			v = "http://" + v
		}
		return strings.TrimSuffix(v, "/")
	}
	return ""
}

// gcsFusePod renders the gcsfuse pod of one mount. gcsfuse runs privileged
// (it opens /dev/fuse and mounts) in the foreground, logging to stdout; the
// mount's own directory is a second hostPath volume, so the kubelet creates
// it before gcsfuse mounts onto it.
func gcsFusePod(namespace, instance, endpoint string, g *runpb.GCSVolumeSource) (string, error) {
	opts, err := gcsFuseArgs(g)
	if err != nil {
		return "", err
	}
	name := gcsMountName(g)
	args := append([]string{"--foreground", "--anonymous-access", "--custom-endpoint=" + endpoint + "/storage/v1/",
		"--implicit-dirs", "-o", "allow_other"}, opts...)
	args = append(args, g.GetBucket(), "/mnt/gcsfuse/"+name)
	var b strings.Builder
	fmt.Fprintf(&b, `apiVersion: v1
kind: Pod
metadata:
  name: %[1]s
  namespace: %[2]s
  labels:
    cloudburrow.dev/owned: "true"
    cloudburrow.dev/instance: %[3]q
    cloudburrow.dev/gcsfuse: %[1]s
  annotations:
    cloudburrow.dev/gcs-bucket: %[4]q
spec:
  restartPolicy: Always
  terminationGracePeriodSeconds: 10
  containers:
    - name: gcsfuse
      image: %[5]s
      command: ["/gcsfuse"]
      args: [%[6]s]
      env:
        - name: STORAGE_EMULATOR_HOST
          value: %[7]q
      securityContext:
        privileged: true
      volumeMounts:
        - name: root
          mountPath: /mnt/gcsfuse
          mountPropagation: Bidirectional
        - name: dir
          mountPath: /mount-dir
  volumes:
    - name: root
      hostPath:
        path: %[8]s
        type: DirectoryOrCreate
    - name: dir
      hostPath:
        path: %[9]s
        type: DirectoryOrCreate
---
`, name, namespace, instance, g.GetBucket(), GCSFuseImage, quote(args), endpoint, gcsFuseHostRoot, gcsHostPath(g))
	return b.String(), nil
}

// ensureGCSFuse applies the gcsfuse pod of each Cloud Storage volume and
// waits until its bucket is mounted, before the workload that mounts it is
// applied.
func (s *Server) ensureGCSFuse(ctx context.Context, project string, volumes []*runpb.Volume) error {
	var mounts []*runpb.GCSVolumeSource
	seen := map[string]bool{}
	for _, v := range volumes {
		if g := v.GetGcs(); g != nil && !seen[gcsMountName(g)] {
			seen[gcsMountName(g)] = true
			mounts = append(mounts, g)
		}
	}
	if len(mounts) == 0 {
		return nil
	}
	endpoint := gcsStorageEndpoint(s.injectedRaw(project))
	if endpoint == "" {
		return apierror.FailedPrecondition("a Cloud Storage volume mounts a bucket, but Cloud Storage is not enabled on this instance")
	}
	for _, g := range mounts {
		manifest, err := gcsFusePod(s.kn.namespace(), s.instance, endpoint, g)
		if err != nil {
			return apierror.InvalidArgument("%v", err)
		}
		if err := s.kn.Apply(ctx, manifest); err != nil {
			return err
		}
	}
	for _, g := range mounts {
		if err := s.awaitGCSMount(ctx, g); err != nil {
			return err
		}
	}
	return nil
}

// awaitGCSMount waits for a gcsfuse pod to report its bucket mounted.
func (s *Server) awaitGCSMount(ctx context.Context, g *runpb.GCSVolumeSource) error {
	name := gcsMountName(g)
	deadline := time.Now().Add(gcsFuseWait)
	var last string
	for {
		out, err := s.kn.Kube.Do(ctx, "", "logs", "-n", s.kn.namespace(), "pod/"+name, "-c", "gcsfuse", "--tail=200")
		if err == nil && strings.Contains(out, gcsFuseMounted) {
			return nil
		}
		if err == nil {
			last = strings.TrimSpace(out)
		}
		if time.Now().After(deadline) {
			if i := strings.LastIndex(last, "\n"); i >= 0 && len(last)-i > 0 {
				last = last[i+1:]
			}
			return apierror.FailedPrecondition("bucket %q was not mounted by Cloud Storage FUSE within %s: %s",
				g.GetBucket(), gcsFuseWait, last)
		}
		select {
		case <-ctx.Done():
			return apierror.FailedPrecondition("bucket %q: %v", g.GetBucket(), ctx.Err())
		case <-time.After(time.Second):
		}
	}
}

// gcsClaims renders, as documents to apply before a Knative Service, a
// PersistentVolume of the node directory each of its Cloud Storage volumes'
// buckets is mounted on, and a claim bound to it. Knative makes a hostPath
// mount read-only whatever the pod asks, and lets a claim be written
// (kubernetes.podspec-persistent-volume-write). Like an NFS volume's, they
// are pre-bound, with a nominal size, and kept when the service is deleted.
func gcsClaims(namespace, instance string, volumes []*runpb.Volume) string {
	var b strings.Builder
	seen := map[string]bool{}
	for _, v := range volumes {
		g := v.GetGcs()
		if g == nil || seen[gcsMountName(g)] {
			continue
		}
		name := gcsMountName(g)
		seen[name] = true
		mode := "ReadWriteMany"
		if g.GetReadOnly() {
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
  hostPath:
    path: %[5]s
    type: Directory
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
`, name, instance, mode, namespace, gcsHostPath(g))
	}
	return b.String()
}
