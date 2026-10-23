import test from 'node:test';
import assert from 'node:assert/strict';
import {execFileSync, spawnSync} from 'node:child_process';
import {createHash} from 'node:crypto';
import {mkdtempSync, mkdirSync, writeFileSync, readFileSync, rmSync, symlinkSync} from 'node:fs';
import {tmpdir} from 'node:os';
import {dirname, join, resolve} from 'node:path';
import {fileURLToPath} from 'node:url';
import {checkDocs, DocsCheckError} from './check-docs.mjs';
import {collectDocsInputs} from './docs-scope.mjs';

const script = fileURLToPath(new URL('./check-docs.mjs', import.meta.url));
const repository = resolve(dirname(script), '..');
const research = 'docs/research/local-cell/README.md';
function fixture(t, body = '# Research\n', extras = {}) {
  const cwd = mkdtempSync(join(tmpdir(), 'witself-docs-[fixture]-'));
  t.after(() => rmSync(cwd, {recursive: true, force: true}));
  const put = (path, content) => {
    mkdirSync(dirname(join(cwd, path)), {recursive: true});
    writeFileSync(join(cwd, path), content);
  };
  execFileSync('git', ['init', '-q'], {cwd});
  put('docs/README.md', '# Docs\n[Research](research/local-cell/README.md)\n');
  put(research, body);
  for (const [path, content] of Object.entries(extras)) put(path, content);
  return {cwd, put, check: () => checkDocs({cwd})};
}
function fails(f, pattern, line) {
  assert.throws(f.check, error => {
    assert.ok(error instanceof DocsCheckError);
    assert.match(error.message, pattern);
    if (line !== undefined) assert.ok(error.diagnostics.some(item => item.file === research && item.line === line && pattern.test(item.message)), error.message);
    return true;
  });
}

test('checks all bounded docs including newly added docs, without parsing links in other documents', t => {
  const f = fixture(t, '# Research\n[Other](other.md)\n[Elsewhere](../../elsewhere.md)\n', {
    'docs/research/local-cell/other.md': '# Other\n',
    'docs/elsewhere.md': '# Elsewhere\n[Deliberately out of scope](missing.md)\n',
  });
  assert.deepEqual(f.check(), {passed: true, files: ['docs/README.md', research, 'docs/research/local-cell/other.md'], links_checked: 3, external_links_skipped: 0});
  f.put('docs/research/local-cell/new.md', '# New\n[Broken](missing.md)\n');
  fails(f, /missing local target: missing.md/);
});

test('source selection cannot bypass the bounded allowlist or silently select no inputs', t => {
  const f = fixture(t);
  for (const files of [[], ['docs/README.md', 'docs/README.md'], ['../README.md'], ['docs/elsewhere.md'], ['docs/research/local-cell/nested/a.md'], ['docs/research/local-cell/missing.md'], 'docs/README.md']) {
    assert.throws(() => checkDocs({cwd: f.cwd, files}), DocsCheckError);
  }
  assert.deepEqual(checkDocs({cwd: f.cwd, files: [research]}).files, [research]);
});

test('rejects invalid UTF-8, NUL, and actual conflict markers with original source lines', t => {
  const f = fixture(t);
  f.put(research, Buffer.from([35, 32, 0xc3, 0x28]));
  fails(f, /invalid UTF-8/, 1);
  f.put(research, '# Header\n\0');
  fails(f, /NUL byte/, 1);
  f.put(research, '# Header\r\n\r\n<<<<<<< branch\r\ntext\r\n=======\r\nother\r\n>>>>>>> branch\r\n');
  fails(f, /unresolved conflict marker/, 3);
});

