package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	pubsub "cloud.google.com/go/pubsub/v2"
	pubsubpb "cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/cloudburrow/cloudburrow/internal/components"
	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/lifecycle"
	"github.com/cloudburrow/cloudburrow/internal/service/storagenotify"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// notifyService serves the Cloud Storage notificationConfigs API in front of
// the storage backend, and routes the backend's events to the topics callers
// registered.
//
// The API has to be in front rather than beside: official clients send every
// Storage call to one endpoint, so publishing notificationConfigs on a second
// port would give CloudBurrow a shape no Google endpoint has.
type notifyService struct {
	cfg config.Config
	// backend is the address of the tunnel to the in-cluster storage
	// Service. Requests that are not notification calls are forwarded there
	// unchanged.
	backend string
	// pubsubAddr is the host address of the Pub/Sub emulator.
	pubsubAddr string

	configs *storagenotify.Store
	router  *storagenotify.Router
	db      store.Store

	mu     sync.Mutex
	ln     net.Listener
	srv    *http.Server
	client *pubsub.Client
	done   chan struct{}
	out    io.Writer
}

// newNotifyService returns the service, or nil when either Cloud Storage or
// Pub/Sub is disabled.
//
// Both are required and neither is optional: notifications are storage events
// delivered over Pub/Sub, so with one of them missing there is nothing to
// serve rather than a degraded version of it.
func newNotifyService(cfg config.Config, out io.Writer) *notifyService {
	var hasStorage, hasPubSub bool
	for _, s := range cfg.EnabledServices() {
		switch s {
		case config.ServiceStorage:
			hasStorage = true
		case config.ServicePubSub:
			hasPubSub = true
		}
	}
	if !hasStorage || !hasPubSub {
		return nil
	}
	return &notifyService{cfg: cfg, out: out}
}

func (n *notifyService) register(coord *lifecycle.Coordinator) {
	if n == nil {
		return
	}
	coord.Register(n)
}

func (n *notifyService) Name() string { return "storage-notify" }

// SetBackend records the address requests are forwarded to.
func (n *notifyService) SetBackend(addr string) {
	if n == nil {
		return
	}
	n.backend = addr
}

// SetPubSub records the Pub/Sub endpoint used for routing.
func (n *notifyService) SetPubSub(addr string) {
	if n == nil {
		return
	}
	n.pubsubAddr = addr
}

// Addr returns the address the fronted storage endpoint listens on.
func (n *notifyService) Addr() string {
	if n == nil {
		return ""
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.ln == nil {
		return ""
	}
	return n.ln.Addr().String()
}

// Configs exposes the configuration store, for admin reset.
func (n *notifyService) Configs() *storagenotify.Store {
	if n == nil {
		return nil
	}
	return n.configs
}

// internalSubscription is the subscription the router reads from.
const internalSubscription = "cloudburrow-storage-events-router"

func (n *notifyService) Start(ctx context.Context) error {
	if n.backend == "" || n.pubsubAddr == "" {
		return errors.New("storage notifications need both a storage tunnel and a Pub/Sub endpoint")
	}

	var db store.Store = store.NewMemory()
	if n.cfg.Mode == config.ModePersistent {
		durable, err := store.OpenDurable(filepath.Join(n.cfg.StateDir, n.cfg.Name, "storage-notify"))
		if err != nil {
			return fmt.Errorf("open notification configuration state: %w", err)
		}
		db = durable
	}
	n.db = db
	n.configs = storagenotify.NewStore(db)

	backendURL, err := url.Parse("http://" + n.backend)
	if err != nil {
		_ = db.Close()
		return fmt.Errorf("parse storage backend address %q: %w", n.backend, err)
	}

	addr := net.JoinHostPort(n.cfg.BindAddress, strconv.Itoa(n.cfg.Endpoints.Storage))
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		_ = db.Close()
		return fmt.Errorf("bind storage port %s: %w", addr, err)
	}

	srv := &http.Server{
		Handler:           storagenotify.NewHandler(n.configs, backendURL),
		ReadHeaderTimeout: storagenotify.ProxyReadHeaderTimeout,
		IdleTimeout:       storagenotify.ProxyIdleTimeout,
	}
	done := make(chan struct{})

	n.mu.Lock()
	n.ln, n.srv, n.done = ln, srv, done
	n.mu.Unlock()

	go func() {
		defer close(done)
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			_ = err
		}
	}()

	// Routing is best-effort at startup: the storage endpoint must serve
	// whether or not Pub/Sub is reachable yet, and a failure here is reported
	// rather than taking the whole instance down over a feature the developer
	// may not be using.
	if err := n.startRouter(ctx); err != nil {
		n.logf("storage notifications: routing is not active: %v\n", err)
	}
	return nil
}

