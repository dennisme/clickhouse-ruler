//go:build cluster

package cluster

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

const (
	rulerPort     = 9090
	rulerSelector = "app.kubernetes.io/name=clickhouse-ruler"
	gitSelector   = "app=git"
	chart         = "../../deploy/chart/clickhouse-ruler"

	// Where each delivery puts the rules, from the chart's own helpers.
	gitSyncLink    = "/rules/current"
	configMapMount = "/etc/clickhouse-ruler/rules"

	fixtures       = "../../deploy/kind"
	initSQL        = "../../deploy/clickhouse/init"
	installTimeout = 5 * time.Minute

	// A sync period of 2s plus one reload, with room for a kubelet that is
	// busy starting something else.
	chainTimeout = 90 * time.Second
)

// A rule that loads, in a group named by the caller so a test can tell a rule
// that arrived from one that was already running.
func ruleFile(group, alert string) string {
	return fmt.Sprintf(`groups:
  - name: %s
    interval: 2s
    rules:
      - alert: %s
        sources:
          team: payments
        expr: |
          SELECT ServiceName, max(Duration) / 1e6 AS value
          FROM otel.otel_traces
          WHERE Timestamp >= {{ .From }} AND Timestamp < {{ .To }}
          GROUP BY ServiceName
          HAVING value > 1e9
        window: 5m
        labels:
          team: payments
          severity: warning
        annotations:
          summary: "{{ .ServiceName }} is slow"
          runbook_url: https://example.com/runbook
`, group, alert)
}

// A file nobody can read, which is the only kind of finding that refuses a
// reading. An error-severity finding on any other check blocks a merge and
// loads anyway, raised on clickhouse_ruler_problem, because a ruler that will
// not start pages nobody (spec 7.6). So a nameless rule is the wrong fixture
// here and broken YAML is the right one.
const unreadableFile = `groups:
  - name: broken
   interval: 2s
	rules: [
`

// stack is the ClickHouse and the git server a release needs. ClickHouse is
// not optional: readiness is rules loaded and at least one source answering
// (spec 8.1), so a ruler with nowhere to query never becomes available and the
// chain would be asserted against a pod no operator would accept.
func stack(t *testing.T, name string) *Namespace {
	t.Helper()

	ns := NewNamespace(t, Context(t), name)
	ns.Kubectl("create", "configmap", "clickhouse-init",
		"--from-file="+initSQL+"/01-schema.sql",
		"--from-file="+initSQL+"/02-ruler-user.sql")
	ns.Apply(fixtures+"/clickhouse.yaml", fixtures+"/git-server.yaml")
	ns.WaitAvailable(installTimeout, "clickhouse", "git")
	return ns
}

// commit writes files into the rules repository and returns the sha, which is
// what every later assertion waits for. Paths are relative to the repository
// root, so a caller decides whether a rule sits in a subdirectory.
func commit(t *testing.T, ns *Namespace, message string, files map[string]string) string {
	t.Helper()

	var script strings.Builder
	script.WriteString("cd /work\n")
	for path, body := range files {
		script.WriteString(fmt.Sprintf("mkdir -p \"$(dirname %q)\"\n", path))
		script.WriteString(fmt.Sprintf("cat > %q <<'RULEFILE'\n%sRULEFILE\n", path, body))
	}
	script.WriteString("git add -A\n")
	script.WriteString(fmt.Sprintf("git commit -q -m %q\n", message))
	script.WriteString("git push -q origin HEAD:refs/heads/main\n")
	script.WriteString("git rev-parse HEAD\n")

	return strings.TrimSpace(ns.Exec(ns.Pod(gitSelector), "git", script.String()))
}

// groups is every rule group the ruler is currently ticking, by the group name
// at the end of its identifier. The identifier also carries the rule file's
// path in the rules tree, which is what identities asserts on.
func groups(t *testing.T, ns *Namespace) map[string]int {
	t.Helper()

	out := map[string]int{}
	pod := ns.Pod(rulerSelector)
	for _, s := range Find(ns.Metrics(pod, "ruler", rulerPort),
		"clickhouse_ruler_rule_group_iterations_total", nil) {
		id := s.Labels["rule_group"]
		if _, name, ok := strings.Cut(id, ":"); ok {
			out[name]++
		} else {
			out[id]++
		}
	}
	return out
}

// identities is every rule group the ruler is ticking, spelled the way the
// rule_group label spells it, sorted. This is the assertion the deployment
// chain was blind to: the identity has to be the rule's path in the rules tree,
// because git-sync hands the ruler a symlink at a worktree named after the
// commit and an identity built from the resolved path renames every series the
// ruler exposes on every merge (spec 8.2).
func identities(t *testing.T, ns *Namespace) []string {
	t.Helper()

	var out []string
	pod := ns.Pod(rulerSelector)
	for _, s := range Find(ns.Metrics(pod, "ruler", rulerPort),
		"clickhouse_ruler_rule_group_iterations_total", nil) {
		out = append(out, s.Labels["rule_group"])
	}
	sort.Strings(out)
	return out
}

