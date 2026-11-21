package main

import (
	"context"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/netfwd"
	"github.com/cloudburrow/cloudburrow/internal/service/firebaserules"
)

// firebaseRulesService keys the Firebase Rules API (#2048), served beside
// Firestore, for gcloud-setup and terraform.
const firebaseRulesService config.Service = "firebaserules"

// firebaseRulesEnabled is whether `up` binds the Firebase Rules port: when
// Firestore is enabled, whose emulator evaluates the rules.
func firebaseRulesEnabled(cfg config.Config) bool {
	return serviceEnabled(cfg, config.ServiceFirestore) && cfg.Endpoints.FirebaseRules >= 0
}

// newFirebaseRulesServer serves the Firebase Rules API v1 over the
// Firestore emulator, reached through the Firestore tunnel, read at each
// call. Its rulesets and releases are kept in the state directory in
// persistent mode, and put back in the emulator when it starts.
func newFirebaseRulesServer(cfg config.Config, firestore func() *netfwd.Forwarder) *firebaserules.Server {
	if !firebaseRulesEnabled(cfg) {
		return nil
	}
	emu := firebaserules.HTTPEmulator{Client: &http.Client{Timeout: 30 * time.Second}, Base: func() string {
		if f := firestore(); f != nil && f.HostAddr() != "" {
			return "http://" + f.HostAddr()
		}
		return ""
	}}
	state := ""
	if cfg.Mode == config.ModePersistent && cfg.StateDir != "" {
		state = filepath.Join(cfg.InstanceDir(), "firebaserules")
	}
	return firebaserules.NewServer(net.JoinHostPort(cfg.BindAddress, strconv.Itoa(cfg.Endpoints.FirebaseRules)), emu, state)
}

// firebaseRulesResetter forgets each project's rulesets and releases
// (#2048), opening its Firestore again.
type firebaseRulesResetter struct {
	srv func() *firebaserules.Server
}

func (r *firebaseRulesResetter) Name() string { return "firebaserules" }

func (r *firebaseRulesResetter) Reset(ctx context.Context) error { return r.ResetProject(ctx, "") }

// ResetProject forgets one project's, or every project's for "".
func (r *firebaseRulesResetter) ResetProject(ctx context.Context, project string) error {
	srv := r.srv()
	if srv == nil {
		return nil
	}
	return srv.ResetProject(ctx, project)
}
