# booth-spark

Project Booth's Spark compute module: start and stop Spark sessions and submit Spark applications
on Kubernetes, through the shell (Manage nav group) and through a versioned job-submission API that
other modules could call. A standalone, optional leaf module: nothing depends on it (ADR 0006).

Architecture, contracts and decisions live in `booth-architecture`; this repo's brief is
`agent-briefs/spark.md` there. The design is `docs/design-v0.md`, ruled on as ADR 0110, and built in
the six steps listed there, one PR each.

## Status

Step 5 of 6: batch applications and interactive sessions run, each in its own namespace, through
the `/v1` API. A session takes SQL and Python statements, one at a time, and is stopped when idle
(`sessions.idleTimeout`, default 20m) or at its maximum lifetime (`sessions.maxLifetime`, default
12h). With `dataAccess.enabled`, a run reads and writes the workspace's booth-database schema, its
lakehouse warehouse (Iceberg) and booth-storage locations as its submitter, capped at editor, and an
application may start from a Python file or a JAR in booth-storage. An ended run's content
(statement results, its log) is cleared after `sessions.resultRetention` (default 7 days). Each
run's Spark UI opens in the shell for its submitter. Not yet: the module's own UI and the rest of
the operations doc (step 6). Known limits are in [docs/operations.md](docs/operations.md).

| Route | Reached through | Credential | What it does |
|---|---|---|---|
| `/livez`, `/healthz` | the kubelet, booth-core's health poll | none | liveness; readiness (the module's own database) |
| `/v1/*` | core's gateway, `/modules/spark/v1/…` | a bearer token: a person's OIDC token, or core's workload token | the job-submission API (`internal/api/openapi/v1.yaml`, also served at `/v1/openapi.yaml`) |
| everything else | core's iframe proxy, `/iframe/spark/…` | core's `X-Booth-Identity` assertion | the module's page; `/runs/<id>/ui/…`, a run's Spark UI, for its submitter only (read-only, default-deny allowlist) |

## How a run is isolated

A run is a namespace, `bspark-<id>`, holding its driver and executors, deleted when the run ends.
It is labelled for its workspace (ADR 0077), enforces Pod Security `restricted`, has a quota and a
limit range, and default-deny NetworkPolicies: its own pods talk to each other, DNS, the driver
(only the driver) to the API server, the backend's pods (only they) to the driver's UI, and, with
`runs.egress.mode=open` (the default), the internet and never a private or cluster range. Two
ValidatingAdmissionPolicies fence it: pods in a run namespace use only the allowlisted image,
carry their workspace's label and the configured nodeSelector, and run as the `executor` account
with no token unless booth-spark itself creates the driver; and the backend's own cluster-wide
rights reach only its own namespaces. Every run namespace is owned by the chart's driver
ClusterRole, so uninstalling the chart, by any route, deletes them all.

## Known residuals (stated plainly)

- **User code can reach the Kubernetes API server.** A run's driver runs the submitter's own code
  and holds a Kubernetes account, because Spark asks the API for its executors (ADR 0110 ruling 3,
  a documented departure from ADR 0057). That account may only manage pods and config maps in the
  run's own namespace, and the admission policy limits what pods it can create, but the API server
  itself is reachable from user code: an API-server vulnerability would be within its reach.
- **Open egress.** With `runs.egress.mode=open` (the default, the user's choice in ADR 0110), a
  run's code can send anything it can read to any internet host. Set `runs.egress.mode=closed`, or
  `submit.minRole=owner`, where editors aren't trusted.
- **NetworkPolicy depends on the CNI.** The isolation above needs a CNI that enforces NetworkPolicy,
  egress included. k3s's does. kind's default (kindnet) enforces ingress but not egress, which is why
  Integration runs kind with Calico.
- **A run's code can spend its quota** on pods running the allowlisted image in its own namespace;
  that is no more than it can already do itself.

## Configuration

`charts/booth-spark/values.yaml` documents every value. The identity settings are the fleet's:
`oidc.issuerUrl`, `oidc.clientId`, `oidc.requireAudience`, `oidc.groupsClaim`, `oidc.jwksUrl`
(ADR 0108), `oidc.workloadIssuerUrl` (ADR 0056), and `identity.issuerUrl` (core's iframe-identity
issuer, ADR 0069). `submit.minRole` (`editor` or `owner`) is the submit floor (ADR 0110). `runs.*`
sizes, places and fences the runs. The chart requires Kubernetes 1.30 or later, with
`ValidatingAdmissionPolicy` served.

## Development

```sh
docker compose -f hack/docker-compose.emulators.yml up -d --wait
eval "$(sh hack/test-env.sh)"
go test ./...                      # unit + contract (contract needs helm)
```

CI (`.github/workflows/ci.yml`) runs those, and lints the OpenAPI document, on every push and PR.
Integration (`.github/workflows/integration.yml`, `test/integration/README.md`) runs on kind against
stand-ins and against a real booth-core and Keycloak (with Calico), on PRs, on `main`, nightly and
by hand.
