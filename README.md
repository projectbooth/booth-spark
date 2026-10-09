# booth-spark

Project Booth's Spark compute module: start and stop Spark sessions and submit Spark applications
on Kubernetes, through the shell (Manage nav group) and through a versioned job-submission API that
other modules could call. A standalone, optional leaf module: nothing depends on it (ADR 0006).

Architecture, contracts and decisions live in `booth-architecture`; this repo's brief is
`agent-briefs/spark.md` there. The v0 design is `docs/design-v0.md`, awaiting the coordinator's
ruling; no feature code is written until then.
