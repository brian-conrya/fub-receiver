# fub-receiver

A small Go service that receives [Follow Up Boss](https://www.followupboss.com/) webhooks, verifies their signature, and republishes them to a Google Cloud Pub/Sub topic.

Not affiliated with or endorsed by Follow Up Boss.

## Why

FUB sends a webhook per event. If you want more than one thing to happen on that event (sync to another CRM, kick off an automation, log to BigQuery, etc.), a single webhook endpoint becomes a bottleneck: it can only do one job, and if that job is slow or down, you've lost the event. FUB only retries briefly.

This service is a thin fan-out layer. It validates the FUB signature once at the edge and hands the payload to Pub/Sub, where any number of subscribers can consume it independently and durably.

## Flow

```
FUB -> fub-receiver -> Pub/Sub topic -> subscribers
```

The service verifies the `FUB-Signature` header (HMAC-SHA256 of the base64-encoded body, hex-encoded), splits the webhook into one message per resource, wraps each with the request's URL query string args, and publishes them to the configured topic.

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

## First-time setup

The steps below create everything the service needs in a GCP project and do the first deploy. They match the names [cloudbuild.yaml](cloudbuild.yaml) expects: an Artifact Registry repository called `containers` and a Cloud Run service called `fub-receiver`, both in `us-central1`.

```sh
export PROJECT_ID=your-project-id
export REGION=us-central1
export TOPIC=fub-events
export SA=fub-receiver@$PROJECT_ID.iam.gserviceaccount.com
gcloud config set project $PROJECT_ID

gcloud services enable run.googleapis.com pubsub.googleapis.com \
  secretmanager.googleapis.com artifactregistry.googleapis.com cloudbuild.googleapis.com

# Topic, image repository, and the identity the service runs as
gcloud pubsub topics create $TOPIC
gcloud artifacts repositories create containers --repository-format=docker --location=$REGION
gcloud iam service-accounts create fub-receiver
gcloud pubsub topics add-iam-policy-binding $TOPIC \
  --member=serviceAccount:$SA --role=roles/pubsub.publisher

# The shared X-System-Key, read from the terminal so it stays out of shell history
read -rs KEY && printf %s "$KEY" | gcloud secrets create fub-x-system-key --data-file=-
gcloud secrets add-iam-policy-binding fub-x-system-key \
  --member=serviceAccount:$SA --role=roles/secretmanager.secretAccessor

# First deploy, built from source with the Dockerfile
gcloud run deploy fub-receiver --source . --region=$REGION \
  --service-account=$SA \
  --allow-unauthenticated \
  --set-env-vars=PROJECT_ID=$PROJECT_ID,TOPIC=$TOPIC \
  --set-secrets=X_SYSTEM_KEY=fub-x-system-key:latest
```

`--allow-unauthenticated` is required: FUB can't present Google credentials, so the endpoint has to be public. The HMAC signature check is what keeps it closed. If an organization policy blocks public Cloud Run services, it needs an exception for this one.

The deploy prints the service URL. That's the URL you register with FUB (see below).

### Continuous deployment

Connect the GitHub repository to Cloud Build and create a trigger that runs [cloudbuild.yaml](cloudbuild.yaml) on **pushes to `main` only**. Don't trigger on pull requests: the build deploys with your project's permissions, so a PR trigger would let anyone who opens a PR run code in your project.

The service account the trigger runs as needs:

- `roles/artifactregistry.writer` to push the image
- `roles/run.developer` to update the service
- `roles/iam.serviceAccountUser` on `fub-receiver@...` to deploy a service that runs as it
- `roles/logging.logWriter` because the build logs to Cloud Logging only

## Deployment

Push to `main` triggers Cloud Build ([cloudbuild.yaml](cloudbuild.yaml)):

1. Runs the same checks as local CI (fmt, vet, lint, test, vuln).
2. Builds and pushes a container image to Artifact Registry at `us-central1-docker.pkg.dev/$PROJECT_ID/containers/fub-receiver`.
3. Updates the `fub-receiver` Cloud Run service in `us-central1`. The new revision is suffixed with the commit short SHA so rollback in the Cloud Run console is unambiguous, plus a fragment of the build ID so re-running a build for the same commit doesn't collide with the revision it already created.

The update keeps the service's existing environment, secret bindings, and service account, so those only need setting once (see [First-time setup](#first-time-setup)).

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
    "url": "https://fub-receiver-XXXXXXX-uc.a.run.app/?account=alpha"
  }'
