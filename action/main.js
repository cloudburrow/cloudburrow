// setup-cloudburrow: install, start, wait, export.
'use strict';

const fs = require('fs');
const path = require('path');
const {
  actionRoot, installArgs, input, append, run, capture, has, instanceFlags, fail, cliSupport, requireInputs, upFlags,
} = require('./lib');

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
  const portBase = input('port-base', '');
  if (!['ephemeral', 'persistent'].includes(mode)) throw new Error(`mode must be ephemeral or persistent, not ${mode}`);
  if (portBase && !/^[0-9]+$/.test(portBase)) throw new Error(`port-base must be a port number, not ${portBase}`);

  // What post needs, recorded before anything is started, so a failure
  // part-way still leaves post able to clean up.
  append('GITHUB_STATE', 'name', name);
  append('GITHUB_STATE', 'services', services);
  append('GITHUB_STATE', 'mode', mode);
  append('GITHUB_STATE', 'port-base', portBase);

  // 1. The binary, in <prefix>/bin, which is where install.sh puts it.
  const prefix = path.join(process.env.RUNNER_TEMP || '/tmp', 'cloudburrow');
  const binDir = path.join(prefix, 'bin');
  const root = actionRoot();
  fs.mkdirSync(binDir, { recursive: true });
  if (version === 'source') {
    // The repository the action was checked out from: with `uses: ./` in
    // this repository, that is the code under test.
    // The storage servers the CLI embeds are built from this source first,
    // as `make build` and the release do: a plain `go build` would embed
    // whatever was committed, or nothing (#623).
    run('make', ['storage-binaries'], { cwd: root });
    // The BigQuery emulator it embeds (#1061), only when BigQuery is
    // asked for: it is two ~200 MB Go builds, minutes on a cold runner.
    if (services.split(',').map((s) => s.trim()).includes('bigquery')) {
      run('make', ['bigquery-binaries'], { cwd: root });
    }
    run('go', ['build', '-o', path.join(binDir, 'cloudburrow'), './cmd/cloudburrow'], { cwd: root });
  } else {
    // The installer verifies the SHA-256 against the release's checksums,
    // and its build attestation when gh can authenticate.
    const token = input('github-token', '');
    run('sh', installArgs(root, prefix, version, token), { env: { ...process.env, GH_TOKEN: token } });
  }
  const bin = path.join(binDir, 'cloudburrow');
  run(bin, ['version']);

  // The flags this CLI has (#679): `latest` is whatever was published last,
  // and a pinned `version` may be older than this action, so what is passed
  // follows the installed CLI rather than the action's ref. An input the CLI
  // cannot honour fails here, before anything is started, so post (which
  // runs only once `bin` is recorded) has nothing to clean up.
  const support = cliSupport(capture(bin, ['version', '--short']), { source: version === 'source' });
  if (support.warning) console.log(`::warning title=setup-cloudburrow::${support.warning}`);
  requireInputs(support, { portBase });
  if (!support.features.trust) {
    console.log(`CLI ${support.version} has no --trust and reads a discovered config without it; up runs without the flag`);
  }

  append('GITHUB_STATE', 'bin', bin);
  // GITHUB_PATH takes one bare directory per line.
  fs.appendFileSync(process.env.GITHUB_PATH, binDir + '\n');

  // 2. Prerequisites, named rather than discovered as a kind stack trace.
  const missing = ['docker', 'kind', 'kubectl'].filter((c) => !has(c));
  if (missing.length) {
    throw new Error(`missing on this runner: ${missing.join(', ')}. ` +
      'Docker is on the GitHub-hosted Ubuntu runners; install kind and kubectl first, for example with helm/kind-action (install_only: true).');
  }
  run('docker', ['info', '--format', '{{.ServerVersion}}']);

  // 3 and 4. Start in the background and wait for readiness.
  const flags = instanceFlags(name, services, mode, portBase);
  // --trust, when the CLI has it, is `up`'s alone; the shared flags stay as
  // they were (upFlags in lib.js says why).
  run(bin, ['up', ...upFlags(support, timeout), ...flags]);
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
