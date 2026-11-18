// The Write stream of the Firebase Web SDK over WebChannel (#1744). In a
// browser the SDK opens Firestore's Write stream over WebChannel, plain
// HTTP requests on /google.firestore.v1.Firestore/Write/channel, not gRPC.
// This loads the SDK's browser build (`firebase` 13.2.0's
// @firebase/firestore index.esm.js), which Node runs on its fetch (the
// useFetchStreams setting, the browser's Fetch API transport), and expects
// the refusals the gRPC stream gets (firestore-edition.test.mjs): on
// `(default)`, a Standard database, each Enterprise-only value is refused
// invalid-argument with the same message and is not stored; an Enterprise
// database stores each and reads it back.

import assert from 'node:assert/strict';
import { test } from 'node:test';

// The Web SDK's browser build, which writes over WebChannel.
import * as browserSDK from './node_modules/@firebase/firestore/dist/index.esm.js';

import { writeStream } from './firestore-edition.mjs';
import { skipUnless } from './guard.mjs';

const skip = skipUnless('FIRESTORE_EMULATOR_HOST');

test('Firestore Write stream over WebChannel through the Firebase SDK: Standard refuses Enterprise-only values, Enterprise stores them', { skip }, async () => {
  // Every write goes up a WebChannel session, not gRPC.
  const realFetch = globalThis.fetch;
  let channel = 0;
  globalThis.fetch = (input, init) => {
    const u = input instanceof Request ? input.url : String(input);
    if (u.includes('/google.firestore.v1.Firestore/Write/channel')) channel++;
    return realFetch(input, init);
  };
  try {
    await writeStream(browserSDK, { useFetchStreams: true });
  } finally {
    globalThis.fetch = realFetch;
  }
  assert.ok(channel > 0, 'the browser build did not write over WebChannel');
});
