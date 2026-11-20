package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/pubsub/v2"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/netfwd"
	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
	"github.com/cloudburrow/cloudburrow/internal/service/storagejobs"
)

// The Cloud Storage companion APIs (#1957), keyed for gcloud-setup and
// terraform.
const (
	storageBatchService    config.Service = "storagebatchoperations"
	storageInsightsService config.Service = "storageinsights"
	storageTransferService config.Service = "storagetransfer"
)

// storageJobs is the three APIs, served over the instance's buckets when
// Cloud Storage is enabled; nil otherwise.
type storageJobs struct {
	rt       *storagejobs.Runtime
	batch    *storagejobs.BatchServer
	insights *storagejobs.InsightsServer
	transfer *storagejobs.TransferServer
}

// storageJobsEnabled reports whether the companions are served.
func storageJobsEnabled(cfg config.Config) bool {
	return serviceEnabled(cfg, config.ServiceStorage)
}

// newStorageJobs builds the three servers. storage is the Cloud Storage
// tunnel, pubsub Pub/Sub's and bq BigQuery's, read at each call; projects
// scopes Storage Insights dataset configurations (#1976), nil for none.
func newStorageJobs(cfg config.Config, storage, pubsubTunnel, bq func() *netfwd.Forwarder,
	projects *resourcemanager.Registry) *storageJobs {
	if !storageJobsEnabled(cfg) {
		return nil
	}
	src := storageSourceFetcher{cfg: cfg}
	if storage != nil {
		src.addr = func() string {
			if f := storage(); f != nil {
				return f.HostAddr()
			}
			return ""
		}
	}
	gcs := &storagejobs.GCS{Base: src.base, Client: &http.Client{Timeout: 30 * time.Minute}}
	var pub storagejobs.Publisher
	if serviceEnabled(cfg, config.ServicePubSub) && pubsubTunnel != nil {
		pub = transferPublisher{tunnel: pubsubTunnel}
	}
	state := ""
	if cfg.StateDir != "" {
		state = filepath.Join(cfg.StateDir, "storagejobs")
	}
	rt := storagejobs.NewRuntime(gcs, pub, state)
	if serviceEnabled(cfg, config.ServiceBigQuery) && bq != nil {
		rt.Linked = insightsLinks{tunnel: bq}
	}
	if projects != nil {
		rt.Projects = insightsProjects{r: projects}
	}
	addr := func(port int) string { return net.JoinHostPort(cfg.BindAddress, strconv.Itoa(port)) }
	j := &storageJobs{rt: rt}
	if cfg.Endpoints.StorageBatchOperations >= 0 {
		j.batch = storagejobs.NewBatchServer(addr(cfg.Endpoints.StorageBatchOperations), rt)
	}
	if cfg.Endpoints.StorageInsights >= 0 {
		j.insights = storagejobs.NewInsightsServer(addr(cfg.Endpoints.StorageInsights), rt)
	}
	if cfg.Endpoints.StorageTransfer >= 0 {
		j.transfer = storagejobs.NewTransferServer(addr(cfg.Endpoints.StorageTransfer), rt)
	}
	return j
}

// servers are the servers to start, in order.
func (j *storageJobs) servers() []interface {
	Name() string
	Start(context.Context) error
	Stop(context.Context) error
	Addr() string
} {
	if j == nil {
		return nil
	}
	var out []interface {
		Name() string
		Start(context.Context) error
		Stop(context.Context) error
		Addr() string
	}
	if j.batch != nil {
		out = append(out, j.batch)
	}
	if j.insights != nil {
		out = append(out, j.insights)
	}
	if j.transfer != nil {
		out = append(out, j.transfer)
	}
	return out
}

// live adds the bound addresses to live, by endpoint name.
func (j *storageJobs) live(live map[string]string) {
	if j == nil {
		return
	}
	if j.batch != nil && j.batch.Addr() != "" {
		live["storagebatchoperations"] = j.batch.Addr()
	}
	if j.insights != nil && j.insights.Addr() != "" {
		live["storageinsights"] = j.insights.Addr()
	}
	if j.transfer != nil && j.transfer.Addr() != "" {
		live["storagetransfer"] = j.transfer.Addr()
	}
}

// storageJobsResetter forgets each project's batch jobs, report
// configurations, transfer jobs and agent pools (#1957).
type storageJobsResetter struct {
	jobs func() *storageJobs
}

func (r *storageJobsResetter) Name() string { return "storagejobs" }

func (r *storageJobsResetter) Reset(ctx context.Context) error { return r.ResetProject(ctx, "") }

// ResetProject forgets one project's, or every project's for "".
func (r *storageJobsResetter) ResetProject(_ context.Context, project string) error {
	j := r.jobs()
	if j == nil {
		return nil
	}
	var errs []error
	if j.batch != nil {
		errs = append(errs, j.batch.ResetProject(project))
	}
	if j.insights != nil {
		errs = append(errs, j.insights.ResetProject(project))
	}
	if j.transfer != nil {
		errs = append(errs, j.transfer.ResetProject(project))
	}
	return errors.Join(errs...)
}

// transferPublisher publishes Storage Transfer notifications to the
// instance's Pub/Sub.
type transferPublisher struct{ tunnel func() *netfwd.Forwarder }

func (p transferPublisher) Publish(ctx context.Context, topic string, data []byte, attrs map[string]string) error {
	f := p.tunnel()
	if f == nil || f.HostAddr() == "" {
		return errors.New("Pub/Sub has not started on this instance")
	}
	project := strings.TrimPrefix(topic, "projects/")
	project, _, _ = strings.Cut(project, "/")
	c, err := pubsub.NewClient(ctx, project, option.WithEndpoint(f.HostAddr()), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		return err
	}
	defer c.Close()
	if _, err := c.TopicAdminClient.Publish(ctx, &pubsubpb.PublishRequest{Topic: topic,
		Messages: []*pubsubpb.PubsubMessage{{Data: data, Attributes: attrs}}}); err != nil {
		return fmt.Errorf("publishing to %s: %w", topic, err)
	}
	return nil
}
