// Package runs is booth-spark's run model and controller (docs/design-v0.md items 1, 3 and 7):
// a run (in step 3, a batch application) is a row in the module's own database and, while it is
// live, a namespace of its own holding one driver and 0 to N executors. The namespace is deleted
// when the run ends, which ends everything in it.
package runs

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Spec is what a caller submits: POST /v1/applications' body (docs/design-v0.md item 6). Every
// field is validated against Limits before anything is created.
type Spec struct {
	Name      string            `json:"name"`
	Main      Main              `json:"main"`
	Args      []string          `json:"args,omitempty"`
	Resources Resources         `json:"resources,omitempty"`
	Conf      map[string]string `json:"conf,omitempty"`
	// DataAccess asks for the run's data access (database, lakehouse, storage locations), as its
	// submitter, capped at editor (docs/design-v0.md item 4).
	DataAccess *DataAccess `json:"dataAccess,omitempty"`
	// MaxDuration bounds the run (Go duration, e.g. "30m"); at most Limits.MaxDuration, which is
	// also the default.
	MaxDuration string `json:"maxDuration,omitempty"`
}

// Main is the application's entry point: inline Python, or a Python file or a JAR (with its main
// class) in booth-storage, read as the run's submitter when it starts.
type Main struct {
	InlinePython string   `json:"inlinePython,omitempty"`
	Python       *FileRef `json:"python,omitempty"`
	Jar          *FileRef `json:"jar,omitempty"`
	MainClass    string   `json:"mainClass,omitempty"`
}

// Resources are what a caller may size, within Limits.
type Resources struct {
	Driver    PodSize      `json:"driver,omitempty"`
	Executors ExecutorSize `json:"executors,omitempty"`
}

// PodSize is a JVM heap size, Spark's notation ("512m", "1g").
type PodSize struct {
	Memory string `json:"memory,omitempty"`
}

// ExecutorSize is the dynamic-allocation range and each executor's heap.
type ExecutorSize struct {
	Min    *int   `json:"min,omitempty"`
	Max    *int   `json:"max,omitempty"`
	Memory string `json:"memory,omitempty"`
}

// SessionSpec is what a caller submits to start a session: POST /v1/sessions' body
// (docs/design-v0.md item 6). A session runs no code of its own; statements go to it afterwards.
type SessionSpec struct {
	Name       string            `json:"name"`
	Resources  Resources         `json:"resources,omitempty"`
	Conf       map[string]string `json:"conf,omitempty"`
	DataAccess *DataAccess       `json:"dataAccess,omitempty"`
	// IdleTimeout stops the session after this long with no statement waiting or running and no
	// new one (Go duration); at most Limits.SessionIdleTimeout, which is also the default.
	IdleTimeout string `json:"idleTimeout,omitempty"`
	// MaxLifetime stops the session after this long regardless; at most Limits.SessionMaxLifetime.
	MaxLifetime string `json:"maxLifetime,omitempty"`
}

// Limits are the chart's `runs` and `sessions` settings that bound what a caller may ask for.
type Limits struct {
	MaxExecutors       int
	DefaultExecutors   int
	DriverMemory       string // default heap
	ExecutorMemory     string // default heap
	MaxMemory          string // largest heap a caller may ask for, driver or executor
	MaxDuration        time.Duration
	SessionIdleTimeout time.Duration
	SessionMaxLifetime time.Duration
	// Data is which data paths this install offers runs.
	Data DataLimits
}

// Validated is a Spec with every default applied and every value checked.
type Validated struct {
	Spec
	DriverHeapMi   int
	ExecutorHeapMi int
	MinExecutors   int
	MaxExecutors   int
	Duration       time.Duration
	// IdleTimeout is a session's (zero for an application).
	IdleTimeout time.Duration
}

// A ValidationError is the caller's mistake (400). An Unavailable is something this version can't
// do yet (422).
type ValidationError struct{ msg string }

func (e ValidationError) Error() string { return e.msg }

type Unavailable struct{ msg string }

func (e Unavailable) Error() string { return e.msg }

func invalid(format string, a ...any) error { return ValidationError{fmt.Sprintf(format, a...)} }

// AllowedConf are the Spark settings a caller may set. Everything else is the module's: the
// master, namespace, images, ports, accounts, UI settings and resources are what make a run
// isolated and bounded, so none of them can be overridden.
var AllowedConf = map[string]bool{
	"spark.sql.shuffle.partitions":              true,
	"spark.default.parallelism":                 true,
	"spark.sql.adaptive.enabled":                true,
	"spark.sql.session.timeZone":                true,
	"spark.sql.ansi.enabled":                    true,
	"spark.serializer":                          false, // deliberately not settable: a class name runs code
	"spark.sql.execution.arrow.pyspark.enabled": true,
}

const (
	maxInlinePython = 256 << 10
	maxArgs         = 64
	maxArgLen       = 4096
	maxConfValue    = 256
	minHeapMi       = 512
)

