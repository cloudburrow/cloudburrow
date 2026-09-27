// Run only by guard.test.mjs, as its own process: a connection to a
// non-loopback address, caught, must still fail the run.

import net from 'node:net';
import { test } from 'node:test';

import { GuardError } from '../guard.mjs';

test('a caught leak', () => {
  const socket = new net.Socket();
  try {
    socket.connect(443, '192.0.2.1'); // TEST-NET-1: never routed
  } catch (err) {
    if (!(err instanceof GuardError)) throw err;
  } finally {
    socket.destroy();
  }
});

test('a caught lookup', async () => {
  try {
    await fetch('http://www.googleapis.com/');
  } catch {
    // Swallowed, as a careless test might.
  }
});
