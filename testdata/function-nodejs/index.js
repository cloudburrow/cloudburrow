// The Node.js fixture used to verify Google Functions Framework and
// Buildpacks support (#1303).
//
// It is an ordinary Functions Framework function: no CloudBurrow-specific
// code, because the point is that a normal function runs unchanged. It
// answers as testdata/function does.
const functions = require('@google-cloud/functions-framework');

// Hello answers an HTTP request, echoing a name when given one.
functions.http('Hello', (req, res) => {
  const name = (req.body && req.body.name) || 'world';
  res.json({greeting: 'hello ' + name});
});

// Event receives a CloudEvent in the Google schema and logs it.
functions.cloudEvent('Event', ce => {
  console.log(
    `received CloudEvent type=${ce.type} subject=${ce.subject} data=${JSON.stringify(ce.data)}`
  );
});
