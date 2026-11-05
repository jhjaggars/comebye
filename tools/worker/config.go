package main

import (
	"bufio"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Config is the provisioner's settings. It lives outside the repo, in a
// KEY=VALUE file (blank lines and # comments allowed), by default
// ~/.config/comebye-worker/config. Nothing secret belongs in it: the
// Tailscale OAuth client has its own 0600 file (TSOAuthFile).
type Config struct {
	Provider string // gcp (default) or aws

	AWSRegion  string
	AWSAZ      string
	AWSProfile string // optional: a named profile of the user's AWS CLI config
	ImageID    string // aws: an AMI id instead of the Debian SSM parameter
	AWSSpot    bool   // aws: launch a persistent spot instance that stops (not terminates) when reclaimed
	VPCRange   string // aws: CIDR of the dedicated VPC; SubnetRange lies inside it

	machineSet, diskSet bool

	Project     string // GCP project id
	Zone        string
	MachineType string
	BootDiskGB  int
	DataDiskGB  int
	DiskType    string

	ImageProject string
	ImageFamily  string

	Network       string // dedicated VPC, created if missing
	SubnetRange   string // CIDR of the dedicated subnet
	SSHSourceCIDR string // the one /32 allowed to reach tcp:22
	SSHUser       string

	IdleMinutes    int    // auto-poweroff after this long with no ssh session and no working agent
	MaxRunDuration string // GCE backstop ("24h"); empty disables

	Tag           string // tailnet tag the workers carry
	TSOAuthFile   string // OAuth client (client_id / client_secret), mode 0600
	TSAuthKeyFile string // fallback: a hand-minted single-use tagged key in a mode-0600 file; deleted after use
	TSAPIBase     string // Tailscale API base (tests; default https://api.tailscale.com)
	TSAllowLogin  string // COMEBYE_TS_ALLOW_LOGIN on the worker: the login that owns the hub device

	HubURL     string // the hub's Serve URL, as the user reaches it (version lookup, peer removal)
	HubPeerURL string // COMEBYE_HUB on the worker: the hub's Serve URL https://<hub>.<tailnet>.ts.net, or the legacy http://<hub>.<tailnet>.ts.net:<peer listen port>
	HubVersion string // optional: pin the comebye version instead of asking the hub

	ReleasesURL  string // comebye release base URL
	HerdrInstall string // URL of herdr's install script
	HerdrSHA256  string // optional: required sha256 of that script

	// Agent harnesses installed on the worker (never signed in). pi is the
	// default; claude and codex are optional.
	Agents    []string
	PiVersion string // npm version of @earendil-works/pi-coding-agent, pinned

	// The LLM gateway (LiteLLM). Off unless LLMGatewayURL is set: then each
	// worker gets its own virtual key, limited by the fields below.
	LLMGatewayURL         string
	LLMProvisionerKeyFile string // key-management routes only; mode 0600
	LLMModels             []string
	LLMMaxBudget          float64 // USD per budget period
	LLMBudgetDuration     string  // LiteLLM duration: 30d, 12h, ...
	LLMRPM                int
	LLMKeyDuration        string // optional: the key itself expires after this long (LiteLLM duration)

	StateDir string // per-worker ssh config and known_hosts
	path     string
}

var (
	nameRe    = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,28}[a-z0-9])?$`)
	projectRe = regexp.MustCompile(`^[a-z][a-z0-9-]{4,28}[a-z0-9]$`)
	zoneRe    = regexp.MustCompile(`^[a-z]+-[a-z]+[0-9]+-[a-z]$`)
	resRe     = regexp.MustCompile(`^[a-z][a-z0-9-]{0,40}[a-z0-9]$`)
	tagRe     = regexp.MustCompile(`^tag:[a-z][a-z0-9-]*$`)
	userRe    = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,30}$`)
	durRe     = regexp.MustCompile(`^[0-9]+[smhd]$`)
	versionRe = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`)
	piVerRe   = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
	modelRe   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/:-]{0,100}$`)
	llmDurRe  = regexp.MustCompile(`^[1-9][0-9]{0,3}(s|m|h|d|w|mo)$`)
	loginRe   = regexp.MustCompile(`^[A-Za-z0-9._%+@-]+$`)
)

// DefaultConfigPath is ~/.config/comebye-worker/config (XDG aware).
func DefaultConfigPath() string {
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "comebye-worker", "config")
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".config", "comebye-worker", "config")
}

