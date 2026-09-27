// Unit tests for the action's CLI version gating (#679).
// Run with: node --test action/lib.test.js (no dependencies).
'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('fs');
const path = require('path');
const {
  actionRoot, installArgs, parseVersion, compareVersions, cliSupport, requireInputs, upFlags, instanceFlags, MIN_CLI,
} = require('./lib');

function cmp(a, b) {
  return compareVersions(parseVersion(a), parseVersion(b));
}

test('parseVersion reads release tags and git describe, and nothing else', () => {
  assert.deepEqual(parseVersion('v0.1.0'), { major: 0, minor: 1, patch: 0, pre: [], describe: false });
  assert.deepEqual(parseVersion('0.2.3\n'), { major: 0, minor: 2, patch: 3, pre: [], describe: false });
  assert.deepEqual(parseVersion('v0.2.0-rc.1').pre, ['rc', '1']);
  assert.equal(parseVersion('v0.1.0-5-gabc1234').describe, true);
  assert.equal(parseVersion('v0.1.0-5-gabc1234-dirty').describe, true);
  assert.deepEqual(parseVersion('v0.1.0-dirty').pre, []);
  for (const bad of ['dev', '', undefined, '63e11c6', 'v1', 'v1.2', 'cloudburrow v0.1.0 (commit 63e11c6)']) {
    assert.equal(parseVersion(bad), null, String(bad));
  }
});

test('compareVersions follows semver precedence', () => {
  assert.equal(cmp('v0.1.0', 'v0.1.0'), 0);
  assert.equal(cmp('v0.1.0', 'v0.1.1'), -1);
  assert.equal(cmp('v0.2.0', 'v0.1.9'), 1);
  assert.equal(cmp('v1.0.0', 'v0.99.99'), 1);
  assert.equal(cmp('v0.2.0-rc.1', 'v0.2.0'), -1);
  assert.equal(cmp('v0.2.0-rc.1', 'v0.1.0'), 1);
  assert.equal(cmp('v0.1.1-rc.1', 'v0.1.0'), 1);
  assert.equal(cmp('v0.2.0-rc.2', 'v0.2.0-rc.10'), -1);
  assert.equal(cmp('v0.2.0-alpha', 'v0.2.0-alpha.1'), -1);
  assert.equal(cmp('v0.2.0-1', 'v0.2.0-alpha'), -1);
  // A build between tags comes after the tag it was described from.
  assert.equal(cmp('v0.1.0-5-gabc1234', 'v0.1.0'), 1);
  assert.equal(cmp('v0.1.0-5-gabc1234', 'v0.1.1'), -1);
});

test('the published v0.1.0 gets neither --trust nor --port-base', () => {
  // Exactly what `cloudburrow version --short` prints for the v0.1.0 release.
  const s = cliSupport('v0.1.0\n');
  assert.deepEqual(s.features, { trust: false, portBase: false });
  assert.equal(s.warning, '');
  assert.deepEqual(upFlags(s, '10m'), ['--detach', '--detach-timeout', '10m']);
  assert.ok(!upFlags(s, '10m').includes('--trust'));
});

test('any release after v0.1.0 gets both', () => {
  for (const v of ['v0.1.1', 'v0.2.0', 'v0.2.0-rc.1', 'v1.0.0', 'v0.1.0-3-g0123abc']) {
    const s = cliSupport(v);
    assert.deepEqual(s.features, { trust: true, portBase: true }, v);
    assert.deepEqual(upFlags(s, '5m'), ['--detach', '--detach-timeout', '5m', '--trust'], v);
    requireInputs(s, { portBase: '9100' });
  }
});

test('a source build gets every flag, whatever version it reports', () => {
  for (const v of ['dev', 'v0.0.0-20260927-abcdef', 'v0.1.0']) {
    const s = cliSupport(v, { source: true });
    assert.deepEqual(s.features, { trust: true, portBase: true }, v);
    assert.equal(s.warning, '');
  }
});

test('an unreadable version is assumed current, with a warning', () => {
  const s = cliSupport('dev');
  assert.deepEqual(s.features, { trust: true, portBase: true });
  assert.match(s.warning, /cannot read the CLI version from "dev"/);
});

test('a CLI older than the minimum is refused, naming the minimum', () => {
  assert.throws(() => cliSupport('v0.0.9'), (err) => {
    assert.match(err.message, new RegExp(`needs ${MIN_CLI.replace(/\./g, '\\.')} or later`));
    assert.match(err.message, /v0\.0\.9/);
    return true;
  });
  assert.throws(() => cliSupport('v0.1.0-rc.1'), /needs v0\.1\.0 or later/);
});

test('port-base with v0.1.0 fails before up, naming the version it needs', () => {
  const s = cliSupport('v0.1.0');
  assert.throws(() => requireInputs(s, { portBase: '9100' }), (err) => {
    assert.match(err.message, /port-base needs a CloudBurrow CLI newer than v0\.1\.0/);
    assert.match(err.message, /installed CLI is v0\.1\.0/);
    assert.doesNotMatch(err.message, /flag provided but not defined/);
    return true;
  });
  // Without the input there is nothing to refuse.
  requireInputs(s, { portBase: '' });
});

test('instanceFlags passes --port-base only when it is set', () => {
  assert.deepEqual(instanceFlags('ci-1', '', 'ephemeral', ''), ['--name', 'ci-1', '--mode', 'ephemeral']);
  assert.deepEqual(instanceFlags('ci-1', 'storage', 'persistent', '9100'),
    ['--name', 'ci-1', '--mode', 'persistent', '--services', 'storage', '--port-base', '9100']);
});

// The runner sets GITHUB_ACTION_PATH only for composite actions; for this
// node action it is unset, which made the release path join `undefined`
// ("The \"path\" argument must be of type string", #679).
test('the action root is found without GITHUB_ACTION_PATH', () => {
  const saved = process.env.GITHUB_ACTION_PATH;
  delete process.env.GITHUB_ACTION_PATH;
  try {
    const root = actionRoot();
    assert.equal(typeof root, 'string');
    assert.ok(fs.existsSync(path.join(root, 'action.yml')), `no action.yml in ${root}`);
    assert.ok(fs.existsSync(path.join(root, 'scripts', 'install.sh')), `no scripts/install.sh in ${root}`);
    assert.ok(fs.existsSync(path.join(root, 'cmd', 'cloudburrow')), `no cmd/cloudburrow in ${root}, which source builds`);
  } finally {
    if (saved !== undefined) process.env.GITHUB_ACTION_PATH = saved;
  }
});

test('installArgs for latest and for a tag', () => {
  const root = actionRoot();
  const sh = path.join(root, 'scripts', 'install.sh');
  assert.deepEqual(installArgs(root, '/t/cloudburrow', 'latest', 'tok'), [sh, '--prefix', '/t/cloudburrow']);
  assert.deepEqual(installArgs(root, '/t/cloudburrow', 'v0.1.0', ''),
    [sh, '--prefix', '/t/cloudburrow', '--version', 'v0.1.0', '--no-attest']);
  for (const a of installArgs(root, '/t/cloudburrow', 'latest', '')) assert.equal(typeof a, 'string');
});
