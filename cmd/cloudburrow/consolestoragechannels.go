package main

// Object change notification channels in the console (#1281): a Channels
// tab on a bucket's page lists the bucket's active channels (registered by
// objects.watchAll), and Stop on each row ends one with channels.stop, the
// call an application makes. Cloud Storage has no channels.list, so the tab
// reads the storage server's own listing, /_cloudburrow/channels, a
// CloudBurrow extension.
//
// A channel's row is addressed [bucket, "channels/<resourceId>"]; it has no
// page of its own.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	urlpkg "net/url"
	"strconv"
	"strings"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

const channelSegment = "channels/"

// channelJSON is a channel as the server lists it.
type channelJSON struct {
	ID             string `json:"id"`
	ResourceID     string `json:"resourceId"`
	Address        string `json:"address"`
	Token          string `json:"token"`
	Expiration     string `json:"expiration"`
	Created        string `json:"created"`
	MessagesQueued int64  `json:"messagesQueued"`
	// Watches is the objects.list selection the channel watches (#1393).
	Watches string `json:"watches"`
}

func channelPath(path []string) (bucket, resourceID string, ok bool) {
	if len(path) != 2 || path[0] == objectPage || !strings.HasPrefix(path[1], channelSegment) {
		return "", "", false
	}
	rid := strings.TrimPrefix(path[1], channelSegment)
	if rid == "" || strings.Contains(rid, "/") {
		return "", "", false
	}
	return path[0], rid, true
}

func channelStopAction() console.Action {
	return console.Action{ID: "stopchannel", Label: "Stop", Destructive: true}
}

func (p storageProvider) listChannels(ctx context.Context, bucket string) ([]channelJSON, error) {
	u := fmt.Sprintf("http://%s/_cloudburrow/channels?bucket=%s", p.endpoint, urlpkg.QueryEscape(bucket))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, apiError(resp)
	}
	var out struct{ Items []channelJSON }
	return out.Items, json.NewDecoder(resp.Body).Decode(&out)
}

func channelExpiry(ms string) string {
	n, err := strconv.ParseInt(ms, 10, 64)
	if err != nil || n == 0 {
		return "Never"
	}
	return time.UnixMilli(n).UTC().Format(time.RFC3339)
}

// channelsSection is a bucket's Channels tab.
func (p storageProvider) channelsSection(ctx context.Context, bucket string) console.Section {
	sec := console.Section{ID: "channels", Label: "Channels"}
	cs, err := p.listChannels(ctx, bucket)
	if err != nil {
		sec.Unavailable = "channels: " + err.Error()
		return sec
	}
	list := console.Listing{
		Columns:    []string{"Channel ID", "Address", "Watches", "Expires", "Messages sent or queued", "Created"},
		NameColumn: "Resource ID",
		Noun:       "channels",
		Items:      []console.Resource{},
		Note: "Object change notification channels, registered with objects.watchAll. Each gets a sync " +
			"message, then a POST per change of an object a listing with its watchAll parameters (Watches) shows. " +
			"Stop ends one, as channels.stop does.",
	}
	for _, c := range cs {
		list.Items = append(list.Items, console.Resource{
			Name: channelSegment + c.ResourceID,
			Fields: map[string]string{"Channel ID": c.ID, "Address": c.Address, "Watches": orDash(c.Watches), "Expires": channelExpiry(c.Expiration),
				"Messages sent or queued": strconv.FormatInt(c.MessagesQueued, 10), "Created": c.Created},
			Actions: []console.Action{channelStopAction()},
		})
	}
	list.Total = len(list.Items)
	sec.Listing = list
	return sec
}

// channelActions are the actions at a channel's address, and whether the
// path is one.
func (p storageProvider) channelActions(path []string) ([]console.Action, bool) {
	if _, _, ok := channelPath(path); !ok {
		return nil, false
	}
	return []console.Action{channelStopAction()}, true
}

// actOnChannels performs Stop, and reports whether the action was it.
func (p storageProvider) actOnChannels(ctx context.Context, path []string, action string) (bool, error) {
	if action != "stopchannel" {
		return false, nil
	}
	bucket, rid, ok := channelPath(path)
	if !ok {
		return true, errors.New("stop names one channel")
	}
	cs, err := p.listChannels(ctx, bucket)
	if err != nil {
		return true, err
	}
	id := ""
	for _, c := range cs {
		if c.ResourceID == rid {
			id = c.ID
		}
	}
	if id == "" {
		return true, fmt.Errorf("bucket %s has no active channel with resource ID %s: it was stopped or expired", bucket, rid)
	}
	body, _ := json.Marshal(map[string]string{"id": id, "resourceId": rid})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+p.endpoint+"/storage/v1/channels/stop", bytes.NewReader(body))
	if err != nil {
		return true, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return true, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		return true, apiError(resp)
	}
	return true, nil
}
