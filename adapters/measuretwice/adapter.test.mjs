import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { mkdtemp, readFile, rm, symlink } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
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

test('a symlinked invocation writes the same report as a direct one', async (t) => {
  const dir = await mkdtemp(join(tmpdir(), 'measuretwice-symlink-'));
  t.after(() => rm(dir, { recursive: true, force: true }));
  const module = fileURLToPath(new URL('./assess.mjs', import.meta.url));
  const link = join(dir, 'assess-link.mjs');
  await symlink(module, link);
  const reports = {};
  // A missing evidence file makes the CLI write an error report and exit 1.
  const run = (name, script) => {
    reports[name] = join(dir, `${name}-report.json`);
    return spawnSync(process.execPath, [script], {
      env: { ...process.env, LOOPER_EVIDENCE_FILE: join(dir, 'missing-evidence.json'), LOOPER_ASSESS_REPORT: reports[name] },
      encoding: 'utf8',
    });
  };
  const expectations = [];
  for (const [name, script] of [['direct', module], ['symlinked', link]]) {
    const result = run(name, script);
    assert.equal(result.status, 1, `${name} invocation exit status`);
    expectations.push(JSON.parse(await readFile(reports[name], 'utf8')));
  }
  for (const report of expectations) {
    assert.equal(report.outcome, 'error');
    assert.match(report.reason, /missing-evidence\.json/);
  }
  assert.deepEqual(expectations[0], expectations[1]);
});
