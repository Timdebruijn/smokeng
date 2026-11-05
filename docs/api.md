# API

For people who script against smokeng: what authenticates a request, which routes exist, what
they return, and what the alert webhook sends.

This is the API the web UI uses. Nothing promises it stays stable beyond what the UI needs, so
pin the smokeng version you script against.

## Authentication

**There are no API tokens and no bearer auth.** The only credential a user route accepts is the
session cookie that OIDC login sets. No `Authorization` header is read anywhere.

| Setup | What a script can do |
| --- | --- |
| OIDC on | Nothing without a session cookie. Use the CLI on the master host (`smokeng config import` / `export`, see [cli.md](cli.md)), or put an authenticating proxy in front. |
| OIDC off | Everything, as admin, with no credentials. smokeng allows this only on loopback, and answers only to loopback host names (and the `--external-url` host) while it has no authentication. |

Against an instance without OIDC, put `localhost` or `127.0.0.1` in the URL. Any other `Host`
header gets a `403`.

### The origin check on changes

Every `POST`, `PUT`, `PATCH` and `DELETE` passes an origin check. `GET`, `HEAD` and `OPTIONS` do
not. A change is refused with `403` when:

- `Sec-Fetch-Site` is present and is not `same-origin` or `none`, or
- `Origin` is present and does not match the request's `Host` (or `--external-url`).

A request with **neither header** passes. `curl` and other non-browser clients send neither, so
a script has to add nothing. If a reverse proxy rewrites `Host` and a browser gets this `403`,
set `--external-url` to the address people use.

Agents do not use sessions. They sign their requests with their own Ed25519 key; see
[agents.md](agents.md).

## Routes

The class says who may call a route:

- **public**: no session.
- **read**: a signed-in viewer. The result is limited to the caller's scope; see
  [access-control.md](access-control.md). With OIDC off, everyone is admin.
- **write**: needs editor on the node the change touches, or global admin.
- **admin**: global admin only. A grant never reaches these.
- **signed**: the agent protocol, authenticated by the agent's key.

