// Package uiproxy forwards one run's Spark UI to the browser (docs/design-v0.md item 5). The UI is
// served by the run's driver, inside the user's own JVM, so it is content the run's code controls,
// and it has no authentication of its own. Nothing about it is trusted:
//
//   - Who may see it is decided before this package runs (the api package: the run's submitter only).
//   - Only an allowlist of request headers is forwarded: no X-Booth-*, no Authorization, no Cookie
//     (core's booth_iframe_session included), no X-Forwarded-*. The driver learns nothing about who
//     is looking.
//   - Only GET and HEAD reach it, and only paths on a default-deny allowlist (Allowed, allow.go):
//     the kill, thread-dump and heap-histogram endpoints never do, whatever the run's Spark
//     configuration says.
//   - Its responses may not set cookies: the UI is served same-origin with the shell (ADR 0069), so
//     a Set-Cookie from user code would land on the shell's origin.
//   - Redirects are rewritten to stay under the run's prefix.
package uiproxy

import (
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Run is what the proxy needs to know about one run's UI.
type Run struct {
	ID        string `json:"id"`
	Workspace string `json:"workspace"`
	// Submitter is the `sub` of the person who submitted the run: the only person who may open its
	// Spark UI (ADR 0110 ruling 4).
	Submitter string `json:"submitter"`
	// UIURL is the driver's UI, e.g. http://<driver service>:4040.
	UIURL string `json:"url"`
}

// idRE is the shape of a run id. Spark's own JavaScript treats a path segment named "proxy" or
// "history" as the start of an application id (utils.js getStandAloneAppId and
// createRESTEndPointForExecutorsPage), so a run may never be called either.
var idRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// ValidID reports whether id is usable as a run id in the UI's path.
func ValidID(id string) bool {
	return idRE.MatchString(id) && id != "proxy" && id != "history"
}

// IframePrefix is where the browser sees this module: booth-core mints relative /iframe/<id>/ URLs
// (ADR 0069, implementation notes, bug 2) and forwards only the remainder.
const IframePrefix = "/iframe/spark"

// BasePath is the run's UI as the browser sees it, the value Spark's UI must use as its proxy base
// (internal/sparkconf) so the links it renders resolve under it.
func BasePath(runID string) string { return IframePrefix + "/runs/" + runID + "/ui" }

// LocalPrefix is the run's UI as this backend receives it (core strips IframePrefix).
func LocalPrefix(runID string) string { return "/runs/" + runID + "/ui" }

// forwardedHeaders is everything the driver gets from the request. Anything else (identity,
// cookies, authorization, forwarding headers) is dropped.
var forwardedHeaders = []string{
	"Accept", "Accept-Language", "Cache-Control", "If-Modified-Since", "If-None-Match", "Range", "User-Agent",
}

// BadPath reports whether an escaped request path tries to smuggle a separator, a NUL or a dot
// segment past the routing (the same rules as booth-core's gateway and booth-streamlit's proxy).
func BadPath(escaped string) bool {
	l := strings.ToLower(escaped)
	if strings.Contains(l, "%2f") || strings.Contains(l, "%5c") || strings.Contains(l, "%00") || strings.Contains(escaped, `\`) {
		return true
	}
	for _, seg := range strings.Split(escaped, "/") {
		if seg == "." || seg == ".." || strings.EqualFold(seg, "%2e") || strings.EqualFold(seg, "%2e%2e") {
			return true
		}
	}
	return false
}

// Serve proxies r, whose path is LocalPrefix(run.ID)+rest, to the run's driver. The caller has
// already authorized the request and checked Allowed and BadPath.
func Serve(w http.ResponseWriter, r *http.Request, run Run, target *url.URL) {
	local := LocalPrefix(run.ID)
	base := BasePath(run.ID)
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			rest := strings.TrimPrefix(pr.In.URL.Path, local)
			if rest == "" {
				rest = "/"
			}
			q := pr.In.URL.Query()
			q.Del("booth_iframe_token") // core never forwards it (ADR 0069); dropped here regardless
			pr.Out.URL = &url.URL{Scheme: target.Scheme, Host: target.Host, Path: strings.TrimSuffix(target.Path, "/") + rest, RawQuery: q.Encode()}
			pr.Out.Host = target.Host
			pr.Out.Header = http.Header{}
			for _, h := range forwardedHeaders {
				if v := pr.In.Header.Values(h); len(v) > 0 {
					pr.Out.Header[h] = v
				}
			}
		},
		ModifyResponse: func(resp *http.Response) error {
			resp.Header.Del("Set-Cookie")
			resp.Header.Del("Set-Cookie2")
			if loc := resp.Header.Get("Location"); loc != "" {
				resp.Header.Set("Location", rewriteLocation(loc, base))
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			http.Error(w, "this run's Spark UI is not reachable (the driver may have finished)", http.StatusBadGateway)
		},
		Transport: transport,
	}
	rp.ServeHTTP(w, r)
}

// transport bounds how long a driver may take; the UI is small pages and JSON.
var transport = &http.Transport{
	ResponseHeaderTimeout: 30 * time.Second,
	IdleConnTimeout:       90 * time.Second,
	MaxIdleConnsPerHost:   4,
}

// rewriteLocation keeps a redirect under the run's prefix. Spark answers "/" and "/jobs" with an
// absolute Location naming whatever Host it was asked for and no prefix
// ("http://<host>/jobs/"): that becomes base+"/jobs/". A Location already under base, or a relative
// one, is left alone.
func rewriteLocation(loc, base string) string {
	u, err := url.Parse(loc)
	if err != nil {
		return base + "/"
	}
	if u.IsAbs() || u.Host != "" {
		u = &url.URL{Path: u.Path, RawQuery: u.RawQuery, Fragment: u.Fragment}
	}
	if !strings.HasPrefix(u.Path, "/") {
		return u.String()
	}
	if u.Path == base || strings.HasPrefix(u.Path, base+"/") {
		return u.String()
	}
	u.Path = base + u.Path
	return u.String()
}
