// Official Node.js client compatibility suite for CloudBurrow (#589).
//
// Loaded with --import into every process of the run, before any test file:
// the node:test runner and each test file's child process. It mirrors
// test/compat-python/conftest.py.
//
// Configuration comes from the running instance itself: `cloudburrow env
// --format json` for every variable a developer would export, or
// CLOUDBURROW_ENV_JSON, a file in that format, for a server run without an
// instance. Two guards hold for the whole run:
//
//   - Credentials: GOOGLE_APPLICATION_CREDENTIALS must be the fixture the
//     instance generated, and application default credentials must resolve to
//     it. Otherwise a misconfigured endpoint could succeed against real Google.
//   - Loopback: every TCP connection and every name lookup must be loopback.
//     Unlike Python's, Node's gRPC (@grpc/grpc-js) is JavaScript and connects
//     through net like the HTTP clients do, so this one socket guard covers
//     Storage, Pub/Sub, Secret Manager, Cloud Tasks and Firestore alike.
//
// A violation fails the process, and so the run, with exit code 4 even if the
// test that caused it caught the error. A credentials refusal exits 3 before
// any test runs, and an unreadable environment exits 2.

import { execFileSync } from 'node:child_process';
import dns from 'node:dns';
import { readFileSync } from 'node:fs';
import net from 'node:net';

export const violations = [];

export class GuardError extends Error {
  constructor(what) {
    super(`cloudburrow compat guard: ${what}`);
    this.name = 'GuardError';
  }
}

export function isLoopback(host) {
  host = String(host ?? '').replace(/^\[|\]$/g, '');
  if (host === 'localhost') return true;
  const mapped = host.match(/^::ffff:(\d+\.\d+\.\d+\.\d+)$/i);
  if (mapped) host = mapped[1];
  switch (net.isIP(host)) {
    case 4:
      return host.split('.')[0] === '127';
    case 6:
      return /^(0{0,4}:){7}0{0,3}1$|^::1$/.test(host);
    default:
      return false;
  }
}

function refuse(what) {
  violations.push(what);
  throw new GuardError(what);
}

function stop(code, message) {
  process.stderr.write(`cloudburrow compat guard: ${message}\n`);
  process.exit(code);
}

// --- loopback --------------------------------------------------------------

// Socket.prototype.connect is what net.connect, tls.connect, http, http2 (and
// so @grpc/grpc-js) and fetch all end in. The host is checked before any
// lookup: resolving www.googleapis.com is already a request that left the
// machine. A Unix socket path is local by definition.
const realConnect = net.Socket.prototype.connect;
net.Socket.prototype.connect = function guardedConnect(...args) {
  let options = args[0];
  if (Array.isArray(options)) options = options[0];
  let host;
  if (options !== null && typeof options === 'object') {
    if (options.path === undefined) host = options.host ?? 'localhost';
  } else if (typeof options === 'number' || /^\d+$/.test(String(options))) {
    host = typeof args[1] === 'string' ? args[1] : 'localhost';
  }
  if (host !== undefined && !isLoopback(host)) {
    refuse(`connection to ${JSON.stringify(host)} is not loopback`);
  }
  return realConnect.apply(this, args);
};

function guardName(fnName, realFn) {
  return function guardedLookup(hostname, ...rest) {
    if (!isLoopback(hostname)) refuse(`name lookup (${fnName}) for ${JSON.stringify(hostname)} is not loopback`);
    return realFn.call(this, hostname, ...rest);
  };
}

const lookups = ['lookup', 'resolve', 'resolve4', 'resolve6', 'resolveAny', 'resolveCname', 'resolveSrv', 'resolveTxt'];
for (const name of lookups) {
  if (typeof dns[name] === 'function') dns[name] = guardName(`dns.${name}`, dns[name]);
  if (typeof dns.promises[name] === 'function') dns.promises[name] = guardName(`dns.promises.${name}`, dns.promises[name]);
  for (const Resolver of [dns.Resolver, dns.promises.Resolver]) {
    const real = Resolver.prototype[name];
    if (typeof real === 'function') Resolver.prototype[name] = guardName(`Resolver.${name}`, real);
  }
}

