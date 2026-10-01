// Package cluster drives a Kubernetes cluster from a test, so the deployment
// chain in spec 10.2 is proven rather than rendered.
//
// It shells out to kubectl and helm rather than taking a Kubernetes client
// dependency. What these tests assert is the chain an operator installs, and
// the commands are the same ones they would run; a client library would be a
// second way to describe the same objects and a module nothing else needs.
package cluster

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Context is the kubectl context the tests act on, which the `just` recipe
// creates. Nothing here creates or deletes a cluster: a test that tore one
// down on failure would take the evidence with it.
func Context(t *testing.T) string {
	t.Helper()

	ctx := os.Getenv("RULER_KUBE_CONTEXT")
	if ctx == "" {
		t.Fatal("RULER_KUBE_CONTEXT must be set, run `just cluster-test`")
	}
	return ctx
}

// Run executes a command and returns its combined output, failing the test on
// a non-zero exit. The output is in the failure because a kubectl error says
// what is wrong and a test reporting only the exit code throws that away.
func Run(t *testing.T, name string, args ...string) string {
	t.Helper()

	out, err := exec.Command(name, args...).CombinedOutput() //nolint:gosec // kubectl and helm with arguments a test wrote
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return string(out)
}

// Try is Run for a command that is allowed to fail, which is what asserting a
// refusal needs.
func Try(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput() //nolint:gosec // kubectl and helm with arguments a test wrote
	return string(out), err
}

// Namespace is one test's own namespace, deleted when the test ends.
type Namespace struct {
	t    *testing.T
	ctx  string
	name string
}

// NewNamespace creates it. The name carries the test's own, so a cluster left
// up after a failure says which test left what behind.
func NewNamespace(t *testing.T, kubeContext, name string) *Namespace {
	t.Helper()

	ns := &Namespace{t: t, ctx: kubeContext, name: name}
	_, _ = Try("kubectl", "--context", kubeContext, "delete", "namespace", name, "--ignore-not-found", "--wait=true")
	Run(t, "kubectl", "--context", kubeContext, "create", "namespace", name)

	t.Cleanup(func() {
		// Backgrounded: a namespace takes tens of seconds to finish
		// terminating and no later test waits on this one's.
		_, _ = Try("kubectl", "--context", kubeContext, "delete", "namespace", name, "--wait=false")
	})
	return ns
}

func (n *Namespace) Name() string { return n.name }

// Kubectl runs a kubectl command in this namespace.
func (n *Namespace) Kubectl(args ...string) string {
	n.t.Helper()
	return Run(n.t, "kubectl", append([]string{"--context", n.ctx, "-n", n.name}, args...)...)
}

// Helm runs a helm command against this namespace.
func (n *Namespace) Helm(args ...string) string {
	n.t.Helper()
	return Run(n.t, "helm", append([]string{"--kube-context", n.ctx, "-n", n.name}, args...)...)
}

// Apply sends manifests to the namespace.
func (n *Namespace) Apply(paths ...string) {
	n.t.Helper()

	args := []string{"apply"}
	for _, p := range paths {
		args = append(args, "-f", p)
	}
	n.Kubectl(args...)
}

// WaitAvailable blocks until those Deployments report available, which is the
// one wait that belongs to kubectl rather than to a poll loop here.
func (n *Namespace) WaitAvailable(timeout time.Duration, deployments ...string) {
	n.t.Helper()

	args := []string{"wait", "--for=condition=available", "--timeout=" + timeout.String()}
	for _, d := range deployments {
		args = append(args, "deployment/"+d)
	}
	n.Kubectl(args...)
}

// Pod is the name of the single pod matching a label selector.
func (n *Namespace) Pod(selector string) string {
	n.t.Helper()

	out := strings.TrimSpace(n.Kubectl("get", "pod", "-l", selector,
		"--field-selector=status.phase=Running", "-o", "name"))
	if out == "" {
		n.t.Fatalf("no running pod matches %s", selector)
	}
	if names := strings.Fields(out); len(names) != 1 {
		n.t.Fatalf("%d pods match %s, want 1: %v", len(names), selector, names)
	}
	return out
}

// Exec runs a shell script inside a container, returning its output. Used to
// commit inside the git pod, which is also how a commit becomes observable:
// the sha it prints is what later assertions wait for.
func (n *Namespace) Exec(pod, container, script string) string {
	n.t.Helper()
	return n.Kubectl("exec", pod, "-c", container, "--", "sh", "-ec", script)
}

// Eventually polls until check passes or the deadline does. The description is
// what the failure reports, so it says what was being waited for rather than
// that something timed out.
func Eventually(t *testing.T, timeout time.Duration, what string, check func() (bool, string)) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	var last string
	for {
		ok, state := check()
		if ok {
			return
		}
		last = state
		if time.Now().After(deadline) {
			t.Fatalf("%s did not happen within %s\n  last seen: %s", what, timeout, last)
		}
		time.Sleep(time.Second)
	}
}

// Sample is one line of Prometheus exposition: a metric name, its labels and
// its value.
type Sample struct {
	Name   string
	Labels map[string]string
	Value  float64
}

// Metrics reads the ruler's /metrics from inside its own container. From
// inside, because a port-forward is a second moving part that fails for its own
// reasons and the surface under test is the one the exec hook posts to.
func (n *Namespace) Metrics(pod, container string, port int) []Sample {
	n.t.Helper()

	raw := n.Exec(pod, container, fmt.Sprintf("wget -qO- http://127.0.0.1:%d/metrics", port))
	return parse(n.t, raw)
}

func parse(t *testing.T, raw string) []Sample {
	t.Helper()

	var out []Sample
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		head, rawValue, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		value, err := strconv.ParseFloat(strings.TrimSpace(rawValue), 64)
		if err != nil {
			continue
		}

		s := Sample{Name: head, Labels: map[string]string{}, Value: value}
		if name, rest, hasLabels := strings.Cut(head, "{"); hasLabels {
			s.Name = name
			for _, pair := range splitLabels(strings.TrimSuffix(rest, "}")) {
				if k, v, ok := strings.Cut(pair, "="); ok {
					s.Labels[strings.TrimSpace(k)] = strings.Trim(v, `"`)
				}
			}
		}
		out = append(out, s)
	}
	return out
}

// splitLabels splits on commas outside quotes, since a label value may hold
// one: a rule group carries a file path and a path may hold anything.
func splitLabels(in string) []string {
	var out []string
	var current strings.Builder
	inQuotes := false

	for _, r := range in {
		switch {
		case r == '"':
			inQuotes = !inQuotes
			current.WriteRune(r)
		case r == ',' && !inQuotes:
			out = append(out, current.String())
			current.Reset()
		default:
			current.WriteRune(r)
		}
	}
	if current.Len() > 0 {
		out = append(out, current.String())
	}
	return out
}

// Find is every sample of that metric whose labels all match.
func Find(samples []Sample, name string, labels map[string]string) []Sample {
	var out []Sample
next:
	for _, s := range samples {
		if s.Name != name {
			continue
		}
		for k, want := range labels {
			if s.Labels[k] != want {
				continue next
			}
		}
		out = append(out, s)
	}
	return out
}

// Value is a single-series metric's value, and whether it was there at all.
func Value(samples []Sample, name string) (float64, bool) {
	for _, s := range samples {
		if s.Name == name {
			return s.Value, true
		}
	}
	return 0, false
}
