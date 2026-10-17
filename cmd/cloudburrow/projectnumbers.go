package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/netfwd"
	"github.com/cloudburrow/cloudburrow/internal/resource"
	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
	"github.com/cloudburrow/cloudburrow/internal/service/storage"
)

// wireProjectNumbers makes every project number this process reports the
// registry's (#1544): resource.ProjectNumber reads the project's record, so
// a project made again under a purged ID reports its new number in Resource
// Manager, Service Usage, Logging, the metadata server, the Run adapter's
// service agents and builds, and the console. The builtin storage server,
// in its pod, is sent the numbers that differ from the derived ones, at
// start once its tunnel is up and after every create and purge.
func wireProjectNumbers(ctx context.Context, reg *resourcemanager.Registry, storageTunnel func() *netfwd.Forwarder) {
	resource.SetProjectNumberResolver(reg.NumberOf)
	if storageTunnel == nil || storageTunnel() == nil {
		return
	}
	p := &numberPusher{reg: reg, tunnel: storageTunnel}
	reg.OnNumbersChanged(func() {
		if err := p.push(ctx); err != nil {
			slog.Warn("could not send the project numbers to the storage server", "error", err)
		}
	})
	go func() {
		for {
			if err := p.push(ctx); err == nil {
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
		}
	}()
}

// numberPusher sends the registry's NumberOverrides to the storage server.
type numberPusher struct {
	mu     sync.Mutex
	reg    *resourcemanager.Registry
	tunnel func() *netfwd.Forwarder
}

func (p *numberPusher) push(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	f := p.tunnel()
	if f == nil || f.HostAddr() == "" {
		return fmt.Errorf("the storage tunnel is not running")
	}
	m, err := p.reg.NumberOverrides()
	if err != nil {
		return err
	}
	body, _ := json.Marshal(m)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+f.HostAddr()+storage.ProjectNumbersPath, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("send the project numbers: %s", resp.Status)
	}
	return nil
}
