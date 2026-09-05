import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { randomBytes } from 'node:crypto';
import { chmod, mkdir, mkdtemp, readFile, readdir, rm, writeFile } from 'node:fs/promises';
import { createServer, createConnection } from 'node:net';
import { arch, homedir, platform, tmpdir } from 'node:os';
import { basename, resolve, join } from 'node:path';
import { pathToFileURL } from 'node:url';
import { setTimeout as delay } from 'node:timers/promises';

export const SCHEMA = 'witself.dashboard-acceptance.v1';
export const PANELS = ['overview', 'transcripts', 'facts', 'memories', 'conversations', 'email', 'secrets'];
export const CHECKS = ['a_bare_url', 'b_authentication', 'c_panels', 'd_redaction', 'e_lifecycle'];
const CANARY = 'STUB_SECRET_CANARY';
const TIMEOUT = 60_000;
const AGENT = 'acceptance';
const MISSING_TOKEN = { error: 'missing or invalid access token (open the ?token= URL printed at startup)' };

// Same banner grammar as cmd/witself/dashboard_test.go, with an exact agent.
export function parseDashboardBanner(text, agent = AGENT) {
  for (const line of text.split(/\r?\n/)) {
    const match = /^witself dashboard: serving agent (\S+) on (http:\/\/127\.0\.0\.1:([0-9]+)\/\?token=[0-9a-f]{32})$/.exec(line);
    if (match && match[1] === agent && Number(match[3]) > 0 && Number(match[3]) <= 65535) return match[2];
  }
  return null;
}

export function containsCanary(value) {
  return (typeof value === 'string' ? value : JSON.stringify(value)).includes(CANARY);
}

const STATUSES = ['pass', 'fail', 'skipped'];
function exactKeys(value, keys) {
  assert.ok(value && typeof value === 'object' && !Array.isArray(value));
  assert.deepEqual(Object.keys(value).sort(), [...keys].sort());
}
function duration(value) { assert.ok(Number.isFinite(value) && value >= 0); }
function count(value) { assert.ok(Number.isSafeInteger(value) && value >= 0); }

// Reject unknown fields, inconsistent passes, raw errors, and unsafe paths.
// Failure reports use fixed stage/code identifiers; no subprocess output is retained.
export function validateSummary(summary) {
  exactKeys(summary, ['schema', 'platform', 'witself_version', 'status', 'duration_ms', 'checks', 'panels', 'failures']);
  assert.equal(summary.schema, SCHEMA);
  exactKeys(summary.platform, ['os', 'arch']);
  assert.match(summary.platform.os, /^(linux|darwin|win32)$/);
  assert.match(summary.platform.arch, /^[a-z0-9_]+$/);
  assert.ok(summary.witself_version === null || (typeof summary.witself_version === 'string' && summary.witself_version.length > 0 && summary.witself_version.length < 512));
  assert.ok(['pass', 'fail'].includes(summary.status));
  duration(summary.duration_ms);
  exactKeys(summary.checks, CHECKS);
  for (const result of Object.values(summary.checks)) {
    exactKeys(result, ['status', 'duration_ms']);
    assert.ok(STATUSES.includes(result.status));
    duration(result.duration_ms);
  }
  assert.equal(summary.panels.length, PANELS.length);
  summary.panels.forEach((panel, i) => {
    exactKeys(panel, ['name', 'hash', 'status', 'duration_ms', 'console_errors', 'failed_requests', 'http_errors', 'stream_cancellations', 'artifacts']);
    assert.equal(panel.name, PANELS[i]);
    assert.equal(panel.hash, `#/${panel.name}`);
    assert.ok(STATUSES.includes(panel.status));
    duration(panel.duration_ms);
    for (const key of ['console_errors', 'failed_requests', 'http_errors', 'stream_cancellations']) count(panel[key]);
    assert.ok(Array.isArray(panel.artifacts));
    const expected = ['png', 'aria.yml', 'txt', 'html'].map(ext => `${panel.name}.${ext}`);
    assert.equal(new Set(panel.artifacts).size, panel.artifacts.length);
    panel.artifacts.forEach(file => assert.ok(expected.includes(file)));
    if (panel.status === 'pass') {
      assert.deepEqual(panel.artifacts, expected);
      assert.equal(panel.console_errors + panel.failed_requests + panel.http_errors, 0);
    }
  });
  assert.ok(Array.isArray(summary.failures));
  for (const failure of summary.failures) {
    exactKeys(failure, ['stage', 'code']);
    assert.match(failure.stage, /^[a-z_]+$/);
    assert.match(failure.code, /^[a-z_]+$/);
  }
  if (summary.checks.c_panels.status === 'pass') assert.ok(summary.panels.every(panel => panel.status === 'pass'));
  if (summary.status === 'pass') {
    assert.ok(summary.witself_version);
    assert.equal(summary.failures.length, 0);
    assert.ok(Object.values(summary.checks).every(result => result.status === 'pass'));
    assert.ok(summary.panels.every(panel => panel.status === 'pass'));
  } else {
    assert.ok(summary.failures.length > 0);
  }
  const serialized = JSON.stringify(summary);
  assert.ok(!containsCanary(serialized));
  assert.ok(!/(?:[?&]token=|Bearer |witself_agt_|\b[0-9a-f]{32}\b|\/Users\/|\/home\/|[A-Za-z]:[\\/]|\\\\Users\\)/i.test(serialized));
  return summary;
}

