#!/usr/bin/env bash
# The cluster nodes' temperatures, from Prometheus, in one query: for the test pool's heat check
# (tools/ci/prefer-vm.sh) and remote.sh's temperature log.
#
#   tools/ci/temps.sh PROMETHEUS_URL [NODE]
#
# Prints one line per node, `NODE cpu=C nvme=N` (whole degrees C; a reading Prometheus does not have is empty), or
# with NODE only that node's line. Gives up after 2 s (TEMPS_TIMEOUT) and prints nothing; it always exits 0, so a
# caller never fails or waits on it.
#
# Where the numbers come from: both exporters run as DaemonSet pods in the monitoring namespace, and every series
# they make has a `pod` label; kube-state-metrics' kube_pod_info{namespace="monitoring"} maps that pod to its `node`.
# (The smartctl exporter's instance is its pod IP, kube_pod_info's pod_ip; node-exporter runs on the host network,
# so its instance is the node IP, kube_pod_info's host_ip. Joining on the pod covers both without parsing an IP.)
#   nvme: the hottest NVMe drive on the node, smartctl_device_temperature{temperature_type="current",device=~"nvme.*"}
#   cpu:  the hottest CPU package sensor, node_hwmon_temp_celsius of the chips named coretemp (Intel) or k10temp (AMD)
# The node names are the cluster's, as `worker status --json` gives a VM's `node`.
set -uo pipefail
url="${1:-}"; want="${2:-}"
[ -n "$url" ] || exit 0
pods='max by (pod, node) (kube_pod_info{namespace="monitoring"})'
q='label_replace(max by (node) (smartctl_device_temperature{temperature_type="current",device=~"nvme.*"} * on (pod) group_left (node) '"$pods"'), "t", "nvme", "", "")'
q="$q"' or label_replace(max by (node) (node_hwmon_temp_celsius * on (job, instance, chip) group_left (chip_name) node_hwmon_chip_names{chip_name=~"coretemp|k10temp"} * on (pod) group_left (node) '"$pods"'), "t", "cpu", "", "")'
t="${TEMPS_TIMEOUT:-2}"
j="$(timeout "$t" curl -sS -m "$t" -G "${url%/}/api/v1/query" --data-urlencode "query=$q" 2>/dev/null)" || exit 0
# Each result is {"metric":{"node":"N","t":"cpu|nvme"},"value":[TIME,"V"]}: one per line, then node, kind and value.
grep -o '{"metric":{[^}]*},"value":\[[^]]*\]}' <<<"$j" | sed -n 's/.*"node":"\([^"]*\)".*"t":"\([a-z]*\)".*"value":\[[^,]*,"\([0-9.]*\)"\].*/\1 \2 \3/p' |
  awk -v want="$want" '
    { v = int($3 + 0.5); if (!($1 in seen)) { seen[$1] = 1; order[++n] = $1 }; r[$1, $2] = v }
    END { for (i = 1; i <= n; i++) { k = order[i]; if (want == "" || want == k) printf "%s cpu=%s nvme=%s\n", k, r[k, "cpu"], r[k, "nvme"] } }'
exit 0
