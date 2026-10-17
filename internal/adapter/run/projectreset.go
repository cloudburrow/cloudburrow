package run

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/grpc/codes"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/resource"
)

// A project's reset (#1540): what Google drops when a project is deleted
// and purged, here when it is purged or `/admin/reset?project=` names it.
// Every Cloud Run workload of the project goes the way its own Delete
// removes it (its Knative Service, Deployment, Jobs and ConfigMaps, and so
// its IAM policy, which its owner's deletion takes), and every Cloud
// Functions function, with its Eventarc trigger's subscription, topic and
// notification, and the Cloud Build builds, logs, triggers and worker pools
// kept in this process under the project's ID or number.

// projectPrefixes are the name prefixes a project's resources carry: its ID
// and its number, which Cloud Functions' builds are named by.
func projectPrefixes(project string) []string {
	return []string{"projects/" + project + "/", "projects/" + strconv.FormatInt(resource.ProjectNumber(project), 10) + "/"}
}

func ofProject(name string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// ResetProject deletes one project's Cloud Run services, jobs (with their
// executions), worker pools and instances, and its functions when fns is
// not nil (fns tears down their event triggers). It also forgets the
// project's Cloud Build state. Every resource is attempted; the failures
// are returned together.
func (s *Server) ResetProject(ctx context.Context, project string, fns *FunctionsServer) error {
	if project == "" {
		return errors.New("a project is required")
	}
	prefixes := projectPrefixes(project)
	var errs []error
	notFound := func(err error) bool { return err == nil || apierror.From(err).Code == codes.NotFound }

	// Functions first: each owns a service, which its deletion removes.
	if fns != nil {
		errs = append(errs, fns.resetProject(ctx, prefixes)...)
	} else {
		dropFunctions(prefixes)
	}

	items, err := s.kn.List(ctx)
	if err != nil {
		errs = append(errs, fmt.Errorf("list services: %w", err))
	}
	for _, k := range items {
		name := k.Metadata.Annotations["cloudburrow.dev/cloud-run-name"]
		if !ofProject(name, prefixes) {
			continue
		}
		if _, err := s.DeleteService(ctx, &runpb.DeleteServiceRequest{Name: name}); !notFound(err) {
			errs = append(errs, fmt.Errorf("delete %s: %w", name, err))
		}
	}
	jobs, err := listObjects[kconfigMap](ctx, s.kn, resConfigMap, labelOwned+"=true,"+labelKind+"="+kindRunJob)
	if err != nil {
		errs = append(errs, fmt.Errorf("list jobs: %w", err))
	}
	for _, cm := range jobs {
		name := cm.Metadata.Annotations[annRunName]
		if !ofProject(name, prefixes) {
			continue
		}
		if _, err := s.Jobs().DeleteJob(ctx, &runpb.DeleteJobRequest{Name: name}); !notFound(err) {
			errs = append(errs, fmt.Errorf("delete %s: %w", name, err))
		}
	}
	for _, kind := range []string{kindWorkerPool, kindInstance} {
		cms, err := s.specsOfKind(ctx, kind)
		if err != nil {
			errs = append(errs, fmt.Errorf("list %ss: %w", kind, err))
			continue
		}
		for _, cm := range cms {
			name := cm.Metadata.Annotations[annRunName]
			if !ofProject(name, prefixes) {
				continue
			}
			var err error
			if kind == kindWorkerPool {
				_, err = s.WorkerPools().DeleteWorkerPool(ctx, &runpb.DeleteWorkerPoolRequest{Name: name})
			} else {
				_, err = s.Instances().DeleteInstance(ctx, &runpb.DeleteInstanceRequest{Name: name})
			}
			if !notFound(err) {
				errs = append(errs, fmt.Errorf("delete %s: %w", name, err))
			}
		}
	}
	resetCloudBuildProject(prefixes)
	return errors.Join(errs...)
}

// resetProject deletes the functions under prefixes. One mid-deploy is
// deleted too: the project is going whatever it was doing.
func (f *FunctionsServer) resetProject(ctx context.Context, prefixes []string) []error {
	functions.mu.Lock()
	var gone []*storedFunction
	for name, sf := range functions.fns {
		if ofProject(name, prefixes) {
			gone = append(gone, sf)
			delete(functions.fns, name)
		}
	}
	functions.mu.Unlock()
	var errs []error
	for _, sf := range gone {
		f.teardown(ctx, sf.trigger)
		svc := sf.fn.GetServiceConfig().GetService()
		if svc == "" {
			continue
		}
		if _, err := f.s.DeleteService(ctx, &runpb.DeleteServiceRequest{Name: svc}); err != nil && apierror.From(err).Code != codes.NotFound {
			errs = append(errs, fmt.Errorf("delete %s's service: %w", sf.fn.GetName(), err))
		}
	}
	return errs
}

// dropFunctions forgets the functions under prefixes, for an adapter with
// no functions server: their services go with the project's others.
func dropFunctions(prefixes []string) {
	functions.mu.Lock()
	defer functions.mu.Unlock()
	for name := range functions.fns {
		if ofProject(name, prefixes) {
			delete(functions.fns, name)
		}
	}
}

// resetCloudBuildProject cancels the project's running builds and forgets
// its builds, their logs and retry inputs, pending approvals, triggers and
// worker pools.
func resetCloudBuildProject(prefixes []string) {
	runningBuilds.mu.Lock()
	for name, cancel := range runningBuilds.cancel {
		if ofProject(name, prefixes) {
			cancel()
			delete(runningBuilds.cancel, name)
		}
	}
	runningBuilds.mu.Unlock()

	cloudBuilds.mu.Lock()
	for name := range cloudBuilds.builds {
		if ofProject(name, prefixes) {
			delete(cloudBuilds.builds, name)
		}
	}
	cloudBuilds.mu.Unlock()

	createdBuilds.mu.Lock()
	for name := range createdBuilds.input {
		if ofProject(name, prefixes) {
			delete(createdBuilds.input, name)
		}
	}
	for name := range createdBuilds.trigger {
		if ofProject(name, prefixes) {
			delete(createdBuilds.trigger, name)
		}
	}
	createdBuilds.mu.Unlock()

	buildLogs.mu.Lock()
	for name := range buildLogs.logs {
		if ofProject(name, prefixes) {
			delete(buildLogs.logs, name)
		}
	}
	for name := range buildLogs.errs {
		if ofProject(name, prefixes) {
			delete(buildLogs.errs, name)
		}
	}
	buildLogs.mu.Unlock()

	pendingBuilds.mu.Lock()
	for name := range pendingBuilds.start {
		if ofProject(name, prefixes) {
			delete(pendingBuilds.start, name)
		}
	}
	pendingBuilds.mu.Unlock()

	pendingOp.mu.Lock()
	for name := range pendingOp.ops {
		if ofProject(name, prefixes) {
			delete(pendingOp.ops, name)
		}
	}
	pendingOp.mu.Unlock()

	triggerStore.mu.Lock()
	for name := range triggerStore.triggers {
		if ofProject(name, prefixes) {
			delete(triggerStore.triggers, name)
		}
	}
	for name := range triggerStore.pools {
		if ofProject(name, prefixes) {
			delete(triggerStore.pools, name)
		}
	}
	for name := range triggerApprovals {
		if ofProject(name, prefixes) {
			delete(triggerApprovals, name)
		}
	}
	triggerStore.mu.Unlock()
}
