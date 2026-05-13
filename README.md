# fub-receiver

A small Go service that receives [Follow Up Boss](https://www.followupboss.com/) webhooks, verifies their signature, and republishes them to a Google Cloud Pub/Sub topic.

## Why

FUB sends a webhook per event. If you want more than one thing to happen on that event (sync to another CRM, kick off an automation, log to BigQuery, etc.), a single webhook endpoint becomes a bottleneck: it can only do one job, and if that job is slow or down, you've lost the event. FUB only retries briefly.

This service is a thin fan-out layer. It validates the FUB signature once at the edge and hands the payload to Pub/Sub, where any number of subscribers can consume it independently and durably.

## Flow

```
FUB -> fub-receiver -> Pub/Sub topic -> subscribers
```

The service verifies the `FUB-Signature` header (HMAC-SHA256 of the base64-encoded body, hex-encoded), wraps the body with the request's URL query string args, and publishes to the configured topic.

## Configuration

Environment variables read at startup:

| Variable       | Required | Description                                                 |
| -------------- | -------- | ----------------------------------------------------------- |
| `PROJECT_ID`   | yes      | GCP project hosting the Pub/Sub topic                       |
| `TOPIC`        | yes      | Pub/Sub topic ID (short name, not the full resource path)   |
| `X_SYSTEM_KEY` | yes      | Secret used to verify FUB webhook signatures (see below)    |
| `PORT`         | no       | HTTP port; defaults to `8080` (Cloud Run sets this for you) |

### X_SYSTEM_KEY

When you register a webhook with FUB, you supply an `X-System-Key` header. FUB then uses that value to HMAC-sign every webhook it sends to your URL. The service validates incoming webhooks against the `X_SYSTEM_KEY` env var, so the two values must match exactly.

**One key per running service.** All FUB accounts feeding into the same receiver must register their webhooks with the same `X-System-Key`. Webhooks signed with anything else are rejected.

Store the value in Secret Manager and bind it to the Cloud Run service. Do not put it in plain environment config.

## Deployment

Push to `main` triggers Cloud Build ([cloudbuild.yaml](cloudbuild.yaml)):

1. Runs the same checks as local CI (fmt, vet, lint, test, vuln).
2. Builds and pushes a container image to Artifact Registry at `us-central1-docker.pkg.dev/$PROJECT_ID/containers/fub-receiver`.
3. Updates the `fub-receiver` Cloud Run service in `us-central1`. The new revision is suffixed with the commit short SHA so rollback in the Cloud Run console is unambiguous.

The Cloud Build trigger itself lives outside this repo (GCP console / Terraform / etc.).

Pull requests run the same check suite via GitHub Actions ([.github/workflows/ci.yml](.github/workflows/ci.yml)) before merge. Dependabot opens PRs weekly for Go modules, GitHub Actions versions, and Docker base images.

## Registering webhooks with FUB

Once the service is deployed, register the events you want against the FUB API. The `X-System-Key` header you send must match the running service's `X_SYSTEM_KEY` env var (see [Configuration](#configuration)).

FUB authenticates with HTTP Basic, using your FUB API key as the username and an empty password.

```sh
curl -X POST https://api.followupboss.com/v1/webhooks \
  -u 'YOUR_FUB_API_KEY:' \
  -H 'Content-Type: application/json' \
  -H 'X-System: your-integration-name' \
  -H 'X-System-Key: SAME_VALUE_AS_X_SYSTEM_KEY_ENV_VAR' \
  -d '{
    "event": "peopleCreated",
    "url": "https://fub-receiver-XXXXXXX-uc.a.run.app/"
  }'
```

Register one POST per event you want (`peopleCreated`, `peopleUpdated`, `peopleTagsCreated`, `dealsUpdated`, etc.). The full event list is in the [FUB API docs](https://docs.followupboss.com/).

To list current webhooks: `GET /v1/webhooks`. To remove one: `DELETE /v1/webhooks/{id}`.

### Passing context via webhook args

The URL you register can carry an arbitrary query string. `fub-receiver` doesn't interpret it; it just extracts the parameters into `webhookArgs` on the published Pub/Sub message verbatim. Use whatever keys make sense for your downstream.

A common use is distinguishing multiple FUB accounts feeding into one receiver. Pick any key name (`account`, `tenant`, `client`, `org`, whatever fits) and register each account against a URL that includes it:

```
https://fub-receiver-XXXXXXX-uc.a.run.app/?account=alpha
https://fub-receiver-XXXXXXX-uc.a.run.app/?account=beta
```

Subscribers read the corresponding key from `webhookArgs` and route accordingly. Without something like this you can't tell the accounts apart, because the IDs in the webhook body are scoped to a single FUB account.

You can pass anything else here too: a routing hint for downstream subscribers, a feature flag, an environment tag (`env=staging`), etc. The query string is part of the registered webhook URL and is visible to anyone who can read your FUB account's webhook list, so don't put secrets in it.

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
2. **Distinguish transient from permanent at the subscriber.** A 404 for an event with a recent `eventCreated` is probably transient: NACK. A 404 for an event hours or days old is probably permanent: ACK and log the discard so it doesn't keep looping. The cutoff depends on FUB's worst-case propagation lag; a few minutes is usually enough.

**Be idempotent.** Pub/Sub can deliver the same message twice (after a NACK, subscriber crash, or ack-deadline timeout). Subscribers should track the latest `eventCreated` per entity and drop any incoming event whose timestamp is not newer than what's already applied. This also means stale events that slip through due to out-of-order delivery don't corrupt state.

## Development

```sh
make tools   # install golangci-lint and govulncheck (one time)
make ci      # full check suite: fmt, vet, lint, test, vuln
make test    # tests only, with race detector and shuffled order
```

Tests use the in-memory `pstest` Pub/Sub fake. No GCP credentials needed to run them.
