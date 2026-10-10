import { useCallback, useEffect, useRef, useState } from "react";

/** The URL fragment as the UI's route ("#/applications/r1" -> ["applications", "r1"]). */
export function useRoute(): string[] {
  const read = () => window.location.hash.replace(/^#\/?/, "").split("/").filter(Boolean).map(decodeURIComponent);
  const [route, setRoute] = useState(read);
  useEffect(() => {
    const on = () => setRoute(read());
    window.addEventListener("hashchange", on);
    return () => window.removeEventListener("hashchange", on);
  }, []);
  return route;
}

export function go(path: string) {
  window.location.hash = "#/" + path;
}

/** Loads with fn, again every `every` ms while `poll` says so, and on reload(). */
export function useLoad<T>(fn: () => Promise<T>, deps: unknown[], every = 3000, poll: (v: T) => boolean = () => false) {
  const [value, setValue] = useState<T | undefined>();
  const [error, setError] = useState<Error | undefined>();
  const fnRef = useRef(fn);
  fnRef.current = fn;
  const pollRef = useRef(poll);
  pollRef.current = poll;
  const [tick, setTick] = useState(0);
  const reload = useCallback(() => setTick((t) => t + 1), []);
  useEffect(() => {
    let alive = true;
    let timer: ReturnType<typeof setTimeout> | undefined;
    const load = () => {
      fnRef.current().then(
        (v) => {
          if (!alive) return;
          setValue(v);
          setError(undefined);
          if (pollRef.current(v)) timer = setTimeout(load, every);
        },
        (e: Error) => {
          if (!alive) return;
          setError(e);
        },
      );
    };
    load();
    return () => {
      alive = false;
      if (timer) clearTimeout(timer);
    };
  }, [...deps, tick, every]);
  return { value, error, reload };
}
