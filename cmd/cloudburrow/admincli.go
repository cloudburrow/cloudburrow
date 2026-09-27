package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/admin"
	"github.com/cloudburrow/cloudburrow/internal/config"
)

// `cloudburrow reset` under a running `up`, `cloudburrow seed` and
// `cloudburrow events` (#586): the admin API's daily operations without curl
// and a hand-read token. Each sends the instance's admin token itself and
// never prints it.

// adminCall sends one request to the running instance's admin API and
// returns the status and body.
func adminCall(cfg config.Config, info runtimeInfo, method, path string, q url.Values, body io.Reader) (int, []byte, error) {
	u := "http://" + info.Control + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := adminRequest(cfg, method, u, body)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := (&http.Client{Timeout: 10 * time.Minute}).Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("the admin API of instance %q did not answer: %w", cfg.Name, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return resp.StatusCode, b, fmt.Errorf("the admin API refused the token in %s (401); "+
			"is it from an earlier `up`? `cloudburrow stop` and start again", adminTokenPath(cfg))
	}
	return resp.StatusCode, b, nil
}

// adminError is the message an admin API error body carries, or the body.
func adminError(status int, b []byte) string {
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(b, &e) == nil && e.Error != "" {
		return e.Error
	}
	if s := strings.TrimSpace(string(b)); s != "" {
		return s
	}
	return http.StatusText(status)
}

// notRunning is the error for an admin command with no `up` to talk to.
func notRunning(cfg config.Config) error {
	return fmt.Errorf("instance %q is not running; start it with `cloudburrow up`", cfg.Name)
}

type resetOptions struct {
	services []string
	project  string
	reseed   bool
}

func (o resetOptions) scoped() bool { return len(o.services) > 0 || o.project != "" || o.reseed }

func resetFlags(args []string) (resetOptions, []string, error) {
	var o resetOptions
	services, rest, err := serviceFlags(args)
	if err != nil {
		return o, nil, err
	}
	o.services = services
	v, _, rest, err := splitFlag(rest, "project", false)
	if err != nil {
		return o, nil, err
	}
	o.project = v
	v, found, rest, err := splitFlag(rest, "reseed", true)
	if err != nil {
		return o, nil, err
	}
	o.reseed = found && v != "false"
	return o, rest, nil
}

// serviceFlags collects every -service, repeated or comma-separated.
func serviceFlags(args []string) ([]string, []string, error) {
	var out, rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			rest = append(rest, args[i:]...)
			break
		}
		trimmed := strings.TrimLeft(a, "-")
		var v string
		switch {
		case !strings.HasPrefix(a, "-"):
			rest = append(rest, a)
			continue
		case trimmed == "service":
			if i+1 >= len(args) {
				return nil, nil, errors.New("flag -service needs a value")
			}
			v = args[i+1]
			i++
		case strings.HasPrefix(trimmed, "service="):
			v = strings.TrimPrefix(trimmed, "service=")
		default:
			rest = append(rest, a)
			continue
		}
		for _, s := range strings.Split(v, ",") {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
	}
	return out, rest, nil
}

type resetResult struct {
	Reset    []string          `json:"reset"`
	Failed   map[string]string `json:"failed"`
	Reseeded []string          `json:"reseeded"`
}

// resetViaAdmin resets through the running `up`. Nothing in the cluster is
// deleted: each service clears its own state through its own API.
func resetViaAdmin(cfg config.Config, info runtimeInfo, o resetOptions, stdout io.Writer) error {
	q := url.Values{}
	for _, s := range o.services {
		q.Add("service", s)
	}
	if o.project != "" {
		q.Set("project", o.project)
	}
	if o.reseed {
		q.Set("reseed", "true")
	}
	status, b, err := adminCall(cfg, info, http.MethodPost, "/admin/reset", q, nil)
	if err != nil {
		return err
	}
	var res resetResult
	if status == http.StatusBadRequest || json.Unmarshal(b, &res) != nil {
		return fmt.Errorf("reset: %s", adminError(status, b))
	}
	if len(res.Reset) > 0 {
		scope := ""
		if o.project != "" {
			scope = " in project " + o.project
		}
		fmt.Fprintf(stdout, "reset: %s%s\n", strings.Join(res.Reset, ", "), scope)
	}
	if len(res.Reseeded) > 0 {
		fmt.Fprintf(stdout, "reseeded from the startup seed file: %s\n", strings.Join(res.Reseeded, ", "))
	}
	if status != http.StatusOK || len(res.Failed) > 0 {
		names := make([]string, 0, len(res.Failed))
		for name, msg := range res.Failed {
			fmt.Fprintf(stdout, "  failed: %-14s %s\n", name, msg)
			names = append(names, name)
		}
		if len(names) == 0 {
			return fmt.Errorf("reset: %s", adminError(status, b))
		}
		return fmt.Errorf("reset did not complete: %d component(s) failed; their state may remain", len(names))
	}
	fmt.Fprintf(stdout, "the running up for instance %q kept serving; its cluster and pods are untouched\n", cfg.Name)
	return nil
}

