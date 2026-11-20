// The Node client of TestServedClientsSendNothingOffTheMachine
// (test/compat/egresscontainer_test.go): Cloud KMS over gRPC and REST,
// pointed at the instance, with the ADC fixture GOOGLE_APPLICATION_CREDENTIALS
// names as its only credential. It prints "OK node <transport>" or
// "FAIL node <transport>: <error>", and runs only inside the no-egress
// container.
const {KeyManagementServiceClient} = require('@google-cloud/kms');
const grpc = require('@grpc/grpc-js');

const addr = process.env.EGRESS_KMS || '';
const parent = process.env.EGRESS_PARENT || '';
// Never fall back to Google's endpoint.
if (!addr || !parent || !process.env.GOOGLE_APPLICATION_CREDENTIALS) {
  console.log('FAIL node: EGRESS_KMS, EGRESS_PARENT and GOOGLE_APPLICATION_CREDENTIALS must be set');
  process.exit(1);
}
const [host, port] = [addr.slice(0, addr.lastIndexOf(':')), Number(addr.slice(addr.lastIndexOf(':') + 1))];

async function run(transport, opts) {
  try {
    const client = new KeyManagementServiceClient(opts);
    await client.listKeyRings({parent});
    console.log('OK node', transport);
    await client.close();
  } catch (e) {
    console.log('FAIL node', transport + ':', String(e).slice(0, 400));
  }
}

(async () => {
  // gRPC over plaintext: gax still reads the fixture (the universe check).
  await run('grpc', {apiEndpoint: host, port, sslCreds: grpc.credentials.createInsecure()});
  // REST sends the fixture's token.
  await run('rest', {apiEndpoint: host, port, protocol: 'http', fallback: true});
  await new Promise(r => setTimeout(r, 3000));
})();