// configuration is the one series of clickhouse_ruler_config_info: the revision
// of the files the ruler loaded and the root it read them from (spec 8.2).
func configuration(t *testing.T, ns *Namespace) Sample {
	t.Helper()

	pod := ns.Pod(rulerSelector)
	got := Find(ns.Metrics(pod, "ruler", rulerPort), "clickhouse_ruler_config_info", nil)
	if len(got) != 1 {
		t.Fatalf("clickhouse_ruler_config_info has %d series, want exactly 1: %v", len(got), got)
	}
	return got[0]
}

func reloadSucceeded(t *testing.T, ns *Namespace) (float64, bool) {
	t.Helper()

	pod := ns.Pod(rulerSelector)
	return Value(ns.Metrics(pod, "ruler", rulerPort),
		"clickhouse_ruler_config_last_reload_successful")
}

// The claim in spec 10.2 that nothing in CI proves: a merged rule reaches a
// running ruler and starts evaluating, through a sidecar's sync, a symlink
// flip, an exec hook and a reload. `helm.yaml` renders the chart and validates
// the manifests, which cannot show that a commit arrives.
func TestAMergedRuleReachesTheRulerAndEvaluates(t *testing.T) {
	ns := stack(t, "chain-git-sync")

	commit(t, ns, "the rule that is already running", map[string]string{
		"rules/payments/checkout.yaml": ruleFile("checkout", "CheckoutIsSlow"),
	})

	ns.Helm("install", "ruler", chart,
		"-f", fixtures+"/git-sync-values.yaml",
		"--wait", "--timeout", installTimeout.String())

	// The install itself is the first half of the chain: the init container
	// syncs before the ruler starts, so a release that became available has
	// already read rules through the symlink.
	if got := groups(t, ns); got["checkout"] != 1 {
		t.Fatalf("after install the ruler ticks %v, want exactly one checkout group", got)
	}

	// What the rule is called before the merge, and what configuration the
	// ruler says it is running. Both are compared with themselves afterwards.
	checkout := filepath.Join("payments", "checkout.yaml") + ":checkout"
	if got := identities(t, ns); len(got) != 1 || got[0] != checkout {
		t.Fatalf("after install the ruler ticks %v, want exactly %q: the identity is the rule's "+
			"path in the tree, not the worktree it was synced into", got, checkout)
	}
	before := configuration(t, ns)

	sha := commit(t, ns, "the rule that is merged while it runs", map[string]string{
		"rules/payments/orders.yaml": ruleFile("orders", "OrdersAreSlow"),
	})

	pod := ns.Pod(rulerSelector)
	Eventually(t, chainTimeout, "the symlink at the rules path flipping to the merged commit", func() (bool, string) {
		link := strings.TrimSpace(ns.Exec(pod, "ruler", "readlink "+gitSyncLink))
		return strings.HasSuffix(link, sha), link
	})

	Eventually(t, chainTimeout, "the merged rule evaluating", func() (bool, string) {
		got := groups(t, ns)
		return got["orders"] == 1 && got["checkout"] == 1, fmt.Sprint(got)
	})

	// The identity survived the merge. The worktree the rules were read from is
	// a different directory now, named after a different commit, and neither
	// the group that was already running nor the one that arrived is named
	// after it: rates hold across the merge and an alert on the running rule
	// was never resolved and recreated under a new name (spec 8.2).
	orders := filepath.Join("payments", "orders.yaml") + ":orders"
	want := []string{checkout, orders}
	if got := identities(t, ns); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("after the merge the ruler ticks %v, want %v", got, want)
	}

	// What did change is the one series that is supposed to: the revision of
	// the files, and the root they were read from.
	after := configuration(t, ns)
	if after.Labels["revision"] == before.Labels["revision"] {
		t.Errorf("clickhouse_ruler_config_info revision = %q both before and after the merge, "+
			"want the added rule file to have changed it", after.Labels["revision"])
	}
	if !strings.Contains(after.Labels["rules_root"], sha) {
		t.Errorf("clickhouse_ruler_config_info rules_root = %q, want the worktree holding %s",
			after.Labels["rules_root"], sha)
	}

	// The reload the hook asked for was accepted, which is the half of 8.2
	// that says the files running are the files that arrived.
	got, ok := reloadSucceeded(t, ns)
	if !ok {
		t.Fatal("the ruler exposes no clickhouse_ruler_config_last_reload_successful")
	}
	if got != 1 {
		t.Errorf("clickhouse_ruler_config_last_reload_successful = %v, want 1", got)
	}
}

