#!/usr/bin/env node
// Read-only structural checks for the small docs lane. No examples are executed
// and no remote URLs are fetched. Existence/line bounds do not prove source meaning.
import {execFileSync} from 'node:child_process';
import {readFileSync, realpathSync, statSync} from 'node:fs';
import {dirname, extname, isAbsolute, relative, resolve, sep} from 'node:path';
import {pathToFileURL} from 'node:url';
import {collectDocsInputs, isDocsPath} from './docs-scope.mjs';

export class DocsCheckError extends Error {
  constructor(diagnostics) {
    super(diagnostics.map(item => `${item.file}:${item.line}: ${item.message}`).join('\n'));
    this.name = 'DocsCheckError';
    this.diagnostics = diagnostics;
  }
}

const blank = text => text.replace(/[^\n\r]/g, ' ');
const escaped = (text, index) => {
  let count = 0;
  while (index > 0 && text[--index] === '\\') count++;
  return count % 2 === 1;
};
const lineAt = (text, index) => text.slice(0, index).split('\n').length;
const inside = (root, path) => {
  const rel = relative(root, path);
  return rel !== '..' && !rel.startsWith(`..${sep}`) && !isAbsolute(rel);
};
const unescapeMarkdown = text => text.replace(/\\([!"#$%&'()*+,\-./:;<=>?@[\]\\^_`{|}~])/g, '$1');
const entities = Object.freeze({amp: '&', quot: '"', apos: "'", lt: '<', gt: '>', nbsp: '\u00a0'});
function decodeEntities(text, strict = false) {
  return text.replace(/&(#x[0-9a-f]+|#[0-9]+|[a-z][a-z0-9]+);/gi, (match, name) => {
    if (name[0] !== '#') {
      if (Object.hasOwn(entities, name)) return entities[name];
      if (strict) throw new Error(`unsupported HTML entity ${match} in link; use percent encoding`);
      return match;
    }
    const number = name[1].toLowerCase() === 'x' ? Number.parseInt(name.slice(2), 16) : Number(name.slice(1));
    if (!Number.isInteger(number) || number <= 0 || number > 0x10ffff || (number >= 0xd800 && number <= 0xdfff)) {
      throw new Error('invalid numeric HTML entity');
    }
    return String.fromCodePoint(number);
  });
}

function repositoryRoot(cwd) {
  if (typeof cwd !== 'string' || !cwd || cwd.includes('\0')) throw new Error('cwd must be a nonempty repository-root path');
  const root = realpathSync(cwd);
  if (!statSync(root).isDirectory()) throw new Error('cwd must be a directory');
  // Do not let inherited Git overrides silently select a different repository.
  const env = Object.fromEntries(Object.entries(process.env).filter(([key]) => !key.startsWith('GIT_')));
  env.GIT_OPTIONAL_LOCKS = '0';
  const top = execFileSync('git', ['-c', 'core.fsmonitor=false', 'rev-parse', '--show-toplevel'], {
    cwd: root, env, encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'], timeout: 10000,
  }).trim();
  if (realpathSync(top) !== root) throw new Error('cwd must be the Git worktree root');
  return root;
}

function decodeUTF8(body) {
  try { return new TextDecoder('utf-8', {fatal: true}).decode(body); }
  catch { throw new Error('invalid UTF-8'); }
}

// Preserve offsets/newlines so diagnostics always refer to original source lines.
function withoutBlocks(text) {
  let fence, listIndent;
  let previousBlank = true, indented = false;
  const lines = text.split('\n').map(line => {
    let content = line.replace(/^(?: {0,3}>[ \t]?)+/, '');
    const list = /^ {0,3}(?:[-+*]|\d+[.)])[ \t]+/.exec(content);
    if (list) { listIndent = list[0].length; content = content.slice(listIndent); }
    else if (listIndent && content.startsWith(' '.repeat(listIndent))) content = content.slice(listIndent);
    else if (content.trim() && !/^\s/.test(content)) listIndent = undefined;
    const marker = /^ {0,3}(`{3,}|~{3,})(.*)$/.exec(content);
    if (fence) {
      if (marker && marker[1][0] === fence[0] && marker[1].length >= fence.length && /^\s*$/.test(marker[2])) fence = undefined;
      previousBlank = true;
      return blank(line);
    }
    if (marker && !(marker[1][0] === '`' && marker[2].includes('`'))) {
      fence = marker[1];
      previousBlank = true;
      return blank(line);
    }
    if (/^(?: {4}|\t)/.test(content) && (previousBlank || indented)) {
      indented = true;
      return blank(line);
    }
    previousBlank = !content.trim();
    if (!previousBlank) indented = false;
    return line;
  });
  return lines.join('\n')
    .replace(/<!--[\s\S]*?(?:-->|$)/g, blank)
    .replace(/<(pre|code)\b[^>]*>[\s\S]*?<\/\1\s*>/gi, blank);
}

function withoutInlineCode(text) {
  let result = '', offset = 0;
  for (let i = 0; i < text.length; i++) {
    if (text[i] !== '`' || escaped(text, i)) continue;
    let end = i + 1;
    while (text[end] === '`') end++;
    const delimiter = text.slice(i, end);
    let close = text.indexOf(delimiter, end);
    while (close !== -1 && (text[close - 1] === '`' || text[close + delimiter.length] === '`')) close = text.indexOf(delimiter, close + delimiter.length);
    if (close === -1) { i = end - 1; continue; }
    close += delimiter.length;
    result += text.slice(offset, i) + blank(text.slice(i, close));
    offset = close;
    i = close - 1;
  }
  return result + text.slice(offset);
}

function closingBracket(text, start) {
  let depth = 1;
  for (let i = start + 1; i < text.length; i++) {
    if (escaped(text, i)) continue;
    if (text[i] === '[') depth++;
    if (text[i] === ']' && --depth === 0) return i;
  }
  return -1;
}

// The intentionally bounded parser supports CommonMark destinations, balanced
// parentheses, angle destinations, optional titles, and multiline whitespace.
// An encountered link that uses other syntax is an error, never a skipped check.
function destination(text, start, inline) {
  let cursor = start;
  while (/\s/.test(text[cursor] ?? '') && cursor < text.length) cursor++;
  const begin = cursor;
  let value;
  if (text[cursor] === '<') {
    cursor++;
    const from = cursor;
    while (cursor < text.length && !(text[cursor] === '>' && !escaped(text, cursor))) {
      if (text[cursor] === '\n' || text[cursor] === '<') throw new Error('unsupported angle link destination');
      cursor++;
    }
    if (cursor === text.length) throw new Error('unterminated angle link destination');
    value = text.slice(from, cursor++);
  } else {
    let depth = 0;
    while (cursor < text.length) {
      const character = text[cursor];
      if (!escaped(text, cursor)) {
        if (/\s/.test(character)) break;
        if (character === '<' || character === '>') throw new Error('unsupported link destination; use angle brackets or percent encoding');
        if (character === '(') depth++;
        if (character === ')') { if (!depth) break; depth--; }
      }
      cursor++;
    }
    if (depth) throw new Error('unbalanced parentheses in link destination');
    value = text.slice(begin, cursor);
  }
  const beforeSpace = cursor;
  while (/\s/.test(text[cursor] ?? '') && cursor < text.length) cursor++;
  if (cursor > beforeSpace && ['"', "'", '('].includes(text[cursor])) {
    const quote = text[cursor++] === '(' ? ')' : text[cursor - 1];
    while (cursor < text.length && !(text[cursor] === quote && !escaped(text, cursor))) cursor++;
    if (cursor === text.length) throw new Error('unterminated link title');
    cursor++;
    while (/\s/.test(text[cursor] ?? '') && cursor < text.length) cursor++;
  }
  if (inline ? text[cursor] !== ')' : cursor !== text.length) throw new Error('unsupported or malformed link destination/title');
  return {value, end: inline ? cursor + 1 : cursor};
}

const referenceKey = text => unescapeMarkdown(text).trim().replace(/\s+/g, ' ').toLowerCase();

function htmlTags(text, report) {
  const tags = [];
  const pattern = /<([a-z][a-z0-9-]*)(\s[^<>]*?)?\s*\/?\s*>/gi;
  for (const match of text.matchAll(pattern)) {
    const attrs = new Map();
    const source = match[2] ?? '';
    const attrPattern = /([^\s=/'"<>]+)(?:\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s'"=<>`]+)))?/g;
    for (const attr of source.matchAll(attrPattern)) {
      const name = attr[1].toLowerCase();
      if (attrs.has(name)) report(match.index, `duplicate HTML attribute ${name}`);
      attrs.set(name, attr[2] ?? attr[3] ?? attr[4] ?? null);
    }
    // Quoted attributes containing > are deliberately unsupported: the regex
    // does not consume them as a complete tag, and the check below rejects them.
    tags.push({name: match[1].toLowerCase(), attrs, start: match.index, end: match.index + match[0].length});
  }
  for (const match of text.matchAll(/<(?:a|img|source|video|audio|iframe|link)\b[^\n]*/gi)) {
    if (!tags.some(tag => tag.start === match.index)) report(match.index, 'unsupported HTML link tag; use a complete tag with quoted href/src');
  }
  return tags;
}

function parseLinks(text, report, inheritedRefs = new Map()) {
  const blocks = withoutBlocks(text);
  let visible = withoutInlineCode(blocks);
  const refs = new Map(inheritedRefs), links = [];
  const definitions = /^ {0,3}\[([^\]\n]+)\]:[ \t]*(.*)$/gm;
  const removals = [];
  for (const match of visible.matchAll(definitions)) {
    let end = match.index + match[0].length;
    let body = blocks.slice(end - match[2].length, end);
    // A destination and/or optional title may continue on the next line.
    if (!body.trim()) {
      const next = /^[ \t]*([^\n]+)/.exec(blocks.slice(end + 1));
      if (next) { body = next[1]; end += 1 + next[0].length; }
    }
    const title = /^[ \t]+(["'(][^\n]*)/.exec(blocks.slice(end + 1));
    if (title) { body += `\n${title[1]}`; end += 1 + title[0].length; }
    try {
      const parsed = destination(body, 0, false);
      if (!parsed.value) throw new Error('reference definition has no destination');
      const key = referenceKey(match[1]);
      if (refs.has(key)) throw new Error(`duplicate reference definition [${match[1]}]`);
      const link = {value: parsed.value, start: match.index};
      refs.set(key, link);
      links.push(link); // Validate unused definitions too: they are authored targets.
    } catch (error) { report(match.index, error.message); }
    removals.push([match.index, end]);
  }
  for (const [start, end] of removals.reverse()) visible = visible.slice(0, start) + blank(visible.slice(start, end)) + visible.slice(end);
  const tagStarts = new Map(htmlTags(visible, () => {}).map(tag => [tag.start, tag]));
  const markdownRanges = [];
  for (let i = 0; i < visible.length; i++) {
    // Skip actual HTML while scanning Markdown. A link consumed from its opening
    // bracket takes priority over a tag-like angle destination inside that link.
    if (tagStarts.has(i)) { i = tagStarts.get(i).end - 1; continue; }
    if (visible[i] !== '[' || escaped(visible, i)) continue;
    if (visible[i + 1] === '[') { report(i, 'unsupported wiki link syntax; use a Markdown link'); i++; continue; }
    const close = closingBracket(visible, i);
    if (close === -1) continue; // Unmatched prose brackets are not links.
    const label = visible.slice(i + 1, close);
    // Linked images are still local resources, even when their outer link is
    // external. Inspect nested labels before consuming the complete outer link.
    if (label.includes('[')) {
      const offset = i + 1;
      const nested = parseLinks(blocks.slice(offset, close), (index, message) => report(offset + index, message), refs);
      links.push(...nested.map(link => ({...link, start: offset + link.start})));
    }
    const image = visible[i - 1] === '!' && !escaped(visible, i - 1);
    if (visible[close + 1] === '(') {
      try {
        const parsed = destination(blocks, close + 2, true);
        links.push({value: parsed.value, start: i, image});
        markdownRanges.push([i, parsed.end]);
        i = parsed.end - 1;
      } catch (error) { report(i, error.message); i = close + 1; }
    } else if (visible[close + 1] === '[') {
      const end = closingBracket(visible, close + 1);
      if (end === -1) { report(i, 'unterminated reference link'); continue; }
      const key = referenceKey(visible.slice(close + 2, end) || label);
      if (!refs.has(key)) report(i, `undefined reference link [${key}]`);
      else links.push({...refs.get(key), start: i, image});
      i = end;
    } else {
      const ref = refs.get(referenceKey(label));
      if (ref) links.push({...ref, start: i, image});
      i = close;
    }
  }
  for (const [start, end] of markdownRanges.reverse()) visible = visible.slice(0, start) + blank(visible.slice(start, end)) + visible.slice(end);
  for (const scanned of htmlTags(visible, report)) {
    const raw = htmlTags(blocks.slice(scanned.start, scanned.end), () => {})[0];
    const tag = raw ? {...raw, start: scanned.start} : scanned;
    for (const name of ['href', 'src']) {
      if (!tag.attrs.has(name)) continue;
      const value = tag.attrs.get(name);
      if (value === null) report(tag.start, `HTML ${name} has no destination`);
      else links.push({value, start: tag.start, image: name === 'src'});
    }
    if (tag.attrs.has('srcset')) report(tag.start, 'unsupported srcset; use a single Markdown image or src target');
    for (const name of ['poster', 'data']) {
      if (tag.attrs.has(name)) report(tag.start, `unsupported HTML resource attribute ${name}; use a Markdown link or image`);
    }
  }
  for (const match of visible.matchAll(/<([a-z][a-z0-9+.-]*:[^<>\s]*)>/gi)) links.push({value: match[1], start: match.index});
  return links;
}

function headingText(text) {
  const plain = value => value.replace(/\*+|~~/g, '')
    .replace(/(^|[^\p{L}\p{N}\\])(_{1,3})(?!\s)(.+?)(?<!\\)\2(?![\p{L}\p{N}_])/gu, '$1$3');
  const visible = withoutInlineCode(text);
  let result = '', start = 0;
  const append = (position, end, value) => { result += plain(text.slice(start, position)) + value; start = end; };
  for (let i = 0; i < text.length; i++) {
    if (escaped(text, i)) continue;
    if (text[i] === '`') {
      let end = i + 1;
      while (text[end] === '`') end++;
      const delimiter = text.slice(i, end);
      let close = text.indexOf(delimiter, end);
      while (close !== -1 && (text[close - 1] === '`' || text[close + delimiter.length] === '`')) close = text.indexOf(delimiter, close + delimiter.length);
      if (close !== -1) {
        let value = text.slice(end, close).replace(/\s/g, ' ');
        if (/^ .* $/.test(value) && value.trim()) value = value.slice(1, -1);
        append(i, close + delimiter.length, value); i = start - 1;
      }
    } else if (text[i] === '[') {
      const close = closingBracket(visible, i);
      if (close === -1) continue;
      let end = close + 1;
      if (text[end] === '(') {
        try { end = destination(text, end + 1, true).end; } catch { continue; }
      } else if (text[end] === '[') {
        const refEnd = closingBracket(visible, end);
        if (refEnd !== -1) end = refEnd + 1;
      }
      append(text[i - 1] === '!' ? i - 1 : i, end, headingText(text.slice(i + 1, close)));
      i = end - 1;
    } else if (text[i] === '<') {
      const close = text.indexOf('>', i + 1);
      if (close !== -1) {
        const value = text.slice(i + 1, close);
        append(i, close + 1, /^[a-z][a-z0-9+.-]*:/i.test(value) ? value : '');
        i = close;
      }
    }
  }
  return decodeEntities(result + plain(text.slice(start)));
}

function markdownAnchors(text) {
  const visible = withoutBlocks(text), ids = new Set(), used = new Set();
  const addHeading = value => {
    // GitHub-style slugs retain Unicode letters/marks, underscores, and dashes.
    const base = unescapeMarkdown(headingText(value)).toLowerCase()
      .replace(/[^\p{L}\p{M}\p{N}\p{Pc}\-\s]/gu, '').replace(/ /g, '-');
    let slug = base, count = 0;
    while (used.has(slug)) slug = `${base}-${++count}`;
    used.add(slug); ids.add(slug);
  };
  const lines = visible.split('\n');
  for (let i = 0; i < lines.length; i++) {
    const line = lines[i].replace(/^(?: {0,3}>[ \t]?)+/, '');
    const atx = /^ {0,3}#{1,6}(?:[ \t]+|$)(.*)$/.exec(line);
    if (atx) addHeading(atx[1].replace(/[ \t]+#+[ \t]*$/, '').trim());
    else if (i > 0 && /^ {0,3}(?:=+|-+)\s*$/.test(line) && lines[i - 1].trim() && !/^\s*(?:[-*+]\s|\d+[.)]\s|[|>])/.test(lines[i - 1])) addHeading(lines[i - 1].trim());
  }
  for (const tag of htmlTags(withoutInlineCode(visible), () => {})) {
    for (const name of tag.name === 'a' ? ['id', 'name'] : ['id']) {
      const value = tag.attrs.get(name);
      if (typeof value === 'string') ids.add(decodeEntities(value));
    }
  }
  return ids;
}

export function checkDocs({cwd = process.cwd(), files} = {}) {
  const diagnostics = [];
  const report = (file, line, message) => diagnostics.push({file, line, message});
  let root, inputs;
  try {
    root = repositoryRoot(cwd);
    inputs = collectDocsInputs(root);
    if (files !== undefined) {
      if (!Array.isArray(files) || !files.length || !files.every(file => typeof file === 'string' && isDocsPath(file)) || new Set(files).size !== files.length) throw new Error('files must be a nonempty unique array of allowed docs paths');
      const byPath = new Map(inputs.map(input => [input.path, input]));
      if (files.some(file => !byPath.has(file))) throw new Error('requested docs file is missing');
      inputs = files.map(file => byPath.get(file));
    }
    if (!inputs.length || inputs.length > 128) throw new Error('docs source set is empty or exceeds the bounded 128-file lane');
  } catch (error) {
    throw new DocsCheckError([{file: '<repository>', line: 1, message: error instanceof DocsCheckError ? error.message : 'cannot collect docs: ' + (error.code ?? error.message)}]);
  }
  const texts = new Map(), anchors = new Map();
  let linksChecked = 0, externalSkipped = 0;
  const readText = path => {
    if (!texts.has(path)) {
      if (statSync(path).size > 32 * 1024 * 1024) throw new Error('local text target exceeds 32 MiB bound');
      const content = decodeUTF8(readFileSync(path));
      if (content.includes('\0')) throw new Error('NUL byte in local text target');
      texts.set(path, content);
    }
    return texts.get(path);
  };
  for (const input of inputs) {
    let text;
    try {
      if (input.body.length > 2 * 1024 * 1024) throw new Error('docs source exceeds 2 MiB bound');
      text = decodeUTF8(input.body);
      if (text.includes('\0')) throw new Error('NUL byte in Markdown source');
      texts.set(resolve(root, input.path), text);
    } catch (error) { report(input.path, 1, error.message); continue; }
    for (const match of text.matchAll(/^(?:<{7,}|={7,}|>{7,}|\|{7,})(?:[ \t].*)?\r?$/gm)) report(input.path, lineAt(text, match.index), 'unresolved conflict marker');
    const links = parseLinks(text, (index, message) => report(input.path, lineAt(text, index), message));
    for (const link of links) {
      try {
        const value = decodeEntities(unescapeMarkdown(link.value), true);
        if (/^(?:https?:|mailto:|tel:)/i.test(value)) { externalSkipped++; continue; }
        if (/^[a-z][a-z0-9+.-]*:/i.test(value) || value.startsWith('//')) throw new Error('unsupported URL scheme; use an explicit HTTP(S) URL or repository-relative target');
        if (/[\x00-\x1f\x7f\\]/.test(value) || /\{\{|\}\}/.test(value)) throw new Error('unsupported local-link syntax');
        const split = value.indexOf('#');
        const rawPath = split < 0 ? value : value.slice(0, split);
        const rawFragment = split < 0 ? '' : value.slice(split + 1);
        if (rawPath.includes('?')) throw new Error('unsupported query on local link');
        let targetPath, fragment;
        try { targetPath = decodeURIComponent(rawPath); fragment = decodeURIComponent(rawFragment); }
        catch { throw new Error('invalid percent encoding in local link'); }
        if (/[\x00-\x1f\x7f\\]/.test(targetPath + fragment)) throw new Error('invalid control character or backslash in local link');
        if (targetPath.startsWith('/')) throw new Error('absolute local links are unsupported; use a repository-relative path');
        const target = resolve(root, dirname(input.path), targetPath || '.');
        // An empty destination/fragment addresses the source document itself.
        const actual = rawPath ? target : resolve(root, input.path);
        if (!inside(root, actual)) throw new Error('local link escapes repository');
        const rel = relative(root, actual).split(sep);
        if (rel.includes('.git')) throw new Error('local link targets Git metadata');
        let real, stat;
        try { real = realpathSync(actual); stat = statSync(real); }
        catch { throw new Error(`missing local target: ${targetPath || input.path}`); }
        if (!inside(root, real)) throw new Error('local link escapes repository through symlink');
        if (relative(root, real).split(sep).includes('.git')) throw new Error('local link targets Git metadata through symlink');
        if (!stat.isFile() && !stat.isDirectory()) throw new Error('local target is not a regular file or directory');
        if (link.image && !stat.isFile()) throw new Error('image target must be a regular file');
        if (fragment) {
          if (!stat.isFile()) throw new Error('fragment on directory link is unsupported; link its Markdown file explicitly');
          const content = readText(real);
          const range = /^L([1-9][0-9]*)(?:-L([1-9][0-9]*))?$/.exec(fragment);
          if (range) {
            const first = Number(range[1]), last = Number(range[2] ?? range[1]);
            const count = content ? content.split('\n').length - (content.endsWith('\n') ? 1 : 0) : 0;
            if (!Number.isSafeInteger(first) || !Number.isSafeInteger(last) || first > last || last > count) throw new Error(`source line range #${fragment} is outside ${count} lines`);
          } else if (/^L[0-9]/.test(fragment)) throw new Error('malformed source line range; use #L1 or #L1-L2');
          else {
            if (!/\.(?:md|markdown)$/i.test(extname(real))) throw new Error('non-Markdown fragment is unsupported; source files require #L1 or #L1-L2');
            if (!anchors.has(real)) anchors.set(real, markdownAnchors(content));
            if (!anchors.get(real).has(fragment)) throw new Error(`missing Markdown anchor: #${fragment}`);
          }
        }
        linksChecked++;
      } catch (error) { report(input.path, lineAt(text, link.start), error.message); }
    }
  }
  if (diagnostics.length) throw new DocsCheckError(diagnostics);
  return {passed: true, files: inputs.map(input => input.path), links_checked: linksChecked, external_links_skipped: externalSkipped};
}

export function main(args = process.argv.slice(2)) {
  if (args.length) throw new DocsCheckError([{file: '<arguments>', line: 1, message: 'usage: node scripts/check-docs.mjs'}]);
  const result = checkDocs();
  console.log(`docs check: PASS (${result.files.length} files, ${result.links_checked} local links; ${result.external_links_skipped} external links not fetched)`);
  return result;
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  try { main(); }
  catch (error) {
    console.error(error instanceof DocsCheckError ? error.message : 'docs checker could not complete');
    console.error('docs check: FAIL');
    process.exitCode = 1;
  }
}
