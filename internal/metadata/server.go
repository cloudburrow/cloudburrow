package metadata

import (
	"context"
	"crypto"
	"errors"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/hostguard"
	"github.com/cloudburrow/cloudburrow/internal/resource"
)

// cryptoSHA256 is referenced from credentials.go; named here so that file does
// not import crypto for one constant.
const cryptoSHA256 = crypto.SHA256

// FlavorHeader is the header every Google client uses to decide whether it is
// talking to a real metadata server.
//
// It is required on requests and set on responses. A server that answered
// without it would be probed and then ignored, which looks like the metadata
// server not existing rather than being misconfigured.
const FlavorHeader = "Metadata-Flavor"

// FlavorValue is the only accepted value.
const FlavorValue = "Google"

// TokenTTL is how long a minted token claims to live.
//
// Nothing enforces it: CloudBurrow does not validate tokens at all. It is set
// to an ordinary value so that clients which refresh on expiry behave the way
// they would against Google, rather than refreshing constantly or never.
const TokenTTL = time.Hour

// AccountHostPrefix begins the first label of a workload's metadata host
// (#1388). Cloud Run tells each workload its own service account; here a
// Cloud Run service, job or worker pool reaches this server under a name of
// its account's, cloudburrow-sa-<hash>.<namespace>.svc.cluster.local: an
// ExternalName Service the Cloud Run adapter creates, which resolves to
// cloudburrow-host. The name the client dialed arrives as Host, and says
// which account to name.
const AccountHostPrefix = "cloudburrow-sa-"

// AccountResolver names the service account a workload host's first label
// stands for, or reports false for a label it never issued.
type AccountResolver func(label string) (email string, ok bool)

// Server is the local GCE metadata server.
type Server struct {
	creds   *Credentials
	project string
	port    int
	host    string
	// accounts resolves a workload's metadata host to its account; nil
	// names the instance's own account to everyone.
	accounts AccountResolver

	mu   sync.Mutex
	ln   net.Listener
	srv  *http.Server
	done chan struct{}
}

// NewServer returns a metadata server for the given credentials.
func NewServer(creds *Credentials, project, host string, port int) *Server {
	if host == "" {
		host = "127.0.0.1"
	}
	return &Server{creds: creds, project: project, host: host, port: port}
}

// WithAccounts makes the server name, to a workload that reaches it under
// a cloudburrow-sa- host, the account that host stands for (#1388). It is
// set before Start.
func (s *Server) WithAccounts(r AccountResolver) *Server {
	s.accounts = r
	return s
}

func (s *Server) Name() string { return "metadata" }

