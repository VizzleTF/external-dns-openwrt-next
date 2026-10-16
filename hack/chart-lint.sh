#!/usr/bin/env bash
# Lints and renders the chart with every values file in its ci/ directory, and
# checks that the values it must refuse are refused. Needs helm.
set -euo pipefail

chart=$(dirname "$0")/../charts/external-dns-openwrt

for values in "$chart"/ci/*.yaml; do
	echo "=== $(basename "$values")"
	helm lint --strict "$chart" -f "$values"
	helm template ci "$chart" -n ci -f "$values" >/dev/null
done

# refused expects rendering with the given flags to fail with message.
refused() {
	local message=$1 out
	shift
	if out=$(helm template ci "$chart" -n ci "$@" 2>&1); then
		echo "rendered, expected a refusal: $message" >&2
		return 1
	fi
	if ! grep -qF -- "$message" <<<"$out"; then
		echo "refused for another reason than \"$message\":" >&2
		echo "$out" >&2
		return 1
	fi
}

minimal=$chart/ci/minimal-values.yaml
echo "=== refusals"
refused "ownershipID is required" -f "$minimal" --set ownershipID=null
refused "router.hostname is required" -f "$minimal" --set router.hostname=null
refused "set router.password or router.existingSecret" -f "$minimal" --set router.password=null
refused "externalDNS.domainFilters is required" -f "$minimal" --set 'externalDNS.domainFilters=null'
refused "with policy sync" -f "$minimal" --set ownershipID=""
refused "set configMapName or secretName, not both" -f "$minimal" \
	--set router.caBundle.configMapName=a --set router.caBundle.secretName=b
refused "serviceMonitor.enabled needs service.enabled" -f "$minimal" \
	--set serviceMonitor.enabled=true --set service.enabled=false
refused "at '/externalDNS/policy'" -f "$minimal" --set externalDNS.policy=create-only
refused "at '/externalDNS/sources/0'" -f "$minimal" --set 'externalDNS.sources={istio-gateway}'
echo "ok"
