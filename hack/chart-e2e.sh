#!/usr/bin/env bash
# Installs the chart into a throwaway k3s in docker, against an OpenWrt router
# in docker, and checks that Ingresses end up on the router and in
# `kubectl get owr`. Needs docker, go and helm.
set -euo pipefail

openwrt_image=${OPENWRT_IMAGE:-openwrt/rootfs:x86-64-25.12.5}
k3s_image=${K3S_IMAGE:-rancher/k3s:v1.33.5-k3s1}
# Not `default`, and a release name other than the chart's: what an install
# next to other workloads looks like.
namespace=external-dns-openwrt
release=homelab
webhook_image=external-dns-openwrt-next:e2e

containers=()
work=$(mktemp -d)
cleanup() {
	if ((${#containers[@]})); then
		docker rm -f "${containers[@]}" >/dev/null
	fi
	docker rmi -f "$webhook_image" >/dev/null 2>&1 || true
	rm -rf "$work"
}
trap cleanup EXIT

cd "$(dirname "$0")/.."
# shellcheck source=hack/openwrt.sh
source hack/openwrt.sh

k3s=external-dns-chart-e2e-$$-k3s
router=external-dns-chart-e2e-$$-router

kubectl() { docker exec -i "$k3s" kubectl "$@"; }
owr_state() { kubectl -n "$namespace" get owr "$1" -o jsonpath='{.status.state}' 2>/dev/null; }
wait_state() {
	for _ in $(seq 120); do
		[[ $(owr_state "$1") == "$2" ]] && return 0
		sleep 2
	done
	echo "timed out waiting for $1 to be $2" >&2
	kubectl -n "$namespace" get owr >&2 || true
	kubectl -n "$namespace" logs "deploy/$release-external-dns-openwrt" --all-containers --tail=50 >&2 || true
	return 1
}

echo "=== router ($openwrt_image)"
start_router "$router" "$openwrt_image"
router_ip=$ip

echo "=== k3s ($k3s_image)"
docker run -d --name "$k3s" --privileged "$k3s_image" server --disable=traefik,metrics-server >/dev/null
containers+=("$k3s")
retry docker exec "$k3s" kubectl get --raw /readyz
k3s_ip=$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$k3s")
docker exec "$k3s" cat /etc/rancher/k3s/k3s.yaml | sed "s/127.0.0.1/$k3s_ip/" >"$work/kubeconfig"
export KUBECONFIG=$work/kubeconfig

echo "=== webhook image"
CGO_ENABLED=0 go build -trimpath -o "$work/webhook" ./cmd/webhook
printf 'FROM cgr.dev/chainguard/static\nCOPY webhook /webhook\nENTRYPOINT ["/webhook"]\n' >"$work/Dockerfile"
docker build -q -t "$webhook_image" "$work" >/dev/null
docker save "$webhook_image" | docker exec -i "$k3s" ctr images import - >/dev/null

echo "=== install"
kubectl create namespace "$namespace" >/dev/null
kubectl -n "$namespace" create secret generic openwrt-router --from-literal=pass="$password" >/dev/null
helm_args=(
	-n "$namespace"
	--set ownershipID=e2e
	--set router.hostname="$router_ip"
	--set router.port=80
	--set router.tls=false
	--set router.existingSecret=openwrt-router
	--set router.existingSecretPasswordKey=pass
	--set 'externalDNS.domainFilters={e2e.lan}'
	--set externalDNS.interval=5s
	--set webhook.image.repository=docker.io/library/external-dns-openwrt-next
	--set webhook.image.tag=e2e
	--set webhook.image.pullPolicy=Never
	--wait --timeout 3m
)
helm install "$release" charts/external-dns-openwrt "${helm_args[@]}"

kubectl -n "$namespace" apply -f - >/dev/null <<EOF
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: nas
  annotations:
    external-dns.kubernetes.io/target: 10.0.0.5
spec:
  rules:
    - host: nas.e2e.lan
---
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: git
  annotations:
    external-dns.kubernetes.io/target: nas.e2e.lan
spec:
  rules:
    - host: git.e2e.lan
---
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: wildcard
  annotations:
    external-dns.kubernetes.io/target: 10.0.0.9
spec:
  rules:
    - host: "*.e2e.lan"
EOF

wait_state a.nas.e2e.lan Ready
wait_state cname.git.e2e.lan Ready
for _ in $(seq 30); do
	[[ $(kubectl -n "$namespace" get owr -o jsonpath='{.items[?(@.spec.dnsName=="*.e2e.lan")].status.state}') == Rejected ]] && break
	sleep 2
done
kubectl -n "$namespace" get owr
docker exec "$router" uci show dhcp | grep -q "name='nas.e2e.lan'"
docker exec "$router" uci show dhcp | grep -q "cname='git.e2e.lan'"

echo "=== upgrade"
helm upgrade "$release" charts/external-dns-openwrt "${helm_args[@]}" --set webhook.logLevel=debug
wait_state a.nas.e2e.lan Ready

echo "=== uninstall"
helm uninstall "$release" -n "$namespace" --wait >/dev/null
# helm leaves the CRDs, and with them the status objects, and the records on
# the router.
kubectl get crd openwrtrecords.external-dns-openwrt.vizzletf.github.io >/dev/null
kubectl -n "$namespace" get owr a.nas.e2e.lan >/dev/null
docker exec "$router" uci show dhcp | grep -q "name='nas.e2e.lan'"
echo "ok"
