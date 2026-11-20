// The Node client of TestEnvResolvesNoGoogleName
// (test/compat/envdns_test.go): google-auth-library and gcp-metadata under
// `cloudburrow env`. Application Default Credentials (the fixture
// GOOGLE_APPLICATION_CREDENTIALS names, refreshed at its token_uri) and the
// metadata server (gcp-metadata, at GCE_METADATA_IP or GCE_METADATA_HOST).
// It prints "OK node <step>" or "FAIL node <step>: <error>", and runs only
// inside the no-egress container.
const {GoogleAuth, Compute} = require('google-auth-library');
const gcpMetadata = require('gcp-metadata');

// Never fall back to Google's defaults.
if (!process.env.GOOGLE_APPLICATION_CREDENTIALS || !process.env.GCE_METADATA_HOST) {
  console.log('FAIL node: GOOGLE_APPLICATION_CREDENTIALS and GCE_METADATA_HOST must be set');
  process.exit(1);
}
const scopes = ['https://www.googleapis.com/auth/cloud-platform'];

async function step(name, f) {
  try {
    await f();
    console.log('OK node', name);
  } catch (e) {
    console.log('FAIL node', name + ':', String(e).slice(0, 400));
  }
}

(async () => {
  if (process.argv[2] === 'token') {
    // An access token for the fixture: gtoken exchanges a signed JWT at
    // its hard-coded https://oauth2.googleapis.com/token, whatever the
    // file's token_uri says.
    await step('token', async () => {
      const client = await new GoogleAuth({scopes}).getClient();
      await client.getAccessToken();
    });
    return;
  }
  await step('adc', async () => {
    const auth = new GoogleAuth({scopes});
    const client = await auth.getClient();
    // As google-gax sets it for every served client: a self-signed JWT
    // with the scopes, no token exchange.
    client.useJWTAccessWithScope = true;
    const headers = await client.getRequestHeaders();
    if (!String(headers.get ? headers.get('authorization') : headers.Authorization).startsWith('Bearer ')) {
      throw new Error('no bearer token');
    }
    if (!(await auth.getProjectId())) throw new Error('no project');
  });
  await step('metadata', async () => {
    if (!(await gcpMetadata.isAvailable())) throw new Error('isAvailable is false');
    await gcpMetadata.project('project-id');
    await new Compute().getAccessToken();
  });
  await new Promise(r => setTimeout(r, 3000));
})();
