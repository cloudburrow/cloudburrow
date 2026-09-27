// Every ```js block in docs/examples/node.md, run as written.
//
// A snippet that is documented and never run is a claim nothing checks, so the
// page is the test's input: a new block is run, a changed block is run, and a
// block that fails fails the suite. Each block is written to a module under
// node_modules/.cache, so its imports resolve to this suite's pinned clients,
// and imported in this process, under the same guards as every other test.
// A block skips, like the service tests, when the instance does not export
// the variable its section needs.

import assert from 'node:assert/strict';
import { mkdirSync, readFileSync, writeFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { test } from 'node:test';
import { fileURLToPath, pathToFileURL } from 'node:url';

import { skipUnless } from './guard.mjs';

const here = dirname(fileURLToPath(import.meta.url));
const page = join(here, '..', '..', 'docs', 'examples', 'node.md');
const out = join(here, 'node_modules', '.cache', 'cloudburrow-examples');

// The variable each section's block needs.
const needs = {
  'Cloud Storage': 'STORAGE_EMULATOR_HOST',
  'Pub/Sub': 'PUBSUB_EMULATOR_HOST',
  'Cloud Tasks': 'CLOUDBURROW_TASKS_ENDPOINT',
  'Secret Manager': 'CLOUDBURROW_SECRETMANAGER_ENDPOINT',
  Firestore: 'FIRESTORE_EMULATOR_HOST',
};

const blocks = [];
let section;
for (const part of readFileSync(page, 'utf8').split(/^```/m).entries()) {
  const [i, text] = part;
  if (i % 2 === 0) {
    const headings = [...text.matchAll(/^## (.+)$/gm)];
    if (headings.length > 0) section = headings.at(-1)[1].trim();
  } else if (text.startsWith('js\n')) {
    blocks.push({ section, code: text.slice('js\n'.length) });
  }
}

test('the Node examples page has an example for every section', () => {
  assert.deepEqual(blocks.map((b) => b.section), Object.keys(needs));
});

mkdirSync(out, { recursive: true });
for (const [i, { section: name, code }] of blocks.entries()) {
  const variable = needs[name];
  const skip = variable ? skipUnless(variable) : false;
  test(`example: ${name}`, { skip }, async () => {
    assert.ok(variable, `no variable is known for the section ${JSON.stringify(name)}`);
    const file = join(out, `block-${i + 1}.mjs`);
    writeFileSync(file, code);
    await import(pathToFileURL(file).href);
  });
}
