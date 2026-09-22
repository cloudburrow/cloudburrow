package main

import "testing"

// TestALineNamesTheContainerThatWroteIt.
//
// A pod's own name cannot distinguish the application's output from its
// sidecar's, and on a Knative pod those say very different things about what is
// wrong.
func TestALineNamesTheContainerThatWroteIt(t *testing.T) {
	pod := podRef{namespace: "default", name: "api-00002-deployment-abc",
		service: "api", containers: []string{"user-container", "queue-proxy"}}

	// A ref that has not been narrowed to a container names the pod, so nothing
	// renders an empty suffix.
	if got := pod.resource(); got != "api-00002-deployment-abc" {
		t.Errorf("unnarrowed resource = %q", got)
	}

	app := pod
	app.container = "user-container"
	proxy := pod
	proxy.container = "queue-proxy"

	if app.resource() != "api-00002-deployment-abc/user-container" {
		t.Errorf("app resource = %q", app.resource())
	}
	if proxy.resource() != "api-00002-deployment-abc/queue-proxy" {
		t.Errorf("proxy resource = %q", proxy.resource())
	}
	// Two containers of one pod must be two followers, or the second one
	// replaces the first and its lines are lost.
	if app.key() == proxy.key() {
		t.Fatal("two containers share a follower key, so only one is followed")
	}
	// The source stays the Cloud Run service: a reader filtering by source wants
	// the service's whole output, sidecar included.
	if app.source() != "run/api" || proxy.source() != "run/api" {
		t.Errorf("sources = %q, %q", app.source(), proxy.source())
	}
}
