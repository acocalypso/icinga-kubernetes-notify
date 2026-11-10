# icinga-kubernetes-notify

Sends notifications when something in Kubernetes breaks, using the health states that
[Icinga for Kubernetes](https://icinga.com/docs/icinga-for-kubernetes/latest/) already computes.

Icinga for Kubernetes shows broken deployments, nodes and jobs in Icinga Web, but version
0.4.0 cannot deliver alerts to Icinga Notifications 1.0 (its events are rejected with
`404 Not Found`). This service closes that gap. It watches the Icinga for Kubernetes
database and reports problems and recoveries to
[Icinga Notifications](https://icinga.com/docs/icinga-notifications/latest/), which then
delivers them through its channels: webhook, email or Telegram.

```
Kubernetes ──► Icinga for Kubernetes ──► database ──► icinga-kubernetes-notify
                                                              │ problems / recoveries
                                                              ▼
                              webhook ◄── channels ◄── Icinga Notifications
```

**What is sent and where** is configured in Icinga Web, not in this service: the payload
template, headers and URL belong to the channel, and event rules decide which cluster or
namespace goes to which destination.

## What it does

- Reports a problem when a deployment, stateful set, daemon set, node, job or cron job
  (optionally replica sets and pods) is `critical` or `unknown` in Icinga for Kubernetes.
  The mapping from state to severity is configurable.
- Reports the recovery on the same incident when the object is healthy again, or when it
  was deleted.
- Waits before reporting (`for`, default 2 minutes) so that rollouts and crash loops that
  alternate between ok and critical do not page.
- Raises a problem for the cluster itself when the Icinga for Kubernetes daemon stops
  writing or cannot reach the Kubernetes API, and freezes all object states meanwhile.
  Stale data never produces a recovery.
- Survives restarts of itself and of the Icinga for Kubernetes daemon without duplicate
  or lost notifications. It keeps no state of its own; open incidents are read back from
  Icinga Notifications at startup.
- Retries events that Icinga Notifications did not accept.
- Only reads. It needs `SELECT` on the Icinga for Kubernetes database and no access to
  Kubernetes.

## Requirements

Tested with:

| Component | Version |
|---|---|
| Icinga for Kubernetes | 0.4.0 |
| Icinga Notifications / Icinga Notifications Web | 1.0.0 |
| Icinga Web | 2.14.1 |
| MariaDB | 11.4 |
| Kubernetes | v1.37.0 |

## Setup

### 1. Database user

```sql
CREATE USER 'kubernetes_ro'@'%' IDENTIFIED BY '<password>';
GRANT SELECT ON kubernetes.* TO 'kubernetes_ro'@'%';
```

### 2. Source in Icinga Notifications

In Icinga Web: `Configuration → Modules → notifications → Sources → New Source`.

| Field | Value |
|---|---|
| Source Name | e.g. `Kubernetes notify bridge` |
| Source Type | `kubernetes-bridge` |
| Authentication | Username and password (password at least 16 characters) |

Event rules are bound to the source type, so keep `kubernetes-bridge` stable.

### 3. Configuration

Copy [config.example.yml](config.example.yml) and adjust it. The minimum is:

```yaml
database:
  host: mysql.example.com
  user: kubernetes_ro
  password_file: /run/secrets/kubernetes-db-password
notifications:
  url: https://notifications.example.com:5680
  username: kubernetes-notify
  password_file: /run/secrets/notifications-source-password
  icingaweb_url: https://icinga.example.com/icingaweb2
```

| Setting | Default | Meaning |
|---|---|---|
| `watch.interval` | `15s` | How often the database is read |
| `watch.kinds` | deployment, statefulset, daemonset, node, job, cronjob | Also available: `replicaset`, `pod` |
| `watch.severity` | `critical: critical`, `unknown: error` | Icinga for Kubernetes state → notification severity (`warning`, `error`, `critical`). States not listed raise nothing. |
| `watch.clusters` | all | Cluster names to report |
| `watch.namespaces`, `watch.exclude_namespaces` | all, none | Glob patterns |
| `watch.for` | `2m` | A problem must last this long before it is reported |
| `watch.recover_after` | `1m` | An object must be healthy this long before recovery is reported |
| `watch.gone_after` | `5m` | A deleted object's open problem is closed after this time |
| `watch.daemon_stale_after` | `2m` | Heartbeat age after which the cluster is reported and its objects are frozen |
| `listen` | `:8080` | `/healthz` and `/metrics`; empty disables |

Lists and maps in the file replace the defaults. Unknown keys are rejected, so a typo does
not silently fall back to a default. Secrets can be given inline, as `password_file`, or
through the environment (`IKN_DATABASE_PASSWORD`, `IKN_NOTIFICATIONS_PASSWORD`,
`IKN_DATABASE_PASSWORD_FILE`, `IKN_NOTIFICATIONS_PASSWORD_FILE`).

Validate a file without starting the service:

```bash
icinga-kubernetes-notify --config config.yml --check-config
```

### 4. Run

```bash
docker build -t icinga-kubernetes-notify .
docker run -d --name icinga-kubernetes-notify \
  -v "$PWD/config.yml:/etc/icinga-kubernetes-notify/config.yml:ro" \
  -p 127.0.0.1:8080:8080 icinga-kubernetes-notify
```

A Compose example is in [examples/docker-compose.yml](examples/docker-compose.yml).
Run exactly one instance per Icinga Notifications installation.

## Configuring what is sent and where

All of this happens in Icinga Web under `Notifications`.

### Where: a webhook channel

`Configuration → Channels → Add Channel`, type **Webhook**.

| Option | Example |
|---|---|
| URL template | `https://hooks.example.com/kubernetes` |
| Method | `POST` |
| Request headers | `Content-Type: application/json` and, on a new line, `Authorization: Bearer <token>` |
| Request body template | see below |
| Response status codes | `200,202,204` |
| TLS options | custom CA, client certificate, server name |

Then create a contact that uses the channel, and an event rule for source type
`kubernetes-bridge` with that contact as recipient.

### What: the body template

The body is a Go template. [examples/webhook-body.tmpl](examples/webhook-body.tmpl) produces:

```json
{
  "version": "1",
  "event": "problem",
  "severity": "critical",
  "cluster": "kib-alpha",
  "namespace": "shop",
  "kind": "deployment",
  "name": "web",
  "summary": "Deployment shop/web has 1 unavailable replicas.",
  "details": "Deployment shop/web has 1 unavailable replicas.",
  "incident_started_at": "2026-11-10T10:35:41.655Z",
  "event_time": "2026-11-10T10:35:41.655Z",
  "url": "http://localhost:18080/kubernetes/deployment?id=34e3cb72-a1fb-473e-8ae5-d05d827bc8aa"
}
```

Fields available in templates (also usable in the URL and header templates):

| Field | Content |
|---|---|
| `.Object.Name` | e.g. `kib-alpha: deployment shop/web` |
| `.Object.Url` | Link to the object in Icinga for Kubernetes Web |
| `index .Object.Tags "cluster"` | Also `namespace`, `kind`, `name`. Nodes and clusters have no `namespace`. |
| `.Incident.Severity` | `warning`, `error`, `critical`, or `ok` on recovery |
| `.Incident.IsRecovered` | `true` for the recovery notification |
| `.Incident.StartedAt`, `.Incident.RecoveredAt` | Timestamps |
| `.Incident.Summary`, `.Incident.Message` | Latest state text |
| `.Event.Summary`, `.Event.Message`, `.Event.Time` | The event that triggered this notification |
| `.Contact.FullName` | Recipient |

Use `{{json <value>}}` so values are quoted and escaped correctly. Cluster, kind,
namespace, name and `.Incident.StartedAt` together identify an incident; the problem and
its recovery carry the same values. Leaving the template empty sends the complete
notification as JSON.

### Routing: event rules

An event rule without a filter receives everything from the source. To route by cluster,
namespace, kind or name, set a filter on these columns:

| Column | Example |
|---|---|
| `object.cluster` | `object.cluster=prod-eu` |
| `object.namespace` | `object.namespace=shop` |
| `object.kind` | `object.kind=node` |
| `object.name` | `object.name=web` |

One rule per destination: for example a catch-all rule to the platform webhook, and a
second rule `object.namespace=shop` to the shop team's webhook. A problem in `shop` then
reaches both. Escalations, schedules, and who is notified when are standard Icinga
Notifications features.

## Events

| Situation | Event |
|---|---|
| Object in a mapped state for longer than `for` | Problem with the mapped severity |
| State changes to another mapped severity | Severity change on the same incident |
| Object healthy (or in an unmapped state) for `recover_after` | Recovery |
| Object deleted while it had a problem | Recovery after `gone_after`: "… no longer exists" |
| Object `pending` | No change; open problems stay open |
| Daemon heartbeat older than `daemon_stale_after` | Problem for the cluster (`kind` = `cluster`); objects frozen |
| Kubernetes API unreachable for the daemon | Problem for the cluster; objects frozen |

## Monitoring the service

`GET /healthz` returns 503 when no poll has succeeded recently. `GET /metrics` exposes
`ikn_last_successful_poll_timestamp_seconds`, `ikn_poll_errors_total`,
`ikn_submit_errors_total`, `ikn_events_submitted_total` and `ikn_open_problems`.

If this service itself is down, nothing is reported. Check `/healthz` from your monitoring.

## Limitations

- It can only report what Icinga for Kubernetes evaluates. Persistent volume claims have
  no state there, and resource quotas, storage usage, capacity and OpenShift resources are
  not covered at all.
- A deployment blocked by a quota is reported as "unavailable replicas"; the reason is only
  visible in the Kubernetes events.
- Delivery to the webhook is done by Icinga Notifications. Its webhook channel makes one
  attempt per notification with a 10 second timeout. Whether the daemon retries failed
  deliveries has not been tested.
- Filters were verified as stored rule definitions; entering them through the Icinga Web
  filter editor has not been clicked through.
- Not tested: more than one cluster in the same database, TLS to the database, `ca_file`
  for an HTTPS Icinga Notifications endpoint, and OpenShift.

## Development

```bash
go vet ./...
go test ./...
```

The decision logic is in [internal/notify/tracker.go](internal/notify/tracker.go) and is
covered by unit tests without a database. The behaviour was additionally verified against
a live stack (kind cluster, Icinga for Kubernetes, Icinga Notifications and a webhook
receiver): problem, recovery, deletion, node loss, restart of this service, rebuild of the
Icinga for Kubernetes database, Icinga Notifications outage, payload template and
namespace routing.

## License

[MIT](LICENSE)