export function createSummary() {
  return {
    schema: SCHEMA, platform: { os: platform(), arch: arch() }, witself_version: null,
    status: 'fail', duration_ms: 0,
    checks: Object.fromEntries(CHECKS.map(key => [key, { status: 'skipped', duration_ms: 0 }])),
    panels: PANELS.map(name => ({ name, hash: `#/${name}`, status: 'skipped', duration_ms: 0,
      console_errors: 0, failed_requests: 0, http_errors: 0, stream_cancellations: 0, artifacts: [] })),
    failures: [],
  };
}

class Failure extends Error {
  constructor(code) { super(code); this.code = code; }
}
function requireThat(ok, code) { if (!ok) throw new Failure(code); }

async function bounded(promise, timeout = TIMEOUT) {
  let timer;
  try {
    return await Promise.race([promise, new Promise((_, reject) => {
      timer = setTimeout(() => reject(new Failure('timeout')), timeout);
    })]);
  } finally { clearTimeout(timer); }
}

export async function browserDeadline(work, closePage, timeout = TIMEOUT) {
  try { return await bounded(work, timeout); }
  catch (error) {
    if (error instanceof Failure && error.code === 'timeout') {
      // Promise.race alone leaves browser work alive. Close and drain before
      // any later stage can navigate or write captures from another panel.
      try { await bounded(closePage(), 10_000); }
      finally { await bounded(work.catch(() => {}), 10_000); }
    }
    throw error;
  }
}

function startProcess(binary, args, env) {
  const child = spawn(binary, args, { env, stdio: ['ignore', 'pipe', 'pipe'], windowsHide: true });
  const processState = { child, stdout: '', stderr: '', ended: false, code: null };
  for (const stream of ['stdout', 'stderr']) {
    child[stream].setEncoding('utf8');
    child[stream].on('data', text => { processState[stream] = (processState[stream] + text).slice(-1024 * 1024); });
  }
  processState.done = new Promise(resolveDone => {
    child.once('error', () => { processState.ended = true; resolveDone(null); });
    child.once('close', code => { processState.ended = true; processState.code = code; resolveDone(code); });
  });
  return processState;
}

async function command(binary, args, env) {
  const proc = startProcess(binary, args, env);
  try {
    requireThat(await bounded(proc.done) === 0, 'command_failed');
    return proc.stdout.trim();
  } finally {
    if (!proc.ended) { proc.child.kill('SIGKILL'); await bounded(proc.done, 10_000); }
  }
}

async function waitForBanner(proc, parser) {
  const deadline = performance.now() + TIMEOUT;
  while (performance.now() < deadline) {
    const value = parser(proc);
    if (value) return value;
    requireThat(!proc.ended, 'process_exited_before_ready');
    // Poll a readiness condition, with an explicit deadline; never infer readiness from time.
    await delay(50);
  }
  throw new Failure('startup_timeout');
}

