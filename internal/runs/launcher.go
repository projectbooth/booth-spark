package runs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Launcher creates and removes run namespaces with the backend's own, fenced rights
// (docs/design-v0.md item 3, charts/booth-spark/templates/rbac.yaml): cluster-wide it may only
// create and delete its own labelled namespaces, read the kubernetes EndpointSlice and the driver
// ClusterRole, and create RoleBindings to two fixed ClusterRoles inside its own namespaces. Every
// other call it makes runs on the RoleBinding it creates in each run's namespace first.
type Launcher struct {
	Kube    kubernetes.Interface
	Cluster Cluster

	mu      sync.Mutex
	roleUID string
}

// DriverState is what the controller needs from a run's driver pod.
type DriverState struct {
	Exists  bool
	Phase   corev1.PodPhase
	Reason  string // why a pending pod can't start, or why a finished one failed
	Fatal   bool   // a pending pod that will never start (bad image, bad config)
	Created time.Time
}

func (l *Launcher) driverRoleUID(ctx context.Context) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.roleUID != "" {
		return l.roleUID, nil
	}
	cr, err := l.Kube.RbacV1().ClusterRoles().Get(ctx, l.Cluster.DriverClusterRole, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("reading ClusterRole %s (the run namespaces' owner): %w", l.Cluster.DriverClusterRole, err)
	}
	l.roleUID = string(cr.UID)
	return l.roleUID, nil
}

// APIEndpoints reads the API server's addresses from the "kubernetes" EndpointSlice in "default"
// (v1 Endpoints is deprecated from Kubernetes 1.33).
func (l *Launcher) APIEndpoints(ctx context.Context) ([]APIEndpoint, error) {
	es, err := l.Kube.DiscoveryV1().EndpointSlices("default").Get(ctx, "kubernetes", metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("reading the kubernetes EndpointSlice: %w", err)
	}
	var port int32
	for _, p := range es.Ports {
		if p.Port != nil && (p.Name == nil || *p.Name == "https") {
			port = *p.Port
		}
	}
	var out []APIEndpoint
	for _, e := range es.Endpoints {
		if e.Conditions.Ready != nil && !*e.Conditions.Ready {
			continue
		}
		for _, a := range e.Addresses {
			if es.AddressType == discoveryv1.AddressTypeIPv4 {
				out = append(out, APIEndpoint{IP: a, Port: port})
			}
		}
	}
	if len(out) == 0 || port == 0 {
		return nil, fmt.Errorf("the kubernetes EndpointSlice lists no ready IPv4 address")
	}
	return out, nil
}

// Launch creates run r's namespace and everything in it. It is idempotent: an object that
// already exists is left as it is, so a controller that restarts mid-launch simply launches again.
func (l *Launcher) Launch(ctx context.Context, r Run) error {
	uid, err := l.driverRoleUID(ctx)
	if err != nil {
		return err
	}
	api, err := l.APIEndpoints(ctx)
	if err != nil {
		return err
	}
	c := l.Cluster
	c.DriverClusterRoleUID = typesUID(uid)
	o, err := Build(r, c, api)
	if err != nil {
		return err
	}
	k := l.Kube
	ns := r.Namespace
	steps := []struct {
		what string
		do   func() error
	}{
		{"namespace", func() error {
			_, err := k.CoreV1().Namespaces().Create(ctx, o.Namespace, metav1.CreateOptions{})
			return err
		}},
		{"controller binding", func() error {
			_, err := k.RbacV1().RoleBindings(ns).Create(ctx, o.ControllerBinding, metav1.CreateOptions{})
			return err
		}},
		{"driver account", func() error {
			_, err := k.CoreV1().ServiceAccounts(ns).Create(ctx, o.DriverAccount, metav1.CreateOptions{})
			return err
		}},
		{"executor account", func() error {
			_, err := k.CoreV1().ServiceAccounts(ns).Create(ctx, o.ExecutorAccount, metav1.CreateOptions{})
			return err
		}},
		{"driver binding", func() error {
			_, err := k.RbacV1().RoleBindings(ns).Create(ctx, o.DriverBinding, metav1.CreateOptions{})
			return err
		}},
		{"quota", func() error {
			_, err := k.CoreV1().ResourceQuotas(ns).Create(ctx, o.Quota, metav1.CreateOptions{})
			return err
		}},
		{"limit range", func() error {
			_, err := k.CoreV1().LimitRanges(ns).Create(ctx, o.LimitRange, metav1.CreateOptions{})
			return err
		}},
	}
	for _, p := range o.NetworkPolicies {
		p := p
		steps = append(steps, struct {
			what string
			do   func() error
		}{"network policy " + p.Name, func() error {
			_, err := k.NetworkingV1().NetworkPolicies(ns).Create(ctx, p, metav1.CreateOptions{})
			return err
		}})
	}
	steps = append(steps,
		struct {
			what string
			do   func() error
		}{"app config map", func() error {
			_, err := k.CoreV1().ConfigMaps(ns).Create(ctx, o.AppConfigMap, metav1.CreateOptions{})
			return err
		}},
		struct {
			what string
			do   func() error
		}{"driver service", func() error {
			_, err := k.CoreV1().Services(ns).Create(ctx, o.DriverService, metav1.CreateOptions{})
			return err
		}},
		// Last: the pod starts only once its namespace is fenced.
		struct {
			what string
			do   func() error
		}{"driver pod", func() error {
			_, err := k.CoreV1().Pods(ns).Create(ctx, o.DriverPod, metav1.CreateOptions{})
			return err
		}},
	)
	for i, s := range steps {
		// Right after the controller binding is created, the authorizer may not have seen it yet:
		// a Forbidden on the next few calls is retried briefly, never ignored.
		err := retryForbidden(ctx, i > 1, s.do)
		if err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("creating %s: %w", s.what, err)
		}
	}
	return nil
}