test('relative links, images, directories, angle destinations, escapes, and titles', t => {
  const f = fixture(t, `# Research
[Dir](.)
[Space](<space name.md> "title")
[Encoded](space%20name.md)
[Balanced](a(b).md 'title')
[Escaped](a\\(b\\).md)
[Entity](a&amp;b.md)
![Picture](image.png "picture")
[Across lines](
  space%20name.md
  "multiline title"
)
[self]()
`, {
    'docs/research/local-cell/space name.md': '# Space\n',
    'docs/research/local-cell/a(b).md': '# Balanced\n',
    'docs/research/local-cell/a&b.md': '# Entity\n',
    'docs/research/local-cell/image.png': Buffer.from([0x89, 0x50, 0x4e, 0x47]),
  });
  assert.equal(f.check().links_checked, 10);
  f.put(research, '# Research\n![Directory image](.)\n');
  fails(f, /image target must be a regular file/, 2);
});

test('full, collapsed, shortcut, image and multiline reference definitions are checked', t => {
  const f = fixture(t, `# Research
[Full][the target]
[The target][]
[The target]
![Image][image]
[Across][multiline]
[With title][titled]
[ordinary prose]

[the target]: target.md "Target"
[image]: picture.png
[multiline]:
  <target.md>
[titled]: target.md
  'More information'
`, {'docs/research/local-cell/target.md': '# Target\n', 'docs/research/local-cell/picture.png': 'image'});
  assert.equal(f.check().links_checked, 11);
  f.put(research, '# Research\n[Broken][missing]\n');
  fails(f, /undefined reference link \[missing\]/, 2);
  f.put(research, '# Research\n[unused]: missing.md\n');
  fails(f, /missing local target: missing.md/, 2);
  f.put(research, '[same]: target.md\n[SAME]: target.md\n');
  fails(f, /duplicate reference definition/, 2);
});

test('fenced, inline, HTML comment and HTML code examples are excluded', t => {
  const f = fixture(t, '# Research\n' + [
    '```md', '[Missing](missing.md)', '```',
    '~~~~md', '[Missing][undefined]', '~~~', '[Missing](missing.md)', '~~~~',
    '> ```md', '> [Missing](missing.md)', '> ```',
    '- ```md', '  [Missing](missing.md)', '  ```',
    '`[Missing](missing.md)` and ``[Missing](`file`).``',
    '\\[Not a link](missing.md)',
    '<!-- [Missing](missing.md) -->',
    '<pre>[Missing](missing.md)</pre>', '<code>[Missing](missing.md)</code>',
    '[Present](#research)',
  ].join('\n') + '\n');
  assert.equal(f.check().links_checked, 2);
});

test('linked images and nested labels cannot hide local targets behind an external link', t => {
  const f = fixture(t, '# Research\n[![Image](missing.png)](https://example.invalid)\n');
  fails(f, /missing local target: missing.png/, 2);
  f.put(research, '# Research\n[![Image][picture]][website]\n[picture]: missing.png\n[website]: https://example.invalid\n');
  fails(f, /missing local target: missing.png/, 2);
  f.put('docs/research/local-cell/missing.png', 'image');
  assert.equal(f.check().passed, true);
});

