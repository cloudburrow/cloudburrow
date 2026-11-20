// Firestore pipelines through the official @google-cloud/firestore client's
// pipeline API (#1951), on an Enterprise edition database: the lambdas
// array_filter and array_transform (with an index), each element bound to
// the lambda's variable, as Google's array function reference
// (firebase.google.com/docs/firestore/pipelines/functions/array-functions)
// gives them, and regex_find.

import assert from 'node:assert/strict';
import { test } from 'node:test';

import { Firestore } from '@google-cloud/firestore';
import {
  add, constant, field, greaterThan, multiply, notEqual, variable,
} from '@google-cloud/firestore/pipelines';

import { enterpriseDatabase } from './firestore-edition.mjs';
import { skipUnless, suffix } from './guard.mjs';

const skip = skipUnless('FIRESTORE_EMULATOR_HOST');

test('Firestore pipeline lambdas: array_filter and array_transform', { skip }, async () => {
  const project = process.env.GOOGLE_CLOUD_PROJECT;
  const id = `piped-node-${suffix()}`.toLowerCase().replace(/[^a-z0-9-]/g, '-').slice(0, 60);
  const drop = await enterpriseDatabase(project, id);
  const db = new Firestore({ projectId: project, databaseId: id });
  try {
    await db.collection('scores').doc('s1').set({
      scores: [1, null, 3], letters: ['a', 'b', 'c'], pair: [10, 20], email: 'john.doe@x.io',
    });
    const snap = await db.pipeline().collection('scores').select(
      field('scores').arrayFilter('x', greaterThan(variable('x'), 1)).as('filtered'),
      field('letters').arrayFilter('x', notEqual(variable('x'), 'b')).as('kept'),
      field('pair').arrayTransform('x', multiply(variable('x'), 2)).as('doubled'),
      field('pair').arrayTransformWithIndex('x', 'i', add(variable('x'), variable('i'))).as('plusIndex'),
      field('email').regexFind(constant('[a-z]+')).as('user'),
    ).execute();
    assert.equal(snap.results.length, 1);
    assert.deepEqual(snap.results[0].data(), {
      filtered: [3], kept: ['a', 'c'], doubled: [20, 40], plusIndex: [10, 21], user: 'john',
    });
  } finally {
    await db.terminate();
    await drop();
  }
});
