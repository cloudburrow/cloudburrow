package storage

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/egress"
	"github.com/cloudburrow/cloudburrow/internal/egress/egresstest"
)

// TestChannelMessagesSendNothingToGoogle (#2015): a notification channel's
// message to a Google host is refused on its name, before it is resolved,
// and the metadata server's link-local address, which LocalChannelAddress
// alone lets through, is refused too; a local address gets its message; the
// loopback-only sink beneath the channel client counts no attempt off the
// machine.
func TestChannelMessagesSendNothingToGoogle(t *testing.T) {
	var rec egresstest.Recorder
	local := httptest.NewServer(rec.Handler())
	defer local.Close()
	sink := egresstest.New(t)
	client := LocalChannelClient()
	egress.SetNext(client, sink.Transport())
	post := func(addr string) error {
		return PostChannelDelivery(context.Background(), client, ChannelDelivery{Address: addr, Header: http.Header{}, Body: []byte("{}")})
	}
	for _, a := range []string{"https://storage.googleapis.com/notify", "https://x-uc.a.run.app/notify", "http://169.254.169.254/x"} {
		if err := post(a); err == nil || !strings.Contains(err.Error(), "refused") {
			t.Errorf("%s: %v, want a local refusal", a, err)
		}
	}
	if err := post(local.URL + "/notify"); err != nil {
		t.Errorf("a local address: %v", err)
	}
	if got := rec.Requests(); len(got) != 1 || !strings.HasPrefix(got[0], "POST /notify ") {
		t.Errorf("local delivery: %v", got)
	}
	sink.RequireNone(t)
}
