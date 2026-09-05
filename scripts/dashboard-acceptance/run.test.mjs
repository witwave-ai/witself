import assert from 'node:assert/strict';
import { EventEmitter } from 'node:events';
import { readFile } from 'node:fs/promises';
import test from 'node:test';
import vm from 'node:vm';
import { CHECKS, PANELS, SCHEMA, STREAM_CLOSE_MARKER, browserDeadline, containsCanary, createSummary, observeStreamClosures, parseDashboardBanner, safeCapture, trackPageRequests, validateSummary } from './run.mjs';

const token = '0123456789abcdef0123456789abcdef';
const url = `http://127.0.0.1:43210/?token=${token}`;
const banner = `witself dashboard: serving agent acceptance on ${url}`;

function passingSummary() {
  const summary = createSummary();
  summary.witself_version = 'witself v0.0.300';
  summary.status = 'pass';
  summary.duration_ms = 1234;
  for (const check of Object.values(summary.checks)) Object.assign(check, { status: 'pass', duration_ms: 10 });
  for (const panel of summary.panels) {
    panel.status = 'pass';
    panel.duration_ms = 25;
    panel.artifacts = [`${panel.name}.png`, `${panel.name}.aria.yml`, `${panel.name}.txt`, `${panel.name}.html`];
  }
  return summary;
}

test('dashboard banner parses accumulated stderr chunks, noise, and CRLF', () => {
  assert.equal(parseDashboardBanner(''), null);
  assert.equal(parseDashboardBanner(`unrelated startup output\n${banner}\r\nmore output\n`), url);
  let stderr = 'an earlier message\n';
  for (const chunk of [banner.slice(0, 20), banner.slice(20, 76), banner.slice(76, -1)]) {
    stderr += chunk;
    assert.equal(parseDashboardBanner(stderr), null);
  }
  stderr += banner.slice(-1);
  assert.equal(parseDashboardBanner(stderr), url);
  assert.equal(parseDashboardBanner(`${banner.replace('acceptance', 'another')}\n${banner}`), url);
  assert.equal(parseDashboardBanner(banner.replace('acceptance', 'acceptance[1]'), 'acceptance[1]'), url);
});

test('dashboard banner rejects wrong agents and malformed or extended token URLs', () => {
  const invalid = [
    banner.replace('agent acceptance ', 'agent other '),
    banner.replace(':43210/', ':0/'),
    banner.replace(':43210/', ':65536/'),
    banner.replace(':43210/', ':-1/'),
    banner.replace(':43210/', ':4.5/'),
    banner.replace('127.0.0.1', 'localhost'),
    banner.replace('127.0.0.1', '127.0.0.2'),
    banner.replace('http:', 'https:'),
    banner.replace(token, token.slice(1)),
    banner.replace(token, `${token}f`),
    banner.replace(token, token.toUpperCase()),
    `${banner}&redirect=https://example.invalid`,
    `${banner}#injected`,
    `${banner}/suffix`,
    `${banner} unexpected output`,
    `prefix ${banner}`,
    banner.replace('/?token=', '/other?token='),
  ];
  for (const value of invalid) assert.equal(parseDashboardBanner(value), null, value);
  assert.equal(parseDashboardBanner(banner.replace(':43210/', ':1/')), url.replace(':43210/', ':1/'));
  assert.equal(parseDashboardBanner(banner.replace(':43210/', ':65535/')), url.replace(':43210/', ':65535/'));
});

test('canary scanning finds nested snapshots, DOM, text, and object keys', () => {
  for (const value of [
    'STUB_SECRET_CANARY',
    '<span>before STUB_SECRET_CANARY after</span>',
    { panels: [{ text: 'safe', aria: ['nested', { text: 'STUB_SECRET_CANARY' }] }] },
    { STUB_SECRET_CANARY: 'hidden in a property name' },
  ]) assert.equal(containsCanary(value), true);
  for (const value of ['', null, false, 0, [], {}, { panels: ['redacted', '<redacted>', 'stub_secret_canary'] }]) {
    assert.equal(containsCanary(value), false);
  }
});

