// Package provisioner checks the example manifests of the in-cluster
// provisioner (issue #78): the Role is exactly the planned list, no manifest
// carries a Secret value, and the set builds with kustomize.
//
// The module has no YAML library and gains none for this: the rules of a Role
// are parsed from the one-line-per-field layout rbac.yaml uses, and a layout the
// parser does not recognise is an error, not a pass.
package provisioner

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// rule is one entry of a Role's rules.
type rule struct {
	groups, resources, verbs, names []string
}

// allowed is the whole of what the provisioner Role may grant: group/resource
// -> the verbs. It is the plan's list (the worker tool's calls in
// tools/worker/kubevirt_live.go). Growing the Role means changing this table in
// the same commit.
var allowed = map[string][]string{
	"kubevirt.io/virtualmachines":                                  {"get", "list", "watch", "create", "patch", "delete"},
	"kubevirt.io/virtualmachineinstances":                          {"get", "list", "watch"},
	"subresources.kubevirt.io/virtualmachines/start":               {"update"},
	"subresources.kubevirt.io/virtualmachines/stop":                {"update"},
	"subresources.kubevirt.io/virtualmachines/portforward":         {"get"},
	"subresources.kubevirt.io/virtualmachineinstances/portforward": {"get"},
	"cdi.kubevirt.io/datavolumes":                                  {"get", "list", "watch", "create", "delete"},
	"cdi.kubevirt.io/datavolumes/source":                           {"create"},
	"/persistentvolumeclaims":                                      {"get", "list", "watch", "delete"},
	"/secrets":                                                     {"get", "list", "create", "update", "delete"},
	"/pods":                                                        {"get", "list", "watch"},
	"/events":                                                      {"get", "list", "watch"},
	"/resourcequotas":                                              {"get", "list", "watch"},
	"/limitranges":                                                 {"get", "list", "watch"},
}

// splitDocs splits a multi-document YAML file on "---" lines.
func splitDocs(src string) []string {
	var docs []string
	var cur []string
	for _, l := range strings.Split(src, "\n") {
		if strings.TrimRight(l, " \t") == "---" {
			docs = append(docs, strings.Join(cur, "\n"))
			cur = nil
			continue
		}
		cur = append(cur, l)
	}
	return append(docs, strings.Join(cur, "\n"))
}

var topKey = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9]*):\s*(.*)$`)

// topLevel returns a document's scalar top-level fields (kind, apiVersion) and
// the set of its top-level keys.
func topLevel(doc string) (scalars map[string]string, keys map[string]bool) {
	scalars, keys = map[string]string{}, map[string]bool{}
	for _, l := range strings.Split(doc, "\n") {
		if m := topKey.FindStringSubmatch(l); m != nil {
			keys[m[1]] = true
			scalars[m[1]] = strings.TrimSpace(strings.SplitN(m[2], " #", 2)[0])
		}
	}
	return
}

// metaName is metadata.name of a document (the first two-space "name:" line).
func metaName(doc string) string {
	for _, l := range strings.Split(doc, "\n") {
		if m := regexp.MustCompile(`^  name:\s*(\S+)`).FindStringSubmatch(l); m != nil {
			return m[1]
		}
	}
	return ""
}

var flowList = regexp.MustCompile(`^\[(.*)\]$`)

func parseList(s string) ([]string, error) {
	s = strings.TrimSpace(strings.SplitN(s, " #", 2)[0])
	m := flowList.FindStringSubmatch(s)
	if m == nil {
		return nil, fmt.Errorf("not a one-line [a, b] list: %q", s)
	}
	var out []string
	for _, p := range strings.Split(m[1], ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		p = strings.Trim(p, `"`)
		out = append(out, p)
	}
	return out, nil
}

