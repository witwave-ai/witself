#!/usr/bin/env node
// Required check names remain real checks in either lane. A missing, failed,
// cancelled, or unexpectedly skipped dependency must never become green.
import assert from 'node:assert/strict';
import {pathToFileURL} from 'node:url';

export function assertGate(needs) {
  assert(needs && typeof needs === 'object' && !Array.isArray(needs), 'Missing dependency results');
  assert.equal(needs.changes?.result, 'success', 'Change classification must succeed');
  const outputs = needs.changes.outputs;
  for (const name of ['application', 'website', 'website_only']) {
    assert(['true', 'false'].includes(outputs?.[name]), `Missing or invalid scope output: ${name}`);
  }
  const application = outputs.application === 'true';
  const website = outputs.website === 'true';
  const websiteOnly = outputs.website_only === 'true';
  assert.equal(application, !websiteOnly, 'Inconsistent application scope');
  assert(!websiteOnly || website, 'Brochure-only scope must validate the website');
  assert.equal(needs.website?.result, website ? 'success' : 'skipped', 'Website validation did not match its scope');
  const applicationResults = Object.entries(needs).filter(([name]) => !['changes', 'website'].includes(name));
  assert(applicationResults.length > 0, 'Missing application dependencies');
  for (const [name, dependency] of applicationResults) {
    assert.equal(dependency?.result, application ? 'success' : 'skipped', `Application gate did not match its scope: ${name}`);
  }
  return websiteOnly ? 'brochure-only' : 'application';
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  const lane = assertGate(JSON.parse(process.env.NEEDS_JSON ?? 'null'));
  console.log(lane === 'brochure-only'
    ? 'PASS: brochure-only change; website validation succeeded; application gates are not applicable.'
    : 'PASS: all required application gates and any affected website validation succeeded.');
}
