// Build step 2's proof (docs/design-v0.md item 5): a real Chromium browses a real Spark driver's UI
// through booth-core's iframe proxy and booth-spark's proxy, inside an iframe on core's own origin
// (standing in for the shell, which is same-origin with core by construction, ADR 0069), the way
// booth-streamlit's browser test does.
//
//   node spark-ui.mjs <core base URL> <submitter's iframe URL> <operator's iframe URL>
//
// The iframe URLs are core's own (GET /api/modules/spark/iframe-url), minted in-cluster a moment
// earlier from real Keycloak tokens; core's navigation token is valid for one minute.
import { chromium } from "playwright";

const [base, submitterURL, operatorURL] = process.argv.slice(2);
if (!base || !submitterURL || !operatorURL) {
  console.error("usage: node spark-ui.mjs <core base URL> <submitter iframe URL> <operator iframe URL>");
  process.exit(2);
}
const prefix = "/iframe/spark/runs/proof-1/ui";
// Core issues /iframe/spark/?<token>; its entry handler accepts any path under the module, so
// point the same token at the run's UI.
const toRun = (u) => u.replace(/^\/iframe\/spark\/\?/, `${prefix}/?`);

const failures = [];
const check = (ok, what) => {
  console.log(`${ok ? "ok  " : "FAIL"} ${what}`);
  if (!ok) failures.push(what);
};

const browser = await chromium.launch();
try {
  const ctx = await browser.newContext();
  const page = await ctx.newPage();
  // Every request the embedded UI makes: it must stay under the run's prefix (a root-relative URL
  // would escape it, the failure mode contracts/ui-integration.md warns about) and succeed.
  const escaped = [];
  const failed = [];
  page.on("response", (resp) => {
    const u = new URL(resp.url());
    if (u.pathname === "/__booth_parent") return;
    if (!u.pathname.startsWith(prefix + "/") && !u.pathname.startsWith("/iframe/spark/?")) escaped.push(u.pathname);
    // The entry hop is core's redirect to the clean URL, then Spark's redirect to /jobs/.
    if (resp.status() >= 400) failed.push(`${resp.status()} ${u.pathname}`);
  });
  const consoleErrors = [];
  page.on("console", (m) => m.type() === "error" && consoleErrors.push(m.text()));

  await page.route(`${base}/__booth_parent`, (route) =>
    route.fulfill({
      contentType: "text/html",
      body: `<!doctype html><title>shell stand-in</title>
<iframe id="ui" src="${toRun(submitterURL)}" style="width:1400px;height:1000px"
  sandbox="allow-scripts allow-same-origin allow-forms allow-popups allow-downloads"></iframe>`,
    }),
  );
  await page.goto(`${base}/__booth_parent`);
  const ui = page.frameLocator("#ui");

  await ui.locator("table").getByText("booth proof job").first().waitFor({ timeout: 60_000 });
  check(true, "the submitter sees the jobs page, with the described job");

  // The Executors tab is built by JavaScript from the REST API (utils.js uiRoot): the strongest
  // check that the UI's own scripts resolve under the prefix.
  await ui.getByRole("link", { name: "Executors" }).click();
  await ui.locator("#active-executors-table").getByText("driver", { exact: true }).first().waitFor({ timeout: 60_000 });
  check(true, "the Executors tab loaded its table from the REST API (driver row)");

  await ui.getByRole("link", { name: "Environment" }).click();
  await ui.getByText("spark.booth.proof.token").first().waitFor({ timeout: 30_000 });
  const envText = (await ui.locator("body").textContent()) ?? "";
  check(!envText.includes("proof-value-must-not-appear"), "the Environment page redacts a secret-named conf value");

  await ui.getByRole("link", { name: "SQL / DataFrame" }).click();
  await ui.getByText("collect").first().waitFor({ timeout: 30_000 });
  check(true, "the SQL tab lists the query");

  // The kill links must not be offered (spark.ui.killEnabled=false).
  await ui.getByRole("link", { name: "Jobs" }).click();
  await ui.locator("table").getByText("booth proof job").first().waitFor({ timeout: 30_000 });
  check((await ui.locator("a.kill-link").count()) === 0, "no kill links are rendered");

  console.log(`requests outside the prefix: ${JSON.stringify(escaped)}`);
  check(escaped.length === 0, "every request the UI made stayed under the run's prefix");
  console.log(`failed responses: ${JSON.stringify(failed)}`);
  check(failed.length === 0, "no request failed");
  if (consoleErrors.length) console.log(`console errors (informational): ${JSON.stringify(consoleErrors)}`);
  await ctx.close();

  // A platform operator in the same workspace is refused (only the submitter, ADR 0110 ruling 4).
  const octx = await browser.newContext();
  const opage = await octx.newPage();
  const first = await opage.goto(`${base}${toRun(operatorURL)}`);
  // Core answers the token URL with a redirect to the clean URL; the module's 403 is the final hop.
  check(first.status() === 403, `a platform operator gets 403 (got ${first.status()})`);
  await octx.close();
} finally {
  await browser.close();
}

if (failures.length) {
  console.error(`${failures.length} check(s) failed`);
  process.exit(1);
}
console.log("spark ui: all browser checks passed");
