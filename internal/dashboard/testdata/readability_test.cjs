'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const test = require('node:test');
const { dom } = require('./overview_harness.cjs');
const source = fs.readFileSync(path.join(__dirname, '../static/app.js'), 'utf8');
const nextTurn = () => new Promise((resolve) => setImmediate(resolve));

for (const section of ['transcripts', 'facts', 'memories', 'secrets', 'conversations']) {
  test(`${section}: no-match and clear preserve rows, focus, and request inventory`, async () => {
    const d = dom((ok, message) => assert.ok(ok, message));
    const requests = [];
    const responses = {
      '/api/transcripts': { transcripts: [{ id: 'tr_sample', title: 'Synthetic transcript' }] },
      '/api/facts?limit=100': { facts: [{ id: 'fact_sample', subject: 'Synthetic fact', predicate: 'example/value', sensitive: true }] },
      '/api/memories?limit=100': { items: [{ id: 'mem_sample', content: 'Synthetic memory', kind: 'note' }] },
      '/api/secrets?limit=100': { secrets: [{ id: 'sec_sample', name: 'Synthetic secret metadata' }] },
      '/api/messages?direction=inbox&limit=100': { messages: [{ id: 'msg_sample', from: { agent_id: 'ag_sample', agent_name: 'Synthetic peer' }, subject: 'Synthetic message', created_at: '2026-09-23T12:00:00Z' }] },
      '/api/messages?direction=outbox&limit=100': { messages: [] },
    };
    const sandbox = {
      module: { exports: {} }, document: d.document,
      window: { location: { hash: '#/' + section }, matchMedia: null }, URLSearchParams,
      setTimeout() {}, clearTimeout() {},
      EventSource: class { addEventListener() {} close() {} },
      async fetch(url) {
        assert.ok(Object.hasOwn(responses, url), `unexpected request: ${url}`);
        requests.push(url);
        return { ok: true, async json() { return responses[url]; } };
      },
    };
    vm.runInNewContext(source, sandbox);
    sandbox.module.exports.route();
    await nextTurn();
    const input = d.document.getElementById('filter-' + section);
    assert.ok(input, d.nodes.view.textContent);
    const rowSelector = section === 'transcripts' ? '.transcript-row' : '.row';
    const rows = d.nodes.view.querySelectorAll(rowSelector);
    assert.equal(rows.length, 1);
    const count = requests.length;
    input.value = 'no-such-record';
    input.dispatch('input');
    const empty = d.document.getElementById('filter-empty-' + section);
    assert.equal(empty.hidden, false);
    assert.match(empty.textContent, new RegExp('No matching ' + section));
    assert.equal(rows[0].style.display, 'none');
    d.document.getElementById('clear-filter-' + section).dispatch('click');
    assert.equal(empty.hidden, true);
    assert.equal(d.document.activeElement, input);
    assert.equal(rows[0].style.display, '');
    assert.equal(d.nodes.view.querySelectorAll(rowSelector)[0], rows[0], 'same private subtree retained');
    assert.equal(requests.length, count, 'no detail/reveal request');
    assert.equal(d.nodes.view.querySelector('.error'), null);
    // A successful empty response is not a filtered-out nonempty inventory.
    if (section !== 'conversations') {
      for (const response of Object.values(responses)) for (const key of Object.keys(response)) response[key] = [];
      sandbox.module.exports.state.filters[section] = 'saved filter';
      sandbox.module.exports.route();
      await nextTurn();
      assert.equal(d.nodes.view.querySelectorAll(rowSelector).length, 0);
      assert.ok(d.nodes.view.querySelector('.empty'));
      assert.equal(d.document.getElementById('filter-empty-' + section).hidden, true);
    }
  });
}

