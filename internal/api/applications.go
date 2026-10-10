package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/projectbooth/booth-spark/internal/auth"
	"github.com/projectbooth/booth-spark/internal/runs"
	"github.com/projectbooth/booth-spark/internal/uiproxy"
)

// RunStore is what the API needs from the run records; *runs.Store implements it.
type RunStore interface {
	Create(ctx context.Context, r runs.Run, idempotencyKey string, a runs.Admission) (runs.Run, error)
	Get(ctx context.Context, id string) (runs.Run, error)
	List(ctx context.Context, workspace, kind string, limit int) ([]runs.Run, error)
	RequestStop(ctx context.Context, id string) error
	LogTail(ctx context.Context, id string) (string, error)
	AddStatement(ctx context.Context, runID, kind, code string, maxOpen int) (runs.Statement, error)
	Statements(ctx context.Context, runID string) ([]runs.Statement, error)
	GetStatement(ctx context.Context, runID, id string) (runs.Statement, error)
}

// Applications is the /v1 job-submission API's configuration (docs/design-v0.md item 6).
type Applications struct {
	Store     RunStore
	Limits    runs.Limits
	Admission runs.Admission
	// LiveLogs reads the last lines of a live run's driver log.
	LiveLogs func(ctx context.Context, ns string, tailLines int64) (string, error)
}

// runView is a run as /v1 shows it.
type runView struct {
	runs.Run
	Resources struct {
		DriverMemory   string `json:"driverMemory"`
		ExecutorMemory string `json:"executorMemory"`
		MinExecutors   int    `json:"minExecutors"`
		MaxExecutors   int    `json:"maxExecutors"`
		MaxDuration    string `json:"maxDuration"`
	} `json:"resources"`
	// UIPath is where the run's Spark UI opens in the shell while it runs (its submitter only).
	UIPath string `json:"uiPath,omitempty"`
}

func view(r runs.Run) runView {
	v := runView{Run: r}
	v.Resources.DriverMemory = strconv.Itoa(r.Spec.DriverHeapMi) + "m"
	v.Resources.ExecutorMemory = strconv.Itoa(r.Spec.ExecutorHeapMi) + "m"
	v.Resources.MinExecutors = r.Spec.MinExecutors
	v.Resources.MaxExecutors = r.Spec.MaxExecutors
	v.Resources.MaxDuration = r.Spec.Duration.String()
	if r.State == runs.Running {
		v.UIPath = uiproxy.BasePath(r.ID) + "/"
	}
	return v
}

// apiError writes /v1's error body: {error, code}.
func apiError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg, "code": code})
}

func (a Applications) routes(r chi.Router, minRole auth.Role) {
	r.Post("/applications", a.submit(minRole))
	r.Get("/applications", a.list)
	r.Get("/applications/{id}", a.get)
	r.Post("/applications/{id}/stop", a.stop)
	r.Get("/applications/{id}/logs", a.logs)
	a.sessionRoutes(r, minRole)
}

func (a Applications) submit(minRole auth.Role) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, _ := auth.FromContext(r.Context())
		if !canSubmit(id.Role, minRole) {
			apiError(w, http.StatusForbidden, "forbidden", "submitting runs needs the "+string(minRole)+" role or above in this workspace")
			return
		}
		key := r.Header.Get("Idempotency-Key")
		if len(key) > 200 {
			apiError(w, http.StatusBadRequest, "invalid", "Idempotency-Key: at most 200 characters")
			return
		}
		var spec runs.Spec
		dec := json.NewDecoder(io.LimitReader(r.Body, 512<<10))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&spec); err != nil {
			apiError(w, http.StatusBadRequest, "invalid", "body: "+err.Error())
			return
		}
		if id.Workload && len(strings.TrimSpace(string(spec.DataAccess))) > 0 && string(spec.DataAccess) != "null" {
			// ADR 0110 ruling 5: a workload token names no person to read data as.
			apiError(w, http.StatusUnprocessableEntity, "unavailable", "a run submitted with a workload token has no data access in this version (ADR 0110)")
			return
		}
		v, err := runs.Validate(spec, a.Limits)
		var verr runs.ValidationError
		var unav runs.Unavailable
		switch {
		case errors.As(err, &verr):
			apiError(w, http.StatusBadRequest, "invalid", err.Error())
			return
		case errors.As(err, &unav):
			apiError(w, http.StatusUnprocessableEntity, "unavailable", err.Error())
			return
		case err != nil:
			log.Printf("api: validate: %v", err)
			apiError(w, http.StatusInternalServerError, "internal", "could not validate the run")
			return
		}
		runID := runs.NewID()
		run, err := a.Store.Create(r.Context(), runs.Run{
			ID: runID, Kind: "application", Workspace: id.Workspace, Submitter: id.Subject, SubmitterName: id.DisplayName,
			Workload: id.Workload, Name: spec.Name, Namespace: runs.NamespacePrefix + runID, FootprintMi: v.FootprintMi(), Spec: v,
		}, key, a.Admission)
		var capErr runs.ErrAtCapacity
		switch {
		case errors.Is(err, runs.ErrDuplicate):
			writeJSON(w, http.StatusOK, view(run))
			return
		case errors.As(err, &capErr):
			apiError(w, http.StatusTooManyRequests, "at_capacity", err.Error())
			return
		case err != nil:
			log.Printf("api: create run: %v", err)
			apiError(w, http.StatusInternalServerError, "internal", "could not record the run")
			return
		}
		log.Printf("api: run %s submitted by sub=%s workspace=%s workload=%v footprint=%dMi", run.ID, id.Subject, id.Workspace, id.Workload, run.FootprintMi)
		writeJSON(w, http.StatusCreated, view(run))
	}
}