// runSeed is `cloudburrow seed <file> [-if-not-exists]`: POST /admin/seed
// with the file as the body.
func runSeed(args []string, stdout, stderr io.Writer) error {
	v, found, rest, err := splitFlag(args, "if-not-exists", true)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return errUsage
	}
	skip := found && v != "false"
	if len(rest) == 0 || strings.HasPrefix(rest[0], "-") {
		fmt.Fprintln(stderr, "usage: cloudburrow seed <file> [-if-not-exists] [flags]")
		return errUsage
	}
	file := rest[0]
	cfg, err := config.Load(config.Options{Args: rest[1:], Output: stderr})
	if err != nil {
		return err
	}
	doc, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	info, ok := running(cfg)
	if !ok {
		return notRunning(cfg)
	}
	q := url.Values{}
	if skip {
		q.Set("ifNotExists", "true")
	}
	status, b, err := adminCall(cfg, info, http.MethodPost, "/admin/seed", q, bytes.NewReader(doc))
	if err != nil {
		return err
	}
	var res struct {
		Seeded []string `json:"seeded"`
	}
	_ = json.Unmarshal(b, &res)
	if status != http.StatusOK {
		if len(res.Seeded) > 0 {
			fmt.Fprintf(stdout, "seeded before the failure: %s\n", strings.Join(res.Seeded, ", "))
		}
		return fmt.Errorf("seed %s: %s", file, adminError(status, b))
	}
	fmt.Fprintf(stdout, "seeded %s: %s\n", file, strings.Join(res.Seeded, ", "))
	return nil
}

type eventsOptions struct {
	service, kind, format string
	since                 time.Time
	limit                 int
}

func eventsFlags(args []string, now time.Time) (eventsOptions, []string, error) {
	o := eventsOptions{format: "text", limit: 100}
	v, _, rest, err := splitFlag(args, "service", false)
	if err != nil {
		return o, nil, err
	}
	o.service = v
	if v, _, rest, err = splitFlag(rest, "kind", false); err != nil {
		return o, nil, err
	}
	o.kind = v
	if v, _, rest, err = splitFlag(rest, "since", false); err != nil {
		return o, nil, err
	}
	if v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			o.since = now.Add(-d)
		} else if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
			o.since = t
		} else {
			return o, nil, fmt.Errorf("invalid -since %q: want a duration such as 10m, or an RFC 3339 time", v)
		}
	}
	var found bool
	if v, found, rest, err = splitFlag(rest, "limit", false); err != nil {
		return o, nil, err
	}
	if found {
		if o.limit, err = strconv.Atoi(v); err != nil || o.limit <= 0 {
			return o, nil, fmt.Errorf("invalid -limit %q: want a positive count", v)
		}
	}
	if v, found, rest, err = splitFlag(rest, "format", false); err != nil {
		return o, nil, err
	}
	if found {
		if v != "text" && v != "json" {
			return o, nil, fmt.Errorf("invalid -format %q: want text or json", v)
		}
		o.format = v
	}
	return o, rest, nil
}

// runEvents is `cloudburrow events`: GET /admin/events, newest first.
func runEvents(args []string, stdout, stderr io.Writer) error {
	o, rest, err := eventsFlags(args, time.Now())
	if err != nil {
		fmt.Fprintln(stderr, err)
		return errUsage
	}
	cfg, err := config.Load(config.Options{Args: rest, Output: stderr})
	if err != nil {
		return err
	}
	info, ok := running(cfg)
	if !ok {
		return notRunning(cfg)
	}
	q := url.Values{"limit": {strconv.Itoa(o.limit)}}
	if o.service != "" {
		q.Set("service", o.service)
	}
	if o.kind != "" {
		q.Set("kind", o.kind)
	}
	if !o.since.IsZero() {
		q.Set("since", o.since.UTC().Format(time.RFC3339Nano))
	}
	status, b, err := adminCall(cfg, info, http.MethodGet, "/admin/events", q, nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("events: %s", adminError(status, b))
	}
	var res struct {
		Events []admin.Event `json:"events"`
	}
	if err := json.Unmarshal(b, &res); err != nil {
		return fmt.Errorf("events: %w", err)
	}
	if o.format == "json" {
		enc := json.NewEncoder(stdout)
		for _, e := range res.Events {
			if err := enc.Encode(e); err != nil {
				return err
			}
		}
		return nil
	}
	for _, e := range res.Events {
		line := fmt.Sprintf("%s  %-14s %-8s %s", e.Time.Local().Format("15:04:05.000"), e.Service, e.Kind, e.Target)
		if code := e.Detail["code"]; code != "" {
			line += "  " + code
		}
		if r := e.Detail["resource"]; r != "" {
			line += "  " + r
		}
		fmt.Fprintln(stdout, line)
	}
	return nil
}
