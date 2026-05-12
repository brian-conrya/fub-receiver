# fub-receiver

A small Go service that receives [Follow Up Boss](https://www.followupboss.com/) webhooks, verifies their signature, and republishes them to a Google Cloud Pub/Sub topic.

## Why

FUB sends a webhook per event. If you want more than one thing to happen on that event (sync to another CRM, kick off an automation, log to BigQuery, etc.), a single webhook endpoint becomes a bottleneck: it can only do one job, and if that job is slow or down, you've lost the event.

This service is a thin fan-out layer. It validates the signature once at the edge and hands the payload to Pub/Sub, where any number of subscribers can consume it independently.

Concretely you get:

- **Durability.** Pub/Sub holds messages and retries delivery if a subscriber is unhealthy. FUB only retries briefly.
- **Decoupling.** Downstream consumers never see FUB's signing scheme; they just read from Pub/Sub.
- **Cheap.** Cloud Run scales to zero. At normal FUB volumes the cost is extremely low.

## Flow

```
FUB -> fub-receiver -> Pub/Sub topic -> subscribers
```

1. Verify the `FUB-Signature` header (HMAC-SHA256 of the base64-encoded body, hex-encoded, keyed with `X_SYSTEM_KEY`).
2. Wrap the webhook body together with the URL query string args.
3. Publish to the configured topic, with the FUB `event` field copied to a Pub/Sub message attribute so subscribers can filter on it.

## Configuration

Environment variables read at startup:

| Variable       | Required | Description                                                 |
| -------------- | -------- | ----------------------------------------------------------- |
| `PROJECT_ID`   | yes      | GCP project hosting the Pub/Sub topic                       |
| `TOPIC`        | yes      | Pub/Sub topic ID (short name, not the full resource path)   |
| `X_SYSTEM_KEY` | yes      | FUB X-System-Key used to verify webhook signatures          |
| `PORT`         | no       | HTTP port; defaults to `8080` (Cloud Run sets this for you) |

Use Secret Manager + Cloud Run secret bindings for `X_SYSTEM_KEY`. Do not put it in plain environment config.

## Published message shape

Each Pub/Sub message has:

**Data** (UTF-8 JSON):

```json
{
  "webhookData": { "...": "verbatim FUB webhook body" },
  "webhookArgs": { "param1": "value1" }
}
```

**Attributes:** a single `event` attribute, set to the `event` field from the webhook body when present (e.g. `peopleCreated`). Subscribers can filter on it directly: `attributes.event = "peopleCreated"`.

## Subscriber design

Pub/Sub is at-least-once and not strictly ordered. The patterns below handle the failure modes that actually come up; you don't need to add ordering on the publish side.

**Events arrive out of order.** A `peopleTagsUpdated` can land at a subscriber before its matching `peopleCreated` for one of three reasons: FUB sometimes hits this webhook out of order (parallel POSTs, their retries); Pub/Sub doesn't preserve publish order without ordering keys; FUB's own API can be briefly inconsistent after firing a webhook. The mitigation: when the subscriber fetches the entity from FUB and gets a 404, NACK the message. Pub/Sub redelivers after the ack deadline (default 10s, backoff up to ~10 min). By the time it comes back, the precondition almost always exists.

**NACK can't loop forever.** If a 404 is permanent (the person was deleted before you processed the create, the webhook payload was malformed, FUB had a bug, etc.), NACKing every time just spins the message at the max backoff interval indefinitely. Two things keep this safe:

1. **Configure a dead-letter topic** on the subscription with `deliveryAttempts = 5` (or whatever you can tolerate). After N delivery attempts, Pub/Sub routes the message to the DLQ instead of redelivering. You monitor the DLQ and triage manually.
2. **Distinguish transient from permanent at the subscriber.** A 404 for an event with a recent `eventCreated` is probably transient -> NACK. A 404 for an event hours or days old is probably permanent -> ACK with a log line, don't loop. The cutoff depends on how long FUB's worst-case propagation lag is; a few minutes is usually enough.

**Be idempotent.** Pub/Sub can deliver the same message twice (after a NACK, subscriber crash, or ack-deadline timeout). Subscribers can track the latest `eventCreated` per entity and drop any incoming event whose timestamp is not newer than what's already applied. This also means stale events that slip through due to out-of-order delivery don't corrupt state.

## Development

```sh
make tools   # install golangci-lint and govulncheck (one time)
make ci      # full check suite: fmt, vet, lint, test, vuln
make test    # tests only, with race detector and shuffled order
```

Tests use the in-memory `pstest` Pub/Sub fake. No GCP credentials needed to run them.

## Deployment

Push to `main` triggers Cloud Build ([cloudbuild.yaml](cloudbuild.yaml)), which:

1. Runs the same checks as local CI.
2. Builds and pushes a container image to Artifact Registry at `us-central1-docker.pkg.dev/$PROJECT_ID/containers/fub-receiver`.
3. Updates the `fub-receiver` Cloud Run service in `us-central1`. The new revision is suffixed with the commit short SHA so rollback in the Cloud Run console is unambiguous.

The Cloud Build trigger itself lives outside this repo (GCP console / Terraform / etc.).

## Continuous integration

Pull requests run the same checks via GitHub Actions ([.github/workflows/ci.yml](.github/workflows/ci.yml)). Dependabot opens PRs weekly for Go modules, GitHub Actions versions, and Docker base images.
