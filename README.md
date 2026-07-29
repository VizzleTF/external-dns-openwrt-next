# external-dns-openwrt-next

An [ExternalDNS](https://github.com/kubernetes-sigs/external-dns) webhook provider that manages DNS records on an OpenWrt router, so hostnames from Kubernetes Ingresses, Services and Gateway API routes resolve on the LAN.

For examples of creating DNS records via CRDs or via Ingress/Service annotations, see the [example directory](./example).

> **Fork.** Continuation of [renanqts/external-dns-openwrt-webhook](https://github.com/renanqts/external-dns-openwrt-webhook),
> whose last code change was in February 2025. It adds working create/update/delete,
> record ownership, multi-target endpoints and an explicit dnsmasq reload.
> See [CHANGELOG.md](./CHANGELOG.md).
>
> Images: `ghcr.io/vizzletf/external-dns-openwrt-next` (linux/amd64, linux/arm64).

## Limitations
- Supported DNS record types: `A`, `CNAME`.
- Per-record TTLs are not supported. UCI `domain`/`cname` sections carry no TTL —
  dnsmasq answers them with its global `local_ttl` — so `Records()` reports a
  fixed 300. Endpoints that request an explicit TTL will be re-planned on every
  run.

## What this fork changes

`policy: sync` works: records are created, updated **and** deleted — and with
ownership enabled it can only ever touch records it created itself.

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

## Ownership — required before `policy: sync`

A router usually holds DNS entries nobody handed to ExternalDNS: the NAS, the
hypervisors, whatever was typed into LuCI once. This provider has no TXT
registry to fall back on — OpenWrt's UCI has no TXT support at all — so
`policy: sync` with everything visible would delete exactly those entries.

Ownership solves it at the source. Every record this provider writes gets an
extra UCI option, and it reports only the sections carrying it:

```
uci add dhcp domain            -> cfg0abc12
uci set dhcp.cfg0abc12.name=grafana.example.com
uci set dhcp.cfg0abc12.ip=10.0.0.10
uci set dhcp.cfg0abc12.external_dns=my-cluster    # ownership marker
```

Anything without the marker is invisible to ExternalDNS: it cannot be updated,
and it cannot be deleted. The marker is inert — `dhcp_domain_add` reads only
`name`/`ip` and `dhcp_cname_add` only `cname`/`target`, so it never reaches the
generated dnsmasq config.

| Variable | Default | Meaning |
| -------- | ------- | ------- |
| `PROVIDER_OPENWRT_OWNERSHIPID` | *(empty)* | Marker value. **Empty disables ownership and every section is reported — do not combine that with `policy: sync`.** Give each ExternalDNS instance writing to the same router a distinct ID. |
| `PROVIDER_OPENWRT_OWNERSHIPOPTION` | `external_dns` | UCI option holding the ID. |
| `PROVIDER_OPENWRT_ADOPTEXISTING` | `true` | Take over an unmarked section that already matches the record exactly, instead of creating a duplicate. |

### Migrating an existing deployment

Turn ownership on while still running `policy: upsert-only`. On the first
reconcile the provider sees none of its records (they carry no marker), so
ExternalDNS asks it to create them all — and adoption stamps the marker onto
the sections already present rather than adding a second copy of each. Records
nobody asked for are never adopted, because ExternalDNS never asks for them.

Once the marker is on the right records, and only then, switch to
`policy: sync`.

A section already owned by a *different* ID is never adopted; it belongs to
another instance.

## Reload strategy

Set with `PROVIDER_OPENWRT_RELOADSTRATEGY`:

| Value | Behaviour |
| ----- | --------- |
| `restart` (default) | Runs `/etc/init.d/dnsmasq restart` over the `rpc/sys` endpoint. The only strategy that applies **both** record types. |
| `reload` | Runs `/etc/init.d/dnsmasq reload`. **Verified ineffective where dnsmasq runs under ujail** (see below), and never applies CNAMEs. `dnsmasq` is accepted as a legacy alias. |
| `uci-apply` | Calls `uci apply` with no arguments. Commits and applies **every** pending UCI config, not just `dhcp`, so anything an admin left staged is applied too. Use when the RPC user cannot reach `rpc/sys`. |
| `none` | Commit only. Records land in `/etc/config/dhcp` but dnsmasq keeps serving the previous set. |

### Why a restart, and not a reload

The two record types land in different places, and only one of them survives a reload:

| Record | Written to | Picked up by |
| ------ | ---------- | ------------ |
| `A` | hostfile `/tmp/hosts/dhcp.*` | `SIGHUP` re-reads hostfiles |
| `CNAME` | `--cname=` in `/var/etc/dnsmasq.conf.*` | only a restart — dnsmasq reads its config file once, at startup |

On top of that, `reload` was measured doing nothing at all on OpenWrt 25: dnsmasq
runs inside **ujail**, `reload_service()` is
`rc_procd start_service; procd_send_signal dnsmasq`, and the signal reaches the
jail wrapper rather than the daemon. The regenerated hostfile contained the new
record while the running dnsmasq — started days earlier — kept answering
`NXDOMAIN` for it.

A restart costs roughly a second of DNS/DHCP downtime, runs only when records
actually changed, and DHCP leases survive in `/tmp/dhcp.leases`.

> **Never call LuCI's `uci apply` with a config name.** Its JSON-RPC binding is
> `function apply(config) return uci:apply(config) end`, but the underlying
> signature is `apply(self, rollback)` — any non-empty argument is read as
> `rollback = true`, which starts a ubus apply with a ≥90 s rollback timer that
> **reverts the change** unless it is confirmed out of band. This fork always
> calls it with an empty argument list.

## Configuration Options

Every environment variable, with its default, is listed in the
[example values file](example/values.yaml). Deployment is via the upstream
[external-dns helm chart](https://github.com/kubernetes-sigs/external-dns/tree/master/charts/external-dns)
with this webhook as a sidecar — see [skaffold.yaml](skaffold.yaml) for a
working local setup.

| Variable | Default | Meaning |
| -------- | ------- | ------- |
| `PROVIDER_OPENWRT_LUCIRPC_HOSTNAME` | `192.168.1.1` | Router address |
| `PROVIDER_OPENWRT_LUCIRPC_PORT` | `443` | LuCI port |
| `PROVIDER_OPENWRT_LUCIRPC_SSL` | `true` | Use HTTPS |
| `PROVIDER_OPENWRT_LUCIRPC_AUTH_USERNAME` / `_PASSWORD` | — | LuCI credentials |
| `PROVIDER_OPENWRT_RELOADSTRATEGY` | `restart` | See [Reload strategy](#reload-strategy) |
| `PROVIDER_OPENWRT_OWNERSHIPID` | *(empty)* | See [Ownership](#ownership--required-before-policy-sync) |
| `PROVIDER_OPENWRT_OWNERSHIPOPTION` | `external_dns` | UCI option holding the ownership ID |
| `PROVIDER_OPENWRT_ADOPTEXISTING` | `true` | Adopt matching unmarked sections |

## Supported records

`A` and `CNAME` only. Anything else is dropped in `AdjustEndpoints` with a
warning rather than being planned and silently skipped on every run.

Per-record TTLs are not supported either — `domain`/`cname` sections have no TTL
field and dnsmasq answers them from its global `local_ttl` — so a TTL on the
desired endpoint is stripped for the same reason.

## Credits

Originally written by [@renanqts](https://github.com/renanqts) — [buy them a coffee](https://www.buymeacoffee.com/renanqts4).