// The other half, and the one a hook could hide. The exec hook posts with
// --fail, so a reload the ruler refused has to be a failed hook that git-sync
// retries, with the previous configuration still running. A hook reporting
// success there would leave an operator reading a ruler that silently kept old
// rules (spec 10.2, 7.9).
func TestARefusedReloadKeepsTheRunningRulesAndFailsTheHook(t *testing.T) {
	ns := stack(t, "chain-refusal")

	commit(t, ns, "the rule that is already running", map[string]string{
		"rules/payments/checkout.yaml": ruleFile("checkout", "CheckoutIsSlow"),
	})

	ns.Helm("install", "ruler", chart,
		"-f", fixtures+"/git-sync-values.yaml",
		"--wait", "--timeout", installTimeout.String())

	commit(t, ns, "a file nobody can read", map[string]string{
		"rules/payments/broken.yaml": unreadableFile,
	})

	Eventually(t, chainTimeout, "the ruler refusing the files it read", func() (bool, string) {
		got, ok := reloadSucceeded(t, ns)
		return ok && got == 0, fmt.Sprint(got, ok)
	})

	// The rules that were running are still running, which is the whole point
	// of refusing rather than loading what arrived.
	if got := groups(t, ns); got["checkout"] != 1 {
		t.Errorf("the ruler ticks %v, want the checkout group that was running before the refusal", got)
	}
	if got := groups(t, ns); got["broken"] != 0 {
		t.Errorf("the ruler ticks %v, want nothing from the refused file", got)
	}

	// git-sync's side of it. The hook is the sidecar's, not the ruler's, so
	// this is the one assertion with no metric behind it.
	pod := ns.Pod(rulerSelector)
	Eventually(t, chainTimeout, "git-sync reporting the hook failed", func() (bool, string) {
		logs, err := Try("kubectl", "--context", Context(t), "-n", ns.Name(),
			"logs", pod, "-c", "git-sync", "--tail=200")
		if err != nil {
			return false, logs
		}
		return strings.Contains(logs, "exechook"), "no exechook failure in the sidecar's log"
	})
}

// The layout 10.2 says constrains the loader rather than the chart. git-sync
// hands the ruler a path through a symlink to a worktree, and a root walked
// without being resolved finds no rule files at all. Unit tested, and until
// now never against a worktree a sidecar made.
func TestEachRuleLoadsOnceUnderASymlinkedRoot(t *testing.T) {
	ns := stack(t, "chain-symlinked-root")

	commit(t, ns, "two rule files in two directories", map[string]string{
		"rules/payments/checkout.yaml": ruleFile("checkout", "CheckoutIsSlow"),
		"rules/search/queries.yaml":    ruleFile("queries", "QueriesAreSlow"),
	})

	ns.Helm("install", "ruler", chart,
		"-f", fixtures+"/git-sync-values.yaml",
		"--wait", "--timeout", installTimeout.String())

	// Exactly once each. Finding nothing is the symlink not being resolved,
	// and finding either twice is a duplicate of itself.
	want := map[string]int{"checkout": 1, "queries": 1}
	Eventually(t, chainTimeout, "both rule groups ticking exactly once", func() (bool, string) {
		got := groups(t, ns)
		return got["checkout"] == want["checkout"] && got["queries"] == want["queries"] && len(got) == len(want),
			fmt.Sprint(got)
	})
}

// The other layout, and a different mechanism. kubelet's atomicity is the same
// trick: the real files sit in a timestamped directory, `..data` is a symlink
// to it, and every file at the root is a symlink through `..data`. A loader
// that does not skip `..*` finds every rule twice.
func TestEachRuleLoadsOnceUnderAConfigMapMount(t *testing.T) {
	ns := stack(t, "chain-configmap-mount")

	ns.Kubectl("create", "configmap", "rules",
		"--from-literal=checkout.yaml="+ruleFile("checkout", "CheckoutIsSlow"),
		"--from-literal=queries.yaml="+ruleFile("queries", "QueriesAreSlow"))

	ns.Helm("install", "ruler", chart,
		"-f", fixtures+"/configmap-values.yaml",
		"--wait", "--timeout", installTimeout.String())

	// The mount really is built out of symlinks, which is what makes the
	// assertion below mean anything.
	pod := ns.Pod(rulerSelector)
	listing := ns.Exec(pod, "ruler", "ls -a "+configMapMount)
	if !strings.Contains(listing, "..data") {
		t.Fatalf("the mount at %s carries no ..data, so this is not the layout under test:\n%s",
			configMapMount, listing)
	}

	want := map[string]int{"checkout": 1, "queries": 1}
	Eventually(t, chainTimeout, "both rule groups ticking exactly once", func() (bool, string) {
		got := groups(t, ns)
		return got["checkout"] == want["checkout"] && got["queries"] == want["queries"] && len(got) == len(want),
			fmt.Sprint(got)
	})
}
