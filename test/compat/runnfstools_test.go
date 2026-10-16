//go:build compat

package compat

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// covers: run.namespaces.services.create, run.namespaces.services.get, run.namespaces.services.replaceService, run.namespaces.services.delete
//
// TestGcloudRunDeployNFSVolume (#1448): `gcloud run deploy --add-volume
// type=nfs` and `--add-volume-mount`, through the configuration
// `cloudburrow gcloud-setup` writes, with egress to Google refused, mounts
// an NFS server in the instance's cluster; the service becomes ready, and
// describe reads the volume and its mount back.
func TestGcloudRunDeployNFSVolume(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvRun)
	server, _ := runNFSServer(t)
	g := newGcloudSession(t, h)
	id := "gcloud-nfs"
	region := "--region=us-central1"
	t.Cleanup(func() { _, _ = g.run(nil, "run", "services", "delete", id, region, "--quiet") })
	g.must("run", "deploy", id, "--image="+helloDigest, region, "--no-allow-unauthenticated",
		"--add-volume=name=share,type=nfs,location="+server+":/",
		"--add-volume-mount=volume=share,mount-path=/mnt/share", "--quiet")
	got := g.must("run", "services", "describe", id, region,
		"--format=value(status.latestReadyRevisionName,spec.template.spec.volumes[0].nfs.server,"+
			"spec.template.spec.volumes[0].nfs.path,spec.template.spec.containers[0].volumeMounts[0].mountPath)")
	if !strings.Contains(got, id+"-00001") || !strings.Contains(got, server) || !strings.Contains(got, "/mnt/share") {
		t.Errorf("describe = %q; want the first revision ready, the NFS server and the mount path", got)
	}
	g.must("run", "services", "delete", id, region, "--quiet")
}

// covers: google.cloud.run.v2.Services/CreateService, google.cloud.run.v2.Services/GetService, google.cloud.run.v2.Services/DeleteService
//
// TestTerraformRunNFSVolume (#1448): a google_cloud_run_v2_service with a
// volumes block of nfs { server, path, read_only } and its volume_mounts
// applies through `cloudburrow terraform` with egress blocked, against an
// NFS server in the instance's cluster, and becomes ready; the official
// client reads the volume back; the plan is clean; destroy removes it.
func TestTerraformRunNFSVolume(t *testing.T) {
	h := New(t)
	m := newTFModule(t)
	svcs := runClient(t, h)
	server, _ := runNFSServer(t)
	ctx := h.Context()
	id := "tfnfs-" + strings.TrimPrefix(h.Project(), "cb-test-")
	name := runParent(h) + "/services/" + id
	m.write(fmt.Sprintf(`
resource "google_cloud_run_v2_service" "s" {
  project             = %[1]q
  name                = %[2]q
  location            = "us-central1"
  deletion_protection = false
  template {
    volumes {
      name = "share"
      nfs {
        server    = %[3]q
        path      = "/"
        read_only = true
      }
    }
    containers {
      image = %[4]q
      volume_mounts {
        name       = "share"
        mount_path = "/mnt/share"
      }
    }
  }
}
`, h.Project(), id, server, helloDigest))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	s, err := svcs.GetService(ctx, &runpb.GetServiceRequest{Name: name})
	if err != nil {
		t.Fatalf("GetService: %v", err)
	}
	v := s.GetTemplate().GetVolumes()
	if len(v) != 1 || v[0].GetNfs().GetServer() != server || v[0].GetNfs().GetPath() != "/" || !v[0].GetNfs().GetReadOnly() ||
		s.GetTemplate().GetContainers()[0].GetVolumeMounts()[0].GetMountPath() != "/mnt/share" {
		t.Errorf("the service's template = %v", s.GetTemplate())
	}
	m.planClean()
	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := svcs.GetService(ctx, &runpb.GetServiceRequest{Name: name}); status.Code(err) != codes.NotFound {
		t.Errorf("GetService after destroy: %v; want NotFound", err)
	}
}

// covers: google.cloud.run.v2.Services/CreateService, google.cloud.run.v2.Services/GetService
//
// TestConsoleRunNFSVolume (#1448), in the run shard: a service deployed on
// the console's Deploy container form with an NFS volume (one read-only)
// against an NFS server in the instance's cluster becomes ready, is read
// back by the official ServicesClient with both, and its edit form is
// prefilled with the same lines.
func TestConsoleRunNFSVolume(t *testing.T) {
	h := New(t)
	addr := consoleAddr(t, h)
	c := runClient(t, h)
	server, _ := runNFSServer(t)
	ctx := h.Context()
	project := h.Project()
	id := "console-nfs"
	name := runParent(h) + "/services/" + id
	t.Cleanup(func() { _, _ = c.DeleteService(h.Context(), &runpb.DeleteServiceRequest{Name: name}) })
	lines := fmt.Sprintf(`{"/mnt/ro":"%[1]s:/:ro","/mnt/share":"%[1]s:/"}`, server)
	if code, body := consoleCreate(t, addr, "run", project, map[string]string{
		"name": id, "image": helloDigest, "nfsVolumes": lines,
	}); code != http.StatusOK {
		t.Fatalf("console deploy = %d: %s", code, body)
	}
	svc, err := c.GetService(ctx, &runpb.GetServiceRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	if v := svc.GetTemplate().GetVolumes(); len(v) != 2 || v[0].GetNfs().GetServer() != server || !v[0].GetNfs().GetReadOnly() ||
		v[1].GetNfs().GetReadOnly() || len(svc.GetTemplate().GetContainers()[0].GetVolumeMounts()) != 2 {
		t.Errorf("the console's service reads %v", svc.GetTemplate())
	}
	if _, values, _ := consoleRunEditForm(t, addr, project, id); values["nfsVolumes"] != lines {
		t.Errorf("the edit form is prefilled with %q; want %q", values["nfsVolumes"], lines)
	}
}
