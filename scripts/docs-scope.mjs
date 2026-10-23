// The docs fast lane is intentionally limited to research notes and their index.
// Expanding this list is a validation-policy change and takes the full lane.
import {createHash} from 'node:crypto';
import {lstatSync, readFileSync, readdirSync} from 'node:fs';
import {join, resolve} from 'node:path';

export const docsIndex = 'docs/README.md';
export const docsDirectory = 'docs/research/local-cell';

export function validPath(path) {
  return typeof path === 'string' && path.length > 0 && !path.startsWith('/') &&
    !path.split('/').some(part => part === '..' || part === '.' || !part) &&
    !/[\0\ufffd\\]/u.test(path);
}

export function isDocsPath(path) {
  if (!validPath(path)) return false;
  return path === docsIndex || /^docs\/research\/local-cell\/[^./][^/]*\.md$/u.test(path);
}

function regularFile(cwd, path) {
  const parts = path.split('/');
  for (let i = 1; i <= parts.length; i++) {
    const info = lstatSync(join(cwd, ...parts.slice(0, i)));
    if (info.isSymbolicLink() || (i < parts.length ? !info.isDirectory() : !info.isFile())) {
      throw new Error('nonregular-docs-input');
    }
    if (i === parts.length && (info.mode & 0o111)) throw new Error('executable-docs-input');
  }
  return {path, body: readFileSync(join(cwd, path))};
}

// Read every eligible document, not just changed paths. The caller can validate
// the complete index/link contract without coupling it to a Git diff. Throws
// on missing, symlinked, nonregular, or executable inputs or parent directories.
// Returned bodies are raw Buffers; callers choose their own strict text parser.
export function collectDocsInputs(cwd = process.cwd()) {
  cwd = resolve(cwd);
  const index = regularFile(cwd, docsIndex);
  const parts = docsDirectory.split('/');
  for (let i = 1; i <= parts.length; i++) {
    const info = lstatSync(join(cwd, ...parts.slice(0, i)));
    if (!info.isDirectory() || info.isSymbolicLink()) throw new Error('nonregular-docs-directory');
  }
  const paths = readdirSync(join(cwd, docsDirectory)).map(name => `${docsDirectory}/${name}`)
    .filter(isDocsPath).sort();
  return [index, ...paths.map(path => regularFile(cwd, path))];
}

// Stable raw-byte evidence for local callers bracketing docs checks. This covers
// docs inputs only; callers must also recheck Git scope/integrity before success.
export function captureDocsSnapshot(cwd = process.cwd()) {
  const hash = createHash('sha256');
  for (const {path, body} of collectDocsInputs(cwd)) {
    hash.update(`${Buffer.byteLength(path)}:${path}:100644:${body.length}:`).update(body);
  }
  return hash.digest('hex');
}