// process.exit inside an 'exit' listener exits at once with that code, so
// node:test's own listener cannot reset it to 0 afterwards.
process.on('exit', () => {
  if (violations.length > 0) {
    process.stderr.write(`\nloopback or credentials guard violations:\n  ${violations.join('\n  ')}\n`);
    process.exit(4);
  }
});

// --- the instance's environment ----------------------------------------------

function loadInstanceEnv() {
  if (process.env.CLOUDBURROW_ENV_JSON) {
    return JSON.parse(readFileSync(process.env.CLOUDBURROW_ENV_JSON, 'utf8'));
  }
  const bin = process.env.CLOUDBURROW_BIN || 'cloudburrow';
  const extra = (process.env.CLOUDBURROW_ARGS || '').split(/\s+/).filter(Boolean);
  return JSON.parse(execFileSync(bin, ['env', '--format', 'json', ...extra], { encoding: 'utf8' }));
}

let env;
try {
  env = loadInstanceEnv();
} catch (err) {
  stop(2, `cannot read the instance's environment from \`cloudburrow env\`: ${err.message}`);
}

const fixture = env.GOOGLE_APPLICATION_CREDENTIALS ?? '';
// Empty is unset: CI clears it deliberately. Anything else must be the
// fixture itself.
const given = process.env.GOOGLE_APPLICATION_CREDENTIALS ?? '';
if (given !== '' && given !== fixture) {
  stop(3, `GOOGLE_APPLICATION_CREDENTIALS is ${JSON.stringify(given)}, not the fixture ${JSON.stringify(fixture)}: ` +
    'refusing to run where real credentials could be used');
}
if (fixture === '') stop(3, '`cloudburrow env` exported no GOOGLE_APPLICATION_CREDENTIALS fixture');
for (const [key, value] of Object.entries(env)) process.env[key] = value;

// The default-credentials lookup must land on the fixture and nowhere else,
// such as a gcloud login in the home directory. Resolving it reads files
// only; the loopback guard is already in place should it try more.
{
  const { GoogleAuth } = await import('google-auth-library');
  let email;
  try {
    email = JSON.parse(readFileSync(fixture, 'utf8')).client_email;
  } catch (err) {
    stop(3, `cannot read the credentials fixture ${JSON.stringify(fixture)}: ${err.message}`);
  }
  const client = await new GoogleAuth().getClient();
  if (!email || client.email !== email) {
    stop(3, `application default credentials resolved to ${client.constructor.name} ${JSON.stringify(client.email)}, not the fixture ${JSON.stringify(email)}`);
  }
}

for (const name of ['PUBSUB_EMULATOR_HOST', 'FIRESTORE_EMULATOR_HOST', 'CLOUDBURROW_TASKS_ENDPOINT', 'CLOUDBURROW_SECRETMANAGER_ENDPOINT']) {
  if (process.env[name]) grpcTarget(process.env[name]);
}

// --- helpers for the tests ---------------------------------------------------

/** host:port for a gRPC client, refusing any non-loopback target. */
export function grpcTarget(addr) {
  const i = addr.lastIndexOf(':');
  const host = i < 0 ? addr : addr.slice(0, i);
  const port = i < 0 ? NaN : Number(addr.slice(i + 1));
  if (!isLoopback(host) || !Number.isInteger(port)) refuse(`gRPC target ${JSON.stringify(addr)} is not a loopback host:port`);
  return { apiEndpoint: host.replace(/^\[|\]$/g, ''), port };
}

/** The reason to skip a test whose service this instance does not export. */
export function skipUnless(name) {
  return process.env[name] ? false : `${name} is not set: this instance does not run the service`;
}

/** A per-test suffix, so reruns against one instance never collide. */
export function suffix() {
  return `${process.pid}-${Date.now() % 1e9}`;
}
