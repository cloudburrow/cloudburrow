package apienable

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/googleapis/gax-go/v2/apierror"
	"google.golang.org/api/googleapi"
)

// The official clients read the ErrorInfo from the JSON body.
func TestJSONParsesAsTheClientsDo(t *testing.T) {
	d := Denial{Service: "storage.googleapis.com", Title: "Cloud Storage API", Number: 5}
	b, _ := json.Marshal(d.JSON())
	ge := &googleapi.Error{Code: http.StatusForbidden, Body: string(b)}
	ae, ok := apierror.FromError(ge)
	if !ok || ae.Reason() != Reason || ae.Metadata()["consumer"] != "projects/5" {
		var x *apierror.APIError
		t.Fatalf("parsed %v %v %v: %s", ok, ae, errors.As(ge, &x), b)
	}
}
