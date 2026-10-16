package run

import (
	"strings"
	"testing"

	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// A service's NFS volume (#1448) is a PersistentVolume of the export and a
// claim bound to it, applied before the Knative Service, whose pod mounts
// the claim: Knative refuses an inline nfs volume.
func TestServiceNFSVolumeIsABoundClaim(t *testing.T) {
	t.Parallel()
	s := svc("app", &runpb.Container{Image: "gcr.io/p/app:v1",
		VolumeMounts: []*runpb.VolumeMount{{Name: "share", MountPath: "/mnt/share"}, {Name: "ro", MountPath: "/mnt/ro"}}})
	s.Template.Volumes = []*runpb.Volume{
		{Name: "share", VolumeType: &runpb.Volume_Nfs{Nfs: &runpb.NFSVolumeSource{Server: "10.0.0.2", Path: "/exports/a"}}},
		{Name: "ro", VolumeType: &runpb.Volume_Nfs{Nfs: &runpb.NFSVolumeSource{Server: "10.0.0.2", ReadOnly: true}}},
	}
	m, err := ToKnative(s, "cloudburrow", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	rw := nfsClaimName(s.Template.Volumes[0].GetNfs())
	ro := nfsClaimName(s.Template.Volumes[1].GetNfs())
	if rw == ro {
		t.Fatalf("two exports share claim %s", rw)
	}
	docs := strings.Split(m, "\n---\n")
	if len(docs) != 5 || !strings.Contains(docs[4], "kind: Service") {
		t.Fatalf("want two PersistentVolumes and claims, then the Service; got %d documents:\n%s", len(docs), m)
	}
	for _, want := range []string{
		"kind: PersistentVolume\nmetadata:\n  name: " + rw,
		"server: \"10.0.0.2\"\n    path: \"/exports/a\"\n    readOnly: false",
		"path: \"/\"\n    readOnly: true",
		"accessModes: [ReadOnlyMany]",
		"claimRef:\n    namespace: cloudburrow\n    name: " + rw,
		"volumeName: " + ro,
		"storageClassName: \"\"",
		"persistentVolumeClaim:\n            claimName: " + rw + "\n",
		"persistentVolumeClaim:\n            claimName: " + ro + "\n            readOnly: true",
		"mountPath: \"/mnt/ro\"\n              readOnly: true",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("manifest lacks %q:\n%s", want, m)
		}
	}
	if strings.Contains(docs[4], "nfs:") {
		t.Errorf("the Knative Service has an inline nfs volume, which Knative refuses:\n%s", docs[4])
	}
	// The read-write mount is not marked read-only.
	if strings.Contains(m, "mountPath: \"/mnt/share\"\n              readOnly") {
		t.Errorf("read-write NFS mount rendered read-only:\n%s", m)
	}
}

// A job's task mounts the export inline: its pod is a batch Job's, which
// Knative does not validate.
func TestJobNFSVolumeIsInline(t *testing.T) {
	t.Parallel()
	job := sampleJob()
	tt := job.Template.Template
	tt.Volumes = []*runpb.Volume{{Name: "share", VolumeType: &runpb.Volume_Nfs{Nfs: &runpb.NFSVolumeSource{Server: "nfs.example", Path: "/x", ReadOnly: true}}}}
	tt.Containers[0].VolumeMounts = []*runpb.VolumeMount{{Name: "share", MountPath: "/data"}}
	m, err := renderExecution(job, "j", "j-abc", tt, 1, "cloudburrow", "test", fakeSecrets{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"nfs:\n            server: \"nfs.example\"\n            path: \"/x\"\n            readOnly: true",
		"mountPath: \"/data\"\n              readOnly: true",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("manifest lacks %q:\n%s", want, m)
		}
	}
	if strings.Contains(m, "PersistentVolume") {
		t.Errorf("a job's NFS volume rendered a claim:\n%s", m)
	}
}

// An NFS volume names its server, as Cloud Run requires.
func TestNFSVolumeRequiresAServer(t *testing.T) {
	t.Parallel()
	s := svc("app", &runpb.Container{Image: "gcr.io/p/app:v1"})
	s.Template.Volumes = []*runpb.Volume{{Name: "v", VolumeType: &runpb.Volume_Nfs{Nfs: &runpb.NFSVolumeSource{Path: "/x"}}}}
	if _, err := ToKnative(s, "cloudburrow", "test", nil); status.Code(err) != codes.InvalidArgument ||
		!strings.Contains(err.Error(), "nfs.server") {
		t.Errorf("NFS volume with no server = %v; want InvalidArgument naming nfs.server", err)
	}
}

// v1 reads an NFS volume back as it was set (#1448), as gcloud describes it.
func TestNFSVolumeReadsBackOnV1(t *testing.T) {
	t.Parallel()
	got := toV1Volumes([]*runpb.Volume{{Name: "share", VolumeType: &runpb.Volume_Nfs{Nfs: &runpb.NFSVolumeSource{
		Server: "10.0.0.2", Path: "/x", ReadOnly: true}}}})
	if len(got) != 1 || got[0].Nfs == nil || got[0].Nfs.Server != "10.0.0.2" || got[0].Nfs.Path != "/x" || !got[0].Nfs.ReadOnly {
		t.Errorf("v1 volumes = %+v", got)
	}
}