var (
	memRE  = regexp.MustCompile(`^([1-9][0-9]{0,5})([mg])$`)
	nameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ._-]{0,99}$`)
)

// HeapMi parses Spark's memory notation into MiB.
func HeapMi(s string) (int, error) {
	m := memRE.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("%q is not a memory size like 512m or 1g", s)
	}
	v, _ := strconv.Atoi(m[1])
	if m[2] == "g" {
		v *= 1024
	}
	return v, nil
}

// PodMemoryMi is a Spark pod's memory: the heap plus Spark's overhead for a Python application
// (spark.kubernetes.memoryOverheadFactor 0.4 for non-JVM jobs, at least 384MiB).
func PodMemoryMi(heapMi int) int {
	overhead := heapMi * 4 / 10
	if overhead < 384 {
		overhead = 384
	}
	return heapMi + overhead
}

// Validate applies defaults and checks an application's Spec against l.
func Validate(s Spec, l Limits) (Validated, error) {
	if !nameRE.MatchString(s.Name) {
		return Validated{Spec: s}, invalid("name: 1 to 100 characters, letters, digits, spaces, '.', '_' or '-', starting with a letter or digit")
	}
	if err := validateMain(s.Main, l); err != nil {
		return Validated{Spec: s}, err
	}
	if len(s.Args) > maxArgs {
		return Validated{Spec: s}, invalid("at most %d args", maxArgs)
	}
	for i, a := range s.Args {
		if len(a) > maxArgLen || strings.ContainsRune(a, 0) {
			return Validated{Spec: s}, invalid("args[%d] is longer than %d bytes or contains NUL", i, maxArgLen)
		}
	}
	v, err := validateCommon(s, l)
	if err != nil {
		return v, err
	}
	v.Duration = l.MaxDuration
	if s.MaxDuration != "" {
		d, err := time.ParseDuration(s.MaxDuration)
		if err != nil || d <= 0 || d > l.MaxDuration {
			return v, invalid("maxDuration: a duration up to %s", l.MaxDuration)
		}
		v.Duration = d
	}
	return v, nil
}

// ValidateSession applies defaults and checks a SessionSpec against l. The result carries no
// code: a session's driver runs the module's session runner (internal/runs/runner).
func ValidateSession(s SessionSpec, l Limits) (Validated, error) {
	if !nameRE.MatchString(s.Name) {
		return Validated{}, invalid("name: 1 to 100 characters, letters, digits, spaces, '.', '_' or '-', starting with a letter or digit")
	}
	v, err := validateCommon(Spec{Name: s.Name, Resources: s.Resources, Conf: s.Conf, DataAccess: s.DataAccess}, l)
	if err != nil {
		return v, err
	}
	dur := func(field, given string, max time.Duration) (time.Duration, error) {
		if given == "" {
			return max, nil
		}
		d, err := time.ParseDuration(given)
		if err != nil || d < time.Second || d > max {
			return 0, invalid("%s: a duration from 1s up to %s", field, max)
		}
		return d, nil
	}
	if v.IdleTimeout, err = dur("idleTimeout", s.IdleTimeout, l.SessionIdleTimeout); err != nil {
		return v, err
	}
	if v.Duration, err = dur("maxLifetime", s.MaxLifetime, l.SessionMaxLifetime); err != nil {
		return v, err
	}
	return v, nil
}

// validateCommon checks what applications and sessions share: conf, data access and resources.
func validateCommon(s Spec, l Limits) (Validated, error) {
	v := Validated{Spec: s}
	keys := make([]string, 0, len(s.Conf))
	for k := range s.Conf {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !AllowedConf[k] {
			return v, invalid("conf %q can't be set; allowed: %s", k, strings.Join(allowedConfList(), ", "))
		}
		if len(s.Conf[k]) > maxConfValue || strings.ContainsFunc(s.Conf[k], unicode.IsControl) {
			return v, invalid("conf %q: value too long or not a single line", k)
		}
	}
	if err := validateData(s.DataAccess, l); err != nil {
		return v, err
	}
	maxHeap, err := HeapMi(l.MaxMemory)
	if err != nil {
		return v, fmt.Errorf("runs limits: %w", err)
	}
	heap := func(field, given, def string) (int, error) {
		if given == "" {
			given = def
		}
		mi, err := HeapMi(given)
		if err != nil {
			return 0, invalid("%s: %v", field, err)
		}
		if mi < minHeapMi || mi > maxHeap {
			return 0, invalid("%s: between %dm and %s", field, minHeapMi, l.MaxMemory)
		}
		return mi, nil
	}
	if v.DriverHeapMi, err = heap("resources.driver.memory", s.Resources.Driver.Memory, l.DriverMemory); err != nil {
		return v, err
	}
	if v.ExecutorHeapMi, err = heap("resources.executors.memory", s.Resources.Executors.Memory, l.ExecutorMemory); err != nil {
		return v, err
	}
	v.MinExecutors, v.MaxExecutors = 0, l.DefaultExecutors
	if p := s.Resources.Executors.Min; p != nil {
		v.MinExecutors = *p
	}
	if p := s.Resources.Executors.Max; p != nil {
		v.MaxExecutors = *p
	}
	if v.MinExecutors < 0 || v.MaxExecutors < v.MinExecutors || v.MaxExecutors > l.MaxExecutors {
		return v, invalid("resources.executors: need 0 <= min <= max <= %d", l.MaxExecutors)
	}
	return v, nil
}

// FootprintMi is the most memory a run's pods can use at once: the driver plus every executor it
// may scale to, each with its data-access sidecars. The admission budget (Limits, memoryBudget) is checked against it.
func (v Validated) FootprintMi() int {
	return PodMemoryMi(v.DriverHeapMi) + v.MaxExecutors*PodMemoryMi(v.ExecutorHeapMi) + (1+v.MaxExecutors)*v.SidecarMi()
}

func allowedConfList() []string {
	var out []string
	for k, ok := range AllowedConf {
		if ok {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// ErrNotFound is returned for an unknown run (or one in another workspace).
var ErrNotFound = errors.New("run not found")
