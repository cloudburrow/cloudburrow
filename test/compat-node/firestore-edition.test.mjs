// The Write stream of the Firebase client SDK (#1738). The Firebase Web SDK
// (`firebase`, its Node build) writes through Firestore's Write stream, not
// Commit. Google's "Supported data types"
// (https://docs.cloud.google.com/firestore/docs/concepts/data-types) marks
// the BSON types "Supported only in Enterprise edition", and says a
// Standard database's array cannot hold an array: on `(default)`, a
// Standard database, each write is refused invalid-argument, as Commit
// refuses it (TestFirestoreEnterpriseOnlyValues), while an Enterprise
// database stores each and reads it back through the same SDK.
// The cases are in firestore-edition.mjs.

import { test } from 'node:test';

import * as nodeSDK from 'firebase/firestore';

import { writeStream } from './firestore-edition.mjs';
import { skipUnless } from './guard.mjs';

const skip = skipUnless('FIRESTORE_EMULATOR_HOST');

test('Firebase SDK Write stream: Standard refuses Enterprise-only values, Enterprise stores them', { skip }, async () => {
  await writeStream(nodeSDK);
});
