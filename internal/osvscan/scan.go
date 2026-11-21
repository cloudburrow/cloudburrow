package osvscan

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// Result is OSV-Scanner's JSON output, the parts read here.
type Result struct {
	Results []struct {
		Source struct {
			Path string `json:"path"`
			Type string `json:"type"`
		} `json:"source"`
		Packages []Package `json:"packages"`
	} `json:"results"`
	ImageMetadata struct {
		OS string `json:"os"`
		// LayerMetadata are the image's layers, bottom first, as an
		// image scan reports them: each package's ImageOriginDetails
		// index is one of them (#2066).
		LayerMetadata []Layer `json:"layer_metadata"`
		// BaseImages are the base images each BaseImageIndex names.
		BaseImages [][]struct {
			Name string `json:"name"`
		} `json:"base_images"`
	} `json:"image_metadata"`
}

// Layer is one layer of a scanned image.
type Layer struct {
	DiffID         string `json:"diff_id"`
	Command        string `json:"command"`
	IsEmpty        bool   `json:"is_empty"`
	BaseImageIndex int    `json:"base_image_index"`
}

// Package is one package OSV-Scanner found, with its vulnerabilities.
type Package struct {
	Package struct {
		Name          string `json:"name"`
		OSPackageName string `json:"os_package_name"`
		Version       string `json:"version"`
		Ecosystem     string `json:"ecosystem"`
		PURL          string `json:"purl"`
		// ImageOriginDetails names the layer the package came from, in
		// an image scan.
		ImageOriginDetails *struct {
			Index int `json:"index"`
		} `json:"image_origin_details"`
	} `json:"package"`
	// Licenses are the package's licenses, as SPDX expressions, which
	// --licenses reports from the package's own metadata (an OS
	// package database records them) (#2066).
	Licenses        []string        `json:"licenses"`
	Vulnerabilities []Vulnerability `json:"vulnerabilities"`
	Groups          []struct {
		IDs         []string `json:"ids"`
		Aliases     []string `json:"aliases"`
		MaxSeverity string   `json:"max_severity"`
	} `json:"groups"`
}

// Vulnerability is an OSV record (https://ossf.github.io/osv-schema/).
type Vulnerability struct {
	ID        string   `json:"id"`
	Summary   string   `json:"summary"`
	Details   string   `json:"details"`
	Aliases   []string `json:"aliases"`
	Upstream  []string `json:"upstream"`
	Modified  string   `json:"modified"`
	Published string   `json:"published"`
	Severity  []struct {
		Type  string `json:"type"`
		Score string `json:"score"`
	} `json:"severity"`
	Affected []struct {
		Package struct {
			Ecosystem string `json:"ecosystem"`
			Name      string `json:"name"`
		} `json:"package"`
		Ranges []struct {
			Type   string `json:"type"`
			Events []struct {
				Introduced string `json:"introduced"`
				Fixed      string `json:"fixed"`
			} `json:"events"`
		} `json:"ranges"`
		EcosystemSpecific map[string]any `json:"ecosystem_specific"`
		DatabaseSpecific  map[string]any `json:"database_specific"`
		Severity          []struct {
			Type  string `json:"type"`
			Score string `json:"score"`
		} `json:"severity"`
	} `json:"affected"`
	References []struct {
		Type string `json:"type"`
		URL  string `json:"url"`
	} `json:"references"`
	DatabaseSpecific map[string]any `json:"database_specific"`
}

// ErrNoPackages means the scanner found nothing it can scan: an image
// with no recognised operating system or package manifests.
var ErrNoPackages = errors.New("no packages found that OSV-Scanner can scan")

// closedProxy is where every proxy variable points while the scanner
// runs: the discard port on loopback, so any request it might still try
// fails without leaving the machine.
const closedProxy = "http://127.0.0.1:9"

// Scan runs the pinned scanner over a `docker save` archive in offline
// mode against d's snapshot.
func Scan(ctx context.Context, d Dir, archive string) (*Result, error) {
	if err := Ready(d); err != nil {
		return nil, err
	}
	work, err := os.MkdirTemp(filepath.Dir(archive), "osv-scan-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(work)
	out := filepath.Join(work, "result.json")
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, d.BinaryPath(), "scan", "image", "--archive", "--offline",
		"--all-packages", "--licenses", "--format", "json", "--output-file", out, archive)
	cmd.Dir = work
	cmd.Env = []string{
		"HOME=" + work, "TMPDIR=" + work, "PATH=/usr/bin:/bin",
		"OSV_SCANNER_LOCAL_DB_CACHE_DIRECTORY=" + d.DBDir(),
		"HTTP_PROXY=" + closedProxy, "HTTPS_PROXY=" + closedProxy, "ALL_PROXY=" + closedProxy,
		"http_proxy=" + closedProxy, "https_proxy=" + closedProxy, "all_proxy=" + closedProxy,
		"NO_PROXY=", "no_proxy=",
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdout = &stderr
	err = cmd.Run()
	// 0: no vulnerabilities; 1: vulnerabilities found; 128: no packages.
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		switch ee.ExitCode() {
		case 1:
			err = nil
		case 128:
			return nil, ErrNoPackages
		}
	}
	if err != nil {
		return nil, fmt.Errorf("osv-scanner: %w: %s", err, tail(stderr.String(), 600))
	}
	b, err := os.ReadFile(out)
	if err != nil {
		return nil, fmt.Errorf("osv-scanner wrote no result: %w: %s", err, tail(stderr.String(), 600))
	}
	r := &Result{}
	if err := json.Unmarshal(b, r); err != nil {
		return nil, fmt.Errorf("osv-scanner result: %w", err)
	}
	// Offline, the scanner reads no language package's license (it asks
	// deps.dev for those): they are read from the image itself (#2072).
	if err := AddLocalLicenses(r, archive); err != nil {
		slog.Warn("Artifact Analysis could not read the image's package licenses", "err", err)
	}
	return r, nil
}

func tail(s string, n int) string {
	if len(s) > n {
		return "..." + s[len(s)-n:]
	}
	return s
}
