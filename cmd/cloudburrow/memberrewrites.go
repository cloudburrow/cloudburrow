package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/iampolicy"
	"github.com/cloudburrow/cloudburrow/internal/netfwd"
	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
)

// memberRewriteFronts are the services that keep their resources' IAM
// policies in another process, reached through their tunnels: Cloud
// Storage buckets and managed folders (the storage server), BigQuery
// tables and routines, Pub/Sub topics, subscriptions, snapshots and
// schemas, Bigtable instances, tables, views and backups, and Spanner
// instances, databases and backups (their fronts).
var memberRewriteFronts = []string{"storage", "bigquery", "pubsub", "bigtable", "spanner"}

// wireMemberRewrites passes a deleted service account's binding rewrites
// (#1751) to every service that keeps IAM policies on its own resources:
// in this process Cloud Tasks queues, Secret Manager secrets, Cloud KMS
// key rings, keys and EKM resources, Logging views, Cloud Run services,
// jobs and worker pools, Artifact Registry repositories and Secure Source
// Manager instances and repositories; in another, the fronts above, each
// at iampolicy.RewriteMemberPath. Google shows the account as
// deleted:serviceAccount:EMAIL?uid=ID in every allow policy that names
// it, turns it back on undelete and drops it on purge. Each service is
// read when the hook runs, since a service starts after the registry
// opens; a front whose tunnel is down (its service off) is skipped.
func wireMemberRewrites(reg *resourcemanager.Registry, tasksSvc *tasksService, secretsSvc *secretsService, kmsSvc *kmsService,
	loggingSvc *loggingService, runSvc *runService, forwarders []*netfwd.Forwarder) {
	client := &http.Client{Timeout: 30 * time.Second}
	reg.OnMemberRewrite(func(from, to string) error {
		var errs []error
		if st := tasksSvc.Store(); st != nil {
			errs = append(errs, st.RewriteIAMMember(from, to))
		}
		if secretsSvc != nil && secretsSvc.store != nil {
			errs = append(errs, secretsSvc.store.RewriteIAMMember(from, to))
		}
		if api := kmsSvc.API(); api != nil {
			errs = append(errs, api.RewriteIAMMember(from, to))
		}
		if loggingSvc != nil {
			if a := loggingSvc.Admin(); a != nil {
				errs = append(errs, a.RewriteIAMMember(from, to))
			}
		}
		if runSvc != nil {
			if a := runSvc.adapter.Load(); a != nil {
				ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
				errs = append(errs, a.RewriteIAMMember(ctx, from, to))
				cancel()
			}
		}
		for _, name := range memberRewriteFronts {
			addr := forwardedAddr(forwarders, name)
			if addr == "" {
				continue
			}
			errs = append(errs, pushMemberRewrite(client, addr, from, to))
		}
		return errors.Join(errs...)
	})
}

// pushMemberRewrite sends one rewrite to the front at addr.
func pushMemberRewrite(client *http.Client, addr, from, to string) error {
	body, _ := json.Marshal(iampolicy.MemberRewrite{From: from, To: to})
	resp, err := client.Post("http://"+addr+iampolicy.RewriteMemberPath, "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("%s: rewrite a deleted account's bindings: %s", addr, resp.Status)
	}
	return nil
}
