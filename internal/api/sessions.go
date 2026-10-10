package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/projectbooth/booth-spark/internal/auth"
	"github.com/projectbooth/booth-spark/internal/runs"
	"github.com/projectbooth/booth-spark/internal/uiproxy"
)

// maxOpenStatements is how many statements a session may have waiting or running at once.
const maxOpenStatements = 20

// sessionView is a session as /v1 shows it.
type sessionView struct {
	runs.Run
	Resources struct {
		DriverMemory   string `json:"driverMemory"`
		ExecutorMemory string `json:"executorMemory"`
		MinExecutors   int    `json:"minExecutors"`
		MaxExecutors   int    `json:"maxExecutors"`
	} `json:"resources"`
	IdleTimeout string `json:"idleTimeout"`
	MaxLifetime string `json:"maxLifetime"`
	// UIPath is where the session's Spark UI opens in the shell while it runs (its submitter only).
	UIPath string `json:"uiPath,omitempty"`
}

func sview(r runs.Run) sessionView {
	v := sessionView{Run: r}
	v.Resources.DriverMemory = strconv.Itoa(r.Spec.DriverHeapMi) + "m"
	v.Resources.ExecutorMemory = strconv.Itoa(r.Spec.ExecutorHeapMi) + "m"
	v.Resources.MinExecutors = r.Spec.MinExecutors
	v.Resources.MaxExecutors = r.Spec.MaxExecutors
	v.IdleTimeout = r.Spec.IdleTimeout.String()
	v.MaxLifetime = r.Spec.Duration.String()
	if r.State == runs.Running {
		v.UIPath = uiproxy.BasePath(r.ID) + "/"
	}
	return v
}

func (a Applications) sessionRoutes(r chi.Router, minRole auth.Role) {
	r.Post("/sessions", a.createSession(minRole))
	r.Get("/sessions", a.listSessions)
	r.Get("/sessions/{id}", a.getSession)
	r.Delete("/sessions/{id}", a.deleteSession)
	r.Get("/sessions/{id}/logs", a.sessionLogs)
	r.Post("/sessions/{id}/statements", a.addStatement)
	r.Get("/sessions/{id}/statements", a.listStatements)
	r.Get("/sessions/{id}/statements/{sid}", a.getStatement)
}

// maySeeSession is step 4's rule (ADR 0110, the coordinator's step 4 scope): a session is its
// submitter's and its workspace owners'. Nobody else in the workspace sees it, a platform operator
// included, because its statements and their results are the submitter's own work.
func maySeeSession(id auth.Identity, s runs.Run) bool {
	return s.Submitter == id.Subject || id.Role == auth.RoleOwner
}

func newSessionToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func (a Applications) createSession(minRole auth.Role) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, _ := auth.FromContext(r.Context())
		if !canSubmit(id.Role, minRole) {
			apiError(w, http.StatusForbidden, "forbidden", "starting sessions needs the "+string(minRole)+" role or above in this workspace")
			return
		}
		key := r.Header.Get("Idempotency-Key")
		if len(key) > 200 {
			apiError(w, http.StatusBadRequest, "invalid", "Idempotency-Key: at most 200 characters")
			return
		}
		var spec runs.SessionSpec
		dec := json.NewDecoder(io.LimitReader(r.Body, 64<<10))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&spec); err != nil {
			apiError(w, http.StatusBadRequest, "invalid", "body: "+err.Error())
			return
		}
		if id.Workload && len(strings.TrimSpace(string(spec.DataAccess))) > 0 && string(spec.DataAccess) != "null" {
			apiError(w, http.StatusUnprocessableEntity, "unavailable", "a session started with a workload token has no data access in this version (ADR 0110)")
			return
		}
		v, err := runs.ValidateSession(spec, a.Limits)
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
			log.Printf("api: validate session: %v", err)
			apiError(w, http.StatusInternalServerError, "internal", "could not validate the session")
			return
		}
		runID := runs.NewID()
		s, err := a.Store.Create(r.Context(), runs.Run{
			ID: runID, Kind: "session", Workspace: id.Workspace, Submitter: id.Subject, SubmitterName: id.DisplayName,
			Workload: id.Workload, Name: spec.Name, Namespace: runs.NamespacePrefix + runID, FootprintMi: v.FootprintMi(), Spec: v,
			SessionToken: newSessionToken(),
		}, key, a.Admission)
		var capErr runs.ErrAtCapacity
		switch {
		case errors.Is(err, runs.ErrDuplicate):
			writeJSON(w, http.StatusOK, sview(s))
			return
		case errors.As(err, &capErr):
			apiError(w, http.StatusTooManyRequests, "at_capacity", err.Error())
			return
		case err != nil:
			log.Printf("api: create session: %v", err)
			apiError(w, http.StatusInternalServerError, "internal", "could not record the session")
			return
		}
		log.Printf("api: session %s started by sub=%s workspace=%s workload=%v idle=%s", s.ID, id.Subject, id.Workspace, id.Workload, v.IdleTimeout)
		writeJSON(w, http.StatusCreated, sview(s))
	}
}

