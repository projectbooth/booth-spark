package uiproxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// driver stands in for a Spark driver's UI: it echoes what it received and can redirect or set a
// cookie, as user code controlling the driver's Jetty could.
func driver(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			// Spark's real answer to "/": absolute, the Host it was asked for, no prefix.
			http.Redirect(w, r, "http://"+r.Host+"/jobs/", http.StatusFound)
			return
		case "/cookie":
			http.SetCookie(w, &http.Cookie{Name: "booth_iframe_session", Value: "planted", Path: "/"})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"path": r.URL.Path, "query": r.URL.RawQuery, "headers": r.Header, "host": r.Host})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func serve(t *testing.T, srv *httptest.Server, path string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	target, _ := url.Parse(srv.URL)
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	Serve(rec, req, Run{ID: "r-1"}, target)
	return rec
}

func TestServe_StripsIdentityAndForwardsThePath(t *testing.T) {
	srv := driver(t)
	rec := serve(t, srv, "/runs/r-1/ui/api/v1/applications?status=running&booth_iframe_token=nav", map[string]string{
		"X-Booth-Identity":  "assertion",
		"X-Booth-Workspace": "acme",
		"X-Booth-Role":      "owner",
		"X-Booth-User":      "u",
		"Authorization":     "Bearer secret",
		"Cookie":            "booth_iframe_session=s",
		"X-Forwarded-For":   "10.0.0.1",
		"Forwarded":         "for=10.0.0.1",
		"Accept":            "application/json",
		"User-Agent":        "test",
	})
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var got struct {
		Path    string              `json:"path"`
		Query   string              `json:"query"`
		Headers map[string][]string `json:"headers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Path != "/api/v1/applications" || got.Query != "status=running" {
		t.Errorf("forwarded %s?%s", got.Path, got.Query)
	}
	for h := range got.Headers {
		switch h {
		case "Accept", "User-Agent", "Accept-Encoding": // Accept-Encoding is the transport's own
		default:
			t.Errorf("header %s reached the driver", h)
		}
	}
}

func TestServe_RootPath(t *testing.T) {
	srv := driver(t)
	rec := serve(t, srv, "/runs/r-1/ui/", nil)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/iframe/spark/runs/r-1/ui/jobs/" {
		t.Errorf("root: %d Location %q", rec.Code, rec.Header().Get("Location"))
	}
}

func TestServe_NoCookiesFromTheDriver(t *testing.T) {
	srv := driver(t)
	rec := serve(t, srv, "/runs/r-1/ui/cookie", nil)
	if c := rec.Header().Values("Set-Cookie"); len(c) != 0 {
		t.Errorf("the driver set a cookie on the shell's origin: %v", c)
	}
}

func TestServe_UnreachableDriver(t *testing.T) {
	target, _ := url.Parse("http://127.0.0.1:1")
	rec := httptest.NewRecorder()
	Serve(rec, httptest.NewRequest(http.MethodGet, "/runs/r-1/ui/jobs/", nil), Run{ID: "r-1"}, target)
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status %d", rec.Code)
	}
}

func TestRewriteLocation(t *testing.T) {
	base := BasePath("r-1")
	for in, want := range map[string]string{
		"http://10.1.2.3:4040/jobs/":           base + "/jobs/",
		"http://evil.example/jobs/?a=1":        base + "/jobs/?a=1",
		"/jobs/":                               base + "/jobs/",
		base + "/stages/":                      base + "/stages/",
		"stages/":                              "stages/",
		"//evil.example/x":                     base + "/x",
		"/iframe/spark/runs/r-10/ui/":          base + "/iframe/spark/runs/r-10/ui/",
		"/iframe/spark/runs/r-1/uix/somewhere": base + "/iframe/spark/runs/r-1/uix/somewhere",
	} {
		if got := rewriteLocation(in, base); got != want {
			t.Errorf("rewriteLocation(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBlocked(t *testing.T) {
	for path, want := range map[string]bool{
		"/jobs/":                    false,
		"/jobs/job/kill/":           true,
		"/stages/stage/kill/":       true,
		"/STAGES/STAGE/KILL/":       true,
		"/executors/threadDump/":    true,
		"/executors/heapHistogram/": true,
		"/api/v1/applications/local-1/executors/driver/threads": true,
		"/api/v1/applications/local-1/allexecutors":             false,
		"/environment/":    false,
		"/static/webui.js": false,
		"/jobs/killer/":    false,
	} {
		if got := Blocked(path); got != want {
			t.Errorf("Blocked(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestBadPath(t *testing.T) {
	for path, want := range map[string]bool{
		"/runs/r-1/ui/jobs/":       false,
		"/runs/r-1/ui/../../v1/me": true,
		"/runs/r-1/ui/%2e%2e/x":    true,
		"/runs/r-1/ui/a%2Fb":       true,
		"/runs/r-1/ui/a%5cb":       true,
		"/runs/r-1/ui/a%00":        true,
		"/runs/r-1/ui/./jobs":      true,
		`/runs/r-1/ui/a\b`:         true,
	} {
		if got := BadPath(path); got != want {
			t.Errorf("BadPath(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestValidID(t *testing.T) {
	for id, want := range map[string]bool{"r-1": true, "proof-1": true, "proxy": false, "history": false, "": false, "R-1": false, "a/b": false, "-x": false} {
		if got := ValidID(id); got != want {
			t.Errorf("ValidID(%q) = %v", id, got)
		}
	}
}
