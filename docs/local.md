# Local test

Runs ExternalDNS with a locally built webhook in a k3d cluster, against a real
router. It writes `a.foobar.io` and `cname.foobar.io` to the router's `dhcp`
config, so point it at a router you can test on.

### Requirements

- docker
- k3d
- skaffold
- kubectl
- an OpenWrt router with LuCI, reachable from the cluster

### Point it at the router

- `skaffold.yaml`: set `router.hostname` to the router. uhttpd's own
  certificate fails the default TLS check, so either add `router.tls: "false"`
  and `router.port: "80"`, or `router.insecureSkipVerify: "true"`.
- `example/services.yaml`: set the password in the `openwrt-luci` Secret.

### Run

```
k3d cluster create --config k3d.yaml
skaffold dev
```

skaffold builds the webhook with ko, imports the image into the k3d cluster
(no registry needed), installs the chart from `charts/external-dns-openwrt`
with it, and rebuilds on every change.

### Check

```
kubectl logs deploy/external-dns-openwrt -c webhook
kubectl get owr
nslookup cname.foobar.io <router>
```

`kubectl get owr` lists the record status the webhook mirrors.

Without a router or k3d, `./hack/chart-e2e.sh` installs the chart into k3s
against an OpenWrt router, both in docker, the way CI does.

`Ctrl+C` in `skaffold dev` removes what it deployed. The records it wrote stay
on the router until you delete them from `/etc/config/dhcp`. Then:

```
k3d cluster delete external-dns-openwrt-next
```
