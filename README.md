# ExternalDNS Webhook Provider for OpenWRT

[ExternalDNS](https://github.com/kubernetes-sigs/external-dns) is a Kubernetes add-on for automatically managing DNS records for Kubernetes ingresses and services by using different DNS providers. This webhook provider allows you to automate DNS records from your Kubernetes clusters into your OpenWRT router. If you like home automation like me, it should help you.

For examples of creating DNS records either via CRDs or via Ingress/Service annotations, check out the [example directory](./example).

> **Fork.** This is a fork of [renanqts/external-dns-openwrt-webhook](https://github.com/renanqts/external-dns-openwrt-webhook),
> whose last code change was in February 2025. It adds full create/update/delete
> support, multi-target endpoints and an explicit dnsmasq reload. See
> [CHANGELOG.md](./CHANGELOG.md) for the details.

## Limitations
- Supported DNS record types: `A`, `CNAME`.
- Per-record TTLs are not supported. UCI `domain`/`cname` sections carry no TTL —
  dnsmasq answers them with its global `local_ttl` — so `Records()` reports a
  fixed 300. Endpoints that request an explicit TTL will be re-planned on every
  run.

## What this fork changes

`policy: sync` now works: records are created, updated **and** deleted.

- **Deletions apply.** Upstream matched records by name only and mutated the
  slice it was iterating over, so with more than one record in a change set some
  were skipped and the run ended in `records not found`.
- **Updates no longer resurrect the old value.** Upstream wrote `UpdateOld` back
  to the router before writing `UpdateNew`.
- **Multi-target endpoints work.** Upstream read `Targets[0]` and dropped the
  rest. Records are now matched on their full identity (name *and* value), so
  removing one target of a name leaves the others alone.
- **Idempotent.** Deleting an absent record or adding an existing one is a
  no-op instead of an error, so one stale entry can no longer wedge every
  subsequent change.
- **One commit per change set**, and nothing is committed when nothing changed.
- **dnsmasq is reloaded.** `uci commit` only writes `/etc/config/dhcp`; it does
  not regenerate `/var/etc/dnsmasq.conf.*` nor signal the daemon, so upstream's
  records stayed invisible until something else restarted the service.

## OpenWRT Prerequisites
You must install the following packages in OpenWRT for the webhook to function:
- luci-mod-rpc
- luci-lib-ipkg
- luci-compat

```bash
# OpenWrt <= 24
opkg update && opkg install luci-mod-rpc luci-lib-ipkg luci-compat

# OpenWrt >= 25 (apk)
apk update && apk add luci-mod-rpc luci-lib-ipkg luci-compat
```

## Reload strategy

Set with `PROVIDER_OPENWRT_RELOADSTRATEGY`:

| Value | Behaviour |
| ----- | --------- |
| `dnsmasq` (default) | Runs `/etc/init.d/dnsmasq reload` over the `rpc/sys` endpoint. Narrowest option — no other service and no other UCI config is touched. |
| `uci-apply` | Calls `uci apply` with no arguments. Commits and applies **every** pending UCI config, not just `dhcp`, so anything an admin left staged is applied too. Use when the RPC user cannot reach `rpc/sys`. |
| `none` | Upstream behaviour: commit only. Records land in `/etc/config/dhcp` but dnsmasq keeps serving the previous set. |

> **Never call LuCI's `uci apply` with a config name.** Its JSON-RPC binding is
> `function apply(config) return uci:apply(config) end`, but the underlying
> signature is `apply(self, rollback)` — any non-empty argument is read as
> `rollback = true`, which starts a ubus apply with a ≥90 s rollback timer that
> **reverts the change** unless it is confirmed out of band. This fork always
> calls it with an empty argument list.

## Configuration Options
You can find all the environment variables allowed as well as the default in the [values file](example/values.yaml#L19).   
The installation can be achieved via [helm chart](skaffold.yaml#L15-L26).

[!["Buy Me A Coffee"](https://www.buymeacoffee.com/assets/img/custom_images/orange_img.png)](https://www.buymeacoffee.com/renanqts4)
