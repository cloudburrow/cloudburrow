package run

import (
	"strings"
	"testing"

	runpb "cloud.google.com/go/run/apiv2/runpb"
)

// A Cloud Storage volume (#1474) is the node directory its gcsfuse pod
// mounted the bucket on, mounted with HostToContainer propagation, and
// read-only when the volume is.
func TestServiceGCSVolumeIsTheMountedHostPath(t *testing.T) {
	t.Parallel()
	s := svc("app", &runpb.Container{Image: "gcr.io/p/app:v1",
		VolumeMounts: []*runpb.VolumeMount{{Name: "rw", MountPath: "/mnt/rw"}, {Name: "ro", MountPath: "/mnt/ro"}}})
	s.Template.Volumes = []*runpb.Volume{
		{Name: "rw", VolumeType: &runpb.Volume_Gcs{Gcs: &runpb.GCSVolumeSource{Bucket: "data"}}},
		{Name: "ro", VolumeType: &runpb.Volume_Gcs{Gcs: &runpb.GCSVolumeSource{Bucket: "data", ReadOnly: true}}},
	}
	m, err := ToKnative(s, "cloudburrow", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	rw, ro := gcsHostPath(s.Template.Volumes[0].GetGcs()), gcsHostPath(s.Template.Volumes[1].GetGcs())
	if rw == ro {
		t.Fatalf("a read-only and a read-write mount share %s", rw)
	}
	rwc, roc := gcsMountName(s.Template.Volumes[0].GetGcs()), gcsMountName(s.Template.Volumes[1].GetGcs())
	for _, want := range []string{
		"kind: PersistentVolume\nmetadata:\n  name: " + rwc,
		"hostPath:\n    path: " + rw + "\n    type: Directory",
		"hostPath:\n    path: " + ro + "\n    type: Directory",
		"accessModes: [ReadOnlyMany]",
		"persistentVolumeClaim:\n            claimName: " + rwc + "\n",
		"persistentVolumeClaim:\n            claimName: " + roc + "\n            readOnly: true",
		"mountPath: \"/mnt/rw\"\n              mountPropagation: HostToContainer",
		"mountPath: \"/mnt/ro\"\n              readOnly: true\n              mountPropagation: HostToContainer",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("manifest lacks %q:\n%s", want, m)
		}
	}
}

// The gcsfuse pod reaches the instance's storage server, never Google, and
// passes mountOptions as gcsfuse flags or FUSE options.
func TestGCSFusePodPointsAtTheInstance(t *testing.T) {
	t.Parallel()
	g := &runpb.GCSVolumeSource{Bucket: "data", ReadOnly: true, MountOptions: []string{"only-dir=sub;uid=1000", "noexec"}}
	m, err := gcsFusePod("cloudburrow", "test", "http://storage.cb.svc.cluster.local:4443", g)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"--custom-endpoint=http://storage.cb.svc.cluster.local:4443/storage/v1/"`,
		`"--anonymous-access"`, `"--foreground"`, `"-o", "ro"`, `"--only-dir=sub"`, `"--uid=1000"`, `"-o", "noexec"`,
		`"data", "/mnt/gcsfuse/` + gcsMountName(g) + `"`,
		"image: " + GCSFuseImage,
		"privileged: true",
		"mountPropagation: Bidirectional",
		"path: " + gcsHostPath(g) + "\n        type: DirectoryOrCreate",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("pod lacks %s:\n%s", want, m)
		}
	}
}

// A mount option may not redirect gcsfuse or give it credentials.
func TestGCSVolumeRefusesEndpointOptions(t *testing.T) {
	t.Parallel()
	for _, opt := range []string{"custom-endpoint=https://storage.googleapis.com", "key-file=/k.json", "anonymous-access=false", "a b"} {
		v := &runpb.Volume{Name: "v", VolumeType: &runpb.Volume_Gcs{Gcs: &runpb.GCSVolumeSource{Bucket: "b", MountOptions: []string{opt}}}}
		if err := checkGCSVolume(v); err == nil {
			t.Errorf("mount option %q accepted", opt)
		}
	}
	if err := checkGCSVolume(&runpb.Volume{Name: "v", VolumeType: &runpb.Volume_Gcs{Gcs: &runpb.GCSVolumeSource{}}}); err == nil {
		t.Error("a volume with no bucket was accepted")
	}
}

func TestGCSStorageEndpoint(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"storage.ns.svc.cluster.local:4443":         "http://storage.ns.svc.cluster.local:4443",
		"http://storage.ns.svc.cluster.local:4443/": "http://storage.ns.svc.cluster.local:4443",
		"": "",
	} {
		if got := gcsStorageEndpoint([]injectedEnv{{name: "STORAGE_EMULATOR_HOST", value: in}}); got != want {
			t.Errorf("gcsStorageEndpoint(%q) = %q, want %q", in, got, want)
		}
	}
}