test('summary factory initializes independent reports for all seven panels and five checks', () => {
  const summary = createSummary();
  assert.equal(summary.schema, SCHEMA);
  assert.equal(SCHEMA, 'witself.dashboard-acceptance.v1');
  assert.deepEqual(PANELS, ['overview', 'transcripts', 'facts', 'memories', 'conversations', 'email', 'secrets']);
  assert.deepEqual(Object.keys(summary.checks), CHECKS);
  assert.deepEqual(summary.panels.map(panel => panel.name), PANELS);
  assert.equal(summary.witself_version, null);
  assert.equal(summary.status, 'fail');
  assert.ok(summary.panels.every(panel => panel.status === 'skipped' && panel.artifacts.length === 0));
  summary.panels[0].artifacts.push('overview.png');
  summary.checks.a_bare_url.status = 'pass';
  const independent = createSummary();
  assert.deepEqual(independent.panels[0].artifacts, []);
  assert.equal(independent.checks.a_bare_url.status, 'skipped');
});

test('summary validator accepts complete success on each supported OS and bounded failure evidence', () => {
  for (const os of ['linux', 'darwin', 'win32']) {
    const summary = passingSummary();
    summary.platform.os = os;
    assert.equal(validateSummary(summary), summary);
  }
  const failed = createSummary();
  failed.failures.push({ stage: 'browser_launch', code: 'timeout' });
  assert.equal(validateSummary(failed), failed);

  const panelFailure = passingSummary();
  panelFailure.status = 'fail';
  panelFailure.checks.c_panels.status = 'fail';
  panelFailure.panels[0].status = 'fail';
  panelFailure.panels[0].console_errors = 1;
  panelFailure.failures.push({ stage: 'overview', code: 'console_errors' });
  assert.equal(validateSummary(panelFailure), panelFailure);
});

test('summary validator rejects inconsistent passes and incomplete retained evidence', () => {
  const mutations = [
    summary => { summary.witself_version = null; },
    summary => { summary.checks.a_bare_url.status = 'fail'; },
    summary => { summary.checks.e_lifecycle.status = 'skipped'; },
    summary => { summary.panels[0].status = 'fail'; },
    summary => { summary.panels[0].status = 'skipped'; },
    summary => { summary.panels[0].console_errors = 1; },
    summary => { summary.panels[0].failed_requests = 1; },
    summary => { summary.panels[0].http_errors = 1; },
    summary => { summary.panels[0].artifacts.pop(); },
    summary => { summary.failures.push({ stage: 'setup', code: 'timeout' }); },
    summary => { summary.status = 'fail'; },
    summary => { summary.panels.pop(); },
    summary => { summary.panels.reverse(); },
    summary => { summary.panels[0].hash = '#/secrets'; },
  ];
  for (const [index, mutate] of mutations.entries()) {
    const summary = passingSummary();
    mutate(summary);
    assert.throws(() => validateSummary(summary), `inconsistent report ${index}`);
  }
});

test('summary validator keeps a passing panels check consistent even when another check failed', () => {
  for (const status of ['fail', 'skipped']) {
    const summary = passingSummary();
    summary.status = 'fail';
    summary.checks.e_lifecycle.status = 'fail';
    summary.panels[0].status = status;
    summary.failures.push({ stage: 'e_lifecycle', code: 'timeout' });
    assert.throws(() => validateSummary(summary), `panels check passes with an individual panel ${status}`);
  }
});

test('summary validator rejects invalid statuses, timing values, counts, and platform metadata', () => {
  const mutations = [
    summary => { summary.schema = 'witself.dashboard-acceptance.v2'; },
    summary => { summary.status = 'success'; },
    summary => { summary.checks.a_bare_url.status = 'pending'; },
    summary => { summary.panels[0].status = 'pending'; },
    summary => { summary.platform.os = 'unknown'; },
    summary => { summary.platform.arch = '../x64'; },
    summary => { summary.witself_version = ''; },
    summary => { summary.witself_version = 42; },
    summary => { summary.witself_version = 'v'.repeat(512); },
  ];
  for (const value of [-1, NaN, Infinity, '1', null]) {
    mutations.push(summary => { summary.duration_ms = value; });
    mutations.push(summary => { summary.checks.a_bare_url.duration_ms = value; });
    mutations.push(summary => { summary.panels[0].duration_ms = value; });
  }
  for (const key of ['console_errors', 'failed_requests', 'http_errors', 'stream_cancellations']) {
    for (const value of [-1, 0.5, NaN, Infinity, '0']) mutations.push(summary => { summary.panels[0][key] = value; });
  }
  for (const [index, mutate] of mutations.entries()) {
    const summary = passingSummary();
    mutate(summary);
    assert.throws(() => validateSummary(summary), `invalid field ${index}`);
  }
});

