// setup-cloudburrow post step: diagnostics, then delete the cluster.
//
// Runs whatever happened to the job (post-if: always() in action.yml). It
// cannot tell whether the job's steps passed, so diagnostics are printed every
// time, bounded and collapsed, rather than only when they would have helped.
'use strict';

const { spawnSync } = require('child_process');
const { instanceFlags } = require('./lib');

function state(key) {
  return process.env[`STATE_${key}`] || '';
}

function show(title, cmd, args) {
  console.log(`::group::${title}`);
  const r = spawnSync(cmd, args, { encoding: 'utf8', timeout: 60000 });
  const out = `${r.stdout || ''}${r.stderr || ''}`;
  // Bounded: a multi-megabyte log in a collapsed group helps nobody.
  console.log(out.length > 200000 ? `…(${out.length - 200000} bytes cut)…\n${out.slice(-200000)}` : out);
  console.log('::endgroup::');
}

function main() {
  const bin = state('bin');
  const name = state('name');
  if (!bin || !name) {
    console.log('setup-cloudburrow did not get as far as installing; nothing to clean up');
    return;
  }
  const flags = instanceFlags(name, state('services'), state('mode') || 'ephemeral');

  show('cloudburrow status', bin, ['status', '--format', 'json', ...flags]);
  show('cloudburrow logs (last 200 lines per container)', bin, ['logs', '--tail', '200', ...flags]);

  console.log(`$ cloudburrow delete --name ${name}`);
  const r = spawnSync(bin, ['delete', ...flags], { stdio: 'inherit', timeout: 10 * 60000 });
  // Checked, not assumed: the self-test reads this line to prove a failed job
  // was still cleaned up.
  const clusters = spawnSync('kind', ['get', 'clusters'], { encoding: 'utf8' }).stdout || '';
  const gone = !clusters.split('\n').includes(`cloudburrow-${name}`);
  if (r.status === 0 && gone) {
    console.log(`setup-cloudburrow: cluster cloudburrow-${name} deleted`);
  } else {
    console.log(`::error title=setup-cloudburrow::cluster cloudburrow-${name} may still exist (delete exited ${r.status}); remaining: ${clusters.trim() || 'none'}`);
    process.exitCode = 1;
  }
}

main();
