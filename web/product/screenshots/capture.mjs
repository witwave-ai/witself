// Real UI captures, with closed synthetic sources. No real account or URL input.
import assert from 'node:assert/strict';
import {spawn} from 'node:child_process';
import {readFile, writeFile, mkdtemp, mkdir, rm, rename} from 'node:fs/promises';
import {tmpdir} from 'node:os';
import {join, resolve} from 'node:path';
import {createRequire} from 'node:module';
import {setTimeout as delay} from 'node:timers/promises';
import {assetDir, repoRoot, imageNames, pngSize, sha256, sourceDigest} from './verify.mjs';

// Reuse the repository's pinned browser dependency rather than introducing a
// second Playwright version. xterm is a capture dependency only, never shipped.
const acceptanceRequire = createRequire(join(repoRoot, 'scripts/dashboard-acceptance/package.json'));
const {chromium, expect} = acceptanceRequire('@playwright/test');
const require = createRequire(import.meta.url);
const args = process.argv.slice(2);
assert(args.length === 0 || (args.length === 2 && args[0] === '--out'), 'Usage: node web/product/screenshots/capture.mjs [--out DIRECTORY]');
const out = args.length ? resolve(args[1]) : assetDir;
const temporary = await mkdtemp(join(tmpdir(), 'witself-product-capture-'));
const safeEnv = Object.fromEntries(Object.entries(process.env).filter(([name]) => !/^(WITSELF_|CLOUDFLARE_|CF_|GH_|GITHUB_TOKEN$)/.test(name)));
const children = [];
let browser;
let stage = 'prepare';

function start(binary, argv, options = {}) {
  const child = spawn(binary, argv, {cwd: repoRoot, env: safeEnv, stdio: ['pipe', 'pipe', 'pipe'], ...options});
  const state = {child, output: '', error: '', closed: false};
  children.push(state);
  child.stdout.on('data', b => { state.output = (state.output + b.toString()).slice(-262144); });
  child.stderr.on('data', b => { state.error = (state.error + b.toString()).slice(-262144); });
  state.done = new Promise(resolveDone => {
    child.once('error', () => { state.closed = true; resolveDone(-1); });
    child.once('close', code => { state.closed = true; resolveDone(code); });
  });
  return state;
}
async function bounded(work, ms) {
  let timer;
  try { return await Promise.race([work, new Promise((_, reject) => { timer = setTimeout(() => reject(new Error('deadline exceeded')), ms); })]); }
  finally { clearTimeout(timer); }
}
async function readiness(proc) {
  const deadline = Date.now() + 30000;
  while (Date.now() < deadline) {
    assert(!proc.closed, 'Demo helper exited before readiness');
    if (proc.output.includes('\n')) return JSON.parse(proc.output.split('\n')[0]);
    await delay(25);
  }
  throw new Error('Demo helper readiness deadline exceeded');
}
function assertPublic(text, privateValues) {
  assert(!privateValues.some(value => value && text.includes(value)), 'Private capture rejected');
  assert(!/STUB_SECRET_CANARY|[?&]token=|Bearer |witself_agt_/.test(text), 'Unsafe capture rejected');
}