// parseRules reads the rules of a Role or ClusterRole document.
func parseRules(doc string) ([]rule, error) {
	var rules []rule
	in := false
	var cur *rule
	for n, l := range strings.Split(doc, "\n") {
		t := strings.TrimSpace(l)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		if !in {
			in = strings.HasPrefix(l, "rules:")
			continue
		}
		if !strings.HasPrefix(l, " ") { // the next top-level key
			break
		}
		t = strings.TrimPrefix(t, "- ")
		if strings.HasPrefix(strings.TrimLeft(l, " "), "- ") {
			rules = append(rules, rule{})
			cur = &rules[len(rules)-1]
		}
		if cur == nil {
			return nil, fmt.Errorf("line %d: field outside a rule: %q", n+1, l)
		}
		k, v, ok := strings.Cut(t, ":")
		if !ok {
			return nil, fmt.Errorf("line %d: unrecognised rule layout: %q", n+1, l)
		}
		list, err := parseList(v)
		if err != nil {
			return nil, fmt.Errorf("line %d (%s): %v", n+1, k, err)
		}
		switch strings.TrimSpace(k) {
		case "apiGroups":
			cur.groups = list
		case "resources":
			cur.resources = list
		case "verbs":
			cur.verbs = list
		case "resourceNames":
			cur.names = list
		default:
			return nil, fmt.Errorf("line %d: unexpected rule field %q (nonResourceURLs and the like are not allowed)", n+1, k)
		}
	}
	if len(rules) == 0 {
		return nil, fmt.Errorf("no rules found")
	}
	return rules, nil
}

// checkRole returns every way a Role's rules break the allow-list.
func checkRole(rules []rule) []string {
	var bad []string
	for i, r := range rules {
		if len(r.groups) == 0 || len(r.resources) == 0 || len(r.verbs) == 0 {
			bad = append(bad, fmt.Sprintf("rule %d: apiGroups, resources and verbs are all required", i+1))
		}
		if len(r.names) > 0 {
			bad = append(bad, fmt.Sprintf("rule %d: resourceNames are not part of the plan", i+1))
		}
		for _, x := range append(append(append([]string{}, r.groups...), r.resources...), r.verbs...) {
			if strings.Contains(x, "*") {
				bad = append(bad, fmt.Sprintf("rule %d: wildcard %q", i+1, x))
			}
		}
		for _, g := range r.groups {
			for _, res := range r.resources {
				verbs, ok := allowed[g+"/"+res]
				if !ok {
					bad = append(bad, fmt.Sprintf("rule %d: %s/%s is not in the plan's list", i+1, g, res))
					continue
				}
				for _, v := range r.verbs {
					if !contains(verbs, v) {
						bad = append(bad, fmt.Sprintf("rule %d: verb %q on %s/%s is not in the plan's list", i+1, v, g, res))
					}
				}
			}
		}
	}
	return bad
}

func contains(s []string, x string) bool {
	for _, y := range s {
		if y == x {
			return true
		}
	}
	return false
}

// granted is group/resource -> verbs, as the rules grant them.
func granted(rules []rule) map[string]map[string]bool {
	g := map[string]map[string]bool{}
	for _, r := range rules {
		for _, gr := range r.groups {
			for _, res := range r.resources {
				k := gr + "/" + res
				if g[k] == nil {
					g[k] = map[string]bool{}
				}
				for _, v := range r.verbs {
					g[k][v] = true
				}
			}
		}
	}
	return g
}

// secretValues lists the Secrets in a file that carry data (data, stringData).
func secretValues(file, src string) []string {
	var bad []string
	for _, doc := range splitDocs(src) {
		sc, keys := topLevel(doc)
		if sc["kind"] != "Secret" {
			continue
		}
		for _, k := range []string{"data", "stringData", "binaryData"} {
			if keys[k] {
				bad = append(bad, fmt.Sprintf("%s: Secret %s has %s", file, metaName(doc), k))
			}
		}
	}
	return bad
}

// credentialShaped matches the text of a Tailscale key or token anywhere in a
// manifest, whatever object it is in.
var credentialShaped = regexp.MustCompile(`tskey-[A-Za-z0-9]|-----BEGIN [A-Z ]*PRIVATE KEY|AKIA[0-9A-Z]{16}|ghp_[A-Za-z0-9]{20}`)

