package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
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

	// mu guards names, each template's own lock, held while it is staged
	// (which may build its module first), so that one template's staging
	// does not hold up another's.
	mu    sync.Mutex
	names map[string]*sync.Mutex
}

var _ dataflow.Provided = (*providedTemplates)(nil)

func (t *providedTemplates) Stage(ctx context.Context, name string) (string, error) {
	t.mu.Lock()
	if t.names == nil {
		t.names = map[string]*sync.Mutex{}
	}
	l := t.names[name]
	if l == nil {
		l = &sync.Mutex{}
		t.names[name] = l
	}
	t.mu.Unlock()
	l.Lock()
	defer l.Unlock()
	flex := strings.HasPrefix(name, "flex/")
	if _, isFlex := beamrun.ProvidedFlexNames[strings.TrimPrefix(name, "flex/")]; flex != isFlex {
		// A classic template's name under flex/, or a Flex Template's
		// outside it: Google has no such file.
		return "", dataflow.ErrNoProvidedTemplate
	}
	path := "gs://" + providedBucket + "/latest/" + name
	// A template staged by an older launcher (another StageVersion) is
	// staged again: its pipeline options are fixed in the template file.
	if v, err := t.storage.Read(ctx, path+"_cbstage"); err == nil && string(v) == beamrun.StageVersion {
		if _, err := t.storage.Read(ctx, path+"_metadata"); err == nil {
			if _, err := t.storage.Read(ctx, path); err == nil {
				return path, nil
			}
		}
	}
	tpl, ok, err := t.p.Template(ctx, strings.TrimPrefix(name, "flex/"))
	if err != nil {
		return "", err
	}
	if !ok {
		return "", dataflow.ErrNoProvidedTemplate
	}
	if err := t.bucket(ctx); err != nil {
		return "", err
	}
	if tpl.Flex {
		// A Flex Template's spec file (templates/configuring-flex-templates):
		// its launcher image, which stands for the template's build, its
		// SDK and its metadata, as Google's templates-maven-plugin writes
		// it.
		spec, err := json.Marshal(map[string]any{"image": beamrun.ProvidedFlexImage(tpl),
			"sdkInfo": map[string]string{"language": "JAVA"}, "metadata": json.RawMessage(tpl.Metadata)})
		if err != nil {
			return "", err
		}
		if err := t.storage.Write(ctx, path, spec); err != nil {
			return "", err
		}
		if err := t.storage.Write(ctx, path+"_metadata", tpl.Metadata); err != nil {
			return "", err
		}
		if err := t.storage.Write(ctx, path+"_cbstage", []byte(beamrun.StageVersion)); err != nil {
			return "", err
		}
		return path, nil
	}
	if err := t.p.Stage(ctx, tpl, t.project(), path, "gs://"+providedBucket+"/staging/"+beamrun.TemplatesRelease+"/"); err != nil {
		return "", err
	}
	if err := t.storage.Write(ctx, path+"_metadata", tpl.Metadata); err != nil {
		return "", err
	}
	if err := t.storage.Write(ctx, path+"_cbstage", []byte(beamrun.StageVersion)); err != nil {
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
