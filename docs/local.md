# Local test

Runs ExternalDNS with a locally built webhook in a k3d cluster, against a real
router. It writes `a.foobar.io` and `cname.foobar.io` to the router's `dhcp`
config, so point it at a router you can test on.

### Requirements

- docker
- k3d
- skaffold
- ko
- kubectl
- an OpenWrt router with LuCI, reachable from the cluster

### Point it at the router

- `example/values.yaml`: set `PROVIDER_OPENWRT_LUCIRPC_HOSTNAME` to the
  router. uhttpd's own certificate fails the default TLS check, so either set
  `PROVIDER_OPENWRT_LUCIRPC_SSL` to `"false"` and the port to `"80"`, or set
  `PROVIDER_OPENWRT_LUCIRPC_INSECURE_SKIP_VERIFY` to `"true"`.
- `example/services.yaml`: set the password in the `openwrt-luci` Secret.

### Run

```
k3d cluster create --config k3d.yaml
skaffold dev
```

skaffold builds the webhook with ko, imports the image into the k3d cluster
(no registry needed), installs the ExternalDNS chart with
`example/values.yaml`, and rebuilds on every change.

### Check

```
kubectl logs deploy/external-dns -c webhook
nslookup cname.foobar.io <router>
```

`Ctrl+C` in `skaffold dev` removes what it deployed. The records it wrote stay
on the router until you delete them from `/etc/config/dhcp`. Then:

```
k3d cluster delete external-dns-openwrt-next
```
