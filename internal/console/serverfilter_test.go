package console

import (
	"context"
	"net/url"
	"strings"
	"testing"
)

type filterProvider struct{ fakeProvider }

func (f filterProvider) ListFiltered(_ context.Context, _, filter string) (Listing, error) {
	return Listing{Items: []Resource{{Name: "match:" + filter}}, ServerFilter: &ServerFilter{Value: filter}}, nil
}

// A screen with a service-side filter gets the screen's filter text; one
// without refuses a filter rather than listing everything (#1228).
func TestResourcesPassTheServerFilter(t *testing.T) {
	t.Parallel()
	srv := serve(t, filterProvider{fakeProvider{id: "f", title: "Filtered"}},
		fakeProvider{id: "plain", title: "Plain", listing: Listing{Items: []Resource{{Name: "all"}}}})
	code, body := get(t, srv, "/api/resources/f?project=p&filter="+url.QueryEscape("labels.env=prod"), nil)
	if code != 200 || !strings.Contains(body, `"match:labels.env=prod"`) || !strings.Contains(body, `"serverFilter":{"value":"labels.env=prod"`) {
		t.Errorf("filtered screen = %d %s", code, body)
	}
	code, body = get(t, srv, "/api/resources/plain?project=p&filter=x", nil)
	if code != 200 || !strings.Contains(body, "takes no filter") || strings.Contains(body, `"all"`) {
		t.Errorf("a filter on a screen without one = %d %s", code, body)
	}
}