func yamlFiles(t *testing.T) map[string]string {
	t.Helper()
	m, err := filepath.Glob("*.yaml")
	if err != nil || len(m) == 0 {
		t.Fatalf("no manifests: %v", err)
	}
	out := map[string]string{}
	for _, f := range m {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		out[f] = string(b)
	}
	return out
}

func TestProvisionerRoleIsExactlyThePlan(t *testing.T) {
	src := yamlFiles(t)["rbac.yaml"]
	var rules []rule
	for _, doc := range splitDocs(src) {
		sc, _ := topLevel(doc)
		if sc["kind"] != "Role" || metaName(doc) != "comebye-provisioner" {
			continue
		}
		var err error
		if rules, err = parseRules(doc); err != nil {
			t.Fatalf("rbac.yaml Role comebye-provisioner: %v", err)
		}
	}
	if rules == nil {
		t.Fatal("rbac.yaml has no Role comebye-provisioner")
	}
	for _, b := range checkRole(rules) {
		t.Error(b)
	}
	// Create and destroy must be covered: the narrow Role replaces binding the
	// wider comebye-worker-tool one.
	g := granted(rules)
	for k, verbs := range allowed {
		for _, v := range verbs {
			if !g[k][v] {
				t.Errorf("the Role lacks %q on %s, which the tool needs", v, k)
			}
		}
	}
}

func TestEveryRoleIsNamespacedAndWithoutWildcards(t *testing.T) {
	for f, src := range yamlFiles(t) {
		for _, doc := range splitDocs(src) {
			sc, _ := topLevel(doc)
			switch sc["kind"] {
			case "ClusterRole", "ClusterRoleBinding":
				t.Errorf("%s: %s %s: cluster scope is not allowed here", f, sc["kind"], metaName(doc))
			case "Role":
				rules, err := parseRules(doc)
				if err != nil {
					t.Errorf("%s: Role %s: %v", f, metaName(doc), err)
				}
				for _, r := range rules {
					for _, x := range append(append(append([]string{}, r.groups...), r.resources...), r.verbs...) {
						if strings.Contains(x, "*") {
							t.Errorf("%s: Role %s: wildcard %q", f, metaName(doc), x)
						}
					}
				}
			case "RoleBinding":
				// A RoleBinding may only reference a Role, never a ClusterRole.
				if regexp.MustCompile(`(?m)^  kind:\s*ClusterRole\b`).MatchString(doc) {
					t.Errorf("%s: RoleBinding %s references a ClusterRole", f, metaName(doc))
				}
			}
		}
	}
}

func TestCheckRoleCatchesWhatItMust(t *testing.T) {
	base := func() []rule {
		return []rule{{groups: []string{"kubevirt.io"}, resources: []string{"virtualmachines"}, verbs: []string{"get"}}}
	}
	cases := map[string]func([]rule) []rule{
		"wildcard verb":     func(r []rule) []rule { r[0].verbs = []string{"*"}; return r },
		"wildcard resource": func(r []rule) []rule { r[0].resources = []string{"*"}; return r },
		"wildcard group":    func(r []rule) []rule { r[0].groups = []string{"*"}; return r },
		"extra verb":        func(r []rule) []rule { r[0].verbs = []string{"get", "update"}; return r },
		"extra resource":    func(r []rule) []rule { r[0].resources = []string{"nodes"}; return r },
		"core group nodes":  func(r []rule) []rule { r[0].groups = []string{""}; r[0].resources = []string{"nodes"}; return r },
		"resourceNames":     func(r []rule) []rule { r[0].names = []string{"x"}; return r },
		"escalate":          func(r []rule) []rule { r[0].verbs = []string{"escalate"}; return r },
	}
	if bad := checkRole(base()); len(bad) != 0 {
		t.Fatalf("a valid rule was refused: %v", bad)
	}
	for name, mut := range cases {
		if len(checkRole(mut(base()))) == 0 {
			t.Errorf("%s: not caught", name)
		}
	}
	if _, err := parseRules("rules:\n  - apiGroups:\n      - kubevirt.io\n"); err == nil {
		t.Error("a multi-line list was accepted: the parser must refuse a layout it cannot read")
	}
	if _, err := parseRules("rules:\n  - nonResourceURLs: [\"/metrics\"]\n    verbs: [\"get\"]\n"); err == nil {
		t.Error("nonResourceURLs was accepted")
	}
}

