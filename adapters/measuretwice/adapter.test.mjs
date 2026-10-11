import assert from 'node:assert/strict';
import { test } from 'node:test';
import { assess } from './assess.mjs';

const packet = { version: 1, revision: 'test-revision', task: { details: 'Return the sum.' }, patch: '+ return a + b', verification: 'passed' };
for (const [fixture, outcome] of Object.entries({ supported: 'pass', contradicted: 'fail', insufficient: 'review' })) {
  test(`real Measuretwice runner maps ${fixture} to ${outcome}`, async () => {
    const report = await assess(packet, { fixture });
    assert.equal(report.aggregate.outcome, outcome);
    assert.equal(report.qualification.status, 'unvalidated');
    assert.equal(report.fixture, true);
  });
}
test('oversized evidence stays unassessed', async () => {
  const report = await assess({ ...packet, patch: '字'.repeat(9000) }, { fixture: 'supported' });
  assert.equal(report.outcome, 'review');
  assert.equal(report.reason, 'evidence_too_large');
});