| Method and path | Class | Purpose |
| --- | --- | --- |
| `GET /healthz` | public | Liveness. Always returns `ok`; it checks no database or prober. |
| `GET /api/v1/me` | public | Who the caller is, role, version. |
| `GET /metrics` | admin | Prometheus metrics. Public if `--metrics-public` is set. |
| `GET /api/v1/targets` | read | The target tree. |
| `POST /api/v1/targets` | write | Create a target. |
| `PATCH /api/v1/targets/{id}` | write | Change or move a target. |
| `DELETE /api/v1/targets/{id}` | write | Delete a target. |
| `GET /api/v1/measurements` | read | One series, as an Arrow IPC stream. |
| `GET /api/v1/availability` | read | Uptime and downtime episodes over a window. |
| `GET /api/v1/paths` | read | Route changes for one series. |
| `GET /api/v1/agents` | read | Agents and when they were last seen. |
| `GET /api/v1/alert-rules` | read | Alert rules. |
| `POST /api/v1/alert-rules` | write | Create a rule. |
| `PATCH /api/v1/alert-rules/{id}` | write | Change a rule. |
| `DELETE /api/v1/alert-rules/{id}` | write | Delete a rule. |
| `GET /api/v1/alerts` | read | What is firing now. |
| `POST /api/v1/alerts/ack` | write | Acknowledge a firing alert, or clear the acknowledgement. |
| `GET /api/v1/alert-events` | read | Transition history (`?limit=N`). |
| `GET /api/v1/alert-baselines` | read | Captured golden references. |
| `POST /api/v1/alert-rules/{id}/baseline` | write | Capture a golden reference. |
| `DELETE /api/v1/alert-rules/{id}/baseline` | write | Remove it. |
| `GET /api/v1/shape-reference` | read | Reference and current distribution for a shape rule. |
| `GET /api/v1/silences` | read | Silences and maintenance windows. |
| `POST /api/v1/silences` | write | Create one. |
| `DELETE /api/v1/silences/{id}` | write | Lift one early. |
| `PATCH /api/v1/agents/{id}` | admin | Rename an agent, or enable or disable it. |
| `DELETE /api/v1/agents/{id}` | admin | Remove an agent. |
| `GET /api/v1/agent-tokens` | admin | List enrolment tokens. |
| `POST /api/v1/agent-tokens` | admin | Mint an enrolment token. |
| `DELETE /api/v1/agent-tokens/{id}` | admin | Revoke one. |
| `GET /api/v1/config` | admin | Export the tree as TOML. |
| `PUT /api/v1/config` | admin | Import TOML (`?allow_unknown_agents=1`). Never prunes; use the CLI for that. |
| `GET /api/v1/grants` | admin | List grants. |
| `POST /api/v1/grants` | admin | Create or update a grant. |
| `DELETE /api/v1/grants/{id}` | admin | Remove a grant. |
| `POST /api/v1/agent/enrol` | signed | Agent enrolment. |
| `POST /api/v1/ingest` | signed | Agent uploads measurements. |
| `GET /api/v1/agent/targets` | signed | Agent fetches its assignments. |
| `POST /api/v1/agent/paths` | signed | Agent submits path changes (JSON); see [Agents](agents.md#how-the-protocol-works). |

The last three rows are the agent protocol, described in [agents.md](agents.md). With OIDC on,
`/auth/login`, `/auth/callback` and `/auth/logout` exist as well.

Errors are `{"error": "..."}` with the HTTP status. With OIDC on, an unauthenticated call gets
`401` and a `login` field. A target outside the caller's scope reads as `404`.

## Query parameters

`measurements` and `paths` take `target_id` (required), `agent_id` (default `0`, the master's own
prober), `from` and `to`. `availability` takes the same, plus `down_threshold`.

| Parameter | Default | Meaning |
| --- | --- | --- |
| `target_id` | required | Target id, from `/api/v1/targets`. |
| `agent_id` | `0`; for availability, every agent the target is assigned to | Vantage point. |
| `from`, `to` | `to` = now, `from` = `to` minus 3600 | Unix seconds. |
| `down_threshold` | `100` | Availability only: the loss percent at which an interval counts as down. |
| `columns` | none | Measurements only: the width of the plot the read is for, 1 to 8192. See [pooled measurements](#pooled-measurements). |

Without `columns`, a `measurements` request is refused with `400` when the window could hold more
than 200,000 intervals. The check counts one interval per second whatever the target's interval, so
a window over about 55 hours is refused even for a 60 s series. Ask for shorter windows and join
them, or pass `columns`.

## Shapes

### Measurements

An Arrow IPC stream (`application/vnd.apache.arrow.stream`), not JSON.

| Column | Type | Meaning |
| --- | --- | --- |
| `ts` | timestamp (s, UTC) | Interval start. |
| `sent`, `received` | uint16 | Packets sent and answered. |
| `flags` | uint8 | Quality bits: 0 userspace TX, 1 userspace RX, 2 raw socket, 3 socket overflow, 4 ICMP error, 5 clock step, 6 send failed, 7 truncated. |
| `samples` | list of uint32 | Round-trip times in microseconds, sorted ascending. |
| `icmp_error` | uint16, nullable | ICMP type and code (`type<<8 \| code`) behind the failures. |
| `send_error` | uint8, nullable | Why the local side could not send. |
| `ipdv_send`, `ipdv_receive`, `server_processing` | list of int32, nullable | irtt series. Null means not measured, not zero. |

```bash
curl -m30 -sf "http://localhost:8080/api/v1/measurements?target_id=7&from=1793000000&to=1793003600" \
  -o series.arrow
```

Read it with any Arrow library, for example `pyarrow.ipc.open_stream(open("series.arrow", "rb"))`.

### Pooled measurements

With `columns=N`, a window that holds at most N intervals comes back exactly as above. A window
that holds more, or whose samples are more than one raw read returns, comes back pooled: the window is cut into N equal columns, every interval goes to
the column its `ts` falls in, and each column that holds any interval is one row. A column with
none is absent, not an empty row. Tell the two shapes apart by the fields: only the pooled one has
`rounds`.

| Column | Type | Meaning |
| --- | --- | --- |
| `ts`, `ts_end` | timestamp (s, UTC) | The column, `[ts, ts_end)`. |
| `rounds` | uint32 | Intervals stored in the column. Against the target's interval, this is coverage. |
| `sent`, `received` | uint64 | Packets sent and answered, summed over the column. Loss is `1 - received/sent`. |
| `flags` | uint8 | Every quality bit any interval in the column carried. |
| `flagged` | uint32 | How many intervals carried one. |
| `quantiles` | list of uint32 | The pooled round-trip times in microseconds, sorted ascending: all of them when there are at most 256, otherwise 256 order statistics at evenly spaced ranks, each standing for an equal share of the replies. Every value is a measured sample. |
| `ipdv_send`, `ipdv_receive`, `server_processing` | list of int32, nullable | The irtt series, pooled the same way. Null where no interval in the column measured the series; empty where intervals measured it with nothing to give. |

Nothing is averaged and nothing is stored: the column is pooled from the stored intervals when
it is read. Per-interval details (`icmp_error`, `send_error`) are not in a pooled read. A read is
refused with `400` when it would walk more than 10 million rows — intervals plus their irtt series
rows; thirty days of a one-second series — or one column would pool more than 20 million samples;
ask for a shorter window, or more columns.

### Targets

```json
{
  "targets": [
    {
      "id": 7, "parent_id": 4, "name": "gateway", "path": "/Home/gateway",
      "host": "192.168.1.1", "address_family": null, "title": null, "notes": null,
      "hidden": false, "enabled": true, "sort_order": 0, "is_group": false,
      "settings": {
        "interval_s": {"local": null, "effective": 60,
                       "source": {"id": 4, "name": "Home", "path": "/Home"}},
        "pings_per_interval": {"local": 20, "effective": 20, "source": "local"}
      }
    }
  ]
}
```

`settings` holds every inheritable setting (`interval_s`, `pings_per_interval`, `probe_type`,
`agents`, `retention_s` and more) as `local`, `effective` and `source`. `source` is `"local"`,
the ancestor that provides the value, or `"outside"` when that ancestor is above the caller's
scope. A group has `is_group: true` and a null `host`.

### Availability

```json
{
  "target": "/Home/gateway", "target_id": 7, "from": 1793000000, "to": 1793086400,
  "interval_s": 60, "down_threshold_pct": 100,
  "agents": [
    {
      "agent_id": 0, "agent": "local",
      "report": {
        "from": 1793000000, "to": 1793086400, "interval_s": 60, "down_threshold_pct": 100,
        "window_s": 86400, "covered_s": 86400, "up_s": 86280, "down_s": 120, "unknown_s": 0,
        "up_intervals": 1438, "down_intervals": 2, "has_data": true,
        "availability": 0.9986, "coverage": 1,
        "downtime": [
          {"start_ts": 1793040000, "end_ts": 1793040120, "duration_s": 120, "intervals": 2}
        ]
      }
    }
  ]
}
```

`availability` is computed over the intervals that have data. `coverage` is the share of the
window that had data. Read the two together.

### Firing alerts

`GET /api/v1/alerts`:

```json
{
  "alerts": [
    {
      "rule": "loss", "metric": "loss", "target": "/Home/gateway", "host": "192.168.1.1",
      "agent": "local", "value": 35, "describes": "loss > 20% for 3 intervals",
      "rule_id": 2, "target_id": 7, "agent_id": 0,
      "acked": false, "silenced": false, "since": 1793040000
    }
  ],
  "enabled": true,
  "delivering": true
}
```

`acked_by`, `acked_at` and `silenced_until` appear when they apply. `delivering` says a webhook
is configured, not that deliveries succeed. To acknowledge, `POST /api/v1/alerts/ack` with
`{"rule_id": 2, "target_id": 7, "agent_id": 0}`; add `"ack": false` to clear it.

### Alert events

`GET /api/v1/alert-events?limit=50`:

```json
{
  "events": [
    {"id": 311, "ts": 1793040000, "firing": true, "rule": "loss",
     "describes": "loss > 20% for 3 intervals", "value": 35,
     "target": "/Home/gateway", "agent_id": 0}
  ]
}
```

### Alert rules and silences

A rule's JSON uses the TOML keys: `name`, `metric`, `op`, `threshold`, `for_intervals`,
`clear_intervals`, `mode`, `baseline` and `severity`, plus `target_id`. On the wire an on/off switch is
`enabled`, where TOML has `disabled`. See [alerting.md](alerting.md).

A silence takes `target_id`, `agent_id` and `rule_id` (all optional; null means all), a
`reason`, and either `duration_s` or `starts_at` and `ends_at` (Unix seconds). It returns
`{"id": N}`.

## Webhook payload

With `--alert-webhook` or `--alert-webhook-file`, smokeng POSTs a JSON array in Alertmanager's v2
format with `Content-Type: application/json`. A request times out after 10 seconds, and a status
of 300 or above counts as a failure. How to set it up is in
[alerting.md](alerting.md#delivery).

Each element of the array:

| Field | Value |
| --- | --- |
| `labels.alertname` | The rule name. |
| `labels.target` | The full target path, such as `/Home/gateway`. |
| `labels.host` | The target's host. |
| `labels.agent` | The vantage point that saw it. `local` is the master's own prober. |
| `labels.metric` | The rule's metric: `loss`, `median`, `p95`, `spread`, `shape` or `bimodality`. |
| `labels.severity` | The rule's `severity`: `critical`, `warning` (the default) or `info`. |
| `annotations.summary` | `<rule> on <target>: <metric> is <value><unit>`. The unit is `%` for loss and `ms` for every other metric. |
| `annotations.description` | `Rule "<name>" (<condition>) has been satisfied for <target>.` |
| `startsAt` | RFC 3339, UTC. When the condition started holding. Omitted when unknown. |
| `endsAt` | RFC 3339, UTC. Only on a resolved alert; a firing alert has none. |
| `generatorURL` | Never filled in by smokeng. |

A firing alert is sent again every `--alert-repeat` (default one minute). A resolved alert is
sent once.

```json
[
  {
    "labels": {
      "alertname": "loss", "target": "/Home/gateway", "host": "192.168.1.1",
      "agent": "local", "metric": "loss", "severity": "warning"
    },
    "annotations": {
      "summary": "loss on /Home/gateway: loss is 35%",
      "description": "Rule \"loss\" (loss > 20% for 3 intervals) has been satisfied for /Home/gateway."
    },
    "startsAt": "2026-11-05T14:00:00Z"
  }
]
```

For `shape` and `bimodality` rules the summary still says `ms`, though the value is a z-score or
a coefficient. The `description` carries the real condition.
