// Shared helpers for the setup-cloudburrow action.
//
// Deliberately dependency-free: the action runs `cloudburrow` and a few
// system tools, and nothing here needs @actions/core. The runner's file
// protocols (GITHUB_ENV, GITHUB_PATH, GITHUB_OUTPUT, GITHUB_STATE) are
// written directly, as the toolkit would write them.
'use strict';

const { spawnSync } = require('child_process');
const fs = require('fs');
const path = require('path');
const crypto = require('crypto');

// actionRoot is the directory holding action.yml: the checkout the runner
// made of this action (or the workspace, with `uses: ./`). It comes from this
// file's own location because the runner sets GITHUB_ACTION_PATH only for
// composite actions, not node ones: reading it gave `undefined`, which broke
// installing a release and let `source` build the workspace instead (#679).
function actionRoot() {
  return path.resolve(__dirname, '..');
}

function input(name, fallback) {
  const v = process.env[`INPUT_${name.replace(/ /g, '_').toUpperCase()}`];
  return v === undefined || v.trim() === '' ? fallback : v.trim();
}

// append writes one entry to a runner file. A value with a newline uses the
// delimiter form, so it can never be read as a second entry.
function append(fileVar, key, value) {
  const file = process.env[fileVar];
  if (!file) throw new Error(`${fileVar} is not set; is this running on GitHub Actions?`);
  value = String(value);
  if (value.includes('\n')) {
    const delim = `CLOUDBURROW_${crypto.randomBytes(8).toString('hex')}`;
    fs.appendFileSync(file, `${key}<<${delim}\n${value}\n${delim}\n`);
  } else {
    fs.appendFileSync(file, `${key}=${value}\n`);
  }
}

// run executes a command with its output in the job log, and fails on a
// non-zero exit.
function run(cmd, args, opts = {}) {
  console.log(`$ ${cmd} ${args.join(' ')}`);
  const r = spawnSync(cmd, args, { stdio: 'inherit', ...opts });
  if (r.error) throw new Error(`${cmd}: ${r.error.message}`);
  if (r.status !== 0) throw new Error(`${cmd} ${args[0] || ''} exited with status ${r.status}`);
}

// capture executes a command and returns its stdout.
function capture(cmd, args, opts = {}) {
  const r = spawnSync(cmd, args, { encoding: 'utf8', ...opts });
  if (r.error) throw new Error(`${cmd}: ${r.error.message}`);
  if (r.status !== 0) throw new Error(`${cmd} ${args.join(' ')} exited with status ${r.status}: ${r.stderr}`);
  return r.stdout;
}

function has(cmd) {
  return spawnSync(cmd, ['version'], { stdio: 'ignore' }).error === undefined;
}

// installArgs are the arguments to `sh` that install a release (a tag, or
// latest) into <prefix>/bin with the action's own scripts/install.sh.
function installArgs(root, prefix, version, token) {
  const args = [path.join(root, 'scripts', 'install.sh'), '--prefix', prefix];
  if (version !== 'latest') args.push('--version', version);
  if (!token) args.push('--no-attest');
  return args;
}

// instanceFlags are passed to every cloudburrow command, so each one names the
// same instance and computes the same ports.
function instanceFlags(name, services, mode, portBase) {
  const flags = ['--name', name, '--mode', mode];
  if (services) flags.push('--services', services);
  // A distinct name gives a job its own cluster, not its own host ports: two
  // jobs on one self-hosted runner still need different port bases (#584).
  if (portBase) flags.push('--port-base', portBase);
  return flags;
}

function fail(err) {
  console.log(`::error title=setup-cloudburrow::${String(err.message || err).replace(/\r?\n/g, ' ')}`);
  process.exitCode = 1;
}

// The oldest CLI this action drives: every command and flag it passes
// unconditionally (up --detach --detach-timeout, wait --timeout, env --format,
// status --format, logs --tail, delete, --name/--mode/--services) is in it.
const MIN_CLI = 'v0.1.0';

// Flags newer than MIN_CLI, each passed only to a CLI that has it (#679).
// `after` is the last release without the flag: until the release that adds
// it is tagged its number is unknown, and "any release after v0.1.0" is what
// is true of it.
const CLI_FEATURES = {
  // v0.1.0 has no trust gate: it reads a discovered cloudburrow.json and
  // .cloudburrow/hooks unasked, so leaving the flag off keeps the behaviour
  // --trust asks for (#598, #619).
  trust: { flag: '--trust', after: 'v0.1.0' },
  // No equivalent in v0.1.0, which moves ports only one by one: an input
  // asking for it fails before `up` rather than being ignored (#584).
  portBase: { flag: '--port-base', after: 'v0.1.0' },
};

