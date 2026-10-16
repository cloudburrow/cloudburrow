package main

// Where Cloud Logging sinks deliver beyond log buckets (#1404): the
// instance's Cloud Storage (storageObjectWriter, logging.go), Pub/Sub and
// BigQuery, each reached through its tunnel with its official client, as an
// application would reach it.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"cloud.google.com/go/bigquery"
	"cloud.google.com/go/pubsub/v2"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/netfwd"
	"github.com/cloudburrow/cloudburrow/internal/service/logging"
)

// destinations are the writers for the services this instance serves.
func (s *loggingService) destinations() logging.Destinations {
	var d logging.Destinations
	if serviceEnabled(s.cfg, config.ServiceStorage) {
		src := storageSourceFetcher{cfg: s.cfg}
		if s.storage != nil {
			src.addr = func() string {
				if f := s.storage(); f != nil {
					return f.HostAddr()
				}
				return ""
			}
		}
		d.Storage = storageObjectWriter{src: src, client: &http.Client{Timeout: 2 * time.Minute}}
	}
	if serviceEnabled(s.cfg, config.ServicePubSub) && s.pubsub != nil {
		d.PubSub = sinkPublisher{tunnel: s.pubsub}
	}
	if serviceEnabled(s.cfg, config.ServiceBigQuery) && s.bigquery != nil {
		d.BigQuery = sinkTables{tunnel: s.bigquery}
	}
	return d
}

// sinkPublisher publishes a Pub/Sub sink's messages.
type sinkPublisher struct{ tunnel func() *netfwd.Forwarder }

func (p sinkPublisher) Publish(ctx context.Context, topic string, msgs []logging.Message) error {
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
	for len(msgs) > 0 {
		n := min(len(msgs), 1000) // Pub/Sub's documented messages per publish
		req := &pubsubpb.PublishRequest{Topic: topic}
		for _, m := range msgs[:n] {
			req.Messages = append(req.Messages, &pubsubpb.PubsubMessage{Data: m.Data, Attributes: m.Attributes})
		}
		if _, err := c.TopicAdminClient.Publish(ctx, req); err != nil {
			if status.Code(err) == codes.NotFound {
				return fmt.Errorf("topic %s: %w", topic, logging.ErrDestinationNotFound)
			}
			return err
		}
		msgs = msgs[n:]
	}
	return nil
}

// sinkTables streams a BigQuery sink's rows.
type sinkTables struct{ tunnel func() *netfwd.Forwarder }

func (t sinkTables) client(ctx context.Context, project string) (*bigquery.Client, error) {
	f := t.tunnel()
	if f == nil || f.HostAddr() == "" {
		return nil, errors.New("BigQuery has not started on this instance")
	}
	return bigquery.NewClient(ctx, project, option.WithEndpoint("http://"+f.HostAddr()), option.WithoutAuthentication())
}

func isNotFound(err error) bool {
	var ge *googleapi.Error
	return errors.As(err, &ge) && ge.Code == http.StatusNotFound
}

