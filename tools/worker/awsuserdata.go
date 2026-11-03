package main

import (
	_ "embed"
	"encoding/base64"
	"fmt"
	"strings"
)

//go:embed scripts/aws-boot.sh
var awsBootScript string

const bootUnit = `[Unit]
Description=Mount the data volume and publish the ssh host key to the console
After=ssh.service

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/local/sbin/comebye-boot

[Install]
WantedBy=multi-user.target
`

// awsUserData is the cloud-config for the instance: the worker user with our
// PUBLIC ssh key, the boot script and its unit. No secret goes in it: it is
// readable through IMDS and DescribeInstanceAttribute.
func awsUserData(c *Config, pubKey, volumeID string) (string, error) {
	f := strings.Fields(pubKey)
	if len(f) < 2 || f[0] != "ssh-ed25519" || !hostKeyRe.MatchString(f[0]+" "+f[1]) {
		return "", fmt.Errorf("not an ed25519 public key")
	}
	if !strings.HasPrefix(volumeID, "vol-") || !shellSafe.MatchString(volumeID) {
		return "", fmt.Errorf("bad volume id %q", volumeID)
	}
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	var b strings.Builder
	fmt.Fprintf(&b, `#cloud-config
users:
  - name: %s
    shell: /bin/bash
    sudo: ALL=(ALL) NOPASSWD:ALL
    lock_passwd: true
    ssh_authorized_keys:
      - %s %s
write_files:
  - path: /etc/comebye-worker/volume-id
    encoding: b64
    permissions: "0644"
    content: %s
  - path: /usr/local/sbin/comebye-boot
    encoding: b64
    permissions: "0755"
    content: %s
  - path: /etc/systemd/system/comebye-boot.service
    encoding: b64
    permissions: "0644"
    content: %s
runcmd:
  - [systemctl, daemon-reload]
  - [systemctl, enable, comebye-boot.service]
  - [systemctl, start, --no-block, comebye-boot.service]
`, c.SSHUser, f[0], f[1], b64(volumeID+"\n"), b64(awsBootScript), b64(bootUnit))
	return b.String(), nil
}
