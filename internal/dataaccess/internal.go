package dataaccess

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/projectbooth/booth-spark/internal/runs"
)

// RunStore is what the internal port reads.
type RunStore interface {
	Get(ctx context.Context, id string) (runs.Run, error)
	ByDataBearer(ctx context.Context, bearer string) (runs.Run, error)
}

// Internal is the backend's internal port (8081): reachable from this install's run namespaces only
// (their to-backend NetworkPolicy, and the chart's ingress policy), never through booth-core.
//
//	POST /internal/token
//	    An agent's token refresh. Authorization: Bearer <the run's data bearer>. Answers the run's
//	    current workload token, or 403 with the reason once the submitter has lost access.
//	POST /internal/broker/api/credentials
//	    The credential sidecars' --core-url points here, so run pods never need a route to
//	    booth-core. Forwarded to core's broker unchanged, but only for a live run of this install,
//	    in that run's workspace (booth-streamlit's shape, ADR 0107 item 3).
//	* /internal/lakehouse/...
//	    Iceberg's REST catalog for the run's Spark, forwarded to core's gateway at
//	    /modules/lakehouse/... with the caller's own token, on the same terms.
//	GET /internal/main
//	    The run's entry point in booth-storage (main.python or main.jar), read as the run.
//	    Authorization: Bearer <the run's data bearer>.
type Internal struct {
	Store   RunStore
	Tokens  *Tokens
	CoreURL string
	HTTP    *http.Client
}

// Router returns the internal port's handler.
func (in *Internal) Router() http.Handler {
	r := chi.NewRouter()
	r.Post("/internal/token", in.token)
	r.Post("/internal/broker/api/credentials", in.broker)
	r.HandleFunc("/internal/lakehouse/*", in.lakehouse)
	r.Get("/internal/main", in.main)
	r.Get("/internal/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	return r
}

func live(r runs.Run) bool { return r.State == runs.Pending || r.State == runs.Running }

// byBearer identifies the calling run by its data bearer. It writes the response itself when it
// fails.
func (in *Internal) byBearer(w http.ResponseWriter, r *http.Request) (runs.Run, bool) {
	bearer, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || len(bearer) < 32 {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return runs.Run{}, false
	}
	run, err := in.Store.ByDataBearer(r.Context(), bearer)
	// The lookup is by exact value; the constant-time compare is belt and braces.
	if err != nil || subtle.ConstantTimeCompare([]byte(run.DataBearer), []byte(bearer)) != 1 || !live(run) {
		log.Printf("internal: %s %s refused from %s: unknown bearer, or the run has ended", r.Method, r.URL.Path, r.RemoteAddr)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return runs.Run{}, false
	}
	return run, true
}

// tokenFor gets the run's current token, answering 403 with the reason once it is refused and 503
// when core is unreachable.
func (in *Internal) tokenFor(w http.ResponseWriter, run runs.Run) (Token, bool) {
	tok, err := in.Tokens.Get(context.Background(), runOf(run))
	switch {
	case Refused(err):
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "data_access_refused", "reason": err.Error()})
		return Token{}, false
	case err != nil:
		log.Printf("internal: minting for %s: %v", run.ID, err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "unavailable"})
		return Token{}, false
	}
	return tok, true
}

func (in *Internal) token(w http.ResponseWriter, r *http.Request) {
	run, ok := in.byBearer(w, r)
	if !ok {
		return
	}
	tok, ok := in.tokenFor(w, run)
	if !ok {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, tok)
}

// byToken identifies the calling run by its workload token's subject, which must name a live run
// of this install, in the workspace the request is for. Core verifies the token itself; this only
// decides whether the request is ours to forward.
func (in *Internal) byToken(w http.ResponseWriter, r *http.Request) (runs.Run, string, bool) {
	refuse := func(code int, why string) (runs.Run, string, bool) {
		log.Printf("internal: %s %s refused from %s: %s", r.Method, r.URL.Path, r.RemoteAddr, why)
		writeJSON(w, code, map[string]string{"error": why})
		return runs.Run{}, "", false
	}
	jwt, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return refuse(http.StatusUnauthorized, "no token")
	}
	sub, err := unverifiedSubject(jwt)
	if err != nil {
		return refuse(http.StatusUnauthorized, "unreadable token")
	}
	rest, ok := strings.CutPrefix(sub, "spark:")
	ws, id, ok2 := strings.Cut(rest, ":")
	if !ok || !ok2 || ws == "" || id == "" || r.Header.Get("X-Workspace") != ws {
		return refuse(http.StatusForbidden, "not a booth-spark run token for this workspace")
	}
	run, err := in.Store.Get(r.Context(), id)
	if err != nil || run.Workspace != ws || !live(run) || run.Data == nil {
		return refuse(http.StatusForbidden, "no such live run with data access")
	}
	return run, jwt, true
}

