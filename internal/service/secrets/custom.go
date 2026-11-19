package secrets

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/orgpolicy"
)

// Custom constraints on secretmanager.googleapis.com/Secret (#1891),
// CREATE and UPDATE, with the fields Google lists: resource.annotations,
// resource.expireTime (a ttl is evaluated as the expire_time it gives),
// resource.rotation.nextRotationTime and .rotationPeriod,
// resource.topics.name, resource.versionAliases and
// resource.versionDestroyTtl ("Use custom organization policies", Secret
// Manager, read 2026-11-19). A violation is FAILED_PRECONDITION
// "Operation denied by custom org policies: [...]", the page's example.

type secretTopic struct {
	Name string `json:"name"`
}

type secretRotation struct {
	NextRotationTime string `json:"nextRotationTime,omitempty"`
	RotationPeriod   string `json:"rotationPeriod,omitempty"`
}

type secretResource struct {
	Name              string            `json:"name"`
	Annotations       map[string]string `json:"annotations,omitempty"`
	ExpireTime        string            `json:"expireTime,omitempty"`
	Rotation          *secretRotation   `json:"rotation,omitempty"`
	Topics            []secretTopic     `json:"topics,omitempty"`
	VersionDestroyTTL string            `json:"versionDestroyTtl,omitempty"`
}

func checkSecretCustom(project, id, method string, annotations map[string]string, expire time.Time, topics []string, sch Schedule) error {
	if !orgpolicy.SourceSet() {
		return nil
	}
	proj, loc, regional := strings.Cut(project, regionalSep)
	name := "projects/" + proj + "/secrets/" + id
	if regional {
		name = "projects/" + proj + "/locations/" + loc + "/secrets/" + id
	}
	res := secretResource{Name: name, Annotations: annotations}
	if !expire.IsZero() {
		res.ExpireTime = expire.UTC().Format(time.RFC3339Nano)
	}
	if !sch.NextRotation.IsZero() || sch.RotationPeriod > 0 {
		res.Rotation = &secretRotation{}
		if !sch.NextRotation.IsZero() {
			res.Rotation.NextRotationTime = sch.NextRotation.UTC().Format(time.RFC3339Nano)
		}
		if sch.RotationPeriod > 0 {
			res.Rotation.RotationPeriod = durationJSON(sch.RotationPeriod)
		}
	}
	for _, t := range topics {
		res.Topics = append(res.Topics, secretTopic{Name: t})
	}
	if sch.VersionDestroyTTL > 0 {
		res.VersionDestroyTTL = durationJSON(sch.VersionDestroyTTL)
	}
	err := orgpolicy.CheckCustom(context.Background(), "projects/"+proj, orgpolicy.TypeSecret, method, res)
	var v *orgpolicy.CustomViolation
	if errors.As(err, &v) {
		return apierror.FailedPrecondition("%s", v.Error())
	}
	return nil
}
