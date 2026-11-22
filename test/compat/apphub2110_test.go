//go:build compat

package compat

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/apphub/apiv1/apphubpb"
	container "cloud.google.com/go/container/apiv1"
	"cloud.google.com/go/container/apiv1/containerpb"
	run "cloud.google.com/go/run/apiv2"
	"cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
)

// appHubMultiRegional is Scope.Type MULTI_REGIONAL, which googleapis
// defines (3) and cloud.google.com/go/apphub v1.4.1 does not name yet.
const appHubMultiRegional = apphubpb.Scope_Type(3)

// TestAppHubMultiRegional (#2110), with the official client over gRPC and
// over REST: as global-regional-applications documents it, a
// MULTI_REGIONAL application lives in us or eu (not global, not a region),
// registers a us-central1 Cloud Run service when it is in us and refuses
// it when it is in eu.
func TestAppHubMultiRegional(t *testing.T) {
	h := New(t)
	clients := appHubClients(t, h)
	h.Endpoint(EnvRun)
	ctx := h.Context()
	suffix := time.Now().UnixNano() % 1e6
	svcName, _ := deployAppHubResources(t, h, h.Project(), fmt.Sprintf("ah-mr-%d", suffix))
	for _, via := range []string{"grpc", "rest"} {
		t.Run(via, func(t *testing.T) {
			c := clients[via]
			detachAppHubProject(t, h, c, h.Project())
			host := "projects/" + h.Project() + "/locations/"
			op, err := c.CreateServiceProjectAttachment(ctx, &apphubpb.CreateServiceProjectAttachmentRequest{Parent: host + "global",
				ServiceProjectAttachmentId: h.Project(), ServiceProjectAttachment: &apphubpb.ServiceProjectAttachment{ServiceProject: "projects/" + h.Project()}})
			if err != nil {
				t.Fatalf("attach: %v", err)
			}
			if _, err := op.Wait(ctx); err != nil {
				t.Fatal(err)
			}
			mk := func(loc, id string) (*apphubpb.Application, error) {
				op, err := c.CreateApplication(ctx, &apphubpb.CreateApplicationRequest{Parent: host + loc, ApplicationId: id,
					Application: &apphubpb.Application{Scope: &apphubpb.Scope{Type: appHubMultiRegional}}})
				if err != nil {
					return nil, err
				}
				return op.Wait(ctx)
			}
			for _, loc := range []string{"global", "us-central1"} {
				if _, err := mk(loc, "bad"); pmCode(err) != codes.InvalidArgument {
					t.Errorf("a MULTI_REGIONAL application in %s: %v; want INVALID_ARGUMENT", loc, err)
				}
			}
			us, eu := fmt.Sprintf("mr-us-%s-%d", via, suffix), fmt.Sprintf("mr-eu-%s-%d", via, suffix)
			// The JSON API names the scope MULTI_REGIONAL, as Google's does;
			// the v1.4.1 REST client, which has no such name, discards it
			// (protojson's DiscardUnknown), so over REST the scope reads
			// back unset, and the gRPC client reads it.
			want := appHubMultiRegional
			if via == "rest" {
				want = apphubpb.Scope_TYPE_UNSPECIFIED
			}
			for loc, id := range map[string]string{"us": us, "eu": eu} {
				a, err := mk(loc, id)
				if err != nil || a.GetScope().GetType() != want {
					t.Fatalf("a MULTI_REGIONAL application in %s: %v, %v", loc, a, err)
				}
				t.Cleanup(func() {
					name := host + loc + "/applications/" + id
					if op, err := c.DeleteService(h.Context(), &apphubpb.DeleteServiceRequest{Name: name + "/services/hello"}); err == nil {
						_ = op.Wait(h.Context())
					}
					if op, err := c.DeleteApplication(h.Context(), &apphubpb.DeleteApplicationRequest{Name: name}); err == nil {
						_ = op.Wait(h.Context())
					}
				})
			}
			got, err := clients["grpc"].GetApplication(ctx, &apphubpb.GetApplicationRequest{Name: host + "us/applications/" + us})
			if err != nil || got.GetScope().GetType() != appHubMultiRegional {
				t.Fatalf("GetApplication: %v, %v", got, err)
			}
			ds, err := c.LookupDiscoveredService(ctx, &apphubpb.LookupDiscoveredServiceRequest{Parent: host + "us-central1", Uri: "//run.googleapis.com/" + svcName})
			if err != nil || ds.GetDiscoveredService() == nil {
				t.Fatalf("LookupDiscoveredService: %v, %v", ds, err)
			}
			reg := func(app string) error {
				op, err := c.CreateService(ctx, &apphubpb.CreateServiceRequest{Parent: app, ServiceId: "hello",
					Service: &apphubpb.Service{DiscoveredService: ds.GetDiscoveredService().GetName()}})
				if err != nil {
					return err
				}
				_, err = op.Wait(ctx)
				return err
			}
			if err := reg(host + "eu/applications/" + eu); pmCode(err) != codes.InvalidArgument {
				t.Errorf("a us-central1 service in an eu application: %v; want INVALID_ARGUMENT", err)
			}
			if err := reg(host + "us/applications/" + us); err != nil {
				t.Errorf("a us-central1 service in a us application: %v", err)
			}
		})
	}
}