func (n *notifyService) startRouter(ctx context.Context) error {
	project := components.EventProject()

	client, err := pubsub.NewClient(ctx, project,
		option.WithEndpoint(n.pubsubAddr),
		option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())),
	)
	if err != nil {
		return fmt.Errorf("connect to Pub/Sub at %s: %w", n.pubsubAddr, err)
	}

	topicName := storagenotify.InternalTopicName(project, components.EventTopic)
	if _, err := client.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: topicName}); err != nil &&
		status.Code(err) != codes.AlreadyExists {
		_ = client.Close()
		return fmt.Errorf("create internal event topic: %w", err)
	}

	subName := fmt.Sprintf("projects/%s/subscriptions/%s", project, internalSubscription)
	if _, err := client.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{
		Name:  subName,
		Topic: topicName,
		// A short ack deadline: routing is a local republish, so a message
		// that is not acked quickly has failed rather than being slow.
		AckDeadlineSeconds: 20,
	}); err != nil && status.Code(err) != codes.AlreadyExists {
		_ = client.Close()
		return fmt.Errorf("create internal event subscription: %w", err)
	}

	n.mu.Lock()
	n.client = client
	n.mu.Unlock()

	src := &pubsubSource{client: client, subscription: subName}
	pub := &pubsubPublisher{client: client}
	n.router = storagenotify.NewRouter(n.configs, src, pub, func(err error) {
		n.logf("storage notifications: %v\n", err)
	})
	return n.router.Start(ctx)
}

func (n *notifyService) logf(format string, a ...any) {
	if n.out != nil {
		fmt.Fprintf(n.out, format, a...)
	}
}

func (n *notifyService) Stop(ctx context.Context) error {
	n.mu.Lock()
	srv, done, client := n.srv, n.done, n.client
	n.srv, n.client = nil, nil
	router := n.router
	n.mu.Unlock()

	var err error
	if router != nil {
		_ = router.Stop(ctx)
	}
	if srv != nil {
		err = srv.Shutdown(ctx)
		if err != nil {
			_ = srv.Close()
		}
		if done != nil {
			select {
			case <-done:
			case <-ctx.Done():
			}
		}
	}
	if client != nil {
		_ = client.Close()
	}
	if n.db != nil {
		if closeErr := n.db.Close(); err == nil {
			err = closeErr
		}
	}
	return err
}

// pubsubSource receives events from the internal subscription.
type pubsubSource struct {
	client       *pubsub.Client
	subscription string
}

func (p *pubsubSource) Receive(ctx context.Context, fn func(storagenotify.Message)) error {
	sub := p.client.Subscriber(p.subscription)
	return sub.Receive(ctx, func(_ context.Context, m *pubsub.Message) {
		// Acked before routing rather than after. A message that cannot be
		// routed will not route on redelivery either — the configuration is
		// what it is — so redelivering it would loop forever on a permanent
		// failure. The failure is counted and reported instead.
		m.Ack()
		fn(storagenotify.Message{Attributes: m.Attributes, Data: m.Data})
	})
}

// pubsubPublisher republishes a routed event to a caller's topic.
type pubsubPublisher struct {
	client *pubsub.Client
}

func (p *pubsubPublisher) Publish(ctx context.Context, topic string, data []byte, attrs map[string]string) error {
	pubCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	publisher := p.client.Publisher(topic)
	defer publisher.Stop()

	result := publisher.Publish(pubCtx, &pubsub.Message{Data: data, Attributes: attrs})
	_, err := result.Get(pubCtx)
	return err
}
