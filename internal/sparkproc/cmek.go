package sparkproc

import (
	"context"

	"github.com/cloudburrow/cloudburrow/internal/cmek"
)

// cmekKinds name cmek's Kinds in Result.KeyKind.
var cmekKinds = map[cmek.Kind]string{cmek.Invalid: "invalid", cmek.NoKMS: "noKMS", cmek.NotFound: "notFound",
	cmek.Unusable: "unusable", cmek.PermissionDenied: "permissionDenied", cmek.Unavailable: "unavailable"}

// checkKey checks job's customer-managed key before it runs: it must be
// a key of the instance's Cloud KMS, usable, that the job's KeyAgent may
// encrypt with. refused is set when it cannot be used.
func (d *Docker) checkKey(ctx context.Context, job Job) (res Result, refused bool) {
	if d.KeyCheck == nil {
		return Result{KeyRefused: "Cloud KMS is not served by this CloudBurrow instance: start it with kms in --services", KeyKind: "noKMS"}, true
	}
	err := d.KeyCheck(ctx, job.Project, job.KeyAgent, job.KMSKey)
	if err == nil {
		return Result{}, false
	}
	kind := "unusable"
	if e := cmek.As(err); e != nil {
		if k, ok := cmekKinds[e.Kind]; ok {
			kind = k
		}
	}
	return Result{KeyRefused: err.Error(), KeyKind: kind}, true
}
