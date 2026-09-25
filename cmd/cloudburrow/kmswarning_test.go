package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/config"
)

// With Cloud KMS enabled, startup says it is not a security boundary and
// names where the key material is kept (#391).
func TestKMSWarningNamesTheBackend(t *testing.T) {
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	cfg.Services = []config.Service{config.ServiceKMS}
	cfg.Endpoints.KMS = 0
	cfg.Cluster.Namespace = "burrow-ns"
	svc := &kmsService{cfg: cfg}
	ctx := context.Background()
	if err := svc.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer svc.Stop(ctx)
	var out bytes.Buffer
	printKMSWarning(&out, svc)
	for _, want := range []string{"not a security boundary",
		"Kubernetes Secrets labelled cloudburrow.dev/service=kms in namespace burrow-ns"} {
		if !strings.Contains(strings.Join(strings.Fields(out.String()), " "), want) {
			t.Errorf("warning lacks %q:\n%s", want, out.String())
		}
	}
}

func TestNoKMSWarningWhenDisabled(t *testing.T) {
	var out bytes.Buffer
	printKMSWarning(&out, nil)
	if out.Len() != 0 {
		t.Errorf("warning printed with Cloud KMS disabled:\n%s", out.String())
	}
}
