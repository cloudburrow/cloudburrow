package marketplacetest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/cloudburrow/cloudburrow/internal/k8s"
)

// Cluster is an in-memory stand-in for kubectl against one cluster, for
// the kubectl calls Marketplace makes: apply, create, get, patch, wait and
// delete of namespaces, the Application CRD, Applications, Deployments and
// Services. Every Deployment reads as ready.
type Cluster struct {
	mu         sync.Mutex
	namespaces map[string]bool
	objects    map[string]map[string]any // kind/ns/name
	uid        int
	// Calls are the kubectl argument lists run.
	Calls []string
}

// NewCluster has the default and kube-system namespaces.
func NewCluster() *Cluster {
	return &Cluster{namespaces: map[string]bool{"default": true, "kube-system": true}, objects: map[string]map[string]any{}}
}

func notFound(kind, name string) error {
	return &k8s.RunError{Err: errors.New("exit status 1"), Stderr: fmt.Sprintf(`Error from server (NotFound): %s "%s" not found`, kind, name)}
}

func key(kind, ns, name string) string { return strings.ToLower(kind) + "/" + ns + "/" + name }

func kindOf(resource string) string {
	switch strings.SplitN(resource, ".", 2)[0] {
	case "applications":
		return "application"
	case "deployment", "deployments":
		return "deployment"
	case "service", "services":
		return "service"
	case "pods":
		return "pod"
	}
	return resource
}

// Run implements k8s.Invoker.
func (c *Cluster) Run(_ context.Context, stdin string, args ...string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Calls = append(c.Calls, strings.Join(args, " "))
	ns := ""
	var rest []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--kubeconfig", "--context":
			i++
		case "-n":
			ns = args[i+1]
			i++
		default:
			rest = append(rest, args[i])
		}
	}
	if len(rest) == 0 {
		return "", errors.New("no verb")
	}
	flag := func(name string) string {
		for i, a := range rest {
			if a == name && i+1 < len(rest) {
				return rest[i+1]
			}
		}
		return ""
	}
	switch rest[0] {
	case "wait":
		return "condition met", nil
	case "apply", "create":
		var obj map[string]any
		if err := json.Unmarshal([]byte(stdin), &obj); err != nil {
			return "", err
		}
		items := []any{obj}
		if obj["kind"] == "List" {
			items, _ = obj["items"].([]any)
		}
		for _, it := range items {
			o, _ := it.(map[string]any)
			md, _ := o["metadata"].(map[string]any)
			name, _ := md["name"].(string)
			kind, _ := o["kind"].(string)
			switch kind {
			case "Namespace":
				c.namespaces[name] = true
			case "CustomResourceDefinition":
			default:
				ons, _ := md["namespace"].(string)
				if ons == "" {
					ons = ns
				}
				k := key(kind, ons, name)
				if _, ok := c.objects[k]; ok && rest[0] == "create" {
					return "", &k8s.RunError{Err: errors.New("exit status 1"), Stderr: "Error from server (AlreadyExists): already exists"}
				}
				c.uid++
				md["uid"] = fmt.Sprintf("uid-%d", c.uid)
				md["namespace"] = ons
				md["creationTimestamp"] = "2026-11-22T10:00:00Z"
				if kind == "Deployment" {
					spec, _ := o["spec"].(map[string]any)
					o["status"] = map[string]any{"readyReplicas": spec["replicas"], "replicas": spec["replicas"]}
				}
				if kind == "Service" {
					spec, _ := o["spec"].(map[string]any)
					spec["clusterIP"] = "10.96.0.10"
				}
				c.objects[k] = o
			}
		}
		return "", nil
	case "patch":
		kind := kindOf(rest[1])
		o, ok := c.objects[key(kind, ns, rest[2])]
		if !ok {
			return "", notFound(rest[1], rest[2])
		}
		var p map[string]any
		_ = json.Unmarshal([]byte(flag("-p")), &p)
		if ps, ok := p["spec"].(map[string]any); ok {
			spec, _ := o["spec"].(map[string]any)
			for k, v := range ps {
				spec[k] = v
			}
		}
		return "", nil
	case "get":
		res := rest[1]
		out := flag("-o")
		if res == "namespaces" {
			var names []string
			for n := range c.namespaces {
				names = append(names, n)
			}
			return strings.Join(names, " "), nil
		}
		if res == "namespace" {
			if !c.namespaces[rest[2]] {
				return "", notFound("namespaces", rest[2])
			}
			return "namespace/" + rest[2], nil
		}
		kind := kindOf(res)
		if sel := flag("-l"); sel != "" || (len(rest) > 2 && strings.HasPrefix(rest[2], "--all-namespaces")) || (len(rest) > 2 && rest[2] == "-o") {
			var items []any
			for k, o := range c.objects {
				if !strings.HasPrefix(k, kind+"/") {
					continue
				}
				md, _ := o["metadata"].(map[string]any)
				if ns != "" && md["namespace"] != ns {
					continue
				}
				if sel != "" {
					lk, lv, _ := strings.Cut(sel, "=")
					labels, _ := md["labels"].(map[string]any)
					if labels[lk] != lv {
						continue
					}
				}
				items = append(items, o)
			}
			b, _ := json.Marshal(map[string]any{"items": items})
			return string(b), nil
		}
		o, ok := c.objects[key(kind, ns, rest[2])]
		if !ok {
			return "", notFound(res, rest[2])
		}
		switch {
		case out == "name":
			return kind + "/" + rest[2], nil
		case strings.HasPrefix(out, "jsonpath={.metadata.uid}"):
			md, _ := o["metadata"].(map[string]any)
			return md["uid"].(string), nil
		}
		b, _ := json.Marshal(o)
		return string(b), nil
	case "delete":
		kind := kindOf(rest[1])
		if sel := flag("-l"); sel != "" {
			lk, lv, _ := strings.Cut(sel, "=")
			for k, o := range c.objects {
				md, _ := o["metadata"].(map[string]any)
				labels, _ := md["labels"].(map[string]any)
				if strings.HasPrefix(k, kind+"/"+ns+"/") && labels[lk] == lv {
					delete(c.objects, k)
				}
			}
			return "", nil
		}
		delete(c.objects, key(kind, ns, rest[2]))
		return "", nil
	}
	return "", fmt.Errorf("marketplacetest: kubectl %s is not simulated", strings.Join(rest, " "))
}