async function freePort() {
  const server = createServer();
  try {
    await bounded(new Promise((resolveListen, reject) => {
      server.once('error', reject);
      server.listen(0, '127.0.0.1', resolveListen);
    }));
    return server.address().port;
  } finally { await bounded(new Promise(resolveClose => server.close(resolveClose))); }
}

async function portClosed(port) {
  return bounded(new Promise((resolveClosed, reject) => {
    const socket = createConnection({ host: '127.0.0.1', port });
    socket.setTimeout(1000);
    socket.once('connect', () => { socket.destroy(); resolveClosed(false); });
    socket.once('timeout', () => { socket.destroy(); reject(new Failure('port_probe_timeout')); });
    socket.once('error', error => {
      socket.destroy();
      if (error.code === 'ECONNREFUSED') resolveClosed(true);
      else reject(new Failure('port_probe_failed'));
    });
  }), 2000);
}

async function waitForPortClosed(port) {
  const deadline = performance.now() + TIMEOUT;
  while (performance.now() < deadline) {
    if (await portClosed(port)) return;
    await delay(100);
  }
  throw new Failure('port_still_open');
}

function parseArgs(args) {
  const values = {};
  for (let i = 0; i < args.length; i += 2) {
    requireThat(['--witself', '--stub-cell', '--out'].includes(args[i]) && args[i + 1] && !values[args[i]], 'invalid_arguments');
    values[args[i]] = resolve(args[i + 1]);
  }
  requireThat(Object.keys(values).length === 3, 'invalid_arguments');
  return { witself: values['--witself'], stub: values['--stub-cell'], out: values['--out'] };
}

// Capture only synthetic fixture content. An unsafe capture fails before any
// bytes (including its screenshot) are retained; errors never echo the value.
export function safeCapture(capture, privateValues) {
  const text = JSON.stringify(capture);
  requireThat(!containsCanary(text), 'redaction_canary');
  requireThat(!privateValues.some(value => value && text.includes(JSON.stringify(value).slice(1, -1))), 'private_capture');
  requireThat(!/[?&]token=[0-9a-f]{32}/.test(text), 'token_capture');
}

async function capturePanel(page, panel, out, privateValues) {
  const [text, aria, dom] = await Promise.all([
    page.locator('body').innerText(), page.locator('body').ariaSnapshot(), page.content(),
  ]);
  safeCapture([text, aria, dom], privateValues);
  const png = await page.screenshot({ fullPage: true, animations: 'disabled', timeout: TIMEOUT });
  safeCapture(await page.content(), privateValues);
  const artifacts = [`${panel.name}.png`, `${panel.name}.aria.yml`, `${panel.name}.txt`, `${panel.name}.html`];
  for (const [i, bytes] of [png, aria, text, dom].entries()) await writeFile(join(out, artifacts[i]), bytes);
  panel.artifacts = artifacts;
}

export const STREAM_CLOSE_MARKER = 'witself-dashboard-acceptance:eventsource-close:';

// Runs before app.js in every document. Report the exact source's explicit
// close synchronously before invoking the native close implementation. This
// also covers stream changes after asynchronous reads within the same panel.
export function observeStreamClosures(marker) {
  const close = EventSource.prototype.close;
  EventSource.prototype.close = function (...args) {
    console.debug(marker + this.url);
    return close.apply(this, args);
  };
}

