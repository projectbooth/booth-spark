// Package api is booth-spark's HTTP surface. One process serves two route groups that core reaches
// in two different ways, and each trusts exactly one kind of credential (docs/design-v0.md item 2):
//
//   - /v1/*, the published job-submission API, through core's gateway (/modules/spark/v1/...) with a
//     bearer token: a person's OIDC token or core's workload token (internal/auth).
//   - everything else that isn't a health check, through core's iframe proxy (/iframe/spark/...)
//     with core's X-Booth-Identity assertion (internal/identity): the module's own UI now, the
//     Spark UI proxy from step 2.
//
// Authenticated today: the two "who am I" calls (/v1/me and /ui/api/me), the applications API
// (/v1/applications, step 3), and the Spark UI proxy (/runs/{id}/ui/..., step 2) over the
// module's run records. Sessions arrive in step 4.
package api

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/projectbooth/booth-spark/internal/auth"
	"github.com/projectbooth/booth-spark/internal/identity"
	"github.com/projectbooth/booth-spark/internal/uiproxy"
)

// Pinger is the database check /healthz runs; *pgxpool.Pool satisfies it.
type Pinger interface {
	Ping(ctx context.Context) error
}

// IframeVerifier checks the X-Booth-Identity assertion; *identity.Verifier satisfies it.
type IframeVerifier interface {
	Verify(r *http.Request) (identity.Caller, error)
}

// Deps is what the router needs.
type Deps struct {
	DB Pinger
	// Tokens verifies /v1 bearer tokens. Nil (no oidc.issuerUrl configured) makes /v1 answer 503.
	Tokens auth.TokenVerifier
	// Iframe verifies the iframe routes' assertion. Required.
	Iframe IframeVerifier
	// SubmitMinRole is the submit floor (ADR 0110, submit.minRole).
	SubmitMinRole auth.Role
	// Runs resolves a run id to its UI. Nil means no run's UI can be opened.
	Runs RunLookup
	// Applications is the /v1 job-submission API. Nil leaves /v1/applications unrouted (404).
	Applications *Applications
}

// OpenAPI is the /v1 interface's OpenAPI 3.1 document (openapi/v1.yaml), linted in CI and kept
// equal to the routes by TestOpenAPI_MatchesTheRoutes.
//
//go:embed openapi/v1.yaml
var OpenAPI []byte

// RunLookup resolves a run id to what the Spark UI proxy needs: StoreRuns, over the module's own
// run records.
type RunLookup interface {
	Lookup(ctx context.Context, id string) (uiproxy.Run, bool)
}

// dbPingTimeout bounds the /healthz database check, so a hung Postgres shows as unhealthy within
// one probe period instead of hanging the probe.
const dbPingTimeout = 2 * time.Second

// NewRouter builds the HTTP handler.
func NewRouter(deps Deps) http.Handler {
	r := chi.NewRouter()
	r.Use(accessLog) // stdout only (ADR 0022)
	r.Use(middleware.Recoverer)

	// Liveness deliberately looks at nothing external: restarting the pod cannot fix a Postgres
	// outage, so it shouldn't get the pod killed.
	r.Get("/livez", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	r.Get("/healthz", healthz(deps))

	r.Route("/v1", func(v1 chi.Router) {
		if deps.Tokens == nil {
			v1.Use(func(http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					auth.WriteError(w, http.StatusServiceUnavailable, "oidc.issuerUrl is not configured: the /v1 API has no identity provider to verify tokens against")
				})
			})
		} else {
			v1.Use(auth.Middleware(deps.Tokens))
		}
		v1.Get("/me", func(w http.ResponseWriter, r *http.Request) {
			id, _ := auth.FromContext(r.Context())
			writeJSON(w, http.StatusOK, me{
				Subject: id.Subject, DisplayName: id.DisplayName, Workspace: id.Workspace, Role: string(id.Role),
				Operator: id.Operator, Workload: id.Workload, CanSubmit: canSubmit(id.Role, deps.SubmitMinRole),
				SubmitMinRole: string(deps.SubmitMinRole),
			})
		})
		if deps.Applications != nil {
			deps.Applications.routes(v1, deps.SubmitMinRole)
		}
		// The interface's own documentation (docs/design-v0.md item 6), for any authenticated caller.
		v1.Get("/openapi.yaml", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/yaml")
			_, _ = w.Write(OpenAPI)
		})
		v1.NotFound(func(w http.ResponseWriter, _ *http.Request) { auth.WriteError(w, http.StatusNotFound, "not found") })
	})

	// The iframe routes: everything else. Authenticated before routing, so an unknown path gives a
	// caller without an assertion 401, not a map of what exists.
	r.Group(func(ui chi.Router) {
		ui.Use(iframeAuth(deps.Iframe))
		ui.Get("/", page(deps.SubmitMinRole))
		ui.Get("/ui/api/me", func(w http.ResponseWriter, r *http.Request) {
			c := callerFrom(r.Context())
			writeJSON(w, http.StatusOK, me{
				Subject: c.Subject, DisplayName: c.DisplayName, Workspace: c.Workspace, Role: string(c.Role),
				Operator: c.Operator, CanSubmit: canSubmit(auth.Role(c.Role), deps.SubmitMinRole),
				SubmitMinRole: string(deps.SubmitMinRole),
			})
		})
		// The Spark UI of one run (docs/design-v0.md item 5).
		ui.Get("/runs/{id}/ui", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, uiproxy.BasePath(chi.URLParam(r, "id"))+"/", http.StatusFound)
		})
		ui.HandleFunc("/runs/{id}/ui/*", sparkUI(deps.Runs))
		ui.NotFound(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "not found", http.StatusNotFound) })
	})
	return r
}

