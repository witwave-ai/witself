#!/usr/bin/env node
// Required check names remain real checks in every lane. A missing, failed,
// cancelled, or unexpectedly skipped dependency must never become green.
import assert from 'node:assert/strict';
import {pathToFileURL} from 'node:url';

export function assertGate(needs) {
  assert(needs && typeof needs === 'object' && !Array.isArray(needs), 'Missing dependency results');
  assert.equal(needs.changes?.result, 'success', 'Change classification must succeed');
  const outputs = needs.changes.outputs;
  for (const name of ['application', 'docs', 'docs_index', 'docs_only', 'website', 'website_only']) {
    assert(['true', 'false'].includes(outputs?.[name]), `Missing or invalid scope output: ${name}`);
  }
  const application = outputs.application === 'true';
  const docs = outputs.docs === 'true';
  const docsOnly = outputs.docs_only === 'true';
  const docsIndex = outputs.docs_index === 'true';
  const website = outputs.website === 'true';
  const websiteOnly = outputs.website_only === 'true';
  assert.equal(application, !(websiteOnly || docsOnly), 'Inconsistent application scope');
  assert(!websiteOnly || (website && !docs && !docsOnly), 'Brochure-only scope must validate only the website');
  assert(!docsOnly || (docs && !website && !websiteOnly), 'Docs-only scope must validate only documentation');
  assert(!docsIndex || docs, 'Documentation index scope must validate documentation');
  assert.equal(needs.website?.result, website ? 'success' : 'skipped', 'Website validation did not match its scope');
  assert.equal(needs.docs?.result, docs ? 'success' : 'skipped', 'Documentation validation did not match its scope');
  const applicationResults = Object.entries(needs).filter(([name]) => !['changes', 'website', 'docs'].includes(name));
  assert(applicationResults.length > 0, 'Missing application dependencies');
  for (const [name, dependency] of applicationResults) {
    assert.equal(dependency?.result, application ? 'success' : 'skipped', `Application gate did not match its scope: ${name}`);
  }
  return docsOnly ? 'docs-only' : websiteOnly ? 'brochure-only' : 'application';
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  const lane = assertGate(JSON.parse(process.env.NEEDS_JSON ?? 'null'));
  const messages = {
    'docs-only': 'PASS: docs-only change; documentation validation succeeded; application gates are not applicable.',
    'brochure-only': 'PASS: brochure-only change; website validation succeeded; application gates are not applicable.',
    application: 'PASS: all required application gates and any affected documentation and website validation succeeded.',
  };
  console.log(messages[lane]);
}
