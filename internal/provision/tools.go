package provision

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// One provisioner may drive several worker configs: AWS beside KubeVirt, say.
// Each is a Tool (a binary and a -config file) with a name, and the host book
// records which Tool owns each host, so that start, stop, status and destroy
// of a host run through the config that made it.

// ConfigProvider reads the provider= line of a worker config (the tool's
// default, gcp, when there is none). The file is read for that line only.
func ConfigProvider(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if k, v, ok := strings.Cut(line, "="); ok && strings.TrimSpace(k) == "provider" {
			if w := providerWord(strings.ToLower(strings.TrimSpace(strings.SplitN(v, "#", 2)[0]))); w != "" {
				return w
			}
		}
	}
	return "gcp"
}

// ToolEntry is one -provision-config entry: a config file and, when it is not
// the default binary, the worker binary that drives it.
type ToolEntry struct{ Config, Bin string }

// ParseToolEntries splits the value of -provision-config (or its environment
// variable): entries separated by commas, each CONFIG or CONFIG:BINARY. A
// single plain path is today's setting and means the same.
func ParseToolEntries(values ...string) ([]ToolEntry, error) {
	var out []ToolEntry
	for _, v := range values {
		for _, e := range strings.Split(v, ",") {
			e = strings.TrimSpace(e)
			if e == "" {
				continue
			}
			cfg, bin, _ := strings.Cut(e, ":")
			cfg, bin = strings.TrimSpace(cfg), strings.TrimSpace(bin)
			if cfg == "" {
				return nil, fmt.Errorf("provision config entry %q has no config file", e)
			}
			out = append(out, ToolEntry{Config: cfg, Bin: bin})
		}
	}
	return out, nil
}

// NameTools gives each tool its name: its provider, and provider-2, -3 when two
// configs share one. It sets Provider as well.
func NameTools(tools []*Tool) {
	seen := map[string]int{}
	for _, t := range tools {
		p := ConfigProvider(t.Config)
		if p == "" {
			p = "worker"
		}
		t.Provider = p
		seen[p]++
		t.Name = p
		if seen[p] > 1 {
			t.Name = fmt.Sprintf("%s-%d", p, seen[p])
		}
	}
}

// ErrNoTool is a host whose owning config is no longer set on this provisioner.
var ErrNoTool = errors.New("the worker config that owns this host is not set on this provisioner")

// Label is the tool's name for a message.
func (t *Tool) Label() string {
	if t.Name != "" {
		return t.Name
	}
	return filepath.Base(t.Config)
}
