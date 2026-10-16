#!/usr/bin/env bash
# Runs the e2e tests against each OpenWrt image in OPENWRT_IMAGES, started in
# docker as a router serving ubus over http.
set -euo pipefail

images=${OPENWRT_IMAGES:-"openwrt/rootfs:x86-64-24.10.8 openwrt/rootfs:x86-64-25.12.5"}

containers=()
cleanup() {
	if ((${#containers[@]})); then
		docker rm -f "${containers[@]}" >/dev/null
	fi
}
trap cleanup EXIT

cd "$(dirname "$0")/.."
# shellcheck source=hack/openwrt.sh
source hack/openwrt.sh

for image in $images; do
	name=external-dns-e2e-$$-${#containers[@]}
	echo "=== $image ($name)"
	start_router "$name" "$image"

	# One package at a time: two sessions adding a section to dhcp at once are
	# handed the same anonymous name, and whichever applies last overwrites the
	# other's section.
	OPENWRT_E2E_HOST=$ip \
		OPENWRT_E2E_ROOT_PASSWORD=$root_password \
		OPENWRT_E2E_USERNAME=$username \
		OPENWRT_E2E_PASSWORD=$password \
		go test -tags e2e -count=1 -p 1 ./...
done