// TestAppHubDiscoversGKEAndWorkerPools (#2110), with the official App Hub,
// Kubernetes Engine and Cloud Run clients, as supported-resources lists
// them: a Cloud Run worker pool is a discovered workload
// (//run.googleapis.com/.../workerPools/P); on an Autopilot cluster the
// Kubernetes Engine API makes, a Deployment, a StatefulSet and a Service
// applied with kubectl are discovered (the workloads under
// .../clusters/C/k8s/namespaces/N/apps/{deployments,statefulsets}/X, the
// Service under .../k8s/namespaces/N/services/X), the kube-system ones
// and the default kubernetes Service are not, and the Deployment is
// registered to a global application as a workload.
func TestAppHubDiscoversGKEAndWorkerPools(t *testing.T) {
	h := New(t)
	c := appHubClients(t, h)["grpc"]
	h.Endpoint(EnvRun)
	gkeAddr := runShardEndpoint(h, "CLOUDBURROW_TEST_RUN_GKE", EnvGKE)
	if _, err := exec.LookPath("kubectl"); err != nil {
		t.Skip("kubectl is required to apply the workloads")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	project := h.Project()
	host := "projects/" + project + "/locations/"
	detachAppHubProject(t, h, c, project)
	op, err := c.CreateServiceProjectAttachment(ctx, &apphubpb.CreateServiceProjectAttachmentRequest{Parent: host + "global",
		ServiceProjectAttachmentId: project, ServiceProjectAttachment: &apphubpb.ServiceProjectAttachment{ServiceProject: "projects/" + project}})
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	suffix := time.Now().UnixNano() % 1e6

	// A worker pool.
	wp, err := run.NewWorkerPoolsClient(ctx, runClientOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = wp.Close() })
	pool := fmt.Sprintf("%sus-central1/workerPools/ah-pool-%d", host, suffix)
	t.Cleanup(func() {
		if op, err := wp.DeleteWorkerPool(h.Context(), &runpb.DeleteWorkerPoolRequest{Name: pool}); err == nil {
			_, _ = op.Wait(h.Context())
		}
	})
	one := int32(1)
	pop, err := wp.CreateWorkerPool(ctx, &runpb.CreateWorkerPoolRequest{Parent: host + "us-central1", WorkerPoolId: fmt.Sprintf("ah-pool-%d", suffix),
		WorkerPool: &runpb.WorkerPool{Template: &runpb.WorkerPoolRevisionTemplate{Containers: sleeper("apphub")},
			Scaling: &runpb.WorkerPoolScaling{ManualInstanceCount: &one}}})
	if err != nil {
		t.Fatalf("CreateWorkerPool: %v", err)
	}
	if _, err := pop.Wait(ctx); err != nil {
		t.Fatalf("CreateWorkerPool's operation: %v", err)
	}
	dw, err := c.LookupDiscoveredWorkload(ctx, &apphubpb.LookupDiscoveredWorkloadRequest{Parent: host + "us-central1", Uri: "//run.googleapis.com/" + pool})
	if err != nil || dw.GetDiscoveredWorkload().GetWorkloadProperties().GetLocation() != "us-central1" {
		t.Fatalf("the worker pool's discovered workload: %v, %v", dw, err)
	}

	// A GKE cluster with a Deployment, a StatefulSet and a Service.
	cm, err := container.NewClusterManagerClient(ctx, option.WithEndpoint(gkeAddr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cm.Close() })
	clusterName := host + "us-central1/clusters/ah-gke"
	cop, err := cm.CreateCluster(ctx, &containerpb.CreateClusterRequest{Parent: host + "us-central1",
		Cluster: &containerpb.Cluster{Name: "ah-gke", Autopilot: &containerpb.Autopilot{Enabled: true},
			MasterAuth: &containerpb.MasterAuth{ClientCertificateConfig: &containerpb.ClientCertificateConfig{IssueClientCertificate: true}}}})
	if err != nil {
		t.Fatalf("CreateCluster: %v", err)
	}
	t.Cleanup(func() {
		dctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if dop, err := cm.DeleteCluster(dctx, &containerpb.DeleteClusterRequest{Name: clusterName}); err == nil {
			gkeWait(t, cm, project, "us-central1", dop)
		}
	})
	gkeWait(t, cm, project, "us-central1", cop)
	cluster, err := cm.GetCluster(ctx, &containerpb.GetClusterRequest{Name: clusterName})
	if err != nil {
		t.Fatalf("GetCluster: %v", err)
	}
	kubeconfig := gkeKubeconfig(t, cluster)
	// The pause image every kind node holds: no pull, and nothing needs to
	// run for App Hub to see the objects.
	manifest := `apiVersion: v1
kind: Namespace
metadata: {name: shop}
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: web, namespace: shop}
spec:
  replicas: 1
  selector: {matchLabels: {app: web}}
  template:
    metadata: {labels: {app: web}}
    spec: {containers: [{name: web, image: registry.k8s.io/pause:3.10, imagePullPolicy: IfNotPresent}]}
---
apiVersion: apps/v1
kind: StatefulSet
metadata: {name: db, namespace: shop}
spec:
  serviceName: web
  replicas: 1
  selector: {matchLabels: {app: db}}
  template:
    metadata: {labels: {app: db}}
    spec: {containers: [{name: db, image: registry.k8s.io/pause:3.10, imagePullPolicy: IfNotPresent}]}
---
apiVersion: v1
kind: Service
metadata: {name: web, namespace: shop}
spec:
  selector: {app: web}
  ports: [{port: 80}]
`
	cmd := exec.CommandContext(ctx, "kubectl", "--kubeconfig", kubeconfig, "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(manifest)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("kubectl apply: %v\n%s", err, b)
	}
	k8s := "//container.googleapis.com/" + clusterName + "/k8s/namespaces/"
	var uris []string
	it := c.ListDiscoveredWorkloads(ctx, &apphubpb.ListDiscoveredWorkloadsRequest{Parent: host + "us-central1"})
	for {
		w, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListDiscoveredWorkloads: %v", err)
		}
		uris = append(uris, w.GetWorkloadReference().GetUri())
	}
	all := "\n" + strings.Join(uris, "\n") + "\n"
	for _, want := range []string{k8s + "shop/apps/deployments/web", k8s + "shop/apps/statefulsets/db", "//run.googleapis.com/" + pool} {
		if !strings.Contains(all, "\n"+want+"\n") {
			t.Errorf("discovered workloads lack %s:\n%s", want, all)
		}
	}
	if strings.Contains(all, "/namespaces/kube-system/") {
		t.Errorf("kube-system's workloads are discovered:\n%s", all)
	}
	svc, err := c.LookupDiscoveredService(ctx, &apphubpb.LookupDiscoveredServiceRequest{Parent: host + "us-central1", Uri: k8s + "shop/services/web"})
	if err != nil || svc.GetDiscoveredService() == nil {
		t.Fatalf("the Kubernetes Service's discovered service: %v, %v", svc, err)
	}
	if none, err := c.LookupDiscoveredService(ctx, &apphubpb.LookupDiscoveredServiceRequest{Parent: host + "us-central1",
		Uri: k8s + "default/services/kubernetes"}); err != nil || none.GetDiscoveredService() != nil {
		t.Errorf("the API server's own Service: %v, %v", none, err)
	}
	dep, err := c.LookupDiscoveredWorkload(ctx, &apphubpb.LookupDiscoveredWorkloadRequest{Parent: host + "us-central1", Uri: k8s + "shop/apps/deployments/web"})
	if err != nil || dep.GetDiscoveredWorkload() == nil {
		t.Fatalf("LookupDiscoveredWorkload: %v, %v", dep, err)
	}
	app := fmt.Sprintf("ah-gke-%d", suffix)
	aop, err := c.CreateApplication(ctx, &apphubpb.CreateApplicationRequest{Parent: host + "global", ApplicationId: app,
		Application: &apphubpb.Application{Scope: &apphubpb.Scope{Type: apphubpb.Scope_GLOBAL}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := aop.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	appName := host + "global/applications/" + app
	t.Cleanup(func() {
		if op, err := c.DeleteWorkload(h.Context(), &apphubpb.DeleteWorkloadRequest{Name: appName + "/workloads/web"}); err == nil {
			_ = op.Wait(h.Context())
		}
		if op, err := c.DeleteApplication(h.Context(), &apphubpb.DeleteApplicationRequest{Name: appName}); err == nil {
			_ = op.Wait(h.Context())
		}
	})
	wop, err := c.CreateWorkload(ctx, &apphubpb.CreateWorkloadRequest{Parent: appName, WorkloadId: "web",
		Workload: &apphubpb.Workload{DiscoveredWorkload: dep.GetDiscoveredWorkload().GetName()}})
	if err != nil {
		t.Fatalf("CreateWorkload: %v", err)
	}
	w, err := wop.Wait(ctx)
	if err != nil || w.GetWorkloadReference().GetUri() != k8s+"shop/apps/deployments/web" || w.GetState() != apphubpb.Workload_ACTIVE {
		t.Fatalf("the registered Deployment: %v, %v", w, err)
	}
}
