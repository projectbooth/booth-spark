# booth-spark

Project Booth's Spark compute module: start and stop Spark sessions and submit Spark applications
on Kubernetes, through the shell (Manage nav group) and through a versioned job-submission API that
other modules could call. A standalone, optional leaf module: nothing depends on it (ADR 0006).

Architecture, contracts and decisions live in `booth-architecture`; this repo's brief is
`agent-briefs/spark.md` there. The design is `docs/design-v0.md`, ruled on as ADR 0110, and built in
the six steps listed there, one PR each.

## Status

Step 2 of 6: the backend with its health checks and manifest, the chart, CI, the identity code, and
the Spark UI proxy, proven against a real driver. The module doesn't start drivers yet (step 3).

| Route | Reached through | Credential | What it does now |
|---|---|---|---|
| `/livez`, `/healthz` | the kubelet, booth-core's health poll | none | liveness; readiness (the module's own database) |
| `/v1/*` | core's gateway, `/modules/spark/v1/…` | a bearer token: a person's OIDC token, or core's workload token | `GET /v1/me`: who the caller is, as this module derived it |
| everything else | core's iframe proxy, `/iframe/spark/…` | core's `X-Booth-Identity` assertion | the module's page; `GET /ui/api/me`; `/runs/<id>/ui/…`, a run's Spark UI, for its submitter only (read-only, kill and thread-dump actions refused) |

Each credential works on its own route group only (docs/design-v0.md item 2).

## Configuration

`charts/booth-spark/values.yaml` documents every value. The identity settings are the fleet's:
`oidc.issuerUrl`, `oidc.clientId`, `oidc.requireAudience`, `oidc.groupsClaim`, `oidc.jwksUrl`
(ADR 0108), `oidc.workloadIssuerUrl` (ADR 0056), and `identity.issuerUrl` (core's iframe-identity
issuer, ADR 0069). `submit.minRole` (`editor` or `owner`) is the submit floor (ADR 0110). The chart
requires Kubernetes 1.30 or later, with `ValidatingAdmissionPolicy` served.

## Development

```sh
docker compose -f hack/docker-compose.emulators.yml up -d --wait
eval "$(sh hack/test-env.sh)"
go test ./...                      # unit + contract (contract needs helm)
```

CI (`.github/workflows/ci.yml`) runs those on every push and PR. Integration
(`.github/workflows/integration.yml`, `test/integration/README.md`) runs on kind against stand-ins
and against a real booth-core and Keycloak, on PRs, on `main`, nightly and by hand.
