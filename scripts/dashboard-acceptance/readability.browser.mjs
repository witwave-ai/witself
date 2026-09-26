import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';
import { launchChromium } from './browser.mjs';
import { createRequire } from 'node:module';

const require = createRequire(import.meta.url);
const { summaryData } = require('../../internal/dashboard/testdata/overview_harness.cjs');
const staticRoot = new URL('../../internal/dashboard/static/', import.meta.url);
const long = 'Synthetic_long_identifier_'.repeat(12);
const value = 'A complete synthetic value <literal> with many words. '.repeat(30);
const facts = [
  { id: 'fact_plain', subject: long, predicate: 'example/value', value, source_kind: long, updated_at: '2026-09-23T12:00:00Z' },
  { id: 'fact_locked', subject: 'Synthetic private fact', predicate: 'example/private', sensitive: true },
];

// Fresh context, intercepted transport only. No actual agent, account, server,
// credential, or OS clipboard. The browser executes the shipped app unchanged.
test('readability: responsive lists, details, filters, transport, and summary refresh', { timeout: 120000 }, async (t) => {
  const browser = await launchChromium(t);
  if (!browser) return;
  try {
    const page = await browser.newPage();
    const errors = [], requests = [];
    // The console polls account context every 5 s and refreshes the summary on
    // its own timer; under the installed clock those still fire in real time.
    // "Never fetch" assertions count only requests a user action could cause.
    const backgroundPolls = new Set(['/api/account/context', '/api/summary']);
    const foregroundRequests = () => requests.filter((url) => !backgroundPolls.has(url.split('?')[0])).length;
    page.on('pageerror', (error) => errors.push(error.message));
    await page.addInitScript(() => {
      window.EventSource = class extends EventTarget {
        constructor() { super(); window.testStream = this; }
        close() {}
      };
      window.testCopies = [];
      Object.defineProperty(navigator, 'clipboard', { value: { writeText: async (text) => { window.testCopies.push(text); } } });
    });
    await page.route('http://dashboard.test/**', async (route) => {
      const url = new URL(route.request().url());
      requests.push(url.pathname + url.search);
      if (url.pathname === '/') return route.fulfill({ contentType: 'text/html', body: await readFile(new URL('index.html', staticRoot), 'utf8') });
      if (url.pathname.startsWith('/static/')) return route.fulfill({ contentType: url.pathname.endsWith('.js') ? 'text/javascript' : 'text/css', body: await readFile(new URL(url.pathname.slice(8), staticRoot), 'utf8') });
      if (url.pathname === '/api/avatar.svg') return route.fulfill({ contentType: 'image/svg+xml', headers: { 'Cache-Control': 'private, no-store' }, body: '<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 80 80"/>' });
      // The console polls account context at boot; this fixture has no manager credential.
      if (url.pathname === '/api/account/context') return route.fulfill({ status: 403, json: { error: 'forbidden' } });
      const responses = {
        '/api/themes': { themes: ['console', 'paper', 'amber', 'high-contrast', 'midnight'] },
        '/api/prefs': { preferences: { prefs: { theme: 'console' } } },
        '/api/facts': { facts }, '/api/facts/fact_plain/history': { assertions: [] }, '/api/facts/fact_locked/history': { assertions: [] },
        '/api/fact': { fact: { value } },
        '/api/transcripts': { transcripts: [{ id: long, title: long, updated_at: '2026-09-23T12:00:00Z', metadata: { agent_name: long, runtime: long, location: { name: long }, initial_cwd: '/synthetic-parent/' + long + '/' } }] },
        ['/api/transcripts/' + long]: { transcript: { title: 'Synthetic transcript' }, entries: [{ sequence: 1, role: 'assistant', body: value }] },
        '/api/memories': { items: [{ id: long, content: long, kind: long, state: 'active', salience: 0.9 }] },
        '/api/secrets': { secrets: [{ id: long, name: long, field_count: 3, sensitive_field_count: 2, lifecycle: long }] },
        '/api/messages': { messages: url.searchParams.get('direction') === 'inbox' ? [{ id: long, from: { agent_id: long, agent_name: long }, subject: long, created_at: '2026-09-23T12:00:00Z' }] : [] },
        '/api/email/address': { available: true, address: { address: 'synthetic@example.test', receive_state: 'enabled' } },
        '/api/email/status': { available: true, status: {} },
        '/api/email': { available: true, messages: [{ subject: long, envelope_sender: long + '@example.test', read_state: { state: long }, received_at: '2026-09-23T12:00:00Z' }] },
        '/api/email/sent': { available: true, messages: [{ subject: long, to: long + '@example.test', state: long }] },
        '/api/self': { identity: { agent_name: 'Synthetic agent' } },
        '/api/summary': summaryData(),
      };
      assert.ok(Object.hasOwn(responses, url.pathname), `unexpected request ${url.pathname}`);
      return route.fulfill({ json: responses[url.pathname] });
    });
    const navigate = async (section) => {
      await page.evaluate((section) => { location.hash = '#/' + section; }, section);
      await page.waitForFunction((section) => !!document.getElementById('filter-' + section), section);
    };
    const noOverflow = async () => {
      const bad = await page.evaluate(() => [...document.querySelectorAll('.row, .panel, .entry, .fact-value, .summary-row, .agent-summary, .transcript-table, .transcript-row, .transcript-selection, .transcript-metadata dd')]
        .filter((el) => el.scrollWidth > el.clientWidth + 1).map((el) => el.className));
      assert.deepEqual(bad, [], 'content boxes must not overflow');
      assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), 'document fits viewport');
    };
    await page.clock.install();
    await page.goto('http://dashboard.test/#/facts');
    assert.equal(requests.filter((url) => url === '/api/avatar.svg').length, 1, 'one avatar request on page load');
    assert.equal(await page.locator('#avatar-enlarged').getAttribute('src'), null);
    await page.locator('#avatar-trigger').click();
    await page.waitForFunction(() => {
      const image = document.getElementById('avatar-enlarged');
      return image.complete && image.naturalWidth > 0;
    });
    // Chromium shares one in-document image resource per URL even under no-store,
    // so opening the portrait adds no request there (measured: 1 at load, 1 after);
    // an engine that refetches may add exactly one more, never a per-open stream.
    const avatarRequests = requests.filter((url) => url === '/api/avatar.svg').length;
    assert.ok(avatarRequests >= 1 && avatarRequests <= 2, 'opening the portrait costs at most one avatar request, saw ' + avatarRequests);
    await page.locator('#avatar-close').click();
    // 640 CSS px also represents the reflow width of a 1280px window at 200% zoom.
    for (const width of [320, 390, 700, 640, 1024, 1280]) {
      await page.setViewportSize({ width, height: 900 });
      for (const section of ['facts', 'transcripts', 'memories', 'secrets', 'conversations']) {
        await navigate(section);
        await noOverflow();
        const title = page.locator(section === 'transcripts' ? '.transcript-open' : '.row .grow a').first();
        assert.ok((await title.boundingBox()).width > 80, `${section} title visible at ${width}`);
        if (section === 'transcripts') {
          assert.deepEqual(await page.locator('.transcript-table th').allTextContents(), ['Agent', 'Location', 'AI client', 'Workspace', 'Updated']);
          assert.equal(await page.locator('.transcript-row td').count(), 5);
          assert.equal(await page.locator('.transcript-row').first().evaluate((el) => getComputedStyle(el).display), width <= 700 ? 'grid' : 'table-row');
          assert.equal(await page.locator('.transcript-mobile-label').first().isVisible(), width <= 700);
          assert.ok(!(await page.locator('#view').textContent()).includes('synthetic-parent'), 'workspace retains only basename');
          const beforeSelect = foregroundRequests();
          await page.locator('.transcript-select').first().focus();
          await page.keyboard.press('Space');
          assert.equal(await page.locator('.transcript-select').first().getAttribute('aria-pressed'), 'true');
          assert.equal(foregroundRequests(), beforeSelect, 'keyboard selection never fetches a body');
        }
        const count = foregroundRequests();
        await page.locator('#filter-' + section).fill('impossible-no-match');
        assert.equal(await page.locator('#filter-empty-' + section).isVisible(), true);
        await page.getByRole('button', { name: 'Clear filter', exact: true }).click();
        assert.equal(await page.locator('#filter-empty-' + section).isVisible(), false);
        assert.equal(await page.locator('#filter-' + section).evaluate((el) => el === document.activeElement), true);
        assert.equal(foregroundRequests(), count, 'filter/clear never fetch');
      }
      await page.evaluate(() => { location.hash = '#/email'; });
      await page.waitForSelector('.email-sent-row');
      await page.waitForSelector('.email-sender');
      await noOverflow();
      await navigate('facts');
      assert.equal(await page.locator('.fact-value .value').first().evaluate((el) => getComputedStyle(el).whiteSpace), 'nowrap');
      await page.locator('a[href="#/facts/fact_plain"]').click();
      await page.waitForSelector('.fact-detail-value');
      assert.equal(await page.locator('.fact-detail-value .value').textContent(), value);
      assert.equal(await page.locator('.fact-detail-value .value').evaluate((el) => getComputedStyle(el).whiteSpace), 'pre-wrap');
      await noOverflow();
      await navigate('transcripts');
      await page.locator('.transcript-open').click();
      await page.waitForSelector('.entry .body');
      await noOverflow();
      if (width <= 700) {
        const body = await page.locator('.entry .body').boundingBox();
        const role = await page.locator('.entry .role').boundingBox();
        const entry = await page.locator('.entry').boundingBox();
        assert.ok(body.y >= role.y + role.height - 1, 'role above body');
        assert.ok(body.width >= entry.width - 2, 'body uses full reading width');
      }
    }
    await page.setViewportSize({ width: 390, height: 844 });
    await navigate('facts');
    const filter = page.locator('#filter-facts');
    await filter.fill('newly-matching-record');
    await page.locator('#clear-filter-facts').focus();
    const beforeLiveFilter = foregroundRequests();
    await page.evaluate((facts) => window.testStream.dispatchEvent(new MessageEvent('facts', {
      data: JSON.stringify({ facts }),
    })), facts);
    assert.equal(await page.locator('#clear-filter-facts').evaluate((el) => el === document.activeElement), true,
      'Clear filter keeps keyboard focus when refreshed records still do not match');
    await page.evaluate((facts) => window.testStream.dispatchEvent(new MessageEvent('facts', {
      data: JSON.stringify({ facts: [...facts, { id: 'fact_arrival', subject: 'newly-matching-record', predicate: 'example/value', value: 'Synthetic arrival' }] }),
    })), facts);
    assert.equal(await page.locator('#filter-empty-facts').isVisible(), false);
    assert.equal(await filter.evaluate((el) => el === document.activeElement), true,
      'Focus returns to input when incoming records hide Clear filter');
    assert.equal(foregroundRequests(), beforeLiveFilter, 'Live focus restoration never fetches');
    await filter.fill('');
    await page.locator('a[href="#/facts/fact_locked"]').click();
    await page.locator('.fact-detail-value').waitFor();
    const reveal = page.getByRole('button', { name: 'reveal sensitive value', exact: true });
    await reveal.waitFor();
    assert.ok((await reveal.boundingBox()).height >= 44);
    await page.getByRole('button', { name: 'copy value without revealing', exact: true }).click();
    await page.waitForFunction(() => window.testCopies.length === 1);
    assert.equal(await page.locator('.fact-detail-value .value').count(), 0, 'copy does not reveal');
    await reveal.click();
    await page.waitForSelector('.fact-detail-value .value');
    await noOverflow();
    await page.getByRole('button', { name: 'hide value', exact: true }).click();
    assert.equal(await page.locator('.fact-detail-value .value').count(), 0);
    await page.evaluate(() => { window.testStream.onopen(); window.testStream.dispatchEvent(new MessageEvent('upstream', { data: JSON.stringify({ source: 'transcripts', ok: false, message: '<unsafe>synthetic</unsafe>' }) })); });
    assert.equal(await page.locator('#live-label').textContent(), 'Connected');
    assert.match(await page.locator('#source-status').textContent(), /transcripts.*stale/);
    assert.equal(await page.locator('unsafe').count(), 0);
    await page.evaluate(() => { location.hash = '#/overview'; });
    await page.waitForSelector('#summary-disclosure-memories');
    assert.equal(await page.locator('a details, a button').count(), 0);
    await page.locator('#summary-disclosure-memories').click();
    await page.evaluate(() => { window.testSummaryNode = document.getElementById('summary-details-memories'); });
    const summaryRequests = requests.filter((url) => url === '/api/summary').length;
    await page.clock.fastForward(30000);
    await page.waitForFunction(() => document.getElementById('summary-details-memories') !== window.testSummaryNode);
    assert.equal(requests.filter((url) => url === '/api/summary').length, summaryRequests + 1);
    assert.equal(await page.locator('#summary-details-memories').evaluate((el) => el.open), true);
    assert.equal(await page.locator('#summary-disclosure-memories').evaluate((el) => el === document.activeElement), true);
    await noOverflow();
    // All five shipped packs, loaded directly without saving a preference.
    for (const theme of ['console', 'paper', 'amber', 'high-contrast', 'midnight']) {
      await page.evaluate((theme) => { document.getElementById('theme-css').href = '/static/themes/' + theme + '.css'; }, theme);
      await page.waitForFunction((theme) => [...document.styleSheets].some((sheet) => sheet.href?.endsWith('/' + theme + '.css')), theme);
      await noOverflow();
    }
    assert.deepEqual(errors, []);
  } finally { await browser.close(); }
});
