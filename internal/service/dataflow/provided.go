package dataflow

import (
	"context"
	"errors"
	"regexp"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// Provided stages Google-provided templates (#2215): Google's own classic
// templates (templates/provided-templates), which Google keeps in its
// buckets gs://dataflow-templates and gs://dataflow-templates-REGION. The
// instance never reads Google's Cloud Storage: its Provided builds them
// from Google's open-source DataflowTemplates sources and stages them in
// the instance's own Cloud Storage.
type Provided interface {
	// Stage stages the provided template name (Word_Count, ...) and
	// answers its local template path, gs://BUCKET/latest/NAME, beside
	// which its NAME_metadata file is. A name it does not have is
	// ErrNoProvidedTemplate.
	Stage(ctx context.Context, name string) (string, error)
}

// ErrNoProvidedTemplate is a Provided's answer for a name Google does not
// provide (or CloudBurrow does not build).
var ErrNoProvidedTemplate = errors.New("no such Google-provided template")

// providedPath is a Google-provided template's path: Google's global
// bucket or a regional one, a version folder (latest, or a release such as
// 2026-11-18-00_RC00) and the template's name.
var providedPath = regexp.MustCompile(`^gs://dataflow-templates(-[a-z]+-[a-z]+[0-9]+)?/([A-Za-z0-9_.-]+)/([A-Za-z0-9_]+)$`)

// templatePath is where gcsPath's template is read: a Google-provided
// template's local copy (staged first), any other path itself.
func (s *Server) templatePath(ctx context.Context, gcsPath string) (string, error) {
	m := providedPath.FindStringSubmatch(gcsPath)
	if m == nil {
		return gcsPath, nil
	}
	if s.opts.Provided == nil {
		return "", apierror.FailedPrecondition("(%s): Google-provided templates are built and run in the instance's Docker, which this instance does not have.", gcsPath) // UNVERIFIED (docs re-read 2026-11-24)
	}
	local, err := s.opts.Provided.Stage(ctx, m[3])
	if errors.Is(err, ErrNoProvidedTemplate) {
		return "", apierror.InvalidArgument("(%s): Unable to open template file: %s.", gcsPath, gcsPath)
	}
	if err != nil {
		return "", apierror.Internal(err, "(%s): The Google-provided template %s could not be staged: %v", gcsPath, m[3], err) // UNVERIFIED (docs re-read 2026-11-24)
	}
	return local, nil
}