// brokerRequest is the part of contracts/credential-broker.md's request the forwarder checks.
type brokerRequest struct {
	Kind   string          `json:"kind"`
	Access string          `json:"access"`
	Scope  json.RawMessage `json:"scope"`
}

func (in *Internal) broker(w http.ResponseWriter, r *http.Request) {
	run, jwt, ok := in.byToken(w, r)
	if !ok {
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	var req brokerRequest
	if err != nil || json.Unmarshal(body, &req) != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unreadable body"})
		return
	}
	if req.Access != "read" && req.Access != "readwrite" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "access must be read or readwrite"})
		return
	}
	switch req.Kind {
	case "postgres":
		var sc struct {
			Workspace string `json:"workspace"`
		}
		if json.Unmarshal(req.Scope, &sc) != nil || sc.Workspace != run.Workspace {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "postgres scope must be the run's own workspace"})
			return
		}
	case "s3":
	default:
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "unknown credential kind"})
		return
	}
	in.forward(w, r, http.MethodPost, "/api/credentials", "", bytes.NewReader(body), jwt, run.Workspace, 30*time.Second, 1<<20)
}

// lakehouse forwards Iceberg's REST catalog to booth-lakehouse, which enforces the caller's role on
// every call (its proxy.py): reads need viewer, changes editor.
func (in *Internal) lakehouse(w http.ResponseWriter, r *http.Request) {
	run, jwt, ok := in.byToken(w, r)
	if !ok {
		return
	}
	rest := strings.TrimPrefix(r.URL.EscapedPath(), "/internal/lakehouse")
	if !strings.HasPrefix(rest, "/iceberg/") || strings.Contains(rest, "..") {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	in.forward(w, r, r.Method, "/modules/lakehouse"+rest, r.URL.RawQuery, io.LimitReader(r.Body, 16<<20), jwt, run.Workspace, 2*time.Minute, 64<<20)
}

func (in *Internal) main(w http.ResponseWriter, r *http.Request) {
	run, ok := in.byBearer(w, r)
	if !ok {
		return
	}
	f := run.Spec.MainFile()
	if f == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "the run's entry point isn't a file in booth-storage"})
		return
	}
	tok, ok := in.tokenFor(w, run)
	if !ok {
		return
	}
	in.forward(w, r, http.MethodGet, ObjectURL(f.BackendID, f.Path), "", nil, tok.JWT, run.Workspace, 10*time.Minute, runs.MaxMainFileBytes)
}

// forward relays one request to booth-core as the run, and its answer back unchanged. A body over
// limit is cut off, which the caller sees as a short read.
func (in *Internal) forward(w http.ResponseWriter, r *http.Request, method, path, query string, body io.Reader, jwt, ws string, timeout time.Duration, limit int64) {
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	target := strings.TrimRight(in.CoreURL, "/") + path
	if query != "" {
		target += "?" + query
	}
	out, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
		return
	}
	out.Header.Set("Authorization", "Bearer "+jwt)
	out.Header.Set("X-Workspace", ws)
	for _, h := range []string{"Content-Type", "Accept", "Content-Encoding"} {
		if v := r.Header.Get(h); v != "" {
			out.Header.Set(h, v)
		}
	}
	hc := in.HTTP
	if hc == nil {
		hc = &http.Client{}
	}
	resp, err := hc.Do(out)
	if err != nil {
		log.Printf("internal: forwarding %s %s: %v", method, path, err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "booth-core unreachable"})
		return
	}
	defer resp.Body.Close()
	// Relayed as-is; a credential in it is never logged or kept.
	for _, h := range []string{"Content-Type", "Content-Length", "Content-Encoding", "ETag"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, io.LimitReader(resp.Body, limit))
}

// unverifiedSubject reads a JWT's `sub` without verifying it. Only for routing a request that core
// then verifies; never for an authorization decision of this module's own.
func unverifiedSubject(jwt string) (string, error) {
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		return "", errors.New("not a JWT")
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", err
	}
	var claims struct {
		Sub string `json:"sub"`
	}
	if err := json.Unmarshal(raw, &claims); err != nil || claims.Sub == "" {
		return "", errors.New("no subject")
	}
	return claims.Sub, nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
