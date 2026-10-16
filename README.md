# external-dns-openwrt-next

An [ExternalDNS](https://github.com/kubernetes-sigs/external-dns) webhook provider that manages DNS records on an OpenWrt router, so hostnames from Kubernetes Ingresses, Services and Gateway API routes resolve on the LAN.

For examples of creating DNS records via CRDs or via Ingress/Service annotations, see the [example directory](./example).

> **Fork.** Continuation of [renanqts/external-dns-openwrt-webhook](https://github.com/renanqts/external-dns-openwrt-webhook),
> whose last code change was in February 2025. It adds working create/update/delete,
> record ownership, multi-target endpoints and all-or-nothing change sets.
> See [CHANGELOG.md](./CHANGELOG.md).
>
> Images: `ghcr.io/vizzletf/external-dns-openwrt-next` (linux/amd64, linux/arm64).

## Limitations
- Supported DNS record types: `A`, `CNAME`. Anything else ExternalDNS asks for —
  `AAAA` from a dual-stack Service, `PTR`, the rest — is dropped, with a warning,
  before it reaches the plan. See [Supported records](#supported-records).
- Names and targets are printable ASCII without spaces, `,`, `"` or `#`, at
  most 253 characters with labels of at most 63, and an `A` target is an IPv4
  address. An endpoint holding anything else is dropped the same way.
- Wildcard names are not supported. dnsmasq answers a hostfile entry or a CNAME
  alias for the literal name only, so `*.apps.lan` would answer `*.apps.lan`
  and nothing under it. Such an endpoint is dropped with a warning.
- A CNAME has one target. An endpoint with more, as `target: a.lan,b.lan`
  gives, is dropped with a warning: dnsmasq does not start with two CNAMEs for
  one alias.
- Per-record TTLs are not supported. UCI `domain`/`cname` sections carry no TTL —
  dnsmasq answers them with its global `local_ttl` — so `Records()` reports a
  fixed 300 and a requested TTL is stripped rather than planned on every run.
- Set identifiers and provider-specific properties are ignored, and stripped
  for the same reason: a section has no place for them. Endpoints for one name
  and type that differ only in set identifier become one record, and
  ExternalDNS keeps one of them.

## What this fork changes

`policy: sync` works, with [`registry: noop`](#registry-noop-not-txt): records
are created, updated **and** deleted — and with ownership enabled it can only
ever touch records it created itself.

- **Deletions apply.** Upstream matched records by name only and mutated the
  slice it was iterating over, so with more than one record in a change set some
  were skipped and the run ended in `records not found`.
- **Updates no longer resurrect the old value.** Upstream wrote `UpdateOld` back
  to the router before writing `UpdateNew`.
- **Multi-target A endpoints work.** Upstream read `Targets[0]` and dropped the
  rest. Records are now matched on their full identity (name *and* value), so
  removing one target of a name leaves the others alone. A CNAME endpoint with
  more than one target is dropped, since dnsmasq does not start with two
  CNAMEs for one alias.
- **Idempotent.** Deleting an absent record or adding an existing one is a
  no-op instead of an error, so one stale entry can no longer wedge every
  subsequent change.
- **One apply per change set**, and nothing is applied when nothing changed.
  A change set that fails halfway is dropped whole: what it staged never
  reaches the router, and the next one starts clean.
- **Records go live.** A change set is applied with `uci apply`, which
  commits it and has procd reload dnsmasq: a changed CNAME restarts it, about
  a second of DNS/DHCP downtime, and a changed A record only has the hostfile
  re-read.

## Router requirements

OpenWrt 24.10 or 25.12, both checked by the e2e tests, with LuCI. No extra
packages: the webhook calls ubus over the `/ubus` endpoint uhttpd serves for
LuCI, and rpcd behind it, both of which come with LuCI. luci-mod-rpc,
luci-lib-ipkg and luci-compat, which earlier versions needed, can be removed.
LuCI master has dropped luci-mod-rpc, which is why the webhook moved off it.

`root` works with the stock ACLs. A login that can write `dhcp` and nothing
else is safer, since the password sits in a Kubernetes Secret. On the router:

```sh
cat > /usr/share/rpcd/acl.d/external-dns.json <<'EOF'
{
	"external-dns": {
		"description": "external-dns: manage DNS records in dhcp",
		"read": {
			"ubus": { "uci": [ "get" ] },
			"uci": [ "dhcp" ]
		},
		"write": {
			"ubus": { "uci": [ "add", "set", "delete", "apply" ] },
			"uci": [ "dhcp" ]
		}
	}
}
EOF
uci add rpcd login
uci set rpcd.@login[-1].username='external-dns'
uci set rpcd.@login[-1].password="$(uhttpd -m 'PASSWORD')"
uci add_list rpcd.@login[-1].read='external-dns'
uci add_list rpcd.@login[-1].write='external-dns'
uci commit rpcd
/etc/init.d/rpcd restart
```

Then set `PROVIDER_OPENWRT_LUCIRPC_AUTH_USERNAME=external-dns` and the
password. Any user but `root` needs an rpcd login like this one: a system
account alone cannot log in to ubus.

## Ownership — required before `policy: sync`

A router usually holds DNS entries nobody handed to ExternalDNS: the NAS, the
hypervisors, whatever was typed into LuCI once. This provider has no TXT
registry to fall back on — OpenWrt's UCI has no TXT support at all — so
`policy: sync` with everything visible would delete exactly those entries.
(ExternalDNS v0.22.0 offers a second answer to that, the
[CRD registry](#alternative-the-crd-registry).)

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

The marker lives in the `external_dns` UCI option. With ownership on, an
unmarked section that already matches a record exactly is adopted — stamped
with the marker — instead of getting a duplicate next to it.

| Variable | Default | Meaning |
| -------- | ------- | ------- |
| `PROVIDER_OPENWRT_OWNERSHIPID` | — | Marker value, required: the webhook does not start without it. Give each ExternalDNS instance writing to the same router a distinct ID. **An explicit empty value disables ownership and every section is reported — set it only with a registry that keeps ownership itself, such as the [CRD registry](#alternative-the-crd-registry), and never with `policy: sync` otherwise.** |

### Registry: `noop`, not `txt`

The chart defaults to `registry: txt`, which keeps the owner of each record in
a TXT record beside it. This provider has nowhere to store one, so the `txt`
registry finds no owner on anything the router reports and drops every update
and delete from the plan: a changed address or a deleted Service never
reaches the router, while ExternalDNS logs
`All records are already up to date`. Set `registry: noop`, which plans
against `Records()` as reported, and let the marker above decide what that
holds. With ownership off, `policy: sync` then deletes every record the
cluster does not ask for.

The [example values](example/values.yaml) do this, and set `domainFilters`
too. Without a filter, anyone who can create an Ingress or a Service can
publish any name for the whole LAN; with one, `sync` touches no name outside
it either.

Use the CRD registry below instead when ownership should live in the cluster
rather than on the router.

### Alternative: the CRD registry

ExternalDNS v0.22.0 added `--registry=crd`, which keeps ownership in `DNSRecord`
objects (`externaldns.k8s.io/v1alpha1`) in the cluster instead of in TXT
records. It is the upstream answer to a provider whose backing store has no TXT
support, and it needs nothing from this webhook:

```
--registry=crd --txt-owner-id=my-cluster --crd-registry-namespace=external-dns
```

It is a genuinely different model, not a second flavour of the marker above.
The CRD registry reads current state from its own `DNSRecord` objects and never
calls `Records()` at all, so anything on the router it did not create is
invisible to the plan and `policy: sync` cannot delete it — the same protection
the marker gives, enforced one layer up.

What you give up is the router as the source of truth. A record deleted in LuCI
behind ExternalDNS's back is still Programmed in the cluster, so nothing
re-creates it; the UCI marker, being on the router, survives a lost cluster and
keeps `Records()` honest. The CRD registry is also alpha, and its API is
in-cluster only.

Set `PROVIDER_OPENWRT_OWNERSHIPID` to an empty value when using it — the marker
is then pure overhead. The two can be combined, but only the registry decides
what is planned.

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

### Records that would stop dnsmasq

dnsmasq does not start with two CNAMEs for one alias, even two with the same
target, or with a CNAME loop, an alias pointing at itself included, and DNS
and DHCP for the whole LAN go down with it. So a CNAME is skipped with a
warning, not written, when another section already holds its alias (one with
another target, whoever owns it, or another instance's with the same target),
or when it would close a loop through any CNAMEs on the router. An identical
unmarked section is still adopted. The rest of the change set goes through.
The skipped record never reaches the router, so ExternalDNS asks for it again
on every run and the warning repeats, but nothing is applied for it. Remove
the section in the way, or change the endpoint, to end it.

An A record is still written next to a section for the same name with another
address that carries no marker or another ID. The provider cannot see that
section, so it warns that dnsmasq will answer both addresses.

### Changing the ownership ID

A section keeps the ID it was written with. Change
`PROVIDER_OPENWRT_OWNERSHIPID` alone and every record written under the old ID
is orphaned: owned by another ID, it is neither adopted nor deleted, and
ExternalDNS asks for all of them again. Each A record gets a second section
next to the orphan, and each CNAME is skipped because its alias is taken. Move
the sections to the new ID instead, with ExternalDNS stopped so that neither ID
writes in between. On the router, for `OLD` and `NEW`:

```
for s in $(uci -X show dhcp | sed -n "s/^dhcp\.\([^.]*\)\.external_dns='OLD'$/\1/p"); do
  uci set "dhcp.$s.external_dns=NEW"
done
uci commit dhcp
```

Then start ExternalDNS with the new ID.

## Configuration Options

Every environment variable, with its default, is listed in the
[example values file](example/values.yaml). Deployment is via the upstream
[external-dns helm chart](https://github.com/kubernetes-sigs/external-dns/tree/master/charts/external-dns)
with this webhook as a sidecar — see [skaffold.yaml](skaffold.yaml) for a
working local setup.

The examples track ExternalDNS v0.22.0 (chart 1.22.0). Two of its breaking
changes reach anything deployed from here:

- annotations lost the `alpha` prefix and have no fallback — use
  `external-dns.kubernetes.io/hostname`, or start ExternalDNS with
  `--annotation-prefix=external-dns.alpha.kubernetes.io/`;
- the chart no longer defaults `policy`, so it has to be set explicitly.

The webhook itself speaks the same wire contract to every ExternalDNS version
that has a webhook provider (v0.14 onwards).

### Two listeners

The webhook serves the provider API on `127.0.0.1:8888`, and the probes and
metrics on `:8080`, which is what the [specification](https://kubernetes-sigs.github.io/external-dns/latest/docs/tutorials/webhook-provider/)
asks for and what the chart probes by default.

Loopback is not decoration. The API authenticates nobody and can rewrite every
record on the router, so binding it to the pod IP would hand that to anything
that can reach the pod. ExternalDNS calls it as a sidecar over loopback
(`--webhook-provider-url` defaults to `http://localhost:8888`), while the
kubelet probes through the pod IP — hence the second listener rather than one
socket serving both.

Set `ROUTER_ADDRESS=0.0.0.0` to run the webhook outside the ExternalDNS pod.
Nothing in it is safe to expose beyond a trusted network.

`/healthz` answers for the process, not for the router: an unreachable router is
reported as a 500 on `/records` and as `http_requests_total{status="500"}`.
Failing the probe instead would restart a pod that is working correctly and
would not bring the router back.

Past the negotiation at startup, ExternalDNS sends each request once. It logs a
500–510 as a soft error and tries again on its next run. Any other failure —
another status, a refused connection, or a request still unanswered after
`--webhook-provider-read-timeout` plus `--webhook-provider-write-timeout` (5 s
and 10 s by default) — makes it exit, and its container restarts. One call to
the router may take `PROVIDER_OPENWRT_LUCIRPC_TIMEOUT`, 15 s by default, and a
change set makes one per record, so a slow router or a large first sync can end
that way rather than in a 500: raise `--webhook-provider-write-timeout` if it
does, as the example values do, to 60 s. A change set ExternalDNS gave up on
is still applied to the end, and the one the restarted ExternalDNS sends waits
for it rather than writing the same records a second time.

| Variable | Default | Meaning |
| -------- | ------- | ------- |
| `PROVIDER_OPENWRT_LUCIRPC_HOSTNAME` | — | Router address, required |
| `PROVIDER_OPENWRT_LUCIRPC_PORT` | `443` | uhttpd port |
| `PROVIDER_OPENWRT_LUCIRPC_SSL` | `true` | Use HTTPS |
| `PROVIDER_OPENWRT_LUCIRPC_AUTH_USERNAME` / `_PASSWORD` | — | `root` or an rpcd login, see [Router requirements](#router-requirements) |
| `PROVIDER_OPENWRT_OWNERSHIPID` | — | Required, empty disables ownership, see [Ownership](#ownership--required-before-policy-sync) |
| `ROUTER_ADDRESS` | `127.0.0.1` | Address the provider API binds to |
| `ROUTER_PORT` | `8888` | Port the provider API listens on |
| `ROUTER_HEALTHCHECK_PORT` | `8080` | Port `/healthz` and `/metrics` listen on, on every interface |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `LOG_ENCODING` | `json` | `json` or `console` |
| `SHUTDOWN_TIMEOUT_SECONDS` | `5` | Graceful shutdown budget |

## Dependencies

The shipped binary links **no third-party packages** — only the Go standard
library. The webhook contract types live in `pkg/webhookapi` rather than being
imported from `sigs.k8s.io/external-dns`, which is the controller rather than a
client SDK and would drag in Kubernetes apimachinery, klog and part of the AWS
SDK for three structs. See [CHANGELOG.md](./CHANGELOG.md#v060).

`/metrics` is served on the observability port, in the Prometheus text format,
written by hand for the same reason — see [Metrics](#metrics).

## Metrics

`GET :8080/metrics`, in the Prometheus text exposition format; the chart can
scrape it through
`provider.webhook.serviceMonitor`.

| Metric | Type | What it tells you |
| ------ | ---- | ----------------- |
| `external_dns_openwrt_build_info{version,goversion}` | gauge | Always 1. `version` comes from the build info ko stamps in. |
| `external_dns_openwrt_http_requests_total{route,status}` | counter | Provider API traffic. A climbing 500 count is the first sign the router is unreachable. ExternalDNS tries a 500–510 again on its next run and exits on any other failure, see [Two listeners](#two-listeners). |
| `external_dns_openwrt_http_request_duration_seconds_total{route}` | counter | Seconds spent serving each route; divide by the request count for the mean. A call to a busy router is the slow part. |
| `external_dns_openwrt_records` | gauge | Records the router reported on the last `Records` call — with ownership on, the ones carrying the marker. |
| `external_dns_openwrt_planned_endpoints_total{action}` | counter | Endpoints ExternalDNS asked for, by `create`/`update`/`delete`. Flat lines are a quiet cluster; a steady climb is a plan that never converges. |
| `external_dns_openwrt_apply_changes_total{result}` | counter | Change sets applied, `success` or `error`. |
| `external_dns_openwrt_last_apply_success_timestamp_seconds` | gauge | When a change set last reached the router. `time() - <metric>` is the alert worth having. |
| `external_dns_openwrt_dropped_endpoints_total{record_type}` | counter | Endpoints dropped in `AdjustEndpoints`, by type — `AAAA` from a dual-stack Service, `PTR` from `--create-ptr`, anything UCI has no section for, and `A`/`CNAME` with a wildcard name, a name or target dnsmasq cannot take, or, for a CNAME, more than one target. The type is the actionable part: it says which flag to narrow, or that an annotation needs fixing. |

No client library: the exposition format is a few lines per metric, and
prometheus/client_golang would be the only third-party code in the binary.

## Supported records

`A` and `CNAME` only. Anything else is dropped in `AdjustEndpoints` with a
warning rather than being planned and silently skipped on every run.

A `domain` section holding an IPv6 address, `::ffff:a.b.c.d` included, is an
`AAAA` record to dnsmasq, so `Records()` leaves it out whatever the ownership
setting: it is never planned against the cluster's `A` records for the name,
and never deleted. An `A` endpoint whose target is not an IPv4 address is
dropped, since dnsmasq would serve it as `AAAA` or not at all.

Per-record TTLs are not supported either — `domain`/`cname` sections have no TTL
field and dnsmasq answers them from its global `local_ttl` — so a TTL on the
desired endpoint is stripped for the same reason.

An endpoint whose name or any target holds whitespace, a control character, a
non-ASCII byte, `,`, `"` or `#`, or a label over 63 characters, is dropped too,
as is a name over 253. The init script copies UCI values into dnsmasq's config
and hostfile verbatim: a newline starts a directive of its own, a comma adds a
CNAME alias, `#` starts a comment, and a stray quote, a non-ASCII byte or an
overlong label in a CNAME keeps dnsmasq from starting. Dropping the endpoint
keeps it from failing the change set it arrives in. A section that already
holds one is still reported, so `policy: sync` with `registry: noop` deletes it;
anywhere else, remove it on the router by hand.

### What the annotations do here

Annotations lost the `alpha` prefix in v0.22.0, so they now read
`external-dns.kubernetes.io/…`. The ones that decide what this provider is
asked to write:

| Annotation | Effect here |
| ---------- | ----------- |
| `hostname` | The name to publish; comma-separated for several. A wildcard (`*.apps.lan`) is dropped, since dnsmasq does not expand it. |
| `target` | Overrides the record value. A target parsing as IPv4 is published as `A`, as IPv6 as `AAAA` — which this provider drops — and anything else as `CNAME`. A CNAME takes one target, so a list of several is dropped. |
| `ttl` | Stripped in `AdjustEndpoints`, as above. |
| `set-identifier` | Stripped in `AdjustEndpoints`. Endpoints for one name and type that differ only in it become one record, and ExternalDNS keeps one of them. |
| `webhook-*`, `alias` | Provider-specific properties, stripped in `AdjustEndpoints`. |
| `record-type` | `ptr` opts a resource into PTR records, as does `--create-ptr`. UCI cannot write them, so they are dropped. |

`--managed-record-types` defaults to `A,AAAA,CNAME`, so a dual-stack Service
yields an `AAAA` endpoint this provider cannot write and warns about on every
reconcile. Narrowing it keeps the plan — and the log — to what the router can
actually serve:

```
--managed-record-types=A --managed-record-types=CNAME
```

## Credits

Originally written by [@renanqts](https://github.com/renanqts) — [buy them a coffee](https://www.buymeacoffee.com/renanqts4).
