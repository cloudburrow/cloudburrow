package bigqueryfront

import (
	"net/http"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/sysparams"
)

// SystemParams applies Google's REST system parameters (#1954) to the
// BigQuery v2 API's JSON answers: fields / $fields and X-Goog-FieldMask
// prune a success, prettyPrint formats it and callback wraps it in JSONP.
// BigQuery's errors already carry the v1 errors[] list, so $.xgafv leaves
// them as they are; a malformed parameter is answered in BigQuery's
// envelope, reason "invalidParameter".
func SystemParams(h http.Handler) http.Handler {
	return sysparams.Wrap(h, sysparams.Options{
		V1Errors: true,
		WriteError: func(w http.ResponseWriter, _ *http.Request, err error) {
			e := apierror.From(err)
			writeError(w, e.HTTPStatus(), "invalidParameter", e.Message)
		},
	})
}
