package runs

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func scalaLimits() Limits {
	l := dataLimits()
	l.Scala = true
	return l
}

// ADR 0121 (docs/design-scala.md, PR 4): inline Scala and a Scala file are entry points the Scala
// runner runs, on an install that offers Scala.
func TestValidate_Scala(t *testing.T) {
	ok := map[string]Spec{
		"inline":        {Name: "s", Main: Main{InlineScala: "println(spark.range(5).count())"}},
		"a file":        {Name: "s", Main: Main{Scala: &FileRef{BackendID: "lake", Path: "jobs/etl.scala"}}},
		"a pinned file": {Name: "s", Main: Main{Scala: &FileRef{BackendID: "lake", Path: "jobs/etl.scala", SHA256: strings.Repeat("cd", 32)}}},
	}
	for name, s := range ok {
		v, err := Validate(s, scalaLimits())
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if !v.Main.IsScala() {
			t.Errorf("%s: not Scala", name)
		}
	}
	if v, _ := Validate(ok["a file"], scalaLimits()); !v.NeedsData() || v.MainFile() == nil || v.MainFile().Path != "jobs/etl.scala" {
		t.Errorf("a Scala file is read from booth-storage: %+v", v.Main)
	}
	if v, _ := Validate(ok["inline"], scalaLimits()); v.Main.InlineScalaBytes != 31 || len(v.Main.InlineScalaSHA256) != 64 || v.Main.InlinePythonBytes != 0 {
		t.Errorf("inline Scala's length and SHA-256: %+v", v.Main)
	}

	bad := map[string]Spec{
		"inline Scala and Python":  {Name: "x", Main: Main{InlineScala: "1", InlinePython: "1"}},
		"inline Scala and a file":  {Name: "x", Main: Main{InlineScala: "1", Scala: &FileRef{BackendID: "b", Path: "a.scala"}}},
		"mainClass with Scala":     {Name: "x", Main: Main{InlineScala: "1", MainClass: "a.B"}},
		"mainClass with a .scala":  {Name: "x", Main: Main{Scala: &FileRef{BackendID: "b", Path: "a.scala"}, MainClass: "a.B"}},
		"a Scala file not .scala":  {Name: "x", Main: Main{Scala: &FileRef{BackendID: "b", Path: "a.sc"}}},
		"a Scala file's bad pin":   {Name: "x", Main: Main{Scala: &FileRef{BackendID: "b", Path: "a.scala", SHA256: "x"}}},
		"inline Scala over 256KiB": {Name: "x", Main: Main{InlineScala: strings.Repeat("a", 256<<10+1)}},
	}
	for name, s := range bad {
		if _, err := Validate(s, scalaLimits()); !errors.As(err, new(ValidationError)) {
			t.Errorf("%s: %v, want a ValidationError", name, err)
		}
	}

	// An install without Scala: 422 with the reason, for inline Scala and a file alike; Python is unaffected.
	off := dataLimits()
	for name, s := range ok {
		var u Unavailable
		if _, err := Validate(s, off); !errors.As(err, &u) || !strings.Contains(err.Error(), "runs.scala.enabled") {
			t.Errorf("%s without Scala: %v", name, err)
		}
	}
	if _, err := Validate(Spec{Name: "py", Main: Main{InlinePython: "1"}}, off); err != nil {
		t.Errorf("Python without Scala: %v", err)
	}
	// A Scala file needs booth-storage, as a Python file does.
	noStorage := scalaLimits()
	noStorage.Data.Storage = false
	if _, err := Validate(ok["a file"], noStorage); !errors.As(err, new(Unavailable)) {
		t.Errorf("a Scala file without booth-storage: %v", err)
	}
}