test('indented code is ignored without hiding list continuation links or creating anchors', t => {
  const f = fixture(t, '# Research\n\n    [Example](missing.md)\n    <a id="fake"></a>\n\n- List item\n\n    [Actual](#research)\n');
  assert.equal(f.check().links_checked, 2);
  f.put(research, '# Research\n\n    <a id="fake"></a>\n\n[Fake](#fake)\n');
  fails(f, /missing Markdown anchor: #fake/, 5);
  f.put(research, '# Research\n\n- List item\n\n    [Actual](missing.md)\n');
  fails(f, /missing local target: missing.md/, 5);
});

test('code-span delimiters inside link destinations do not erase the authored path', t => {
  const f = fixture(t, '# Research\n[File](back`tick`.md)\n<a href="back`tick`.md">File</a>\n', {
    'docs/research/local-cell/back`tick`.md': '# Target\n',
  });
  assert.equal(f.check().passed, true);
  f.put(research, '# Research\n[File](missing`tick`.md)\n');
  fails(f, /missing local target: missing`tick`.md/, 2);
});

test('Markdown anchors include duplicates, collisions, Unicode, inline markup, and setext headings', t => {
  const f = fixture(t, `# Research
## Repeat
## Repeat
## Repeat-1
## Repeat
## Café & déjà_vu!
## Use \`code\` and **bold**
## [Read this](target.md)
## [Nested destination](a(b).md)
## _Emphasis_. and \`_literal_\`
Setext title
------------
[1](#repeat) [2](#repeat-1) [3](#repeat-1-1) [4](#repeat-2)
[Unicode](#caf%C3%A9--d%C3%A9j%C3%A0_vu)
[Markup](#use-code-and-bold) [Label](#read-this) [Setext](#setext-title)
[Cross file](target.md#target-heading)
[Nested](#nested-destination) [Formatting](#emphasis-and-_literal_)
`, {'docs/research/local-cell/target.md': '# Target heading\n', 'docs/research/local-cell/a(b).md': '# Balanced\n'});
  assert.equal(f.check().passed, true);
  f.put(research, '# Research\n[Missing](#absent)\n');
  fails(f, /missing Markdown anchor: #absent/, 2);
  f.put(research, '# Research\n```md\n## Hidden\n```\n[Missing](#hidden)\n');
  fails(f, /missing Markdown anchor: #hidden/, 5);
});

test('HTML anchors, href and src are checked; unsupported HTML link forms fail clearly', t => {
  const f = fixture(t, `# Research
<a id="Exact_ID"></a>
<a name='legacy'></a>
<span id="section-id">Text</span>
[Exact](#Exact_ID) [Legacy](#legacy) [Section](#section-id)
<a href="target.md#target">Target</a>
<img src='picture.png' alt='picture'>
`, {'docs/research/local-cell/target.md': '# Target\n', 'docs/research/local-cell/picture.png': 'image'});
  assert.equal(f.check().links_checked, 6);
  f.put(research, '# Research\n<img src="missing.png">\n');
  fails(f, /missing local target: missing.png/, 2);
  f.put(research, '# Research\n<img srcset="one.png 1x, two.png 2x">\n');
  fails(f, /unsupported srcset/, 2);
  f.put(research, '# Research\n<a href="missing.md"\n');
  fails(f, /unsupported HTML link tag/, 2);
});

test('source line ranges use real line counts, including CRLF and terminal-newline boundaries', t => {
  const f = fixture(t, '# Research\n[First](../../../source.go#L1) [Range](../../../source.go#L1-L3)\n[Markdown lines](target.md#L2)\n', {
    'source.go': 'one\r\ntwo\r\nthree\r\n', 'docs/research/local-cell/target.md': '# Target\nbody\n',
  });
  assert.equal(f.check().passed, true);
  for (const fragment of ['L0', 'L4', 'L3-L2', 'L1-L4', 'L1-3', 'L999999999999999999999']) {
    f.put(research, `# Research\n[Bounds](../../../source.go#${fragment})\n`);
    fails(f, /source line range/, 2);
  }
  f.put('source.go', 'one\ntwo\nthree');
  f.put(research, '[Last](../../../source.go#L3)\n');
  assert.equal(f.check().passed, true);
  f.put('source.go', '');
  fails(f, /outside 0 lines/);
  f.put('source.go', Buffer.from([0xff]));
  fails(f, /invalid UTF-8/);
});

test('current source checks do not excuse missing or out-of-range links in historical prose', t => {
  const f = fixture(t, '# Historical\nThis research used revision `abc123`.\n[Old code](../../../old.go#L99)\n', {'old.go': 'one\n'});
  fails(f, /outside 1 lines/, 3);
  f.put(research, '# Historical\n[Old code](../../../missing.go#L1)\n');
  fails(f, /missing local target/, 2);
});

test('rejects lexical and symlink repository escapes and Git metadata', t => {
  const f = fixture(t);
  const outside = mkdtempSync(join(tmpdir(), 'witself-docs-outside-'));
  t.after(() => rmSync(outside, {recursive: true, force: true}));
  writeFileSync(join(outside, 'outside.md'), '# Outside\n');
  symlinkSync(join(outside, 'outside.md'), join(f.cwd, 'outside-link.md'));
  for (const [target, pattern] of [
    ['../../../../outside.md', /escapes repository/],
    ['..%2F..%2F..%2F..%2Foutside.md', /escapes repository/],
    ['../../../outside-link.md', /escapes repository through symlink/],
    ['../../../.git/config', /Git metadata/],
    ['/etc/passwd', /absolute local links/],
  ]) {
    f.put(research, `# Research\n[Unsafe](${target})\n`);
    fails(f, pattern, 2);
  }
  symlinkSync(join(f.cwd, 'docs/README.md'), join(f.cwd, 'inside-link.md'));
  f.put(research, '[Inside](../../../inside-link.md#docs)\n');
  assert.equal(f.check().passed, true);
});

test('source symlink escape is rejected before reading its body', t => {
  const f = fixture(t);
  const outside = mkdtempSync(join(tmpdir(), 'witself-docs-source-'));
  t.after(() => rmSync(outside, {recursive: true, force: true}));
  writeFileSync(join(outside, 'outside.md'), '# Outside\n');
  rmSync(join(f.cwd, research));
  symlinkSync(join(outside, 'outside.md'), join(f.cwd, research));
  assert.throws(f.check, DocsCheckError);
});

test('unsupported and malformed local links report their source line instead of disappearing', t => {
  const f = fixture(t);
  for (const body of [
    '[Link](target.md?raw=1)', '[Link](target%ZZ.md)', '[Link](target.md "unfinished)',
    '[Link](target.md extra)', '[Link](<target.md)', '[Link](target(foo.md)',
    '[Link](file:///tmp/target.md)', '[Link](//host/target.md)', '[[target.md]]',
    '[Link]({{site.baseurl}}/target.md)', '[Link](bad%00name.md)', '[Link](unknown&copy;.md)',
    '[Link](../../../code.go#heading)', '[Link](.#heading)',
    '<file:///outside.md>', '<custom:missing>', '<object data="file.pdf"></object>',
  ]) {
    f.put(research, '# Research\n' + body + '\n');
    assert.throws(f.check, error => error instanceof DocsCheckError && error.diagnostics.some(item => item.line === 2), body);
  }
});

test('external URLs are counted without network access, including pinned historical references', t => {
  const f = fixture(t, '# Research\n[Unavailable](https://does-not-resolve.invalid/path#heading)\n[Historical](https://github.com/example/project/blob/abc123/file.go#L99999)\n[Email](mailto:someone@example.invalid)\n');
  assert.equal(f.check().external_links_skipped, 3);
});

test('cwd must be the actual Git worktree root and cannot be redirected by ambient GIT_DIR', t => {
  const f = fixture(t);
  for (const cwd of ['', null, 1, join(f.cwd, 'docs'), join(f.cwd, research), join(f.cwd, 'missing')]) assert.throws(() => checkDocs({cwd}), DocsCheckError);
  const old = process.env.GIT_DIR;
  process.env.GIT_DIR = '/does/not/exist';
  try { assert.equal(f.check().passed, true); }
  finally { if (old === undefined) delete process.env.GIT_DIR; else process.env.GIT_DIR = old; }
});

test('CLI emits an actual PASS or FAIL with the corresponding exit status', t => {
  const f = fixture(t);
  const call = args => spawnSync(process.execPath, [script, ...args], {cwd: f.cwd, encoding: 'utf8'});
  let result = call([]);
  assert.equal(result.status, 0);
  assert.match(result.stdout, /docs check: PASS/);
  assert.equal(result.stderr, '');
  f.put(research, '[Bad](missing.md)\n');
  result = call([]);
  assert.equal(result.status, 1);
  assert.match(result.stderr, /docs check: FAIL/);
  assert.doesNotMatch(result.stdout, /PASS/);
  assert.equal(call(['--ignore-missing']).status, 1);
});

test('current repository proof validates the entire allowlist without changing its bytes', () => {
  const snapshot = () => collectDocsInputs(repository).map(({path, body}) => [path, createHash('sha256').update(body).digest('hex')]);
  const before = snapshot();
  const result = checkDocs({cwd: repository});
  assert.equal(result.passed, true);
  assert.equal(result.files.length, before.length);
  assert.ok(result.links_checked > 0);
  assert.deepEqual(snapshot(), before);
});