// workloadNamespaceSuffix is what may follow a workload host's first label:
// nothing, <namespace>.svc or <namespace>.svc.cluster.local, the forms a
// pod's resolver expands a Service name to. <label>.<namespace> alone is
// refused, since a namespace can share its name with a public top-level
// domain (#676).
var workloadNamespaceSuffix = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?\.svc(\.cluster\.local)?$`)

// workloadAccount is the account a request's Host names, when it is a
// workload's metadata host the resolver issued.
func (s *Server) workloadAccount(r *http.Request) (string, bool) {
	if s.accounts == nil {
		return "", false
	}
	host := strings.ToLower(r.Host)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimSuffix(host, ".")
	label, rest, _ := strings.Cut(host, ".")
	if !strings.HasPrefix(label, AccountHostPrefix) || (rest != "" && !workloadNamespaceSuffix.MatchString(rest)) {
		return "", false
	}
	return s.accounts(label)
}

// accountOf is the account a request is answered as: the workload's own, or
// the instance's.
func (s *Server) accountOf(r *http.Request) string {
	if email, ok := s.workloadAccount(r); ok {
		return email
	}
	return s.creds.Email
}

// Addr returns the resolved listen address, or "" before Start.
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}

// projectNumber is the served project's number, as every service reports it.
func (s *Server) projectNumber() string {
	return strconv.FormatInt(resource.ProjectNumber(s.project), 10)
}

// Handler builds the route table.
//
// It is exported so tests can drive every route through an httptest server
// without binding a port.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// The root probe. A client asks for "/" and checks only the response
	// header, which is how it decides it is running on GCE at all.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			s.text(w, "computeMetadata/\n")
			return
		}
		http.NotFound(w, r)
	})
	mux.HandleFunc("/computeMetadata/v1/", func(w http.ResponseWriter, r *http.Request) {
		s.text(w, "instance/\nproject/\n")
	})

	mux.HandleFunc("/computeMetadata/v1/project/project-id", func(w http.ResponseWriter, r *http.Request) {
		s.text(w, s.project)
	})
	// The project number is resource.ProjectNumber, the one Resource Manager
	// v1 and v3 report and the service agents' emails carry (#1211): a client
	// that reads it here and compares it with any of those finds it equal.
	mux.HandleFunc("/computeMetadata/v1/project/numeric-project-id", func(w http.ResponseWriter, r *http.Request) {
		s.text(w, s.projectNumber())
	})

	mux.HandleFunc("/computeMetadata/v1/instance/id", func(w http.ResponseWriter, r *http.Request) {
		s.text(w, "0000000000000000000")
	})
	mux.HandleFunc("/computeMetadata/v1/instance/name", func(w http.ResponseWriter, r *http.Request) {
		s.text(w, "cloudburrow-local")
	})
	// Zone is returned in its fully qualified form, because clients parse the
	// last path segment out of it and a bare zone name breaks that.
	mux.HandleFunc("/computeMetadata/v1/instance/zone", func(w http.ResponseWriter, r *http.Request) {
		s.text(w, fmt.Sprintf("projects/%s/zones/%s", s.projectNumber(), DefaultZone))
	})
	mux.HandleFunc("/computeMetadata/v1/instance/machine-type", func(w http.ResponseWriter, r *http.Request) {
		s.text(w, "projects/"+s.projectNumber()+"/machineTypes/cloudburrow-local")
	})

	mux.HandleFunc("/computeMetadata/v1/instance/service-accounts/", s.serviceAccounts)

	// The token endpoint the generated credentials point at. It is not part
	// of the metadata contract; it is here so that an ADC file can name a
	// token URI that never leaves the machine.
	mux.HandleFunc("/token", s.token)
	mux.HandleFunc("/certs", s.certs)
	// IAM Credentials (#303), for impersonation; see iamcredentials.go.
	mux.HandleFunc(iamPrefix, s.iamCredentials)

	// metadata.google.internal is the name clients use on GCE; it is in a
	// top-level domain no attacker can register, so a client that maps it
	// here is answered (#676).
	//
	// A workload's own host (#1388) is answered too: its first label is one
	// the Cloud Run adapter issued, under a cluster-local suffix, which no
	// attacker can register any more than cloudburrow-host.
	inner := s.requireFlavor(mux)
	guarded := hostguard.Wrap(inner, "metadata.google.internal")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := s.workloadAccount(r); ok {
			inner.ServeHTTP(w, r)
			return
		}
		guarded.ServeHTTP(w, r)
	})
}

// DefaultZone is the zone reported to clients.
const DefaultZone = "us-central1-a"

// requireFlavor enforces the anti-SSRF header Google's metadata server
// requires.
//
// Without it, any web page a developer visited could read tokens from
// 169.254.169.254 by making a plain cross-origin request. Enforcing it here
// is not protecting a secret — the tokens grant nothing — but a local server
// that accepts requests a real one rejects teaches code to work here and fail
// in production.
func (s *Server) requireFlavor(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(FlavorHeader, FlavorValue)
		// The token exchange is an OAuth endpoint, not a metadata one, and
		// clients do not send the header to it.
		// So is IAM Credentials: it is a Google API, called by its own
		// clients, which have never heard of the metadata header.
		if r.URL.Path == "/token" || r.URL.Path == "/certs" || strings.HasPrefix(r.URL.Path, iamPrefix) {
			next.ServeHTTP(w, r)
			return
		}
		if r.Header.Get(FlavorHeader) != FlavorValue {
			http.Error(w, "Metadata-Flavor: Google header is required", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// serviceAccounts handles everything under the service-accounts tree.
//
// Both "default" and the account's own email must work, because clients use
// whichever they were configured with and treat a 404 as "no such account"
// rather than "wrong alias".
//
// The account is the workload's own when it reached this server under its
// cloudburrow-sa- host (#1388), as Cloud Run names a revision's service
// account, and the instance's otherwise.
func (s *Server) serviceAccounts(w http.ResponseWriter, r *http.Request) {
	const prefix = "/computeMetadata/v1/instance/service-accounts/"
	rest := strings.TrimPrefix(r.URL.Path, prefix)
	email := s.accountOf(r)

	if rest == "" {
		s.text(w, "default/\n"+email+"/\n")
		return
	}

	account, field, _ := strings.Cut(rest, "/")
	if account != "default" && account != email {
		http.Error(w, "service account not found", http.StatusNotFound)
		return
	}

	switch field {
	case "":
		s.text(w, "aliases\nemail\nidentity\nscopes\ntoken\n")
	case "email":
		s.text(w, email)
	case "aliases":
		s.text(w, "default")
	case "scopes":
		s.text(w, "https://www.googleapis.com/auth/cloud-platform\n")
	case "token":
		s.writeAccessToken(w)
	case "identity":
		audience := r.URL.Query().Get("audience")
		if audience == "" {
			// Google returns 400 here, and a client that forgot the audience
			// needs to see that rather than receive a token for nothing.
			http.Error(w, "non-empty audience parameter required", http.StatusBadRequest)
			return
		}
		tok, err := s.creds.MintIDTokenFor(email, audience, time.Now(), TokenTTL)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		s.text(w, tok)
	default:
		http.Error(w, "unknown metadata key", http.StatusNotFound)
	}
}

func (s *Server) writeAccessToken(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"access_token":%q,"expires_in":%d,"token_type":"Bearer"}`,
		s.creds.MintAccessToken(), int(TokenTTL.Seconds()))
}