// ADR 0121 ruling 8: a Scala driver's overhead is at least 640MiB, with the JVM caps; executors,
// JARs and Python are sized as before.
func TestScalaSizing(t *testing.T) {
	scala, err := Validate(Spec{Name: "s", Main: Main{InlineScala: "1"}}, scalaLimits())
	if err != nil {
		t.Fatal(err)
	}
	py, _ := Validate(Spec{Name: "p", Main: Main{InlinePython: "1"}}, scalaLimits())
	jar, _ := Validate(Spec{Name: "j", Main: Main{Jar: &FileRef{BackendID: "b", Path: "a.jar"}, MainClass: "a.B"}}, scalaLimits())
	if got := scala.DriverPodMemoryMi(); got != 1152 {
		t.Errorf("a Scala driver at 512m: %dMi, want 1152", got)
	}
	if py.DriverPodMemoryMi() != 896 || jar.DriverPodMemoryMi() != 896 {
		t.Errorf("Python and JAR drivers at 512m: %d %d, want 896", py.DriverPodMemoryMi(), jar.DriverPodMemoryMi())
	}
	if ScalaDriverPodMemoryMi(1024) != 1664 || ScalaDriverPodMemoryMi(2048) != 2867 {
		t.Errorf("Scala drivers at 1g and 2g: %d %d, want 1664 and 2867 (0.4 x heap above 1600m)", ScalaDriverPodMemoryMi(1024), ScalaDriverPodMemoryMi(2048))
	}
	// The footprint admission checks: the driver's Scala size plus two 896Mi executors.
	if scala.FootprintMi() != 1152+2*896 || py.FootprintMi() != 3*896 {
		t.Errorf("footprints: Scala %d, Python %d", scala.FootprintMi(), py.FootprintMi())
	}
	// The NFL sizing (driver 1g, one 1536m executor, the lakehouse lease) still fits 4096Mi.
	nfl, err := Validate(Spec{Name: "nfl", Main: Main{InlineScala: "1"}, DataAccess: &DataAccess{Lakehouse: true},
		Resources: Resources{Driver: PodSize{Memory: "1g"}, Executors: ExecutorSize{Max: ptr(1), Memory: "1536m"}}}, scalaLimits())
	if err != nil {
		t.Fatal(err)
	}
	if got := nfl.FootprintMi(); got != 4006 {
		t.Errorf("the NFL sizing in Scala: %dMi, want 4006 (fits 4096)", got)
	}
}

// The Scala runner starts the driver: the runner jar with booth.runner.Script, the script (from
// the ConfigMap or booth-storage) and the run's args, with the JVM caps.
func TestBuild_Scala(t *testing.T) {
	lim := testLimits()
	lim.Scala = true
	v, err := Validate(Spec{Name: "s", Main: Main{InlineScala: "println(1)"}, Args: []string{"a", "b"}}, lim)
	if err != nil {
		t.Fatal(err)
	}
	o, err := Build(Run{ID: "rsca", Workspace: "acme", Namespace: "bspark-rsca", Spec: v}, testCluster(), []APIEndpoint{{IP: "172.19.0.3", Port: 6443}})
	if err != nil {
		t.Fatal(err)
	}
	c := o.DriverPod.Spec.Containers[0]
	args := strings.Join(c.Args, " ")
	if !strings.HasSuffix(args, " --class booth.runner.Script /opt/booth/booth-scala-runner.jar /opt/booth/run/main.scala a b") {
		t.Errorf("Scala driver args end ...%s", args[max(0, len(args)-120):])
	}
	if !strings.Contains(args, "spark.driver.extraJavaOptions="+ScalaDriverJavaOptions) {
		t.Errorf("Scala driver args lack the JVM caps: %s", args)
	}
	if o.AppConfigMap.Data["main.scala"] != "println(1)" {
		t.Errorf("ConfigMap = %v", o.AppConfigMap.Data)
	}
	if got := c.Resources.Limits.Memory().String(); got != "1152Mi" {
		t.Errorf("Scala driver memory %s, want 1152Mi", got)
	}
	// A Python run is unchanged: no runner, no JVM caps, 896Mi.
	p := build(t, testCluster())
	pargs := strings.Join(p.DriverPod.Spec.Containers[0].Args, " ")
	if strings.Contains(pargs, "booth.runner") || strings.Contains(pargs, "extraJavaOptions") || p.DriverPod.Spec.Containers[0].Resources.Limits.Memory().String() != "896Mi" {
		t.Errorf("a Python driver changed: %s", pargs)
	}
	if _, ok := p.AppConfigMap.Data["main.scala"]; ok {
		t.Error("a Python run's ConfigMap has main.scala")
	}

	// A Scala file from booth-storage: the start gate copies it to main.scala, pinned if asked.
	sum := strings.Repeat("cd", 32)
	fv, err := Validate(Spec{Name: "etl", Main: Main{Scala: &FileRef{BackendID: "lake", Path: "jobs/etl.scala", SHA256: sum}},
		DataAccess: &DataAccess{Lakehouse: true}}, scalaLimits())
	if err != nil {
		t.Fatal(err)
	}
	file := dataTestRun(t, Main{InlinePython: "1"}) // its data plan; the entry point is the Scala file
	file.Spec = fv
	fo, err := Build(file, dataCluster(), []APIEndpoint{{IP: "172.19.0.3", Port: 6443}})
	if err != nil {
		t.Fatal(err)
	}
	fargs := strings.Join(fo.DriverPod.Spec.Containers[0].Args, " ")
	if !strings.HasSuffix(fargs, " --class booth.runner.Script /opt/booth/booth-scala-runner.jar /opt/booth/main/main.scala") {
		t.Errorf("Scala file driver args end ...%s", fargs[max(0, len(fargs)-120):])
	}
	gate := ""
	for _, ic := range fo.DriverPod.Spec.InitContainers {
		if ic.Name == "start-gate" {
			gate = strings.Join(ic.Args, " ")
		}
	}
	if !strings.Contains(gate, "--to=/opt/booth/main/main.scala") || !strings.Contains(gate, "--sha256="+sum) || !strings.Contains(gate, "--source=main.scala (booth-storage lake/jobs/etl.scala)") {
		t.Errorf("Scala file start gate: %s", gate)
	}
}

