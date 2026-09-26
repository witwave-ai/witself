import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';
import { launchChromium } from './browser.mjs';

// Real app, shell, stylesheet, and browser layout; only transport is synthetic.
// Run npm run test:browser after installing Playwright Chromium.
const staticRoot = new URL('../../internal/dashboard/static/', import.meta.url);
const message = (number, peer = 'peer') => ({
  id: `${peer}-message-${number}`, subject: `Message ${number}`,
  created_at: `2026-09-22T12:00:${String(number).padStart(2, '0')}Z`,
  from: { agent_id: peer, agent_name: peer }, to: { kind: 'agent' },
  read_state: { state: 'unread' },
});

for (const { viewport, tallFooter } of [
  { viewport: { width: 390, height: 844 } },
  { viewport: { width: 1280, height: 900 } },
  { viewport: { width: 390, height: 844 }, tallFooter: true },
]) {
  test(`conversation preserves reading position at ${viewport.width}x${viewport.height}${tallFooter ? ' with a tall footer' : ''}`, { timeout: 120000 }, async (t) => {
    const browser = await launchChromium(t);
    if (!browser) return;
    try {
      const page = await browser.newPage({ viewport });
      const errors = [];
      page.on('pageerror', (error) => errors.push(error.message));
      await page.addInitScript(() => {
        window.EventSource = class extends EventTarget {
          constructor() { super(); window.testStream = this; }
          close() {}
        };
      });
      await page.route('http://dashboard.test/**', async (route) => {
        const url = new URL(route.request().url());
        if (url.pathname === '/') {
          return route.fulfill({ contentType: 'text/html', body: await readFile(new URL('index.html', staticRoot), 'utf8') });
        }
        if (['/static/app.js', '/static/base.css', '/static/themes/console.css'].includes(url.pathname)) {
          return route.fulfill({ contentType: url.pathname.endsWith('.js') ? 'text/javascript' : 'text/css',
            body: await readFile(new URL(url.pathname.slice('/static/'.length), staticRoot), 'utf8') });
        }
        if (url.pathname === '/api/avatar.svg') {
          return route.fulfill({ contentType: 'image/svg+xml', body: '<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 80 80"/>' });
        }
        // The console polls account context at boot; this fixture has no manager credential.
        if (url.pathname === '/api/account/context') { return route.fulfill({ status: 403, json: { error: 'forbidden' } }); }
        const responses = {
          '/api/themes': { themes: ['console'] },
          '/api/prefs': { preferences: { prefs: { theme: 'console' } } },
          '/api/messages': { messages: url.searchParams.get('direction') === 'inbox' ? ['peer', 'other'].flatMap((peer) => Array.from({ length: 30 }, (_, i) => message(i + 1, peer))) : [] },
        };
        assert.ok(Object.hasOwn(responses, url.pathname), `unexpected request: ${url.pathname}`);
        return route.fulfill({ json: responses[url.pathname] });
      });
      await page.goto('http://dashboard.test/#/conversations');
      if (tallFooter) {
        await page.addStyleTag({ content: '.statusbar { min-height: 160px; }' });
      }
      await page.locator('#view a[href="#/conversations/peer"]').click();
      await page.waitForFunction(() => document.querySelectorAll('.bubble').length === 30);
      const assertLatestVisible = async (number) => {
        const rect = await page.locator('.bubble').last().boundingBox();
        assert.ok(rect && rect.y >= 0 && rect.y + rect.height <= viewport.height,
          `message ${number} must be fully visible: ${JSON.stringify(rect)}`);
        assert.equal(await page.locator('.bubble .subject').last().textContent(), `Message ${number}`);
      };
      // Let layout and scroll anchoring settle after every synthetic transport
      // frame. Assertions inspect the actual scrolling owner, not a mock call.
      const settle = () => page.evaluate(() => new Promise((resolve) => {
        requestAnimationFrame(() => requestAnimationFrame(resolve));
      }));
      const metrics = () => page.evaluate(() => {
        const view = document.getElementById('view');
        const owner = view.clientHeight >= view.scrollHeight ? document.scrollingElement : view;
        return { top: owner.scrollTop, max: owner.scrollHeight - owner.clientHeight,
          documentOwned: owner === document.scrollingElement };
      });
      const scroll = async (offset, fromBottom = false) => {
        await page.evaluate(({ offset, fromBottom }) => {
          const view = document.getElementById('view');
          const owner = view.clientHeight >= view.scrollHeight ? document.scrollingElement : view;
          owner.scrollTop = fromBottom ? owner.scrollHeight - owner.clientHeight - offset : offset;
        }, { offset, fromBottom });
        await settle();
        return (await metrics()).top;
      };
      const emit = async (incoming) => {
        await page.evaluate((incoming) => {
          window.testStream.dispatchEvent(new MessageEvent('messages', {
            data: JSON.stringify({ inbox: [incoming], outbox: [] }),
          }));
        }, incoming);
        await settle();
      };
      const assertPosition = async (expected, reason) => {
        assert.ok(Math.abs((await metrics()).top - expected) <= 1, reason);
      };
      await settle();
      assert.equal((await metrics()).documentOwned, viewport.width === 390);
      assert.ok((await metrics()).max > 1000, 'fixture must require scrolling');
      await assertLatestVisible(30);

      if (tallFooter) {
        const footer = await page.locator('.statusbar').boundingBox();
        assert.ok(footer.height > 64, 'footer must exceed the follow threshold');
        const initial = await metrics();
        assert.ok(initial.max - initial.top > 64, 'initial positioning leaves the footer below the viewport');
        // No manual scrolling: the initial conversation positioning must put
        // the reader within follow range even with a large footer below it.
        await emit(message(31));
        assert.equal(await page.locator('.bubble').count(), 31);
        await assertLatestVisible(31);
        await emit(message(32));
        await assertLatestVisible(32);
        assert.deepEqual(errors, []);
        return;
      }

      await scroll(32, true);
      await emit(message(31));
      assert.equal(await page.locator('.bubble').count(), 31);
      await assertLatestVisible(31);

      const readingTop = await scroll(320);
      const before = await page.locator('#view').innerHTML();
      await emit(message(31, 'other'));
      assert.equal(await page.locator('#view').innerHTML(), before);
      await assertPosition(readingTop, 'unrelated peer preserves reading position');
      await emit({ ...message(31), read_state: { state: 'read' } });
      await assertPosition(readingTop, 'metadata refresh preserves reading position');
      await emit(message(32));
      assert.equal(await page.locator('.bubble').count(), 32);
      await assertPosition(readingTop, 'related new message preserves reading position');

      const nearTop = await scroll(20, true);
      await emit({ ...message(32), read_state: { state: 'read' } });
      await assertPosition(nearTop, 'near-bottom metadata refresh does not force follow');
      await emit(message(32, 'other'));
      await assertPosition(nearTop, 'near-bottom unrelated arrival does not force follow');
      await scroll(0);
      await emit(message(33));
      await assertPosition(0, 'related arrival preserves navigation position');
      await emit(message(33, 'other'));
      await assertPosition(0, 'unrelated arrival preserves navigation position');

      await page.evaluate(() => { location.hash = '#/conversations/other'; });
      await page.waitForFunction(() => document.querySelector('.message-preview')?.dataset.messageId === 'other-message-1');
      await settle();
      await assertLatestVisible(33);
      await scroll(200);
      await page.evaluate(() => { location.hash = '#/conversations/peer'; });
      await page.waitForFunction(() => document.querySelector('.message-preview')?.dataset.messageId === 'peer-message-1');
      await settle();
      await assertLatestVisible(33);
      assert.deepEqual(errors, []);
    } finally {
      await browser.close();
    }
  });
}