func (a Applications) listSessions(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	list, err := a.Store.List(r.Context(), id.Workspace, "session", 200)
	if err != nil {
		log.Printf("api: list sessions: %v", err)
		apiError(w, http.StatusInternalServerError, "internal", "could not list sessions")
		return
	}
	out := []sessionView{}
	for _, s := range list {
		if maySeeSession(id, s) {
			out = append(out, sview(s))
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": out})
}

// loadSession returns the session named in the path if the caller may see it. One they may not
// see is indistinguishable from one that doesn't exist (404).
func (a Applications) loadSession(w http.ResponseWriter, r *http.Request) (runs.Run, auth.Identity, bool) {
	id, _ := auth.FromContext(r.Context())
	s, err := a.Store.Get(r.Context(), chi.URLParam(r, "id"))
	if errors.Is(err, runs.ErrNotFound) || (err == nil && (s.Kind != "session" || s.Workspace != id.Workspace || !maySeeSession(id, s))) {
		apiError(w, http.StatusNotFound, "not_found", "no such session")
		return s, id, false
	}
	if err != nil {
		log.Printf("api: get session: %v", err)
		apiError(w, http.StatusInternalServerError, "internal", "could not read the session")
		return s, id, false
	}
	return s, id, true
}

func (a Applications) getSession(w http.ResponseWriter, r *http.Request) {
	if s, _, ok := a.loadSession(w, r); ok {
		writeJSON(w, http.StatusOK, sview(s))
	}
}

func (a Applications) deleteSession(w http.ResponseWriter, r *http.Request) {
	s, id, ok := a.loadSession(w, r)
	if !ok {
		return
	}
	if s.State.Terminal() {
		writeJSON(w, http.StatusOK, sview(s))
		return
	}
	if err := a.Store.RequestStop(r.Context(), s.ID); err != nil {
		log.Printf("api: delete session: %v", err)
		apiError(w, http.StatusInternalServerError, "internal", "could not stop the session")
		return
	}
	log.Printf("api: session %s delete requested by sub=%s", s.ID, id.Subject)
	s.StopRequested = true
	writeJSON(w, http.StatusAccepted, sview(s))
}

func (a Applications) sessionLogs(w http.ResponseWriter, r *http.Request) {
	s, _, ok := a.loadSession(w, r)
	if !ok {
		return
	}
	var text string
	var err error
	if s.State.Terminal() {
		text, err = a.Store.LogTail(r.Context(), s.ID)
	} else if a.LiveLogs != nil {
		text, err = a.LiveLogs(r.Context(), s.Namespace, 500)
	}
	if err != nil {
		log.Printf("api: session logs: %v", err)
		apiError(w, http.StatusBadGateway, "unavailable", "the session's log is not readable right now")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = io.WriteString(w, text)
}

func (a Applications) addStatement(w http.ResponseWriter, r *http.Request) {
	s, id, ok := a.loadSession(w, r)
	if !ok {
		return
	}
	// Statements are the submitter's own code, run with the session's access: a workspace owner
	// may see and stop the session, not type into it.
	if s.Submitter != id.Subject {
		apiError(w, http.StatusForbidden, "forbidden", "only the session's submitter can run statements in it")
		return
	}
	var body struct {
		Kind string `json:"kind"`
		Code string `json:"code"`
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, 512<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		apiError(w, http.StatusBadRequest, "invalid", "body: "+err.Error())
		return
	}
	if body.Kind != "sql" && body.Kind != "python" {
		apiError(w, http.StatusBadRequest, "invalid", "kind: sql or python")
		return
	}
	if strings.TrimSpace(body.Code) == "" || len(body.Code) > 256<<10 {
		apiError(w, http.StatusBadRequest, "invalid", "code: 1 byte to 256 KiB")
		return
	}
	st, err := a.Store.AddStatement(r.Context(), s.ID, body.Kind, body.Code, maxOpenStatements)
	switch {
	case errors.Is(err, runs.ErrSessionOver):
		apiError(w, http.StatusConflict, "session_over", "the session has ended or is ending")
		return
	case errors.Is(err, runs.ErrTooManyStatements):
		apiError(w, http.StatusTooManyRequests, "at_capacity", "this session already has "+strconv.Itoa(maxOpenStatements)+" statements waiting or running")
		return
	case err != nil:
		log.Printf("api: add statement: %v", err)
		apiError(w, http.StatusInternalServerError, "internal", "could not record the statement")
		return
	}
	writeJSON(w, http.StatusCreated, st)
}

func (a Applications) listStatements(w http.ResponseWriter, r *http.Request) {
	s, _, ok := a.loadSession(w, r)
	if !ok {
		return
	}
	list, err := a.Store.Statements(r.Context(), s.ID)
	if err != nil {
		log.Printf("api: list statements: %v", err)
		apiError(w, http.StatusInternalServerError, "internal", "could not list statements")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"statements": list})
}

func (a Applications) getStatement(w http.ResponseWriter, r *http.Request) {
	s, _, ok := a.loadSession(w, r)
	if !ok {
		return
	}
	st, err := a.Store.GetStatement(r.Context(), s.ID, chi.URLParam(r, "sid"))
	if errors.Is(err, runs.ErrNotFound) {
		apiError(w, http.StatusNotFound, "not_found", "no such statement in this session")
		return
	}
	if err != nil {
		log.Printf("api: get statement: %v", err)
		apiError(w, http.StatusInternalServerError, "internal", "could not read the statement")
		return
	}
	writeJSON(w, http.StatusOK, st)
}