func TestNoManifestHoldsASecretValue(t *testing.T) {
	for f, src := range yamlFiles(t) {
		for _, b := range secretValues(f, src) {
			t.Error(b)
		}
		if m := credentialShaped.FindString(src); m != "" {
			t.Errorf("%s: looks like a credential (%q...)", f, m)
		}
	}
	// The lint itself.
	if len(secretValues("x.yaml", "kind: Secret\nmetadata:\n  name: a\nstringData:\n  k: v\n")) == 0 {
		t.Error("lint missed stringData")
	}
	if len(secretValues("x.yaml", "apiVersion: v1\nkind: Secret\nmetadata:\n  name: a\n---\nkind: Secret\nmetadata:\n  name: b\ndata:\n  k: dg==\n")) != 1 {
		t.Error("lint should flag exactly the second Secret")
	}
	if len(secretValues("x.yaml", "kind: ConfigMap\ndata:\n  k: v\n")) != 0 {
		t.Error("lint flagged a ConfigMap")
	}
}

func TestManifestShape(t *testing.T) {
	f := yamlFiles(t)
	d := f["deployment.yaml"]
	for _, want := range []string{
		"replicas: 1", "strategy: {type: Recreate}", "automountServiceAccountToken: true",
		"serviceAccountName: comebye-provisioner", "NET_ADMIN", "-hub-only", "-ledger=false",
		// The worker directory (ssh keys, known_hosts) is on the claim.
		"XDG_STATE_HOME, value: /var/lib/comebye-worker", "{name: worker, mountPath: /var/lib/comebye-worker}", "-provision-config=",
	} {
		if !strings.Contains(d, want) {
			t.Errorf("deployment.yaml: missing %q", want)
		}
	}
	if strings.Contains(d, "\n            - -provision-create") {
		t.Error("deployment.yaml turns -provision-create on: it ships off")
	}
	if !strings.Contains(f["pvc.yaml"], "storage: 100Mi") {
		t.Error("pvc.yaml: the worker directory claim is 100Mi")
	}
	if !strings.Contains(f["configmap.yaml"], "\n    state_dir=/var/lib/comebye-worker\n") {
		t.Error("configmap.yaml: the tool's state_dir must be the claim's mount path")
	}
	if !strings.Contains(f["configmap.yaml"], "provider=kubevirt") {
		t.Error("configmap.yaml: not a kubevirt worker config")
	}
	var names []string
	for _, src := range f {
		for _, doc := range splitDocs(src) {
			sc, _ := topLevel(doc)
			if sc["kind"] == "Secret" {
				names = append(names, metaName(doc))
			}
		}
	}
	sort.Strings(names)
	if strings.Join(names, ",") != "comebye-provisioner-tailscale-auth,comebye-provisioner-tailscale-oauth" {
		t.Errorf("placeholder Secrets = %v", names)
	}
}

func TestKustomizeBuilds(t *testing.T) {
	kc, err := exec.LookPath("kubectl")
	if err != nil {
		t.Skip("kubectl is not on PATH")
	}
	var out, errb bytes.Buffer
	cmd := exec.Command(kc, "kustomize", ".")
	cmd.Stdout, cmd.Stderr = &out, &errb
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir()}
	if err := cmd.Run(); err != nil {
		t.Fatalf("kubectl kustomize: %v\n%s", err, errb.String())
	}
	built := out.String()
	for _, want := range []string{"kind: Deployment", "kind: PersistentVolumeClaim", "kind: ServiceAccount", "kind: RoleBinding", "namespace: comebye-workers"} {
		if !strings.Contains(built, want) {
			t.Errorf("built output lacks %q", want)
		}
	}
	// The built output is what gets applied: lint it too.
	for _, b := range secretValues("kustomize output", built) {
		t.Error(b)
	}
	if strings.Contains(built, "kind: ClusterRole") {
		t.Error("kustomize output holds a ClusterRole")
	}
}