try {
  const captureDigest = await sourceDigest();
  stage = 'build demo helper';
  const binary = join(temporary, process.platform === 'win32' ? 'demo.exe' : 'demo');
  const build = start('go', ['build', '-o', binary, './web/product/screenshots/demo']);
  assert.equal(await bounded(build.done, 180000), 0, 'Demo helper build failed');
  stage = 'start isolated demo';
  const ansiPath = join(temporary, 'terminal.ansi');
  const demo = start(binary, ['--ansi-out', ansiPath]);
  const {url} = await readiness(demo);
  const local = new URL(url);
  assert.equal(local.protocol, 'http:');
  assert.equal(local.hostname, '127.0.0.1');
  const token = local.searchParams.get('token');
  assert(token && token.length >= 16);
  const ansi = await readFile(ansiPath, 'utf8');
  assert(ansi.includes('Atlas') && ansi.includes('Memories'));
  assertPublic(ansi, [token]);
  stage = 'launch pinned browser';
  browser = await chromium.launch({headless: true, timeout: 30000});
  const context = await browser.newContext({viewport: {width: 1440, height: 1000}, deviceScaleFactor: 1, locale: 'en-US', timezoneId: 'UTC', colorScheme: 'dark', reducedMotion: 'reduce'});
  // No external request can leave either screenshot page.
  await context.route('**/*', route => {
    const requestURL = new URL(route.request().url());
    return requestURL.origin === local.origin ? route.continue() : route.abort();
  });
  const page = await context.newPage();
  page.setDefaultTimeout(15000);
  const errors = [];
  page.on('pageerror', () => errors.push('pageerror'));
  page.on('console', msg => { if (msg.type() === 'error') errors.push('console'); });
  page.on('response', response => { if (response.status() >= 400) errors.push('http'); });
  stage = 'capture web console';
  await page.goto(url, {waitUntil: 'domcontentloaded'});
  await expect(page.locator('#agent-name')).toHaveText('Atlas');
  await page.locator('[data-nav="memories"]').click();
  await expect(page.locator('#view .focus-open').first()).toBeVisible();
  await page.locator('#view .focus-open').first().click();
  await expect(page.locator('#focus-detail')).toBeVisible();
  await expect(page.locator('#focus-detail')).toContainText('Wayfinding should feel calm');
  await expect(page.locator('#live-label')).toHaveText('Connected');
  await page.evaluate(() => document.fonts.ready);
  const cookies = await context.cookies();
  assertPublic(await page.content(), [token, ...cookies.map(cookie => cookie.value)]);
  assert.equal(errors.length, 0, 'Console rendering reported errors');
  const webPNG = await page.screenshot({animations: 'disabled'});

  stage = 'capture terminal renderer';
  const terminal = await context.newPage();
  const xtermRoot = resolve(require.resolve('@xterm/xterm'), '../..');
  await terminal.setContent('<!doctype html><meta charset="utf-8"><style>body{margin:0;background:#0c1117}#terminal{display:inline-block;padding:20px}.xterm-viewport{overflow:hidden!important}</style><div id="terminal"></div>');
  await terminal.addStyleTag({path: join(xtermRoot, 'css/xterm.css')});
  await terminal.addScriptTag({path: require.resolve('@xterm/xterm')});
  const terminalText = await terminal.evaluate(async ({ansi}) => {
    const term = new Terminal({cols: 120, rows: 40, convertEol: true, scrollback: 0, disableStdin: true, fontFamily: '"DejaVu Sans Mono", "Menlo", monospace', fontSize: 16, lineHeight: 1.15, cursorBlink: false, theme: {background: '#0c1117', foreground: '#c9d6e2'}});
    term.open(document.querySelector('#terminal'));
    await document.fonts.ready;
    await new Promise(resolve => term.write(ansi + '\x1b[?25l', resolve));
    await new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve)));
    return Array.from({length: term.rows}, (_, i) => term.buffer.active.getLine(i)?.translateToString(true) ?? '').join('\n');
  }, {ansi});
  assert(terminalText.includes('Atlas') && terminalText.includes('Wayfinding'), 'Expected demo terminal content missing');
  assertPublic(terminalText, [token]);
  const tuiPNG = await terminal.locator('#terminal').screenshot({animations: 'disabled'});

  stage = 'write reviewed capture candidates';
  const images = {};
  for (const [name, bytes] of [[imageNames[0], webPNG], [imageNames[1], tuiPNG]]) {
    await writeFile(join(temporary, name), bytes);
    images[name] = {...pngSize(bytes), sha256: sha256(bytes)};
  }
  assert.equal(await sourceDigest(), captureDigest, 'Capture inputs changed while screenshots were being rendered');
  const manifest = {schema: 'witself.product-screenshots.v1', fixture: 'synthetic-demo-only', source_digest: captureDigest, captured_at: new Date().toISOString(), renderer: {browser: browser.version(), terminal: 'xterm 6.0.0', platform: process.platform}, images};
  await writeFile(join(temporary, 'manifest.json'), JSON.stringify(manifest, null, 2) + '\n');
  // Both images must pass before any existing public asset is replaced.
  await mkdir(out, {recursive: true});
  for (const name of [...imageNames, 'manifest.json']) {
    const next = join(out, name + '.next');
    await writeFile(next, await readFile(join(temporary, name)));
    await rename(next, join(out, name));
  }
  console.log('Captured actual web and terminal consoles using synthetic demo data. Review both PNGs before publication.');
} catch {
  // Do not echo a browser error, helper banner, environment, or URL token.
  console.error(`Screenshot capture failed during: ${stage}. Existing screenshots were not intentionally replaced.`);
  process.exitCode = 1;
} finally {
  if (browser) await browser.close().catch(() => {});
  for (const proc of children.reverse()) {
    if (proc.closed) continue;
    proc.child.stdin.end();
    await Promise.race([proc.done, delay(3000)]);
    if (!proc.closed) { proc.child.kill('SIGKILL'); await bounded(proc.done, 5000).catch(() => {}); }
  }
  await rm(temporary, {recursive: true, force: true});
}
