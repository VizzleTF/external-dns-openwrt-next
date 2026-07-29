# Changelog

## v0.6.0

Refactor pass. No behaviour change on the wire; the container just got a lot
smaller and the code a lot less coupled.

### Changed

- **Dropped the `sigs.k8s.io/external-dns` dependency.** That module is the
  controller, not a client SDK: importing its `endpoint`/`plan`/`provider`
  packages pulled in `k8s.io/apimachinery` (for CRD types this webhook never
  touches), klog, part of the AWS SDK and — from v0.21 — istio and contour.
  The webhook contract is a documented JSON API pinned by the media type
  `application/external.dns.webhook+json;version=1`, so the handful of wire
  types now live in `pkg/webhookapi`, with tests that pin every JSON field name
  against the upstream definitions.
- **Dropped gin, ginprom and prometheus.** Four routes and a health check do
  not need a framework, and nothing scraped the metrics endpoint. The HTTP
  layer is `net/http` plus a `ServeMux`.
- **Dropped viper and mapstructure.** Configuration only ever came from
  environment variables, yet viper dragged in HCL, TOML, INI and properties
  parsers plus a filesystem abstraction. `pkg/config` is now a small
  reflection binder over the existing `mapstructure` tags, and the exact
  variable names are pinned by a test.
- **Dropped zap** in favour of `log/slog`.

| | before | after |
| --- | --- | --- |
| binary | 30 MB | 9.5 MB |
| modules in graph | 298 | 36 (all test-only) |
| third-party packages linked | 232 | **0** |

### Fixed

- The logger swallowed an invalid `LOG_LEVEL` (`return nil` on a parse error)
  and silently kept the first logger on a second call.
- No timeout on the LuCI HTTP client: only the dial was bounded, so a router
  that accepted a connection and then stalled would hang the request.
- `errors.Is` instead of `==` for the auth sentinels, and a `>= 400` status
  check instead of `> 226`.
- `ShutodwnTimeout` typo, package-shadowing identifiers, and a shutdown path
  that cancelled the context before waiting on it.

### Removed

- The `/metrics` endpoint and `ROUTER_GIN_RELEASE_MODE` (setting the latter is
  now simply ignored).

## v0.5.0

### Fixed

- **Unsupported record types caused a permanent plan loop.** ExternalDNS
  manages `A`, `AAAA` and `CNAME` by default, but this provider can only write
  `A` and `CNAME`. An `AAAA` endpoint was therefore planned, silently skipped
  at write time, found missing on the next `Records()` call, and planned again
  — forever. `AdjustEndpoints` now drops unsupported types with a warning.
- **A per-record TTL had the same effect.** `domain`/`cname` sections carry no
  TTL, so an endpoint asking for one could never match what `Records()` reports.
  `AdjustEndpoints` now strips it.
- `PROVIDER_OPENWRT_OWNERSHIPOPTION` is validated at startup against the
  characters UCI accepts (`[A-Za-z0-9_]`). Previously a bad value was only
  discovered when the first record failed to write.

### Changed

- CI additionally runs `go test -race`.
- Repository hygiene after the fork: `CODEOWNERS`, skaffold/k3d artifact names,
  and a README configuration table replacing the stale line-number anchors.

## v0.4.0

### Fixed

- **Records were still not going live.** `PROVIDER_OPENWRT_RELOADSTRATEGY=dnsmasq`
  ran `/etc/init.d/dnsmasq reload`, which regenerates `/var/etc/dnsmasq.conf.*`
  and the `/tmp/hosts/dhcp.*` hostfile but does not make the running daemon
  re-read them: dnsmasq runs inside **ujail**, `reload_service()` is
  `rc_procd start_service; procd_send_signal dnsmasq`, and the signal reaches
  the jail wrapper instead of the daemon.

  Measured end to end: a record was committed, the hostfile contained it, and
  dnsmasq — running since days earlier — still answered `NXDOMAIN`, while
  another name from the very same hostfile resolved.

  A plain `SIGHUP` would not have been enough either. `A` records live in the
  hostfile, which `SIGHUP` re-reads, but `CNAME` records are `--cname=` entries
  in the config file, which dnsmasq reads once at startup.

### Changed

- New default strategy **`restart`** (`/etc/init.d/dnsmasq restart`), the only
  one that applies both record types. It costs about a second of DNS/DHCP
  downtime, runs only when records actually changed, and DHCP leases survive in
  `/tmp/dhcp.leases`.
- The old `dnsmasq` strategy is renamed **`reload`** and documented as
  ineffective under ujail. `dnsmasq` still validates, as a legacy alias.

## v0.3.0

Repository renamed to **external-dns-openwrt-next**; module path and image are
now `github.com/VizzleTF/external-dns-openwrt-next` and
`ghcr.io/vizzletf/external-dns-openwrt-next`.

### Added