test('summary validator rejects unknown fields, raw errors, and unsafe artifact paths', () => {
  const mutations = [
    summary => { summary.extra = 'unexpected'; },
    summary => { summary.platform.home = '/tmp/home'; },
    summary => { summary.checks.a_bare_url.response = 'unexpected'; },
    summary => { summary.panels[0].html = 'unexpected'; },
    summary => { summary.checks.extra = { status: 'pass', duration_ms: 0 }; },
    summary => { summary.panels[0].artifacts.push('overview.png'); },
  ];
  for (const path of ['../overview.png', '/tmp/overview.png', 'C:\\Users\\someone\\overview.png', 'secrets.png', 'overview.svg']) {
    mutations.push(summary => { summary.panels[0].artifacts[0] = path; });
  }
  for (const [index, mutate] of mutations.entries()) {
    const summary = passingSummary();
    mutate(summary);
    assert.throws(() => validateSummary(summary), `unexpected content ${index}`);
  }
  for (const failure of [
    { stage: 'setup', code: 'timeout', message: 'raw subprocess output' },
    { stage: '/tmp/setup', code: 'timeout' },
    { stage: 'setup', code: 'a raw Error: failed' },
  ]) {
    const summary = createSummary();
    summary.failures.push(failure);
    assert.throws(() => validateSummary(summary));
  }
});

test('summary validator rejects canaries, credentials, and absolute home paths', () => {
  const privateValues = [
    'STUB_SECRET_CANARY',
    `http://127.0.0.1:43210/?token=${token}`,
    'Bearer synthetic-private-value',
    'witself_agt_synthetic-private-value',
    token,
    '/Users/example/private',
    '/home/example/private',
    'C:\\Users\\example\\private',
    'C:/Users/example/private',
  ];
  for (const value of privateValues) {
    const summary = passingSummary();
    summary.witself_version = `witself ${value}`;
    assert.throws(() => validateSummary(summary));
  }
});

test('a normal full commit hash in the version is retained without accepting an access token', () => {
  const summary = passingSummary();
  summary.witself_version = 'witself v0.0.300 (commit 0123456789abcdef0123456789abcdef01234567, built 2026-09-05)';
  assert.equal(validateSummary(summary), summary);
});

test('capture privacy checks also match JSON-escaped Windows paths and token values', () => {
  for (const value of ['C:\\Users\\acceptance\\private', '/home/acceptance/private', token]) {
    assert.throws(() => safeCapture(['visible text', { dom: `<div>${value}</div>` }], [value]));
  }
  assert.throws(() => safeCapture('STUB_SECRET_CANARY', []));
  assert.doesNotThrow(() => safeCapture(['safe synthetic fixture'], ['C:\\Users\\acceptance']));
});

test('browser deadline closes and drains timed-out work before returning', async () => {
  let rejectWork;
  let drained = false;
  let closed = false;
  const work = new Promise((_, reject) => { rejectWork = reject; }).finally(() => { drained = true; });
  await assert.rejects(browserDeadline(work, async () => {
    closed = true;
    rejectWork(new Error('page closed'));
  }, 1), /timeout/);
  assert.ok(closed);
  assert.ok(drained);
  assert.equal(await browserDeadline(Promise.resolve('done'), () => { assert.fail('closed successful work'); }, 1000), 'done');
});

function browserEvents() {
  const page = new EventEmitter();
  const panel = createSummary().panels.find(panel => panel.name === 'email');
  const tracker = trackPageRequests(page, () => panel);
  const sources = [];
  class EventSource {
    constructor(path) {
      this.url = new URL(path, 'http://127.0.0.1:43210').href;
      this.request = { url: () => this.url, resourceType: () => 'eventsource', failure: () => ({ errorText: 'net::ERR_ABORTED' }) };
      sources.push(this);
      page.emit('request', this.request);
      page.emit('response', { request: () => this.request, status: () => 200 });
    }
    addEventListener() {}
    close() { page.emit('requestfailed', this.request); }
  }
  const context = vm.createContext({
    EventSource,
    console: { debug: text => page.emit('console', { type: () => 'debug', text: () => text }) },
  });
  vm.runInContext(`(${observeStreamClosures.toString()})(${JSON.stringify(STREAM_CLOSE_MARKER)})`, context);
  return { page, panel, tracker, sources, context };
}

