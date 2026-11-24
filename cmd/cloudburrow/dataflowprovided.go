package main

import (
	"context"
	"errors"
	"net/http"
	"sync"

	"cloud.google.com/go/storage"
	"google.golang.org/api/googleapi"

	"github.com/cloudburrow/cloudburrow/internal/beamrun"
	"github.com/cloudburrow/cloudburrow/internal/service/dataflow"
)

// providedBucket is the instance's copy of Google's provided-templates
// bucket: gs://dataflow-templates, which every gs://dataflow-templates-REGION
// path also reads (#2215).
const providedBucket = "dataflow-templates"

// providedTemplates stages the Google-provided templates (beamrun's
// build of DataflowTemplates) in the instance's Cloud Storage, each on its
// first use: gs://dataflow-templates/latest/NAME and NAME_metadata, its
// libraries under gs://dataflow-templates/staging/RELEASE/.
type providedTemplates struct {
	p       *beamrun.Provided
	storage *dataprocStorage
	project func() string

	mu sync.Mutex
}

var _ dataflow.Provided = (*providedTemplates)(nil)

func (t *providedTemplates) Stage(ctx context.Context, name string) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	path := "gs://" + providedBucket + "/latest/" + name
	if _, err := t.storage.Read(ctx, path+"_metadata"); err == nil {
		if _, err := t.storage.Read(ctx, path); err == nil {
			return path, nil
		}
	}
	tpl, ok, err := t.p.Template(ctx, name)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", dataflow.ErrNoProvidedTemplate
	}
	if err := t.bucket(ctx); err != nil {
		return "", err
	}
	if err := t.p.Stage(ctx, tpl, t.project(), path, "gs://"+providedBucket+"/staging/"+beamrun.TemplatesRelease+"/"); err != nil {
		return "", err
	}
	if err := t.storage.Write(ctx, path+"_metadata", tpl.Metadata); err != nil {
		return "", err
	}
	return path, nil
}

// bucket makes the templates' bucket, unless it is there.
func (t *providedTemplates) bucket(ctx context.Context) error {
	c, err := t.storage.client(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	if _, err := c.Bucket(providedBucket).Attrs(ctx); err == nil {
		return nil
	}
	err = c.Bucket(providedBucket).Create(ctx, t.project(), &storage.BucketAttrs{Location: "US"})
	var ge *googleapi.Error
	if errors.As(err, &ge) && ge.Code == http.StatusConflict {
		return nil
	}
	return err
}
