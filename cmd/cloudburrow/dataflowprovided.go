package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"strings"
	"sync"

	artifactregistry "cloud.google.com/go/artifactregistry/apiv1"
	arpb "cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"
	"cloud.google.com/go/storage"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

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
	// runAddr is the instance's Artifact Registry (Cloud Run's) address,
	// which Google's YAML template's packages are seeded into.
	runAddr func() string

	// yamlMu guards yamlSeeded: the YAML template's packages are seeded
	// once a process (seedYAML).
	yamlMu     sync.Mutex
	yamlSeeded bool

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
	if name == "flex/"+beamrun.YAMLTemplateName {
		if err := t.seedYAML(ctx); err != nil {
			return "", err
		}
	}
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
			"sdkInfo": map[string]string{"language": tpl.Language}, "metadata": json.RawMessage(tpl.Metadata)})
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

// seedYAML readies Google's YAML template (beamrun's yaml.go): its
// launcher is built, the pinned packages its default dependencies need
// are uploaded (twine's protocol) to the instance's Artifact Registry
// Python repository beamrun.ProvidedPythonIndex names, made unless it is
// there, and job_builder_util_transforms' sdist, built from its source,
// is written to gs://dataflow-templates/ at the path main.py reads it
// from (through storage.googleapis.com, which a job resolves to the
// instance). Nothing is read from Google.
func (t *providedTemplates) seedYAML(ctx context.Context) error {
	t.yamlMu.Lock()
	defer t.yamlMu.Unlock()
	if t.yamlSeeded {
		return nil
	}
	y, err := t.p.BuildYAMLLauncher(ctx)
	if err != nil {
		return err
	}
	addr := ""
	if t.runAddr != nil {
		addr = t.runAddr()
	}
	if addr == "" {
		return errors.New("Google's YAML template installs its packages from the instance's Artifact Registry, which this instance does not serve (enable Cloud Run)")
	}
	if err := providedPythonRepository(ctx, addr); err != nil {
		return err
	}
	base := "http://" + addr + "/" + beamrun.ProvidedPythonLocation + "-python.pkg.dev/" + beamrun.ProvidedPythonProject + "/" + beamrun.ProvidedPythonRepository + "/"
	for i, pk := range beamrun.YAMLPackages() {
		b, err := os.ReadFile(y.Packages[i])
		if err != nil {
			return err
		}
		if err := twineUpload(ctx, base, pk, b); err != nil {
			return err
		}
	}
	if err := t.bucket(ctx); err != nil {
		return err
	}
	obj := "gs://" + providedBucket + "/" + beamrun.JobBuilderObject
	tar, err := os.ReadFile(y.Tarball)
	if err != nil {
		return err
	}
	if have, err := t.storage.Read(ctx, obj); err != nil || !bytes.Equal(have, tar) {
		if err := t.storage.Write(ctx, obj, tar); err != nil {
			return fmt.Errorf("stage job_builder_util_transforms at %s: %w", obj, err)
		}
	}
	t.yamlSeeded = true
	return nil
}

// providedPythonRepository makes the provided templates' Python
// repository, unless it is there.
func providedPythonRepository(ctx context.Context, addr string) error {
	c, err := artifactregistry.NewClient(ctx, option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		return fmt.Errorf("connect to Artifact Registry: %w", err)
	}
	defer c.Close()
	parent := "projects/" + beamrun.ProvidedPythonProject + "/locations/" + beamrun.ProvidedPythonLocation
	if _, err := c.GetRepository(ctx, &arpb.GetRepositoryRequest{Name: parent + "/repositories/" + beamrun.ProvidedPythonRepository}); err == nil {
		return nil
	} else if status.Code(err) != codes.NotFound {
		return fmt.Errorf("read the provided templates' Python repository: %w", err)
	}
	op, err := c.CreateRepository(ctx, &arpb.CreateRepositoryRequest{Parent: parent, RepositoryId: beamrun.ProvidedPythonRepository,
		Repository: &arpb.Repository{Format: arpb.Repository_PYTHON,
			Description: "Python packages of Google-provided Dataflow templates (DataflowTemplates " + beamrun.TemplatesRelease + "), seeded by CloudBurrow"}})
	if status.Code(err) == codes.AlreadyExists {
		return nil
	}
	if err != nil {
		return fmt.Errorf("make the provided templates' Python repository: %w", err)
	}
	_, err = op.Wait(ctx)
	return err
}

// twineUpload uploads one distribution file to the Python repository at
// base with twine's upload protocol; a file already there is kept.
func twineUpload(ctx context.Context, base string, pk beamrun.PyPackage, content []byte) error {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, kv := range [][2]string{{":action", "file_upload"}, {"protocol_version", "1"}, {"name", pk.Name}, {"version", pk.Version},
		{"filetype", "bdist_wheel"}, {"pyversion", "py3"}, {"metadata_version", "2.1"}, {"license", pk.License}, {"sha256_digest", pk.SHA256}} {
		_ = mw.WriteField(kv[0], kv[1])
	}
	fw, err := mw.CreateFormFile("content", pk.File)
	if err != nil {
		return err
	}
	_, _ = fw.Write(content)
	_ = mw.Close()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base, &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("upload %s: %w", pk.File, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusConflict {
		return nil
	}
	return fmt.Errorf("upload %s: %s: %s", pk.File, resp.Status, strings.TrimSpace(string(b)))
}