func (a Applications) list(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	list, err := a.Store.List(r.Context(), id.Workspace, "application", 200)
	if err != nil {
		log.Printf("api: list runs: %v", err)
		apiError(w, http.StatusInternalServerError, "internal", "could not list runs")
		return
	}
	mine := r.URL.Query().Get("mine") == "true"
	state := r.URL.Query().Get("state")
	out := []runView{}
	for _, run := range list {
		if (mine && run.Submitter != id.Subject) || (state != "" && string(run.State) != state) {
			continue
		}
		out = append(out, view(run))
	}
	writeJSON(w, http.StatusOK, map[string]any{"applications": out})
}

// load returns the run named in the path if it belongs to the caller's workspace. A run in
// another workspace is indistinguishable from an unknown one.
func (a Applications) load(w http.ResponseWriter, r *http.Request) (runs.Run, auth.Identity, bool) {
	id, _ := auth.FromContext(r.Context())
	run, err := a.Store.Get(r.Context(), chi.URLParam(r, "id"))
	if errors.Is(err, runs.ErrNotFound) || (err == nil && (run.Workspace != id.Workspace || run.Kind != "application")) {
		apiError(w, http.StatusNotFound, "not_found", "no such run in this workspace")
		return run, id, false
	}
	if err != nil {
		log.Printf("api: get run: %v", err)
		apiError(w, http.StatusInternalServerError, "internal", "could not read the run")
		return run, id, false
	}
	return run, id, true
}

func (a Applications) get(w http.ResponseWriter, r *http.Request) {
	if run, _, ok := a.load(w, r); ok {
		writeJSON(w, http.StatusOK, view(run))
	}
}

// mayManage is item 3's rule for stopping a run and reading its logs: its submitter, an owner of
// its workspace, or a platform operator. Logs can carry what the run's code printed, its token
// included (step 5), so a viewer or another editor never reads them.
func mayManage(id auth.Identity, run runs.Run) bool {
	return run.Submitter == id.Subject || id.Role == auth.RoleOwner || id.Operator
}

func (a Applications) stop(w http.ResponseWriter, r *http.Request) {
	run, id, ok := a.load(w, r)
	if !ok {
		return
	}
	if !mayManage(id, run) {
		apiError(w, http.StatusForbidden, "forbidden", "only the run's submitter, a workspace owner or a platform operator can stop it")
		return
	}
	if run.State.Terminal() {
		writeJSON(w, http.StatusOK, view(run))
		return
	}
	if err := a.Store.RequestStop(r.Context(), run.ID); err != nil {
		log.Printf("api: stop run: %v", err)
		apiError(w, http.StatusInternalServerError, "internal", "could not stop the run")
		return
	}
	log.Printf("api: run %s stop requested by sub=%s", run.ID, id.Subject)
	run.StopRequested = true
	writeJSON(w, http.StatusAccepted, view(run))
}

func (a Applications) logs(w http.ResponseWriter, r *http.Request) {
	run, id, ok := a.load(w, r)
	if !ok {
		return
	}
	if !mayManage(id, run) {
		apiError(w, http.StatusForbidden, "forbidden", "only the run's submitter, a workspace owner or a platform operator can read its logs")
		return
	}
	tail := int64(500)
	if v := r.URL.Query().Get("tail"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 1 || n > 10000 {
			apiError(w, http.StatusBadRequest, "invalid", "tail: 1 to 10000 lines")
			return
		}
		tail = n
	}
	if run.ContentClearedAt != nil {
		apiError(w, http.StatusGone, "cleared", "the run's log was cleared "+run.ContentClearedAt.UTC().Format(time.RFC3339)+" (sessions.resultRetention)")
		return
	}
	var text string
	var err error
	if run.State.Terminal() {
		text, err = a.Store.LogTail(r.Context(), run.ID)
	} else if a.LiveLogs != nil {
		text, err = a.LiveLogs(r.Context(), run.Namespace, tail)
	}
	if err != nil {
		log.Printf("api: logs: %v", err)
		apiError(w, http.StatusBadGateway, "unavailable", "the run's log is not readable right now")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = io.WriteString(w, text)
}

// StoreRuns resolves the Spark UI's runs from the run records: a run's UI exists while it runs.
type StoreRuns struct{ Store RunStore }

// Lookup implements RunLookup.
func (s StoreRuns) Lookup(ctx context.Context, id string) (uiproxy.Run, bool) {
	run, err := s.Store.Get(ctx, id)
	if err != nil || run.State != runs.Running {
		return uiproxy.Run{}, false
	}
	return uiproxy.Run{
		ID: run.ID, Workspace: run.Workspace, Submitter: run.Submitter,
		UIURL: "http://" + runs.DriverService + "." + run.Namespace + ".svc:" + strconv.Itoa(runs.UIPort),
	}, true
}
