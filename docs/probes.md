# Probes

For anyone who has to say what a smokeng graph measures, and when it misleads. It answers: what
does each probe type time, what counts as loss, and what does that number not tell you?

Every probe type produces a distribution of N round-trip times per interval. Pick the type with
`probe_type`; the settings are in [Configuration](configuration.md#probe-settings-reference).

| Type | Timer starts | Timer stops | Loss is |
| --- | --- | --- | --- |
| `icmp` | echo request handed to the kernel | echo reply received | no reply within `timeout_ms` |
| `dns` | UDP query written | a reply with the same query ID arrives | no reply within `timeout_ms` |
| `tcp` | `connect()` called | handshake complete | timeout, or connection refused |
| `http` / `https` | request started (new connection) | response headers received | timeout, refused or reset, TLS rejected, status 400 or higher |
| `irtt` | per packet, by the irtt client | reply for that packet | packet without a reply |

Path discovery (traceroute) is not a probe type; see [Path discovery](#path-discovery).

## What loss means

A probe is lost when it was sent and no usable answer came back. A **local failure** is not the
target's fault and is recorded differently: the probe counts as sent and is flagged as a *send
failure* with a reason. The loss rail shows it labelled as smokeng's side, not the network's.

| Event | Counted as |
| --- | --- |
| Timeout (any type) | loss |
| `http`/`https` status 400 or higher | loss; the status is named once per interval in the log |
| `tcp` connection refused (`ECONNREFUSED`) | loss; the log says so once per interval |
| ICMP error for an echo (unreachable, prohibited) | loss, with the error named on the graph |
| `dns` reply of any kind (NXDOMAIN and SERVFAIL included) | **not loss**; see [DNS](#dns) |
| Out of file descriptors, ports or buffers on the prober (`tcp`, `http`) | send failure |
| Query or request that could not be built or written | send failure |
| `irtt` session that never opened | send failure for every probe of the interval |
| `irtt` session that paced fewer packets than asked | not counted at all; the interval is narrower |
| Receive queue overflow on the ICMP socket | interval flagged `dropped replies`; some loss is ours |

Badges and how to read them: [Reading graphs](reading-graphs.md).

## ICMP

**Times** the echo request to its reply. Kernel timestamps are used where the kernel provides
them, so the number is the network and not the prober.

- **A router is a bad subject.** A router answers echo requests on its control plane, rate
  limits them and serves them at low priority. Loss or a wide band to a router's own address is
  not forwarding loss. Measure a host behind it, or compare with a `tcp` or `irtt` target on
  the same path.
- A middlebox can answer for a dead host. An echo reply says an address answered, not that the
  service is up.
- ICMP and TCP/UDP can be queued differently along a path. If they disagree, believe the one
  that matches your traffic.

Settings: `packet_size`, `dscp`, `timeout_ms`.

## DNS

**Times** one UDP query to the target, which is the resolver being measured, from the write to
the reply. Only the reply ID is matched; the answer is not inspected.

- **The same query goes out every interval**, so a recursive resolver answers from cache after
  the first one. You are measuring the cache, not the resolver's upstream work. To test the
  upstream, query an authoritative server, or a name you control with a short TTL.
- Without `dns_query` the probe asks `.` NS, the smallest question any resolver can answer.
  `dns_rr_type` defaults to `A` for a named query.
- **A reply of any kind is a success**, SERVFAIL and NXDOMAIN included. A resolver that
  returns errors fast looks fast and healthy. UDP only; there is no TCP fallback.
- A resolver going slow is invisible to ICMP.

Settings: `dns_query`, `dns_rr_type`, `probe_port` (default 53), `dscp`, `timeout_ms`.

## TCP

**Times** the handshake: `connect()` until the kernel completes it. The connection is then
aborted with a reset, so the prober does not pin ephemeral ports in TIME_WAIT.

- It goes through a queue ICMP does not share, so it catches a router that deprioritises ICMP.
- **A refused connection counts as loss.** The host answered, but the service did not, and the
  service is what a `tcp` target asks about.
- It measures the handshake, not the service. To ask "does it serve", use `http`.
- `probe_port` has no default. A `tcp` target without it is not measured at all; the log and
  the target's page say so.
- The handshake completes inside the kernel, so the measurement carries the `userspace TX/RX`
  flag. See [Timing accuracy](#timing-accuracy).

Settings: `probe_port` (required), `dscp`, `timeout_ms`.

## HTTP and HTTPS

**Times** a `GET` on a fresh connection, from the start of the request to the **response
headers**: TCP handshake, TLS handshake for `https`, request, and the server's processing.

- **Server think time is in the number.** A slow application looks like a slow network. To
  separate the two, compare with a `tcp` target on the same port.
- Timing stops at the headers. The body is not read, so transfer speed does not widen the band.
- There is no keep-alive: every probe includes the handshakes, so every sample is a cold client.
- **Status 400 or higher is loss.** 1xx to 3xx are good measurements.
- **Redirects are not followed.** A 3xx is one round trip to this host and is recorded as such.
- smokeng resolves the name itself and dials that address. The URL, and so virtual hosting and
  certificate verification, use the target's `host`.
- Certificates are verified, and one that cannot be verified reads as total loss. See
  [Internal certificates](#internal-certificates).
- The request carries `User-Agent: smokeng (+https://github.com/timdebruijn/smokeng)`.

Settings: `http_path` (default `/`), `probe_port` (default 80, or 443 for `https`),
`tls_skip_verify`, `dscp`, `timeout_ms`.

## irtt

**Times** each packet of one UDP session against an
[`irtt server`](https://github.com/heistp/irtt) at the far end, so it works only where you run
both sides. There is one session per interval; the far end paces the train and reports each
packet.

- Ordinary UDP: it is not subject to the control-plane rate limits of ICMP, and no middlebox
  answers it for the target.
- It honours `probe_mode`. In burst mode `burst_gap_ms` must be above zero, or the target is
  refused.
- Besides the round trip, smokeng stores `ipdv_send`, `ipdv_receive` (inter-packet delay
  variation per direction) and `server_processing`. `graph_series` chooses which are drawn;
  see [Configuration](configuration.md#graph_series).
- **Absolute one-way delay needs synchronised clocks.** It subtracts one machine's wall clock
  from another's, so it measures NTP as much as the network. smokeng does not graph it. IPDV
  compares consecutive packets on one monotonic clock, so the offset cancels and it holds
  without sync.
- `packet_size` sets the irtt packet length. `timeout_ms` is not a per-packet timeout: the
  session ends at the end of the interval plus `timeout_ms`.

Settings: `packet_size`, `probe_port` (default 2112), `dscp`, `timeout_ms`.

### Authenticating to an irtt server

An open irtt server is a UDP reflection and amplification vector, and UDP is spoofable. A server
you expose should require an HMAC key: `irtt server --hmac=<secret>`. It then drops any packet
without a valid HMAC. The HMAC authenticates the packets; it does not encrypt them.

The key is a shared secret, so it is **not** a target setting: it would travel through the API
and the exported TOML. It lives in a keyfile on the host that runs the probe, mapping each irtt
endpoint to its secret:

```toml
# /var/lib/smokeng/irtt-hmac-keys.toml: mode 0600, deploy from a vault
"resolver.gemeentex.nl:2112" = "the-shared-secret"
"10.30.0.5:2112"             = "another-secret"
```

```bash
smokeng serve --irtt-hmac-keys /var/lib/smokeng/irtt-hmac-keys.toml
```

The endpoint is the target's configured host and port as written in the target, not its
resolved address, so the mapping survives DNS changes. An endpoint with no entry is probed
without HMAC. If its server requires one, the session is refused and recorded as a flagged send
failure: that tells you the key is missing, not that the server is down. The Ansible role
renders the file from `smokeng_irtt_hmac_keys`; keep the values in a vault. A remote agent needs
its own keyfile; see [Remote agents](agents.md).

## Path discovery

Path discovery is not a probe type. It runs beside whatever the target's `probe_type` is, every
`trace_interval_s` seconds (default 300, `0` is off), and answers one question: did the path
change when the smoke changed?

- **What it sends.** One ICMP echo per TTL, from 1 up to 30 hops, on a socket of its own. It
  waits up to 2 seconds per hop (this is not `timeout_ms`) and stops when the destination
  answers with an echo reply. It is ICMP even for an `https` target, so a path that filters
  ICMP ends in stars.
- **What is stored.** The hop list as text: addresses separated by commas, with `*` for a hop
  that did not answer. Per-hop latency is not measured.
- **When it is stored.** Only when the hop list differs from the last stored one. A path-change
  mark on the graph therefore means *the hop list differed*, not *the route changed*.
- **False marks.** One lost time-exceeded reply turns an address into `*` and makes a mark.
  Load balancing (ECMP) that sends consecutive echoes through different routers can do the same
  without any routing change. Many routers rate-limit time-exceeded replies. Be suspicious of
  a lone mark; look for it to repeat or to persist.
- **Linux only**, on the master's prober and on remote agents alike; each vantage point records
  its own path. Elsewhere no path is recorded, rather than a wrong one. An agent needs a master
  of v0.26.0 or later to send them.
- `dscp` and `packet_size` do not apply.

## Which settings apply

| Setting | Applies to | Notes |
| --- | --- | --- |
| `packet_size` | `icmp`, `irtt` | Payload bytes for `icmp`, packet length for `irtt`; 12 to 9000 on write |
| `dscp` | `icmp`, `dns`, `tcp`, `http`, `https`, `irtt` | Not path discovery. **Admin only** to set |
| `timeout_ms` | all five | Per probe for `dns`, `tcp`, `http`, `https`. For `icmp` and `irtt`, an interval is closed `timeout_ms` after it ends |

`dscp` marks the prober's traffic on the network it runs on, so only a global admin can set it:
a user with an edit grant on a subtree is refused (`internal/api/targets.go`). `retention_s` is
admin-only for the same reason. Use `dscp` to measure one traffic class: the same host as two
targets, with `dscp = 46` and `0`, shows whether the network treats them differently.

## Privileges

- **ICMP and path discovery** need unprivileged ping sockets. The one sysctl covers IPv4 and
  IPv6:

  Setup, with the recommended service-gid-only range, is in
  [Getting started](getting-started.md). The wide range (`0 2147483647`) lets every local user
  open ICMP sockets.

  Without it, `icmp` falls back to a raw socket if the process may open one (root or
  `CAP_NET_RAW`), flagged `raw socket`. Path discovery has no fallback: without the sysctl no
  path is recorded.
- `dns`, `tcp`, `http`, `https` and `irtt` use ordinary UDP and TCP sockets and need no
  privilege.

## Dual stack and what a name resolves to

IPv4 and IPv6 are separate targets, on purpose: they take different paths and fail
independently. A hostname resolves **one address per family**. `v4` takes an A record, `v6` an
AAAA record (`internal/probe/dnscache/dnscache.go`).

- The first matching record in the answer is the one measured. smokeng honours the record's TTL
  with a floor of 30 seconds and a ceiling of 24 hours, and refreshes in the background.
- **A multi-A, anycast or CDN name measures one endpoint**: the one DNS put first at that
  moment. It can change when the TTL expires and the answer comes back in another order. The
  graph describes that endpoint, not the service as a whole. To watch several, create one
  target per address with a literal IP.
- A literal IP bypasses DNS.
- Names are resolved with the resolvers in `/etc/resolv.conf` of the probing host.

## No source address or interface

There is no setting for the source address or the interface. Probes leave by whatever route the
OS picks. On a multihomed host, pin them from outside smokeng: run the prober in a network
namespace, or use a policy rule (`ip rule`) on the prober's UID.

## Politeness: how much load a target gets

The load on one target is `pings_per_interval / interval_s` probes per second. The defaults, 20
per 60 s, are one probe every three seconds. smokeng refuses configuration above 100 probes per
second or 500 000 bytes per second for one target, and above 5000 probes per second in total
([Limits](configuration.md#limits)).

The two modes spend that load differently:

| Mode | Packets go out | It answers |
| --- | --- | --- |
| `burst` | `pings_per_interval` packets, `burst_gap_ms` apart (default 10 ms) | How is the network *at one moment*? |
| `spread` | evenly across the whole interval | How is the network *over the interval*? |

- **Burst puts N packets within N x `burst_gap_ms`**: 20 at 10 ms is 200 ms. Many routers and
  hosts rate-limit ICMP, and a burst trips that limit, so the loss you see can be produced by
  the burst. If loss looks like a limit, switch the target to `spread` and compare.
- Use `burst` for the SmokePing-style picture of a path and for your own equipment. Use `spread`
  for third-party and public targets, or anything that rate-limits.
- A burst must fit in its interval: `pings_per_interval x burst_gap_ms` must be less than
  `interval_s x 1000`.

## Internal certificates

Most `https` targets worth watching are internal, and an internal certificate is signed by a CA
the host does not trust. There are two ways out. They are not equivalent.

**Trust the CA.** Verification stays on.

```bash
smokeng serve --tls-ca-file /etc/ssl/certs/gemeentex-root.pem
```

The flag takes a comma-separated list and each file may hold several certificates. They are
added to the system roots rather than replacing them. A file that parses to no certificate is
an error.

A file may hold **only** certificates. A master hands every file to its agents, so a bundle
with a private key appended is refused at startup, with the block type named and none of its
contents. What agents receive is the certificates, parsed and encoded again, never the file as
it was written.

It is a flag and not a target setting because a CA belongs to the deployment, not to one
measurement.

**Remote agents get these CAs from the master**, delivered with their assignments, so a
rotation reaches every agent within a poll. They apply to `https` probes only, never to an
agent's own connection to its master. An agent can add its own with `--tls-ca-file` or refuse
the master's with `--no-remote-cas`; see [Remote agents](agents.md).

**Or turn verification off for one target.**

```toml
[targets."Klanten/GemeenteX/oud-apparaat"]
host = "10.20.0.9"
address_family = "v4"
probe_type = "https"
tls_skip_verify = true
```

It is inheritable, so it can cover a subtree; avoid that. A measurement taken this way says
that something answered on the port, not that it was the service the target names. The target's
page and the graph show it. There is no instance-wide switch.

## Timing accuracy

`icmp` and `dns` are kernel-timestamped, so what they report is the network and not the network
plus whatever delayed the prober. `tcp`, `http`, `https` and `irtt` are timed around a
userspace call and carry the `userspace TX/RX` flag on every measurement. A band widened by a
busy prober must never be readable as a slow service.

Under heavy load the userspace-timed `tcp` band widened about thirteenfold while the
kernel-timed `icmp` and `dns` did not move; the numbers are in [DESIGN.md](../DESIGN.md). If this
matters for your `tcp` and `http` targets, run the prober as its own process; see
[Operations](operations.md).

`tcp`, `http` and `https` cannot be stamped by the kernel: the handshake completes inside the
kernel and userspace only sees the call return.