func (t sinkTables) TableSchema(ctx context.Context, project, dataset, table string) ([]logging.Field, bool, error) {
	c, err := t.client(ctx, project)
	if err != nil {
		return nil, false, err
	}
	defer c.Close()
	ds := c.Dataset(dataset)
	if _, err := ds.Metadata(ctx); err != nil {
		if isNotFound(err) {
			return nil, false, fmt.Errorf("dataset %s:%s: %w", project, dataset, logging.ErrDestinationNotFound)
		}
		return nil, false, err
	}
	md, err := ds.Table(table).Metadata(ctx)
	if err != nil {
		if isNotFound(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return fromSchema(md.Schema), true, nil
}

func fromSchema(s bigquery.Schema) []logging.Field {
	out := make([]logging.Field, 0, len(s))
	for _, f := range s {
		out = append(out, logging.Field{Name: f.Name, Type: string(f.Type), Repeated: f.Repeated, Fields: fromSchema(f.Schema)})
	}
	return out
}

func toSchema(fs []logging.Field) bigquery.Schema {
	out := make(bigquery.Schema, 0, len(fs))
	for _, f := range fs {
		out = append(out, &bigquery.FieldSchema{Name: f.Name, Type: bigquery.FieldType(f.Type), Repeated: f.Repeated, Schema: toSchema(f.Fields)})
	}
	return out
}

func (t sinkTables) CreateTable(ctx context.Context, project, dataset, table string, fields []logging.Field, partitioned bool) error {
	c, err := t.client(ctx, project)
	if err != nil {
		return err
	}
	defer c.Close()
	md := &bigquery.TableMetadata{Schema: toSchema(fields)}
	if partitioned {
		md.TimePartitioning = &bigquery.TimePartitioning{Type: bigquery.DayPartitioningType, Field: "timestamp"}
	}
	return c.Dataset(dataset).Table(table).Create(ctx, md)
}

func (t sinkTables) ExtendTable(ctx context.Context, project, dataset, table string, fields []logging.Field) error {
	c, err := t.client(ctx, project)
	if err != nil {
		return err
	}
	defer c.Close()
	_, err = c.Dataset(dataset).Table(table).Update(ctx, bigquery.TableMetadataToUpdate{Schema: toSchema(fields)}, "")
	return err
}

// sinkRow is one row with its insert ID.
type sinkRow struct {
	values map[string]any
	id     string
}

func (r sinkRow) Save() (map[string]bigquery.Value, string, error) {
	m := make(map[string]bigquery.Value, len(r.values))
	for k, v := range r.values {
		m[k] = v
	}
	return m, r.id, nil
}

func (t sinkTables) InsertRows(ctx context.Context, project, dataset, table string, rows []map[string]any, ids []string) error {
	c, err := t.client(ctx, project)
	if err != nil {
		return err
	}
	defer c.Close()
	savers := make([]bigquery.ValueSaver, len(rows))
	for i := range rows {
		savers[i] = sinkRow{values: rows[i], id: ids[i]}
	}
	return c.Dataset(dataset).Table(table).Inserter().Put(ctx, savers)
}

// linkedTables makes and fills Log Analytics linked datasets (#1423). Its
// requests carry logging.LinkedWriterHeader, the only writes the BigQuery
// front lets into a linked dataset (bigqueryfront/loglinks.go).
type linkedTables struct{ tunnel func() *netfwd.Forwarder }

// linkedTransport adds logging.LinkedWriterHeader to each request.
type linkedTransport struct{ next http.RoundTripper }

func (t linkedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set(logging.LinkedWriterHeader, "1")
	return t.next.RoundTrip(r)
}

func (t linkedTables) client(ctx context.Context, project string) (*bigquery.Client, error) {
	f := t.tunnel()
	if f == nil || f.HostAddr() == "" {
		return nil, errors.New("BigQuery has not started on this instance")
	}
	hc := &http.Client{Transport: linkedTransport{next: http.DefaultTransport}, Timeout: 2 * time.Minute}
	return bigquery.NewClient(ctx, project, option.WithEndpoint("http://"+f.HostAddr()), option.WithHTTPClient(hc))
}

func (t linkedTables) Create(ctx context.Context, project, dataset, description string, fields []logging.Field) error {
	c, err := t.client(ctx, project)
	if err != nil {
		return err
	}
	defer c.Close()
	ds := c.Dataset(dataset)
	if err := ds.Create(ctx, &bigquery.DatasetMetadata{Description: description,
		Labels: map[string]string{logging.LinkedDatasetLabel: "true"}}); err != nil {
		var ge *googleapi.Error
		if errors.As(err, &ge) && ge.Code == http.StatusConflict {
			return fmt.Errorf("%s:%s: %w", project, dataset, logging.ErrDatasetExists)
		}
		if errors.As(err, &ge) && ge.Code == http.StatusNotFound {
			return fmt.Errorf("%s: %w", project, logging.ErrProjectNotServed)
		}
		return err
	}
	if err := ds.Table(logging.LinkedEntriesTable).Create(ctx, &bigquery.TableMetadata{Schema: toSchema(fields),
		Description: "The log bucket's entries, which the _AllLogs view reads."}); err != nil {
		_ = ds.DeleteWithContents(ctx)
		return err
	}
	view := fmt.Sprintf("SELECT * FROM `%s.%s.%s`", project, dataset, logging.LinkedEntriesTable)
	if err := ds.Table("_AllLogs").Create(ctx, &bigquery.TableMetadata{ViewQuery: view}); err != nil {
		_ = ds.DeleteWithContents(ctx)
		return err
	}
	return nil
}

func (t linkedTables) Append(ctx context.Context, project, dataset string, rows []map[string]any, ids []string) error {
	c, err := t.client(ctx, project)
	if err != nil {
		return err
	}
	defer c.Close()
	savers := make([]bigquery.ValueSaver, len(rows))
	for i := range rows {
		savers[i] = sinkRow{values: rows[i], id: ids[i]}
	}
	return c.Dataset(dataset).Table(logging.LinkedEntriesTable).Inserter().Put(ctx, savers)
}

func (t linkedTables) Delete(ctx context.Context, project, dataset string) error {
	c, err := t.client(ctx, project)
	if err != nil {
		return err
	}
	defer c.Close()
	if err := c.Dataset(dataset).DeleteWithContents(ctx); err != nil && !isNotFound(err) {
		return err
	}
	return nil
}
