import assert from 'node:assert/strict';
import test from 'node:test';
import { launchChromium } from './browser.mjs';

for (const skip of [undefined, '0', 'true', '1']) {
  test(`missing Chromium is loud and requires the exact waiver (skip=${skip})`, async () => {
    const reports = [], skips = [];
    const launch = () => launchChromium({ skip: (reason) => skips.push(reason) }, {
      browserType: { async launch() { throw new Error("browserType.launch: Executable doesn't exist at synthetic-path"); } },
      env: { WITSELF_DASHBOARD_BROWSER_SKIP: skip },
      report: (line) => reports.push(line),
    });
    if (skip === '1') {
      assert.equal(await launch(), null);
      assert.equal(skips.length, 1);
      assert.match(skips[0], /WITSELF_DASHBOARD_BROWSER_SKIP=1/);
    } else {
      await assert.rejects(launch, /NOT RUN.*Chromium is absent/);
      assert.deepEqual(skips, []);
    }
    assert.equal(reports.length, 1);
    assert.match(reports[0], /^NOT RUN.*playwright install chromium/);
  });
}

test('the missing-browser waiver never hides a broken browser launch', async () => {
  const failure = new Error('synthetic launch failure');
  await assert.rejects(() => launchChromium({ skip() { assert.fail('must not skip'); } }, {
    browserType: { async launch() { throw failure; } },
    env: { WITSELF_DASHBOARD_BROWSER_SKIP: '1' },
    report() { assert.fail('must not report a missing binary'); },
  }), (error) => error === failure);
});

test('an installed browser still runs when the missing-browser waiver is set', async () => {
  const browser = {};
  assert.equal(await launchChromium({ skip() { assert.fail('must run'); } }, {
    browserType: { async launch(options) {
      assert.deepEqual(options, { headless: true, timeout: 30000 });
      return browser;
    } },
    env: { WITSELF_DASHBOARD_BROWSER_SKIP: '1' },
  }), browser);
});
