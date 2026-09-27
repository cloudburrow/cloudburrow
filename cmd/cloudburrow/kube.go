package main

import (
	"errors"

	"github.com/cloudburrow/cloudburrow/internal/k8s"
)

// kubeInvoker runs every kubectl the CLI's console reads, log follower and
// Cloud SQL snapshotter start, through internal/k8s (docs/architecture.md §3,
// rule 2). A test replaces it to see each call's arguments.
var kubeInvoker k8s.Invoker = k8s.Subprocess{}

// kubeRunner is a Runner over kubeInvoker. An empty namespace leaves -n to
// the caller's arguments, so a read that spans namespaces, or names one after
// the verb, keeps the argument order kubectl was always given.
func kubeRunner(kubeconfig, namespace string) *k8s.Runner {
	return k8s.NewWith(kubeInvoker, kubeconfig, "", namespace)
}

// kubectlCause is what the console quotes from a failed kubectl: what it
// printed on stderr when it printed anything, else exec's own error (the
// exit status, or why kubectl did not start).
func kubectlCause(err error) error {
	var re *k8s.RunError
	if !errors.As(err, &re) {
		return err
	}
	if re.Stderr != "" {
		return errors.New(re.Stderr)
	}
	return re.Err
}

// kubectlExit is exec's error alone, without what kubectl printed, for the
// callers that reported only the exit status before they ran kubectl
// through internal/k8s.
func kubectlExit(err error) error {
	var re *k8s.RunError
	if errors.As(err, &re) {
		return re.Err
	}
	return err
}