// parseVersion reads what `cloudburrow version --short` prints: a release tag
// (v0.1.0, v0.2.0-rc.1), or `git describe` for a build between tags
// (v0.1.0-5-gabc1234, maybe -dirty), which comes after that tag. Anything
// else ("dev", a bare commit) is null: not a version this action can place.
function parseVersion(s) {
  const m = /^v?(\d+)\.(\d+)\.(\d+)(?:-([0-9A-Za-z.-]+))?(?:\+[0-9A-Za-z.-]+)?$/.exec(String(s || '').trim());
  if (!m) return null;
  const v = { major: +m[1], minor: +m[2], patch: +m[3], pre: [], describe: false };
  let pre = m[4] || '';
  if (/^\d+-g[0-9a-f]+(-dirty)?$/.test(pre)) {
    v.describe = true;
    pre = '';
  } else if (pre === 'dirty') {
    pre = '';
  }
  v.pre = pre ? pre.split('.') : [];
  return v;
}

// compareVersions orders two parsed versions by semver precedence, with a
// `git describe` build just after the tag it describes.
function compareVersions(a, b) {
  for (const k of ['major', 'minor', 'patch']) {
    if (a[k] !== b[k]) return a[k] < b[k] ? -1 : 1;
  }
  if (a.pre.length === 0 || b.pre.length === 0) {
    if (a.pre.length !== b.pre.length) return a.pre.length === 0 ? 1 : -1;
    return Number(a.describe) - Number(b.describe);
  }
  for (let i = 0; i < Math.max(a.pre.length, b.pre.length); i++) {
    if (i >= a.pre.length) return -1;
    if (i >= b.pre.length) return 1;
    const x = a.pre[i];
    const y = b.pre[i];
    if (x === y) continue;
    const xn = /^\d+$/.test(x);
    const yn = /^\d+$/.test(y);
    if (xn && yn) return +x < +y ? -1 : 1;
    if (xn !== yn) return xn ? -1 : 1;
    return x < y ? -1 : 1;
  }
  return 0;
}

// cliSupport decides which of CLI_FEATURES the installed CLI has, from its
// `version --short`. A CLI built from the action's own checkout (`source`)
// is the same commit as the action, so it has all of them; so, with a
// warning, has one whose version cannot be read, which is a local build
// rather than a release. A release older than MIN_CLI is refused.
function cliSupport(versionOutput, { source = false } = {}) {
  const all = () => Object.fromEntries(Object.keys(CLI_FEATURES).map((k) => [k, true]));
  const shown = String(versionOutput || '').trim();
  if (source) return { version: shown, features: all(), warning: '' };
  const v = parseVersion(shown);
  if (!v) {
    return {
      version: shown,
      features: all(),
      warning: `cannot read the CLI version from ${JSON.stringify(shown)}; assuming it is current and passing every flag`,
    };
  }
  if (compareVersions(v, parseVersion(MIN_CLI)) < 0) {
    throw new Error(`the installed CloudBurrow CLI is ${shown}; this action needs ${MIN_CLI} or later. Set version to ${MIN_CLI} or later, or latest`);
  }
  const features = {};
  for (const [k, f] of Object.entries(CLI_FEATURES)) {
    features[k] = compareVersions(v, parseVersion(f.after)) > 0;
  }
  return { version: shown, features, warning: '' };
}

// requireInputs fails for an input the installed CLI cannot honour, naming
// the version that can, before anything is started.
function requireInputs(support, { portBase }) {
  if (portBase && !support.features.portBase) {
    throw new Error(`port-base needs a CloudBurrow CLI newer than ${CLI_FEATURES.portBase.after}, which has no --port-base; ` +
      `the installed CLI is ${support.version}. Set version to a later release or source, or leave port-base empty`);
  }
}

// upFlags are the flags only `up` takes, as far as the installed CLI has them.
// --trust: the workflow's author chose to run CloudBurrow on this checkout,
// so its cloudburrow.json and .cloudburrow/hooks are theirs (#598).
function upFlags(support, timeout) {
  const flags = ['--detach', '--detach-timeout', timeout];
  if (support.features.trust) flags.push(CLI_FEATURES.trust.flag);
  return flags;
}

module.exports = {
  actionRoot, installArgs, input, append, run, capture, has, instanceFlags, fail,
  MIN_CLI, CLI_FEATURES, parseVersion, compareVersions, cliSupport, requireInputs, upFlags,
};
