package byoctest

import (
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/byoc"
)

func TestParseCloudInitReadsWhatRenderWrote(t *testing.T) {
	client, err := byoc.NewKeyPair("cloudburrow-x")
	if err != nil {
		t.Fatal(err)
	}
	host, err := byoc.NewKeyPair("")
	if err != nil {
		t.Fatal(err)
	}
	ud, err := byoc.RenderCloudInit(byoc.CloudInitOptions{Cloud: byoc.Azure, Instance: "x", User: "cloudburrow", AuthorizedKey: client.Public,
		HostKeyPrivate: host.PrivatePEM, HostKeyPublic: host.Public, AutoStopMinutes: 45})
	if err != nil {
		t.Fatal(err)
	}
	priv, pub, key, stop, err := parseCloudInit(ud)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(priv) != strings.TrimSpace(host.PrivatePEM) || pub != strings.TrimSpace(host.Public) || key != client.Public || stop != 45 {
		t.Fatalf("parsed %q %q %q %d", priv[:20], pub, key, stop)
	}
}