func defaults() Config {
	h, _ := os.UserHomeDir()
	cfgDir := filepath.Join(h, ".config", "comebye-worker")
	state := filepath.Join(h, ".local", "state", "comebye-worker")
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		state = filepath.Join(d, "comebye-worker")
	}
	return Config{
		Provider:       "gcp",
		VPCRange:       "10.77.0.0/16",
		MachineType:    "e2-standard-4",
		BootDiskGB:     20,
		DataDiskGB:     50,
		DiskType:       "pd-balanced",
		ImageProject:   "debian-cloud",
		ImageFamily:    "debian-12",
		Network:        "comebye-worker",
		SubnetRange:    "10.77.0.0/24",
		SSHUser:        "worker",
		IdleMinutes:    120,
		MaxRunDuration: "24h",
		Tag:            "tag:worker",
		TSOAuthFile:    filepath.Join(cfgDir, "tailscale-oauth"),
		TSAuthKeyFile:  filepath.Join(cfgDir, "authkey"),
		TSAPIBase:      "https://api.tailscale.com",
		ReleasesURL:    "https://github.com/jhjaggars/comebye/releases",
		HerdrInstall:   "https://herdr.dev/install.sh",
		StateDir:       state,

		Agents:                []string{"pi"},
		PiVersion:             "0.85.1",
		LLMProvisionerKeyFile: filepath.Join(cfgDir, "llm-provisioner-key"),
		LLMModels:             []string{"rits/zai-org/glm-5-3"},
		LLMMaxBudget:          5,
		LLMBudgetDuration:     "30d",
		LLMRPM:                30,
	}
}

// parseKV reads KEY=VALUE lines. Unknown keys are an error, so a typo does
// not silently fall back to a default.
func parseKV(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]string{}
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("%s:%d: want KEY=VALUE", path, n)
		}
		out[strings.ToLower(strings.TrimSpace(k))] = strings.Trim(strings.TrimSpace(v), `"'`)
	}
	return out, sc.Err()
}

// LoadConfig reads path over the defaults and validates the result.
func LoadConfig(path string) (*Config, error) {
	c := defaults()
	c.path = path
	kv, err := parseKV(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("no config at %s: copy tools/worker/config.example there and fill it in", path)
		}
		return nil, err
	}
	if err := c.apply(kv); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if c.Provider == "aws" {
		if !c.machineSet {
			c.MachineType = "t3.xlarge"
		}
		if !c.diskSet {
			c.DiskType = "gp3"
		}
	}
	return &c, c.Validate()
}

func expand(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		h, _ := os.UserHomeDir()
		return filepath.Join(h, strings.TrimPrefix(p, "~"))
	}
	return p
}

func (c *Config) apply(kv map[string]string) error {
	for k, v := range kv {
		var err error
		switch k {
		case "provider":
			c.Provider = v
		case "aws_region":
			c.AWSRegion = v
		case "aws_az":
			c.AWSAZ = v
		case "aws_profile":
			c.AWSProfile = v
		case "aws_spot":
			switch v {
			case "true":
				c.AWSSpot = true
			case "false":
				c.AWSSpot = false
			default:
				err = fmt.Errorf("aws_spot must be true or false, not %q", v)
			}
		case "image_id":
			c.ImageID = v
		case "vpc_range":
			c.VPCRange = v
		case "project":
			c.Project = v
		case "zone":
			c.Zone = v
		case "machine_type":
			c.MachineType, c.machineSet = v, true
		case "boot_disk_gb":
			c.BootDiskGB, err = strconv.Atoi(v)
		case "data_disk_gb":
			c.DataDiskGB, err = strconv.Atoi(v)
		case "disk_type":
			c.DiskType, c.diskSet = v, true
		case "image_project":
			c.ImageProject = v
		case "image_family":
			c.ImageFamily = v
		case "network":
			c.Network = v
		case "subnet_range":
			c.SubnetRange = v
		case "ssh_source_cidr":
			c.SSHSourceCIDR = v
		case "ssh_user":
			c.SSHUser = v
		case "idle_minutes":
			c.IdleMinutes, err = strconv.Atoi(v)
		case "max_run_duration":
			c.MaxRunDuration = v
		case "tag":
			c.Tag = v
		case "ts_oauth_file":
			c.TSOAuthFile = expand(v)
		case "ts_authkey_file":
			c.TSAuthKeyFile = expand(v)
		case "ts_api_base":
			c.TSAPIBase = v
		case "ts_allow_login":
			c.TSAllowLogin = v
		case "hub_url":
			c.HubURL = strings.TrimRight(v, "/")
		case "hub_peer_url":
			c.HubPeerURL = strings.TrimRight(v, "/")
		case "hub_version":
			c.HubVersion = v
		case "releases_url":
			c.ReleasesURL = strings.TrimRight(v, "/")
		case "herdr_install_url":
			c.HerdrInstall = v
		case "herdr_install_sha256":
			c.HerdrSHA256 = strings.ToLower(v)
		case "agents":
			c.Agents = splitList(v)
		case "pi_version":
			c.PiVersion = v
		case "llm_gateway_url":
			c.LLMGatewayURL = strings.TrimRight(v, "/")
		case "llm_provisioner_key_file":
			c.LLMProvisionerKeyFile = expand(v)
		case "llm_models":
			c.LLMModels = splitList(v)
		case "llm_max_budget":
			c.LLMMaxBudget, err = strconv.ParseFloat(v, 64)
		case "llm_budget_duration":
			c.LLMBudgetDuration = v
		case "llm_key_duration":
			c.LLMKeyDuration = v
		case "llm_rpm":
			c.LLMRPM, err = strconv.Atoi(v)
		case "state_dir":
			c.StateDir = expand(v)
		default:
			return fmt.Errorf("unknown setting %q", k)
		}
		if err != nil {
			return fmt.Errorf("%s: %v", k, err)
		}
	}
	return nil
}

