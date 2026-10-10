package uiproxy

import (
	"bufio"
	"os"
	"strings"
	"testing"
)

// sample values for the placeholders in testdata/spark-4.1.3-rest-api.txt.
var placeholders = strings.NewReplacer(
	"[app-id]", "spark-0123456789abcdef", "[base-app-id]", "spark-0123456789abcdef",
	"[attempt-id]", "1", "[job-id]", "3", "[stage-id]", "4", "[stage-attempt-id]", "0",
	"[executor-id]", "driver", "[rdd-id]", "5", "[execution-id]", "6", "[stream-id]", "7",
	"[batch-id]", "8", "[outputOp-id]", "9",
)

// ADR 0110 step 3, requirement 1: every REST endpoint of the pinned Spark version, documented or
// served by its sources, is either allowed or refused on purpose. A new endpoint in a newer Spark
// is refused (default deny) until someone adds it to the list and decides.
func TestAllowlist_EveryDocumentedEndpointIsDecided(t *testing.T) {
	f, err := os.Open("testdata/spark-4.1.3-rest-api.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	docs, total := 0, 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 3 || (fields[0] != "allow" && fields[0] != "deny") {
			t.Fatalf("malformed line %q", line)
		}
		total++
		if fields[2] == "docs" {
			docs++
		}
		path := "/api/v1" + placeholders.Replace(fields[1])
		if strings.Contains(path, "[") {
			t.Fatalf("unsubstituted placeholder in %q", path)
		}
		want := fields[0] == "allow"
		for _, p := range []string{path, path + "/"} {
			if got := Allowed(p); got != want {
				t.Errorf("Allowed(%q) = %v, but the list says %s (%s)", p, got, fields[0], fields[2])
			}
		}
	}
	// The documentation lists 26 endpoints; all must be in the file.
	if docs != 26 {
		t.Errorf("%d documented endpoints in the list, want all 26 from monitoring.html", docs)
	}
	if total < docs {
		t.Error("list is empty")
	}
}

// UI pages and assets: the tabs Spark attaches, allowed; their kill, thread-dump and heap pages, and
// anything else, refused by default.
func TestAllowed_UIPages(t *testing.T) {
	for path, want := range map[string]bool{
		"":                            true,
		"/":                           true,
		"/jobs/":                      true,
		"/jobs":                       true,
		"/jobs/job/":                  true,
		"/stages/stage/":              true,
		"/stages/pool/":               true,
		"/storage/rdd/":               true,
		"/environment/":               true,
		"/executors/":                 true,
		"/SQL/":                       true,
		"/SQL/execution/":             true,
		"/StreamingQuery/statistics/": true,
		"/streaming/batch/":           true,
		"/static/webui.js":            true,
		"/static/images/sort_asc.png": true,
		"/jobs/job/kill/":             false,
		"/stages/stage/kill/":         false,
		"/executors/threadDump/":      false,
		"/executors/heapHistogram/":   false,
		"/static/":                    false,
		"/static/../api/v1/x":         false,
		"/metrics/json":               false,
		"/api/v1/applications/a/executors/driver/threads": false,
		"/api/v1/applications/a/logs":                     false,
		"/api/v1":                                         false,
		"/api/v2/applications":                            false,
		"/anything-else":                                  false,
		"/jobs/job/extra":                                 false,
	} {
		if got := Allowed(path); got != want {
			t.Errorf("Allowed(%q) = %v, want %v", path, got, want)
		}
	}
}
