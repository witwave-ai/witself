import {existsSync, rmSync} from 'node:fs';

// Flags cover init and direct fixture commands without changing user config.
export function fixtureGitArgs(args) {
  return ['-c', 'gc.auto=0', '-c', 'maintenance.auto=false', ...args];
}

export function configureFixtureGit(git) {
  // Code under test may sanitize GIT_* variables, so indirect Git commands
  // also need these settings in this disposable repository only.
  git('config', '--local', 'gc.auto', '0');
  git('config', '--local', 'maintenance.auto', 'false');
}

function wait(milliseconds) {
  Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, milliseconds);
}

export function cleanupGitFixture(dir, testName, {remove = rmSync, exists = existsSync, pause = wait} = {}) {
  for (let attempt = 0; attempt < 2; attempt++) {
    try {
      remove(dir, {recursive: true, force: true, maxRetries: 10, retryDelay: 100});
    } catch {
      // Retry residual files without exposing paths or raw filesystem errors.
    }
    if (!exists(dir)) return;
    if (attempt === 0) pause(100);
  }
  throw new Error(`Temporary Git fixture cleanup failed for test: ${testName}`);
}
