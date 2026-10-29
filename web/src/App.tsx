import { api } from "./api";
import { ErrorBox } from "./components";
import { useLoad, useRoute } from "./hooks";
import { ApplicationPage, NewApplicationPage, NewSessionPage, RunsPage, SessionPage } from "./pages";

export default function App() {
  const me = useLoad(api.me, []);
  const route = useRoute();
  if (me.error) return <ErrorBox error={me.error} />;
  if (!me.value) return <p className="muted">Loading…</p>;
  const m = me.value;
  let page;
  switch (route[0]) {
    case "applications":
      page = route[1] ? <ApplicationPage id={route[1]} me={m} /> : <RunsPage me={m} />;
      break;
    case "sessions":
      page = route[1] ? <SessionPage id={route[1]} me={m} /> : <RunsPage me={m} />;
      break;
    case "new":
      // #/new/<id>: Run again, filled in from that run.
      page = m.canSubmit ? <NewApplicationPage key={route[1] ?? ""} me={m} from={route[1]} /> : <RunsPage me={m} />;
      break;
    case "new-session":
      page = m.canSubmit ? <NewSessionPage me={m} /> : <RunsPage me={m} />;
      break;
    default:
      page = <RunsPage me={m} />;
  }
  return (
    <main>
      <header className="header">
        <h1>Spark</h1>
        <span className="muted">
          {m.displayName}, {m.role} in <strong>{m.workspace}</strong>
          {m.operator && ", platform operator"}
        </span>
      </header>
      {page}
    </main>
  );
}
