// The guards, proven by running sessions that break them.

import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { mkdtempSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { test } from 'node:test';
import { fileURLToPath } from 'node:url';

const here = dirname(fileURLToPath(import.meta.url));

// A fresh run: without NODE_TEST_CONTEXT, which the runner sets for its own
// children, the child would not start a runner of its own.
function run(args, extraEnv = {}) {
  const env = { ...process.env, ...extraEnv };
  delete env.NODE_TEST_CONTEXT;
  return spawnSync(process.execPath, ['--import', './guard.mjs', ...args, 'guardcases/leak_case.mjs'], {
    cwd: here, env, encoding: 'utf8',
  });
}

test('a non-loopback request fails the run even when caught', () => {
  // The file's own process exits 4, both of its tests having passed.
  const direct = run([]);
  assert.equal(direct.status, 4, direct.stdout + direct.stderr);
  assert.match(direct.stderr, /connection to "192\.0\.2\.1" is not loopback/);
  assert.match(direct.stderr, /www\.googleapis\.com.* is not loopback/);
  // Under the runner, as `make compat-node` runs it, that fails the run.
  const runner = run(['--test']);
  assert.equal(runner.status, 1, runner.stdout + runner.stderr);
});

test('a non-fixture credentials file stops the run', () => {
  const dir = mkdtempSync(join(tmpdir(), 'cb-compat-node-'));
  const other = join(dir, 'real-looking.json');
  writeFileSync(other, JSON.stringify({ type: 'service_account', client_email: 'someone@real-project.iam.gserviceaccount.com' }));
  const r = run([], { GOOGLE_APPLICATION_CREDENTIALS: other });
  assert.equal(r.status, 3, r.stdout + r.stderr);
  assert.match(r.stderr, /not the fixture/);
});
