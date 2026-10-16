# external-dns-openwrt

ExternalDNS with the [OpenWrt webhook provider](https://github.com/VizzleTF/external-dns-openwrt-next),
publishing A and CNAME records from Ingresses, Services and Gateway API routes
to dnsmasq on an OpenWrt router.

```yaml
ownershipID: my-cluster
router:
  hostname: 192.168.1.1
  existingSecret: openwrt-router   # key `password`
externalDNS:
  domainFilters: [lan]
```

```sh
helm install external-dns-openwrt oci://ghcr.io/vizzletf/charts/external-dns-openwrt \
  -n external-dns-openwrt --create-namespace -f values.yaml
kubectl -n external-dns-openwrt get owr
```

See the project's [README](https://github.com/VizzleTF/external-dns-openwrt-next#installation)
for the router login, the values and what each one does, and
[values.yaml](values.yaml) for every value.
