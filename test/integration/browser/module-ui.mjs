// Build step 6: the module's own UI in a real Chromium, through booth-core's iframe proxy and
// assertion, inside an iframe on core's own origin (standing in for the shell, which is same-origin
// with core, ADR 0069), as spark-ui.mjs browses a Spark UI.
//
//   node module-ui.mjs <core base URL> <editor's iframe URL> <viewer's iframe URL> <run name>
//
// The editor submits an application through the UI's form. Its code prints markup that would run
// script if it were rendered (an <img onerror>): the run page must show it as text. The viewer then
// sees the run listed and no way to submit. Every request the UI makes stays under /iframe/spark/,
// and the page logs no error (a CSP violation included).
import { chromium } from "playwright";

const [base, editorURL, viewerURL, runName] = process.argv.slice(2);
if (!base || !editorURL || !viewerURL || !runName) {
  console.error("usage: node module-ui.mjs <core base URL> <editor iframe URL> <viewer iframe URL> <run name>");
  process.exit(2);
}
const failures = [];
const check = (ok, what) => {
  console.log(`${ok ? "ok  " : "FAIL"} ${what}`);
  if (!ok) failures.push(what);
};
const marker = "UI-RUN-DONE";
const payload = `<img src=x onerror="window.top.__pwned=1">`;
const code = `print(${JSON.stringify(payload)}, flush=True)\nprint("${marker}", flush=True)\n`;

async function shell(ctx, url) {
  const page = await ctx.newPage();
  const escaped = [];
  const errors = [];
  page.on("response", (resp) => {
    const u = new URL(resp.url());
    if (u.pathname === "/__booth_parent") return;
    if (!u.pathname.startsWith("/iframe/spark/")) escaped.push(u.pathname);
    // A failed request, named (the console only says "Failed to load resource").
    if (resp.status() >= 400) {
      const what = `${resp.status()} ${u.pathname}${u.search.replace(/booth_iframe_token=[^&]*/, "booth_iframe_token=<redacted>")}`;
      errors.push(what);
      resp.text().then((t) => console.log(`failed request ${what}: ${t.slice(0, 300)}`), () => {});
    }
  });
  page.on("console", (m) => m.type() === "error" && errors.push(m.text()));
  page.on("pageerror", (e) => errors.push(String(e)));
  await page.route(`${base}/__booth_parent`, (route) =>
    route.fulfill({
      contentType: "text/html",
      body: `<!doctype html><title>shell stand-in</title>
<iframe id="ui" src="${url}" style="width:1300px;height:1000px"
  sandbox="allow-scripts allow-same-origin allow-forms allow-popups allow-downloads"></iframe>`,
    }),
  );
  await page.goto(`${base}/__booth_parent`);
  return { page, ui: page.frameLocator("#ui"), escaped, errors };
}

const browser = await chromium.launch();
try {
  const ctx = await browser.newContext();
  const ed = await shell(ctx, editorURL);
  await ed.ui.getByRole("heading", { name: "Applications" }).waitFor({ timeout: 60_000 });
  check(true, "the editor sees the module's UI through core (Applications, Sessions)");
  check((await ed.ui.getByText(/editor-user, editor in/).count()) >= 1, "it names the signed-in person and role, from core's assertion");

  await ed.ui.getByRole("button", { name: "New application" }).click();
  await ed.ui.getByLabel("Name", { exact: true }).fill(runName);
  await ed.ui.getByLabel("PySpark code").fill(code);
  await ed.ui.getByLabel("Executors (most)").fill("0");
  await ed.ui.getByRole("button", { name: "Submit" }).click();
  await ed.ui.getByRole("heading", { name: runName }).waitFor({ timeout: 30_000 });
  check(true, "the form submitted the application and opened its page");

  const log = ed.ui.getByLabel("Driver log");
  let text = "";
  for (let i = 0; i < 150 && !text.includes(marker); i++) {
    await new Promise((r) => setTimeout(r, 2000));
    text = (await log.textContent().catch(() => "")) ?? "";
  }
  check(text.includes(marker), "the run's log reached the page");
  check(text.includes(payload), "the run's log shows the markup its code printed, as text");
  check((await log.locator("img, script").count()) === 0, "... and no element from it exists in the page");
  check((await ed.page.evaluate(() => window.__pwned)) === undefined, "... and its script never ran");
  await ed.ui.getByText("succeeded").first().waitFor({ timeout: 120_000 });
  check(true, "the run page follows the run to succeeded");

  const vw = await shell(ctx, viewerURL);
  await vw.ui.getByRole("heading", { name: "Applications" }).waitFor({ timeout: 60_000 });
  await vw.ui.getByRole("link", { name: runName }).waitFor({ timeout: 30_000 });
  check(true, "the viewer sees the editor's run listed");
  check((await vw.ui.getByRole("button", { name: "New application" }).count()) === 0, "the viewer has no way to submit");
  check((await vw.ui.getByText(/needs editor or above/).count()) === 1, "... and is told why");
  await vw.ui.getByRole("link", { name: runName }).click();
  await vw.ui.getByText("Its submitter, the workspace's owners and platform operators can read its log.").waitFor({ timeout: 30_000 });
  check(true, "the viewer gets the run's state, not its log");

  for (const s of [ed, vw]) {
    check(s.escaped.length === 0, `every request stayed under /iframe/spark/ (${s.escaped.join(", ") || "none escaped"})`);
    check(s.errors.length === 0, `no console error, CSP violations included (${s.errors.join(" | ") || "none"})`);
  }
} finally {
  await browser.close();
}
if (failures.length) {
  console.error(`${failures.length} check(s) failed`);
  process.exit(1);
}
console.log("all module UI browser checks passed");
