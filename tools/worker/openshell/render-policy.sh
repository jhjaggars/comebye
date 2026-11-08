#!/bin/sh
# render-policy.sh [--llm-host HOST] [TEMPLATE] > policy.yaml
llm=""; tmpl="$(dirname "$0")/policy.yaml.tmpl"
while [ $# -gt 0 ]; do case $1 in --llm-host) llm=$2; shift 2 ;; *) tmpl=$1; shift ;; esac; done
case $llm in *[!A-Za-z0-9.-]*) echo "render-policy: --llm-host must be a bare hostname" >&2; exit 2 ;; esac
if [ -n "$llm" ]; then
  sed -e '/@@BEGIN llm/d' -e '/@@END llm/d' -e "s/@@LLM_GATEWAY_HOST@@/$llm/" "$tmpl"
else
  sed -e '/@@BEGIN llm/,/@@END llm/d' "$tmpl"
fi
