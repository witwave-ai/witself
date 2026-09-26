import { chromium } from '@playwright/test';

// Missing binaries may be explicitly waived locally. Broken launches and test
// failures always fail, including when the missing-browser waiver is enabled.
export async function launchChromium(t, { browserType = chromium, env = process.env, report = console.error } = {}) {
  try {
    return await browserType.launch({ headless: true, timeout: 30000 });
  } catch (error) {
    if (!error.message.includes("Executable doesn't exist")) throw error;
    const reason = 'NOT RUN dashboard browser regressions: Chromium is absent; run npx playwright install chromium';
    report(reason);
    if (env.WITSELF_DASHBOARD_BROWSER_SKIP === '1') {
      t.skip(reason + ' (WITSELF_DASHBOARD_BROWSER_SKIP=1)');
      return null;
    }
    throw new Error(reason + '; only WITSELF_DASHBOARD_BROWSER_SKIP=1 permits a skip');
  }
}