// ADR 0121 ruling 5: a session is Python- or Scala-flavoured, chosen at creation.
func TestValidateSession_Language(t *testing.T) {
	l := testLimits()
	l.SessionIdleTimeout, l.SessionMaxLifetime = 20*time.Minute, 12*time.Hour
	l.Scala = true
	for given, want := range map[string]string{"": "python", "python": "python", "scala": "scala"} {
		v, err := ValidateSession(SessionSpec{Name: "s", Language: given}, l)
		if err != nil || v.Language != want {
			t.Errorf("language %q: %q %v, want %q", given, v.Language, err, want)
		}
	}
	if _, err := ValidateSession(SessionSpec{Name: "s", Language: "r"}, l); !errors.As(err, new(ValidationError)) {
		t.Errorf("language r: %v", err)
	}
	off := l
	off.Scala = false
	var u Unavailable
	if _, err := ValidateSession(SessionSpec{Name: "s", Language: "scala"}, off); !errors.As(err, &u) || !strings.Contains(err.Error(), "runs.scala.enabled") {
		t.Errorf("a Scala session without Scala: %v", err)
	}
	if _, err := ValidateSession(SessionSpec{Name: "s"}, off); err != nil {
		t.Errorf("a Python session without Scala: %v", err)
	}
	py, _ := ValidateSession(SessionSpec{Name: "s"}, l)
	sc, _ := ValidateSession(SessionSpec{Name: "s", Language: "scala"}, l)
	if strings.Join(py.StatementKinds(), ",") != "sql,python" || strings.Join(sc.StatementKinds(), ",") != "sql,scala" {
		t.Errorf("statement kinds: %v %v", py.StatementKinds(), sc.StatementKinds())
	}
	// A session recorded before ADR 0121 has no language: Python.
	if strings.Join((Validated{}).StatementKinds(), ",") != "sql,python" {
		t.Error("a session without a language isn't Python")
	}
	if sc.DriverPodMemoryMi() != 1152 || py.DriverPodMemoryMi() != 896 {
		t.Errorf("session drivers: Scala %d, Python %d", sc.DriverPodMemoryMi(), py.DriverPodMemoryMi())
	}
}

// A Scala session's driver runs booth.runner.Session from the runner jar, with the JVM caps; a
// Python session's runs the Python runner as before.
func TestBuild_ScalaSession(t *testing.T) {
	l := testLimits()
	l.SessionIdleTimeout, l.SessionMaxLifetime = 20*time.Minute, 12*time.Hour
	l.Scala = true
	v, err := ValidateSession(SessionSpec{Name: "s", Language: "scala"}, l)
	if err != nil {
		t.Fatal(err)
	}
	o, err := Build(Run{ID: "rses", Kind: "session", Workspace: "acme", Namespace: "bspark-rses", Spec: v, SessionToken: "tok"},
		testCluster(), []APIEndpoint{{IP: "172.19.0.3", Port: 6443}})
	if err != nil {
		t.Fatal(err)
	}
	c := o.DriverPod.Spec.Containers[0]
	args := strings.Join(c.Args, " ")
	if !strings.HasSuffix(args, " --class booth.runner.Session /opt/booth/booth-scala-runner.jar") || !strings.Contains(args, "spark.driver.extraJavaOptions="+ScalaDriverJavaOptions) {
		t.Errorf("Scala session driver args: %s", args)
	}
	if c.Resources.Limits.Memory().String() != "1152Mi" {
		t.Errorf("Scala session driver memory: %s", c.Resources.Limits.Memory())
	}
	pv, _ := ValidateSession(SessionSpec{Name: "s"}, l)
	po, err := Build(Run{ID: "rpys", Kind: "session", Workspace: "acme", Namespace: "bspark-rpys", Spec: pv, SessionToken: "tok"},
		testCluster(), []APIEndpoint{{IP: "172.19.0.3", Port: 6443}})
	if err != nil {
		t.Fatal(err)
	}
	pargs := strings.Join(po.DriverPod.Spec.Containers[0].Args, " ")
	if !strings.HasSuffix(pargs, " /opt/booth/run/main.py") || strings.Contains(pargs, "booth.runner") || strings.Contains(pargs, "extraJavaOptions") {
		t.Errorf("a Python session's driver changed: %s", pargs)
	}
}