func splitList(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// validateAgents and validateLLM are split out of Validate.
func (c *Config) validateAgents() error {
	if len(c.Agents) == 0 {
		return fmt.Errorf("agents is empty (pi, claude, codex)")
	}
	for _, a := range c.Agents {
		if a != "pi" && a != "claude" && a != "codex" {
			return fmt.Errorf("agents: %q is not pi, claude or codex", a)
		}
	}
	if !piVerRe.MatchString(c.PiVersion) {
		return fmt.Errorf("pi_version %q is not a version like 0.85.1 (it is pinned)", c.PiVersion)
	}
	return nil
}

func (c *Config) hasAgent(a string) bool {
	for _, x := range c.Agents {
		if x == a {
			return true
		}
	}
	return false
}

func (c *Config) validateLLM() error {
	if c.LLMGatewayURL == "" {
		return nil
	}
	if _, err := hubURL(c.LLMGatewayURL, true); err != nil {
		return fmt.Errorf("llm_gateway_url: %v", err)
	}
	if !c.hasAgent("pi") {
		return fmt.Errorf("llm_gateway_url needs pi in agents: the key and models.json are pi's")
	}
	if c.LLMProvisionerKeyFile == "" {
		return fmt.Errorf("llm_provisioner_key_file is empty")
	}
	if len(c.LLMModels) == 0 || len(c.LLMModels) > 20 {
		return fmt.Errorf("llm_models: want 1 to 20 model names")
	}
	for _, m := range c.LLMModels {
		if !modelRe.MatchString(m) {
			return fmt.Errorf("llm_models: bad model name %q", m)
		}
	}
	if !(c.LLMMaxBudget > 0 && c.LLMMaxBudget <= 10000) {
		return fmt.Errorf("llm_max_budget must be above 0 and at most 10000 (USD)")
	}
	if !llmDurRe.MatchString(c.LLMBudgetDuration) {
		return fmt.Errorf("llm_budget_duration %q: want something like 30d, 12h or 1mo", c.LLMBudgetDuration)
	}
	if c.LLMKeyDuration != "" && !llmDurRe.MatchString(c.LLMKeyDuration) {
		return fmt.Errorf("llm_key_duration %q: want something like 30d, or empty for no expiry", c.LLMKeyDuration)
	}
	if c.LLMRPM < 1 || c.LLMRPM > 100000 {
		return fmt.Errorf("llm_rpm must be 1..100000")
	}
	return nil
}

// Validate refuses anything that would be spliced into a command line,
// a shell script or a firewall rule unchecked.
func (c *Config) Validate() error {
	chk := func(ok bool, format string, a ...any) error {
		if ok {
			return nil
		}
		return fmt.Errorf(format, a...)
	}
	if err := c.validateProvider(chk); err != nil {
		return err
	}
	if err := c.validateAgents(); err != nil {
		return err
	}
	if err := c.validateLLM(); err != nil {
		return err
	}
	for _, e := range []error{
		chk(c.BootDiskGB >= 10 && c.BootDiskGB <= 2000, "boot_disk_gb out of range"),
		chk(c.DataDiskGB >= 10 && c.DataDiskGB <= 10000, "data_disk_gb out of range"),
		chk(resRe.MatchString(c.Network), "bad network name %q", c.Network),
		chk(userRe.MatchString(c.SSHUser) && c.SSHUser != "root", "bad ssh_user %q", c.SSHUser),
		chk(!c.AWSSpot || c.Provider == "aws", "aws_spot needs provider=aws"),
		chk(c.IdleMinutes >= 5 && c.IdleMinutes <= 10080, "idle_minutes must be 5..10080"),
		chk(c.MaxRunDuration == "" || durRe.MatchString(c.MaxRunDuration), "bad max_run_duration %q (want e.g. 24h, or empty)", c.MaxRunDuration),
		chk(tagRe.MatchString(c.Tag), "bad tag %q (want tag:name)", c.Tag),
		chk(c.TSAllowLogin == "" || loginRe.MatchString(c.TSAllowLogin), "bad ts_allow_login %q", c.TSAllowLogin),
		chk(c.HerdrSHA256 == "" || regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(c.HerdrSHA256), "herdr_install_sha256 must be 64 hex digits"),
		chk(c.HubVersion == "" || versionRe.MatchString(c.HubVersion), "hub_version %q is not a release tag like v0.3.0", c.HubVersion),
	} {
		if e != nil {
			return e
		}
	}
	if _, n, err := net.ParseCIDR(c.SubnetRange); err != nil || n.String() != c.SubnetRange {
		return fmt.Errorf("subnet_range %q is not a CIDR network", c.SubnetRange)
	}
	if c.Provider == "aws" {
		_, v, err := net.ParseCIDR(c.VPCRange)
		_, sn, _ := net.ParseCIDR(c.SubnetRange)
		if err != nil || v.String() != c.VPCRange || !v.Contains(sn.IP) {
			return fmt.Errorf("vpc_range %q is not a CIDR network containing subnet_range %q", c.VPCRange, c.SubnetRange)
		}
	}
	if err := validateSSHSource(c.SSHSourceCIDR); err != nil {
		return err
	}
	if c.TSAllowLogin == "" {
		return fmt.Errorf("ts_allow_login is required: the Tailscale login that owns the hub device (the worker admits the hub's requests as that login)")
	}
	if _, err := hubURL(c.HubURL, true); err != nil {
		return fmt.Errorf("hub_url: %v", err)
	}
	if err := validatePeerURL(c.HubPeerURL); err != nil {
		return fmt.Errorf("hub_peer_url: %v", err)
	}
	return nil
}

// validateSSHSource wants exactly one IPv4 /32: the workstation. Anything
// wider (or 0.0.0.0/0, or a placeholder) is refused.
func validateSSHSource(s string) error {
	if s == "" || strings.Contains(strings.ToUpper(s), "CHANGE_ME") {
		return fmt.Errorf("ssh_source_cidr is required: your workstation's public IPv4 as a /32")
	}
	ip, n, err := net.ParseCIDR(s)
	if err != nil || ip.To4() == nil {
		return fmt.Errorf("ssh_source_cidr %q is not an IPv4 CIDR", s)
	}
	if ones, _ := n.Mask.Size(); ones != 32 {
		return fmt.Errorf("ssh_source_cidr %q must be a single address (/32)", s)
	}
	return nil
}

// hubURL checks a hub address: https (the Serve URL) or, for the peer
// listener, plain http, always on a ts.net name.
func hubURL(s string, serve bool) (*url.URL, error) {
	if s == "" {
		return nil, fmt.Errorf("required")
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || u.Path != "" && u.Path != "/" || u.RawQuery != "" || u.User != nil {
		return nil, fmt.Errorf("%q is not a bare URL like https://hub.tailnet.ts.net", s)
	}
	want := "http"
	if serve {
		want = "https"
	}
	if u.Scheme != want {
		return nil, fmt.Errorf("%q must be %s://", s, want)
	}
	h := u.Hostname()
	if !strings.HasSuffix(h, ".ts.net") {
		return nil, fmt.Errorf("%q is not a *.ts.net name", s)
	}
	if !serve && u.Port() == "" {
		return nil, fmt.Errorf("%q needs the hub's peer listen port (COMEBYE_PEER_LISTEN)", s)
	}
	return u, nil
}

// validatePeerURL checks COMEBYE_HUB for the worker: either the hub's Tailscale
// Serve URL (https://<hub>.<tailnet>.ts.net, port 443; the worker is admitted
// by its tag's app capability) or, as the legacy path, the hub's peer listener
// (http://<hub>.<tailnet>.ts.net:<port>).
func validatePeerURL(s string) error {
	if u, err := url.Parse(s); err == nil && u.Scheme == "https" {
		if _, err := hubURL(s, true); err != nil {
			return err
		}
		if p := u.Port(); p != "" && p != "443" {
			return fmt.Errorf("%q: the Serve URL is on port 443; a hub peer listener port uses http://", s)
		}
		return nil
	}
	_, err := hubURL(s, false)
	return err
}

// WorkerDir is where one worker's ssh config and known_hosts live.
func (c *Config) WorkerDir(name string) string { return filepath.Join(c.StateDir, name) }

// KeyPath is the one ssh key the provisioner uses for all its workers.
func (c *Config) KeyPath() string { return filepath.Join(c.StateDir, "id_ed25519") }

// Hostnames used in GCE.
func dataDiskName(name string) string { return name + "-data" }

const (
	labelKey = "managed-by"
	labelVal = "comebye-worker"
)

var (
	regionRe   = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-[0-9]$`)
	profileRe  = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)
	amiRe      = regexp.MustCompile(`^ami-[0-9a-f]{8,17}$`)
	gravitonRe = regexp.MustCompile(`^[a-z]+[0-9]+g[a-z]*\.`)
	awsTypeRe  = regexp.MustCompile(`^[a-z][a-z0-9]*\.[a-z0-9]+$`)
)

// validateProvider checks the cloud-specific settings.
func (c *Config) validateProvider(chk func(bool, string, ...any) error) error {
	switch c.Provider {
	case "gcp":
		for _, e := range []error{
			chk(projectRe.MatchString(c.Project), "project %q is not a GCP project id", c.Project),
			chk(zoneRe.MatchString(c.Zone), "zone %q is not a zone like us-central1-a", c.Zone),
			chk(regexp.MustCompile(`^[a-z0-9-]+$`).MatchString(c.MachineType), "bad machine_type %q", c.MachineType),
			chk(regexp.MustCompile(`^pd-[a-z]+$`).MatchString(c.DiskType), "bad disk_type %q", c.DiskType),
			chk(regexp.MustCompile(`^[a-z0-9-]+$`).MatchString(c.ImageProject), "bad image_project %q", c.ImageProject),
			chk(regexp.MustCompile(`^[a-z0-9-]+$`).MatchString(c.ImageFamily), "bad image_family %q", c.ImageFamily),
		} {
			if e != nil {
				return e
			}
		}
	case "aws":
		for _, e := range []error{
			chk(regionRe.MatchString(c.AWSRegion), "aws_region %q is not a region like us-east-1", c.AWSRegion),
			chk(strings.HasPrefix(c.AWSAZ, c.AWSRegion) && len(c.AWSAZ) == len(c.AWSRegion)+1 && c.AWSAZ[len(c.AWSAZ)-1] >= 'a' && c.AWSAZ[len(c.AWSAZ)-1] <= 'z',
				"aws_az %q is not a zone of %s (like %sa)", c.AWSAZ, c.AWSRegion, c.AWSRegion),
			chk(c.AWSProfile == "" || profileRe.MatchString(c.AWSProfile), "bad aws_profile %q", c.AWSProfile),
			chk(c.ImageID == "" || amiRe.MatchString(c.ImageID), "bad image_id %q", c.ImageID),
			chk(awsTypeRe.MatchString(c.MachineType), "bad machine_type %q for aws (like t3.xlarge)", c.MachineType),
			chk(!gravitonRe.MatchString(c.MachineType), "machine_type %q is Arm (Graviton): the bootstrap is tested on amd64 only", c.MachineType),
			chk(c.DiskType == "gp3" || c.DiskType == "gp2", "bad disk_type %q for aws (gp3 or gp2)", c.DiskType),
		} {
			if e != nil {
				return e
			}
		}
	default:
		return fmt.Errorf("provider %q: want gcp or aws", c.Provider)
	}
	return nil
}
