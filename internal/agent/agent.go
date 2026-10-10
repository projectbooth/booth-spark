// Package agent is what runs inside a data run's pods (docs/design-v0.md item 4), from the
// backend's own image:
//
//	booth-spark agent token   a native sidecar: fetches the run's current workload token from the
//	                          backend's internal port with the run's data bearer, and keeps it in a
//	                          memory-backed file the credential sidecars and Spark read;
//	booth-spark agent ready   its startup probe: the token is on disk;
//	booth-spark agent wait    the start gate, an init container after the sidecars: waits for each
//	                          sidecar's first lease (--healthz, loopback) and, in the driver of a run
//	                          whose entry point is in booth-storage, copies that file in (--fetch,
//	                          --to). Spark starts after it.
//
// No token is ever written anywhere but the token file, and the data bearer only ever leaves the
// pod in an Authorization header to the backend.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Main runs the subcommand args[0] and returns the process's exit code.
func Main(ctx context.Context, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: booth-spark agent token|ready|wait")
		return 2
	}
	var err error
	switch args[0] {
	case "token":
		err = runToken(ctx, envConfig())
	case "ready":
		err = ready(envConfig().TokenFile)
	case "wait":
		err = runWait(ctx, args[1:], envConfig())
	default:
		err = fmt.Errorf("unknown agent command %q", args[0])
	}
	if err != nil {
		msg := err.Error()
		log.Printf("agent: %s", msg)
		// The kubelet keeps this as the container's termination message, which the backend reports
		// as the run's reason.
		_ = os.WriteFile("/dev/termination-log", []byte(msg), 0o644)
		return 1
	}
	return 0
}

// Config is the agent's environment (set by internal/runs).
type Config struct {
	TokenURL   string        // BOOTH_AGENT_TOKEN_URL
	BearerFile string        // BOOTH_AGENT_BEARER_FILE
	TokenFile  string        // BOOTH_AGENT_TOKEN_FILE
	RefreshMax time.Duration // BOOTH_AGENT_REFRESH_MAX (test knob)
	HTTP       *http.Client
	Now        func() time.Time
	Sleep      func(context.Context, time.Duration) bool
}

func envConfig() Config {
	c := Config{
		TokenURL:   os.Getenv("BOOTH_AGENT_TOKEN_URL"),
		BearerFile: os.Getenv("BOOTH_AGENT_BEARER_FILE"),
		TokenFile:  os.Getenv("BOOTH_AGENT_TOKEN_FILE"),
	}
	if v := os.Getenv("BOOTH_AGENT_REFRESH_MAX"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			c.RefreshMax = d
		}
	}
	return c
}

func (c Config) client() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 15 * time.Second}
}

func (c Config) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c Config) sleep(ctx context.Context, d time.Duration) bool {
	if c.Sleep != nil {
		return c.Sleep(ctx, d)
	}
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

func readBearer(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading the run's data bearer: %w", err)
	}
	return strings.TrimSpace(string(b)), nil
}

type token struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// fetchToken asks the backend for the run's current token. A refusal (403) carries the reason.
func fetchToken(ctx context.Context, c Config) (token, error) {
	bearer, err := readBearer(c.BearerFile)
	if err != nil {
		return token{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.TokenURL, nil)
	if err != nil {
		return token{}, err
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := c.client().Do(req)
	if err != nil {
		return token{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error, Reason string
		}
		_ = json.Unmarshal(raw, &e)
		return token{}, fmt.Errorf("the backend answered %d %s %s", resp.StatusCode, e.Error, e.Reason)
	}
	var t token
	if err := json.Unmarshal(raw, &t); err != nil || t.Token == "" {
		return token{}, errors.New("the backend's answer has no token")
	}
	return t, nil
}

// writeAtomic writes data to path through a temporary file renamed over it, so a reader never sees
// a partial file.
func writeAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// refreshIn is when to fetch again: at two thirds of the token's remaining life, at most
// RefreshMax, at least 5 seconds.
func refreshIn(c Config, t token) time.Duration {
	d := t.ExpiresAt.Sub(c.now()) * 2 / 3
	if c.RefreshMax > 0 && d > c.RefreshMax {
		d = c.RefreshMax
	}
	if d < 5*time.Second {
		d = 5 * time.Second
	}
	return d
}

// runToken keeps the token file current until ctx ends. A failure keeps the previous token (it
// stays valid until it expires) and retries with backoff; the backend decides when the run ends.
func runToken(ctx context.Context, c Config) error {
	if c.TokenURL == "" || c.BearerFile == "" || c.TokenFile == "" {
		return errors.New("BOOTH_AGENT_TOKEN_URL, BOOTH_AGENT_BEARER_FILE and BOOTH_AGENT_TOKEN_FILE are required")
	}
	backoff := time.Second
	for {
		t, err := fetchToken(ctx, c)
		wait := backoff
		if err == nil {
			if err = writeAtomic(c.TokenFile, []byte(t.Token), 0o440); err == nil {
				backoff = time.Second
				wait = refreshIn(c, t)
			}
		}
		if err != nil {
			log.Printf("agent: refreshing the run's token (retrying in %s): %v", backoff, err)
			if backoff *= 2; backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
		}
		if !c.sleep(ctx, wait) {
			return nil
		}
	}
}

func ready(tokenFile string) error {
	b, err := os.ReadFile(tokenFile)
	if err != nil || len(strings.TrimSpace(string(b))) == 0 {
		return errors.New("no token yet")
	}
	return nil
}

// runWait is the start gate.
func runWait(ctx context.Context, args []string, c Config) error {
	fs := flag.NewFlagSet("wait", flag.ContinueOnError)
	var healthz multi
	fs.Var(&healthz, "healthz", "a sidecar's loopback /healthz (repeatable)")
	fetch := fs.String("fetch", "", "the backend's /internal/main, to copy the run's entry point from")
	to := fs.String("to", "", "where the entry point goes")
	timeout := fs.Duration("timeout", 5*time.Minute, "how long to wait for every sidecar")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	for _, u := range healthz {
		if err := waitHealthy(ctx, c, u); err != nil {
			return err
		}
	}
	if *fetch != "" {
		if *to == "" {
			return errors.New("--fetch needs --to")
		}
		return fetchMain(ctx, c, *fetch, *to)
	}
	return nil
}

func waitHealthy(ctx context.Context, c Config, u string) error {
	hc := &http.Client{Timeout: 2 * time.Second}
	for {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		resp, err := hc.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		if !c.sleep(ctx, 500*time.Millisecond) {
			return fmt.Errorf("a credential sidecar (%s) got no lease in time: the run's data access couldn't be set up", u)
		}
	}
}

// fetchMain copies the run's entry point from booth-storage, through the backend.
func fetchMain(ctx context.Context, c Config, u, to string) error {
	bearer, err := readBearer(c.BearerFile)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return fmt.Errorf("reading the run's entry point: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<10))
		return fmt.Errorf("reading the run's entry point from booth-storage: %d %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	tmp, err := os.CreateTemp(filepath.Dir(to), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, resp.Body); err != nil {
		tmp.Close()
		return fmt.Errorf("reading the run's entry point: %w", err)
	}
	if err := tmp.Chmod(0o444); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), to)
}

type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }
