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
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
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
	relay := flag.String("relay", "", "host:port: relay stdin and stdout to it (the ssh config's ProxyCommand) and exit")
	flag.Parse()
	if *relay != "" {
		if err := relayStdio(*relay); err != nil {
			log.Fatal(err)
		}
		return
	}
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

	// Each VM boots as its cloud's stand-in while the create call runs, as
	// a VM boots before its cloud reports it running.
	aws := awsfake.New()
	defer aws.Close()
	aws.SetSSMTarget(awsAddr)
	aws.Boot = func(userData string) {
		if err := byoctest.BootStandIn(context.Background(), awsName, "ubuntu", userData); err != nil {
			log.Printf("AWS VM: %v", err)
			return
		}
		log.Printf("an AWS VM booted as %s", awsName)
	}
	az := azurefake.New()
	defer az.Close()
	az.Boot = func(user, customData string) {
		if err := byoctest.BootStandIn(context.Background(), azName, user, customData); err != nil {
			log.Printf("Azure VM: %v", err)
			return
		}
		log.Printf("an Azure VM booted as %s", azName)
	}

	caPath := filepath.Join(*dir, "azurefake.pem")
	if err := os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: az.Certificate().Raw}), 0o600); err != nil {
		log.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		log.Fatal(err)
	}
	sshConfig := filepath.Join(*dir, "ssh_config")
	// Azure's VMs have addresses in 203.0.113.0/24 (TEST-NET-3), which
	// reach nothing: each is the Azure stand-in here. A ProxyCommand, not
	// HostName and Port, as ssh's -p would override Port; the VM's address
	// stays the host's name, so its pinned host key still matches.
	if err := os.WriteFile(sshConfig, []byte(fmt.Sprintf("Host 203.0.113.*\n  ProxyCommand %q -relay %s\n", self, azAddr)), 0o600); err != nil {
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
	// /approve is the user confirming IAM Identity Center's code on AWS's
	// page.
	mux.HandleFunc("POST /approve", func(w http.ResponseWriter, r *http.Request) {
		aws.Approve()
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
	<-ctx.Done()
}

// relayStdio copies stdin to addr and addr to stdout until either ends.
func relayStdio(addr string) error {
	c, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		return err
	}
	defer c.Close()
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(c, os.Stdin); done <- struct{}{} }()
	go func() { _, _ = io.Copy(os.Stdout, c); done <- struct{}{} }()
	<-done
	return nil
}

func splitStandIn(v string) (name, addr string, err error) {
	name, addr, ok := strings.Cut(v, "=")
	if !ok || name == "" || addr == "" {
		return "", "", fmt.Errorf("a stand-in is container=host:port, not %q", v)
	}
	return name, addr, nil
}
