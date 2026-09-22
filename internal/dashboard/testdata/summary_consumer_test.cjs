"use strict";
const test=require('node:test');
const assert=require('node:assert/strict');
const fs=require('node:fs');
const {fixture}=require('./overview_harness.cjs');
test('Go and JS agree on new activity snapshots',t=>{
 const path=process.env.WITSELF_SUMMARY_FIXTURE;
 assert.ok(path,'run via TestSummaryActivityConsumerAgreement with generated synthetic snapshots');
 const h=fixture(t);
 for(const [i,sample] of JSON.parse(fs.readFileSync(path,'utf8')).entries()) {
  const normalized=h.app.normalizeSummary(sample);
  assert.deepEqual([normalized.categories[0].activity.status,normalized.categories[3].activity.status],sample.expected,`sample ${i}`);
 }
});