// Keep the browser listeners shared with transition regressions.
export function trackPageRequests(page, currentPanel) {
  const pending = new Set();
  const erroredStreams = new WeakSet();
  const replacedStreams = new WeakSet();
  const streams = new Set();
  page.on('console', message => {
    if (message.type() === 'debug' && message.text().startsWith(STREAM_CLOSE_MARKER)) {
      const url = message.text().slice(STREAM_CLOSE_MARKER.length);
      for (const stream of streams) {
        if (stream.url() === url) replacedStreams.add(stream);
      }
    }
    if (message.type() === 'error' && currentPanel()) currentPanel().console_errors++;
  });
  page.on('pageerror', () => { if (currentPanel()) currentPanel().console_errors++; });
  page.on('request', request => {
    if (request.resourceType() === 'eventsource') streams.add(request);
    else pending.add(request);
  });
  page.on('response', response => {
    if (response.request().resourceType() === 'eventsource' && response.status() >= 400) erroredStreams.add(response.request());
    if (response.status() >= 400 && currentPanel()) currentPanel().http_errors++;
  });
  page.on('requestfinished', request => { pending.delete(request); streams.delete(request); });
  page.on('requestfailed', request => {
    pending.delete(request); streams.delete(request);
    const panel = currentPanel();
    if (!panel) return;
    // app.js closes a stream it opened moments ago when the mailbox enrolls
    // (?email_sent=true is replaced by ?email=true&email_sent=true), sometimes
    // before the first stream's headers arrive. An explicit close followed by
    // the browser's abort is expected whether or not a 200 was seen; a stream
    // that received an HTTP error stays a failure.
    if (replacedStreams.has(request) && !erroredStreams.has(request) && request.failure()?.errorText === 'net::ERR_ABORTED') panel.stream_cancellations++;
    else panel.failed_requests++;
  });
  return { pending, replaceStreams() { for (const stream of streams) replacedStreams.add(stream); } };
}