test('mailbox enrollment replaces the established sent-only stream without failing email acceptance', async () => {
  const { panel, tracker, sources, context } = browserEvents();
  const nodes = { view: { innerHTML: '' }, 'status-upstream': { textContent: '', removeAttribute() {} } };
  const responses = {
    '/api/email/address': { available: true, address: { address: 'acceptance@example.test' } },
    '/api/email/status': { available: true, status: { maximum_raw_bytes: 1024, attachment_capacity: {} } },
    '/api/email?limit=100': { available: true, messages: [{ subject: 'safe subject' }] },
  };
  let finishMailbox;
  const mailbox = new Promise(resolve => { finishMailbox = resolve; });
  Object.assign(context, {
    module: { exports: {} },
    window: { location: { hash: '#/email' }, matchMedia: null },
    document: { getElementById: id => nodes[id] || null },
    fetch: async path => {
      assert.ok(Object.hasOwn(responses, path), `unexpected fetch ${path}`);
      await mailbox;
      return { ok: true, json: async () => responses[path] };
    },
  });
  vm.runInContext(await readFile(new URL('../../internal/dashboard/static/app.js', import.meta.url), 'utf8'), context);
  const app = context.module.exports;
  // The driver has just navigated. The first stream opens while enrollment
  // reads are still pending, then actual app code closes it within this panel.
  tracker.replaceStreams();
  app.openEmailEvents();
  const probe = app.probeEmailMailbox();
  assert.equal(sources.length, 1);
  assert.equal(new URL(sources[0].url).search, '?email_sent=true');
  finishMailbox();
  await probe;
  assert.equal(sources.length, 2);
  assert.equal(new URL(sources[1].url).search, '?email=true&email_sent=true');
  assert.match(nodes.view.innerHTML, /safe subject/);
  assert.equal(panel.failed_requests, 0);
  assert.equal(panel.stream_cancellations, 1);
});

test('stream replacements exempt only the explicitly closed successful request', () => {
  const { page, panel, context } = browserEvents();
  const first = new context.EventSource('/api/events?email_sent=true');
  first.close();
  assert.equal(panel.stream_cancellations, 1);
  const replacement = new context.EventSource('/api/events?email_sent=true');
  // Reusing the URL must not inherit the previous Request's exemption.
  page.emit('requestfailed', replacement.request);
  assert.equal(panel.failed_requests, 1);
  const unexpected = new context.EventSource('/api/events?email_sent=true');
  new context.EventSource('/api/events?email=true&email_sent=true');
  // A newer stream by itself is not evidence that the old one was closed.
  page.emit('requestfailed', unexpected.request);
  assert.equal(panel.failed_requests, 2);
  const reset = new context.EventSource('/api/events');
  reset.request.failure = () => ({ errorText: 'net::ERR_CONNECTION_RESET' });
  reset.close();
  assert.equal(panel.failed_requests, 3);
  assert.equal(panel.stream_cancellations, 1);
});

test('close markers do not excuse unsuccessful streams, ordinary requests, or HTTP and console errors', () => {
  for (const resourceType of ['eventsource', 'fetch']) {
    const { page, panel } = browserEvents();
    const request = { url: () => 'http://127.0.0.1:43210/api/events', resourceType: () => resourceType,
      failure: () => ({ errorText: 'net::ERR_ABORTED' }) };
    page.emit('request', request);
    page.emit('response', { request: () => request, status: () => resourceType === 'fetch' ? 200 : 503 });
    page.emit('console', { type: () => 'debug', text: () => STREAM_CLOSE_MARKER + request.url() });
    page.emit('requestfailed', request);
    page.emit('console', { type: () => 'error', text: () => 'synthetic error' });
    page.emit('pageerror', new Error('synthetic page error'));
    assert.equal(panel.failed_requests, 1);
    assert.equal(panel.stream_cancellations, 0);
    assert.equal(panel.http_errors, resourceType === 'fetch' ? 0 : 1);
    assert.equal(panel.console_errors, 2);
  }
});

test('an explicitly closed stream that never received headers is an expected cancellation', () => {
  const { page, panel } = browserEvents();
  // Delayed initial SSE headers: the request is in flight with no response yet
  // when app.js closes it during mailbox enrollment.
  const request = { url: () => 'http://127.0.0.1:43210/api/events?email_sent=true', resourceType: () => 'eventsource',
    failure: () => ({ errorText: 'net::ERR_ABORTED' }) };
  page.emit('request', request);
  page.emit('console', { type: () => 'debug', text: () => STREAM_CLOSE_MARKER + request.url() });
  page.emit('requestfailed', request);
  assert.equal(panel.stream_cancellations, 1);
  assert.equal(panel.failed_requests, 0);
  assert.equal(panel.http_errors, 0);
});

test('full-page reload still recognizes successful stream cancellation without an explicit close', () => {
  const { page, panel, tracker, context } = browserEvents();
  const source = new context.EventSource('/api/events');
  tracker.replaceStreams();
  page.emit('requestfailed', source.request);
  assert.equal(panel.stream_cancellations, 1);
  assert.equal(panel.failed_requests, 0);
});
