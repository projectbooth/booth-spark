package runs

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

func typesUID(s string) types.UID { return types.UID(s) }

// Cluster-side operations the controller needs; *Launcher implements them.
type cluster interface {
	Launch(ctx context.Context, r Run) error
	Delete(ctx context.Context, ns string) error
	Driver(ctx context.Context, ns string) (DriverState, error)
	LogTail(ctx context.Context, ns string, maxBytes int64) (string, error)
	RunNamespaces(ctx context.Context) (map[string]string, error)
	RunnerReady(ctx context.Context, ns, token string) bool
	RunnerSubmit(ctx context.Context, ns, token string, st Statement) error
	RunnerResult(ctx context.Context, ns, token, id string) (RunnerResult, error)
}

// Controller converges live runs to their namespaces (docs/design-v0.md items 1 and 7). The
// database is the source of truth; every pass reads it, so a backend restart loses nothing.
type Controller struct {
	Store   *Store
	Cluster cluster
	// Interval between passes.
	Interval time.Duration
	// PendingTimeout fails a run whose driver hasn't started in this long.
	PendingTimeout time.Duration
	// LogTailBytes is how much of the driver's log is kept when a run ends.
	LogTailBytes int64
	Now          func() time.Time
}

// Run passes until ctx is done.
func (c *Controller) Run(ctx context.Context) {
	t := time.NewTicker(c.Interval)
	defer t.Stop()
	for i := 0; ; i++ {
		c.Pass(ctx)
		// The sweep lists namespaces cluster-wide; every tenth pass is plenty.
		if i%10 == 0 {
			c.Sweep(ctx)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Pass handles every live run once.
func (c *Controller) Pass(ctx context.Context) {
	live, err := c.Store.Live(ctx)
	if err != nil {
		log.Printf("runs: listing live runs: %v", err)
		return
	}
	for _, r := range live {
		if err := c.step(ctx, r); err != nil {
			log.Printf("runs: %s: %v", r.ID, err)
		}
	}
}

func (c *Controller) finish(ctx context.Context, r Run, st State, reason string) error {
	tail := ""
	if r.Launched {
		var err error
		if tail, err = c.Cluster.LogTail(ctx, r.Namespace, c.LogTailBytes); err != nil {
			log.Printf("runs: %s: keeping the driver's log: %v", r.ID, err)
		}
	}
	if err := c.Store.Finish(ctx, r.ID, st, reason, tail, c.Now()); err != nil {
		return err
	}
	log.Printf("runs: %s %s: %s", r.ID, st, reason)
	// The namespace goes with the run: its pods, its token, its quota, everything.
	return c.Cluster.Delete(ctx, r.Namespace)
}

func (c *Controller) step(ctx context.Context, r Run) error {
	now := c.Now()
	if r.StopRequested {
		return c.finish(ctx, r, Stopped, "stopped on request")
	}
	if !r.Launched {
		if err := c.Cluster.Launch(ctx, r); err != nil {
			return c.finish(ctx, r, Failed, "could not start: "+err.Error())
		}
		return c.Store.MarkLaunched(ctx, r.ID)
	}
	since := r.CreatedAt
	if r.StartedAt != nil {
		since = *r.StartedAt
	}
	if now.Sub(since) > r.Spec.Duration {
		if r.Kind == "session" {
			return c.finish(ctx, r, Stopped, fmt.Sprintf("reached its maximum lifetime (%s)", r.Spec.Duration))
		}
		return c.finish(ctx, r, Stopped, fmt.Sprintf("reached its maximum duration (%s)", r.Spec.Duration))
	}
	d, err := c.Cluster.Driver(ctx, r.Namespace)
	if err != nil {
		return err
	}
	switch {
	case !d.Exists:
		// Launched, but no driver: the namespace was removed from outside, or is going.
		if now.Sub(r.CreatedAt) > time.Minute {
			return c.finish(ctx, r, Failed, "the driver pod is gone")
		}
	case d.Fatal:
		return c.finish(ctx, r, Failed, "the driver can't start: "+d.Reason)
	case d.Phase == corev1.PodPending:
		if now.Sub(r.CreatedAt) > c.PendingTimeout {
			return c.finish(ctx, r, Failed, fmt.Sprintf("the driver didn't start within %s (%s)", c.PendingTimeout, d.Reason))
		}
	case d.Phase == corev1.PodRunning:
		if r.Kind == "session" {
			return c.session(ctx, r, now)
		}
		if r.State == Pending {
			return c.Store.MarkRunning(ctx, r.ID, now)
		}
	case d.Phase == corev1.PodSucceeded && r.Kind == "session":
		// A session's runner never exits on its own.
		return c.finish(ctx, r, Failed, "the session's driver exited")
	case d.Phase == corev1.PodSucceeded:
		return c.finish(ctx, r, Succeeded, "the application finished")
	case d.Phase == corev1.PodFailed:
		reason := d.Reason
		if reason == "" {
			reason = "the driver failed"
		}
		return c.finish(ctx, r, Failed, reason)
	}
	return nil
}

// Sweep deletes run namespaces whose run is no longer live (a crash between a run's end and its
// namespace's deletion, or a lost database).
func (c *Controller) Sweep(ctx context.Context) {
	nss, err := c.Cluster.RunNamespaces(ctx)
	if err != nil {
		log.Printf("runs: sweep: %v", err)
		return
	}
	if len(nss) == 0 {
		return
	}
	live, err := c.Store.Live(ctx)
	if err != nil {
		return
	}
	keep := map[string]bool{}
	for _, r := range live {
		keep[r.Namespace] = true
	}
	for ns, id := range nss {
		if keep[ns] {
			continue
		}
		log.Printf("runs: sweep: deleting %s (run %s is not live)", ns, id)
		if err := c.Cluster.Delete(ctx, ns); err != nil {
			log.Printf("runs: sweep: %s: %v", ns, err)
		}
	}
}

// session drives a live session whose driver pod is running (build step 4): it becomes running
// once its runner answers; its statements are handed to the runner one at a time, in order, and
// their results recorded; and it stops after its idle timeout with nothing waiting or running. A
// statement waiting or running is activity. Everything is read from the database each pass, so
// a backend that restarts mid-statement picks the statement up where the runner has it.
func (c *Controller) session(ctx context.Context, r Run, now time.Time) error {
	if r.State == Pending {
		if !c.Cluster.RunnerReady(ctx, r.Namespace, r.SessionToken) {
			return nil // still starting; the pending timeout bounds this
		}
		if err := c.Store.MarkRunning(ctx, r.ID, now); err != nil {
			return err
		}
		started := now
		r.StartedAt = &started
	}
	open, err := c.Store.OpenStatements(ctx, r.ID)
	if err != nil {
		return err
	}
	for _, st := range open {
		if st.State == StatementWaiting {
			// The runner runs one statement at a time in order, so it can hold the queue; but a
			// statement is handed over only once the one before it is done, so that a backend
			// restart never leaves two in flight that the database thinks are queued.
			if err := c.Cluster.RunnerSubmit(ctx, r.Namespace, r.SessionToken, st); err != nil {
				return fmt.Errorf("handing statement %s to the runner: %w", st.ID, err)
			}
			return c.Store.StartStatement(ctx, st.ID, now)
		}
		res, err := c.Cluster.RunnerResult(ctx, r.Namespace, r.SessionToken, st.ID)
		if errors.Is(err, ErrUnknownStatement) {
			// Recorded as running but the runner never got it (the backend stopped between the two
			// writes): hand it over again; the runner ignores an id it already has.
			return c.Cluster.RunnerSubmit(ctx, r.Namespace, r.SessionToken, st)
		}
		if err != nil {
			return fmt.Errorf("asking the runner about statement %s: %w", st.ID, err)
		}
		if res.State != StatementAvailable && res.State != StatementError {
			return nil // still running: activity, and nothing after it may start
		}
		if err := c.Store.FinishStatement(ctx, st.ID, res.State, res.Output, res.Error, now); err != nil {
			return err
		}
	}
	if len(open) > 0 {
		return nil // something just finished: activity
	}
	// Idle time counts from the last activity or from when the session became ready, whichever is
	// later: a slow start is not idleness.
	last := r.CreatedAt
	for _, t := range []*time.Time{r.LastActivityAt, r.StartedAt} {
		if t != nil && t.After(last) {
			last = *t
		}
	}
	if r.Spec.IdleTimeout > 0 && now.Sub(last) > r.Spec.IdleTimeout {
		return c.finish(ctx, r, Stopped, fmt.Sprintf("idle for %s (its idle timeout)", r.Spec.IdleTimeout))
	}
	return nil
}
