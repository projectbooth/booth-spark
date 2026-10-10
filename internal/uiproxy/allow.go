package uiproxy

import (
	"regexp"
	"strings"
)

// The Spark UI and REST API reachable through the proxy: a default-deny allowlist of GET paths
// (ADR 0110 step 3, requirement 1). Anything not matched here never reaches a driver. The list is
// Spark 4.1.3's own (docs/design-v0.md item 1 pins it):
//
//   - UI pages: the tabs SparkUI attaches (jobs, stages, storage, environment, executors, SQL,
//     Structured Streaming, DStreams) and their detail pages, minus the kill handlers and the
//     executor thread-dump and heap-histogram pages.
//   - Static assets under /static/.
//   - The REST API under /api/v1: every endpoint documented in
//     https://spark.apache.org/docs/4.1.3/monitoring.html#rest-api, plus the undocumented ones the UI's
//     own JavaScript calls (allmiscellaneousprocess, taskTable), read from Spark 4.1.3's
//     status/api/v1 sources. testdata/spark-4.1.3-rest-api.txt lists them all, and
//     TestAllowlist_EveryDocumentedEndpointIsDecided asserts each is allowed or refused on purpose.
//
// Refused on purpose: an executor's /threads (full stack traces, served even with
// spark.ui.threadDumpsEnabled=false), /logs and /<attempt>/logs (the event-log download), and every
// /<attempt-id>/... variant (no attempt ids exist for a live driver on Kubernetes).

// uiPages are UI page paths, matched with or without a trailing slash.
var uiPages = map[string]bool{
	"/":                          true,
	"/jobs":                      true,
	"/jobs/job":                  true,
	"/stages":                    true,
	"/stages/stage":              true,
	"/stages/pool":               true,
	"/storage":                   true,
	"/storage/rdd":               true,
	"/environment":               true,
	"/executors":                 true,
	"/SQL":                       true,
	"/SQL/execution":             true,
	"/StreamingQuery":            true,
	"/StreamingQuery/statistics": true,
	"/streaming":                 true,
	"/streaming/batch":           true,
}

var (
	staticRE = regexp.MustCompile(`^/static/[A-Za-z0-9._-]+(/[A-Za-z0-9._-]+)*$`)
	// An application id as Spark mints it ("spark-<hex>" on Kubernetes, "local-<millis>" locally).
	appID   = `[A-Za-z0-9][A-Za-z0-9._-]*`
	n       = `\d+`
	restRE  = regexp.MustCompile(`^/api/v1/(version|applications)/?$`)
	perApp  = regexp.MustCompile(`^/api/v1/applications/` + appID + `(/.*)?$`)
	appRest = []*regexp.Regexp{
		regexp.MustCompile(`^$`),
		regexp.MustCompile(`^/jobs$`),
		regexp.MustCompile(`^/jobs/` + n + `$`),
		regexp.MustCompile(`^/executors$`),
		regexp.MustCompile(`^/allexecutors$`),
		regexp.MustCompile(`^/allmiscellaneousprocess$`),
		regexp.MustCompile(`^/stages$`),
		regexp.MustCompile(`^/stages/` + n + `$`),
		regexp.MustCompile(`^/stages/` + n + `/` + n + `(/(taskSummary|taskList|taskTable))?$`),
		regexp.MustCompile(`^/storage/rdd$`),
		regexp.MustCompile(`^/storage/rdd/` + n + `$`),
		regexp.MustCompile(`^/environment$`),
		regexp.MustCompile(`^/sql$`),
		regexp.MustCompile(`^/sql/` + n + `$`),
		regexp.MustCompile(`^/streaming/(statistics|receivers|batches)$`),
		regexp.MustCompile(`^/streaming/receivers/` + n + `$`),
		regexp.MustCompile(`^/streaming/batches/` + n + `(/operations(/` + n + `)?)?$`),
	}
)

// Allowed reports whether rest, the path after a run's LocalPrefix, may be forwarded to its driver.
// Callers have already refused everything but GET and HEAD, and BadPath.
func Allowed(rest string) bool {
	if rest == "" {
		rest = "/"
	}
	if uiPages[rest] || (strings.HasSuffix(rest, "/") && uiPages[strings.TrimSuffix(rest, "/")]) {
		return true
	}
	if staticRE.MatchString(rest) {
		return !strings.Contains(rest, "..")
	}
	if restRE.MatchString(rest) {
		return true
	}
	m := perApp.FindStringSubmatch(rest)
	if m == nil {
		return false
	}
	suffix := strings.TrimSuffix(m[1], "/")
	for _, re := range appRest {
		if re.MatchString(suffix) {
			return true
		}
	}
	return false
}