// token is the OAuth endpoint the generated ADC file points at.
//
// The assertion is not verified. CloudBurrow authenticates nothing, and a
// check here would imply a guarantee the rest of the system does not make.
// What it does check is that an assertion was sent at all, so a client that
// is silently sending nothing finds out.
func (s *Server) token(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "token exchange requires POST", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "malformed token request", http.StatusBadRequest)
		return
	}
	if r.PostForm.Get("assertion") == "" && r.PostForm.Get("refresh_token") == "" {
		http.Error(w, `{"error":"invalid_request","error_description":"missing assertion"}`,
			http.StatusBadRequest)
		return
	}
	s.writeAccessToken(w)
}

// certs serves the public key, so a client configured to fetch certificates
// from this instance does not reach out to Google.
func (s *Server) certs(w http.ResponseWriter, r *http.Request) {
	body, err := s.creds.JWKS()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

func (s *Server) text(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(body))
}

// Start binds the listener and begins serving. It does not block.
func (s *Server) Start(ctx context.Context) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", net.JoinHostPort(s.host, fmt.Sprint(s.port)))
	if err != nil {
		return fmt.Errorf("bind metadata port %d: %w", s.port, err)
	}

	srv := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 5 * time.Second}
	done := make(chan struct{})

	s.mu.Lock()
	s.ln, s.srv, s.done = ln, srv, done
	s.mu.Unlock()

	go func() {
		defer close(done)
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			_ = err
		}
	}()
	return nil
}

// Stop shuts the server down within ctx's deadline.
func (s *Server) Stop(ctx context.Context) error {
	s.mu.Lock()
	srv, done := s.srv, s.done
	s.srv = nil
	s.mu.Unlock()

	if srv == nil {
		return nil
	}
	err := srv.Shutdown(ctx)
	if err != nil {
		_ = srv.Close()
	}
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
		}
	}
	return err
}