- **Record ownership.** Records written by this provider carry a UCI marker
  option, and only marked sections are reported back to ExternalDNS. Entries
  created by hand become invisible: they cannot be updated and cannot be
  deleted, which is what makes `policy: sync` safe on a router that also holds
  manually maintained DNS. Configured with `PROVIDER_OPENWRT_OWNERSHIPID`
  (empty = disabled, previous behaviour), `PROVIDER_OPENWRT_OWNERSHIPOPTION`
  (default `external_dns`) and `PROVIDER_OPENWRT_ADOPTEXISTING`.

  The marker is inert: `dhcp_domain_add` reads only `name`/`ip` and
  `dhcp_cname_add` only `cname`/`target`, so it never reaches the generated
  dnsmasq config. A TXT registry is not an option — OpenWrt's UCI has no TXT
  support at all.
- **Adoption.** When a record ExternalDNS asks for already exists unmarked, the
  provider stamps the marker onto that section instead of adding a duplicate.
  This migrates an existing deployment on the first reconcile, with no manual
  edits on the router. Sections owned by a different ID are never adopted.

### Fixed

- **Data race on the session token.** The LuCI client wrote `token` from
  `auth()` while other requests read it, with no synchronisation.
- **Credentials in debug logs.** The request URL was logged in full including
  `?auth=<session token>`, the login body was logged with the router password,
  and the token was logged again on every `getUri` call. The URL is now redacted
  and the body is not logged.
- UCI options are decoded loosely instead of through fixed struct tags, so a
  `domain` section holding a *list* of names is skipped rather than failing the
  whole read.

## v0.2.0

First release of the [VizzleTF](https://github.com/VizzleTF/external-dns-openwrt-next)
fork of [renanqts/external-dns-openwrt-webhook](https://github.com/renanqts/external-dns-openwrt-webhook)
(upstream `v0.1.0`, last code change 2025-02-27).

`policy: sync` is now usable: records are created, updated and deleted.

### Fixed

- **Deletions never completed.** `DeleteDNSRecords` and `UpdateDNSRecords`
  removed elements from the very slice they were ranging over, so indices
  shifted and entries after the first removal were skipped. The leftovers then
  tripped the `records not found` check and the whole `ApplyChanges` failed, so
  ExternalDNS retried the same change set forever.
- **`records not found` was a hard error.** Removing a record that is already
  absent is the desired end state, not a failure. Both directions are now
  idempotent, so a single stale entry can no longer wedge every other change.
- **Updates re-created the old value.** `ApplyChanges` called
  `UpdateDNSRecords` on `changes.UpdateOld` *and* on `changes.UpdateNew`.
  `UpdateOld` is the previous state — it must only be withdrawn.
- **Multi-target endpoints were truncated.** `endpoints2DNSRecords` read
  `ep.Targets[0]` and discarded the rest.
- **Records were matched by name only.** With several sections sharing a name,
  the one that got deleted depended on Go's randomised map iteration order.
  Matching now uses the full identity — type, name *and* value.
- **`Records()` reported one endpoint per UCI section**, so a multi-target name
  came back as several endpoints with the same name and type and ExternalDNS
  planned a change on every run. Sections are now merged into a single endpoint
  with sorted targets, and the endpoint list itself is sorted.
- **Changes were committed but never applied.** `uci commit` only writes
  `/etc/config/dhcp`; it neither regenerates `/var/etc/dnsmasq.conf.*` nor
  signals the daemon, so records stayed invisible until something else
  restarted dnsmasq. A reload step was added — see `PROVIDER_OPENWRT_RELOADSTRATEGY`.
- **The commit ran even on the error path**, leaving partially staged UCI
  changes behind. There is now exactly one commit per change set, and none at
  all when nothing changed.
- **`ApplyChanges` fetched all records and threw the result away** before doing
  any work.

### Added

- `PROVIDER_OPENWRT_RELOADSTRATEGY`: `dnsmasq` (default, runs
  `/etc/init.d/dnsmasq reload` via `rpc/sys`), `uci-apply`, or `none`.
  The `uci-apply` path deliberately calls `uci apply` with **no** arguments:
  LuCI's binding is `function apply(config)` but the real signature is
  `apply(self, rollback)`, so any non-empty argument arms a ≥90 s rollback
  timer that reverts the change unless confirmed out of band.
- Container images published to `ghcr.io/vizzletf/external-dns-openwrt-next`
  for `linux/amd64` and `linux/arm64`.
- Regression tests for each of the fixes above.

### Changed

- Module path is now `github.com/VizzleTF/external-dns-openwrt-next`.
- `OpenWRT.SetDNSRecords`/`UpdateDNSRecords`/`DeleteDNSRecords` are replaced by
  a single `ApplyDNSRecords(ctx, remove, add)` that reconciles in one pass.

### Known limitations

- Record types are still limited to `A` and `CNAME`.
- Per-record TTLs are not supported: UCI `domain`/`cname` sections have no TTL
  field and dnsmasq serves them with its global `local_ttl`. `Records()`
  reports a fixed 300, so endpoints requesting an explicit TTL are re-planned
  on every run.
