// setup-cloudburrow: install, start, wait, export.
'use strict';

const fs = require('fs');
const path = require('path');
const { input, append, run, capture, has, instanceFlags, fail } = require('./lib');

// defaultName is ci-<run id>-<job id>, so two jobs of one run never share an
// instance even on one self-hosted runner, within CloudBurrow's name rule:
// [a-z0-9-], at most 32 characters, starting and ending alphanumeric.
function defaultName() {
  const job = (process.env.GITHUB_JOB || '').toLowerCase().replace(/[^a-z0-9-]+/g, '-');
  const name = `ci-${process.env.GITHUB_RUN_ID || 'local'}-${job}`.slice(0, 32);
  return name.replace(/-+$/, '');
}

function main() {
  const version = input('version', 'latest');
  const services = input('services', '');
  const mode = input('mode', 'ephemeral');
  const name = input('name', defaultName());
  const timeout = input('timeout', '10m');
  if (!['ephemeral', 'persistent'].includes(mode)) throw new Error(`mode must be ephemeral or persistent, not ${mode}`);

  // What post needs, recorded before anything is started, so a failure
  // part-way still leaves post able to clean up.
  append('GITHUB_STATE', 'name', name);
  append('GITHUB_STATE', 'services', services);
  append('GITHUB_STATE', 'mode', mode);

  // 1. The binary, in <prefix>/bin, which is where install.sh puts it.
  const prefix = path.join(process.env.RUNNER_TEMP || '/tmp', 'cloudburrow');
  const binDir = path.join(prefix, 'bin');
  fs.mkdirSync(binDir, { recursive: true });
  if (version === 'source') {
    // The repository the action was checked out from: with `uses: ./` in
    // this repository, that is the code under test.
    run('go', ['build', '-o', path.join(binDir, 'cloudburrow'), './cmd/cloudburrow'], { cwd: process.env.GITHUB_ACTION_PATH });
  } else {
    // The installer verifies the SHA-256 against the release's checksums,
    // and its build attestation when gh can authenticate.
    const args = [path.join(process.env.GITHUB_ACTION_PATH, 'scripts', 'install.sh'), '--prefix', prefix];
    if (version !== 'latest') args.push('--version', version);
    const token = input('github-token', '');
    if (!token) args.push('--no-attest');
    run('sh', args, { env: { ...process.env, GH_TOKEN: token } });
  }
  const bin = path.join(binDir, 'cloudburrow');
  append('GITHUB_STATE', 'bin', bin);
  // GITHUB_PATH takes one bare directory per line.
  fs.appendFileSync(process.env.GITHUB_PATH, binDir + '\n');
  run(bin, ['version']);

  // 2. Prerequisites, named rather than discovered as a kind stack trace.
  const missing = ['docker', 'kind', 'kubectl'].filter((c) => !has(c));
  if (missing.length) {
    throw new Error(`missing on this runner: ${missing.join(', ')}. ` +
      'Docker is on the GitHub-hosted Ubuntu runners; install kind and kubectl first, for example with helm/kind-action (install_only: true).');
  }
  run('docker', ['info', '--format', '{{.ServerVersion}}']);

  // 3 and 4. Start in the background and wait for readiness.
  const flags = instanceFlags(name, services, mode);
  run(bin, ['up', '--detach', '--detach-timeout', timeout, ...flags]);
  run(bin, ['wait', '--timeout', '1m', ...flags]);

  // 5. The environment, as `cloudburrow env` gives it to a developer.
  const env = capture(bin, ['env', '--format', 'plain', ...flags]);
  const exported = [];
  for (const line of env.split('\n')) {
    const i = line.indexOf('=');
    if (i <= 0) continue;
    append('GITHUB_ENV', line.slice(0, i), line.slice(i + 1));
    exported.push(line.slice(0, i));
  }
  console.log(`exported ${exported.join(', ')}`);

  append('GITHUB_OUTPUT', 'name', name);
  append('GITHUB_OUTPUT', 'bin-dir', binDir);
}

try {
  main();
} catch (err) {
  fail(err);
}
