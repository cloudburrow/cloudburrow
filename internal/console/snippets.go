package console

// Client-library snippets for the Connect page (#802). Each is a block copied
// from the documentation it cites, character for character, and
// TestConnectSnippetsAreTheDocumentedOnes holds it there: a snippet that
// drifted from the page the suites run would be advice no test stands behind.

// Snippet is one client-library example.
type Snippet struct {
	// Language is the tab it is shown under.
	Language string `json:"language"`
	// Title names the client.
	Title string `json:"title"`
	// Needs are the variables it reads; it is shown only when `env` exports
	// every one of them.
	Needs []string `json:"needs"`
	// Code is the block, verbatim from Source.
	Code string `json:"code"`
	// Source is the document it comes from, relative to the repository.
	Source string `json:"source"`
	// Note is one sentence the block needs to be used correctly.
	Note string `json:"note,omitempty"`
}

// snippets are in the order the page shows them.
var snippets = []Snippet{
	{
		Language: "Go", Title: "Cloud Run v2",
		Needs:  []string{"CLOUDBURROW_RUN_ENDPOINT"},
		Source: "docs/credentials.md",
		Note:   "No client library reads an emulator variable for Cloud Run: the endpoint and a plaintext channel are given in code.",
		Code: `c, err := run.NewServicesClient(ctx,
	option.WithEndpoint(os.Getenv("CLOUDBURROW_RUN_ENDPOINT")),
	option.WithoutAuthentication(),
	option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))`,
	},
	{
		Language: "Go", Title: "BigQuery",
		Needs:  []string{"CLOUDBURROW_BIGQUERY_ENDPOINT", "GOOGLE_CLOUD_PROJECT"},
		Source: "docs/configuration.md",
		Note:   "The emulator serves the instance's own project only.",
		Code: `client, err := bigquery.NewClient(ctx, os.Getenv("GOOGLE_CLOUD_PROJECT"),
    option.WithEndpoint(os.Getenv("CLOUDBURROW_BIGQUERY_ENDPOINT")),
    option.WithoutAuthentication())`,
	},
	{
		Language: "Python", Title: "Cloud Storage",
		Needs:  []string{"STORAGE_EMULATOR_HOST"},
		Source: "docs/examples/python.md",
		Note:   "STORAGE_EMULATOR_HOST is all the client needs; Python needs the scheme in it, which env includes.",
		Code: `import uuid
from google.cloud import storage

client = storage.Client()
bucket = client.create_bucket(f"example-{uuid.uuid4().hex[:12]}")
bucket.blob("hello.txt").upload_from_string("hello")
assert bucket.blob("hello.txt").download_as_text() == "hello"
bucket.delete(force=True)`,
	},
	{
		Language: "Python", Title: "Cloud Tasks",
		Needs:  []string{"CLOUDBURROW_TASKS_ENDPOINT"},
		Source: "docs/credentials.md",
		Note:   "client_options alone is not enough: the channel it builds uses TLS, and the port is plaintext.",
		Code: `import os, grpc
from google.cloud import tasks_v2
from google.cloud.tasks_v2.services.cloud_tasks.transports import CloudTasksGrpcTransport

channel = grpc.insecure_channel(os.environ["CLOUDBURROW_TASKS_ENDPOINT"])
client = tasks_v2.CloudTasksClient(transport=CloudTasksGrpcTransport(channel=channel))`,
	},
	{
		Language: "Node.js", Title: "Cloud Storage",
		Needs:  []string{"STORAGE_EMULATOR_HOST", "GOOGLE_CLOUD_PROJECT"},
		Source: "docs/credentials.md",
		Note:   "@google-cloud/storage cannot use STORAGE_EMULATOR_HOST as exported: pass it as apiEndpoint and take it out of the environment.",
		Code: `const apiEndpoint = process.env.STORAGE_EMULATOR_HOST;
delete process.env.STORAGE_EMULATOR_HOST;
const storage = new Storage({ apiEndpoint, projectId: process.env.GOOGLE_CLOUD_PROJECT });`,
	},
	{
		Language: "Node.js", Title: "Cloud Tasks",
		Needs:  []string{"CLOUDBURROW_TASKS_ENDPOINT"},
		Source: "docs/credentials.md",
		Note:   "apiEndpoint alone is not enough: the client still builds a TLS channel, and the port is plaintext.",
		Code: "import { CloudTasksClient } from '@google-cloud/tasks';\n" +
			"import { grpc } from 'google-gax';\n\n" +
			"const endpoint = new URL(`http://${process.env.CLOUDBURROW_TASKS_ENDPOINT}`);\n" +
			"const client = new CloudTasksClient({\n" +
			"  apiEndpoint: endpoint.hostname,\n" +
			"  port: Number(endpoint.port),\n" +
			"  sslCreds: grpc.credentials.createInsecure(),\n" +
			"});",
	},
}

// SnippetsFor returns the snippets whose variables are all among exported,
// so the page offers no example for a service this instance does not serve.
func SnippetsFor(exported []EnvVar) []Snippet {
	have := map[string]bool{}
	for _, v := range exported {
		have[v.Name] = true
	}
	var out []Snippet
	for _, s := range snippets {
		ok := true
		for _, n := range s.Needs {
			ok = ok && have[n]
		}
		if ok {
			out = append(out, s)
		}
	}
	return out
}
