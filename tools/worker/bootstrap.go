package main

import (
	_ "embed"
	"fmt"
	"regexp"
	"strings"
)

//go:embed scripts/startup.sh
var startupScript string

//go:embed scripts/bootstrap-root.sh
var bootstrapRoot string

//go:embed scripts/bootstrap-user.sh
var bootstrapUser string

//go:embed scripts/idle-check.sh
var idleCheck string

var shellSafe = regexp.MustCompile(`^[A-Za-z0-9._:/@%+=,-]*$`)

// withVars prepends VAR='value' assignments to a script. Values are checked
// against a conservative alphabet on top of quoting, so a config typo cannot
// turn into shell syntax on the VM.
func withVars(script string, vars [][2]string) (string, error) {
	var b strings.Builder
	b.WriteString("#!/bin/bash\n")
	for _, kv := range vars {
		if !shellSafe.MatchString(kv[1]) {
			return "", fmt.Errorf("refusing %s=%q: characters outside the safe set", kv[0], kv[1])
		}
		fmt.Fprintf(&b, "%s=%s\n", kv[0], shellQuote(kv[1]))
	}
	b.WriteString("export " + strings.Join(varNames(vars), " ") + "\n")
	// drop the script's own shebang
	if i := strings.Index(script, "\n"); strings.HasPrefix(script, "#!") && i >= 0 {
		script = script[i+1:]
	}
	b.WriteString(script)
	return b.String(), nil
}

func varNames(vars [][2]string) []string {
	out := make([]string, len(vars))
	for i, kv := range vars {
		out[i] = kv[0]
	}
	return out
}

func rootScript(c *Config) (string, error) {
	return withVars(bootstrapRoot, [][2]string{
		{"WK_USER", c.SSHUser},
		{"WK_IDLE_MINUTES", fmt.Sprint(c.IdleMinutes)},
	})
}

func userScript(c *Config, name, version string) (string, error) {
	return withVars(bootstrapUser, [][2]string{
		{"WK_VERSION", version},
		{"WK_RELEASES", c.ReleasesURL},
		{"WK_HERDR_URL", c.HerdrInstall},
		{"WK_HERDR_SHA", c.HerdrSHA256},
		{"WK_HUB", c.HubPeerURL},
		{"WK_NAME", name},
		{"WK_LOGIN", c.TSAllowLogin},
	})
}