func retryForbidden(ctx context.Context, retry bool, f func() error) error {
	var err error
	for attempt := 0; attempt < 20; attempt++ {
		err = f()
		if err == nil || !retry || !apierrors.IsForbidden(err) {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(250 * time.Millisecond):
		}
	}
	return err
}

// Delete removes a run's namespace, and with it everything in it. Already gone is success.
func (l *Launcher) Delete(ctx context.Context, ns string) error {
	err := l.Kube.CoreV1().Namespaces().Delete(ctx, ns, metav1.DeleteOptions{PropagationPolicy: ptr(metav1.DeletePropagationBackground)})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// Driver reports the state of a run's driver pod.
func (l *Launcher) Driver(ctx context.Context, ns string) (DriverState, error) {
	p, err := l.Kube.CoreV1().Pods(ns).Get(ctx, DriverPod, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return DriverState{}, nil
	}
	if err != nil {
		return DriverState{}, err
	}
	st := DriverState{Exists: true, Phase: p.Status.Phase, Created: p.CreationTimestamp.Time}
	for _, cs := range p.Status.ContainerStatuses {
		if w := cs.State.Waiting; w != nil {
			switch w.Reason {
			case "ErrImagePull", "ImagePullBackOff", "InvalidImageName", "CreateContainerConfigError", "CreateContainerError":
				st.Fatal, st.Reason = true, w.Reason+": "+w.Message
			}
		}
		if t := cs.State.Terminated; t != nil && p.Status.Phase == corev1.PodFailed {
			st.Reason = fmt.Sprintf("the driver exited with code %d (%s)", t.ExitCode, t.Reason)
		}
	}
	if p.Status.Phase == corev1.PodPending && st.Reason == "" {
		for _, c := range p.Status.Conditions {
			if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse {
				st.Reason = "not scheduled: " + c.Message
			}
		}
	}
	return st, nil
}

// LogTail returns up to maxBytes from the end of the driver's log ("" if there is none).
func (l *Launcher) LogTail(ctx context.Context, ns string, maxBytes int64) (string, error) {
	// The API can only limit bytes from the start of a log; the last lines, trimmed to maxBytes,
	// are what's useful at the end of a run.
	tail := int64(20000)
	return l.logs(ctx, ns, &corev1.PodLogOptions{Container: "driver", TailLines: &tail}, maxBytes)
}

// Logs streams up to maxBytes of the driver's log, last tailLines lines.
func (l *Launcher) Logs(ctx context.Context, ns string, tailLines, maxBytes int64) (string, error) {
	return l.logs(ctx, ns, &corev1.PodLogOptions{Container: "driver", TailLines: &tailLines}, maxBytes)
}

func (l *Launcher) logs(ctx context.Context, ns string, opts *corev1.PodLogOptions, maxBytes int64) (string, error) {
	rc, err := l.Kube.CoreV1().Pods(ns).GetLogs(DriverPod, opts).Stream(ctx)
	if apierrors.IsNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer rc.Close()
	// Keep the last maxBytes of what the (line-limited) request returns, reading at most 64 MiB.
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, io.LimitReader(rc, 64<<20)); err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	b := buf.Bytes()
	if int64(len(b)) > maxBytes {
		b = b[int64(len(b))-maxBytes:]
	}
	return string(bytes.ToValidUTF8(b, nil)), nil
}

// RunNamespaces lists the namespaces this install created.
func (l *Launcher) RunNamespaces(ctx context.Context) (map[string]string, error) {
	list, err := l.Kube.CoreV1().Namespaces().List(ctx, metav1.ListOptions{LabelSelector: LabelInstance + "=" + l.Cluster.Instance})
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, ns := range list.Items {
		out[ns.Name] = ns.Labels[LabelRun]
	}
	return out, nil
}