// sparkUI authorizes and proxies one run's Spark UI. The order matters: a caller learns nothing
// about a run in another workspace (404, as for an unknown id), and a member of the run's
// workspace who isn't its submitter is refused before anything reaches the driver (403). Nobody
// else is exempt, operators and workspace owners included: the page is content the run's code
// controls, served same-origin with the shell, so only its author may run it (ADR 0110 ruling 4,
// ARCHITECTURE item 55).
func sparkUI(runs RunLookup) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c := callerFrom(r.Context())
		id := chi.URLParam(r, "id")
		if uiproxy.BadPath(r.URL.EscapedPath()) {
			http.Error(w, "bad path", http.StatusBadRequest)
			return
		}
		var run uiproxy.Run
		ok := false
		if runs != nil && uiproxy.ValidID(id) {
			run, ok = runs.Lookup(r.Context(), id)
		}
		if !ok || run.Workspace != c.Workspace {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if run.Submitter != c.Subject {
			log.Printf("spark ui: refused run=%s to sub=%s (not its submitter)", id, c.Subject)
			http.Error(w, "only the person who submitted this run can open its Spark UI", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "the Spark UI is read-only here; stop a run from the module's own page", http.StatusMethodNotAllowed)
			return
		}
		rest := strings.TrimPrefix(r.URL.Path, uiproxy.LocalPrefix(id))
		if !uiproxy.Allowed(rest) {
			http.Error(w, "this Spark UI page or action is not available in Booth", http.StatusForbidden)
			return
		}
		target, err := url.Parse(run.UIURL)
		if err != nil {
			http.Error(w, "this run's Spark UI address is invalid", http.StatusBadGateway)
			return
		}
		uiproxy.Serve(w, r, run, target)
	}
}

// me is the "who am I" body both route groups return: what this module derived from the caller's
// verified credential, never from a forwarded header alone (ADR 0041).
type me struct {
	Subject       string `json:"subject"`
	DisplayName   string `json:"displayName"`
	Workspace     string `json:"workspace"`
	Role          string `json:"role"`
	Operator      bool   `json:"operator"`
	Workload      bool   `json:"workload"`
	CanSubmit     bool   `json:"canSubmit"`
	SubmitMinRole string `json:"submitMinRole"`
}

// canSubmit applies ADR 0110's floor: at least minRole (editor or owner), so never a viewer.
func canSubmit(role, minRole auth.Role) bool {
	return auth.Rank(role) >= auth.Rank(minRole) && auth.Rank(role) > auth.Rank(auth.RoleViewer)
}

type callerKey struct{}

func callerFrom(ctx context.Context) identity.Caller {
	c, _ := ctx.Value(callerKey{}).(identity.Caller)
	return c
}

// iframeAuth verifies core's assertion on every iframe route (ADR 0069). A missing or invalid
// assertion is 401, a valid one granting nothing in the workspace is 403.
func iframeAuth(v IframeVerifier) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c, err := v.Verify(r)
			if err != nil {
				status := http.StatusUnauthorized
				if errors.Is(err, identity.ErrForbidden) {
					status = http.StatusForbidden
				}
				log.Printf("iframe auth: refused %s %s: %v", r.Method, r.URL.Path, err)
				http.Error(w, http.StatusText(status), status)
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), callerKey{}, c)))
		})
	}
}

// healthz is both the readiness probe and what booth-core polls (the manifest's healthCheckPath).
// The database is required: without it there are no run records to serve, so the module is
// unready (503).
func healthz(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body := map[string]string{"status": "ok", "database": "ok"}
		code := http.StatusOK
		ctx, cancel := context.WithTimeout(r.Context(), dbPingTimeout)
		defer cancel()
		if deps.DB == nil {
			body["database"], body["status"], code = "unconfigured", "unavailable", http.StatusServiceUnavailable
		} else if err := deps.DB.Ping(ctx); err != nil {
			body["database"], body["status"], code = "unreachable", "unavailable", http.StatusServiceUnavailable
		}
		writeJSON(w, code, body)
	}
}

// pageTmpl is the scaffold's placeholder page. html/template escapes every value, so a display
// name can't inject markup into a page served same-origin with the shell (ADR 0069).
var pageTmpl = template.Must(template.New("page").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>Spark</title>
<style>body{font-family:system-ui,sans-serif;margin:0;padding:0;color:#1f2937}main{max-width:40rem}
@media (prefers-color-scheme: dark){body{color:#e5e7eb;background:#111827}}</style></head>
<body><main>
<h1>Spark</h1>
<p>Signed in as <strong>{{.DisplayName}}</strong>, {{.Role}} in <strong>{{.Workspace}}</strong>{{if .Operator}}, platform operator{{end}}.</p>
<p>{{if .CanSubmit}}You will be able to submit Spark applications and start sessions here.{{else}}Your role can see runs but not submit them (submitting needs {{.SubmitMinRole}} or above).{{end}}</p>
<p>Nothing runs yet: this is the module's first build step.</p>
</main></body></html>
`))

func page(minRole auth.Role) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c := callerFrom(r.Context())
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		_ = pageTmpl.Execute(w, me{
			DisplayName: c.DisplayName, Workspace: c.Workspace, Role: string(c.Role), Operator: c.Operator,
			CanSubmit: canSubmit(auth.Role(c.Role), minRole), SubmitMinRole: string(minRole),
		})
	}
}

// accessLog logs one line per request: method, path, status, size, duration. Never the query
// string: core's iframe entry URL carries a navigation token there (ADR 0069), and later /v1 calls
// may carry identifiers a log shouldn't keep. Never a header either.
func accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)
		log.Printf("http: %s %s %d %dB %s", r.Method, r.URL.Path, ww.Status(), ww.BytesWritten(), time.Since(start).Round(time.Millisecond))
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