export async function run(args) {
  const { witself, stub, out } = parseArgs(args);
  await mkdir(out, { recursive: true });
  // Do not mistake an older run's screenshots for evidence from this attempt.
  requireThat((await readdir(out)).length === 0, 'output_directory_not_empty');
  const summary = createSummary();
  const started = performance.now();
  let stage = 'setup', temp, stubProcess, serverProcess, browser, page, activePanel, env, port, serverReady = false;
  const privateValues = [homedir()];
  function recordFailure(error, failureStage = stage) {
    summary.failures.push({ stage: failureStage, code: error instanceof Failure ? error.code :
      (error?.name === 'TimeoutError' ? 'timeout' : failureStage === 'browser_launch' ? 'browser_launch_failed' : 'assertion_or_runtime_error') });
  }
  async function pageBounded(work, timeout = TIMEOUT) {
    return browserDeadline(work, () => page?.close(), timeout);
  }
  async function check(key, fn) {
    stage = key;
    const began = performance.now();
    try { await pageBounded(fn()); summary.checks[key].status = 'pass'; }
    catch (error) { summary.checks[key].status = 'fail'; recordFailure(error); }
    finally { summary.checks[key].duration_ms = Math.round(performance.now() - began); }
  }
  try {
    temp = await mkdtemp(join(tmpdir(), 'witself-dashboard-acceptance-'));
    privateValues.push(temp);
    const token = `witself_agt_${randomBytes(24).toString('hex')}`;
    privateValues.push(token);
    const tokenFile = join(temp, 'agent.token');
    await writeFile(tokenFile, `${token}\n`, { mode: 0o600, flag: 'wx' });
    await chmod(tokenFile, 0o600);
    env = Object.fromEntries(Object.entries(process.env).filter(([key]) => !key.startsWith('WITSELF_')));
    env.WITSELF_HOME = join(temp, 'home');
    const version = await command(witself, ['--version'], env);
    safeCapture(version, privateValues);
    summary.witself_version = version;
    stubProcess = startProcess(stub, ['--listen', '127.0.0.1:0', '--token-file', tokenFile], env);
    const endpoint = await waitForBanner(stubProcess, proc => /^stub-cell: (http:\/\/127\.0\.0\.1:[0-9]+)\r?$/m.exec(proc.stdout)?.[1]);
    port = await freePort(); // --port 0 derives an agent port; it does not request an ephemeral port.
    serverProcess = startProcess(witself, ['dashboard', 'serve', '--agent', AGENT, '--endpoint', endpoint,
      '--token-file', tokenFile, '--port', String(port)], env);
    const tokenedURL = await waitForBanner(serverProcess, proc => parseDashboardBanner(proc.stderr));
    const cleanURL = `http://127.0.0.1:${port}/`;
    requireThat(new URL(tokenedURL).origin === new URL(cleanURL).origin, 'unexpected_listener');
    serverReady = true;
    privateValues.push(new URL(tokenedURL).searchParams.get('token'));
    const { chromium, request, expect: baseExpect } = await import('@playwright/test');
    const expect = baseExpect.configure({ timeout: TIMEOUT });
    await check('a_bare_url', async () => {
      const unauthenticated = await bounded(request.newContext());
      try {
        const response = await unauthenticated.get(cleanURL, { maxRedirects: 0, timeout: TIMEOUT });
        assert.equal(response.status(), 401);
        assert.deepEqual(await response.json(), MISSING_TOKEN);
      } finally { await bounded(unauthenticated.dispose()); }
    });
    stage = 'browser_launch';
    browser = await chromium.launch({ headless: true, timeout: TIMEOUT });
    const context = await bounded(browser.newContext({ viewport: { width: 1440, height: 1100 }, colorScheme: 'dark' }));
    context.setDefaultTimeout(TIMEOUT);
    context.setDefaultNavigationTimeout(TIMEOUT);

    page = await bounded(context.newPage());
    await bounded(page.addInitScript(observeStreamClosures, STREAM_CLOSE_MARKER));
    // Startup loads belong to overview too, including both authenticated navigations.
    activePanel = summary.panels[0];
    const { pending, replaceStreams } = trackPageRequests(page, () => activePanel);
    await check('b_authentication', async () => {
      const [exchange, cleanResponse] = await Promise.all([
        page.waitForResponse(response => response.url() === tokenedURL, { timeout: TIMEOUT }),
        page.goto(tokenedURL, { waitUntil: 'load' }),
      ]);
      assert.equal(exchange.status(), 303);
      assert.equal(exchange.headers().location, '/');
      assert.equal(cleanResponse.status(), 200);
      assert.equal(page.url(), cleanURL);
      const cookies = await context.cookies(cleanURL);
      const cookie = cookies.find(item => item.name === `witself_dashboard_${port}`);
      requireThat(cookie?.httpOnly && cookie.sameSite === 'Strict', 'cookie_policy');
      assert.equal(await page.evaluate(() => document.cookie), '');
      await expect.poll(() => pending.size).toBe(0);
      await expect(page.locator('#live-label')).toHaveText('live');
      // Full-page navigation destroys EventSources without calling close().
      replaceStreams();
      assert.equal((await page.goto(cleanURL, { waitUntil: 'load' })).status(), 200);
      assert.equal(await page.evaluate(() => document.cookie), '');
    });
    const panelsStarted = performance.now();
    stage = 'c_panels';
    for (const panel of summary.panels) {
      if (page.isClosed()) break;
      activePanel = panel;
      const panelStarted = performance.now();
      let captured = false;
      try {
        await pageBounded((async () => {
          await page.evaluate(hash => { location.hash = hash; }, panel.hash);
          await expect(page.locator(`a[data-nav="${panel.name}"]`)).toHaveClass(/active/);
          const heading = panel.name === 'overview' ? 'inventory' : panel.name === 'email' ? 'received email' : panel.name;
          await expect(page.locator('#view h2').filter({ hasText: new RegExp(`^${heading}(?:$| )`) }).first()).toBeVisible();
          await expect(page.locator('#view .row').first()).toBeVisible();
          if (panel.name === 'overview') {
            await expect(page.locator('#view')).toContainText('Enforced plan & entitlements');
            await expect(page.locator('#view')).toContainText('standard');
          }
          if (panel.name === 'email') {
            await expect(page.locator('.email-row:not(.email-sent-row)').first()).toContainText('safe subject');
            await expect(page.locator('.email-sent-row').first()).toContainText('safe sent subject');
            await expect(page.getByRole('heading', { name: 'email storage', exact: true })).toBeVisible();
            await expect(page.getByRole('progressbar', { name: 'account-wide attachment capacity' })).toBeVisible();
          }
          await expect.poll(() => pending.size, { timeout: TIMEOUT }).toBe(0);
          await expect(page.locator('#live-label')).toHaveText('live');
          await expect(page.locator('#status-upstream')).toHaveText('');
          await capturePanel(page, panel, out, privateValues);
          captured = true;
          requireThat(panel.console_errors === 0, 'console_errors');
          requireThat(panel.failed_requests === 0, 'failed_requests');
          requireThat(panel.http_errors === 0, 'http_errors');
        })());
        panel.status = 'pass';
      } catch (error) {
        panel.status = 'fail';
        recordFailure(error, panel.name);
        if (!captured && !page.isClosed() && !(error instanceof Failure && ['redaction_canary', 'private_capture', 'token_capture'].includes(error.code))) {
          try { await pageBounded(capturePanel(page, panel, out, privateValues), 10_000); }
          catch (captureError) { recordFailure(captureError, panel.name); }
        }
      } finally { panel.duration_ms = Math.round(performance.now() - panelStarted); }
    }
    activePanel = null;
    summary.checks.c_panels = { status: summary.panels.every(panel => panel.status === 'pass') ? 'pass' : 'fail', duration_ms: Math.round(performance.now() - panelsStarted) };
    await check('d_redaction', async () => {
      requireThat(summary.panels.every(panel => panel.artifacts.length === 4), 'incomplete_captures');
      for (const panel of summary.panels) {
        for (const artifact of panel.artifacts.filter(name => !name.endsWith('.png'))) {
          safeCapture(await readFile(join(out, artifact), 'utf8'), privateValues);
        }
      }
      requireThat(!summary.failures.some(failure => ['redaction_canary', 'private_capture', 'token_capture'].includes(failure.code)), 'unsafe_capture');
    });
  } catch (error) { recordFailure(error); }
  finally {
    stage = 'cleanup';
    if (browser) {
      try { await bounded(browser.close(), 10_000); } catch (error) { recordFailure(error); }
    }
    // Exercise the real CLI even when the browser failed. Browser-owned SSE
    // streams are closed first; cleanup killing a child can never earn a pass.
    if (serverReady) {
      await check('e_lifecycle', async () => {
        const status = JSON.parse(await command(witself, ['dashboard', 'status', '--json'], env));
        requireThat(status.dashboards.length === 1 && status.dashboards[0].live === true &&
          status.dashboards[0].pid === serverProcess.child.pid && status.dashboards[0].port === port &&
          status.dashboards[0].agent_name === AGENT, 'live_registry_missing');
        await command(witself, ['dashboard', 'stop', '--agent', AGENT], env);
        requireThat(await bounded(serverProcess.done) === 0, 'serve_exit_failed');
        await waitForPortClosed(port);
        const stopped = JSON.parse(await command(witself, ['dashboard', 'status', '--json'], env));
        requireThat(stopped.dashboards.length === 0, 'registry_not_released');
      });
    }
    stage = 'cleanup';
    for (const proc of [serverProcess, stubProcess]) {
      if (proc && !proc.ended) {
        // Cleanup never counts as the CLI stop acceptance check.
        try { proc.child.kill('SIGKILL'); await bounded(proc.done, 10_000); } catch (error) { recordFailure(error); }
      }
    }
    if (temp) {
      try { await rm(temp, { recursive: true, force: true, maxRetries: 3, retryDelay: 100 }); } catch (error) { recordFailure(error); }
    }
    summary.duration_ms = Math.round(performance.now() - started);
    summary.status = summary.failures.length === 0 && Object.values(summary.checks).every(result => result.status === 'pass') ? 'pass' : 'fail';
    validateSummary(summary);
    await writeFile(join(out, 'summary.json'), `${JSON.stringify(summary, null, 2)}\n`);
  }
  return summary;
}

if (process.argv[1] && import.meta.url === pathToFileURL(resolve(process.argv[1])).href) {
  try {
    const summary = await run(process.argv.slice(2));
    console.log(`dashboard acceptance: ${summary.status}; evidence: ${basename(resolve(process.argv.at(-1)))}`);
    process.exitCode = summary.status === 'pass' ? 0 : 1;
  } catch (error) {
    // Never print raw Playwright/subprocess errors: they may contain the access URL or local paths.
    console.error(`dashboard acceptance: ${error instanceof Failure ? error.code : 'report_failed'}`);
    process.exitCode = 1;
  }
}