```

Register one POST per event you want (`peopleCreated`, `peopleUpdated`, `peopleTagsCreated`, `dealsUpdated`, etc.). The full event list is in the [FUB API docs](https://docs.followupboss.com/).

To list current webhooks: `GET /v1/webhooks`. To remove one: `DELETE /v1/webhooks/{id}`.

### The `account` parameter

Always register webhook URLs with an `account` query parameter naming the FUB account, even if you only have one:

```
https://fub-receiver-XXXXXXX-uc.a.run.app/?account=alpha
https://fub-receiver-XXXXXXX-uc.a.run.app/?account=beta
```

The name `account` is fixed. The service builds each message's ordering key from it (see [Subscriber design](#subscriber-design)), and IDs in the webhook body are only unique within one FUB account, so this is also how subscribers tell accounts apart. A webhook without `account` still publishes, but it logs a warning, and its ordering keys collide with those of any other account that's missing one.

### Other webhook args

Any other query parameters are passed through into `webhookArgs` on the published message without interpretation: a routing hint for downstream subscribers, a feature flag, an environment tag (`env=staging`), etc. If a key repeats, only its first value is kept.

The query string is part of the registered webhook URL and is visible to anyone who can read your FUB account's webhook list, so don't put secrets in it.

## HTTP responses

The service accepts `POST` on any path and replies with JSON:

| Status | Meaning                                                     |
| ------ | ----------------------------------------------------------- |
| `200`  | Every message was published. Body is `{}`.                  |
| `400`  | Missing or invalid `FUB-Signature`, or the body isn't JSON. |
| `413`  | Body is larger than 1 MiB.                                  |
| `500`  | Publishing to Pub/Sub failed.                               |

Error bodies look like `{"message":"Invalid FUB-Signature header"}`. A `500` is the only response worth FUB retrying; the others will fail the same way every time.

## Published message shape

Each Pub/Sub message has:

**Data** (UTF-8 JSON):

```json
{
  "webhookData": {
    "event": "peopleUpdated",
    "resourceIds": ["123"],
    "...": "remaining FUB webhook fields, unchanged"
  },
  "webhookArgs": { "account": "alpha" }
}
```

`webhookData` is the FUB webhook body with one change: a webhook naming several resources is split into one message per resource, so `resourceIds` always holds exactly one id. **That id is a string**, even though FUB sends it as a number. Events with no `resourceIds` are published once with the body unchanged.

**Attributes:** a single `event` attribute, set to the `event` field from the webhook body when present (e.g. `peopleCreated`). Subscribers can filter on it directly: `attributes.event = "peopleCreated"`.

**Ordering key:** `account:resourceId` (e.g. `alpha:123`) when the message names a resource, and empty otherwise.

## Subscriber design

Pub/Sub is at-least-once. The patterns below handle the failure modes that actually come up.

**One message per resource, keyed by person.** A webhook naming N resources is fanned out into N messages, each carrying a single-element `resourceIds` and an ordering key of `account:personId`. A subscriber that enables ordering therefore sees one person's events strictly in sequence, while still processing different people concurrently. The key includes the account because person ids are only unique within one. Events that name no resource are published once with no ordering key.

Enabling ordering is the *subscriber's* choice: `OrderingKey` is ignored by subscriptions that don't have `enableMessageOrdering` set, so subscriptions that want unordered delivery are unaffected and need no change. Note that `enableMessageOrdering` is immutable: turning it on means creating a new subscription, not updating the existing one.

**Ordering does not give you mutual exclusion.** Pub/Sub delivers one message at a time per key only in the happy path. On ack-deadline expiry it redelivers while the original delivery may still be in flight, so two workers can still process one person at once. Ordering buys sequencing; excluding concurrent work needs a lock the subscriber holds itself.

**With ordering enabled, a dead-letter topic stops being optional.** A message that keeps failing blocks every later message for its key, so one poison message wedges that person indefinitely rather than merely being slow.

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

## License

[MIT](LICENSE)
