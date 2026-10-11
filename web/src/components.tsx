import type { ReactNode } from "react";
import type { State, StatementState } from "./api";

export function Badge({ state }: { state: State | StatementState }) {
  return <span className={"badge badge-" + state}>{state}</span>;
}

export function ErrorBox({ error }: { error?: Error }) {
  if (!error) return null;
  return (
    <div className="error" role="alert">
      {error.message}
    </div>
  );
}

export function Field({ label, children, hint }: { label: string; children: ReactNode; hint?: string }) {
  return (
    <label className="field">
      <span className="field-label">{label}</span>
      {children}
      {hint && <span className="field-hint">{hint}</span>}
    </label>
  );
}

export function when(t?: string) {
  if (!t) return "";
  const d = new Date(t);
  return isNaN(d.getTime()) ? t : d.toLocaleString();
}

/**
 * Text the run's code produced (its log, a statement's output or traceback), shown as text: React
 * escapes it, so markup in it is never rendered (docs/design-v0.md item 5).
 */
export function Pre({ text, label }: { text: string; label: string }) {
  return (
    <pre className="pre" aria-label={label}>
      {text}
    </pre>
  );
}
