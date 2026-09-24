// Shared helpers for the setup-cloudburrow action.
//
// Deliberately dependency-free: the action runs `cloudburrow` and a few
// system tools, and nothing here needs @actions/core. The runner's file
// protocols (GITHUB_ENV, GITHUB_PATH, GITHUB_OUTPUT, GITHUB_STATE) are
// written directly, as the toolkit would write them.
'use strict';

const { spawnSync } = require('child_process');
const fs = require('fs');
const crypto = require('crypto');

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

// instanceFlags are passed to every cloudburrow command, so each one names the
// same instance.
function instanceFlags(name, services, mode) {
  const flags = ['--name', name, '--mode', mode];
  if (services) flags.push('--services', services);
  return flags;
}

function fail(err) {
  console.log(`::error title=setup-cloudburrow::${String(err.message || err).replace(/\r?\n/g, ' ')}`);
  process.exitCode = 1;
}

module.exports = { input, append, run, capture, has, instanceFlags, fail };
