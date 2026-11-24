// Command byocfakes serves the local fakes of AWS (awsfake) and Azure
// (azurefake) for bring your own cloud's first-run walk-through (#2209,
// test/browser/byocwalkthrough2209_test.go), in a process of its own so an
// instance's `up`, its console and the `cloudburrow cloud` commands it
// starts all reach the same fake clouds. It is test infrastructure: never
// built into cloudburrow, never shipped.
//
// It writes the file CLOUDBURROW_BYOC_FAKES names for cloudburrow (every
// endpoint on 127.0.0.1), and boots the Docker-in-Docker sshd stand-ins as
// the VMs the fakes create: an AWS VM is reached through the fake's
// Session Manager, whose agent dials --aws-standin's sshd; an Azure VM's
// address is mapped to --azure-standin's by the ssh config it writes.
//
//	go run ./test/tools/byocfakes --dir <scratch> --aws-standin cb-remotehost-23022=127.0.0.1:23022 \
//	    --azure-standin cb-remotehost-23023=127.0.0.1:23023
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/byoc/awsprov/awsfake"
	"github.com/cloudburrow/cloudburrow/internal/byoc/azureprov/azurefake"
	"github.com/cloudburrow/cloudburrow/internal/byoc/byoctest"
)

func main() {
	dir := flag.String("dir", "", "directory for the fakes file, the Azure fake's certificate, the ssh config and the fakes' keychain")
	listen := flag.String("listen", "127.0.0.1:0", "address of the control endpoint (the sign-in page opener)")
	awsStandIn := flag.String("aws-standin", "", "container=host:port of the stand-in an AWS VM boots as")
	azureStandIn := flag.String("azure-standin", "", "container=host:port of the stand-in an Azure VM boots as")
	flag.Parse()
	if *dir == "" || *awsStandIn == "" || *azureStandIn == "" {
		log.Fatal("--dir, --aws-standin and --azure-standin are required")
	}
	awsName, awsAddr, err := splitStandIn(*awsStandIn)
	if err != nil {
		log.Fatal(err)
	}
	azName, azAddr, err := splitStandIn(*azureStandIn)
	if err != nil {
		log.Fatal(err)
	}
	if err := os.MkdirAll(*dir, 0o700); err != nil {
		log.Fatal(err)
	}

	aws := awsfake.New()
	defer aws.Close()
	aws.SetSSMTarget(awsAddr)
	az := azurefake.New()
	defer az.Close()

	caPath := filepath.Join(*dir, "azurefake.pem")
	if err := os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: az.Certificate().Raw}), 0o600); err != nil {
		log.Fatal(err)
	}
	host, port, _ := net.SplitHostPort(azAddr)
	sshConfig := filepath.Join(*dir, "ssh_config")
	// Azure's VMs have addresses in 203.0.113.0/24 (TEST-NET-3), which
	// reach nothing: each is the Azure stand-in here.
	if err := os.WriteFile(sshConfig, []byte(fmt.Sprintf("Host 203.0.113.*\n  HostName %s\n  Port %s\n", host, port)), 0o600); err != nil {
		log.Fatal(err)
	}

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /open", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			URL string `json:"url"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := az.OpenBrowser(in.URL); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	// /live lists what is left in each fake cloud, for the walk-through's
	// last check after Delete.
	mux.HandleFunc("GET /live", func(w http.ResponseWriter, r *http.Request) {
		var out []string
		for _, res := range aws.Live() {
			out = append(out, "aws "+res.Kind+" "+res.ID)
		}
		for _, id := range az.IDs() {
			out = append(out, "azure "+id)
		}
		_ = json.NewEncoder(w).Encode(out)
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()

	fakes := map[string]string{
		"aws": aws.URL(), "azure": az.URL(), "azure_ca": caPath, "open_url": "http://" + ln.Addr().String() + "/open",
		"ssh_config": sshConfig, "keychain": filepath.Join(*dir, "keychain"), "public_ip": "203.0.113.77",
		"live_url": "http://" + ln.Addr().String() + "/live",
	}
	b, _ := json.MarshalIndent(fakes, "", "  ")
	fakesPath := filepath.Join(*dir, "fakes.json")
	if err := os.WriteFile(fakesPath, b, 0o600); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("CLOUDBURROW_BYOC_FAKES=%s\n", fakesPath)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	boot(ctx, aws, az, awsName, azName)
}

func splitStandIn(v string) (name, addr string, err error) {
	name, addr, ok := strings.Cut(v, "=")
	if !ok || name == "" || addr == "" {
		return "", "", fmt.Errorf("a stand-in is container=host:port, not %q", v)
	}
	return name, addr, nil
}

// boot stands the stand-ins in for each VM the fakes create, once, as its
// cloud-init would boot it.
func boot(ctx context.Context, aws *awsfake.Server, az *azurefake.Server, awsName, azName string) {
	booted := map[string]bool{}
	var mu sync.Mutex
	t := time.NewTicker(300 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		for _, r := range aws.Live() {
			if r.Kind != "instance" || r.UserData == "" {
				continue
			}
			mu.Lock()
			done := booted[r.ID]
			booted[r.ID] = true
			mu.Unlock()
			if done {
				continue
			}
			if err := byoctest.BootStandIn(ctx, awsName, "ubuntu", r.UserData); err != nil {
				log.Printf("AWS VM %s: %v", r.ID, err)
				continue
			}
			log.Printf("AWS VM %s booted as %s", r.ID, awsName)
		}
		for _, id := range az.IDs() {
			if !strings.Contains(strings.ToLower(id), "/microsoft.compute/virtualmachines/") || booted[id] {
				continue
			}
			body := az.Resource(id)
			props, _ := body["properties"].(map[string]any)
			osp, _ := props["osProfile"].(map[string]any)
			raw, _ := osp["customData"].(string)
			ud, err := base64.StdEncoding.DecodeString(raw)
			if err != nil || len(ud) == 0 {
				continue
			}
			user, _ := osp["adminUsername"].(string)
			booted[id] = true
			if err := byoctest.BootStandIn(ctx, azName, user, string(ud)); err != nil {
				log.Printf("Azure VM %s: %v", id, err)
				continue
			}
			log.Printf("Azure VM %s booted as %s", id, azName)
		}
	}
}
