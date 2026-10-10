package runs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// RunnerResult is a statement's state as a session's runner reports it.
type RunnerResult struct {
	State  string          `json:"state"`
	Output json.RawMessage `json:"output"`
	Error  string          `json:"error"`
}

// ErrUnknownStatement means the runner has no record of the statement.
var ErrUnknownStatement = errors.New("the session runner doesn't know this statement")

// runnerClient calls a session's runner (runner/session_runner.py) at its driver Service, with
// the session's bearer. Only the backend's pods can reach that port (NetworkPolicy).
var runnerClient = &http.Client{Timeout: 15 * time.Second}

func runnerURL(ns, path string) string {
	return "http://" + DriverService + "." + ns + ".svc:" + strconv.Itoa(SessionPort) + path
}

func runnerCall(ctx context.Context, method, ns, token, path string, body any, out any) (int, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, runnerURL(ns, path), rd)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := runnerClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return resp.StatusCode, err
	}
	if out != nil && resp.StatusCode < 300 {
		if err := json.Unmarshal(data, out); err != nil {
			return resp.StatusCode, fmt.Errorf("runner: %w", err)
		}
	}
	return resp.StatusCode, nil
}

// RunnerReady reports whether a session's runner answers (its SparkSession exists).
func (l *Launcher) RunnerReady(ctx context.Context, ns, token string) bool {
	code, err := runnerCall(ctx, http.MethodGet, ns, token, "/healthz", nil, nil)
	return err == nil && code == http.StatusOK
}

// RunnerSubmit hands a statement to a session's runner. Idempotent: a statement the runner
// already has is not run twice.
func (l *Launcher) RunnerSubmit(ctx context.Context, ns, token string, st Statement) error {
	code, err := runnerCall(ctx, http.MethodPost, ns, token, "/statements", map[string]string{"id": st.ID, "kind": st.Kind, "code": st.Code}, nil)
	if err != nil {
		return err
	}
	if code != http.StatusAccepted {
		return fmt.Errorf("the session runner answered %d", code)
	}
	return nil
}

// RunnerResult asks a session's runner for a statement's state.
func (l *Launcher) RunnerResult(ctx context.Context, ns, token, id string) (RunnerResult, error) {
	var res RunnerResult
	code, err := runnerCall(ctx, http.MethodGet, ns, token, "/statements/"+id, nil, &res)
	if err != nil {
		return res, err
	}
	if code == http.StatusNotFound {
		return res, ErrUnknownStatement
	}
	if code != http.StatusOK {
		return res, fmt.Errorf("the session runner answered %d", code)
	}
	return res, nil
}
