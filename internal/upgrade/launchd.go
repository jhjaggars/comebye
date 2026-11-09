package upgrade

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
)

// DefaultLaunchdLabel is the label the macOS install guide gives the agent.
const DefaultLaunchdLabel = "com.jhjaggars.comebye"

var plistLabelRe = regexp.MustCompile(`<key>Label</key>\s*<string>([^<]+)</string>`)

func (o *Options) launchdMode() bool { return o.Restart == RestartLaunchd }

func (o *Options) uid() int {
	if o.UID > 0 {
		return o.UID
	}
	return os.Getuid()
}

// launchdTarget is the service target "gui/<uid>/<label>".
func (o *Options) launchdTarget() string {
	return "gui/" + strconv.Itoa(o.uid()) + "/" + o.LaunchdLabel
}

func (o *Options) launchctlRun(args ...string) (string, error) {
	cmd := exec.Command(o.Launchctl, args...)
	cmd.Env = o.Env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// launchdAvailable reports whether launchctl exists and the labelled agent
// is loaded in this user's GUI domain.
func launchdAvailable(o *Options) bool {
	if _, err := exec.LookPath(o.Launchctl); err != nil {
		return false
	}
	_, err := o.launchctlRun("print", o.launchdTarget())
	return err == nil
}

// findLaunchdLabel names the agent that runs this install's binary: the
// label of the plist under LaunchAgents that mentions o.Bin, else the
// default label. "" when there is none.
func findLaunchdLabel(o *Options) string {
	dir := o.LaunchAgentsDir
	if dir == "" {
		if home, err := os.UserHomeDir(); err == nil {
			dir = filepath.Join(home, "Library", "LaunchAgents")
		}
	}
	if dir != "" {
		ents, _ := os.ReadDir(dir)
		bins := []string{o.Bin, cleanExe(o.Bin)}
		for _, e := range ents {
			if !strings.HasSuffix(e.Name(), ".plist") {
				continue
			}
			b, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				continue
			}
			s := string(b)
			m := plistLabelRe.FindStringSubmatch(s)
			if m == nil {
				continue
			}
			for _, bin := range bins {
				if strings.Contains(s, "<string>"+bin+"</string>") {
					return m[1]
				}
			}
		}
	}
	return DefaultLaunchdLabel
}

// resolveLaunchdAuto picks launchd for auto mode: macOS, launchctl present
// and the agent loaded. It sets o.LaunchdLabel on success.
func resolveLaunchdAuto(o *Options) bool {
	if o.hostOS() != "darwin" {
		return false
	}
	if o.LaunchdLabel == "" {
		o.LaunchdLabel = findLaunchdLabel(o)
	}
	return launchdAvailable(o)
}

func (o *Options) hostOS() string {
	if o.HostOS != "" {
		return o.HostOS
	}
	return runtime.GOOS
}

// restartLaunchd kills and restarts the agent: launchctl kickstart -k.
func restartLaunchd(o *Options) error {
	o.printf("restarting %s with launchctl kickstart -k\n", o.launchdTarget())
	if out, err := o.launchctlRun("kickstart", "-k", o.launchdTarget()); err != nil {
		return fmt.Errorf("launchctl kickstart -k %s: %v: %s", o.launchdTarget(), err, strings.TrimSpace(out))
	}
	return nil
}

// waitLaunchd waits for the agent to be running and the local server to
// answer.
func waitLaunchd(o *Options) error {
	tries := max(o.SmokeTries, 30)
	var last string
	for i := 0; i < tries; i++ {
		out, err := o.launchctlRun("print", o.launchdTarget())
		last = "not loaded"
		if err == nil {
			last = launchdState(out)
			if last == "running" && indexTag(o) != "" {
				return nil
			}
		}
		o.Sleep(o.SmokeWait)
	}
	return fmt.Errorf("%s is %q, or the local server at %s does not answer", o.launchdTarget(), last, o.LocalURL)
}

var launchdStateRe = regexp.MustCompile(`(?m)^\s*state = (\S+)`)

func launchdState(print string) string {
	if m := launchdStateRe.FindStringSubmatch(print); m != nil {
		return m[1]
	}
	return "unknown"
}
