# Changelog

## v0.2.0

First release of the [VizzleTF](https://github.com/VizzleTF/external-dns-openwrt-webhook)
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
- Container images published to `ghcr.io/vizzletf/external-dns-openwrt-webhook`
  for `linux/amd64` and `linux/arm64`.
- Regression tests for each of the fixes above.

### Changed

- Module path is now `github.com/VizzleTF/external-dns-openwrt-webhook`.
- `OpenWRT.SetDNSRecords`/`UpdateDNSRecords`/`DeleteDNSRecords` are replaced by
  a single `ApplyDNSRecords(ctx, remove, add)` that reconciles in one pass.

### Known limitations

- Record types are still limited to `A` and `CNAME`.
- Per-record TTLs are not supported: UCI `domain`/`cname` sections have no TTL
  field and dnsmasq serves them with its global `local_ttl`. `Records()`
  reports a fixed 300, so endpoints requesting an explicit TTL are re-planned
  on every run.
