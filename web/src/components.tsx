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

/**
 * A term's plain-language explanation, on hover and on keyboard focus. A title attribute until
 * booth-design's shared tooltip component lands; swap it in here, and every hint follows.
 */
export function Hint({ text }: { text: string }) {
  return (
    <span className="hint" title={text} aria-label={text} role="img" tabIndex={0}>
      ?
    </span>
  );
}

/** A Go duration as people say it: "6h0m0s" -> "6h", "1h30m0s" -> "1h30m", "20m0s" -> "20m". */
export function duration(d?: string) {
  if (!d) return "";
  return d.replace(/(\d)m0s$/, "$1m").replace(/(\d)h0m$/, "$1h");
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
