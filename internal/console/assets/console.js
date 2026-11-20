/* CloudBurrow console.
 *
 * No framework and no build step: the assets ship as they are written, which
 * is what makes "no external CDN after installation" true by construction
 * rather than by a bundler configuration nobody checks.
 *
 * Every screen reads live state from /api. There is no fixture data here, on
 * purpose: a placeholder row is indistinguishable from a real one at a glance,
 * and a developer who trusts one is being misled by the tool.
 */
"use strict";

// Routes carry the section they belong to. A flat list of fourteen entries is
// a list to read; grouped, it is a place to look — which is the whole reason
// the console this mirrors groups its products rather than listing them.
// Routes carry the product being emulated, grouped the way the Google Cloud
// console groups its products.
//
// The point of this console is that an application cannot tell the difference,
// so the navigation names what is being emulated — Cloud Run, not "Services";
// Pub/Sub, not "Topics" — and groups it under the console's own headings.
// Calling Cloud Run "Serverless" described a category instead of naming the
// product a developer is actually pointing their SDK at.
//
// The page title is the product; what the table holds is the listing's noun.
// The Agent Platform pages (#1839), in the order of Google's left
// navigation. Google does not publish that order; it is reconstructed from
// the console pages its documentation links to (docs/console-parity.md §6).
const AP = "/agent-platform";
const AP_PATHS = {
  overview: `${AP}/overview`,
  modelGarden: `${AP}/model-garden`,
  localModels: `${AP}/model-garden/local-models`,
  agentGarden: `${AP}/agent-garden`,
  studio: `${AP}/studio`,
  embeddings: `${AP}/studio/embeddings`,
  predict: `${AP}/online-prediction/endpoints`,
  endpoints: `${AP}/endpoints`,
};
const AP_DOCS = "https://docs.cloud.google.com/gemini-enterprise-agent-platform/";

function agentPlatformRoutes() {
  const base = { section: "AI and machine learning", product: "vertexai", productTitle: "Agent Platform", icon: "ai" };
  const page = (path, title, group, more) => ({ ...base, path, title, group, ...more });
  // A page Google's console has and CloudBurrow does not serve.
  const absent = (path, title, group, api, about, docs, instead) =>
    page(path, title, group, { service: null, screen: "not-served", notServed: true, api, about, docs, instead });
  // The pages that run locally keep their place in the product menu while
  // their local backend is off (listedWhenOff), as Google's menu always has
  // them: each draws its off state, saying how to start it, and the
  // overview marks it "off".
  return [
    page(AP_PATHS.overview, "Overview", null, { service: null, screen: "ap-overview" }),
    page(AP_PATHS.modelGarden, "Model Garden", null, { service: null, screen: "model-garden" }),
    // Agent Builder.
    page(AP_PATHS.agentGarden, "Agent Garden", "Agent Builder", { service: null, screen: "agent-garden" }),
    absent(`${AP}/runtimes`, "Deployments", "Agent Builder", "projects.locations.reasoningEngines (Agent Runtime, formerly Agent Engine)",
      "Agents deployed to Agent Runtime, with their sessions, memories, traces and evaluation", AP_DOCS + "scale/runtime/manage-deployed-agents",
      "Run an Agent Development Kit agent on your machine against this instance's local generateContent endpoint instead."),
    absent(`${AP}/memory-bank`, "Memory Bank", "Agent Builder", "projects.locations.reasoningEngines.memories (v1beta1)",
      "Long-term memories generated from an agent's sessions", AP_DOCS + "scale"),
    absent(`${AP}/agent-registry/agents`, "Agent Registry", "Agent Builder", "the Agent Registry API",
      "A catalogue of the agents, endpoints and MCP servers in an organization", AP_DOCS + "agents"),
    absent(`${AP}/evaluation`, "Evaluation", "Agent Builder", "the Gen AI evaluation service (Agent Platform Evals)",
      "Evaluate models and agents against datasets and metrics", AP_DOCS + "optimize/evaluation/evaluate-agents"),
    absent(`${AP}/rag`, "RAG Engine", "Agent Builder", "projects.locations.ragCorpora",
      "Corpora of your documents for retrieval-augmented generation", AP_DOCS + "build"),
    // Agent Studio.
    page(AP_PATHS.studio, "Studio", "Agent Studio", { service: "playground", screen: "playground", listedWhenOff: true }),
    page(AP_PATHS.embeddings, "Embeddings", "Agent Studio", { service: "ai-embed", screen: "embeddings", listedWhenOff: true }),
    absent(`${AP}/studio/media`, "Media Studio", "Agent Studio", "Imagen, Veo and Lyria generation",
      "Generate images, video, speech and music (Agent Media Studio)", AP_DOCS + "agent-studio"),
    absent(`${AP}/studio/prompt-gallery`, "Prompt gallery", "Agent Studio", null,
      "Sample prompts to open in Studio", AP_DOCS + "agent-studio",
      "Studio sends your own prompts to the local model."),
    // Models.
    page(AP_PATHS.localModels, "Local models", "Models", { service: "ai-local", screen: "local-models", listedWhenOff: true }),
    absent(`${AP}/models`, "Model Registry", "Models", "projects.locations.models",
      "Models uploaded or trained in a project, with their versions", AP_DOCS + "models"),
    absent(`${AP}/tuning`, "Tuning", "Models", "projects.locations.tuningJobs",
      "Supervised and preference tuning jobs for Gemini and open models", AP_DOCS + "models"),
    page(AP_PATHS.predict, "Online prediction", "Models", { service: "ai-predict", screen: "predict", listedWhenOff: true }),
    page(AP_PATHS.endpoints, "Endpoints", "Models", { service: null, screen: "ap-endpoints" }),
    absent(`${AP}/batch-predictions`, "Batch inference", "Models", "projects.locations.batchPredictionJobs",
      "Batch prediction jobs over Cloud Storage or BigQuery input", AP_DOCS + "models"),
    // Notebooks.
    absent(`${AP}/colab/notebooks`, "Colab Enterprise", "Notebooks", "the Colab Enterprise API (notebookRuntimes)",
      "Managed notebooks and their runtimes", "https://docs.cloud.google.com/gemini-enterprise-agent-platform/notebooks/overview"),
    absent(`${AP}/workbench/instances`, "Workbench", "Notebooks", "notebooks.googleapis.com (Workbench instances)",
      "JupyterLab instances", "https://docs.cloud.google.com/gemini-enterprise-agent-platform/notebooks/overview"),
    // Model development.
    absent(`${AP}/datasets`, "Datasets", "Model development", "projects.locations.datasets",
      "Managed datasets for training", AP_DOCS + "machine-learning"),
    absent(`${AP}/feature-store`, "Feature Store", "Model development", "projects.locations.featureOnlineStores",
      "Feature groups and online stores", AP_DOCS + "machine-learning"),
    absent(`${AP}/training`, "Training", "Model development", "projects.locations.customJobs and trainingPipelines",
      "Managed Training jobs and pipelines", AP_DOCS + "machine-learning"),
    absent(`${AP}/pipelines`, "Pipelines", "Model development", "projects.locations.pipelineJobs",
      "Kubeflow and TFX pipeline runs, schedules and templates", AP_DOCS + "machine-learning"),
    absent(`${AP}/experiments`, "Experiments", "Model development", "projects.locations.metadataStores (experiments)",
      "Experiment runs and their parameters and metrics", AP_DOCS + "machine-learning"),
    absent(`${AP}/metadata`, "Metadata", "Model development", "projects.locations.metadataStores",
      "Artifacts, executions and contexts of ML workflows", AP_DOCS + "machine-learning"),
    absent(`${AP}/ray`, "Ray on Agent Platform", "Model development", "projects.locations.persistentResources (Ray clusters)",
      "Ray clusters for distributed training and serving", AP_DOCS + "machine-learning"),
    // Deploy and use.
    absent(`${AP}/matching-engine/indexes`, "Vector Search", "Deploy and use", "projects.locations.indexes and indexEndpoints",
      "Vector indexes and the endpoints that serve them", AP_DOCS + "build"),
    absent(`${AP}/model-monitoring/model-monitors`, "Monitoring", "Deploy and use", "projects.locations.modelMonitors",
      "Model monitors for drift and skew", AP_DOCS + "machine-learning"),
  ];
}

const AP_ROUTES = agentPlatformRoutes();

const ROUTES = [
  { path: "/", service: null, title: "Welcome" },

  // Cloud Run is one product with two pages, as Google's console has them:
  // Services and Jobs (#785). The product key stays "run", so a pin made
  // before the second page existed keeps pointing at it. A service called
  // "jobs" is shadowed by the Jobs page, as one called "create" is by the
  // deploy form.
  { path: "/run",      service: "run",      title: "Services", kind: "Service", logs: true, section: "Serverless computing",
    product: "run", productTitle: "Cloud Run" },
  { path: "/run/jobs", service: "run-jobs", title: "Jobs", kind: "Job", logs: true,     section: "Serverless computing",
    product: "run", productTitle: "Cloud Run" },
  { path: "/run/worker-pools", service: "run-worker-pools", title: "Worker pools", kind: "Worker pool", logs: true, section: "Serverless computing",
    product: "run", productTitle: "Cloud Run" },
  // Every domain mapping of the project, as Google's /run/domains (#1852).
  { path: "/run/domains", service: "run-domains", title: "Domain mappings", kind: "Domain mapping", section: "Serverless computing",
    product: "run", productTitle: "Cloud Run" },
  { path: "/run/instances", service: "run-instances", title: "Instances", kind: "Instance", section: "Serverless computing",
    product: "run", productTitle: "Cloud Run" },
  // Cloud Build is its own product (#1854), its menu in Google's order:
  // Dashboard, History, Triggers, Repositories, Worker pools, Settings.
  // History is the build history Cloud Run's menu held (#1347); its old
  // address, and a build's under it, lead here.
  { path: "/cloud-build/dashboard", service: "cloud-build-dashboard", title: "Dashboard", section: "CI/CD",
    product: "cloud-build", productTitle: "Cloud Build" },
  { path: "/cloud-build/builds", service: "run-builds", title: "History", kind: "Build", section: "CI/CD",
    product: "cloud-build", productTitle: "Cloud Build" },
  { path: "/cloud-build/triggers", service: "cloud-build-triggers", title: "Triggers", kind: "Trigger", section: "CI/CD",
    product: "cloud-build", productTitle: "Cloud Build" },
  { path: "/cloud-build/repositories", service: "cloud-build-repositories", title: "Repositories", section: "CI/CD",
    product: "cloud-build", productTitle: "Cloud Build" },
  { path: "/cloud-build/worker-pools", service: "cloud-build-worker-pools", title: "Worker pools", kind: "Worker pool", section: "CI/CD",
    product: "cloud-build", productTitle: "Cloud Build" },
  { path: "/cloud-build/settings", service: "cloud-build-settings", title: "Settings", section: "CI/CD",
    product: "cloud-build", productTitle: "Cloud Build" },
  { path: "/run/builds", service: "run-builds", title: "History", kind: "Build", section: "CI/CD", navHidden: true,
    redirect: "/cloud-build/builds", redirectTree: true, product: "cloud-build", productTitle: "Cloud Build" },
  // Secure Source Manager's instances and repositories (#1527).
  { path: "/secure-source-manager", service: "secure-source-manager", title: "Instances", kind: "Instance", section: "CI/CD",
    product: "secure-source-manager", productTitle: "Secure Source Manager" },
  { path: "/artifact-registry", service: "artifact-registry", title: "Repositories", kind: "Repository", section: "CI/CD",
    product: "artifact-registry", productTitle: "Artifact Registry" },
  // Eventarc's Triggers page (#1877), served on the Run port over Pub/Sub.
  { path: "/eventarc/triggers", service: "eventarc", title: "Triggers", kind: "Trigger", section: "Integration services",
    product: "eventarc", productTitle: "Eventarc" },
  // Artifact Registry's menu (#1855): Repositories, then Settings (the
  // project's settings and each location's VPC-SC policy and platform logs).
  // Not under /artifact-registry/, where it would shadow a repository named
  // "settings".
  { path: "/artifact-registry-settings", service: "artifact-registry-settings", icon: "artifact-registry", title: "Settings",
    section: "CI/CD", product: "artifact-registry", productTitle: "Artifact Registry" },
  // Cloud Functions v2, served on the Run port (#1377).
  { path: "/functions", service: "functions", title: "Functions", kind: "Function", logs: true, section: "Serverless computing",
    product: "functions", productTitle: "Cloud Functions" },

  // Kubernetes Engine is one product with five pages, not five products.
  //
  // The drawer lists products; a bare row called "Services" or "Jobs" sitting
  // as a peer of Cloud Run is ambiguous with Cloud Run's own services and
  // jobs, and someone who pinned "Jobs" could not tell from the drawer which
  // product they had pinned. `product` is the drawer's row, `title` is the
  // page inside it.
  { path: "/kubernetes/workloads",  service: "workloads",   title: "Workloads", kind: "Workload", logs: true, section: "Containers",
    product: "kubernetes", productTitle: "Kubernetes Engine" },
  { path: "/kubernetes/pods",       service: "pods",        title: "Pods", kind: "Pod", logs: true,      section: "Containers",
    product: "kubernetes", productTitle: "Kubernetes Engine" },
  { path: "/kubernetes/services",   service: "k8sservices", title: "Services", kind: "Service", logs: true,  section: "Containers",
    product: "kubernetes", productTitle: "Kubernetes Engine" },
  { path: "/kubernetes/jobs",       service: "jobs",        title: "Jobs", kind: "Job", logs: true,      section: "Containers",
    product: "kubernetes", productTitle: "Kubernetes Engine" },
  { path: "/kubernetes/nodes",      service: "nodes",       title: "Nodes", kind: "Node",     section: "Containers",
    product: "kubernetes", productTitle: "Kubernetes Engine" },
  { path: "/kubernetes/storage",    service: "k8sstorage",  title: "Storage", kind: "Persistent volume claim",   section: "Containers",
    product: "kubernetes", productTitle: "Kubernetes Engine" },
  { path: "/kubernetes/events",     service: "events",      title: "Events", kind: "Event",    section: "Containers",
    product: "kubernetes", productTitle: "Kubernetes Engine" },

  // Cloud Storage is one product. Its menu follows Google's (#1843):
  // Buckets, Observability, Storage Intelligence's Configuration, Inventory
  // reports, Transfer data and Settings. Observability, Inventory reports
  // and Transfer data are other services (Cloud Monitoring, Storage
  // Insights, Storage Transfer Service) CloudBurrow does not run, so their
  // pages say so in Google's place. The project's soft-deleted buckets
  // (#789) are on Buckets through View options, as Google shows them;
  // /storage/deleted, their page before, leads there.
  { path: "/storage/browser",  service: "storage",          title: "Buckets", kind: "Bucket",         section: "Storage",
    product: "storage", productTitle: "Cloud Storage" },
  { path: "/storage/observability", screen: "unavailable", icon: "storage", title: "Observability", section: "Storage",
    product: "storage", productTitle: "Cloud Storage",
    unavailable: "Cloud Storage's Observability dashboards read Cloud Monitoring metrics, which CloudBurrow does not " +
      "emulate. Each bucket's requests are in Logs Explorer and in the console's Requests page." },
  { path: "/storage/intelligence", service: "storage-intelligence", icon: "storage", title: "Configuration",
    navTitle: "Storage Intelligence", section: "Storage", product: "storage", productTitle: "Cloud Storage" },
  { path: "/storage/inventory-reports", screen: "unavailable", icon: "storage", title: "Inventory reports", section: "Storage",
    product: "storage", productTitle: "Cloud Storage",
    unavailable: "Inventory reports are written by Storage Insights, which CloudBurrow does not run. " +
      "A bucket's objects can be listed on its Objects tab or with objects.list." },
  { path: "/storage/transfer", screen: "unavailable", icon: "storage", title: "Transfer data", section: "Storage",
    product: "storage", productTitle: "Cloud Storage",
    unavailable: "Transfer data opens Storage Transfer Service, which CloudBurrow does not run. Objects can be " +
      "copied between buckets with an object's Copy, or uploaded with Upload files and Upload folder." },
  { path: "/storage/settings", service: "storage-settings", icon: "storage", title: "Settings",        section: "Storage",
    product: "storage", productTitle: "Cloud Storage" },
  { path: "/storage/deleted",  service: "storage-deleted", icon: "storage",  title: "Deleted buckets", kind: "Deleted bucket", section: "Storage",
    navTitle: "Soft-deleted buckets", navHidden: true, redirect: "/storage/browser?view=softdeleted",
    product: "storage", productTitle: "Cloud Storage" },

  // Firestore's menu is Google's (#1845): Databases first, then a
  // database's own pages. `tabOf` makes a menu entry a tab of the database
  // being looked at ((default) elsewhere), so Indexes, Import/Export,
  // Disaster recovery and Time-to-live open that database's tab; each
  // entry's own address leads to (default)'s. Security rules, Usage,
  // Insights and Key Visualizer are in Google's place and say why they are
  // not available. Databases is not under /firestore/, where it would
  // shadow a collection named "databases".
  { path: "/firestore-databases", service: "firestore-databases", icon: "firestore", title: "Databases", kind: "Database", section: "Databases",
    product: "firestore", productTitle: "Firestore" },
  // `crumb` names the breadcrumb's first link where the page's menu title
  // ("Data") would not say which product a document is in (#1842).
  { path: "/firestore", service: "firestore", title: "Data", crumb: "Firestore", section: "Databases",
    product: "firestore", productTitle: "Firestore" },
  ...[["query", "Query builder"], ["indexes", "Indexes"], ["importexport", "Import/Export"],
    ["schedules", "Disaster recovery"], ["ttl", "Time-to-live"]].map(([tab, title]) => ({
    path: "/firestore-" + tab, service: "firestore-databases", icon: "firestore", title, section: "Databases",
    product: "firestore", productTitle: "Firestore", tabOf: { route: "/firestore-databases", tab },
    redirect: "/firestore-databases/(default)?tab=" + tab })),
  { path: "/firestore-rules", screen: "unavailable", service: null, requires: "firestore-databases", icon: "firestore",
    title: "Security rules", section: "Databases", product: "firestore", productTitle: "Firestore",
    unavailable: "Firestore Security Rules are kept by the Firebase Rules API, which CloudBurrow does not serve. " +
      "Server client libraries, which reach the local Firestore, ignore rules, as they do on Google's." },
  { path: "/firestore-usage", screen: "unavailable", service: null, requires: "firestore-databases", icon: "firestore",
    title: "Usage", section: "Databases", product: "firestore", productTitle: "Firestore",
    unavailable: "Usage charts read Cloud Monitoring metrics, which CloudBurrow does not emulate. Each call to " +
      "Firestore is in the console's Requests page." },
  { path: "/firestore-insights", screen: "unavailable", service: null, requires: "firestore-databases", icon: "firestore",
    title: "Insights", section: "Databases", product: "firestore", productTitle: "Firestore",
    unavailable: "Query insights are gathered by Google's Firestore query statistics, which the local Firestore " +
      "does not record. Query Explain is on an Enterprise database's page." },
  { path: "/firestore-key-visualizer", screen: "unavailable", service: null, requires: "firestore-databases", icon: "firestore",
    title: "Key Visualizer", section: "Databases", product: "firestore", productTitle: "Firestore",
    unavailable: "Key Visualizer draws Google's access-pattern heatmaps from its storage servers; the local " +
      "Firestore keeps none." },
  { path: "/datastore", service: "datastore", title: "Datastore", section: "Databases" },
  // The project's Bigtable instances, with their clusters, app profiles and
  // IAM policy (#1358). First, as Google's Bigtable opens on its Instances list (#1858). Not under /bigtable/, where it would shadow a table.
  { path: "/bigtable-instances", service: "bigtable-instances", icon: "bigtable", title: "Instances", kind: "Instance", section: "Databases",
    product: "bigtable", productTitle: "Bigtable" },
  { path: "/bigtable",  service: "bigtable",  title: "Tables", kind: "Table",  section: "Databases",
    product: "bigtable", productTitle: "Bigtable" },
  { path: "/spanner",   service: "spanner",   title: "Spanner", kind: "Instance",   section: "Databases" },
  // Cloud SQL's menu is Google's (#1860): Instances, the product's landing
  // page, every engine's instances in one list, then Backups, the
  // project's backups. Each instance's page has Google's own menu
  // (Overview, Connections, Users, Databases, Backups, Replicas,
  // Operations, Logs); the PostgreSQL and MySQL database browsers are its
  // Cloud SQL Studio, opened from the instance `up` runs for each, and
  // keep their addresses. MySQL's page is not under /cloudsql/, where it
  // would shadow a PostgreSQL database named "mysql".
  { path: "/cloudsql-instances", service: "cloudsql-instances", title: "Instances", kind: "Instance", section: "Databases",
    product: "cloudsql", productTitle: "Cloud SQL", icon: "cloudsql", logs: true },
  { path: "/cloudsql-backups", service: "cloudsql-backups", title: "Backups", section: "Databases",
    product: "cloudsql", productTitle: "Cloud SQL", icon: "cloudsql" },
  { path: "/cloudsql",       service: "cloudsql",       title: "PostgreSQL", kind: "Database", section: "Databases",
    navTitle: "Cloud SQL Studio (PostgreSQL)", navHidden: true, product: "cloudsql", productTitle: "Cloud SQL" },
  { path: "/cloudsql-mysql", service: "cloudsql-mysql", title: "MySQL", kind: "Database",      section: "Databases",
    navTitle: "Cloud SQL Studio (MySQL)", navHidden: true, product: "cloudsql", productTitle: "Cloud SQL", icon: "cloudsql" },
  // Memorystore for Redis (#1302) and the Backup and DR Service (#1433) had
  // providers, and console API pages, but no route: /memorystore and
  // /backupdr were "No such screen", so nothing of either was reachable from
  // the console (found by TestMemorystoreRotateServerCAThroughThePage).
  { path: "/memorystore", service: "memorystore", title: "Redis", kind: "Instance", section: "Databases",
    product: "memorystore", productTitle: "Memorystore" },
  // Memorystore for Redis Cluster (#1519), a page of its own beside the
  // instances, as the Memorystore console's Redis Cluster tab is.
  { path: "/memorystore-clusters", service: "memorystore-clusters", icon: "memorystore", title: "Redis Cluster", kind: "Cluster", section: "Databases",
    product: "memorystore", productTitle: "Memorystore" },
  // Memorystore for Valkey (#1519), memorystore.googleapis.com's instances.
  { path: "/memorystore-valkey", service: "memorystore-valkey", icon: "memorystore", title: "Valkey", kind: "Instance", section: "Databases",
    product: "memorystore", productTitle: "Memorystore" },
  { path: "/backupdr", service: "backupdr", icon: "storage", title: "Backup and DR", kind: "Backup vault", section: "Storage" },
  // Google files BigQuery under Analytics, a category with no vendored icon
  // here, so it sits with the other data stores rather than under a heading
  // drawn without one (#698). It is one product with two pages: datasets,
  // and Job history (#993). The product key stays "bigquery", so a pin made
  // before the second page existed keeps pointing at it. Job history is not
  // under /bigquery/, where it would shadow a dataset named "jobs".
  // BigQuery's menu follows Google's (#1861): Studio first, then Search,
  // the datasets and Job history this console had, the pipelines and
  // sharing pages, Partner Center and Settings. A page whose service
  // CloudBurrow does not run is in Google's place and says so. `requires`
  // keeps a page with no service of its own out of the menus while
  // BigQuery is not served.
  { path: "/bigquery-studio", service: null, requires: "bigquery", screen: "bigquery-studio", icon: "bigquery",
    title: "Studio", section: "Databases", product: "bigquery", productTitle: "BigQuery" },
  { path: "/bigquery-search", service: null, requires: "bigquery", screen: "unavailable", icon: "bigquery", title: "Search",
    section: "Databases", product: "bigquery", productTitle: "BigQuery",
    unavailable: "Search opens Dataplex Universal Catalog's search across Google Cloud, which CloudBurrow does not run. The Explorer pane in Studio searches this project's datasets, tables, views, routines and models, and the toolbar's search finds every served resource." },
  { path: "/bigquery",      service: "bigquery",      title: "Datasets", kind: "Dataset",    section: "Databases",
    product: "bigquery", productTitle: "BigQuery" },
  { path: "/bigquery-jobs", service: "bigquery-jobs", icon: "bigquery", title: "Job history", kind: "Job", section: "Databases",
    product: "bigquery", productTitle: "BigQuery", icon: "bigquery" },
  { path: "/bigquery-transfers", service: null, requires: "bigquery", screen: "unavailable", icon: "bigquery", title: "Data transfers",
    section: "Databases", product: "bigquery", productTitle: "BigQuery",
    unavailable: "Data transfers are the BigQuery Data Transfer Service's, which CloudBurrow does not run. A dataset's or table's Load from Cloud Storage and Load from a file load data, and Export to Cloud Storage exports it." },
  { path: "/bigquery-scheduled-queries", service: null, requires: "bigquery", screen: "unavailable", icon: "bigquery", title: "Scheduled queries",
    section: "Databases", product: "bigquery", productTitle: "BigQuery",
    unavailable: "A scheduled query is a BigQuery Data Transfer Service transfer, which CloudBurrow does not run. Run a query in Studio, or from a client on a schedule of its own." },
  { path: "/bigquery-sharing", service: null, requires: "bigquery", screen: "unavailable", icon: "bigquery", title: "Analytics Hub",
    section: "Databases", product: "bigquery", productTitle: "BigQuery",
    unavailable: "Analytics Hub (Sharing) exchanges and listings are the Analytics Hub API's, which CloudBurrow does not run. A dataset is shared through its Permissions tab." },
  { path: "/bigquery-dataform", service: null, requires: "bigquery", screen: "unavailable", icon: "bigquery", title: "Dataform",
    section: "Databases", product: "bigquery", productTitle: "BigQuery",
    unavailable: "Dataform repositories and workflows are the Dataform API's, which CloudBurrow does not run." },
  { path: "/bigquery-partner-center", service: null, requires: "bigquery", screen: "unavailable", icon: "bigquery", title: "Partner Center",
    section: "Databases", product: "bigquery", productTitle: "BigQuery",
    unavailable: "Partner Center lists Google Cloud partners' solutions, which a local instance does not hold." },
  { path: "/bigquery-settings", service: null, requires: "bigquery", screen: "unavailable", icon: "bigquery", title: "Settings",
    section: "Databases", product: "bigquery", productTitle: "BigQuery",
    unavailable: "BigQuery's Settings (the project's default location and other configuration settings, set with ALTER PROJECT) are not served locally. A query's location and dialect are set in Studio, under Edit, Query settings." },

  // Pub/Sub is one product with three pages. A subscription is reached from its
  // topic too, but one whose topic was deleted, or one that was detached, is
  // reachable only here (#595).
  { path: "/pubsub/topics",        service: "pubsub",               title: "Topics", kind: "Topic",
    section: "Integration services", product: "pubsub", productTitle: "Pub/Sub" },
  { path: "/pubsub/subscriptions", service: "pubsub-subscriptions", title: "Subscriptions", kind: "Subscription",
    section: "Integration services", product: "pubsub", productTitle: "Pub/Sub" },
  // Snapshots are created from a subscription's page and listed here (#787).
  { path: "/pubsub/snapshots",     service: "pubsub-snapshots", icon: "pubsub",     title: "Snapshots", kind: "Snapshot",
    section: "Integration services", product: "pubsub", productTitle: "Pub/Sub" },
  // A schema's revisions, and a test of a message against it, are on its
  // page (#788).
  { path: "/pubsub/schemas",       service: "pubsub-schemas", icon: "pubsub",       title: "Schemas", kind: "Schema",
    section: "Integration services", product: "pubsub", productTitle: "Pub/Sub" },
  { path: "/tasks/queues",  service: "tasks",  title: "Cloud Tasks", navTitle: "Queues", kind: "Queue",
    section: "Integration services" },
  { path: "/scheduler/jobs", service: "scheduler", title: "Cloud Scheduler", navTitle: "Jobs", kind: "Job",
    section: "Integration services" },

  // Agent Platform (formerly Vertex AI, #1839). Google renamed Vertex AI to
  // Gemini Enterprise Agent Platform in April 2026 and moved its console
  // from /vertex-ai/ to /agent-platform/; the addresses CloudBurrow served
  // before (/ai/...) and Google's old /vertex-ai/ prefix redirect here
  // (RenamedAIPath). The product key stays "vertexai", so a pin made before
  // the rename keeps pointing at it. Pages are grouped as Google's console
  // groups them; a page CloudBurrow has no backend for is drawn by
  // renderNotServed, which says so, rather than as an empty list.
  ...AP_ROUTES.slice(0, AP_ROUTES.findIndex((r) => r.path === AP_PATHS.predict)),
  // CloudBurrow's own: the local catalogue, each model's provenance, licence,
  // access and runtime, a row opening its page (#41).
  { path: "/agent-platform/local-catalogue", service: "ai", title: "Local catalogue", kind: "Model", icon: "ai", group: "Models",
    section: "AI and machine learning", product: "vertexai", productTitle: "Agent Platform" },
  ...AP_ROUTES.slice(AP_ROUTES.findIndex((r) => r.path === AP_PATHS.predict)),

  // Secret Manager (#1850): Google's menu, Secret Manager (the global
  // secrets), Regional secrets and Settings.
  { path: "/secrets", service: "secrets", title: "Secret Manager", navTitle: "Secret Manager", kind: "Secret",
    section: "Security and identity", product: "secrets", productTitle: "Secret Manager" },
  { path: "/secrets-regional", service: "secrets-regional", icon: "secrets", title: "Regional secrets", kind: "Secret",
    section: "Security and identity", product: "secrets", productTitle: "Secret Manager" },
  { path: "/secrets-settings", service: null, requires: "secrets", screen: "unavailable", icon: "secrets", title: "Settings",
    section: "Security and identity", product: "secrets", productTitle: "Secret Manager",
    unavailable: "Google's Secret Manager settings are the project's own Google Cloud configuration, which Google's documentation does not describe and a local instance does not hold. A secret's replication, encryption, rotation, expiration and destruction delay are set on the secret itself, with Create secret and Edit secret." },
  // Key Management (#1851), in Google's menu order: Key management (the key
  // rings), Autokey, EKM connections, KMS config, Key inventory; then
  // CloudBurrow's Single-tenant HSM, whose instances Google manages with
  // gcloud only.
  { path: "/kms/keyrings", service: "kms", title: "Key management", navTitle: "Key management", kind: "Key ring",
    section: "Security and identity", product: "kms", productTitle: "Key Management" },
  // Cloud KMS EKM connections and Autokey (#1452): stored connections, and
  // the Autokey configurations a project resolves through, with its key handles.
  { path: "/kms/autokey", service: "kms-autokey", icon: "kms", title: "Autokey", kind: "Key handle", section: "Security and identity",
    product: "kms", productTitle: "Key Management" },
  { path: "/kms/ekm", service: "kms-ekm", icon: "kms", title: "EKM connections", kind: "EKM connection", section: "Security and identity",
    product: "kms", productTitle: "Key Management" },
  { path: "/kms/config", title: "KMS config", service: null, requires: "kms", screen: "unavailable", icon: "kms",
    section: "Security and identity", product: "kms", productTitle: "Key Management",
    unavailable: "KMS config holds a project's and an organization's Cloud KMS settings, such as the default Key Access Justifications policy for new keys, which Google serves through an API CloudBurrow does not run. Autokey's configuration is on the Autokey page, the default EKM connection on EKM connections, and a key's own settings on its page." },
  { path: "/kms/inventory", service: "kms-inventory", icon: "kms", title: "Key inventory", section: "Security and identity",
    product: "kms", productTitle: "Key Management" },
  { path: "/kms/hsm", service: "kms-hsm", icon: "kms", title: "Single-tenant HSM", kind: "Single-tenant HSM instance", section: "Security and identity",
    product: "kms", productTitle: "Key Management" },
  // Certificate Authority Service (#1834): CA pools, their CAs and the
  // certificates they issue, at Google's own path.
  { path: "/security/cas", service: "privateca", title: "CA pools", section: "Security and identity",
    product: "privateca", productTitle: "Certificate Authority Service" },
  { path: "/security/cas/templates", service: "privateca-templates", icon: "privateca", title: "Certificate templates",
    section: "Security and identity", product: "privateca", productTitle: "Certificate Authority Service" },
  // IAM & Admin (#1864): Google's menu, in Google's order. A page whose API
  // CloudBurrow does not serve is in Google's place and says so; Labels,
  // Tags, Organization Policies, Settings and Manage resources are the local
  // Resource Manager's, IAM, Service Accounts, Roles, Audit Logs and Groups
  // the local IAM's.
  // IAM (#1368, #1864): the selected project's principals, Google's IAM
  // page (renderIAMPage); /iam-admin/iam/<project> keeps the policy's
  // Permissions tab, with Test permissions and the condition linter.
  { path: "/iam-admin/iam", service: "iam", screen: "iam-page", title: "IAM", kind: "Principal", section: "IAM & Admin",
    product: "iam", productTitle: "IAM & Admin" },
  { path: "/iam-admin/cloudidentity", title: "Identity & Organization", service: null, requires: "iam", screen: "unavailable", icon: "iam", section: "IAM & Admin", product: "iam", productTitle: "IAM & Admin",
    unavailable: "Identity & Organization is Cloud Identity's sign-up and organization setup checklist, which a local instance has no counterpart for: its organization (organizations/100000000001) exists from the start. Groups are on the Groups page." },
  // Policy Troubleshooter and Policy Analyzer (#1879): iam:troubleshoot and
  // analyzeIamPolicy, over the evaluator `up --iam-enforcement=on` enforces.
  { path: "/iam-admin/troubleshooter", title: "Policy Troubleshooter", service: null, requires: "iam", screen: "iam-troubleshooter", icon: "iam", section: "IAM & Admin", product: "iam", productTitle: "IAM & Admin" },
  { path: "/iam-admin/analyzer", title: "Policy Analyzer", service: null, requires: "iam", screen: "iam-analyzer", icon: "iam", section: "IAM & Admin", product: "iam", productTitle: "IAM & Admin" },
  // Organization Policies (#1864): the selected project's constraints, the
  // policy set on it and the effective one, over Resource Manager's v1
  // org policy methods.
  { path: "/iam-admin/orgpolicies", service: "iam-orgpolicies", icon: "iam", title: "Organization Policies", kind: "Constraint", section: "IAM & Admin",
    product: "iam", productTitle: "IAM & Admin" },
  // IAM & Admin > Service accounts (#1747): the project's accounts, their
  // keys and Permissions tab, Create, Edit, Disable, Enable, Delete,
  // Undelete, Create key and Upload key.
  { path: "/iam-admin/serviceaccounts", service: "iam-service-accounts", icon: "iam", title: "Service Accounts", kind: "Service account", section: "IAM & Admin", logs: true,
    product: "iam", productTitle: "IAM & Admin" },
  // IAM & Admin > Workload Identity Federation (#1941): the project's
  // workload identity pools, a pool's providers, Create pool with a first
  // provider, Add provider, Edit, Disable, Enable, Delete and Restore.
  { path: "/iam-admin/workload-identity-pools", service: "iam-workload-identity", icon: "iam", title: "Workload Identity Federation", kind: "Workload identity pool", section: "IAM & Admin",
    product: "iam", productTitle: "IAM & Admin" },
  { path: "/iam-admin/workforce-identity-pools", title: "Workforce Identity Federation", service: null, requires: "iam", screen: "unavailable", icon: "iam", section: "IAM & Admin", product: "iam", productTitle: "IAM & Admin",
    unavailable: "Workforce identity pools are the IAM API's locations.workforcePools, which CloudBurrow does not serve." },
  // Labels (#1864): the selected project's labels, Resource Manager's.
  { path: "/iam-admin/labels", service: "iam-labels", icon: "iam", title: "Labels", kind: "Label", section: "IAM & Admin",
    product: "iam", productTitle: "IAM & Admin" },
  // Settings (#1864): the selected project's name, ID and number.
  { path: "/iam-admin/settings", service: "iam-settings", icon: "iam", title: "Settings", kind: "Project", section: "IAM & Admin",
    product: "iam", productTitle: "IAM & Admin" },
  { path: "/iam-admin/privacy", title: "Privacy & Security", service: null, requires: "iam", screen: "unavailable", icon: "iam", section: "IAM & Admin", product: "iam", productTitle: "IAM & Admin",
    unavailable: "Privacy & Security (Access Transparency, Access Approval and the data processing terms) concerns Google's own access to a project's data, which a local instance has none of." },
  { path: "/iam-admin/iap", title: "Identity-Aware Proxy", service: null, requires: "iam", screen: "unavailable", icon: "iam", section: "IAM & Admin", product: "iam", productTitle: "IAM & Admin",
    unavailable: "Identity-Aware Proxy is the IAP API's, which CloudBurrow does not serve. A Cloud Run service is reached on its local URL directly." },
  // IAM & Admin > Roles (#1743): the project's and the organization's
  // custom roles, with Create role, Edit role, Delete and Undelete.
  { path: "/iam-admin/roles", service: "iam-roles", icon: "iam", title: "Roles", kind: "Role", section: "IAM & Admin",
    product: "iam", productTitle: "IAM & Admin" },
  // IAM & Admin > Audit Logs (#1756): the services that can write Data
  // Access audit logs (queryAuditableServices) and the project policy's
  // log types for each.
  { path: "/iam-admin/audit", service: "iam-audit", icon: "iam", title: "Audit Logs", kind: "Service", section: "IAM & Admin",
    product: "iam", productTitle: "IAM & Admin" },
  { path: "/iam-admin/groups", service: "iam-groups", icon: "iam", title: "Groups", kind: "Group", section: "IAM & Admin",
    product: "iam", productTitle: "IAM & Admin" },
  { path: "/iam-admin/essential-contacts", title: "Essential Contacts", service: null, requires: "iam", screen: "unavailable", icon: "iam", section: "IAM & Admin", product: "iam", productTitle: "IAM & Admin",
    unavailable: "Essential Contacts is the Essential Contacts API's, which CloudBurrow does not serve: a local instance sends no notifications." },
  { path: "/iam-admin/asset-inventory", title: "Asset Inventory", service: null, requires: "iam", screen: "unavailable", icon: "iam", section: "IAM & Admin", product: "iam", productTitle: "IAM & Admin",
    unavailable: "Asset Inventory is the Cloud Asset API's, which CloudBurrow does not serve. The toolbar's search finds every served resource, and each product's list holds its own." },
  { path: "/iam-admin/quotas", title: "Quotas & System Limits", service: null, requires: "iam", screen: "unavailable", icon: "iam", section: "IAM & Admin", product: "iam", productTitle: "IAM & Admin",
    unavailable: "Quotas are the Cloud Quotas API's, which CloudBurrow does not serve. The limits Google documents per API are enforced by the services themselves." },
  { path: "/iam-admin/pam", title: "Privileged Access Manager", service: null, requires: "iam", screen: "unavailable", icon: "iam", section: "IAM & Admin", product: "iam", productTitle: "IAM & Admin",
    unavailable: "Privileged Access Manager (entitlements and grants) is the Privileged Access Manager API's, which CloudBurrow does not serve. Grant a role on the IAM page; an IAM condition on request.time makes it temporary." },
  { path: "/iam-admin/principal-access-boundary", title: "Principal Access Boundary", service: null, requires: "iam", screen: "unavailable", icon: "iam", section: "IAM & Admin", product: "iam", productTitle: "IAM & Admin",
    unavailable: "Principal access boundary policies are the IAM v3 API's, which CloudBurrow does not serve." },

  { path: "/monitoring", service: null, screen: "monitoring", title: "Monitoring", section: "Operations" },
  // Cloud Logging's menu in Google's order (#1863): Logs Explorer, Log
  // Analytics, Logs Dashboard, Log-based Metrics, Log Router, Logs Storage,
  // then the pages Google keeps elsewhere that CloudBurrow serves here
  // (saved queries are the Explorer's Query library, log scopes and
  // Settings Google's Observability settings, exclusions part of a sink).
  { path: "/logs",     service: null, screen: "logs",     title: "Logs Explorer", section: "Operations",
    product: "logging", productTitle: "Cloud Logging" },
  // Log Analytics (#1863): SQL over the log views of buckets upgraded to
  // use it, run in the instance's BigQuery over each bucket's linked
  // dataset; without BigQuery served the page says why it is unavailable,
  // so the entry has no service (which would hide it) but names its
  // provider.
  { path: "/logs/analytics", service: null, provider: "logging-analytics", screen: "log-analytics", icon: "logs", title: "Log Analytics", section: "Operations",
    product: "logging", productTitle: "Cloud Logging",
    unavailable: "Log Analytics runs SQL over a bucket's linked BigQuery dataset, in this instance's BigQuery, " +
      "which this instance does not serve: start it with bigquery among its --services." },
  { path: "/logs/dashboard", service: null, screen: "logs-dashboard", icon: "logs", title: "Logs Dashboard", section: "Operations",
    product: "logging", productTitle: "Cloud Logging" },
  // Cloud Logging's admin resources (#1226), beside the Explorer: they are
  // stored and validated; a metric has no time series and a sink delivers
  // nothing, which each screen says.
  { path: "/logs/metrics",    service: "logging-metrics",    icon: "logs", title: "Log-based Metrics", kind: "Log-based metric",
    section: "Operations", product: "logging", productTitle: "Cloud Logging" },
  { path: "/logs/sinks",      service: "logging-sinks",      icon: "logs", title: "Log Router", kind: "Sink",
    section: "Operations", product: "logging", productTitle: "Cloud Logging" },
  { path: "/logs/storage",    service: "logging-buckets",    icon: "logs", title: "Logs Storage", kind: "Log bucket",
    section: "Operations", product: "logging", productTitle: "Cloud Logging" },
  // Saved queries and log scopes (#1519).
  { path: "/logs/saved-queries", service: "logging-saved-queries", icon: "logs", title: "Saved queries", kind: "Saved query",
    section: "Operations", product: "logging", productTitle: "Cloud Logging" },
  { path: "/logs/log-scopes", service: "logging-log-scopes", icon: "logs", title: "Log scopes", kind: "Log scope",
    section: "Operations", product: "logging", productTitle: "Cloud Logging" },
  { path: "/logs/exclusions", service: "logging-exclusions", icon: "logs", title: "Exclusions", kind: "Exclusion",
    section: "Operations", product: "logging", productTitle: "Cloud Logging" },
  // Logging's Settings (#1311): GetSettings, UpdateSettings, GetCmekSettings.
  { path: "/logs/settings",   service: "logging-settings",   icon: "logs", title: "Settings",
    section: "Operations", product: "logging", productTitle: "Cloud Logging" },
  { path: "/activity", service: null, screen: "activity", title: "Activity",      section: "Operations" },
  { path: "/requests", service: null, screen: "requests", title: "Request Log",   section: "Operations" },
  { path: "/faults",   service: null, screen: "faults",   title: "Fault injection", section: "Operations" },

  // Resource Manager is one product with three pages here: projects, the
  // organization and its folders (#1294), and tags (#1295). The product key
  // stays "projects", so a pin made before the other pages were routed keeps
  // pointing at it. Resource hierarchy and Tags had providers but no route,
  // so neither was reachable from the console.
  { path: "/projects", service: "projects", title: "Projects", kind: "Project", section: "Management tools",
    product: "projects", productTitle: "Resource Manager" },
  { path: "/resource-hierarchy", service: "hierarchy", icon: "hierarchy", title: "Resource hierarchy", kind: "Folder", section: "Management tools",
    product: "projects", productTitle: "Resource Manager" },
  { path: "/tags", service: "tags", title: "Tags", kind: "Tag key", section: "Management tools",
    product: "projects", productTitle: "Resource Manager" },
  // APIs & Services (#1352): a project's APIs, Enable and Disable. It had a
  // provider but no route.
  { path: "/apis", service: "serviceusage", title: "Enabled APIs & services", kind: "API/Service", section: "Management tools",
    product: "serviceusage", productTitle: "APIs & Services" },
  // Service Usage operations (#1530): what operations.list answers, with
  // Delete.
  { path: "/apis/operations", service: "serviceusage-operations", icon: "serviceusage", title: "Operations", kind: "Operation", section: "Management tools",
    product: "serviceusage", productTitle: "APIs & Services" },
  // Connect (#802): what `env`, `gcloud-setup`, `terraform`, `version` and
  // `diagnose` give at a terminal. Not a Google Cloud product, so it carries
  // no product icon.
  { path: "/connect", service: null, screen: "connect", icon: "connect", title: "Connect", section: "Management tools" },

  // The New project page (#1842), at the address Google's has.
  { path: "/projectcreate", service: null, screen: "new-project", title: "New project" },
  { path: "/search", service: null, screen: "search", title: "Search results" },
  { path: "/products", service: null, screen: "products", title: "All products" },
  // The Instance page (#801): state save and load, reset and seed, as the
  // cloudburrow commands of those names. Also reached from Settings and
  // utilities.
  { path: "/instance", service: null, screen: "instance", title: "Instance", section: "Management tools" },
  // Pages of another product's service that Google's IAM & Admin menu
  // also lists (#1864). They come after the product's own routes, so a
  // lookup by service still finds Resource Manager's page, and `navAfter`
  // places each in the IAM & Admin menu after the page it names.
  // Tags (#1295) is Resource Manager's tag keys page; Google files it under
  // IAM & Admin, so it is in this menu too.
  { path: "/iam-admin/tags", service: "tags", icon: "iam", title: "Tags", kind: "Tag key", section: "IAM & Admin",
    product: "iam", productTitle: "IAM & Admin", navAfter: "/iam-admin/labels" },
  // Manage resources (#1864): Resource Manager's projects, the page Google's
  // IAM & Admin menu opens at /cloud-resource-manager.
  { path: "/cloud-resource-manager", service: "projects", icon: "iam", title: "Manage resources", kind: "Project", section: "IAM & Admin",
    product: "iam", productTitle: "IAM & Admin", navAfter: "/iam-admin/audit" },
];

// Every product listing gets a matching create address.
//
// A dialog has no URL, so a form opened in one is lost to a refresh, a back
// button or a link sent to a colleague. Whether a given product's form opens
// here or in a dialog is the backend's call — capabilities carry `page` — but
// the address exists either way, so the decision can change without breaking
// a link. Generated rather than written out, because a create path that had
// to be remembered would be forgotten by the next product added.
//
// They carry no `section`, which is what keeps them out of the navigation.
for (const listing of [...ROUTES]) {
  if (!listing.service || listing.screen) continue;
  ROUTES.push({
    path: `${listing.path}/create`, service: listing.service, screen: "create",
    title: `Create in ${listing.title}`, of: listing,
  });
}

// A route's product identity. A single-page product is its own product, so
// every caller can treat the two the same.
const productKey = (entry) => entry.product || entry.service || entry.path;
const productTitle = (entry) => entry.productTitle || entry.title;

// pagesOf returns the pages a product owns, in declaration order.
// A route with `navAfter` (#1864) is listed after the page it names.
const pagesOf = (key) => {
  const pages = ROUTES.filter((r) => r.section && productKey(r) === key && !r.navAfter);
  for (const r of ROUTES) {
    if (!r.navAfter || !r.section || productKey(r) !== key) continue;
    const i = pages.findIndex((p) => p.path === r.navAfter);
    pages.splice(i < 0 ? pages.length : i + 1, 0, r);
  }
  return pages;
};

// Product marks.
//
// Every mark the console draws is CloudBurrow's own line drawing, authored
// here. No Google product or category icon is shipped: the terms for that
// artwork were never established, so it was removed (#684) — see
// assets/icons/PROVENANCE.md. A screen with no drawing of its own gets the
// dashboard mark rather than a broken image.
const ICONS = {
  // Cloud Run: a container with a run triangle.
  run:       '<rect x="3" y="5" width="18" height="14" rx="3"/><path d="M10 9.5l5 2.5-5 2.5z"/>',
  // Cloud Run jobs: the same mark, since it is the same product.
  "run-jobs": '<rect x="3" y="5" width="18" height="14" rx="3"/><path d="M10 9.5l5 2.5-5 2.5z"/>',
  "run-instances": '<rect x="3" y="5" width="18" height="14" rx="3"/><path d="M10 9.5l5 2.5-5 2.5z"/>',
  "run-worker-pools": '<rect x="3" y="5" width="18" height="14" rx="3"/><path d="M10 9.5l5 2.5-5 2.5z"/>',
  "run-domains": '<rect x="3" y="5" width="18" height="14" rx="3"/><path d="M10 9.5l5 2.5-5 2.5z"/>',
  "run-builds": '<rect x="3" y="5" width="18" height="14" rx="3"/><path d="M10 9.5l5 2.5-5 2.5z"/>',
  // Eventarc: an event routed from a source to a destination.
  eventarc: '<circle cx="5" cy="12" r="2.5"/><circle cx="19" cy="6" r="2.5"/><circle cx="19" cy="18" r="2.5"/><path d="M7.5 12h4l5-5M11.5 12l5 5"/>',
  "cloud-build-triggers": '<path d="M13 3L5 13h6l-1 8 8-10h-6z"/>',
  "cloud-build-dashboard": '<rect x="3" y="3" width="8" height="8" rx="1"/><rect x="13" y="3" width="8" height="5" rx="1"/><rect x="13" y="10" width="8" height="11" rx="1"/><rect x="3" y="13" width="8" height="8" rx="1"/>',
  "cloud-build-repositories": '<circle cx="6" cy="5" r="2"/><circle cx="6" cy="19" r="2"/><circle cx="18" cy="8" r="2"/><path d="M6 7v10M18 10c0 4-6 3-12 7"/>',
  "cloud-build-settings": '<circle cx="12" cy="12" r="3"/><path d="M12 2v3M12 19v3M2 12h3M19 12h3M4.9 4.9l2.1 2.1M17 17l2.1 2.1M4.9 19.1L7 17M17 7l2.1-2.1"/>',
  "cloud-build-worker-pools": '<rect x="3" y="4" width="8" height="7" rx="1.5"/><rect x="13" y="4" width="8" height="7" rx="1.5"/><rect x="3" y="13" width="8" height="7" rx="1.5"/><rect x="13" y="13" width="8" height="7" rx="1.5"/>',
  // Secure Source Manager: a branch.
  "secure-source-manager": '<circle cx="6" cy="5" r="2"/><circle cx="6" cy="19" r="2"/><circle cx="18" cy="8" r="2"/><path d="M6 7v10M18 10c0 4-6 3-12 7"/>',
  // Cloud Functions: braces around a bolt.
  functions: '<path d="M8 4c-2 0-3 1-3 3v2l-2 3 2 3v2c0 2 1 3 3 3M16 4c2 0 3 1 3 3v2l2 3-2 3v2c0 2-1 3-3 3"/><path d="M13 7l-3 5h4l-3 5"/>',
  // Artifact Registry: stacked boxes.
  "artifact-registry": '<rect x="4" y="4" width="16" height="6" rx="1"/><rect x="4" y="14" width="16" height="6" rx="1"/>',
  // Cloud Storage: a bucket.
  storage:   '<path d="M4 7h16l-1.6 11.2a2 2 0 0 1-2 1.8H7.6a2 2 0 0 1-2-1.8z"/><path d="M3 7h18"/><path d="M9 4h6l1 3H8z"/>',
  // Pub/Sub: one publisher, many subscribers.
  pubsub:    '<circle cx="5" cy="12" r="2"/><circle cx="19" cy="6" r="2"/><circle cx="19" cy="12" r="2"/><circle cx="19" cy="18" r="2"/><path d="M7 12h3M10 12l7-5M10 12h7M10 12l7 5"/>',
  // Pub/Sub subscriptions: one subscriber taking messages off a topic.
  "pubsub-subscriptions": '<circle cx="5" cy="12" r="2"/><circle cx="19" cy="12" r="2"/><path d="M7 12h10M14 9l3 3-3 3"/>',
  // Cloud Tasks: a queue of work, oldest first.
  tasks:     '<rect x="3" y="4" width="18" height="4" rx="1"/><rect x="3" y="10" width="18" height="4" rx="1"/><rect x="3" y="16" width="12" height="4" rx="1"/>',
  // Secret Manager: a key.
  secrets:   '<circle cx="8" cy="12" r="3.5"/><path d="M11.5 12H21"/><path d="M18 12v3M15 12v2.5"/>',
  // Cloud KMS: a key inside a shield.
  kms:       '<path d="M12 3l7 3v5c0 4.5-3 8.3-7 10-4-1.7-7-5.5-7-10V6z"/><circle cx="10" cy="11" r="2"/><path d="M12 11h4M15 11v2"/>',
  // Certificate Authority Service: a certificate with a seal.
  privateca: '<rect x="3" y="4" width="18" height="12" rx="1"/><path d="M7 8h10M7 11h6"/><circle cx="17" cy="16" r="3"/><path d="M15.5 18.5L15 22l2-1 2 1-.5-3.5"/>',
  // BigQuery: a magnifier over a bar chart.
  bigquery:  '<circle cx="10.5" cy="10.5" r="6.5"/><path d="M15.2 15.2L21 21"/><path d="M8 12.5v-2M10.5 12.5v-4M13 12.5v-3"/>',
  // Cloud Scheduler: a clock.
  scheduler: '<circle cx="12" cy="12" r="9"/><path d="M12 7v5l3 2"/>',
  // Kubernetes Engine: the helm.
  k8s:       '<path d="M12 3l7.5 3.8v10.4L12 21l-7.5-3.8V6.8z"/><circle cx="12" cy="12" r="2.5"/><path d="M12 3v6.5M19.5 6.8l-5.4 4M19.5 17.2l-5.4-4M12 21v-6.5M4.5 17.2l5.4-4M4.5 6.8l5.4 4"/>',
  workloads: '<rect x="3" y="4" width="7" height="7" rx="1"/><rect x="14" y="4" width="7" height="7" rx="1"/><rect x="3" y="13" width="7" height="7" rx="1"/><rect x="14" y="13" width="7" height="7" rx="1"/>',
  pods:      '<path d="M12 3l7.5 3.8v10.4L12 21l-7.5-3.8V6.8z"/>',
  k8sservices: '<circle cx="12" cy="12" r="2.5"/><circle cx="12" cy="4" r="1.8"/><circle cx="19" cy="16" r="1.8"/><circle cx="5" cy="16" r="1.8"/><path d="M12 6v3.5M13.8 13.4l3.6 1.8M10.2 13.4l-3.6 1.8"/>',
  jobs:      '<rect x="3" y="7" width="18" height="13" rx="2"/><path d="M9 7V5a2 2 0 0 1 2-2h2a2 2 0 0 1 2 2v2"/><path d="M9 13h6"/>',
  // Cluster nodes: the machines underneath.
  nodes:     '<rect x="3" y="4" width="18" height="6" rx="1"/><rect x="3" y="14" width="18" height="6" rx="1"/><path d="M7 7h.01M7 17h.01"/>',
  // Cluster storage: a stack of disks.
  k8sstorage:'<ellipse cx="12" cy="6" rx="8" ry="3"/><path d="M4 6v12c0 1.7 3.6 3 8 3s8-1.3 8-3V6"/><path d="M4 12c0 1.7 3.6 3 8 3s8-1.3 8-3"/>',
  events:    '<circle cx="12" cy="12" r="9"/><path d="M12 7v6M12 16h.01"/>',
  // Agent Platform (formerly Vertex AI): a spark.
  ai:        '<path d="M12 3l1.9 5.1L19 10l-5.1 1.9L12 17l-1.9-5.1L5 10l5.1-1.9z"/><path d="M18 16l.8 2.2L21 19l-2.2.8L18 22l-.8-2.2L15 19l2.2-.8z"/>',
  playground:'<rect x="3" y="4" width="18" height="14" rx="3"/><path d="M8 10.5h.01M12 10.5h.01M16 10.5h.01"/><path d="M8 14h6"/>',
  // Resource Manager: a folder of projects.
  projects:  '<path d="M3 7h6l2 2h10v10H3z"/><path d="M3 7V5h6l2 2"/>',
  // IAM: a person beside a key.
  // Resource hierarchy: an organization over two folders.
  hierarchy: '<rect x="9" y="3" width="6" height="5" rx="1"/><rect x="3" y="16" width="6" height="5" rx="1"/><rect x="15" y="16" width="6" height="5" rx="1"/><path d="M12 8v4M6 16v-4h12v4"/>',
  // Tags: a tag with its hole.
  tags:      '<path d="M3 12V4h8l10 10-8 8z"/><circle cx="7.5" cy="8" r="1.5"/>',
  // APIs & Services: braces around three calls (Connect has the plug).
  serviceusage: '<path d="M8 4H5v16h3M16 4h3v16h-3"/><path d="M9 12h.01M12 12h.01M15 12h.01" stroke-width="2.5"/>',
  iam:       '<circle cx="8" cy="8" r="3"/><path d="M3 19a5 5 0 0 1 10 0"/><circle cx="17" cy="13" r="2"/><path d="M17 15v5M17 18h2"/>',
  // Firestore: stacked documents.
  firestore: '<path d="M8 3h9l3 3v11H8z"/><path d="M5 7v14h11"/><path d="M11 9h6M11 13h6"/>',
  // Datastore: an entity table.
  datastore: '<rect x="3" y="4" width="18" height="16" rx="2"/><path d="M3 9h18M3 14.5h18M9 9v11"/>',
  // Bigtable: a wide grid of cells.
  bigtable:  '<rect x="3" y="5" width="18" height="14" rx="1"/><path d="M3 9.7h18M3 14.3h18M7.5 5v14M12 5v14M16.5 5v14"/>',
  // Spanner: a globe, for a database that spans regions.
  spanner:   '<circle cx="12" cy="12" r="9"/><path d="M3 12h18"/><path d="M12 3c2.5 2.5 3.8 5.5 3.8 9s-1.3 6.5-3.8 9c-2.5-2.5-3.8-5.5-3.8-9S9.5 5.5 12 3z"/>',
  // Memorystore: a memory module, for an in-memory store.
  memorystore: '<rect x="3" y="7" width="18" height="10" rx="2"/><path d="M7 17v3M12 17v3M17 17v3M7 11h2M11 11h2M15 11h2"/>',
  // Cloud SQL: a database cylinder with a query line.
  cloudsql:  '<ellipse cx="12" cy="5.5" rx="7" ry="2.5"/><path d="M5 5.5v13c0 1.4 3.1 2.5 7 2.5s7-1.1 7-2.5v-13"/><path d="M9 12h6M9 16h4"/>',
  monitoring:'<path d="M3 3v18h18"/><path d="M7 15l4-5 3 3 5-7"/>',
  requests:  '<path d="M4 7h13M13 3l4 4-4 4"/><path d="M20 17H7M11 13l-4 4 4 4"/>',
  faults:    '<path d="M13 3L5 14h6l-1 7 8-11h-6z"/>',
  instance:  '<rect x="4" y="4" width="16" height="6" rx="1"/><rect x="4" y="14" width="16" height="6" rx="1"/><path d="M8 7h.01M8 17h.01"/>',
  logs:      '<path d="M5 4h11l3 3v13H5z"/><path d="M8 11h8M8 15h5"/>',
  activity:  '<path d="M3 12h4l3-7 4 14 3-7h4"/>',
  // Connect: a plug.
  connect:   '<path d="M9 3v5M15 3v5"/><path d="M6 8h12v3a6 6 0 0 1-12 0z"/><path d="M12 17v4"/>',
  dashboard: '<rect x="3" y="3" width="8" height="10" rx="1"/><rect x="13" y="3" width="8" height="6" rx="1"/><rect x="3" y="15" width="8" height="6" rx="1"/><rect x="13" y="11" width="8" height="10" rx="1"/>',
};

// Category marks, keyed by the section name: CloudBurrow's own line drawings,
// like the product marks above. The category names are Google's product
// taxonomy; the drawings are not Google's artwork.
const CATEGORY_ICONS = {
  // A lightning bolt: code that runs on demand.
  "Serverless computing": '<path d="M13 3L5 13h6l-1 8 8-10h-6z"/>',
  // A shipping container.
  "Containers": '<rect x="3" y="6" width="18" height="12" rx="1"/><path d="M7 6v12M11 6v12M15 6v12M19 6v12"/>',
  // A drive.
  "Storage": '<rect x="3" y="13" width="18" height="7" rx="2"/><path d="M5 13l2-8h10l2 8"/><path d="M16 16.5h.01"/>',
  // A database cylinder.
  "Databases": '<ellipse cx="12" cy="5.5" rx="7" ry="2.5"/><path d="M5 5.5v13c0 1.4 3.1 2.5 7 2.5s7-1.1 7-2.5v-13"/><path d="M5 12c0 1.4 3.1 2.5 7 2.5s7-1.1 7-2.5"/>',
  // Two joined links.
  "Integration services": '<path d="M10 14a4 4 0 0 0 5.7 0l3-3a4 4 0 0 0-5.7-5.7l-1 1"/><path d="M14 10a4 4 0 0 0-5.7 0l-3 3a4 4 0 0 0 5.7 5.7l1-1"/>',
  // A spark.
  "AI and machine learning": '<path d="M12 3l1.9 5.1L19 10l-5.1 1.9L12 17l-1.9-5.1L5 10l5.1-1.9z"/>',
  // A shield.
  "Security and identity": '<path d="M12 3l7 3v5c0 4.5-3 8.3-7 10-4-1.7-7-5.5-7-10V6z"/><path d="M9 12l2 2 4-4"/>',
  // A pulse line.
  "Operations": '<path d="M3 12h4l3-7 4 14 3-7h4"/>',
  // Two arrows chasing each other: build, then deploy, then build again.
  "CI/CD": '<path d="M4 12a8 8 0 0 1 13.7-5.6L20 9"/><path d="M20 4v5h-5"/><path d="M20 12a8 8 0 0 1-13.7 5.6L4 15"/><path d="M4 20v-5h5"/>',
  // A person: who may do what.
  "IAM & Admin": '<circle cx="12" cy="8" r="4"/><path d="M4 21a8 8 0 0 1 16 0"/>',
  // A wrench.
  "Management tools": '<path d="M14.5 6.5a4 4 0 0 0 5 5L21 13l-2 2-1.5-1.5a4 4 0 0 1-5-5L5 16v3h3l7.5-7.5"/>',
};

// lineMark renders one of the drawings above as an inline SVG.
const lineMark = (paths) =>
  el("span", { html: `<svg viewBox="0 0 24 24" aria-hidden="true">${paths}</svg>` });

// Capabilities come from the backend, so a control only ever appears when
// the service behind it can actually perform it.
let SERVICES = [];
let DEFAULT_PROJECT = "";
const capabilityOf = (id) => SERVICES.find((s) => s.id === id) || {};

// --- scopes narrower than the project (#1360) ---------------------------
//
// A service whose screens address something inside the project — Bigtable's
// tables live in an instance — names a query parameter (its capability's
// scope), carried in the URL the way the project is. Every request made for
// the service and every link within its screens keeps it, so a table's page,
// its rows and its actions address the instance its list was read from.

// scopeKeyOf is the service's scope parameter, or "" for one scoped by the
// project alone.
const scopeKeyOf = (service) => capabilityOf(service).scope || "";

// carryScope copies the service's scope from the address onto url.
function carryScope(url, service) {
  const key = scopeKeyOf(service);
  if (!key) return url;
  // A Filterer's own pickers (Listing.Filters, #1846) travel with it.
  const here = new URLSearchParams(location.search);
  for (const k of [key, ...(capabilityOf(service).filters || [])]) {
    const value = here.get(k);
    if (value && !url.searchParams.has(k)) url.searchParams.set(k, value);
  }
  return url;
}

// scopeServiceOfPath is the scoped service whose screens a console path is
// on, or "".
function scopeServiceOfPath(pathname) {
  for (const s of SERVICES) {
    if (!s.scope) continue;
    const r = ROUTES.find((x) => x.service === s.id);
    const base = r ? r.path : "/" + s.id;
    if (pathname === base || pathname.startsWith(base + "/")) return s.id;
  }
  return "";
}

// scopedAPIPath adds the service's scope to an /api/<kind>/<service> request
// made from that service's own screens, when the request does not name it. A
// search, or another product's page reading the service, is not inside the
// scope chosen there.
function scopedAPIPath(path) {
  if (typeof path !== "string" || !path.startsWith("/api/")) return path;
  const url = new URL(path, location.origin);
  const service = url.pathname.split("/")[3] || "";
  if (!scopeKeyOf(service) || scopeServiceOfPath(location.pathname) !== service) return path;
  carryScope(url, service);
  return url.pathname + url.search;
}

// scopeSubtitle names the scope a scoped service's page is in, after its
// project: " · instance archive".
function scopeSubtitle(service) {
  const key = scopeKeyOf(service);
  const value = key ? new URLSearchParams(location.search).get(key) : "";
  return value ? ` · ${key} ${value}` : "";
}

// scopePicker is a list screen's picker for its scope: choosing puts it in
// the address (the default leaves it out) and redraws.
function scopePicker(spec, reload, clears = []) {
  const id = `scope-${spec.key}`;
  const select = el("select", { id, name: spec.key },
    ...(spec.options || []).map((o) =>
      el("option", { value: o.id, text: o.label || o.id, selected: o.id === spec.selected })));
  select.addEventListener("change", () => {
    const params = new URLSearchParams(location.search);
    if (select.value && select.value !== spec.default) params.set(spec.key, select.value);
    else params.delete(spec.key);
    // A filter after this one was chosen among this one's (#1846): a
    // kind of another namespace.
    for (const k of clears) params.delete(k);
    const qs = params.toString();
    history.pushState(history.state, "", location.pathname + (qs ? `?${qs}` : ""));
    reload();
  });
  return el("div", { class: "card server-filter scope-picker" },
    el("label", { for: id, text: spec.label || spec.key }),
    el("div", { class: "server-filter-row" }, select));
}

const el = (tag, attrs = {}, ...children) => {
  const node = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (v === null || v === undefined || v === false) continue;
    if (k === "class") node.className = v;
    else if (k === "text") node.textContent = v;
    else if (k === "html") node.innerHTML = v;
    else if (k.startsWith("on")) node.addEventListener(k.slice(2), v);
    else node.setAttribute(k, v === true ? "" : v);
  }
  for (const c of children.flat()) {
    if (c === null || c === undefined) continue;
    node.append(c);
  }
  return node;
};

const announce = (msg) => { document.getElementById("live").textContent = msg; };

// setChildren replaces a node's children, dropping absent ones.
//
// Node.replaceChildren stringifies anything that is not a Node, so passing a
// conditional child that evaluated to null renders the literal text "null" on
// the page. That is not hypothetical: every list screen without a note showed
// "null" above its filter row. el() already filters its children this way;
// this is the same rule for the places that call replaceChildren directly.
const setChildren = (node, ...children) => {
  node.replaceChildren(...children.flat().filter((c) => c !== null && c !== undefined && c !== false));
};

// --- data ------------------------------------------------------------

// Every call is bounded.
//
// The server bounds its own work, but a request that never reaches it — a
// wedged tunnel, a port-forward that died — would leave the screen loading
// with no elapsed time, no cancel and no eventual error. That is the one
// failure this console is not allowed to have, because it is the one that
// looks exactly like success not having happened yet.
//
// Reads get 30 seconds against the server's own 20s budget; mutations get 70
// against its 60. Both are slack on top of the server's number rather than a
// second opinion about how long the work should take.
const READ_DEADLINE_MS = 30000;
const WRITE_DEADLINE_MS = 70000;

async function api(path, options = {}) {
  const deadline = options.deadline || READ_DEADLINE_MS;
  const controller = new AbortController();
  const started = Date.now();
  const timer = setTimeout(() => controller.abort(), deadline);
  // A caller's own signal is honoured as well as the deadline, so a screen
  // can offer Cancel without having to reimplement the timeout.
  let cancelled = false;
  const outer = options.signal;
  const onOuter = () => { cancelled = true; controller.abort(); };
  if (outer) {
    if (outer.aborted) onOuter();
    else outer.addEventListener("abort", onOuter, { once: true });
  }

  path = scopedAPIPath(path);
  let res;
  try {
    res = await fetch(path, {
      ...options,
      signal: controller.signal,
      headers: { Accept: "application/json", ...(options.headers || {}) },
    });
  } catch (err) {
    // A named path and an elapsed time, because "Failed to fetch" tells the
    // user nothing about which part of their instance stopped answering.
    if (cancelled) throw new Error("cancelled");
    if (controller.signal.aborted) {
      throw new Error(`${path} did not answer within ${Math.round((Date.now() - started) / 1000)}s`);
    }
    throw err;
  } finally {
    clearTimeout(timer);
    if (outer) outer.removeEventListener("abort", onOuter);
  }

  const text = await res.text();
  let body = {};
  try { body = text ? JSON.parse(text) : {}; } catch { /* not JSON */ }
  if (!res.ok) {
    // The service's own message reaches the screen. A status code alone
    // hides the constraint the caller actually violated.
    const failure = new Error(body.error || `${path} responded ${res.status} ${res.statusText}`);
    // The status and body, for a screen that answers a refusal rather than
    // showing it: the fault screen asks for the admin token on a 401.
    failure.status = res.status;
    failure.body = body;
    // A failed mutation is a record on the server like a successful one, and
    // its id is what lets the panel show one entry rather than two.
    if (body.operation) failure.operation = body.operation;
    throw failure;
  }
  return body;
}

const send = (path, method, body) =>
  api(path, {
    method,
    deadline: WRITE_DEADLINE_MS,
    headers: body ? { "Content-Type": "application/json" } : {},
    body: body ? JSON.stringify(body) : undefined,
  });

// --- failure surfaces --------------------------------------------------
//
// A thrown exception and a hung backend look identical from the user's
// chair, and both look like loading. Nothing caught either: a render function
// that threw left whatever was last painted — usually a skeleton — on screen
// for good, saying nothing.

// screenFailed replaces the current screen with an error card.
function screenFailed(err, what = "This screen") {
  const view = document.getElementById("view");
  const message = err && err.message ? err.message : String(err);
  if (!view) return;
  // Not errorState: its fixed line says the console could not read something
  // from the instance, and this is the console's own code failing. Saying the
  // backend broke would send the user to debug the wrong thing.
  setChildren(view, el("div", { class: "state error", role: "alert" },
    el("h2", { text: `${what} stopped` }),
    el("p", { text: "The console hit an error in its own code. This is a bug in CloudBurrow." }),
    el("pre", { text: message }),
    el("button", { class: "secondary", text: "Reload", onclick: () => location.reload() })));
  announce(`${what} stopped: ${message}`);
}

// shellFailed is for a failure with no screen to replace — one raised before
// or outside a render.
function shellFailed(err) {
  const message = err && err.message ? err.message : String(err);
  let banner = document.getElementById("shell-error");
  if (!banner) {
    banner = el("div", { id: "shell-error", class: "shell-error", role: "alert" });
    document.body.prepend(banner);
  }
  setChildren(banner,
    el("span", { text: `The console hit an error: ${message}` }),
    el("button", { class: "secondary", text: "Reload", onclick: () => location.reload() }));
}

function installFailureSurfaces() {
  window.addEventListener("error", (e) => {
    // A failed resource load fires this too, with no Error object; those are
    // not script failures and must not blank the page.
    if (!e.error) return;
    shellFailed(e.error);
  });
  window.addEventListener("unhandledrejection", (e) => shellFailed(e.reason));
}

// --- operations ------------------------------------------------------
//
// One ledger, two views.
//
// The server holds the record — /api/operations — and the notifications panel
// is a view of it rather than a second copy of it. There used to be two
// independent ledgers: the panel was fed by an in-memory array that nothing
// seeded, so reloading the page silently erased the record of what had just
// happened while the same operations were still sitting in the Activity
// screen. The two surfaces could flatly contradict each other.
//
// The in-memory entries are optimistic and exist only between the click and
// the response — the one window the server cannot see. As soon as a response
// names its operation id, the local entry is reconciled away.

const OPERATIONS = [];
let SERVER_OPERATIONS = [];
// Keys the user has already looked at. Seeded from whatever exists at first
// load, so the badge counts what has happened since the console was opened
// rather than announcing the whole history on every reload.
const SEEN_OPERATIONS = new Set();
let LOCAL_SEQ = 0;

function recordOperation(label) {
  const op = { key: `local-${++LOCAL_SEQ}`, label, state: "running", at: new Date() };
  OPERATIONS.unshift(op);
  renderOperations();

  const finish = (state) => (detail, id) => {
    op.state = state;
    op.detail = detail || "";
    if (id) { op.id = id; op.key = id; }
    renderOperations();
    // The server's own record is the one that survives a reload, so it is
    // fetched as soon as there is something new in it.
    refreshOperations();
  };
  return { succeeded: finish("succeeded"), failed: finish("failed") };
}

// relativeTime answers the question the panel is actually asked.
//
// "Which of these happened first, and how long ago" is the reading; a
// wall-clock time makes the reader do the subtraction, and the timestamp was
// being captured and then thrown away entirely.
function relativeTime(date) {
  const secs = Math.max(0, Math.round((Date.now() - date.getTime()) / 1000));
  if (secs < 10) return "just now";
  if (secs < 60) return `${secs} sec ago`;
  const mins = Math.round(secs / 60);
  if (mins < 60) return `${mins} min ago`;
  const hours = Math.round(mins / 60);
  if (hours < 24) return `${hours} hr ago`;
  return `${Math.round(hours / 24)} d ago`;
}

const OPERATION_STATES = { SUCCEEDED: "succeeded", FAILED: "failed" };

// mergedOperations is the panel's list: the server's record, plus the local
// entries it does not know about yet.
function mergedOperations() {
  const fromServer = SERVER_OPERATIONS.map((o) => ({
    key: o.id, id: o.id, title: `${o.kind} ${o.resource || ""}`.trim(),
    state: OPERATION_STATES[o.state] || "running",
    detail: o.error || "", at: new Date(o.started),
  }));
  const known = new Set(fromServer.map((o) => o.id));
  const local = OPERATIONS
    .filter((o) => !o.id || !known.has(o.id))
    .map((o) => ({ key: o.key, id: o.id, title: o.label, state: o.state,
                   detail: o.detail || "", at: o.at }));
  return [...local, ...fromServer].sort((a, b) => b.at - a.at).slice(0, 20);
}

async function refreshOperations() {
  try {
    const data = await api(`/api/operations?project=${encodeURIComponent(currentProject())}`);
    SERVER_OPERATIONS = data.operations || [];
    OPERATIONS_STALE = "";
  } catch (err) {
    // Reported in the panel rather than as a snackbar: the bell failing to
    // refresh is worth knowing when you look at it, and not worth
    // interrupting whatever you were doing.
    OPERATIONS_STALE = err.message;
  }
  renderOperations();
}

let OPERATIONS_STALE = "";

function renderOperations() {
  const list = document.getElementById("notification-list");
  const count = document.getElementById("notification-count");
  const empty = document.querySelector("#notifications-panel .panel-empty");
  const stale = document.getElementById("notification-stale");
  if (!list) return;

  const ops = mergedOperations();

  setChildren(list, ...ops.map((op) =>
    el("li", { class: SEEN_OPERATIONS.has(op.key) ? null : "is-unread" },
      // Running is not a warning. Rendering it in the same amber the tables
      // use for "Paused" said something had gone slightly wrong; a moving
      // indicator says the only true thing, which is that it is still going.
      op.state === "running"
        ? el("span", { class: "status is-working" },
            spinner(), el("span", { text: op.title }))
        : el("span", { class: "status", "data-state": op.state === "succeeded" ? "ok" : "error" },
            el("span", { text: op.title })),
      el("div", { class: "op-when unavailable", text: relativeTime(op.at) }),
      // A failure carries its cause, and the cause links to the lines that
      // produced it: "it failed" without the reason is the least useful thing
      // a console can say.
      op.detail
        ? (op.state === "failed" && op.id
            ? el("a", { class: "op-detail",
                        href: `/logs?operation=${encodeURIComponent(op.id)}`,
                        text: op.detail })
            : el("div", { class: "op-detail unavailable", text: op.detail }))
        : null)));

  if (empty) empty.hidden = ops.length > 0;
  if (stale) {
    stale.hidden = !OPERATIONS_STALE;
    stale.textContent = OPERATIONS_STALE
      ? `Showing the last known list — refresh failed: ${OPERATIONS_STALE}`
      : "";
  }

  // The badge counts what has not been looked at, not what is running. A
  // badge that cleared itself the moment an operation finished told the user
  // nothing about the failure they had not seen yet.
  const unread = ops.filter((o) => !SEEN_OPERATIONS.has(o.key)).length;
  if (unread > 0) { count.hidden = false; count.textContent = String(unread); }
  else { count.hidden = true; }

  const bar = document.getElementById("busy-bar");
  if (bar) bar.hidden = !ops.some((o) => o.state === "running");
}

// markOperationsSeen is what opening the panel means.
function markOperationsSeen() {
  for (const op of mergedOperations()) SEEN_OPERATIONS.add(op.key);
  renderOperations();
}

// --- theme -----------------------------------------------------------
//
// The documented console behaviour is that changing the theme does not
// reload the page, so the attribute is swapped in place.

function applyTheme(choice) {
  const root = document.documentElement;
  if (choice === "system") root.removeAttribute("data-theme");
  else root.setAttribute("data-theme", choice);
  try { localStorage.setItem("cb-theme", choice); } catch { /* private mode */ }
}

function initTheme() {
  let stored = "system";
  try { stored = localStorage.getItem("cb-theme") || "system"; } catch { /* private mode */ }
  applyTheme(stored);
  for (const input of document.querySelectorAll('input[name="theme"]')) {
    input.checked = input.value === stored;
    input.addEventListener("change", () => applyTheme(input.value));
  }
}

// --- panels ----------------------------------------------------------

// --- overlays ---------------------------------------------------------
//
// An overlay that appears between two frames gives no sense of where it came
// from. `hidden` on its own cannot be animated — display:none has no
// intermediate state — so the attribute comes off a frame before the open
// class goes on, and goes back on once the exit has run.
//
// The class, not the attribute, is the source of truth for "is this open":
// during the exit the panel is still in the layout and must not be treated as
// open by the toggle or the outside-click handler.
const OVERLAY_EXIT_MS = 200;
const OVERLAY_TIMERS = new WeakMap();

const overlayOpen = (node) => node.classList.contains("is-open");

function showOverlay(node) {
  const pending = OVERLAY_TIMERS.get(node);
  if (pending) { clearTimeout(pending); OVERLAY_TIMERS.delete(node); }
  node.hidden = false;
  node.classList.remove("is-closing");
  // Not if it was closed again before the frame came: a picker closed by
  // the click that followed its opening stayed drawn open (#1842).
  requestAnimationFrame(() => { if (!node.hidden && !OVERLAY_TIMERS.has(node)) node.classList.add("is-open"); });
}

function hideOverlay(node) {
  node.classList.remove("is-open");
  // See-through on its way out, and not in the way of a click (#1842).
  node.classList.add("is-closing");
  const pending = OVERLAY_TIMERS.get(node);
  if (pending) clearTimeout(pending);
  // Hidden after the exit, so the panel does not vanish mid-animation. With
  // motion reduced the transition is none and this is simply a short delay
  // before an already-invisible node is taken out of the tree.
  OVERLAY_TIMERS.set(node, setTimeout(() => {
    node.hidden = true;
    node.classList.remove("is-closing");
    OVERLAY_TIMERS.delete(node);
  }, OVERLAY_EXIT_MS));
}

// The search field collapses to a control at narrow widths rather than
// disappearing.
//
// It used to be `display: none` below 960px, which deleted the console's most
// prominent control on the width most likely to be a phone — and offered
// nothing in its place, so cross-service search was simply unavailable there.
function initSearchToggle() {
  const toggle = document.getElementById("search-toggle");
  const input = document.getElementById("search");
  if (!toggle || !input) return;

  const root = document.documentElement;
  const close = () => {
    root.removeAttribute("data-search");
    toggle.setAttribute("aria-expanded", "false");
  };

  toggle.addEventListener("click", () => {
    if (root.getAttribute("data-search") === "open") { close(); toggle.focus(); return; }
    root.setAttribute("data-search", "open");
    toggle.setAttribute("aria-expanded", "true");
    input.focus();
  });
  input.addEventListener("keydown", (e) => {
    if (e.key === "Escape") { close(); toggle.focus(); }
  });
  // Leaving the field puts the bar back, the way a collapsed search does
  // everywhere else. Deferred, because the blur fires before a click on
  // anything inside the field's own row.
  input.addEventListener("blur", () => setTimeout(() => {
    if (document.activeElement !== input) close();
  }, 100));
}

function initPanel(buttonId, panelId, opts = {}) {
  const button = document.getElementById(buttonId);
  const panel = document.getElementById(panelId);

  const close = () => {
    hideOverlay(panel);
    button.setAttribute("aria-expanded", "false");
  };
  const open = () => {
    showOverlay(panel);
    button.setAttribute("aria-expanded", "true");
    if (opts.onOpen) opts.onOpen();
    // Focus moves into the dialog, and Escape returns it: a dialog that
    // traps neither is one a keyboard user cannot leave.
    // A panel with nothing to operate (Account), or whose contents are
    // redrawn as it opens (Notifications, which refreshes its list and so
    // removed the link focus had just moved to), takes focus itself, so it
    // is read out and Escape reaches it (#1604).
    const first = opts.focusPanel ? null : panel.querySelector("input, button, a[href], select, textarea");
    if (first) first.focus();
    else { panel.tabIndex = -1; panel.focus(); }
  };

  button.addEventListener("click", () => (overlayOpen(panel) ? (close(), button.focus()) : open()));
  // Escape closes it from inside it, from its button, and from wherever
  // focus fell to when what it was on was redrawn away. Open is read from
  // the button, which says so at once, rather than from is-open, which the
  // entrance transition sets a frame later.
  document.addEventListener("keydown", (e) => {
    if (e.key !== "Escape" || button.getAttribute("aria-expanded") !== "true") return;
    const at = document.activeElement;
    if (panel.contains(at) || at === button || !at || at === document.body) { close(); button.focus(); }
  });
  document.addEventListener("click", (e) => {
    if (overlayOpen(panel) && !panel.contains(e.target) && !button.contains(e.target)) close();
  });
}

// --- navigation ------------------------------------------------------

// The navigation menu.
//
// Modelled on the console this mirrors: a pinned section at the top, then the
// remaining products grouped under Google's own product categories, each group
// collapsible. Products can be pinned and unpinned, and the whole menu can be
// pinned open so it stops overlaying the page.
//
// Category names are Google's published product taxonomy — not names invented
// here. Their marks are CloudBurrow's own drawings (CATEGORY_ICONS).

const PINNED_KEY = "cloudburrow.pinned";
const OPEN_GROUPS_KEY = "cloudburrow.navgroups";
const MORE_OPEN_KEY = "cloudburrow.navmore";

function readStored(key, fallback) {
  try {
    const raw = localStorage.getItem(key);
    return raw ? JSON.parse(raw) : fallback;
  } catch { return fallback; }
}

function writeStored(key, value) {
  try { localStorage.setItem(key, JSON.stringify(value)); } catch { /* private mode */ }
}

// Pinned by default: the services a developer opens first. The console ships
// a default pin set too rather than an empty menu.
const DEFAULT_PINNED = ["storage", "pubsub", "run"];

// An ordered list rather than a set: the menu this mirrors lets you arrange
// your pinned products, and a set cannot hold an order.
let PINNED = null;
let OPEN_GROUPS = null;
// Set while the user is rearranging the menu, so a reveal does not fight the
// control they just used. Cleared on navigation, where revealing is the point.
let REVEAL_SUSPENDED = false;

function pinnedOrder() {
  if (!PINNED) PINNED = readStored(PINNED_KEY, DEFAULT_PINNED).slice();
  return PINNED;
}

function isPinned(service) {
  return pinnedOrder().includes(service);
}

function togglePinned(service) {
  const order = pinnedOrder();
  const at = order.indexOf(service);
  if (at >= 0) order.splice(at, 1);
  else order.push(service);
  writeStored(PINNED_KEY, order);
}

// movePinned reorders one product, which is what dragging does.
function movePinned(service, before) {
  const order = pinnedOrder();
  const from = order.indexOf(service);
  if (from < 0) return;
  order.splice(from, 1);
  const to = before ? order.indexOf(before) : order.length;
  order.splice(to < 0 ? order.length : to, 0, service);
  writeStored(PINNED_KEY, order);
}

function openGroups() {
  if (!OPEN_GROUPS) OPEN_GROUPS = new Set(readStored(OPEN_GROUPS_KEY, []));
  return OPEN_GROUPS;
}

function markFor(entry) {
  // A page borrows another's mark with `icon`: Cloud SQL's MySQL page carries
  // the product's one mark, not a drawing of its own.
  return lineMark(ICONS[entry.icon || entry.service || entry.screen] || ICONS.dashboard);
}

// navLink renders one product row, with its pin control.
// dragHandle makes a pinned row movable, as the real menu's are.
function dragHandle(entry, redraw) {
  return el("span", {
    class: "nav-drag", "aria-hidden": "true", title: "Drag to reorder",
    html: '<svg viewBox="0 0 24 24"><path d="M9 6h.01M9 12h.01M9 18h.01M15 6h.01M15 12h.01M15 18h.01"/></svg>',
  });
}

// --- the category flyout -----------------------------------------------
//
// Hovering or focusing a category opens a panel beside the drawer listing its
// products. The drawer's own scroll position never moves, which is the point:
// expanding in place pushes the row you pointed at out of view.

let FLYOUT = null;
let FLYOUT_OPEN_TIMER = null;
let FLYOUT_CLOSE_TIMER = null;
// Returning focus to the category that opened the panel would fire its own
// focus handler and open the panel straight back up, so Escape could never
// dismiss it. Set for the duration of that one focus() call.
let FLYOUT_REFUSE_FOCUS = false;
const FLYOUT_INTENT_MS = 180;

function flyoutHost() {
  if (FLYOUT) return FLYOUT;
  FLYOUT = el("div", { id: "nav-flyout", class: "nav-flyout", hidden: true, role: "menu" });
  FLYOUT.addEventListener("mouseenter", () => {
    if (FLYOUT_CLOSE_TIMER) { clearTimeout(FLYOUT_CLOSE_TIMER); FLYOUT_CLOSE_TIMER = null; }
  });
  FLYOUT.addEventListener("mouseleave", () => scheduleFlyoutClose());
  FLYOUT.addEventListener("keydown", (e) => {
    const items = [...FLYOUT.querySelectorAll("a")];
    const at = items.indexOf(document.activeElement);
    if (e.key === "Escape" || e.key === "ArrowLeft") {
      e.preventDefault();
      // Escape dismisses the panel, not the whole drawer. Without this the
      // global handler closed the menu underneath it and took focus to the
      // hamburger, so one key did two things and neither was undoable.
      e.stopPropagation();
      const opener = FLYOUT.opener;
      closeFlyout();
      if (opener && opener.isConnected) {
        FLYOUT_REFUSE_FOCUS = true;
        opener.focus();
        FLYOUT_REFUSE_FOCUS = false;
      }
      return;
    }
    const step = e.key === "ArrowDown" ? 1 : e.key === "ArrowUp" ? -1 : 0;
    if (!step || !items.length) return;
    e.preventDefault();
    items[(Math.max(at, 0) + step + items.length) % items.length].focus();
  });
  document.body.append(FLYOUT);
  return FLYOUT;
}

function scheduleFlyoutClose() {
  if (FLYOUT_CLOSE_TIMER) clearTimeout(FLYOUT_CLOSE_TIMER);
  FLYOUT_CLOSE_TIMER = setTimeout(closeFlyout, FLYOUT_INTENT_MS);
}

function closeFlyout() {
  if (FLYOUT_OPEN_TIMER) { clearTimeout(FLYOUT_OPEN_TIMER); FLYOUT_OPEN_TIMER = null; }
  if (FLYOUT_CLOSE_TIMER) { clearTimeout(FLYOUT_CLOSE_TIMER); FLYOUT_CLOSE_TIMER = null; }
  if (!FLYOUT) return;
  hideOverlay(FLYOUT);
  if (FLYOUT.opener) FLYOUT.opener.setAttribute("aria-expanded", "false");
}

function openFlyout(toggle, section, items, redraw, focusFirst = false) {
  // Below the docked width there is no room beside the drawer, and a touch
  // device has no hover to express intent with: there, clicking to expand is
  // the whole interaction.
  if (FLYOUT_REFUSE_FOCUS) return;
  // A closed drawer is visibility:hidden, and a panel hanging off an
  // invisible row is a menu with no menu.
  if (document.documentElement.dataset.nav === "closed") return;
  if (!window.matchMedia("(min-width: 1280px) and (hover: hover)").matches) return;
  if (FLYOUT_CLOSE_TIMER) { clearTimeout(FLYOUT_CLOSE_TIMER); FLYOUT_CLOSE_TIMER = null; }
  if (FLYOUT_OPEN_TIMER) clearTimeout(FLYOUT_OPEN_TIMER);

  // A delay, so dragging the pointer down the drawer does not flash a panel
  // for every category it crosses.
  const show = () => {
    const host = flyoutHost();
    host.opener = toggle;
    const box = toggle.getBoundingClientRect();
    host.style.top = `${Math.round(box.top)}px`;
    host.style.left = `${Math.round(box.right + 4)}px`;
    setChildren(host,
      el("p", { class: "nav-flyout-title", text: section }),
      el("ul", {}, ...items.map((entry) =>
        el("li", {},
          el("a", {
            href: entry.path + scopeSearch(), role: "menuitem",
            onclick: () => closeFlyout(),
          },
            el("span", { class: "nav-icon" }, markFor(entry)),
            el("span", { text: productTitle(entry) }))))));
    showOverlay(host);
    toggle.setAttribute("aria-expanded", "true");
    if (focusFirst) {
      const first = host.querySelector("a");
      if (first) first.focus();
    }
  };
  if (focusFirst) show();
  else FLYOUT_OPEN_TIMER = setTimeout(show, FLYOUT_INTENT_MS);
}

function navLink(entry, onPinChange, nested = false, draggable = false) {
  // The drawer names and pins the product, so pinning "Kubernetes Engine"
  // cannot be mistaken for pinning one of its five pages.
  const key = productKey(entry);
  const label = entry.section ? productTitle(entry) : entry.title;
  const pinned = isPinned(key);
  const pin = el("button", {
    class: "nav-pin" + (pinned ? " is-pinned" : ""),
    "aria-label": (pinned ? "Unpin " : "Pin ") + label,
    "aria-pressed": pinned ? "true" : "false",
    title: pinned ? "Unpin" : "Pin",
    onclick: (e) => {
      e.preventDefault();
      e.stopPropagation();
      togglePinned(key);
      onPinChange();
    },
  }, el("span", { html: `<svg viewBox="0 0 24 24" aria-hidden="true">${PIN_ICON}</svg>` }));

  const row = el("li", {
    class: "nav-item" + (nested ? " nav-nested" : "") + (draggable ? " is-draggable" : ""),
    draggable: draggable ? "true" : null,
    "data-service": key || null,
  },
    draggable ? dragHandle(entry, onPinChange) : null,
    el("a", {
      href: entry.path + scopeSearch(),
      // Every page the product owns, so the drawer marks the product while
      // any of its pages is open.
      "data-path": pagesOf(key).map((r) => r.path).join(" ") || entry.path,
      "aria-label": label, title: label,
    },
      el("span", { class: "nav-icon" }, markFor(entry)),
      el("span", { class: "nav-label", text: label })),
    entry.service ? pin : null);

  if (draggable) {
    row.addEventListener("dragstart", (e) => {
      e.dataTransfer.setData("text/plain", key);
      e.dataTransfer.effectAllowed = "move";
      row.classList.add("is-dragging");
    });
    row.addEventListener("dragend", () => row.classList.remove("is-dragging"));
    row.addEventListener("dragover", (e) => {
      e.preventDefault();
      e.dataTransfer.dropEffect = "move";
      row.classList.add("is-drop-target");
    });
    row.addEventListener("dragleave", () => row.classList.remove("is-drop-target"));
    row.addEventListener("drop", (e) => {
      e.preventDefault();
      row.classList.remove("is-drop-target");
      const moved = e.dataTransfer.getData("text/plain");
      if (moved && moved !== key) {
        movePinned(moved, key);
        onPinChange();
      }
    });
  }
  return row;
}

// scopeSearch is the query string a navigation link may carry.
//
// Only the project travels. Passing on the whole of location.search took the
// current screen's own state with it — a detail screen's ?resource= made the
// drawer's link to that product re-open the resource you were already looking
// at, and a search's ?q= rode along to every product.
function scopeSearch() {
  const from = new URLSearchParams(location.search);
  const scope = new URLSearchParams();
  for (const key of ["project"]) {
    const value = from.get(key);
    if (value) scope.set(key, value);
  }
  const query = scope.toString();
  return query ? "?" + query : "";
}

const PIN_ICON = '<path d="M9 4h6l-1 6 3 3v2H7v-2l3-3z"/><path d="M12 15v5"/>';

function buildNav(services) {
  const list = document.getElementById("nav-list");
  const available = new Set(services.map((s) => s.id));
  const redraw = () => buildNav(services);

  // Screens, not every address. A create form and the catalogue page both
  // have routes so they can be linked to; neither is a row in the menu.
  const entries = ROUTES.filter((r) =>
    r.path !== "/search" && r.path !== "/projectcreate" && r.path !== "/products" && r.screen !== "create" &&
    (!r.service || available.has(r.service) || !r.section) && (!r.requires || available.has(r.requires)));

  const dashboard = entries.find((e) => e.path === "/");
  // One row per product, not per page. A product's first available page is
  // what the row points at, and the page it lands on is marked by the
  // in-product navigation rather than by the drawer.
  const pages = entries.filter((e) => e.path !== "/" && e.section);
  const products = [];
  const seenProducts = new Set();
  for (const page of pages) {
    const key = productKey(page);
    if (seenProducts.has(key)) continue;
    seenProducts.add(key);
    products.push(page);
  }
  // In the order the developer arranged them, not the order they are declared.
  const byProduct = new Map(products.map((e) => [productKey(e), e]));
  const pinned = pinnedOrder().map((id) => byProduct.get(id)).filter(Boolean);

  // A rule, not a heading's border: the blocks are separate things and the
  // separator belongs between them rather than attached to whichever heading
  // happens to come next.
  const divider = () => el("li", { class: "nav-divider", role: "separator" });

  const children = [];
  if (dashboard) children.push(navLink(dashboard, redraw));

  // Pinned first, as the console does. The heading is omitted when nothing is
  // pinned rather than leaving an empty section.
  if (pinned.length) {
    if (dashboard) children.push(divider());
    children.push(el("li", { class: "nav-section", role: "presentation" },
      el("span", { text: "Pinned" })));
    children.push(...pinned.map((e) => navLink(e, redraw, false, true)));
  }

  // The break between the pinned products and the full catalogue. Without it
  // the two read as one list and a pinned product looks like a category.
  if (dashboard || pinned.length) children.push(divider());

  // The catalogue sits behind "More products", as it does in the menu this
  // mirrors: pinned products are what you reach for, and everything else is
  // one level further in rather than a wall of categories under them.
  // The category holding the screen you are on is opened for this render only.
  //
  // "Where am I" is the first question a menu answers, and it could not answer
  // it: both the catalogue and every category default to closed, so a deep
  // link to a product left the drawer with no row for the current screen and
  // nothing marked. Revealing it is not a preference change, so it is not
  // written to storage — collapsing the category again must still stick.
  // Matched against every page, not only the row the drawer shows: a deep
  // link to /kubernetes/pods has to reveal Containers even though the row
  // there is Kubernetes Engine.
  const active = pages.find((e) => e.path === location.pathname);
  const revealed = REVEAL_SUSPENDED ? null : (active ? active.section : null);

  // The stored preference and the effective state are kept apart on purpose.
  //
  // A toggle must act on what the user chose, not on what the reveal forced.
  // Computing `!moreOpen` from the effective value left the control stuck: on
  // a revealed screen the effective value is always true, so the toggle could
  // only ever write false, and the catalogue could never be opened for good.
  const moreStored = readStored(MORE_OPEN_KEY, false);
  const moreOpen = moreStored || Boolean(revealed);
  children.push(el("li", { class: "nav-group-item" },
    el("button", {
      class: "nav-group nav-more" + (moreOpen ? " is-open" : ""),
      "aria-expanded": moreOpen ? "true" : "false",
      onclick: () => {
        // Acting on the control drops the reveal, so the choice sticks.
        REVEAL_SUSPENDED = true;
        writeStored(MORE_OPEN_KEY, !moreStored);
        redraw();
      },
    },
      el("span", { class: "nav-label", text: "More products" }),
      el("span", { class: "nav-chevron", "aria-hidden": "true",
                   html: '<svg viewBox="0 0 24 24"><path d="M9 6l6 6-6 6"/></svg>' }))));

  // Then the categories, each collapsible and carrying its own mark.
  const groups = new Map();
  for (const e of products) {
    if (!groups.has(e.section)) groups.set(e.section, []);
    groups.get(e.section).push(e);
  }

  for (const [section, items] of moreOpen ? groups : []) {
    const open = openGroups().has(section) || section === revealed;
    const mark = CATEGORY_ICONS[section];
    const toggle = el("button", {
      class: "nav-group" + (open ? " is-open" : ""),
      "aria-expanded": open ? "true" : "false",
      "aria-haspopup": "true",
      // Pointing opens the category beside the drawer; clicking still expands
      // it in place. With nine categories in a 320px column, expanding pushes
      // the row you clicked out of view and makes comparing two of them
      // impossible — the flyout is what makes the catalogue browsable by
      // pointing, and the in-place expansion stays for touch and for narrow
      // windows where there is no room beside the drawer.
      onmouseenter: () => openFlyout(toggle, section, items, redraw),
      onmouseleave: () => scheduleFlyoutClose(),
      onfocus: () => openFlyout(toggle, section, items, redraw),
      onkeydown: (e) => {
        if (e.key === "ArrowRight") { e.preventDefault(); openFlyout(toggle, section, items, redraw, true); }
        if (e.key === "Escape") closeFlyout();
      },
      onclick: () => {
        closeFlyout();
        REVEAL_SUSPENDED = true;
        const set = openGroups();
        if (set.has(section)) set.delete(section);
        else set.add(section);
        writeStored(OPEN_GROUPS_KEY, [...set]);
        redraw();
      },
    },
      el("span", { class: "nav-icon" }, mark ? lineMark(mark) : null),
      el("span", { class: "nav-label", text: section }),
      el("span", { class: "nav-chevron", "aria-hidden": "true",
                   html: '<svg viewBox="0 0 24 24"><path d="M9 6l6 6-6 6"/></svg>' }));

    children.push(el("li", { class: "nav-group-item" }, toggle));
    if (open) {
      children.push(...items.map((e) => navLink(e, redraw, true)));
    }
  }

  // Screens with no product category sit at the end, as utilities do, behind
  // their own rule.
  const loose = entries.filter((x) => x.path !== "/" && !x.section);
  if (loose.length) {
    children.push(divider());
    for (const e of loose) children.push(navLink(e, redraw));
  }

  setChildren(list, ...children);
  markCurrent();

  // A revealed row deep in the catalogue is no use below the fold.
  const current = list.querySelector('a[aria-current="page"]');
  if (current && revealed) revealInside(list, current);
}

// revealInside scrolls container until node is inside it, as
// scrollIntoView({ block: "nearest" }) would, without scrollIntoView: in
// Chrome that also moves the sequential focus starting point to the node, so
// on every page but the dashboard the first Tab skipped the skip link and the
// toolbar and landed in the drawer beside the current product (#1604).
function revealInside(container, node) {
  const c = container.getBoundingClientRect(), r = node.getBoundingClientRect();
  if (r.top < c.top) container.scrollTop -= c.top - r.top;
  else if (r.bottom > c.bottom) container.scrollTop += Math.min(r.bottom - c.bottom, r.top - c.top);
}

function markCurrent() {
  for (const a of document.querySelectorAll("#nav a")) {
    // A drawer row stands for a product, and data-path lists every page that
    // product owns — so the row stays marked wherever inside it you are.
    const paths = (a.dataset.path || "").split(" ");
    if (paths.includes(location.pathname)) a.setAttribute("aria-current", "page");
    else a.removeAttribute("aria-current");
  }
}

// The navigation menu.
//
// Three states, as the console it mirrors has:
//
//   closed  the menu is not on screen and the page uses the full width
//   open    it overlays the page, above a scrim, and closes when you pick
//           something — which is what a menu does
//   docked  it is pinned open and the page sits beside it
//
// The menu button toggles closed and open. The pin inside the menu docks it.
// Both are remembered, because a menu you have to reopen on every page is a
// menu you stop using.
const NAV_STATE_KEY = "cloudburrow.nav";

function navState() {
  return document.documentElement.dataset.nav || "closed";
}

function navIsNarrow() {
  return window.matchMedia("(max-width: 959px)").matches;
}

function applyNavState(state) {
  const nav = document.getElementById("nav");
  const scrim = document.getElementById("nav-scrim");
  const toggle = document.getElementById("nav-toggle");
  const dock = document.getElementById("nav-dock");

  document.documentElement.dataset.nav = state;
  const open = state === "open" || state === "docked";
  toggle.setAttribute("aria-expanded", open ? "true" : "false");
  dock.setAttribute("aria-pressed", state === "docked" ? "true" : "false");
  dock.setAttribute("aria-label", state === "docked" ? "Unpin menu" : "Keep menu open");
  // Only the overlay is modal. A docked menu is part of the page and must not
  // be hidden from a screen reader or sit behind a scrim.
  scrim.hidden = state !== "open";
  nav.setAttribute("aria-hidden", state === "closed" ? "true" : "false");

  // While the menu is modal the content behind it is not interactive, and it
  // should not be reachable by a screen reader either.
  //
  // Only the content region. The toolbar keeps working, because the menu
  // button lives in it and is one of the ways out — and inert cannot be
  // undone on a descendant, so marking the toolbar would disable the very
  // control that closes the menu. Tab is kept inside the drawer by the trap
  // in initNavToggle rather than by making the toolbar unreachable.
  const main = document.getElementById("main");
  if (main) {
    if (state === "open") main.setAttribute("inert", "");
    else main.removeAttribute("inert");
  }
}

function setNavState(state, { remember = true } = {}) {
  applyNavState(state);
  if (remember && !navIsNarrow()) {
    try { localStorage.setItem(NAV_STATE_KEY, state); } catch { /* private mode */ }
  }
}

function openNav() {
  setNavState("open", { remember: false });
  // Focus moves into the menu, and Escape gives it back: a modal you cannot
  // leave by keyboard is a trap.
  const first = document.querySelector("#nav a, #nav button");
  if (first) first.focus();
}

function closeNav({ focusToggle = false } = {}) {
  setNavState("closed", { remember: false });
  if (focusToggle) document.getElementById("nav-toggle").focus();
}

function initNavToggle() {
  const toggle = document.getElementById("nav-toggle");
  const scrim = document.getElementById("nav-scrim");
  const dock = document.getElementById("nav-dock");
  const nav = document.getElementById("nav");

  // A wide window starts docked and a narrow one closed, which is what the
  // width itself suggests. A remembered choice wins over both.
  let initial = "closed";
  if (!navIsNarrow()) {
    let stored = null;
    try { stored = localStorage.getItem(NAV_STATE_KEY); } catch { /* ignore */ }
    initial = stored === "closed" || stored === "docked" ? stored
      : window.matchMedia("(min-width: 1280px)").matches ? "docked" : "closed";
  }
  applyNavState(initial);

  toggle.addEventListener("click", () => {
    if (navState() === "closed") openNav();
    else if (navState() === "open") closeNav();
    else setNavState("closed"); // undocking through the menu button
  });
  scrim.addEventListener("click", () => closeNav({ focusToggle: true }));

  dock.addEventListener("click", () => {
    setNavState(navState() === "docked" ? "open" : "docked");
    dock.focus();
  });

  document.addEventListener("keydown", (e) => {
    if (navState() !== "open") return;
    if (e.key === "Escape") { closeNav({ focusToggle: true }); return; }
    if (e.key !== "Tab") return;

    // Focus stays in the menu while it is modal. Without this, tabbing walks
    // out of the drawer and onto the page behind the scrim — controls the user
    // cannot see and, because the scrim is covering them, cannot click either.
    const focusable = [...nav.querySelectorAll("a[href], button:not([disabled])")]
      .filter((el) => el.offsetParent !== null);
    if (!focusable.length) return;
    const first = focusable[0];
    const last = focusable[focusable.length - 1];
    if (e.shiftKey && document.activeElement === first) {
      e.preventDefault();
      last.focus();
    } else if (!e.shiftKey && document.activeElement === last) {
      e.preventDefault();
      first.focus();
    }
  });

  // Following a link closes an overlaid menu. A docked one stays, because it
  // is not covering anything.
  nav.addEventListener("click", (e) => {
    if (navState() === "open" && e.target.closest("a")) closeNav();
  });

  document.getElementById("nav-all").addEventListener("click", () => {
    // It navigates, and it leaves the drawer alone.
    //
    // It used to expand all nine categories in place and write that to
    // storage — producing one scrolling column of twenty rows that is harder
    // to scan than the collapsed state it replaced, and permanently
    // overwriting a preference the user had set by hand, with nothing to
    // restore it. It was the only control in this console that did that.
    closeNav();
    navigate("/products");
  });
}

// renderProducts is the full catalogue, in the content area.
//
// Twenty products across nine categories do not fit a 320px drawer; the
// console this mirrors puts them on a page laid out in columns, and so does
// this. Pinning from here writes the same store the drawer reads.
async function renderProducts(view) {
  const available = new Set(SERVICES.map((sv) => sv.id));
  const entries = ROUTES.filter((r) =>
    r.section && (!r.service || available.has(r.service)) && (!r.requires || available.has(r.requires)));

  // One card per product, not per page, matching the drawer.
  const products = [];
  const seen = new Set();
  for (const page of entries) {
    const key = productKey(page);
    if (seen.has(key)) continue;
    seen.add(key);
    products.push(page);
  }

  const groups = new Map();
  for (const entry of products) {
    if (!groups.has(entry.section)) groups.set(entry.section, []);
    groups.get(entry.section).push(entry);
  }

  const filter = el("input", {
    class: "filter", type: "search", placeholder: "Filter products",
    "aria-label": "Filter products",
  });
  const grid = el("div", { class: "catalogue" });

  const draw = () => {
    const q = filter.value.trim().toLowerCase();
    const matches = (entry) =>
      !q || productTitle(entry).toLowerCase().includes(q) ||
      entry.section.toLowerCase().includes(q) ||
      pagesOf(productKey(entry)).some((page) => page.title.toLowerCase().includes(q));

    const shown = [...groups]
      .map(([section, items]) => [section, items.filter(matches)])
      .filter(([, items]) => items.length);

    if (!shown.length) {
      return setChildren(grid,
        el("div", { class: "state state-inline" },
          el("h2", { text: "No matching products" }),
          el("p", { text: `Nothing matches “${q}”.` }),
          el("button", { class: "secondary", text: "Clear filter",
                         onclick: () => { filter.value = ""; draw(); } })));
    }

    setChildren(grid, ...shown.map(([section, items]) =>
      el("section", { class: "catalogue-group" },
        el("h2", {},
          CATEGORY_ICONS[section]
            ? el("span", { class: "nav-icon" }, lineMark(CATEGORY_ICONS[section]))
            : null,
          el("span", { text: section })),
        el("ul", {}, ...items.map((entry) => {
          const key = productKey(entry);
          const pinned = isPinned(key);
          return el("li", { class: "catalogue-item" },
            el("a", { href: entry.path + scopeSearch() },
              el("span", { class: "nav-icon" }, markFor(entry)),
              el("span", {}, el("span", { class: "catalogue-name", text: productTitle(entry) }),
                pagesOf(key).length > 1
                  ? el("span", { class: "catalogue-pages",
                                 text: pagesOf(key).map((pg) => pg.title).join(" · ") })
                  : null)),
            el("button", {
              class: "nav-pin" + (pinned ? " is-pinned" : ""),
              "aria-label": (pinned ? "Unpin " : "Pin ") + productTitle(entry),
              "aria-pressed": pinned ? "true" : "false",
              title: pinned ? "Unpin" : "Pin",
              onclick: () => { togglePinned(key); buildNav(SERVICES); draw(); },
            }, el("span", { html: `<svg viewBox="0 0 24 24" aria-hidden="true">${PIN_ICON}</svg>` })));
        })))));
  };

  filter.addEventListener("input", draw);
  draw();

  setChildren(view,
    pageHeader("All products", "Every product this instance emulates. Pin one to keep it at the top of the menu."),
    el("div", { class: "filter-bar" }, filter),
    grid);
  announce(`${products.length} products`);
}

// --- states ----------------------------------------------------------
//
// Four distinct states, because each is separately easy to get wrong and an
// error rendered as an empty table is the one that costs a developer an hour.

// A skeleton that eventually says something.
//
// Left alone it shimmers forever, and a shimmer is indistinguishable from
// progress. After a few seconds it names what it is waiting for and offers a
// way out, which is the escalation the console this mirrors performs.
const STILL_LOADING_MS = 8000;

const loadingState = (rows = 5, opts = {}) => {
  const skeleton = el("div", { class: "skeleton", "aria-label": "Loading", role: "status" },
    Array.from({ length: rows }, () => el("div")));
  if (!opts.what) return skeleton;

  const note = el("p", { class: "still-loading", hidden: true, role: "status" },
    el("span", { text: `Still loading ${opts.what}…` }),
    opts.onCancel
      ? el("button", { class: "secondary", text: "Cancel", onclick: opts.onCancel })
      : null);
  const wrap = el("div", { class: "loading" }, skeleton, note);
  // Checked rather than cleared: the skeleton is replaced wholesale when the
  // data arrives, so there is nothing left to hold a handle on.
  setTimeout(() => { if (wrap.isConnected) note.hidden = false; }, STILL_LOADING_MS);
  return wrap;
};

const emptyState = (title, hint) =>
  el("div", { class: "state" },
    el("h2", { text: title }),
    el("p", { text: hint }));

// pageHeader is the block every product screen opens with.
//
// Three screens emitted a bare h1 instead, so the title sat at a different
// height with no rule under it and no room for a subtitle — the kind of drift
// that makes one console feel like three applications.
//
// The Google Cloud console carries a screen's own actions on the title's line
// (#1842): "Buckets  + Create  Refresh" with the info-panel toggle at the
// right on a list, "<Kind> details  Edit  Delete" with a back arrow on a
// resource. opts.back is the address the arrow returns to; opts.actions are
// the controls on the title line. headerActions(header) is the slot itself,
// for a screen that adds its controls once its data has arrived.
const ICON_BACK = '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M20 12H5M11 6l-6 6 6 6"/></svg>';
const ICON_ADD = '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M12 5v14M5 12h14"/></svg>';
const ICON_REFRESH = '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M19 12a7 7 0 1 1-2.05-4.95"/><path d="M19 4v4h-4"/></svg>';

function backArrow(href, label = "Back") {
  return el("a", { class: "icon-button back-arrow", href, "aria-label": label, title: label, html: ICON_BACK });
}

function pageHeader(title, subtitle, opts = {}) {
  return el("div", { class: "page-header" },
    el("div", { class: "page-title-row" },
      opts.back ? backArrow(opts.back, opts.backLabel) : null,
      el("h1", { text: title }),
      el("div", { class: "page-header-actions" }, ...(opts.actions || []))),
    subtitle ? el("p", { class: "subtitle", text: subtitle }) : null);
}

// headerActions is the title-line slot of a screen's header, added to a
// header drawn without one.
function headerActions(header) {
  const node = (Array.isArray(header) ? header : [header])
    .find((n) => n && n.classList && n.classList.contains("page-header"));
  if (!node) return null;
  let slot = node.querySelector(".page-header-actions");
  if (slot) return slot;
  let row = node.querySelector(".page-title-row");
  if (!row) {
    const h1 = node.querySelector("h1");
    row = el("div", { class: "page-title-row" });
    if (h1) { h1.replaceWith(row); row.append(h1); } else node.prepend(row);
  }
  slot = el("div", { class: "page-header-actions" });
  row.append(slot);
  return slot;
}

// createButton is a list's Create on the title line, with Google's plus.
function createButton(label, onclick) {
  // "primary" marks it as the screen's main action for scripts and
  // assistive tooling; it is drawn as Google's text button.
  const button = el("button", { class: "header-button primary", type: "button", onclick });
  button.innerHTML = ICON_ADD;
  button.append(el("span", { text: label }));
  return button;
}

// refreshButton reloads, and says it is working for as long as it is.
function refreshButton(run) {
  const button = el("button", { class: "header-button", type: "button" });
  button.innerHTML = ICON_REFRESH;
  button.append(el("span", { text: "Refresh" }));
  button.addEventListener("click", async () => {
    setBusy(button, true);
    try { await run(); } finally { setBusy(button, false); }
  });
  return button;
}

// noteBanner is what a screen says about its emulation (#1842). Google's
// list pages open on the table, with at most a dismissible banner, so the
// note is a banner with a Dismiss control, remembered per screen for this
// tab, instead of a paragraph pushing the table down. The text stays in the
// info panel too (drawInfoPanel reads LAST_NOTE).
const DISMISSED_NOTES_KEY = "cloudburrow.notes.dismissed";
let LAST_NOTE = "";

function noteBanner(text, key = location.pathname) {
  LAST_NOTE = text || "";
  if (!text) return null;
  let dismissed = [];
  try { dismissed = JSON.parse(sessionStorage.getItem(DISMISSED_NOTES_KEY) || "[]"); } catch { /* refused */ }
  const id = `${key}\n${text}`;
  if (dismissed.includes(id)) return null;
  const banner = el("div", { class: "note-banner", role: "note" },
    el("span", { class: "note-icon", "aria-hidden": "true",
      html: '<svg viewBox="0 0 24 24"><circle cx="12" cy="12" r="9"/><path d="M12 11v5M12 8v.01"/></svg>' }),
    el("p", { class: "unavailable note-text", text }),
    el("button", { type: "button", class: "icon-button note-dismiss", "aria-label": "Dismiss this note",
      title: "Dismiss",
      html: '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M6 6l12 12M18 6L6 18"/></svg>',
      onclick: () => {
        try {
          dismissed.push(id);
          sessionStorage.setItem(DISMISSED_NOTES_KEY, JSON.stringify(dismissed.slice(-100)));
        } catch { /* storage refused: dismissed for this render only */ }
        banner.remove();
        syncStickyOffsets();
      } }));
  return banner;
}

// statusIcon is a row's state as Google draws it (#1842): an icon in the
// first column, its text for assistive technology and in the tooltip.
const STATUS_GLYPHS = {
  ok: '<svg class="status-glyph" viewBox="0 0 24 24" aria-hidden="true"><circle cx="12" cy="12" r="9"/><path d="M8 12.5l2.7 2.7L16 9.8"/></svg>',
  warn: '<svg class="status-glyph" viewBox="0 0 24 24" aria-hidden="true"><path d="M12 3l9.5 17h-19z"/><path d="M12 10v4M12 17v.01"/></svg>',
  error: '<svg class="status-glyph" viewBox="0 0 24 24" aria-hidden="true"><circle cx="12" cy="12" r="9"/><path d="M9 9l6 6M15 9l-6 6"/></svg>',
  "": '<svg class="status-glyph" viewBox="0 0 24 24" aria-hidden="true"><circle cx="12" cy="12" r="9"/><path d="M12 8v4M12 16v.01"/></svg>',
};

function statusIcon(status) {
  const state = stateOf(status);
  const node = el("span", { class: "status status-icon", "data-state": state, title: status || "No status" });
  node.innerHTML = status ? STATUS_GLYPHS[state] || STATUS_GLYPHS[""] : "";
  node.append(el("span", { class: "sr-only", text: status || "—" }));
  return node;
}

// filterBar is every list table's filter (#1842), as Google's list toolbar
// draws it: "Filter <noun>", where choosing a property and typing a value
// makes a chip, chips are joined by AND, and plain text matches any column.
// Typing narrows the rows as it is typed; Enter keeps it as a chip.
//
//   const f = filterBar({ noun, columns: () => ["Name", "Location"], onChange: draw });
//   f.node            the bar, to put above the table
//   f.matches(get)    whether a row passes: get(column) is the row's value
//                     in a column, get(null) every value it has
//   f.active()        whether anything is filtering
//   f.clear()         drops every chip and the typed text
//   f.input           the text box (input.filter)
function filterBar({ noun, columns, onChange, extra = [] }) {
  let chips = [];
  const input = el("input", {
    class: "filter", type: "search", placeholder: `Filter ${noun}`,
    "aria-label": `Filter ${noun}`, autocomplete: "off",
    "aria-haspopup": "listbox", "aria-expanded": "false",
  });
  const chipRow = el("div", { class: "filter-chips", role: "list", "aria-label": "Filters" });
  const menu = el("ul", { class: "filter-menu", role: "listbox", "aria-label": "Filter by property", hidden: true });

  const parse = (text) => {
    const m = /^([^:]+):\s*(.*)$/.exec(text);
    if (m) {
      const col = columns().find((c) => c.toLowerCase() === m[1].trim().toLowerCase());
      if (col) return { column: col, value: m[2].trim() };
    }
    return { column: null, value: text.trim() };
  };
  const closeMenu = () => { menu.hidden = true; input.setAttribute("aria-expanded", "false"); };
  const openMenu = () => {
    if (input.value.trim()) return closeMenu();
    setChildren(menu,
      el("li", { class: "filter-menu-title", role: "presentation", text: "Properties" }),
      ...columns().map((c) => el("li", { role: "presentation" },
        el("button", { type: "button", role: "option", class: "filter-menu-item", text: c,
          onmousedown: (e) => e.preventDefault(),
          onclick: () => { input.value = `${c}: `; closeMenu(); input.focus(); } }))));
    menu.hidden = false;
    input.setAttribute("aria-expanded", "true");
  };
  const drawChips = () => {
    setChildren(chipRow,
      ...chips.map((c, i) => el("span", { class: "chip", role: "listitem" },
        el("span", { text: `${c.column || "Filter"}: ${c.value}` }),
        el("button", { type: "button", class: "chip-clear", "aria-label": `Remove filter ${c.column || ""} ${c.value}`.replace(/\s+/g, " "),
          html: '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M6 6l12 12M18 6L6 18"/></svg>',
          onclick: () => { chips.splice(i, 1); drawChips(); onChange(); input.focus(); } }))),
      chips.length ? el("button", { type: "button", class: "chip-clear-all", text: "Clear filters",
        onclick: () => { chips = []; drawChips(); onChange(); input.focus(); } }) : null);
  };
  input.addEventListener("focus", openMenu);
  input.addEventListener("click", openMenu);
  input.addEventListener("blur", () => setTimeout(closeMenu, 150));
  input.addEventListener("input", () => { if (input.value.trim()) closeMenu(); else openMenu(); onChange(); });
  input.addEventListener("keydown", (e) => {
    if (e.key === "Escape") { closeMenu(); return; }
    if (e.key === "ArrowDown" && !menu.hidden) {
      e.preventDefault();
      const first = menu.querySelector("button");
      if (first) first.focus();
      return;
    }
    if (e.key !== "Enter") return;
    e.preventDefault();
    const chip = parse(input.value);
    if (!chip.value) return;
    chips.push(chip);
    input.value = "";
    drawChips();
    onChange();
    announce(`Filter ${chip.column || ""} ${chip.value} added`);
  });
  menu.addEventListener("keydown", (e) => {
    const items = [...menu.querySelectorAll("button")];
    const at = items.indexOf(document.activeElement);
    if (e.key === "ArrowDown" && at < items.length - 1) { e.preventDefault(); items[at + 1].focus(); }
    if (e.key === "ArrowUp") { e.preventDefault(); if (at > 0) items[at - 1].focus(); else input.focus(); }
    if (e.key === "Escape") { closeMenu(); input.focus(); }
  });

  const test = (chip, get) => {
    const q = chip.value.toLowerCase();
    if (!q) return true;
    const values = chip.column ? [get(chip.column)] : get(null);
    return values.some((v) => String(v || "").toLowerCase().includes(q));
  };
  return {
    input,
    node: el("div", { class: "filter-field" },
      el("span", { class: "filter-icon", "aria-hidden": "true",
        html: '<svg viewBox="0 0 24 24"><path d="M4 6h16M7 12h10M10 18h4"/></svg>' }),
      el("div", { class: "filter-box" }, input, menu),
      ...extra),
    chips: chipRow,
    matches: (get) => [...chips, parse(input.value)].every((c) => test(c, get)),
    active: () => chips.length > 0 || Boolean(input.value.trim()),
    text: () => [...chips.map((c) => `${c.column ? c.column + ": " : ""}${c.value}`), input.value.trim()]
      .filter(Boolean).join(" AND "),
    clear: () => { chips = []; input.value = ""; drawChips(); },
  };
}

// viewOptions is the column chooser beside the filter (#1842), Google's
// "View options" / "Column display options": a checkbox per data column,
// remembered per service. hidden() is the set of columns turned off.
const COLUMNS_KEY = "cloudburrow.columns";

function viewOptions({ service, columns, onChange }) {
  const stored = readStored(COLUMNS_KEY, {}) || {};
  let off = new Set(Array.isArray(stored[service]) ? stored[service] : []);
  const button = el("button", { type: "button", class: "icon-button view-options",
    "aria-label": "View options", title: "View options", "aria-haspopup": "true", "aria-expanded": "false",
    html: '<svg viewBox="0 0 24 24" aria-hidden="true"><rect x="3" y="5" width="18" height="14" rx="2"/><path d="M9 5v14M15 5v14"/></svg>' });
  const panel = el("div", { class: "view-options-menu", role: "group", "aria-label": "Column display options", hidden: true });
  const save = () => {
    const all = readStored(COLUMNS_KEY, {}) || {};
    all[service] = [...off];
    writeStored(COLUMNS_KEY, all);
  };
  const draw = () => setChildren(panel,
    el("p", { class: "view-options-title", text: "Column display options" }),
    ...columns().map((c) => {
      const id = `col-${c.replace(/[^A-Za-z0-9]+/g, "-")}`;
      return el("label", { class: "view-options-item", for: id },
        el("input", { type: "checkbox", id, checked: off.has(c) ? null : "checked",
          onchange: (e) => {
            if (e.target.checked) off.delete(c); else off.add(c);
            save();
            onChange();
          } }),
        el("span", { text: c }));
    }));
  const close = () => { panel.hidden = true; button.setAttribute("aria-expanded", "false"); };
  button.addEventListener("click", () => {
    if (!panel.hidden) return close();
    draw();
    panel.hidden = false;
    button.setAttribute("aria-expanded", "true");
    const first = panel.querySelector("input");
    if (first) first.focus();
  });
  panel.addEventListener("keydown", (e) => { if (e.key === "Escape") { close(); button.focus(); } });
  document.addEventListener("click", (e) => {
    if (!panel.hidden && !panel.contains(e.target) && e.target !== button && !button.contains(e.target)) close();
  });
  return {
    node: el("div", { class: "view-options-wrap" }, button, panel),
    hidden: () => off,
  };
}

// A cancel is not a failure: the user stopped it.
//
// Rendered as its own state, because the error card says "the console could
// not read this from the local instance" — which would be reporting a fault
// that did not happen, on a screen the user deliberately stopped.
const cancelledState = (title, retry) =>
  el("div", { class: "state" },
    el("h2", { text: title }),
    el("p", { text: "You stopped this request before it finished." }),
    el("button", { class: "secondary", onclick: retry, text: "Try again" }));

const isCancelled = (err) => String(err && err.message) === "cancelled";

const errorState = (title, detail, retry) =>
  el("div", { class: "state error", role: "alert" },
    el("h2", { text: title }),
    el("p", { text: "The console could not read this from the local instance." }),
    el("pre", { text: detail }),
    retry ? el("button", { class: "secondary", onclick: retry, text: "Retry" }) : null);

// --- screens ---------------------------------------------------------

// The cluster utilisation panel.
//
// The numbers are the cluster's own: capacity from the node object, usage from
// the kubelet's summary. Nothing here is estimated, and when the reading
// fails the panel says so rather than drawing an empty bar that reads as
// "idle".
function meterRow(label, used, total, format) {
  const pct = total > 0 ? Math.min(100, (used / total) * 100) : 0;
  return el("div", { class: "meter" },
    el("div", { class: "meter-head" },
      el("span", { text: label }),
      el("span", { class: "meter-value", text: `${format(used)} / ${format(total)}` })),
    el("div", {
      class: "meter-track", role: "meter", "aria-label": label,
      "aria-valuenow": String(Math.round(pct)), "aria-valuemin": "0", "aria-valuemax": "100",
    },
      el("div", { class: "meter-fill" + (pct >= 90 ? " is-high" : ""),
                  style: `width:${pct.toFixed(1)}%` })),
    el("div", { class: "meter-pct", text: `${pct.toFixed(1)}%` }));
}

const formatCores = (n) => `${n.toFixed(2)} vCPU`;
const formatBytes = (n) => {
  if (!n) return "0 B";
  const units = ["B", "KiB", "MiB", "GiB", "TiB"];
  let i = 0, v = n;
  while (v >= 1024 && i < units.length - 1) { v /= 1024; i++; }
  return `${v.toFixed(v < 10 ? 1 : 0)} ${units[i]}`;
};

// The last reading that actually worked.
//
// Every tick replaced the panel wholesale, so one transient blip — a kubelet
// restart, a momentary timeout — erased a working reading and put an error in
// its place. On a healthy cluster the panel flickered between numbers and a
// failure message. A reading that never moves is worse than none; a reading
// that keeps vanishing is the same problem from the other side.
let LAST_METRICS = null;

// --- charts -----------------------------------------------------------
//
// An inline SVG, drawn from points the server retained. There is no charting
// library here for the same reason there is no framework: the assets ship as
// they are written, and "no external CDN after installation" is true by
// construction rather than by a bundler configuration nobody checks.
//
// The one rule this drawing obeys: a gap is drawn as a gap. A reading that
// failed is not joined to the readings either side, because a straight line
// across a period nobody measured is the chart inventing the thing it exists
// to report.

const CHART_W = 600;
const CHART_H = 120;

function sparkline(points, opts = {}) {
  // A known ceiling — a node's capacity — is the honest scale: it says how
  // much headroom there is, not just how the value moved. Without one the
  // series scales to itself, with a margin so the line is not drawn along the
  // top edge where it reads as saturated.
  const peak = Math.max(...points.map((p) => p.value || 0), 0);
  const max = opts.max || (peak > 0 ? peak * 1.25 : 1);
  const span = points.length > 1 ? points.length - 1 : 1;
  const x = (i) => (i / span) * CHART_W;
  const y = (v) => CHART_H - (Math.min(v, max) / max) * CHART_H;

  // One path per run of consecutive readings. A missing sample ends the run.
  const runs = [];
  let run = [];
  points.forEach((p, i) => {
    if (p.value === null || p.value === undefined) {
      if (run.length) runs.push(run);
      run = [];
      return;
    }
    run.push(`${run.length ? "L" : "M"}${x(i).toFixed(1)},${y(p.value).toFixed(1)}`);
  });
  if (run.length) runs.push(run);

  const svg = document.createElementNS("http://www.w3.org/2000/svg", "svg");
  svg.setAttribute("viewBox", `0 0 ${CHART_W} ${CHART_H}`);
  svg.setAttribute("preserveAspectRatio", "none");
  svg.setAttribute("class", "chart");
  svg.setAttribute("role", "img");
  svg.setAttribute("aria-label", opts.label || "chart");

  for (const r of runs) {
    // A single point has no line; a dot is what one reading looks like.
    if (r.length === 1) {
      const dot = document.createElementNS("http://www.w3.org/2000/svg", "circle");
      const [, cx, cy] = r[0].match(/M([\d.]+),([\d.]+)/);
      dot.setAttribute("cx", cx); dot.setAttribute("cy", cy); dot.setAttribute("r", "2");
      dot.setAttribute("class", "chart-point");
      svg.append(dot);
      continue;
    }
    const path = document.createElementNS("http://www.w3.org/2000/svg", "path");
    path.setAttribute("d", r.join(" "));
    path.setAttribute("class", "chart-line");
    svg.append(path);
  }
  return svg;
}

// chartCard is a titled chart with its own window and source stated.
//
// Both are on screen rather than assumed: a chart that does not say what it
// covers or where the numbers came from is a picture, not a measurement.
function chartCard(title, points, opts = {}) {
  const readings = points.filter((p) => p.value !== null && p.value !== undefined);
  const body = readings.length < 2
    ? el("p", { class: "unavailable",
        text: readings.length === 1
          ? "Not enough history yet — one reading so far."
          : "Not enough history yet." })
    : sparkline(points, opts);

  return el("div", { class: "chart-card" },
    el("div", { class: "chart-head" },
      el("h3", { text: title }),
      el("span", { class: "chart-now", text: opts.current || "" })),
    body,
    el("p", { class: "chart-foot unavailable", text: opts.foot || "" }));
}

// seriesPoints turns the retained samples into one metric's points.
//
// A sample the server recorded as unavailable becomes a null, which the
// drawing turns into a gap rather than a line.
function seriesPoints(samples, pick) {
  return samples.map((s) => {
    if (s.unavailable || !(s.nodes || []).length) return { at: s.at, value: null };
    const value = pick(s.nodes[0]);
    // undefined is a field the node did not report, which is a gap; 0 is a
    // reading of zero, which is a value. Collapsing the two would draw a
    // missing filesystem as an empty one.
    return { at: s.at, value: value === undefined || value === null ? null : value };
  });
}

// ratepoints turns a cumulative counter into a per-second rate.
//
// A node's network and filesystem counters only ever go up, so charting them raw
// draws a line that climbs forever and says nothing: "12 GB received since boot"
// is not an answer to "is the network busy". The rate between two readings is.
function ratePoints(samples, pick) {
  const out = [];
  let prev = null;
  for (const s of samples) {
    const usable = !s.unavailable && (s.nodes || []).length;
    const value = usable ? pick(s.nodes[0]) : undefined;
    const at = new Date(s.at);
    if (!usable || value === undefined || value === null || !prev) {
      out.push({ at: s.at, value: null });
      // A gap must not become the baseline for the next rate, or the next point
      // would be the whole missing span divided by one interval.
      prev = usable && value !== undefined && value !== null ? { at, value } : null;
      continue;
    }
    const seconds = (at - prev.at) / 1000;
    // A counter that went backwards is a reset — the interface was recreated —
    // and the difference is not a rate.
    if (seconds <= 0 || value < prev.value) {
      out.push({ at: s.at, value: null });
    } else {
      out.push({ at: s.at, value: (value - prev.value) / seconds });
    }
    prev = { at, value };
  }
  return out;
}

// RANGES are the windows the Monitoring screen can show.
//
// The series holds an hour. Charting all of it always meant a spike thirty
// seconds ago was three pixels wide, so the question "what just happened" was
// the one the chart was worst at.
const RANGES = [
  { label: "5 min", seconds: 5 * 60 },
  { label: "15 min", seconds: 15 * 60 },
  { label: "1 hour", seconds: 60 * 60 },
  { label: "All", seconds: 0 },
];
const RANGE_KEY = "cloudburrow.monitoring.range";

// withinRange drops the samples older than the selected window.
function withinRange(samples, seconds) {
  if (!seconds || !samples.length) return samples;
  const cutoff = new Date(samples[samples.length - 1].at).getTime() - seconds * 1000;
  return samples.filter((s) => new Date(s.at).getTime() >= cutoff);
}

function renderMetrics(target, m) {
  if (!m.unavailable) LAST_METRICS = { data: m, at: new Date() };

  // Only when there has never been a successful reading is there nothing to
  // show. Anything else keeps the numbers and says how old they are.
  if (m.unavailable && !LAST_METRICS) {
    setChildren(target,
      el("h2", { text: "Cluster utilisation" }),
      el("p", { class: "unavailable", text: m.unavailable }));
    return;
  }

  const shown = m.unavailable ? LAST_METRICS.data : m;
  const at = LAST_METRICS.at;
  target.classList.toggle("is-stale", Boolean(m.unavailable));

  setChildren(target,
    el("h2", { text: "Cluster utilisation" }),
    ...(shown.nodes || []).flatMap((n) => [
      el("div", { class: "meter-node" },
        el("span", { class: "mono", text: n.name }),
        el("span", { class: "status", "data-state": n.ready ? "ok" : "error" },
          el("span", { text: n.ready ? "Ready" : "Not ready" })),
        el("span", { class: "unavailable", text: `${n.pods} pods` })),
      meterRow("CPU", n.cpuUsedCores || 0, n.cpuCapacityCores || 0, formatCores),
      meterRow("Memory", n.memoryUsedBytes || 0, n.memoryTotalBytes || 0, formatBytes),
    ]),
    // Relative, because a formatted clock time looks equally fresh at five
    // seconds and five minutes old — which is exactly the distinction the
    // line exists to make.
    m.unavailable
      ? el("p", { class: "panel-empty metrics-stale", role: "status",
          text: `Last reading ${relativeTime(at)} — refresh failed: ${m.unavailable}` })
      : el("p", { class: "panel-empty", text: `Updated ${relativeTime(at)}` }));
}

// QUICK_ACCESS is the Welcome page's products, in the order Google's Quick
// access lists the ones CloudBurrow serves (#1842).
const QUICK_ACCESS = ["/apis", "/iam-admin/iam", "/storage/browser", "/bigquery", "/kubernetes/workloads",
  "/run", "/cloudsql-instances", "/pubsub/topics", "/logs", "/secrets", "/kms/keyrings", "/firestore-databases"];
// WELCOME_RESOURCES are the resource counts the Resources card reads.
const WELCOME_RESOURCES = ["storage", "bigquery", "run", "pubsub", "secrets", "cloudsql-instances",
  "memorystore", "functions", "tasks", "spanner"];

function welcomeCards(project) {
  const served = new Set(SERVICES.map((sv) => sv.id));
  const isServed = (r) => r && (!r.service || served.has(r.service));
  const routeAt = (path) => ROUTES.find((r) => r.path === path);

  const info = el("div", { class: "card welcome-project", id: "welcome-project" },
    el("h2", { text: "Project info" }),
    el("p", { class: "unavailable", text: project ? "reading…" : "No project is selected." }));
  if (project) {
    api("/api/resources/projects").then((data) => {
      const it = (data.items || []).find((i) => i.name === project);
      const f = (it && it.fields) || {};
      setChildren(info,
        el("h2", { text: "Project info" }),
        el("dl", {},
          el("dt", { text: "Project name" }), el("dd", { text: f.Name || project }),
          el("dt", { text: "Project number" }), el("dd", { class: "mono", text: f.Number || "—" }),
          el("dt", { text: "Project ID" }), el("dd", { class: "mono", text: project })),
        el("div", { class: "card-actions" },
          el("a", { href: `/projects/${encodeURIComponent(project)}${scopeSearch()}`, text: "Go to project settings" })));
    }).catch((err) => {
      setChildren(info, el("h2", { text: "Project info" }), el("p", { class: "unavailable", text: String(err.message) }));
    });
  }

  const quick = QUICK_ACCESS.map(routeAt).filter(isServed);
  const access = el("div", { class: "card welcome-quick" },
    el("h2", { text: "Quick access" }),
    el("ul", { class: "quick-access" },
      ...quick.map((r) => el("li", {},
        el("a", { href: r.path + scopeSearch(), class: "quick-tile" },
          el("span", { text: productTitle(r) })))),
      el("li", {}, el("a", { href: "/products", class: "quick-tile" }, el("span", { text: "View all products" })))));

  const resources = el("ul", { class: "welcome-resources" });
  const resourceCard = el("div", { class: "card welcome-resources-card" },
    el("h2", { text: "Resources" }), resources);
  const counted = WELCOME_RESOURCES.filter((id) => served.has(id));
  if (!project || !counted.length) {
    resources.replaceWith(el("p", { class: "unavailable", text: "No resources to count." }));
  }
  for (const id of project ? counted : []) {
    const r = ROUTES.find((x) => x.service === id && !x.screen);
    if (!r) continue;
    const row = el("li", {},
      el("a", { href: r.path + scopeSearch(), text: `${productTitle(r)}${r.productTitle ? ": " + r.title : ""}` }),
      el("span", { class: "unavailable", text: " …" }));
    resources.append(row);
    api(`/api/resources/${id}?project=${encodeURIComponent(project)}`).then((data) => {
      const n = data.prompt || data.unavailable ? null : (data.total ?? (data.items || []).length);
      row.lastChild.textContent = n === null ? " —" : ` ${n}`;
    }).catch(() => { row.lastChild.textContent = " —"; });
  }

  const activity = el("div", { class: "card welcome-activity" },
    el("h2", { text: "Activity" }),
    el("p", { class: "panel-empty", text: "What was created, changed and deleted on this instance." }),
    el("div", { class: "card-actions" },
      el("a", { href: "/activity" + scopeSearch(), text: "View all activity" }),
      el("a", { href: "/requests" + scopeSearch(), text: "Request Log" })));

  // APIs: the project's enabled APIs, from Service Usage, as Google's
  // Dashboard links to the APIs overview.
  const apisCount = el("span", { text: "…" });
  const apis = served.has("serviceusage") && project
    ? el("div", { class: "card welcome-apis" },
        el("h2", { text: "APIs" }),
        el("p", {}, apisCount, " enabled APIs and services"),
        el("div", { class: "card-actions" },
          el("a", { href: "/apis" + scopeSearch(), text: "Go to APIs overview" })))
    : null;
  if (apis) {
    api(`/api/resources/serviceusage?project=${encodeURIComponent(project)}`).then((data) => {
      apisCount.textContent = data.prompt || data.unavailable ? "—" : String(data.total ?? (data.items || []).length);
    }).catch(() => { apisCount.textContent = "—"; });
  }

  return el("div", { class: "cards welcome-cards" }, info, resourceCard, access, apis, activity);
}

async function renderDashboard(view) {
  const cancel = new AbortController();
  setChildren(view, 
    el("h1", { text: "Welcome" }),
    el("p", { class: "subtitle", text: "Live state of this CloudBurrow instance." }),
    loadingState(3, { what: "instance status", onCancel: () => cancel.abort() })
  );

  let status;
  try {
    status = await api("/api/status", { signal: cancel.signal });
  } catch (err) {
    setChildren(view, 
      el("h1", { text: "Welcome" }),
      isCancelled(err)
        ? cancelledState("Dashboard not loaded", () => renderDashboard(view))
        : errorState("Instance status unavailable", String(err.message),
                     () => renderDashboard(view))
    );
    return;
  }

  const readyState = status.ready ? "ok" : "warn";
  const cards = el("div", { class: "cards" },
    el("div", { class: "card" },
      el("h2", { text: "Instance" }),
      el("dl", {},
        el("dt", { text: "Name" }), el("dd", { text: status.instance || "unknown" }),
        el("dt", { text: "State" }),
        el("dd", {}, el("span", { class: "status", "data-state": readyState },
          el("span", { text: status.state || (status.ready ? "ready" : "starting") }))),
        el("dt", { text: "Mode" }), el("dd", { text: status.mode || "unknown" })
      )),
    el("div", { class: "card" },
      el("h2", { text: "Cluster" }),
      el("dl", {},
        el("dt", { text: "Name" }), el("dd", { text: status.cluster || "unknown" }),
        el("dt", { text: "Kubernetes" }), el("dd", { text: status.kubernetes || "unknown" }),
        el("dt", { text: "Namespace" }), el("dd", { text: status.namespace || "unknown" })
      ))
  );

  const endpoints = Object.entries(status.endpoints || {}).sort();
  cards.append(
    el("div", { class: "card" },
      el("h2", { text: "Endpoints" }),
      endpoints.length
        ? el("dl", {}, endpoints.flatMap(([name, addr]) =>
            [el("dt", { text: name }), el("dd", { text: addr })]))
        : el("p", { class: "unavailable", text: "No endpoints are published." }))
  );

  const services = (status.services || []).map((s) =>
    el("li", {},
      el("span", { class: "status", "data-state": s.enabled ? "ok" : "" },
        el("span", { text: s.title || s.id })),
      s.enabled ? null : el("span", { class: "unavailable", text: ` — ${s.reason || "not enabled"}` }))
  );

  const utilisation = el("div", { class: "card utilisation" },
    el("h2", { text: "Cluster utilisation" }),
    el("p", { class: "unavailable", text: "reading…" }));

  // Component health, read now rather than latched at startup.
  //
  // The card used to render a boolean captured when the instance came up, so
  // a tunnel whose pod had gone away still read as ready — the dashboard
  // answered "did this ever work" while looking like it answered "is this
  // working".
  const components = el("div", { class: "card components" },
    el("h2", { text: "Components" }),
    el("p", { class: "unavailable", text: "reading…" }));

  const drawComponents = (st) => {
    const tunnels = st.tunnels || [];
    const named = Object.entries(st.components || {});
    if (!tunnels.length && !named.length) {
      return setChildren(components,
        el("h2", { text: "Components" }),
        el("p", { class: "unavailable", text: "This instance reports no components." }));
    }
    setChildren(components,
      el("h2", { text: "Components" }),
      el("ul", { class: "component-list" },
        ...named.map(([name, ready]) =>
          el("li", {},
            el("span", { class: "status", "data-state": ready ? "ok" : "warn" },
              el("span", { text: name })))),
        ...tunnels.map((t) =>
          el("li", {},
            el("span", { class: "status", "data-state": t.running ? "ok" : "error" },
              el("span", { text: t.name })),
            el("span", { class: "unavailable mono", text: t.host || "" }),
            // A tunnel that has been re-established is not the same as one
            // that never had to be. Silence about it is how a flapping
            // backend stays invisible.
            t.restarts
              ? el("span", { class: "status", "data-state": "warn" },
                  el("span", { text: `${t.restarts} restart${t.restarts === 1 ? "" : "s"}` }))
              : null))));
  };
  drawComponents(status);

  // Google's Welcome page (#1842): the project you are working in, quick
  // access to products, the project's resources and its activity, before
  // this instance's own state, which Google's console has no counterpart
  // for. https://support.google.com/cloud/answer/3465889
  const project = currentProject();
  setChildren(view,
    el("div", { class: "page-header" },
      el("h1", { text: "Welcome" }),
      el("p", { class: "subtitle", text: project ? `You're working in ${project}` : "Live state of this CloudBurrow instance." })),
    welcomeCards(project),
    el("h2", { class: "section-heading", text: "Instance" }),
    cards,
    el("div", { class: "cards", style: "margin-top:16px" },
      utilisation,
      components,
      el("div", { class: "card" },
        el("h2", { text: "Services" }),
        services.length
          ? el("ul", { style: "list-style:none;margin:0;padding:0" }, services)
          : el("p", { class: "unavailable", text: "No services are enabled." })))
  );

  // Live rather than a snapshot: utilisation that never moves is worse than
  // none, because it reads as a measurement. The timer is cleared by the
  // router when the screen changes, so leaving the dashboard stops the polling
  // instead of leaving it running against a page nobody is looking at.
  const tick = async () => {
    try {
      renderMetrics(utilisation, await api("/api/metrics"));
    } catch (err) {
      renderMetrics(utilisation, { unavailable: String(err.message) });
    }
    // Component health moves on the same tick, for the same reason: a panel
    // that reports a state it captured once is not reporting health.
    try {
      drawComponents(await api("/api/status"));
    } catch { /* the utilisation panel above already says the instance is unreachable */ }
  };
  await tick();
  stopMetrics();
  METRICS_TIMER = setInterval(tick, 5000);

  // Handed to the one visibility listener rather than each dashboard adding
  // its own, which would leave a listener behind on every navigation.
  METRICS_TICK = tick;

  announce("Welcome page loaded");
}

// renderTableInto draws a listing: action bar, filter, sortable table, footer.
//
// Both the list screen and the detail screen call it, so there is one table
// implementation and a row's contents cannot be drawn differently from the row
// it came from. opts.rowControls turns on create, delete, selection and
// per-row actions, which belong to a list and not to the inside of one of its
// rows. opts.refetch, when given, lets the table reload its own data without
// the screen being rebuilt around it.
const PAGE_SIZES = [25, 50, 100];

function renderTableInto(view, header, data, noun, reload, route, opts = {}) {
  const caps = opts.rowControls ? capabilityOf(route.service) : {};
  // Actions on the page that take the checked rows, such as Compose on a
  // bucket's objects (#790). Only rows that are objects can be checked.
  // A top-level list's selection actions (#1864) are its ListActions,
  // sent with an empty path: Roles' Create role from selection.
  const selectActions = data.selectActions || [];
  const selectable = Boolean(opts.rowControls && caps.delete) || selectActions.length > 0;
  // Or a list screen's rows marked selectable: History's builds awaiting
  // approval, which its Approve takes (#1854). A top-level list's own
  // selection actions take any of its rows (#1864).
  const canSelect = (item) => !selectActions.length || Boolean(item.object) || Boolean(item.selectable) ||
    (!data.selectMarked && !(opts.pagePath || []).length);

  // All of the table's state lives here, so a refresh can put it back.
  let sortColumn = null;
  let sortAscending = true;
  let page = 0;
  let pageSize = PAGE_SIZES[0];
  let selected = new Set();
  // The row the info panel is describing. Separate from `selected`, which is
  // the multi-row selection a bulk delete acts on: "which one am I looking
  // at" and "which ones am I acting on" are different questions.
  let inspected = null;

  // Rows with a request outstanding against them. A row whose delete is in
  // flight looks identical to one that is idle unless something says so, and
  // the user's reading of "nothing happened" is a second click.
  const operating = new Set();

  const nameColumn = () => data.nameColumn || "Name";
  const allDataColumns = () => data.columns || [];
  // The data columns on screen: View options can turn any of them off
  // (#1842). The filter still reads the hidden ones, as Google's does.
  const chooser = opts.rowControls && route.service
    ? viewOptions({ service: route.service, columns: allDataColumns, onChange: () => draw() })
    : null;
  const dataColumns = () => allDataColumns().filter((c) => !chooser || !chooser.hidden().has(c));
  const hasActions = () =>
    data.items.some((i) => (i.actions || []).length || i.object) || caps.delete;
  // Declared by the provider, or inferred from the rows that happen to be
  // present. Inference alone made the column come and go as the data
  // changed, taking any sort applied to it with it.
  const hasStatus = () => Boolean(data.alwaysStatus || data.items.some((i) => i.status));
  // Status is the first column, as an icon, the way Google's lists draw it
  // (#1842); the row menu's column has no header text.
  const columns = () => [
    ...(selectable ? ["select"] : []),
    ...(hasStatus() ? ["Status"] : []),
    nameColumn(),
    ...dataColumns(),
    ...(hasActions() ? ["Actions"] : []),
  ];

  const valueOf = (item, column) => {
    if (column === nameColumn()) return item.name || "";
    if (column === "Status") return item.status || "";
    return (item.fields || {})[column] || "";
  };

  // The filter reads every column, not only the name. A filter that
  // silently ignored the columns beside it is worse than none: typing a
  // location and getting nothing reads as "there are none". A chip names
  // one column (#1842).
  const filterColumns = () => [nameColumn(), ...allDataColumns(), ...(hasStatus() ? ["Status"] : [])];
  const bar = filterBar({ noun, columns: filterColumns, onChange: () => { page = 0; draw(); } });
  const matches = (item) => bar.matches((column) => column === null
    ? filterColumns().map((c) => valueOf(item, c))
    : valueOf(item, column));

  const body = el("tbody");
  const headRow = el("tr");
  const footer = el("div", { class: "table-footer" });
  const bulk = el("button", {
    class: "secondary danger", text: "Delete", disabled: "disabled",
    onclick: () => deleteSelected(),
  });
  const selectionLabel = el("span", { class: "selection-count unavailable", text: "" });
  // In the order they were checked, which is the order Compose joins them.
  const selectionButtons = selectActions.map((a) => el("button", {
    class: "secondary", text: a.label, disabled: "disabled",
    onclick: () => {
      const byName = new Map(data.items.map((i) => [i.name, i]));
      const names = [...selected].map((n) => byName.get(n)).filter(Boolean)
        .map((i) => (i.object ? i.object[i.object.length - 1] : i.name));
      const fields = (a.fields || []).map((f) =>
        f.name === a.selectionField ? { ...f, default: names.join("\n") } : f);
      // The whole page again, whose summary counts the objects too.
      runAction(route, opts.pagePath || [], { ...a, fields }, () => reload());
    },
  }));

  const visibleRows = () => {
    let rows = data.items.filter((i) => matches(i));
    if (sortColumn) {
      // Compared numerically when both sides are numbers, so "10" does not
      // sort before "9", and case-insensitively otherwise.
      rows = [...rows].sort((a, b) => {
        const x = valueOf(a, sortColumn), y = valueOf(b, sortColumn);
        const nx = Number(x), ny = Number(y);
        const cmp = (x !== "" && y !== "" && !Number.isNaN(nx) && !Number.isNaN(ny))
          ? nx - ny
          : x.toLowerCase().localeCompare(y.toLowerCase());
        return sortAscending ? cmp : -cmp;
      });
    }
    return rows;
  };

  const deleteSelected = () => {
    const names = [...selected];
    if (!names.length) return;
    confirmDestructive({
      title: `Delete ${names.length} ${names.length === 1 ? noun.replace(/s$/, "") : noun}?`,
      detail: [names.join(", "), DELETE_DETAIL[route.service]].filter(Boolean).join("\n\n"),
      confirmWord: names.length === 1 ? names[0] : String(names.length),
      onConfirm: async () => {
        for (const name of names) operating.add(name);
        draw();
        try {
          for (const name of names) {
            await send(`/api/resources/${route.service}?project=` +
              `${encodeURIComponent(currentProject())}&name=${encodeURIComponent(name)}`, "DELETE");
            operating.delete(name);
            draw();
          }
        } finally {
          // A failure stops the loop, so the rows it never reached must not
          // be left looking busy forever.
          for (const name of names) operating.delete(name);
          draw();
        }
        selected = new Set();
        if (route.service === "projects") {
          window.dispatchEvent(new CustomEvent("cb-projects-changed"));
        }
        refresh();
      },
    });
  };

  // A handle the action functions use to say when a row's request starts and
  // stops. Redrawing is cheap and keeps one code path drawing rows.
  const rowBusy = (name) => ({
    start() { operating.add(name); draw(); },
    end() { operating.delete(name); draw(); },
  });

  // A row inside a resource page is addressed by the page's path plus its own
  // name, the way the server's PathActor reaches it. Addressed by name alone,
  // a subscription row's Delete on a topic page would reach the service's
  // top-level actions, which address a different resource or none (#595).
  // A list screen's pagePath is empty, and its rows keep their name.
  //
  // An object row is the exception: its page is addressed apart from the
  // folders beside it (an object and a folder can share a name), so its
  // actions go to the path it opens (#790). A row naming the path its
  // actions address — a Bigtable column family, beside rows whose keys could
  // be its name (#797), or a soft-deleted bucket or object, which is its name
  // and its generation (#789) — is addressed there.
  const rowTarget = (item) =>
    (item.actsOn || []).length ? item.actsOn
      : (item.target || []).length ? item.target
      : item.object && item.opens ? item.opens
      : (opts.pagePath || []).length ? [...opts.pagePath, item.name] : item.name;

  const rowActionsCell = (item) => {
    const busy = operating.has(item.name);
    const rowAction = (a) => () => runAction(route, rowTarget(item), a, refresh, rowBusy(item.name),
      (item.actsOn || []).length ? item.name : null);
    // Edit sink in a sink's menu (#1863): the form its page offers; first
    // where Google's menu begins with it, as a trigger's Edit, Disable,
    // Delete (#1854).
    const edit = caps.editRow ? [{
      label: caps.editRow,
      run: () => editFromRow(route, (item.opens || []).length ? item.opens : [item.name], refresh),
    }] : [];
    // A row's own buttons, before its menu: a trigger's Run (#1854).
    const buttons = (item.actions || []).filter((a) => a.button).map((a) =>
      el("button", { type: "button", class: "secondary row-button", text: a.label,
        disabled: busy ? "disabled" : null, "aria-label": `${a.label} ${item.name}`, onclick: rowAction(a) }));
    const actions = [
      ...(caps.editRowFirst ? edit : []),
      ...(item.actions || []).filter((a) => !a.button).map((a) => ({
        label: a.label, destructive: a.destructive,
        // A row whose actions address a path of their own is named by its
        // row, not the path's last segment: a value inside a Datastore
        // property is addressed by its steps as JSON, [6,"when"], and its
        // Remove value is confirmed by typing back items[6].when (#911).
        run: () => runAction(route, rowTarget(item), a, refresh, rowBusy(item.name),
          (item.actsOn || []).length ? item.name : null),
      })),
      ...(caps.editRowFirst ? [] : edit),
      ...(caps.delete ? [{
        label: caps.deleteLabel || "Delete", destructive: true,
        run: () => deleteResource(route, item.name, refresh, rowBusy(item.name)),
      }] : []),
      ...(item.object ? objectActions(route, item, refresh, rowBusy(item.name)) : []),
    ];
    if (!actions.length && !buttons.length) return el("td", {});
    if (!actions.length) return el("td", { class: "row-actions" }, ...buttons);
    // Suppressed rather than merely ignored while the row is busy: a live
    // menu over an in-flight request is an invitation to issue a second one,
    // and Pause has no confirmation step to catch it.
    if (busy) {
      return el("td", { class: "row-actions" },
        el("button", { class: "icon-button overflow-trigger", disabled: true,
          "aria-label": `Actions for ${item.name} are unavailable while it is being changed`,
          html: '<svg viewBox="0 0 24 24" aria-hidden="true"><circle cx="12" cy="5" r="1.6"/><circle cx="12" cy="12" r="1.6"/><circle cx="12" cy="19" r="1.6"/></svg>' }));
    }
    // Behind an overflow menu: a row of buttons competes with the data for
    // attention, and the console this mirrors puts them behind one control.
    return el("td", { class: "row-actions" }, ...buttons, overflowMenu(actions, item.name));
  };

  const draw = () => {
    const rows = visibleRows();
    const total = rows.length;
    const pages = Math.max(1, Math.ceil(total / pageSize));
    if (page >= pages) page = pages - 1;
    const start = page * pageSize;
    const shown = rows.slice(start, start + pageSize);

    setChildren(body, ...shown.map((item) => {
      const cells = [];
      if (selectable && !canSelect(item)) {
        cells.push(el("td", { class: "select-cell" }));
      } else if (selectable) {
        cells.push(el("td", { class: "select-cell" },
          el("input", {
            type: "checkbox", "aria-label": `Select ${item.name}`,
            checked: selected.has(item.name) ? "checked" : null,
            onchange: (e) => {
              if (e.target.checked) selected.add(item.name);
              else selected.delete(item.name);
              drawSelection();
            },
          })));
      }
      const busy = operating.has(item.name);
      if (hasStatus()) {
        // The status the row had is not the status it has: while a request is
        // outstanding the cell says "working", not the value it is about to
        // stop being.
        cells.push(busy
          ? el("td", { class: "status-cell" }, el("span", { class: "status is-working" },
              spinner(`${item.name} is being changed`)))
          : el("td", { class: "status-cell" }, statusIcon(item.status)));
      }
      cells.push(el("td", {}, item.link
        ? el("a", { href: item.link, text: item.name })
        : (item.opens || []).length
          ? el("a", { href: detailHref(route, item.opens), text: item.name })
        : caps.detail
          ? el("a", { href: detailHref(route, item.name), text: item.name })
          : document.createTextNode(item.name)));
      for (const c of dataColumns()) {
        cells.push(el("td", { text: (item.fields || {})[c] || "—" }));
      }
      if (hasActions()) cells.push(rowActionsCell(item));
      const row = el("tr", {
        class: [selected.has(item.name) ? "is-selected" : "",
                busy ? "is-operating" : "",
                // A row for something that does not exist in its own right,
                // listed so what is under it stays reachable, is in italics
                // as on Google's console: a Firestore document with only
                // subcollections (#875).
                item.absent ? "is-absent" : "",
                inspected === item.name ? "is-inspected" : ""]
          .filter(Boolean).join(" ") || null,
      }, ...cells);
      if (opts.rowControls) {
        const inspect = () => {
          inspected = item.name;
          draw();
          drawInfoPanel(item, dataColumns(), route, refresh);
        };
        // A click anywhere that is not itself a control inspects the row. A
        // link or a checkbox keeps doing its own job.
        row.addEventListener("click", (e) => {
          if (e.target.closest("a, button, input, select, label")) return;
          inspect();
        });
        // And by keyboard.
        //
        // Inspection was mouse-only: a keyboard user could reach every control
        // in a row and not the row itself, so the info panel — the thing that
        // says what a row actually holds — was unreachable without a pointer.
        //
        // The row is a button in the accessibility tree rather than a link,
        // because it opens a panel beside the table rather than navigating.
        row.tabIndex = 0;
        row.setAttribute("role", "button");
        row.setAttribute("aria-pressed", inspected === item.name ? "true" : "false");
        row.setAttribute("aria-label", `Inspect ${item.name}`);
        row.addEventListener("keydown", (e) => {
          if (e.key !== "Enter" && e.key !== " ") return;
          // Only when the row itself has focus. A keystroke inside a control in
          // the row belongs to that control — Space on a checkbox toggles it,
          // and stealing it would break selection.
          if (e.target !== row) return;
          e.preventDefault();
          inspect();
        });
      }
      return row;
    }));

    if (!shown.length) {
      // An empty filter result is its own state: "no matches for X" is a
      // different fact from "you have none", and only one of them is a reason
      // to go and create something.
      //
      // Which one this is depends on the filter, not on the row count. A
      // table emptied by deleting its last row was offering to clear a filter
      // nobody had typed, and calling nothing a non-match.
      const query = bar.text();
      const state = bar.active()
        ? el("div", { class: "state state-inline" },
            el("h2", { text: `No matching ${noun}` }),
            el("p", { text: `Nothing matches “${query}”.` }),
            el("button", {
              class: "secondary", text: "Clear filter",
              onclick: () => { bar.clear(); page = 0; draw(); },
            }))
        : el("div", { class: "state state-inline" },
            el("h2", { text: `No ${noun} yet` }),
            el("p", { text: "Create one here, or with an SDK, the CLI or gcloud — " +
                            "it will appear either way." }),
            caps.create
              ? el("button", { class: "primary", text: caps.create.label,
                  onclick: () => startCreate(route, caps.create, refresh) })
              : null);
      setChildren(body, el("tr", {},
        el("td", { colspan: String(columns().length) }, state)));
    }

    drawHead();
    drawFooter(total, start, shown.length);
    drawSelection();
  };

  const drawFooter = (total, start, count) => {
    drawFreshness();
    const pages = Math.max(1, Math.ceil(total / pageSize));
    const from = count ? start + 1 : 0;
    const to = start + count;
    setChildren(footer,
      el("label", { class: "page-size" },
        el("span", { text: "Rows per page" }),
        el("select", {
          "aria-label": "Rows per page",
          onchange: (e) => { pageSize = Number(e.target.value); page = 0; draw(); },
        }, ...PAGE_SIZES.map((n) =>
          el("option", { value: String(n), text: String(n), selected: n === pageSize ? "selected" : null })))),
      // The range follows the filter, so the count on screen always describes
      // the rows on screen.
      el("span", { class: "page-range", text: `${from}–${to} of ${total}` }),
      el("button", {
        class: "icon-button", "aria-label": "Previous page",
        disabled: page === 0 ? "disabled" : null,
        onclick: () => { page--; draw(); },
        html: '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M15 6l-6 6 6 6"/></svg>',
      }),
      el("button", {
        class: "icon-button", "aria-label": "Next page",
        disabled: page >= pages - 1 ? "disabled" : null,
        onclick: () => { page++; draw(); },
        html: '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M9 6l6 6-6 6"/></svg>',
      }),
      // The pager above moves through rows already fetched. This fetches more.
      //
      // The backend bounds every listing, so without this the rows past that
      // bound were unreachable from the console at all — the pager moved
      // through a fixed set and the note said "there may be more".
      loadMore());
  };

  // loadMore fetches the rows after the ones on screen and appends them.
  const loadMore = () => {
    if (!data.more || !data.cursor || !opts.pagePath) return null;
    const button = el("button", { class: "secondary", text: "Load more" });
    button.addEventListener("click", async () => {
      setBusy(button, true);
      try {
        const url = new URL(`/api/page/${route.service}`, location.origin);
        url.searchParams.set("project", currentProject());
        url.searchParams.set("cursor", data.cursor);
        for (const segment of opts.pagePath) url.searchParams.append("name", segment);
        const next = await api(url.pathname + url.search);
        // Appended rather than replacing: the rows on screen stay on screen, and
        // a sort or filter the user set up keeps applying to all of them.
        data.items = [...data.items, ...(next.items || [])];
        data.total = data.items.length;
        data.cursor = next.cursor || "";
        data.more = Boolean(next.more);
        announce(`${(next.items || []).length} more rows loaded`);
        draw();
      } catch (err) {
        notify(`Could not load more: ${err.message}`, "error");
        setBusy(button, false);
      }
    });
    return button;
  };

  const drawSelection = () => {
    if (!selectable) return;
    const n = selected.size;
    bulk.disabled = n === 0;
    for (const b of selectionButtons) b.disabled = n === 0;
    selectionLabel.textContent = n ? `${n} selected` : "";
    const boxes = [...body.querySelectorAll('input[type="checkbox"]')];
    const head = headRow.querySelector('input[type="checkbox"]');
    if (head) {
      const onPage = visibleRows().slice(page * pageSize, page * pageSize + pageSize)
        .filter(canSelect);
      const chosen = onPage.filter((i) => selected.has(i.name)).length;
      head.checked = chosen > 0 && chosen === onPage.length;
      head.indeterminate = chosen > 0 && chosen < onPage.length;
    }
    for (const box of boxes) {
      const row = box.closest("tr");
      if (row) row.classList.toggle("is-selected", box.checked);
    }
  };

  const drawHead = () => {
    setChildren(headRow, ...columns().map((c) => {
      if (c === "select") {
        return el("th", { class: "select-cell", scope: "col" },
          el("input", {
            type: "checkbox", "aria-label": `Select all ${noun} on this page`,
            onchange: (e) => {
              const onPage = visibleRows().slice(page * pageSize, page * pageSize + pageSize)
                .filter(canSelect);
              for (const item of onPage) {
                if (e.target.checked) selected.add(item.name);
                else selected.delete(item.name);
              }
              draw();
            },
          }));
      }
      // Actions is a column of controls, not of values, so it does not sort:
      // offering it would be a control that does nothing.
      if (c === "Actions") {
        return el("th", { scope: "col", class: "actions-cell" }, el("span", { class: "sr-only", text: c }));
      }
      const active = sortColumn === c;
      const arrow = active ? (sortAscending ? "↑" : "↓") : "";
      return el("th", {
        scope: "col",
        "aria-sort": active ? (sortAscending ? "ascending" : "descending") : "none",
      },
        el("button", {
          class: "sort-button" + (active ? " is-active" : "") + (c === "Status" ? " is-status" : ""),
          title: c === "Status" ? "Sort by status" : null,
          onclick: () => {
            if (sortColumn === c) sortAscending = !sortAscending;
            else { sortColumn = c; sortAscending = true; }
            draw();
            announce(`Sorted by ${c}, ${sortAscending ? "ascending" : "descending"}`);
          },
        },
          c === "Status" ? el("span", { class: "status-head-icon", "aria-hidden": "true", html: STATUS_GLYPHS[""] }) : null,
          el("span", { class: c === "Status" ? "sr-only" : null, text: c }),
          el("span", { class: "sort-arrow", "aria-hidden": "true", text: arrow })));
    }));
  };

  // Refreshing reloads the rows without rebuilding the screen, so sort, filter,
  // page and scroll position survive. Re-rendering the whole screen threw all
  // of that away and flashed a skeleton over data that was already correct.
  // When the rows were last read, and whether the last attempt worked. A
  // list that goes quietly stale is a list that lies by omission.
  let readAt = new Date();
  let staleBecause = "";
  const freshness = el("p", { class: "table-freshness unavailable", role: "status" });

  const drawFreshness = () => {
    setChildren(freshness,
      el("span", { text: staleBecause
        ? `Last read ${relativeTime(readAt)} — refresh failed: ${staleBecause}`
        : `Updated ${relativeTime(readAt)}` }));
    freshness.classList.toggle("is-stale", Boolean(staleBecause));
  };

  // quiet is the automatic poll: it must not raise a snackbar every interval
  // on an instance whose backend is down, and must not blank a table that
  // still holds the last good rows.
  const refresh = async (quiet = false) => {
    if (!opts.refetch) return reload();
    try {
      const fresh = await opts.refetch();
      if (fresh && Array.isArray(fresh.items)) {
        data = fresh;
        // A selection may name rows that no longer exist.
        const names = new Set(data.items.map((i) => i.name));
        selected = new Set([...selected].filter((n) => names.has(n)));
        readAt = new Date();
        staleBecause = "";
        draw();
        announce(`${data.items.length} ${noun}, updated just now`);
        return;
      }
    } catch (err) {
      staleBecause = err.message;
      drawFreshness();
      if (quiet) return;
      notify(`Could not refresh: ${err.message}`, "error");
    }
    if (quiet) return;
    reload();
  };

  // The manual control says it is working for as long as it is, and clears
  // in a finally so a failure does not strand the spinner.
  const refreshControl = refreshButton(() => refresh());

  // The automatic poll. Registered so the router clears it, and suspended
  // for a hidden tab through the one visibility listener rather than a
  // second mechanism.
  if (opts.refetch) {
    registerListPoll(() => refresh(true));
  }

  // The panel's open state is the viewer's, not the screen's: it stays open
  // across navigations the way a docked region should.
  const infoToggle = () => {
    const open = infoPanelOpen();
    const button = el("button", {
      class: "icon-button info-toggle", type: "button", "aria-expanded": open ? "true" : "false",
      "aria-controls": "info-panel",
      onclick: () => {
        if (infoPanelOpen()) return closeInfoPanel();
        writeStored(INFO_OPEN_KEY, true);
        const panel = infoPanelHost();
        panel.hidden = false;
        document.documentElement.setAttribute("data-info", "open");
        button.setAttribute("aria-expanded", "true");
        labelInfoToggle(button, true);
        const item = data.items.find((i) => i.name === inspected);
        drawInfoPanel(item, dataColumns(), route, refresh);
        panel.focus();
      },
    });
    labelInfoToggle(button, open);
    INFO_TOGGLE = button;
    return button;
  };

  // Restored on render, so a screen change does not close it.
  if (opts.rowControls && infoPanelOpen()) {
    const panel = infoPanelHost();
    if (panel) {
      panel.hidden = false;
      document.documentElement.setAttribute("data-info", "open");
      drawInfoPanel(null, dataColumns(), route, refresh);
    }
  } else if (!opts.rowControls) {
    // A screen with no rows to inspect has nothing to put in it.
    const panel = infoPanelHost();
    if (panel) panel.hidden = true;
    document.documentElement.removeAttribute("data-info");
  }

  drawFreshness();
  draw();

  // A list screen's Create, Refresh and info-panel toggle are on its title
  // line, as Google's are (#1842); the bar under it keeps what acts on the
  // product or on the checked rows. A table inside a resource page keeps its
  // own bar, since the title line there belongs to the resource.
  const slot = opts.rowControls ? headerActions(header) : null;
  if (slot) {
    setChildren(slot,
      caps.create ? createButton(caps.create.label, () => startCreate(route, caps.create, refresh)) : null,
      refreshControl,
      infoToggle());
  }
  const listActions = opts.rowControls ? listActionButtons(route, data, refresh) : [];
  setChildren(view, ...header,
    noteBanner(data.note),
    // The page's actions and the table's filter are different things, so they
    // are different bars: one acts on the product, the other narrows the view.
    opts.rowControls
      ? (listActions.length || selectable
          ? el("div", { class: "action-bar" },
              ...listActions,
              // A list screen's selection actions, History's Approve
              // (#1854); Delete only where the rows can be deleted.
              ...selectionButtons,
              caps.delete ? bulk : null,
              selectionLabel)
          : null)
      : el("div", { class: "action-bar" }, ...selectionButtons,
          selectActions.length ? selectionLabel : null, refreshControl),
    el("div", { class: "filter-bar" }, bar.node, chooser ? chooser.node : null, freshness),
    bar.chips,
    el("div", { class: "table-wrap" },
      el("table", {}, el("thead", {}, headRow), body)),
    footer);
}

// detailHref is the address of one row's contents.
// detailHref is a resource's own address, one path segment per level.
//
// Readable as text, which docs/console-parity.md section 5 requires, and
// deep-linkable at any depth: /cloudsql/cloudburrow/widgets is a table.
function detailHref(route, path) {
  const segments = Array.isArray(path) ? path : [path];
  const url = new URL(
    route.path + "/" + segments.map(encodeURIComponent).join("/"),
    location.origin);
  const project = new URLSearchParams(location.search).get("project");
  if (project) url.searchParams.set("project", project);
  carryScope(url, route.service);
  return url.pathname + url.search;
}

// renderDetail shows what is inside one row.
//
// It draws the provider's listing with the same renderer as the list screen:
// one table implementation, so the two cannot drift apart, and sorting and
// filtering work here for free.
// SELECT_SECTION selects a tab of the detail page on screen, for a grouped
// section's own strip (#1845).
let SELECT_SECTION = null;

async function renderDetail(view, route, resourcePath) {
  SELECT_SECTION = null;
  const segments = Array.isArray(resourcePath) ? resourcePath : [resourcePath];
  const name = segments[segments.length - 1];
  const project = new URLSearchParams(location.search).get("project") || "";
  const back = new URL(route.path, location.origin);
  if (project) back.searchParams.set("project", project);
  carryScope(back, route.service);

  // One crumb per level, built from the path rather than from two fixed
  // nodes. A database, a table and a column are three levels, and every one
  // above the last is a working link back to it.
  const trail = [
    el("a", { href: back.pathname + back.search, text: route.crumb || route.title }),
  ];
  segments.forEach((segment, i) => {
    trail.push(el("span", { "aria-hidden": "true", text: "/" }));
    trail.push(i === segments.length - 1
      ? el("span", { text: segment })
      : el("a", { href: detailHref(route, segments.slice(0, i + 1)), text: segment }));
  });

  // A top-level resource of a route that names its kind is titled as
  // Google titles it, "<Kind> details", with its name under the title line
  // (#1842); deeper pages (an object, a document) keep their own name. The
  // back arrow goes one level up: the list, or the resource above.
  const kindTitled = Boolean(route.kind) && segments.length === 1;
  const up = segments.length > 1 ? detailHref(route, segments.slice(0, -1)) : back.pathname + back.search;
  const nameLine = kindTitled ? el("p", { class: "resource-name", text: name }) : null;
  const crumb = el("div", { class: "page-header" },
    // A breadcrumb, because a screen you can only leave with the browser
    // button is a screen you are stuck in.
    el("nav", { class: "breadcrumb", "aria-label": "Breadcrumb" }, ...trail),
    el("div", { class: "page-title-row" },
      backArrow(up, segments.length > 1 ? "Back" : `Back to ${route.title}`),
      // Inside BigQuery Studio's tab (#1861) the page is a tab's content,
      // under the Studio's own h1.
      el(view.dataset.embedded === "true" ? "h2" : "h1", { text: kindTitled ? `${route.kind} details` : name }),
      el("div", { class: "page-header-actions" })),
    nameLine,
    el("p", { class: "subtitle", text: (project ? `Project ${project}` : "All projects") + scopeSubtitle(route.service) }));
  const header = [crumb];
  // A provider-drawn trail, for a page whose path is not its hierarchy: an
  // object's crumbs are its bucket and folders (#790).
  const drawTrail = (crumbs) => {
    const nav = crumb.querySelector(".breadcrumb");
    const nodes = [el("a", { href: back.pathname + back.search, text: route.crumb || route.title })];
    crumbs.forEach((c, i) => {
      nodes.push(el("span", { "aria-hidden": "true", text: "/" }));
      nodes.push(i === crumbs.length - 1 || !(c.path || []).length
        ? el("span", { text: c.label })
        : el("a", { href: detailHref(route, c.path), text: c.label }));
    });
    setChildren(nav, ...nodes);
  };

  const path = `/api/detail/${route.service}?project=${encodeURIComponent(project)}` +
               segments.map((sg) => `&name=${encodeURIComponent(sg)}`).join("");
  const cancel = new AbortController();
  setChildren(view, ...header,
    loadingState(5, { what: name, onCancel: () => cancel.abort() }));

  let data;
  try {
    data = await api(path, { signal: cancel.signal });
  } catch (err) {
    return setChildren(view, ...header,
      isCancelled(err)
        ? cancelledState(`${name} not loaded`, () => renderDetail(view, route, segments))
        : errorState(`${name} unavailable`, String(err.message),
                     () => renderDetail(view, route, segments)));
  }
  // The prompt is checked first, for the same reason it is on the list
  // screen: needing a project is a precondition, not a failure, and it must
  // never be rendered as one.
  if (data.prompt) {
    return setChildren(view, ...header, emptyState("Choose a project", data.prompt));
  }
  if (data.unavailable) {
    return setChildren(view, ...header,
      errorState(`${name} unavailable`, data.unavailable, () => renderDetail(view, route, segments)));
  }

  if ((data.trail || []).length) drawTrail(data.trail);
  // A page addressed by something no reader recognises — a Datastore
  // entity's encoded key (#875) — says what it is in its heading.
  if (data.title) (nameLine || crumb.querySelector("h1, h2")).textContent = data.title;
  const sections = (data.sections || []).slice();
  // A Logs tab where Google's page has one (#1842, audit item 9): a Cloud
  // Run service's, job's and worker pool's, a function's, and a GKE
  // workload's, pod's, Service's and Job's ("Click the Logs tab to get the
  // request and container logs for all revisions of this service",
  // https://docs.cloud.google.com/run/docs/logging). The route says so with
  // `logs`; a resource whose Google page has no Logs tab (a bucket, a topic)
  // gets none, and reaches its lines from the Logs Explorer as there. The
  // tab reads the instance's log store, which holds what is written to Cloud
  // Logging when the instance serves it, and links to the Explorer with the
  // same filter applied.
  //
  // A provider that draws its own Logs tab keeps it: a Cloud Run execution's
  // tasks are pods that finish before the log follower's next sweep, so their
  // output is read from the pods themselves (#785).
  // A placeholder (console.KindLogs) puts the tab in the provider's order,
  // as Google's service page has Metrics, SLOs, Logs (#1852).
  const ownLogs = sections.find((s) => s.id === "logs");
  if (!ownLogs && route.logs && segments.length === 1) {
    sections.push({
      id: "logs", label: "Logs", kind: "logs",
      resource: name,
    });
  } else if (ownLogs && ownLogs.kind === "logs" && !ownLogs.resource) {
    // A placeholder (console.KindLogs) puts the Logs tab in the provider's
    // order, as Google's service page has Metrics, SLOs, Logs (#1852).
    ownLogs.resource = name;
  }

  const queryable = capabilityOf(route.service).query;
  // A page with a provider's Studio (Spanner Studio, #1859) has its editor
  // there, so it has no Query tab of its own.
  // A placeholder (console.KindQuery) puts the query tab in the
  // provider's order under its own label, as Firestore's database page
  // has its Query builder (#1845); with no form for the page it is dropped.
  const ownQuery = sections.find((s) => s.id === "query" && s.kind === "query");
  if (ownQuery) {
    ownQuery.query = ownQuery.query || data.query || queryable;
    if (!ownQuery.query) sections.splice(sections.indexOf(ownQuery), 1);
  } else if (queryable && !sections.some((s) => s.kind === "studio")) {
    // The provider may narrow the form to the resource being looked at — a
    // Bigtable table's column families are not a Firestore collection's fields —
    // so the resource's own query spec wins over the service-wide one.
    sections.push({
      id: "query", label: "Query", kind: "query",
      query: data.query || queryable,
    });
  }
  // A resource whose page is all details (a build trigger, a worker pool)
  // shows them on a Details tab, the first tab Google's pages put them on
  // (#1842), rather than in the header above no tabs at all.
  const detailsTab = !sections.length && (data.summary || []).length > 0;
  if (detailsTab) {
    sections.push({ id: "details", label: "Details", kind: "properties",
      groups: [{ properties: data.summary }] });
  }
  if (!sections.length) {
    return setChildren(view, ...header,
      emptyState(`Nothing to show for ${name}`,
        "This resource reports no sections, which is a gap in the provider rather than a fault."));
  }

  // What this is, before what is inside it.
  //
  // The properties are the ones the list row already showed; clicking through
  // used to drop them, so the screen named after a resource said nothing
  // about it. Google's pages carry them in the page's header, beside the
  // title (a bucket's Location, Storage class, Public access), not in a
  // "Details" card of their own between the header and the tabs (#1842,
  // audit item 9), so they are a row of the header here, above the tab
  // strip. A provider that holds none draws none.
  const summary = (data.summary || []).length && !detailsTab
    ? el("dl", { class: "resource-summary", "aria-label": "Details" },
        ...(data.summary).map((prop) =>
          el("div", { class: "resource-summary-item" },
            el("dt", { text: prop.label }),
            propertyValue(prop))))
    : null;
  if (summary) { crumb.append(summary); crumb.classList.add("has-summary"); }

  // The tab the address names, else the one Google's page opens on (#1847).
  // A page drawn in a Studio tab keeps its sub-tab on the tab, not in the
  // address, which is the Studio's (#1861).
  const embedded = view.dataset.embedded === "true";
  const wanted = (embedded ? view.dataset.tab : new URLSearchParams(location.search).get("tab")) || data.openTab;
  let current = Math.max(0, sections.findIndex((sec) => sec.id === wanted));

  const panel = el("div", { class: "tab-panel", id: "detail-panel", role: "tabpanel" });

  const drawPanel = () => drawSectionInto(panel, sections[current]);
  const drawSectionInto = (panel, section) => {
    const reload = () => renderDetail(view, route, segments);
    // The previous tab's list poll redraws its table into this same panel:
    // left running, it replaced a Query tab, editor, results and all, with
    // the Tables listing at its next tick (#1568). A listing section
    // registers its own again as it draws.
    stopListPoll();

    // A section that cannot be read says so inside its own panel, whatever
    // kind it is. Rendering it as an empty table would claim the resource
    // holds nothing.
    const failure = section.unavailable || (section.listing || {}).unavailable;
    if (failure) {
      return setChildren(panel,
        section.kind === "permissions" ? permissionsNote(section.permissions) : null,
        errorState(`${section.label} unavailable`, failure, reload));
    }

    let note = section.note
      ? el("p", { class: "unavailable", text: section.note })
      : null;
    // A tab's own actions, at the top of the tab rather than beside the
    // page title: Google's Lifecycle tab has Add a rule (#1843).
    if ((section.actions || []).length) {
      const bar = el("div", { class: "action-bar section-actions", id: `section-actions-${section.id}` },
        ...section.actions.map((a) => el("button", {
          type: "button", class: "secondary" + (a.destructive ? " danger" : ""), text: a.label,
          onclick: () => runAction(route, segments, a, reload, NO_ROW, data.title || null),
        })));
      note = note ? el("div", {}, bar, note) : bar;
    }

    // A grouped section's own strip, its siblings' labels (#1845).
    if (section.group) {
      const members = sections.map((s, i) => [s, i]).filter(([s]) => s.group === section.group);
      const sub = el("div", { class: "tab-strip sub-tabs", role: "tablist", "aria-label": `${section.group} tabs` },
        ...members.map(([s, i]) => el("button", {
          class: "tab" + (i === current ? " is-selected" : ""), role: "tab", id: `subtab-${s.id}`,
          "aria-selected": i === current ? "true" : "false", tabindex: i === current ? "0" : "-1",
          text: s.label, onclick: () => SELECT_SECTION && SELECT_SECTION(i),
        })));
      note = note ? el("div", {}, sub, note) : sub;
    }

    // A section's own tabs (#1860): Google's Cloud SQL Connections page
    // has Summary, Networking and Security. The strip is a tablist of its
    // own inside the panel, the tab chosen kept in the address as subtab.
    if ((section.tabs || []).length) {
      const subs = section.tabs;
      const subWanted = new URLSearchParams(location.search).get("subtab");
      let sub = Math.max(0, subs.findIndex((t) => t.id === subWanted));
      const subPanel = el("div", { class: "tab-panel sub-tab-panel", id: `subpanel-${section.id}`, role: "tabpanel" });
      const subTabs = subs.map((t, i) => el("button", {
        class: "tab" + (i === sub ? " is-selected" : ""), role: "tab", id: `subtab-${t.id}`,
        "aria-selected": i === sub ? "true" : "false", "aria-controls": subPanel.id,
        tabindex: i === sub ? "0" : "-1", text: t.label,
        onclick: () => pick(i),
      }));
      const pick = (i) => {
        sub = i;
        subTabs.forEach((tab, j) => {
          tab.classList.toggle("is-selected", j === i);
          tab.setAttribute("aria-selected", j === i ? "true" : "false");
          tab.setAttribute("tabindex", j === i ? "0" : "-1");
        });
        const url = new URL(location.href);
        url.searchParams.set("subtab", subs[i].id);
        history.replaceState({}, "", url);
        subPanel.setAttribute("aria-labelledby", `subtab-${subs[i].id}`);
        drawSectionInto(subPanel, subs[i]);
        subTabs[i].focus();
        announce(`${subs[i].label} selected`);
      };
      const subStrip = el("div", { class: "tab-strip sub-tab-strip", role: "tablist",
        "aria-label": `${section.label} tabs` }, ...subTabs);
      subStrip.addEventListener("keydown", (e) => {
        const step = e.key === "ArrowRight" ? 1 : e.key === "ArrowLeft" ? -1
          : e.key === "Home" ? -sub : e.key === "End" ? subs.length - 1 - sub : 0;
        if (!step && e.key !== "Home" && e.key !== "End") return;
        e.preventDefault();
        pick((sub + step + subs.length) % subs.length);
      });
      subPanel.setAttribute("aria-labelledby", `subtab-${subs[sub].id}`);
      setChildren(panel, note, subStrip, subPanel);
      return drawSectionInto(subPanel, subs[sub]);
    }

    // The provider says what the section holds. Inferring it from whichever
    // field happened to be populated would put the decision in the client,
    // and an empty one would be indistinguishable from a table with no rows.
    switch (section.kind) {
      case "properties":
        return drawPropertiesSection(panel, section, note);
      case "text":
        return drawTextSection(panel, section, note);
      case "chart":
        return drawChartSection(panel, section, note);
      case "logs":
        return drawLogsSection(panel, section, note);
      case "permissions":
        // Its own actions over the principals: a bucket's access control
        // and Switch to (#1843).
        drawPermissionsSection(panel, section, reload);
        if (note) panel.prepend(note);
        return;
      case "query":
        return setChildren(panel,
          queryPane(route, segments, section.query, () => {}));
      case "studio": {
        // A provider's workspace in the tab (#1859): Spanner Studio.
        const draw = STUDIO_SECTIONS[route.service];
        if (draw) return draw(panel, route, segments, project);
        return setChildren(panel, emptyState("No studio",
          "This console has no workspace for this service's Studio section."));
      }
      case undefined:
      case "":
      case "listing":
        return drawListingSection(panel, section, note, reload);
      default:
        // Said out loud rather than drawn as an empty table: a kind this
        // console does not know is a version skew, and guessing hides it.
        return setChildren(panel, emptyState(
          `This console cannot draw a ${section.kind} section`,
          "The instance is serving a section kind this console does not know how to render."));
    }
  };

  // The resource's own log lines, plus a link to the Explorer with the same
  // filter already applied — so a reader who wants the live stream and the full
  // control set gets there without rebuilding the query.
  const drawLogsSection = (into, section, note) => {
    setChildren(into, note, loadingState(4));
    const query = new URLSearchParams({ resource: section.resource, limit: "200" });
    if (project) query.set("project", project);
    const explorer = new URLSearchParams({ resource: section.resource });
    if (project) explorer.set("project", project);

    api(`/api/logs?${query}`).then((data) => {
      const entries = data.entries || [];
      // Which store the lines are from: with Cloud Logging served, what
      // clients write through its API is here beside the pods' output.
      if (data.cloudLogging === false && !note) {
        note = el("p", { class: "unavailable",
          text: "Cloud Logging is not served by this instance, so these are the containers' own lines only." });
      }
      const link = el("a", { class: "button-link secondary",
        href: `/logs?${explorer}`,
        text: "Open in Logs Explorer" });
      if (!entries.length) {
        // Which kind of empty. A pod's log line often carries no project, so a
        // project-scoped read of a noisy resource can come back empty while the
        // instance holds hundreds of its lines.
        return setChildren(into, note,
          emptyState(`No entries for ${name}`,
            `This instance holds ${data.held || 0} entries, ${data.unattributed || 0} of ` +
            "which carry no project. A pod's line usually cannot be attributed to one, " +
            "so the Explorer with all sources selected may show more."),
          el("div", { class: "card-actions" }, link));
      }
      const listing = {
        nameColumn: "Time",
        columns: ["Severity", "Source", "Message"],
        noun: "entries",
        alwaysStatus: false,
        total: entries.length,
        // Newest first, which is the opposite of the stream's order and the
        // right one for a tab someone opens to see what just happened.
        items: [...entries].reverse().map((e) => ({
          name: new Date(e.timestamp).toLocaleTimeString(),
          status: e.severity,
          fields: { Severity: e.severity, Source: e.source || "—", Message: e.message },
        })),
      };
      setChildren(into);
      renderTableInto(into, [note, el("div", { class: "card-actions" }, link)].filter(Boolean),
                      listing, "entries", () => drawPanel(), route, {});
    }).catch((err) => {
      setChildren(into, note,
        errorState("Logs unavailable", String(err.message), () => drawPanel()));
    });
  };

  const drawListingSection = (into, section, note, reload) => {
    let list = section.listing || {};
    const noun = list.noun || section.label.toLowerCase();
    // An upload control where the provider says objects can land. It joins
    // the section's note, so it is drawn whether or not there are rows yet —
    // an empty bucket is exactly where the first upload happens.
    if (section.uploadTo) {
      const up = uploadControl(route, section.uploadTo, reload);
      // Show versions (#853): the same place listed with every generation,
      // live and noncurrent, from the storage server's versions=true.
      up.append(versionsToggle(() => drawPanel()));
      // Google's Show list (#1843): Soft-deleted objects only draws the
      // bucket's soft-deleted objects, with Restore, in the Objects tab.
      const softSection = sections.find((sc) => sc.id === "deleted");
      if (softSection) up.append(objectsShowPicker(() => drawPanel()));
      note = note ? el("div", {}, note, up) : up;
      if (softSection && objectsShow === OBJECTS_SHOW_SOFT) {
        return drawListingSection(into, { ...softSection, uploadTo: null }, note, reload);
      }
      if (readStored(VERSIONS_KEY, false)) return drawVersionsSection(into, section, note, reload);
    }
    if (!(list.items || []).length) {
      // The listing's own note, when it has one.
      //
      // A provider sets it precisely for this case — "this selector matches no
      // pods", "this table has no column families, so nothing can be written to
      // it" — and it was rendered only through renderTableInto, which an empty
      // listing never reaches. So every explanation written for an empty section
      // was invisible in exactly the case it was written for, replaced by
      // "holds none yet".
      //
      // "yet" is also wrong here. A Service whose selector matches nothing is
      // not a Service that will match something later, and a page that says so
      // tells the reader to wait instead of to look at the selector.
      return setChildren(into, note,
        emptyState(`No ${noun}`,
          list.note || `${name} reports no ${noun}.`));
    }
    setChildren(into, note);
    list = linked(list);
    renderTableInto(into, note ? [note] : [], list, noun, reload, route, {
      pagePath: segments,
      refetch: async () => {
        const fresh = await api(path);
        const same = (fresh.sections || []).find((sec) => sec.id === section.id);
        return same ? linked(same.listing) : null;
      },
    });
  };

  // A row that has a level below it becomes a link into that level. The
  // provider declares it; the client neither guesses nor offers a link
  // that would 501.
  //
  // Applied to a refetched listing too: a refresh or the poll used to
  // redraw the rows without their links (#790).
  const linked = (l) => {
    if (!l || !(l.rowsOpenable || (l.items || []).some((i) => i.opens))) return l;
    return {
      ...l,
      items: l.items.map((item) => {
        // A row's own path wins: a listing can mix rows that open with
        // rows that do not, which is what a bucket's folders and objects
        // are.
        if (item.opens) return { ...item, link: detailHref(route, item.opens) };
        if (!l.rowsOpenable) return item;
        return { ...item, link: item.link || detailHref(route, [...segments, item.name]) };
      }),
    };
  };

  // The object list with Show versions on (#853): every generation under
  // the section's prefix, each row opening that generation's page, with
  // Restore as live version and Delete version on its menu.
  const drawVersionsSection = (into, section, note, reload) => {
    const versions = `/api/objects/${route.service}/versions?${objectQuery(section.uploadTo)}`;
    setChildren(into, note, loadingState(3, { what: "versions" }));
    api(versions).then((list) => {
      if (!(list.items || []).length) {
        return setChildren(into, note, emptyState("No versions", list.note || `${name} holds no objects.`));
      }
      setChildren(into, note);
      renderTableInto(into, note ? [note] : [], linked(list), "versions", reload, route, {
        pagePath: segments,
        refetch: async () => linked(await api(versions)),
      });
    }).catch((err) => {
      setChildren(into, note, errorState("Versions unavailable", String(err.message), () => drawPanel()));
    });
  };

  // The IAM policy (#793), the way Google's console draws a resource's
  // Permissions: one row per principal and role, Grant access above them,
  // Remove principal on each row. Every change goes to the server with the
  // etag this page read, so a policy changed since is refused by the service
  // rather than overwritten.
  const drawPermissionsSection = (into, section, reload) => {
    const view = section.permissions || {};
    const rows = (view.bindings || []).flatMap((b) =>
      (b.members || []).map((member) => ({ member, role: b.role, condition: b.condition || "",
        conditional: b.conditional || null })))
      .sort((a, b) => a.member.localeCompare(b.member) || a.role.localeCompare(b.role));
    const withCondition = rows.some((r) => r.condition);
    const change = (body) => send(
      `/api/permissions/${route.service}?project=${encodeURIComponent(currentProject())}`,
      "POST", { Path: segments, Etag: view.etag, ...body });

    const remove = (row) => confirmDestructive({
      title: `Remove ${row.member} from ${row.role}?`,
      detail: `${row.member} will no longer be listed with ${row.role} on ${name}.`,
      confirmWord: row.member,
      confirmLabel: "Remove",
      consequence: "",
      onConfirm: async () => {
        const op = recordOperation(`Remove ${row.member} from ${row.role} on ${name}`);
        try {
          const res = await change({ Remove: { Member: row.member, Role: row.role, Condition: row.conditional } });
          op.succeeded("", res.operation);
        } catch (err) {
          op.failed(err.message, err.operation);
          throw err;
        }
        notify(`Removed ${row.member} from ${row.role}`);
        reload();
      },
    });

    const grant = el("button", { class: "primary", id: "grant-access", text: "Grant access",
      onclick: () => openGrantForm(view, name, change, reload) });
    // Test permissions (#1242) is the service's TestIamPermissions: it
    // changes nothing, so it needs no etag and records no operation.
    const test = el("button", { class: "secondary", id: "test-permissions", text: "Test permissions",
      onclick: () => openTestPermissionsForm(view, name, (perms) => send(
        `/api/permissions/${route.service}?project=${encodeURIComponent(currentProject())}`,
        "POST", { Path: segments, Test: { Permissions: perms } })) });

    const table = rows.length
      ? el("div", { class: "table-wrap" },
          el("table", { id: "permissions-table" },
            el("thead", {}, el("tr", {},
              el("th", { text: "Principal" }),
              el("th", { text: "Role" }),
              withCondition ? el("th", { text: "Condition" }) : null,
              el("th", {}, el("span", { class: "sr-only", text: "Actions" })))),
            el("tbody", {}, ...rows.map((row) => el("tr", {},
              el("td", { class: "mono", text: row.member }),
              el("td", { class: "mono", text: row.role }),
              withCondition ? el("td", { class: "permissions-condition" },
                row.conditional ? el("span", { class: "condition-title", text: row.conditional.title }) : null,
                row.conditional ? el("code", { class: "condition-expression", text: row.conditional.expression }) : null,
                row.conditional && row.conditional.description
                  ? el("span", { class: "form-help", text: row.conditional.description }) : null,
                row.conditional ? null : el("span", { text: row.condition || "—" })) : null,
              el("td", { class: "row-actions" },
                // Edit condition (#1742): where the service stores IAM
                // Conditions, a row's condition is added, changed or
                // removed, as Google's console edits a role's condition.
                view.conditions && (row.conditional || !row.condition)
                  ? el("button", { class: "secondary", text: row.conditional ? "Edit condition" : "Add condition",
                      "aria-label": `${row.conditional ? "Edit" : "Add"} the condition of ${row.member}'s ${row.role}`,
                      onclick: () => openConditionForm(row, name, change, reload, view.resource || "") })
                  : null,
                el("button", { class: "secondary danger", text: "Remove principal",
                  "aria-label": `Remove ${row.member} from ${row.role}`,
                  onclick: () => remove(row) })))))))
      : emptyState("No principals",
          `No principal holds a role on ${name}. Grant access adds one.`);
    setChildren(into, permissionsNote(view),
      el("div", { class: "action-bar" }, grant, test), table);
  };

  // Properties render as the same definition list the summary card uses, so a
  // resource's configuration and its identity are one visual language rather
  // than two.
  const drawPropertiesSection = (into, section, note) => {
    const groups = (section.groups || []).filter((g) => (g.properties || []).length);
    if (!groups.length) {
      return setChildren(into, note,
        emptyState(`No ${section.label.toLowerCase()}`,
                   `${name} reports none.`));
    }
    setChildren(into, note, ...groups.map((group) =>
      el("div", { class: "card properties" },
        group.heading ? el("h2", { text: group.heading }) : null,
        el("dl", {}, ...group.properties.flatMap((prop) => [
          el("dt", { text: prop.label }),
          propertyValue(prop),
        ])))));
  };

  // Preformatted: newlines are the content, and a YAML document that lost
  // them is not a YAML document.
  const drawTextSection = (into, section, note) => {
    if (!section.text) {
      return setChildren(into, note,
        emptyState(`No ${section.label.toLowerCase()}`, `${name} reports none.`));
    }
    setChildren(into, note,
      el("div", { class: "card text-section" },
        el("div", { class: "text-actions" },
          copyButton(section.text, `${section.label} for ${name}`)),
        el("pre", { class: "mono", text: section.text })));
  };

  const drawChartSection = (into, section, note) => {
    const series = (section.series || []).filter((sv) => (sv.points || []).length);
    if (!series.length) {
      return setChildren(into, note,
        emptyState("No readings yet",
          "This instance has taken no measurements for this resource yet."));
    }
    setChildren(into, note, el("div", { class: "charts" }, ...series.map((sv) =>
      chartCard(sv.label, sv.points, {
        label: `${sv.label} for ${name}`,
        max: sv.max || undefined,
        foot: sv.unit ? `Measured in ${sv.unit}` : "",
      }))));
  };

  // One section is not a tab strip. A strip of one is a control that does
  // nothing, and the real console does not draw one either.
  let strip = null;
  if (sections.length > 1) {
    // Sections sharing a `group` are one tab under the group's name, with
    // their own labels on a strip inside it (#1845): Google's Indexes page
    // holds its Manual and Automatic tabs. The group's tab is its first
    // member's; selecting it opens that member.
    const leadOf = (i) => {
      const g = sections[i].group;
      return g ? sections.findIndex((s) => s.group === g) : i;
    };
    const tabs = sections.map((section, i) =>
      el("button", {
        class: "tab" + (leadOf(current) === i ? " is-selected" : ""),
        role: "tab", id: `tab-${section.id}`,
        "aria-selected": leadOf(current) === i ? "true" : "false",
        "aria-controls": "detail-panel",
        tabindex: leadOf(current) === i ? "0" : "-1",
        hidden: leadOf(i) !== i,
        text: section.group && leadOf(i) === i ? section.group : section.label,
        onclick: () => select(i),
      }));

    const select = (i) => {
      current = i;
      tabs.forEach((tab, j) => {
        const on = j === leadOf(i);
        tab.classList.toggle("is-selected", on);
        tab.setAttribute("aria-selected", on ? "true" : "false");
        tab.setAttribute("tabindex", on ? "0" : "-1");
      });
      // The tab is part of the address, so a colleague can be sent the tab
      // rather than the resource.
      if (embedded) {
        view.dataset.tab = sections[i].id;
        view.dispatchEvent(new CustomEvent("cb-subtab", { detail: sections[i].id }));
      } else {
        const url = new URL(location.href);
        url.searchParams.set("tab", sections[i].id);
        history.replaceState({}, "", url);
      }
      panel.setAttribute("aria-labelledby", `tab-${sections[i].id}`);
      drawPanel();
      if (!tabs[i].hidden) tabs[i].focus();
      announce(`${sections[i].label} selected`);
    };
    SELECT_SECTION = select;

    // A side menu (Detail.SideNav) is the same tablist, drawn down the
    // left of the page as Google's Cloud SQL instance menu (#1860) and
    // Spanner's instance and database pages (#1859) are, moved through
    // with the up and down arrows.
    strip = el("div", { class: "tab-strip" + (data.sideNav ? " is-side-nav side-nav" : ""), role: "tablist",
      "aria-label": `${name} sections`, "aria-orientation": data.sideNav ? "vertical" : null },
      ...tabs);
    // Arrow keys move between tabs, which is what makes a tablist a tablist
    // rather than a row of buttons.
    strip.addEventListener("keydown", (e) => {
      const fwd = data.sideNav ? "ArrowDown" : "ArrowRight";
      const bwd = data.sideNav ? "ArrowUp" : "ArrowLeft";
      // Over the tabs drawn: a group's members after its first are not.
      const shown = sections.map((_, i) => i).filter((i) => leadOf(i) === i);
      const at = Math.max(0, shown.indexOf(leadOf(current)));
      const step = e.key === fwd ? 1 : e.key === bwd ? -1
        : e.key === "Home" ? -at : e.key === "End" ? shown.length - 1 - at : 0;
      if (!step && e.key !== "Home" && e.key !== "End") return;
      e.preventDefault();
      select(shown[(at + step + shown.length) % shown.length]);
    });
    if (!data.sideNav) crumb.append(strip);
  }

  // What can be done to this resource, on the resource's own page.
  //
  // The provider decides; the server checks the same list before performing
  // one, so a button that is absent here is also refused there. An edit form
  // is offered only when the resource carries one — what may be changed about
  // a queue is not what may be changed about a subscription, so the form
  // belongs to the resource and not to the service.
  const reloadPage = () => renderDetail(view, route, segments);
  // An action that removes the resource on screen goes up a level on
  // success: reloading would draw a page for something that no longer
  // exists. An action says so with leaves (a deleted document); a delete on a
  // nested resource's own page — the execution's job, the revision's service
  // — does so too.
  //
  // A page with a provider-drawn trail goes up to the nearest crumb above it
  // that is a page: a deleted object version's is its folder (#853), where
  // the path's parent would be no page at all.
  //
  // The page above can be gone too: deleting a generic repository's last
  // file deletes its version and its package (#1592), so the package's
  // page would only say NotFound. The nearest page above that still reads
  // is the one gone to, and the list when none does.
  const trailUp = (data.trail || []).slice(0, -1).reverse().find((c) => (c.path || []).length);
  const leavePage = async () => {
    const ups = trailUp ? [trailUp.path] : [];
    for (let n = segments.length - 1; n > 0; n--) ups.push(segments.slice(0, n));
    for (const up of ups) {
      // A page that is gone answers unavailable, or fails.
      try {
        const above = await api(`/api/detail/${route.service}?project=${encodeURIComponent(project)}` +
          up.map((sg) => `&name=${encodeURIComponent(sg)}`).join(""));
        if (!above.unavailable) return navigate(detailHref(route, up));
      } catch { /* gone as well: one level further up */ }
    }
    navigate(route.path);
  };
  const leaves = (a) => a.leaves || (a.destructive && a.id.startsWith("delete") && segments.length > 1);
  // An action a tab draws at its top (Section.Actions) is drawn there only,
  // not beside the title as well (#1845): Google's Create Index is on the
  // Indexes tab, not on the database's title line.
  const onTabs = new Set(sections.flatMap((s) => (s.actions || []).map((a) => a.id)));
  // An action a tab's table offers on its checked rows is that table's
  // button, not the header's (#1851): a key's Disable over its versions.
  const onRows = new Set(sections.flatMap((s) => ((s.listing || {}).selectActions || []).map((a) => a.id)));
  const pageActions = [
    ...(data.actions || []).filter((a) => !onTabs.has(a.id) && !onRows.has(a.id)).map((a) =>
      el("button", {
        class: "secondary" + (a.destructive ? " danger" : ""),
        text: a.label,
        onclick: () => runAction(route, segments, a, leaves(a) ? leavePage : reloadPage, NO_ROW, data.title || null),
      })),
  ];
  // An object's page (#1843): Download and Delete beside its title, as on
  // Google's object details, through the same routes the object rows use.
  if ((data.object || []).length === 2) {
    const q = objectQuery(data.object);
    const label = data.object.join("/");
    pageActions.unshift(el("button", {
      class: "secondary", text: "Download", id: "object-page-download",
      onclick: () => {
        const a = el("a", { href: `/api/objects/${route.service}/download?${q}`, download: "" });
        document.body.append(a);
        a.click();
        a.remove();
      },
    }));
    pageActions.push(el("button", {
      class: "secondary danger", text: "Delete", id: "object-page-delete",
      onclick: () => confirmDestructive({
        title: `Delete ${label}?`,
        confirmWord: data.object[1].split("/").pop() || data.object[1],
        onConfirm: async () => {
          const op = recordOperation(`Delete ${label}`);
          try {
            const res = await send(`/api/objects/${route.service}?${q}`, "DELETE");
            op.succeeded("", res.operation);
            notify(`Deleted ${label}`);
            leavePage();
          } catch (err) {
            op.failed(err.message, err.operation);
            throw err;
          }
        },
      }),
    }));
  }
  if (data.reveal) {
    pageActions.push(el("button", {
      class: "secondary", text: data.reveal,
      onclick: () => revealValue(route, segments, data.reveal),
    }));
    // A version row's View secret value (#1850) opens this page with
    // reveal=1: the value is shown as if the button were pressed, recorded
    // the same way, and the address loses the flag so a reload shows nothing.
    const asked = new URLSearchParams(location.search);
    if (asked.get("reveal") === "1") {
      asked.delete("reveal");
      const rest = asked.toString();
      history.replaceState(history.state, "", location.pathname + (rest ? "?" + rest : ""));
      setTimeout(() => revealValue(route, segments, data.reveal), 0);
    }
  }
  if (data.edit) {
    pageActions.unshift(el("button", {
      class: "primary", text: data.edit.label || "Edit",
      onclick: () => openEditForm(route, segments, data.edit, reloadPage),
    }));
  }
  // Beside the title, as Google's Edit and Delete are (#1842).
  if (pageActions.length) {
    setChildren(headerActions(crumb), el("div", { class: "page-actions" }, ...pageActions));
  }

  panel.setAttribute("aria-labelledby", `tab-${sections[current].id}`);
  setChildren(view, ...header, data.sideNav && strip
    ? el("div", { class: "detail-with-nav side-nav-layout" }, strip, panel) : panel);
  drawPanel();
  announce(`${data.title || name} opened`);
  // An address naming one of the page's actions opens it once (#1851):
  // Create key ring goes on to the ring's Create key. The action is taken
  // off the address, so a reload or Back does not open it again.
  const asked = new URLSearchParams(location.search).get("action");
  const opened = asked && (data.actions || []).find((a) => a.id === asked);
  if (asked) {
    const here = new URL(location.href);
    here.searchParams.delete("action");
    history.replaceState(history.state, "", here.pathname + here.search);
  }
  if (opened) runAction(route, segments, opened, reloadPage, NO_ROW, data.title || null);
}

// openActionForm collects an action's inputs and then performs it.
function openActionForm(route, segments, action, onDone, title = null) {
  const name = title || segments[segments.length - 1];
  const fields = buildCreateForm({ label: action.label, fields: action.fields });
  // An action whose form is Google's steps (Action.Steps, #1858): Create
  // application profile's type first, then Next to its details. The
  // steps after the first open with Next.
  const actionSteps = action.steps
    ? createSteps(fields.nodes, action.steps === "next" ? "Next" : "Continue", { foldRest: true })
    : { nodes: fields.nodes, revealInvalid() {} };
  const error = el("p", { class: "form-error", role: "alert", hidden: true });
  let submitting = false;

  const { dialog, close } = openModal({
    labelledBy: "action-title",
    canClose: () => !submitting,
  });

  const primary = el("button", {
    type: "submit",
    class: action.destructive ? "primary danger" : "primary",
    text: action.submit || action.label,
  });
  const cancel = el("button", { type: "button", class: "secondary", text: "Cancel",
                                onclick: () => close() });

  // A checked box that puts something at risk — Replace the destination —
  // asks first, and wants the thing at risk named back (#790).
  const risky = (values) => (action.fields || []).find((f) =>
    f.type === "checkbox" && f.confirm && values[f.name] === "true");

  const submit = async (e) => {
    e.preventDefault();
    error.hidden = true;
    if (submitting || !fields.validate()) { actionSteps.revealInvalid(); return; }
    const values = fields.values();
    // An action that changes what a resource does — a Pub/Sub Seek changes
    // which messages are redelivered — asks for the resource's name back
    // before it is sent (#787). Cancelled, the form stays open.
    if (action.confirm) {
      await confirmDestructive({
        title: `${action.label}: ${name}?`,
        detail: action.confirm,
        confirmWord: name,
        confirmLabel: action.label,
        consequence: "",
        onConfirm: async () => {
          const failure = await perform(values);
          if (failure) throw failure;
        },
      });
      return;
    }
    const risk = risky(values);
    if (risk) {
      const word = (values[risk.confirmWith] || "").trim() || name;
      // Cancelled, the form stays open with what was typed. Refused, the
      // API's message is shown in both dialogs.
      await confirmDestructive({
        title: `${action.label}: replace ${word}?`,
        detail: risk.confirm,
        confirmWord: word,
        confirmLabel: "Replace",
        onConfirm: async () => {
          const failure = await perform(values);
          if (failure) throw failure;
        },
      });
      return;
    }
    await perform(values);
  };

  // perform sends the action; it returns the error it failed with, having
  // shown it on the form, or null.
  const perform = async (values) => {
    submitting = true;
    primary.disabled = true;
    const op = recordOperation(`${action.label} ${name}`);
    try {
      const files = fields.files();
      const res = files.length
        ? await sendActionWithFile(route, { Path: segments, Action: action.id, Values: values }, files[0].file)
        : await send(
          `/api/actions/${route.service}?project=${encodeURIComponent(currentProject())}`,
          "POST", { Path: segments, Action: action.id, Values: values });
      op.succeeded("", res.operation);
      submitting = false;
      notify(segments.length ? `${action.label} applied to ${name}` : `${action.label} done`);
      onDone();
      // An action that answers with rows — a pull — keeps its dialog open to
      // show them. Closing it would be the pull done and its messages thrown
      // away, which for "Pull and ack" means gone for good.
      if (res.result) {
        setChildren(dialog, el("div", { class: "modal-body", id: "action-result" },
          el("h2", { id: "action-title", text: `${action.label}: ${name}` }),
          resultTable(res.result, { route, path: segments }),
          el("div", { class: "modal-actions" },
            el("button", { type: "button", class: "primary", text: "Close", onclick: () => close() }))));
        return null;
      }
      close();
      return null;
    } catch (err) {
      op.failed(err.message, err.operation);
      error.textContent = err.message;
      error.hidden = false;
      submitting = false;
      primary.disabled = false;
      return err;
    }
  };

  dialog.append(el("form", { class: "modal-body", novalidate: true, onsubmit: submit },
    el("h2", { id: "action-title", text: segments.length ? `${action.label} for ${name}` : action.label }),
    error,
    ...actionSteps.nodes,
    el("div", { class: "modal-actions" }, cancel, primary)));
  fields.focusFirst();
}

// listActionButtons are the list screen's own actions (#1289), on no
// existing resource: Cloud Run's Create service from source. Each opens its
// form and is sent with an empty path, which the upload route checks against
// the listing's actions.
function listActionButtons(route, data, onDone) {
  // A list action the listing also offers on its selection (#1864) is
  // drawn once, as the selection's button.
  const onSelection = new Set((data.selectActions || []).map((a) => a.id));
  return (data.actions || []).filter((a) => !onSelection.has(a.id)).map((a) => el("button", {
    type: "button", class: "secondary", id: `list-action-${a.id}`, text: a.label,
    // One that is a link opens its page: the Datastore page's Indexes
    // (#1846).
    onclick: () => (a.href ? navigate(a.href) : openActionForm(route, [], a, onDone, data.noun || route.title)),
  }));
}

// sendActionWithFile performs an action whose form holds a file (#999):
// BigQuery's Load from a file. The request and the file go in one multipart
// body to the action upload route, which streams the file to the service
// under the upload limit in Settings. A file over that limit is refused here
// before it is sent, as an object upload is; the server refuses it anyway.
async function sendActionWithFile(route, request, file) {
  let limit = null;
  try { limit = (await api("/api/settings")).uploadLimitBytes; } catch { /* the server checks */ }
  if (limit && file.size > limit) {
    throw new Error(`${file.name} is larger than the upload limit of ${formatBytes(limit)}; change it in Settings`);
  }
  const form = new FormData();
  form.append("request", JSON.stringify(request));
  form.append("file", file, file.name);
  const res = await fetch(`/api/actions/${route.service}/upload?project=${encodeURIComponent(currentProject())}`,
    { method: "POST", body: form, credentials: "same-origin", headers: { Accept: "application/json" } });
  const data = await res.json().catch(() => ({}));
  if (!res.ok) {
    const e = new Error(data.error || res.statusText);
    e.operation = data.operation;
    throw e;
  }
  return data;
}

// permissionsNote is what every Permissions tab says first: the policy is
// stored and nothing is enforced (#793, ADR-0006). The words and the link
// come from the server, which sets them on every Permissions section.
function permissionsNote(view) {
  view = view || {};
  return el("div", { class: "card permissions-note", role: "note", id: "permissions-note" },
    el("p", { text: view.note || "" }),
    view.link
      ? el("p", {}, el("a", { href: view.link, target: "_blank", rel: "noopener noreferrer",
          text: "What CloudBurrow stores and enforces for this service" }))
      : null);
}

// openGrantForm is Grant access: principals and a role, added to the policy
// the page read. Cancel closes it at once, with nothing sent and nothing asked
// (#783's rule); a refusal is shown on the form in the service's words.
function openGrantForm(view, name, change, onDone) {
  const fields = buildCreateForm({ label: "Grant access", fields: [
    { name: "members", label: "New principals", type: "textarea", required: true,
      help: view.principalHelp || "" },
    { name: "role", label: "Role", type: "text", required: true, help: view.roleHelp || "" },
    ...(view.conditions ? conditionFields(null) : []),
  ] });
  const error = el("p", { class: "form-error", role: "alert", hidden: true });
  let submitting = false;
  const { dialog, close } = openModal({ labelledBy: "grant-title", canClose: () => !submitting });
  const primary = el("button", { type: "submit", class: "primary", text: "Save" });
  const cancel = el("button", { type: "button", class: "secondary", text: "Cancel",
                                onclick: () => close() });

  const submit = async (e) => {
    e.preventDefault();
    error.hidden = true;
    if (submitting || !fields.validate()) return;
    const values = fields.values();
    const members = values.members.split(/[\s,]+/).map((m) => m.trim()).filter(Boolean);
    submitting = true;
    primary.disabled = true;
    const op = recordOperation(`Grant ${values.role} on ${name}`);
    try {
      const res = await change({ Grant: { Members: members, Role: values.role,
        Condition: view.conditions ? conditionFrom(values) : null } });
      op.succeeded("", res.operation);
      submitting = false;
      close();
      notify(`Granted ${values.role} on ${name}`);
      onDone();
    } catch (err) {
      op.failed(err.message, err.operation);
      error.textContent = err.message;
      error.hidden = false;
      submitting = false;
      primary.disabled = false;
    }
  };

  dialog.append(el("form", { class: "modal-body", novalidate: true, onsubmit: submit },
    el("h2", { id: "grant-title", text: `Grant access to ${name}` }),
    el("p", { class: "form-help", text: view.note || "" }),
    error,
    ...fields.nodes,
    el("div", { class: "modal-actions" }, cancel, primary)));
  fields.focusFirst();
}

// conditionFields are an IAM Condition's fields (#1742), as Google's console
// asks for them: a title, a description and the CEL expression, all empty
// for no condition.
function conditionFields(c) {
  return [
    { name: "conditionTitle", label: "Condition title", type: "text", default: c ? c.title : "",
      help: "Optional. A condition needs a title and an expression; leave both empty for none." },
    { name: "conditionDescription", label: "Condition description", type: "text", default: c ? (c.description || "") : "" },
    { name: "conditionExpression", label: "Condition expression (CEL)", type: "textarea", default: c ? c.expression : "",
      help: 'Such as resource.name == "projects/PROJECT_ID/databases/DATABASE_ID" or ' +
        'request.time < timestamp("2027-01-01T00:00:00Z"). Evaluated on resource.name, resource.type, ' +
        "resource.service and request.time." },
  ];
}

// conditionFrom is the condition the form's fields name, or null for none.
function conditionFrom(values) {
  const c = { title: (values.conditionTitle || "").trim(), description: (values.conditionDescription || "").trim(),
              expression: (values.conditionExpression || "").trim() };
  return c.title || c.description || c.expression ? c : null;
}

// openConditionForm is Edit condition (#1742): one principal's role moved to
// the binding with the condition the form names (none when it is empty), in
// one SetIamPolicy with the etag the page read.
function openConditionForm(row, name, change, onDone, resource = "") {
  const fields = buildCreateForm({ label: "Edit condition", fields: conditionFields(row.conditional) });
  const error = el("p", { class: "form-error", role: "alert", hidden: true });
  let submitting = false;
  // Run Linter (#1756), as Google's condition editor runs it: the IAM API's
  // iamPolicies.lintPolicy of the expression, an error or warning icon for
  // each result with its details on hover, run again while the expression
  // is edited once it has run; Save is refused while an error stands. The
  // policy's full resource name (the Permissions view's resource) goes as
  // LintPolicy's full_resource_name.
  const lintList = el("ul", { class: "lint-results", id: "lint-results", "aria-live": "polite" });
  let linted = false, lintErrors = 0, lintTimer = null, lintSeq = 0;
  const lint = async () => {
    const values = fields.values();
    const expression = (values.conditionExpression || "").trim();
    const seq = ++lintSeq;
    if (!expression) {
      lintErrors = 0;
      setChildren(lintList);
      return;
    }
    try {
      const res = await send("/api/iam/lint", "POST",
        { resource, title: (values.conditionTitle || "").trim(), expression });
      if (seq !== lintSeq) return;
      const results = res.results || [];
      lintErrors = results.filter((r) => r.severity === "ERROR").length;
      setChildren(lintList, ...(results.length ? results.map((r) => {
        const kind = r.severity === "ERROR" ? "error" : "warning";
        const unit = (r.unit || "").replace(/^LintValidationUnits\//, "");
        return el("li", { class: `lint-result lint-${kind}`, title: `${unit}: ${r.message}` },
          el("span", { class: "lint-icon", "aria-hidden": "true", text: kind === "error" ? "\u26D4" : "\u26A0" }),
          el("span", { class: "lint-severity", text: r.severity }),
          el("span", { class: "lint-where", text: ` at character ${r.position} (${unit}): ` }),
          el("span", { class: "lint-message", text: r.message }));
      }) : [el("li", { class: "lint-result lint-ok", text: "No issues found." })]));
      announce(results.length ? `${results.length} linter results, ${lintErrors} errors` : "The linter found no issues");
    } catch (err) {
      if (seq !== lintSeq) return;
      setChildren(lintList, el("li", { class: "lint-result lint-error", text: err.message }));
    }
  };
  const runLinter = el("button", { type: "button", class: "secondary", id: "run-linter", text: "Run Linter",
    onclick: async () => { linted = true; await lint(); } });
  const { dialog, close } = openModal({ labelledBy: "condition-title", canClose: () => !submitting });
  const primary = el("button", { type: "submit", class: "primary", text: "Save" });
  const cancel = el("button", { type: "button", class: "secondary", text: "Cancel", onclick: () => close() });
  const submit = async (e) => {
    e.preventDefault();
    error.hidden = true;
    if (submitting || !fields.validate()) return;
    if (linted) await lint();
    if (lintErrors > 0) {
      error.textContent = "The linter found errors in the expression: fix them before saving.";
      error.hidden = false;
      return;
    }
    submitting = true;
    primary.disabled = true;
    const op = recordOperation(`Edit the condition of ${row.member}'s ${row.role} on ${name}`);
    try {
      const res = await change({ EditCondition: { Member: row.member, Role: row.role,
        From: row.conditional, To: conditionFrom(fields.values()) } });
      op.succeeded("", res.operation);
      submitting = false;
      close();
      notify(`Changed the condition of ${row.member}'s ${row.role}`);
      onDone();
    } catch (err) {
      op.failed(err.message, err.operation);
      error.textContent = err.message;
      error.hidden = false;
      submitting = false;
      primary.disabled = false;
    }
  };
  dialog.append(el("form", { class: "modal-body", novalidate: true, onsubmit: submit },
    el("h2", { id: "condition-title", text: `Condition of ${row.member}'s ${row.role}` }),
    error,
    ...fields.nodes,
    el("div", { class: "action-bar" }, runLinter),
    lintList,
    el("div", { class: "modal-actions" }, cancel, primary)));
  const expression = dialog.querySelector("#f-conditionExpression");
  if (expression) {
    expression.addEventListener("input", () => {
      if (!linted) return;
      clearTimeout(lintTimer);
      lintTimer = setTimeout(lint, 300);
    });
  }
  fields.focusFirst();
}

// openTestPermissionsForm is Test permissions (#1242): permissions in, and
// for each the service's answer, held or not, as TestIamPermissions gives
// it. The dialog stays open so another set can be tried.
function openTestPermissionsForm(view, name, ask) {
  const fields = buildCreateForm({ label: "Test permissions", fields: [
    { name: "permissions", label: "Permissions", type: "textarea", required: true,
      help: "One or more permissions, separated by commas or new lines, such as storage.buckets.get. " +
        "The answer is the service's TestIamPermissions: which of them the caller holds." },
  ] });
  const error = el("p", { class: "form-error", role: "alert", hidden: true });
  const results = el("div", { id: "test-permissions-result", "aria-live": "polite" });
  let submitting = false;
  const { dialog, close } = openModal({ labelledBy: "test-permissions-title", canClose: () => !submitting });
  const primary = el("button", { type: "submit", class: "primary", text: "Test" });
  const done = el("button", { type: "button", class: "secondary", text: "Close", onclick: () => close() });

  const submit = async (e) => {
    e.preventDefault();
    error.hidden = true;
    if (submitting || !fields.validate()) return;
    const perms = fields.values().permissions.split(/[\s,]+/).map((p) => p.trim()).filter(Boolean);
    submitting = true;
    primary.disabled = true;
    try {
      const res = await ask(perms);
      const held = new Set(res.permissions || []);
      setChildren(results, el("table", { class: "table", id: "test-permissions-table" },
        el("thead", {}, el("tr", {}, el("th", { text: "Permission" }), el("th", { text: "Result" }))),
        el("tbody", {}, ...(res.asked || perms).map((p) => el("tr", {},
          el("td", { class: "mono", text: p }),
          el("td", { text: held.has(p) ? "Held" : "Not held" }))))));
      announce(`${held.size} of ${(res.asked || perms).length} permissions held`);
    } catch (err) {
      setChildren(results);
      error.textContent = err.message;
      error.hidden = false;
    }
    submitting = false;
    primary.disabled = false;
  };

  dialog.append(el("form", { class: "modal-body", novalidate: true, onsubmit: submit },
    el("h2", { id: "test-permissions-title", text: `Test permissions on ${name}` }),
    el("p", { class: "form-help", text: view.note || "" }),
    error,
    ...fields.nodes,
    results,
    el("div", { class: "modal-actions" }, done, primary)));
  fields.focusFirst();
}

// resultTable draws the rows an action answered with.
//
// at, when given, is { route, path }: where a row's own actions are sent, as
// a pulled message's Ack is sent to its subscription with the message's ack
// ID (#1847). Each is performed once, its button then saying so.
function resultTable(listing, at = null) {
  const cols = listing.columns || [];
  const items = listing.items || [];
  const acts = at && items.some((r) => (r.actions || []).length);
  const rowButtons = (r) => el("td", { class: "row-actions" }, ...(r.actions || []).map((a) => {
    const b = el("button", { type: "button", class: "secondary", text: a.label,
      "aria-label": `${a.label} ${r.name}` });
    b.onclick = async () => {
      b.disabled = true;
      try {
        await runAction(at.route, at.path, a, () => {}, NO_ROW, r.name);
        b.textContent = a.done || `${a.label}ed`;
      } catch { b.disabled = false; }
    };
    return b;
  }));
  return el("div", {},
    listing.note ? el("p", { class: "form-help", id: "action-result-note", text: listing.note }) : null,
    items.length
      ? el("table", { class: "table", id: "action-result-table" },
          el("thead", {}, el("tr", {},
            el("th", { text: listing.nameColumn || "Name" }), ...cols.map((c) => el("th", { text: c })),
            acts ? el("th", { "aria-label": "Actions" }) : null)),
          el("tbody", {}, ...items.map((r) => el("tr", {},
            // A row that names something with a page of its own links to
            // it: a BigQuery load's job, in Job history (#993).
            el("td", {}, r.link ? el("a", { href: r.link, text: r.name }) : r.name),
            ...cols.map((c) => el("td", { text: (r.fields || {})[c] || "" })),
            acts ? rowButtons(r) : null))))
      : null);
}

// --- objects: upload, download, preview, delete (#295) ------------------
//
// Bytes move over routes of their own, streamed both ways. A preview is
// fetched and drawn as text or as an image, never as the document the object
// claims to be: the server sends it as text/plain or as a raster image with a
// CSP forbidding script, and the page puts the text in a <pre> by
// textContent, so an HTML object is shown as its source.

const PREVIEW_LIMIT = 1 << 20;
const PREVIEW_IMAGES = ["image/png", "image/jpeg", "image/gif", "image/webp"];

// Show versions on an object list (#853), kept for the viewer across pages
// as the Cloud Storage console keeps it.
const VERSIONS_KEY = "cloudburrow.storage.versions";

// The Objects tab's Show list (#1843), for this page view.
const OBJECTS_SHOW_LIVE = "Live objects only";
const OBJECTS_SHOW_SOFT = "Soft-deleted objects only";
let objectsShow = OBJECTS_SHOW_LIVE;

function objectsShowPicker(onChange) {
  const select = el("select", { id: "objects-show", name: "show" },
    ...[OBJECTS_SHOW_LIVE, OBJECTS_SHOW_SOFT].map((o) =>
      el("option", { value: o, text: o, selected: o === objectsShow })));
  select.addEventListener("change", () => { objectsShow = select.value; onChange(); });
  return el("label", { class: "objects-show", for: "objects-show" }, "Show ", select);
}

function versionsToggle(onChange) {
  const box = el("input", { type: "checkbox", id: "object-versions-toggle",
    checked: readStored(VERSIONS_KEY, false) });
  box.addEventListener("change", () => {
    writeStored(VERSIONS_KEY, box.checked);
    onChange();
  });
  return el("label", { class: "versions-toggle", for: "object-versions-toggle" }, box, " Show versions");
}

function objectQuery(path) {
  const q = new URLSearchParams({ project: currentProject() });
  for (const seg of path) q.append("name", seg);
  return q;
}

function previewable(item) {
  const t = (item.contentType || "").split(";")[0].trim().toLowerCase();
  if (item.size > PREVIEW_LIMIT) return false;
  return PREVIEW_IMAGES.includes(t) || t.startsWith("text/") || t.endsWith("json") ||
    t.endsWith("+xml") || t === "application/xml" || t === "image/svg+xml" ||
    t === "application/javascript" || t.endsWith("yaml") || t === "application/x-ndjson";
}

function objectActions(route, item, refresh, row) {
  const q = objectQuery(item.object);
  const label = item.object.join("/");
  const actions = [{
    label: "Download",
    run: () => {
      // A plain navigation to an attachment: the browser streams it to disk
      // and the page stays where it is.
      const a = el("a", { href: `/api/objects/${route.service}/download?${q}`, download: "" });
      document.body.append(a);
      a.click();
      a.remove();
    },
  }];
  if (previewable(item)) {
    actions.push({ label: "Preview", run: () => previewObject(route, q, label) });
  }
  actions.push({
    label: "Delete", destructive: true,
    run: () => confirmDestructive({
      title: `Delete ${label}?`,
      confirmWord: item.name,
      onConfirm: async () => {
        const op = recordOperation(`Delete ${label}`);
        row.start();
        try {
          const res = await send(`/api/objects/${route.service}?${q}`, "DELETE");
          op.succeeded("", res.operation);
          notify(`Deleted ${label}`);
          refresh();
        } catch (err) {
          op.failed(err.message, err.operation);
          notify(`Could not delete ${label}: ${err.message}`, "error");
          throw err;
        } finally {
          row.end();
        }
      },
    }),
  });
  return actions;
}

async function previewObject(route, q, label) {
  const { dialog, close } = openModal({ labelledBy: "preview-title" });
  const title = el("h2", { id: "preview-title", text: `Preview: ${label}` });
  const body = el("div", { class: "modal-body", id: "object-preview" }, title, loadingState(3));
  dialog.append(body);
  const closeBtn = el("div", { class: "modal-actions" },
    el("button", { type: "button", class: "primary", text: "Close", onclick: () => close() }));
  try {
    const res = await fetch(`/api/objects/${route.service}/preview?${q}`, { credentials: "same-origin" });
    if (!res.ok) {
      let msg = res.statusText;
      try { msg = (await res.json()).error || msg; } catch { /* not JSON */ }
      throw new Error(msg);
    }
    const type = (res.headers.get("Content-Type") || "").split(";")[0];
    let content;
    if (PREVIEW_IMAGES.includes(type)) {
      // The same URL again, as an image source: the shell's CSP allows
      // same-origin images and nothing looser, and the server vets the
      // second request exactly as it vetted the first.
      content = el("img", { class: "object-preview-image", src: `/api/objects/${route.service}/preview?${q}`,
        alt: `Preview of ${label}` });
    } else {
      content = el("pre", { class: "mono object-preview-text", text: await res.text() });
    }
    setChildren(body, title, content, closeBtn);
  } catch (err) {
    setChildren(body, title, el("p", { class: "form-error", role: "alert", text: err.message }), closeBtn);
  }
}

function uploadControl(route, prefix, reload) {
  // Google's Objects tab offers Upload files and Upload folder (#1843). A
  // folder's files keep their paths under it; each file is its own upload,
  // through the same route, so the limit and the ledger are per file.
  const input = el("input", { type: "file", id: "object-upload-input", hidden: true, multiple: true,
    "aria-label": "Choose files to upload" });
  const folderInput = el("input", { type: "file", id: "object-upload-folder-input", hidden: true,
    "aria-label": "Choose a folder to upload" });
  folderInput.setAttribute("webkitdirectory", "");
  const status = el("span", { class: "status", id: "object-upload-status" });
  const button = el("button", { type: "button", class: "secondary", id: "object-upload",
    text: "Upload files", onclick: () => input.click() });
  const folderButton = el("button", { type: "button", class: "secondary", id: "object-upload-folder",
    text: "Upload folder", onclick: () => folderInput.click() });
  const uploadOne = async (file, limit) => {
    const rel = (file.webkitRelativePath || "").split("/").filter(Boolean);
    const dirs = rel.length > 1 ? rel.slice(0, -1) : [];
    const into = [...prefix, ...dirs];
    const where = [...into, file.name].join("/");
    if (limit && file.size > limit) {
      notify(`${file.name} is larger than the upload limit of ${formatBytes(limit)}; change it in Settings`, "error");
      return false;
    }
    const form = new FormData();
    form.append("file", file, file.name);
    const op = recordOperation(`Upload ${where}`);
    status.textContent = `Uploading ${file.name}…`;
    try {
      const res = await fetch(`/api/objects/${route.service}/upload?${objectQuery(into)}`,
        { method: "POST", body: form, credentials: "same-origin" });
      const data = await res.json().catch(() => ({}));
      if (!res.ok) {
        const e = new Error(data.error || res.statusText);
        e.operation = data.operation;
        throw e;
      }
      op.succeeded("", data.operation);
      notify(`Uploaded ${data.uploaded}`);
      return true;
    } catch (err) {
      op.failed(err.message, err.operation);
      notify(`Could not upload ${file.name}: ${err.message}`, "error");
      return false;
    }
  };
  const uploadAll = async (from) => {
    const files = [...(from.files || [])];
    if (!files.length) return;
    // Checked here so an oversized file is refused before it is sent; the
    // server refuses it regardless.
    let limit = null;
    try { limit = (await api("/api/settings")).uploadLimitBytes; } catch { /* the server checks */ }
    button.disabled = true;
    folderButton.disabled = true;
    let any = false;
    try {
      for (const file of files) any = (await uploadOne(file, limit)) || any;
    } finally {
      button.disabled = false;
      folderButton.disabled = false;
      status.textContent = "";
      from.value = "";
    }
    if (any) reload();
  };
  input.addEventListener("change", () => uploadAll(input));
  folderInput.addEventListener("change", () => uploadAll(folderInput));
  return el("div", { class: "card-actions" }, button, folderButton, input, folderInput, status);
}

// The upload limit, a server-side setting: it is what the server enforces,
// so it lives there rather than in this browser.
async function initUploadSetting() {
  const input = document.getElementById("upload-limit");
  if (!input) return;
  try {
    input.value = String(Math.round((await api("/api/settings")).uploadLimitBytes / (1 << 20)));
  } catch { input.disabled = true; return; }
  input.addEventListener("change", async () => {
    const mib = Number(input.value);
    try {
      const res = await send("/api/settings", "PUT", { uploadLimitBytes: Math.round(mib * (1 << 20)) });
      input.value = String(Math.round(res.uploadLimitBytes / (1 << 20)));
      notify(`Upload limit set to ${formatBytes(res.uploadLimitBytes)}`);
    } catch (err) {
      notify(`Could not change the upload limit: ${err.message}`, "error");
    }
  });
}

// revealValue asks for a resource's secret value and shows it once.
//
// The value is fetched when the button is pressed, never with the page, and it
// is not written into the URL, the history or any listing. It is dropped when
// the dialog closes, because a secret left on screen behind whatever the
// operator does next is a secret on a shared screen.
async function revealValue(route, segments, label) {
  const name = segments[segments.length - 1];
  let res;
  try {
    res = await send(
      `/api/reveal/${route.service}?project=${encodeURIComponent(currentProject())}`,
      "POST", { Path: segments });
  } catch (err) {
    return notify(`Could not read ${name}: ${err.message}`, "error");
  }

  const { dialog, close } = openModal({ labelledBy: "reveal-title" });
  const value = el("pre", { class: "mono reveal-value", text: res.value });
  dialog.append(el("div", { class: "modal-body" },
    el("h2", { id: "reveal-title", text: res.label || label }),
    el("p", { class: "form-help",
      text: "This access was recorded in Activity. Close this dialog when you " +
            "are done; the value is not kept." }),
    value,
    el("div", { class: "modal-actions" },
      copyButton(res.value, `the value of ${name}`),
      el("button", { type: "button", class: "primary", text: "Done",
                     onclick: () => close() }))));
  dialog.querySelector(".modal-actions button:last-child").focus();
}

// showOneTime shows a value the API returned once, such as a new HMAC key's
// secret (#792), in a dialog of its own.
//
// The value is in this dialog and nowhere else: not the URL, the history, a
// notification, the Activity panel or a listing, and it is dropped with the
// dialog. A stray click on the backdrop does not close it, since closing it
// is the last chance to copy the value.
function showOneTime(once) {
  const { dialog, close } = openModal({
    labelledBy: "one-time-title",
    canClose: (reason) => reason !== "backdrop",
  });
  const props = (once.properties || []).filter((p) => p.value);
  dialog.append(el("div", { class: "modal-body", id: "one-time" },
    el("h2", { id: "one-time-title", text: once.title }),
    props.length
      ? el("dl", { class: "one-time-properties" }, ...props.flatMap((p) => [
          el("dt", { text: p.label }),
          el("dd", { class: "mono", text: p.value }),
        ]))
      : null,
    el("p", { class: "form-help", text: once.label }),
    el("pre", { class: "mono reveal-value", id: "one-time-value", text: once.value }),
    el("p", { class: "one-time-note", role: "alert", text: once.note }),
    el("div", { class: "modal-actions" },
      copyButton(once.value, once.label.toLowerCase()),
      el("button", { type: "button", class: "primary", text: "Done",
                     onclick: () => close() }))));
  dialog.querySelector(".modal-actions button").focus();
}

// editFromRow opens a row's edit form from its menu (#1863): the form is
// the one the resource's page offers, read from its detail.
async function editFromRow(route, segments, onDone) {
  const project = currentProject();
  try {
    const data = await api(`/api/detail/${route.service}?project=${encodeURIComponent(project)}` +
      segments.map((sg) => `&name=${encodeURIComponent(sg)}`).join(""));
    if (!data.edit) throw new Error("This resource has nothing that can be edited.");
    openEditForm(route, segments, data.edit, onDone);
  } catch (err) {
    notify(String(err.message), "error");
  }
}

// openEditForm changes a resource in place.
//
// It reuses the create form wholesale — the same validation, the same grouping,
// the same discard prompt — because an edit form that looked or behaved
// differently from a create form would be a second form implementation to keep
// in step with the first.
function openEditForm(route, segments, spec, onDone) {
  const name = segments[segments.length - 1];
  const fields = buildCreateForm(spec);
  const error = el("p", { class: "form-error", role: "alert", hidden: true });
  let submitting = false;

  // Dismissing mid-save would leave the change completing against a form
  // that no longer exists, and the operator with no idea whether it applied.
  const { dialog, close } = openModal({
    labelledBy: "edit-title",
    canClose: () => !submitting,
  });

  const primary = el("button", { type: "submit", class: "primary", text: spec.submit || spec.label || "Save" });
  const cancel = el("button", { type: "button", class: "secondary", text: "Cancel",
                                onclick: () => close() });

  const submit = async (e) => {
    e.preventDefault();
    error.hidden = true;
    if (submitting || !fields.validate()) return;
    submitting = true;
    primary.disabled = true;
    const op = recordOperation(`Update ${name}`);
    try {
      const res = await send(
        `/api/resources/${route.service}?project=${encodeURIComponent(currentProject())}`,
        "PATCH", { Path: segments, Values: fields.values() });
      op.succeeded("", res.operation);
      submitting = false;
      close();
      notify(`Updated ${name}`);
      onDone();
    } catch (err) {
      // The API's own message, on the form rather than in a snackbar: the
      // reason a change was refused belongs where the change was made.
      op.failed(err.message, err.operation);
      error.textContent = err.message;
      error.hidden = false;
      submitting = false;
      primary.disabled = false;
    }
  };

  dialog.append(el("form", { class: "modal-body", novalidate: true, onsubmit: submit },
    el("h2", { id: "edit-title", text: `${spec.title || spec.label || "Edit"} ${name}` }),
    spec.note ? el("p", { class: "form-help", text: spec.note }) : null,
    error,
    ...fields.nodes,
    el("div", { class: "modal-actions" }, cancel, primary)));
  fields.focusFirst();
}

async function renderList(view, route) {
  const project = new URLSearchParams(location.search).get("project") || "";
  // A filter the service evaluates (#1228), kept in the URL so a filtered
  // screen can be reloaded and shared.
  const serverFilter = new URLSearchParams(location.search).get("filter") || "";
  const listURL = `/api/resources/${route.service}?project=${encodeURIComponent(project)}` +
    (serverFilter ? `&filter=${encodeURIComponent(serverFilter)}` : "");

  const header = [pageHeader(route.title, project ? `Project ${project}` : "All projects")];
  const cancel = new AbortController();
  setChildren(view, ...header,
    loadingState(5, { what: route.title.toLowerCase(), onCancel: () => cancel.abort() }));

  let data;
  try {
    data = await api(listURL, { signal: cancel.signal });
  } catch (err) {
    setChildren(view, ...header,
      isCancelled(err)
        ? cancelledState(`${route.title} not loaded`, () => renderList(view, route))
        : errorState(`${route.title} unavailable`, String(err.message),
                     () => renderList(view, route)));
    return;
  }

  // The plural word for these rows, used by the filter, the empty state and
  // the announcements. Declared here because every branch below needs it.
  const noun = data.noun || route.title.toLowerCase();
  // What the screen says about its scope before its rows, such as the
  // project's Cloud Storage service account over its HMAC keys (#792).
  // A list screen drawn as tabs (#1843): Google's Cloud Storage Settings
  // has Project access, whose content is the summary, and
  // Interoperability, which holds the rows.
  if ((data.tabs || []).length) {
    const wanted = new URLSearchParams(location.search).get("tab");
    const tabs = data.tabs;
    const current = Math.max(0, tabs.findIndex((t) => t.id === wanted));
    const strip = el("div", { class: "tab-strip", role: "tablist", "aria-label": `${route.title} sections` },
      ...tabs.map((t, i) => el("button", {
        class: "tab" + (i === current ? " is-selected" : ""), role: "tab", id: `tab-${t.id}`,
        "aria-selected": i === current ? "true" : "false", tabindex: i === current ? "0" : "-1",
        text: t.label,
        onclick: () => {
          const url = new URL(location.href);
          url.searchParams.set("tab", t.id);
          history.replaceState(history.state, "", url);
          renderList(view, route);
        },
      })));
    header.push(strip);
    if (tabs[current].id !== data.listTab) {
      setChildren(view, ...header, noteBanner(tabs[current].note), ...listingSummary(data));
      announce(`${tabs[current].label} selected`);
      return;
    }
  } else if (!data.summaryOnly) {
    header.push(...listingSummary(data));
  }
  if (data.serverFilter) header.push(serverFilterBox(data.serverFilter, () => renderList(view, route)));
  if (data.scope) header.push(scopePicker(data.scope, () => renderList(view, route), (data.filters || []).map((f) => f.key)));
  (data.filters || []).forEach((f, i, all) =>
    header.push(scopePicker(f, () => renderList(view, route), all.slice(i + 1).map((x) => x.key))));

  // A screen that needs something from the user is not a broken screen. This
  // is rendered as a prompt rather than as an error, because a red failure
  // box for "choose a project" taught the user a working instance was broken.
  if (data.prompt) {
    setChildren(view, ...header, emptyState(`Choose a project`, data.prompt));
    announce(data.prompt);
    return;
  }

  // An unreachable backend is an error state, never an empty table: an empty
  // table says "you have none", which sends a developer to debug their code.
  if (data.unavailable) {
    setChildren(view, ...header,
      errorState(`${route.title} unavailable`, data.unavailable, () => renderList(view, route)));
    announce(`${route.title} unavailable`);
    return;
  }

  // A screen that is its summary cards alone, as Google's Cloud Build
  // Dashboard is a card per trigger (#1854).
  if (data.summaryOnly && (data.summary || []).length) {
    setChildren(headerActions(header), refreshButton(() => renderList(view, route)));
    setChildren(view, ...header, noteBanner(data.note),
      el("div", { class: "summary-cards" }, ...listingSummary(data)));
    announce(`${data.summary.length} ${noun} loaded`);
    return;
  }

  if (!data.items.length) {
    const caps = capabilityOf(route.service);
    // The copy follows what this screen can actually do.
    //
    // It used to say "Create one here" on every empty screen, including the ones
    // with no create control — Kubernetes, Events, Nodes — so the sentence named
    // a button that was not there and could not be. And the listing's own note,
    // which is where a provider explains what the screen is and is not, was
    // dropped on the empty path: the one path where it is the only thing on the
    // page.
    if (data.serverFilter && data.serverFilter.value) {
      setChildren(view, ...header, data.serverFilter.error ? null :
        emptyState(`No ${noun} match the filter`, "Change or clear the filter to see more."));
      announce(data.serverFilter.error ? `Filter refused` : `No ${noun} match the filter`);
      return;
    }
    const empty = emptyState(`No ${noun} yet`,
      caps.create
        ? "Create one here, or with an SDK, the CLI or gcloud — it will appear either way."
        : "Nothing here yet. This screen is read-only, so anything that appears " +
          "will have been created by an SDK, the CLI, or CloudBurrow itself.");
    if (caps.create) {
      empty.append(el("button", { class: "primary", text: caps.create.label,
        onclick: () => startCreate(route, caps.create, () => renderList(view, route)) }));
    }
    empty.append(...listActionButtons(route, data, () => renderList(view, route)));
    // The title line carries the same Create, and Refresh, as a list with
    // rows does (#1842).
    setChildren(headerActions(header),
      caps.create ? createButton(caps.create.label,
        () => startCreate(route, caps.create, () => renderList(view, route))) : null,
      refreshButton(() => renderList(view, route)));
    setChildren(view, ...header, noteBanner(data.note), empty);
    announce(`No ${noun}`);
    return;
  }

  renderTableInto(view, header, data, noun, () => renderList(view, route), route,
                  {
                    rowControls: true,
                    // An empty path is the list screen itself, which is what the
                    // page route reads it as.
                    pagePath: [],
                    // Lets the table reload its own rows without the screen
                    // being rebuilt around it, so sort, filter, page and
                    // scroll position survive a refresh.
                    refetch: () => api(listURL),
                  });
  announce(`${data.items.length} ${noun} loaded`);
}

// serverFilterBox is a list screen's filter in the service's own syntax
// (#1228): applying it puts it in the URL and lists again, and a refusal is
// shown under the box rather than as a broken screen.
function serverFilterBox(spec, reload) {
  const input = el("input", { type: "search", id: "server-filter", name: "filter",
    value: spec.value || "", placeholder: spec.placeholder || "",
    "aria-describedby": "server-filter-help" + (spec.error ? " server-filter-error" : ""),
    "aria-invalid": spec.error ? "true" : "false" });
  const apply = (value) => {
    const params = new URLSearchParams(location.search);
    if (value) params.set("filter", value); else params.delete("filter");
    const qs = params.toString();
    history.replaceState(history.state, "", location.pathname + (qs ? `?${qs}` : ""));
    reload();
  };
  return el("form", { class: "card server-filter", role: "search", "aria-label": "Filter",
                      onsubmit: (e) => { e.preventDefault(); apply(input.value.trim()); } },
    el("label", { for: "server-filter", text: "Filter" }),
    el("div", { class: "server-filter-row" },
      input,
      el("button", { type: "submit", class: "secondary", id: "server-filter-apply", text: "Apply" }),
      spec.value ? el("button", { type: "button", class: "secondary", id: "server-filter-clear",
                                  text: "Clear", onclick: () => apply("") }) : null),
    spec.help ? el("p", { class: "form-help", id: "server-filter-help", text: spec.help }) : null,
    spec.error ? el("p", { class: "form-error", id: "server-filter-error", role: "alert", text: spec.error }) : null);
}

// propertyValue is a property's <dd>: its value, as a link when the
// provider names the page it opens (#1554).
function propertyValue(prop) {
  const value = prop.link ? el("a", { href: prop.link, text: prop.value }) : prop.value;
  if (!prop.copy) return el("dd", {}, value);
  // A Copy button of its own, labelled as Google labels it: Copy URL,
  // Copy gsutil URI (#1843).
  const button = copyButton(prop.value, prop.label);
  button.textContent = prop.copy;
  button.classList.add("property-copy");
  return el("dd", { class: "has-copy" }, el("span", { class: "property-copy-value" }, value), button);
}

// listingSummary draws a listing's summary groups as the same property cards
// a resource page uses.
function listingSummary(data) {
  return (data.summary || []).filter((g) => (g.properties || []).length).map((group) =>
    el("div", { class: "card properties listing-summary" },
      group.heading ? el("h2", { text: group.heading }) : null,
      el("dl", {}, ...group.properties.flatMap((prop) => [
        el("dt", { text: prop.label }),
        propertyValue(prop),
      ]))));
}

// --- modals -----------------------------------------------------------
//
// One shell for every dialog. Focus is captured on open and returned to the
// control that opened it, Tab cycles inside the dialog instead of walking off
// behind it, and the page underneath is inert. Written once, because "a
// dialog that traps focus" and "a dialog that does not" are not two designs;
// they are one design and one bug.
//
// canClose is asked before every dismissal and is told which one it is: a
// stray backdrop click and a deliberate Escape are different intentions, and
// a form with typed input in it should survive one of them.

// MODAL_SIDE_NEXT draws the next dialog as a right-side panel, as BigQuery
// Studio draws Create dataset and Create table (#1861); openModal clears it.
let MODAL_SIDE_NEXT = false;

function openModal({ labelledBy, canClose = () => true }) {
  // A dialog still fading out goes at once when another opens. Left for its
  // exit, it holds the same IDs as the new one, so the new dialog's
  // aria-labelledby would name it by the old title, and a script looking for
  // the new dialog's fields or buttons would find the old, inert ones, which
  // vanish under it: back-to-back confirmations, such as the BigQuery
  // editor's Run statement, open within that exit.
  for (const old of document.querySelectorAll(".modal.is-closing")) old.remove();
  const dialog = el("div", { class: "modal", role: "dialog", "aria-modal": "true",
                             "aria-labelledby": labelledBy });
  if (MODAL_SIDE_NEXT) dialog.classList.add("is-side");
  MODAL_SIDE_NEXT = false;
  const opener = document.activeElement;

  // Only the nodes this modal marked are unmarked on close. inert set on an
  // ancestor cannot be cancelled on a descendant, so a second dialog that
  // blindly cleared the flag would wake the page up underneath the first.
  //
  // The live region is exempt. inert takes its subtree out of the
  // accessibility tree, which would silently swallow every announcement made
  // while a dialog is open — including the one that says the create
  // succeeded, which is made from inside the dialog.
  const inerted = [...document.body.children]
    .filter((n) => !n.inert && n.id !== "live" && n.id !== "snackbars");

  const focusable = () => [...dialog.querySelectorAll(
    "a[href], button, input, select, textarea, [tabindex]")]
    .filter((n) => !n.disabled && n.tabIndex !== -1 && !n.closest("[hidden]"));

  const close = (reason = "explicit") => {
    if (!canClose(reason)) return false;
    // Removed after the exit, but made inert and untouchable straight away:
    // a dialog that is on its way out must not still be clickable, and the
    // page behind it must come back immediately.
    dialog.classList.remove("is-open");
    dialog.classList.add("is-closing");
    dialog.inert = true;
    setTimeout(() => dialog.remove(), OVERLAY_EXIT_MS);
    for (const n of inerted) n.inert = false;
    // Back to the button that opened it, not to the top of the page: a
    // keyboard user who cancels a create should be where they started.
    if (opener && opener.isConnected && opener.focus) opener.focus();
    else document.getElementById("main").focus();
    // Announced rather than returned, because the three dismissal paths —
    // Escape, the backdrop and a button — all land here and a caller that
    // needs to know it is gone should not have to hook each of them.
    dialog.dispatchEvent(new CustomEvent("cb-closed"));
    return true;
  };

  dialog.addEventListener("keydown", (e) => {
    if (e.key === "Escape") { e.preventDefault(); close("escape"); return; }
    if (e.key !== "Tab") return;
    const items = focusable();
    if (items.length < 2) return;
    const first = items[0];
    const last = items[items.length - 1];
    if (e.shiftKey && document.activeElement === first) { e.preventDefault(); last.focus(); }
    else if (!e.shiftKey && document.activeElement === last) { e.preventDefault(); first.focus(); }
  });
  dialog.addEventListener("click", (e) => { if (e.target === dialog) close("backdrop"); });

  for (const n of inerted) n.inert = true;
  document.body.append(dialog);
  requestAnimationFrame(() => dialog.classList.add("is-open"));
  return { dialog, close };
}

// copyButton copies a value and says it did.
//
// The real console offers this on every identifier and endpoint. Here it
// matters most on a YAML pane, where the alternative is selecting a hundred
// lines by hand.
function copyButton(value, what) {
  const button = el("button", { class: "secondary", text: "Copy" });
  button.addEventListener("click", async () => {
    try {
      await navigator.clipboard.writeText(value);
      notify(`Copied ${what}`);
    } catch (err) {
      // The clipboard API needs a secure context and a user gesture, and
      // refuses in some browsers regardless. Saying so beats a button that
      // silently does nothing.
      notify(`Could not copy: ${err.message}`, "error");
    }
  });
  return button;
}

// spinner is the one indeterminate indicator in the console.
//
// Silent unless it is given a label: a decorative spinner beside text that
// already says what is happening should not be read out twice.
function spinner(label) {
  return el("span", {
    class: "spinner",
    role: label ? "status" : null,
    "aria-label": label || null,
    "aria-hidden": label ? null : "true",
  });
}

// setBusy puts a button in a busy state without taking its label away.
//
// A button whose text is swapped for "Working…" stops saying what it will do,
// and by the time it comes back the user has forgotten what they clicked.
function setBusy(button, busy) {
  if (!button) return;
  button.disabled = busy;
  button.classList.toggle("is-busy", busy);
  if (busy) {
    button.setAttribute("aria-busy", "true");
    if (!button.querySelector(".spinner")) button.prepend(spinner());
  } else {
    button.removeAttribute("aria-busy");
    const existing = button.querySelector(".spinner");
    if (existing) existing.remove();
  }
}

// --- the info panel ---------------------------------------------------
//
// The third region of a product page. Inspecting a row used to mean leaving
// the list for the detail route — losing the filter, the sort and the scroll
// position — and only five products implement Driller at all, so for every
// other one a row could not be inspected in any way.
//
// It shows what the provider actually returned for that row and nothing else:
// a panel that displayed a field the backend does not hold would be inventing
// the answer to the question it exists to answer.

const INFO_OPEN_KEY = "cloudburrow.infopanel";
let INFO_TOGGLE = null;

const infoPanelOpen = () => readStored(INFO_OPEN_KEY, false) === true;

function infoPanelHost() {
  const panel = document.getElementById("info-panel");
  if (panel && !panel.dataset.wired) {
    panel.dataset.wired = "true";
    panel.addEventListener("keydown", (e) => {
      if (e.key !== "Escape") return;
      e.stopPropagation();
      closeInfoPanel();
    });
  }
  return panel;
}

function closeInfoPanel() {
  writeStored(INFO_OPEN_KEY, false);
  const panel = infoPanelHost();
  if (panel) panel.hidden = true;
  document.documentElement.removeAttribute("data-info");
  if (INFO_TOGGLE && INFO_TOGGLE.isConnected) {
    INFO_TOGGLE.setAttribute("aria-expanded", "false");
    labelInfoToggle(INFO_TOGGLE, false);
    INFO_TOGGLE.focus();
  }
}

// labelInfoToggle names the info-panel toggle for its state. It is an icon
// on the title line (#1842); its name is the text Google's tooltip carries.
function labelInfoToggle(button, open) {
  const label = open ? "Hide info panel" : "Show info panel";
  button.title = label;
  button.innerHTML = '<svg viewBox="0 0 24 24" aria-hidden="true"><rect x="3" y="4" width="18" height="16" rx="2"/><path d="M15 4v16"/></svg>';
  button.append(el("span", { class: "sr-only", text: label }));
}

// drawInfoPanel fills the panel from one row, or says nothing is selected.
function drawInfoPanel(item, columns, route, onDone) {
  const panel = infoPanelHost();
  if (!panel || panel.hidden) return;

  if (!item) {
    return setChildren(panel,
      el("div", { class: "info-empty" },
        el("h2", { text: "Select a resource" }),
        el("p", { class: "unavailable",
                  text: "Choose a row to see what this instance holds for it." })));
  }

  const caps = capabilityOf(route.service);
  const actions = [
    ...(item.actions || []).map((a) => ({
      label: a.label, destructive: a.destructive,
      run: () => runAction(route, (item.target || []).length ? item.target : item.name, a, onDone),
    })),
    ...(caps.delete ? [{
      label: "Delete", destructive: true,
      run: () => deleteResource(route, item.name, onDone),
    }] : []),
  ];

  setChildren(panel,
    el("div", { class: "info-head" },
      el("h2", { text: item.name }),
      el("button", { class: "icon-button", "aria-label": "Close info panel",
                     onclick: () => closeInfoPanel(),
                     html: '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M6 6l12 12M18 6L6 18"/></svg>' })),
    item.status
      ? el("p", {}, el("span", { class: "status", "data-state": stateOf(item.status) },
          el("span", { text: item.status })))
      : null,
    // Only the columns the listing itself declares, so the panel can never
    // show a field the provider did not return.
    el("dl", { class: "info-fields" }, ...columns.flatMap((c) => {
      const value = (item.fields || {})[c];
      return value ? [el("dt", { text: c }), el("dd", { text: value })] : [];
    })),
    item.link
      ? el("p", {}, el("a", { href: item.link, text: "Open" }))
      : (item.opens || []).length
        ? el("p", {}, el("a", { href: detailHref(route, item.opens), text: "Open" }))
      : caps.detail
        ? el("p", {}, el("a", { href: detailHref(route, item.name), text: "Open" }))
        : null,
    actions.length
      ? el("div", { class: "info-actions" }, ...actions.map((a) =>
          el("button", {
            class: "secondary" + (a.destructive ? " danger" : ""),
            text: a.label, onclick: () => a.run(),
          })))
      : null);
}

// --- the query pane ---------------------------------------------------
//
// An editor over the transport at POST /api/query. Deliberately plain: no
// syntax highlighting, no autocomplete, no third-party editor component —
// the assets ship as they are written, and a CDN-loaded editor would break
// the one promise this console makes about working offline.
//
// What it does have is the backend's own error text, unchanged. A syntax
// error names the character; a read-only violation names the statement.
// Replacing either with "query failed" throws away the entire answer.

const QUERY_DRAFT_KEY = "cloudburrow.query";

function queryPane(route, segments, spec, onDone) {
  // A provider with no query language gets a form instead of a statement box.
  // Which one it is comes from the backend: "does this database have a query
  // language" is not a question the browser can answer, and a textarea over
  // Firestore would mean inventing a syntax nobody else accepts.
  if (spec && (spec.fields || []).length) {
    return queryFormPane(route, segments, spec, onDone);
  }
  const hint = spec && spec.hint;
  const draftKey = `${QUERY_DRAFT_KEY}.${route.service}.${segments.join("/")}`;

  const editor = el("textarea", {
    class: "query-editor mono", rows: "6", spellcheck: "false",
    "aria-label": "Statement",
    placeholder: "SELECT * FROM widgets LIMIT 10",
  });
  // The draft survives a navigation away and back, because losing a
  // half-written query to a misclick is the fastest way to stop using a
  // query pane. Per viewer and per resource; never sent anywhere.
  // A page that links here with ?query= (Log Analytics, #1423) opens the
  // editor on that statement instead of the draft.
  const linked = new URLSearchParams(location.search).get("query");
  editor.value = linked !== null ? linked : readStored(draftKey, "");
  editor.addEventListener("input", () => writeStored(draftKey, editor.value));

  const results = el("div", { class: "query-results" });
  const error = el("p", { class: "form-error", role: "alert", hidden: true });
  const run = el("button", { class: "primary", text: "Run" });
  const hintLine = hint ? el("p", { class: "unavailable", text: hint }) : null;

  // Read-write is a mode the user switches to, never something inferred from
  // the text (#798). It is not remembered: every visit starts read-only, so
  // the state that writes is always the one somebody chose just now.
  const write = spec && spec.write;
  let writing = false;
  let modeSwitch = null;
  if (write) {
    const option = (value, label) => {
      const input = el("input", { type: "radio", name: `query-mode-${draftKey}`, value });
      input.checked = value === "read-only";
      input.addEventListener("change", () => { if (input.checked) setMode(value === "read-write"); });
      return el("label", { class: "query-mode-option" }, input, el("span", { text: label }));
    };
    modeSwitch = el("fieldset", { class: "query-mode" },
      el("legend", { text: "Mode" }),
      option("read-only", "Read-only"),
      option("read-write", `${write.label || "Read-write"} (writes data)`));
  }
  // The words are the provider's where it gives them (#994): BigQuery's mode
  // runs DDL and scripts, with no transaction to name. Spanner's are the
  // defaults (#798).
  const runLabel = (write && write.run) || "Run DML";
  const setMode = (rw) => {
    writing = rw;
    run.textContent = rw ? runLabel : "Run";
    run.classList.toggle("danger", rw);
    editor.placeholder = rw ? ((write && write.placeholder) || "INSERT INTO widgets (id, name) VALUES (1, 'one')")
                            : "SELECT * FROM widgets LIMIT 10";
    if (hintLine) hintLine.textContent = rw ? write.hint : hint;
    error.hidden = true;
  };

  // The confirmation names the database the statement will change and shows
  // the statement, so what is about to be committed is on screen once more.
  // Cancel discards without asking (#783): nothing was typed into the dialog.
  const confirmWrite = (statement) => new Promise((settle) => {
    let decided = false;
    const { dialog, close } = openModal({ labelledBy: "dml-confirm-title" });
    const cancel = el("button", { type: "button", class: "secondary", text: "Cancel",
                                  onclick: () => close() });
    const confirm = el("button", { type: "submit", class: "primary danger", text: runLabel });
    dialog.append(el("form", { class: "modal-body",
        onsubmit: (e) => { e.preventDefault(); decided = true; close(); } },
      el("h2", { id: "dml-confirm-title", text: `Write to ${write.target}?` }),
      el("p", { text: write.confirm || "This statement runs in a read-write transaction and is committed." }),
      el("pre", { class: "mono confirm-detail", text: statement }),
      el("div", { class: "modal-actions" }, cancel, confirm)));
    dialog.addEventListener("cb-closed", () => settle(decided));
    confirm.focus();
  });

  const executeWrite = async (statement) => {
    if (!(await confirmWrite(statement))) return;
    setBusy(run, true);
    const started = performance.now();
    try {
      const data = await send(
        `/api/query/${route.service}?project=${encodeURIComponent(currentProject())}`,
        "POST", { Path: segments, Statement: statement, Mode: "read-write" });
      const took = Math.round(performance.now() - started);
      const n = data.rowCount || 0;
      // A provider whose write changes no rows, such as DDL, says what it
      // did instead of a count (#994).
      const text = data.message ? `${data.message} · ${took} ms`
                                : `${n} row${n === 1 ? "" : "s"} affected in ${write.target} · ${took} ms`;
      setChildren(results, el("p", { class: "unavailable", text }));
      announce(text);
    } catch (err) {
      // The backend's message: a Spanner constraint violation names the
      // row, a BigQuery refusal the statement's position.
      setChildren(results);
      error.textContent = err.message;
      error.hidden = false;
    } finally {
      setBusy(run, false);
    }
    if (onDone) onDone();
  };

  const execute = async () => {
    const statement = editor.value.trim();
    if (!statement) return;
    error.hidden = true;
    if (writing) return executeWrite(statement);
    setBusy(run, true);
    const started = performance.now();
    try {
      const data = await send(
        `/api/query/${route.service}?project=${encodeURIComponent(currentProject())}`,
        "POST", { Path: segments, Statement: statement });
      const listing = data.listing || {};
      const took = Math.round(performance.now() - started);

      if (!(listing.items || []).length) {
        setChildren(results,
          el("p", { class: "unavailable",
                    text: `No rows. ${took} ms.` }));
      } else {
        setChildren(results);
        renderTableInto(results, [
          el("p", { class: "unavailable",
                    text: `${listing.items.length} row${listing.items.length === 1 ? "" : "s"} · ${took} ms` }),
        ], listing, listing.noun || "rows", () => execute(), route, {});
      }
      announce(`Query returned ${(listing.items || []).length} rows`);
    } catch (err) {
      // The database said this. Saying it again in our own words would be
      // replacing the answer with a summary of the answer.
      setChildren(results);
      error.textContent = err.message;
      error.hidden = false;
    } finally {
      setBusy(run, false);
    }
    if (onDone) onDone();
  };

  run.addEventListener("click", execute);
  // Ctrl/Cmd+Enter runs, which is what every query surface binds it to.
  editor.addEventListener("keydown", (e) => {
    if ((e.metaKey || e.ctrlKey) && e.key === "Enter") {
      e.preventDefault();
      execute();
    }
  });

  return el("div", { class: "query-pane" },
    el("div", { class: "card" },
      modeSwitch,
      editor,
      el("div", { class: "card-actions" },
        run,
        el("button", { class: "secondary", text: "Clear",
          onclick: () => { editor.value = ""; writeStored(draftKey, ""); setChildren(results); error.hidden = true; } }),
        el("span", { class: "unavailable", text: "⌘/Ctrl + Enter to run" })),
      hintLine),
    error,
    results);
}

// queryFormPane builds a structured query from controls.
//
// Firestore, Datastore and Bigtable queries are structures, not text, so this
// reuses the create form's builder: the same validation, the same grouping, the
// same required marks. Results render with the same table as every other
// listing, so sorting and filtering work here without being written twice.
function queryFormPane(route, segments, spec, onDone) {
  const fields = buildCreateForm({ label: spec.label || "Run", fields: spec.fields });
  const results = el("div", { class: "query-results" });
  const error = el("p", { class: "form-error", role: "alert", hidden: true });
  const run = el("button", { class: "primary", type: "submit", text: spec.label || "Run" });
  followTabLabels(fields.nodes, run, spec);

  const execute = async (e) => {
    if (e) e.preventDefault();
    error.hidden = true;
    if (!fields.validate()) return;
    setBusy(run, true);
    const started = performance.now();
    try {
      const data = await send(
        `/api/query/${route.service}?project=${encodeURIComponent(currentProject())}`,
        "POST", { Path: segments, Values: fields.values() });
      const listing = data.listing || {};
      const took = Math.round(performance.now() - started);
      const count = (listing.items || []).length;
      if (!count) {
        setChildren(results, el("p", { class: "unavailable",
          text: `No rows matched. ${took} ms.` }));
      } else {
        setChildren(results);
        renderTableInto(results, [
          el("p", { class: "unavailable",
                    text: `${count} row${count === 1 ? "" : "s"} · ${took} ms` }),
        ], listing, listing.noun || "rows", () => execute(), route, {});
      }
      announce(`Query returned ${count} rows`);
    } catch (err) {
      // The database said this. Saying it again in our own words would be
      // replacing the answer with a summary of the answer.
      setChildren(results);
      error.textContent = err.message;
      error.hidden = false;
    } finally {
      setBusy(run, false);
    }
    if (onDone) onDone();
  };

  return el("div", { class: "query-pane" },
    el("form", { class: "card query-form", novalidate: true, onsubmit: execute },
      ...fields.nodes,
      el("div", { class: "card-actions" }, run,
        el("button", { class: "secondary", type: "button", text: spec.clearLabel || "Clear results",
          onclick: () => { setChildren(results); error.hidden = true; } }))),
    error,
    results);
}

// followTabLabels names the run button for the tab showing in the form's
// tab strip, as Google's Query by GQL is run with Run query (#1846).
function followTabLabels(nodes, run, spec) {
  const labels = spec.tabLabels || {};
  if (!Object.keys(labels).length) return;
  const strips = nodes.flatMap((n) => (n.querySelectorAll ? [...n.querySelectorAll(".form-tabs")] : []));
  const name = () => {
    const shown = strips.map((s) => labels[s.dataset.selected]).find(Boolean);
    run.textContent = shown || spec.label || "Run";
  };
  for (const s of strips) new MutationObserver(name).observe(s, { attributes: true, attributeFilter: ["data-selected"] });
  name();
}

// --- create form ------------------------------------------------------
//
// The form is described by the backend, so a field only appears when the
// service behind it can accept it.
//
// One builder serves both the dialog and the full-page form. Two renderings
// of one description would drift, and validation is the part that would drift
// silently — the half that still looked right while accepting what the API
// refuses.

// attachCompletions has Ctrl+Space in a text control open a list of the
// words that begin with the word at the cursor (all of them at a word's
// start); Up and Down move through it, Enter or a click puts the chosen
// word in place of the one typed, and Escape closes it (#1846).
function attachCompletions(control, words) {
  const list = el("ul", { class: "completions", role: "listbox", hidden: true, "aria-label": "Completions" });
  list.id = `${control.id || control.name}-completions`;
  control.setAttribute("aria-autocomplete", "list");
  control.setAttribute("aria-controls", list.id);
  let shown = [], active = 0;
  const wordAt = () => {
    const end = control.selectionStart ?? control.value.length;
    let start = end;
    while (start > 0 && /[A-Za-z0-9_$`]/.test(control.value[start - 1])) start--;
    return { start, end, typed: control.value.slice(start, end) };
  };
  const close = () => { list.hidden = true; shown = []; control.setAttribute("aria-expanded", "false"); };
  const choose = (w) => {
    const { start, end } = wordAt();
    control.value = control.value.slice(0, start) + w + control.value.slice(end);
    const at = start + w.length;
    control.setSelectionRange(at, at);
    control.dispatchEvent(new Event("input", { bubbles: true }));
    close();
    control.focus();
  };
  const draw = () => {
    setChildren(list, ...shown.map((w, i) => {
      const item = el("li", { role: "option", text: w, class: i === active ? "active" : "" });
      item.setAttribute("aria-selected", String(i === active));
      item.addEventListener("mousedown", (e) => { e.preventDefault(); choose(w); });
      return item;
    }));
  };
  const open = () => {
    const typed = wordAt().typed.replace(/^`/, "").toLowerCase();
    shown = words.filter((w) => w.replace(/^`/, "").toLowerCase().startsWith(typed));
    active = 0;
    if (!list.isConnected) control.after(list);
    if (!shown.length) { close(); return; }
    draw();
    list.hidden = false;
    control.setAttribute("aria-expanded", "true");
  };
  control.addEventListener("keydown", (e) => {
    if (e.ctrlKey && (e.key === " " || e.code === "Space")) { e.preventDefault(); open(); return; }
    if (list.hidden) return;
    if (e.key === "ArrowDown" || e.key === "ArrowUp") {
      e.preventDefault();
      active = (active + (e.key === "ArrowDown" ? 1 : shown.length - 1)) % shown.length;
      draw();
    } else if (e.key === "Enter" || e.key === "Tab") {
      e.preventDefault();
      choose(shown[active]);
    } else if (e.key === "Escape") {
      e.preventDefault();
      close();
    }
  });
  control.addEventListener("input", () => { if (!list.hidden) open(); });
  control.addEventListener("blur", () => setTimeout(close, 150));
}

function buildCreateForm(spec) {
  // A hidden field is what the form was drawn from, sent back unchanged so
  // the backend can refuse the change when that has changed since: a
  // Datastore value is addressed by its place in its array, and another
  // writer can move it (#923). It has no control, so it is never invalid,
  // dirty or focused.
  const hidden = spec.fields.filter((f) => f.type === "hidden");
  const entries = spec.fields.filter((f) => f.type !== "hidden").map((f) => {
    const id = `f-${f.name}`;
    const errorId = `e-${f.name}`;
    const helpId = f.help ? `h-${f.name}` : null;
    const isCheck = f.type === "checkbox";
    const isMap = f.type === "map";
    const isArea = f.type === "textarea" || isMap;
    // A choice among values the backend listed, such as a bucket
    // notification's topic among the project's topics (#791).
    const isSelect = f.type === "select";
    // A table's schema (#854): rows of name, type and mode, submitted as the
    // JSON array the backend parses. The editor is the rows; the value is a
    // hidden textarea, so validation, dirtiness and submission are the same
    // code as every other field's.
    if (f.type === "schema") return schemaEntry(f, id, errorId, helpId);
    // A choice drawn as radio buttons, as Google draws a bucket's access
    // control and its storage class choice (#1843). The value travels in a
    // hidden input, so dirtiness, validation and submission are the same
    // code as every other field's.
    if (f.type === "radio") return radioEntry(f, id, errorId, helpId);
    // Google's Manage traffic rows (#1852): a revision and its percent per
    // row, with Add revision, submitted as a JSON array.
    if (f.type === "traffic") return trafficEntry(f, id, errorId, helpId);
    // Google's inline source editor (#1852): files, one open at a time in a
    // code editor, submitted as a JSON object of file name to contents.
    if (f.type === "sourcefiles") return sourceFilesEntry(f, id, errorId, helpId);
    // A repeatable group of fields (#1863): Google's exclusion filters,
    // a row each with its name, filter and Disable.
    if (f.type === "group") return groupEntry(f, id, errorId, helpId);

    // A textarea rather than an input wherever the value can hold newlines:
    // Enter inserts one instead of submitting the form, which is the whole
    // difference between a usable DDL box and a single-line one.
    const control = isArea
      ? el("textarea", { id, name: f.name, rows: isMap ? "4" : "5", required: f.required })
      : isSelect
      ? el("select", { id, name: f.name, required: f.required },
          // An empty option (an optional choice left unset) is named, so
          // it is not read as a blank item (#1604); optionLabels name an
          // option as Google's console does (#1852, #1860).
          ...(f.options || []).map((o) => el("option", { value: o, text: (f.optionLabels || {})[o] || o || "None" })))
      : el("input", {
          id, name: f.name, type: f.type || "text", required: f.required,
          // `pattern` is only enforced on the text-like inputs. Attaching one
          // elsewhere would be a constraint nothing applies.
          pattern: isCheck || isArea ? null : (f.pattern || null),
        });
    if (isCheck) control.checked = f.default === "true" && !f.unavailable;
    else if (isSelect) control.value = f.default || (f.options || [])[0] || "";
    else control.value = f.default || "";
    if (helpId) control.setAttribute("aria-describedby", helpId);
    // Ctrl+Space offers the field's completions at the cursor, as Google's
    // Query by GQL offers the namespace's kinds (#1846).
    if ((f.completions || []).length && !isCheck && !isSelect) attachCompletions(control, f.completions);
    // An immutable field is shown so the operator can see which resource they
    // are editing, and refused so the form does not accept a change the API
    // will not apply. Disabled rather than hidden: hiding it would read as
    // though the resource did not have the property.
    if (f.immutable) {
      control.disabled = true;
      control.setAttribute("aria-readonly", "true");
    }
    // A choice Google's page offers that this instance cannot make (#1847):
    // drawn where Google draws it, disabled, with the reason, rather than
    // left out.
    const unavailableId = isCheck && f.unavailable ? `u-${f.name}` : null;
    if (unavailableId) {
      control.disabled = true;
      control.setAttribute("aria-describedby", [helpId, unavailableId].filter(Boolean).join(" "));
    }

    const label = el("label", { for: id },
      el("span", { text: f.label }),
      // The marker is decorative: assistive technology reads the requirement
      // from the control's own `required`, so announcing "star" as well would
      // be saying it twice.
      f.required ? el("span", { class: "required-mark", "aria-hidden": "true", text: "*" }) : null);
    const help = f.help ? el("p", { id: helpId, class: "form-help", text: f.help }) : null;
    const error = el("p", { id: errorId, class: "form-field-error", hidden: true });
    const unavailable = unavailableId
      ? el("p", { id: unavailableId, class: "form-help form-unavailable", text: f.unavailable }) : null;

    // A textarea's options are templates (#1859): Google's Create database
    // has Show DDL templates, each inserted into the schema box.
    const templates = f.type === "textarea" && (f.options || []).length
      ? el("details", { class: "form-templates" },
          el("summary", { text: "Show DDL templates" }),
          ...f.options.map((t, i) => el("div", { class: "form-template" },
            el("pre", { class: "mono", text: t }),
            el("button", { type: "button", class: "secondary", id: `${id}-template-${i}`, text: "Insert",
              onclick: () => {
                control.value = control.value.trim() ? `${control.value.trim()}\n\n${t}` : t;
                control.dispatchEvent(new Event("input", { bubbles: true }));
                control.focus();
              } }))))
      : null;
    // Upload file beside a textarea (Field.Upload, #1850): the chosen text
    // file is read into the textarea, which stays the value.
    const upload = isArea && !isMap && f.upload ? uploadIntoTextarea(f, control) : null;
    const node = isCheck
      ? el("div", { class: "form-row is-check" + (unavailableId ? " is-unavailable" : "") },
          el("div", { class: "check-line" }, control, label), help, unavailable, error)
      : el("div", { class: "form-row" }, label,
          // Browse (#1845): a gs:// field chooses a bucket of this
          // instance's Cloud Storage, as Google's Choose Destination does.
          f.browse === "storage" ? el("div", { class: "browse-row" }, control,
            el("button", { type: "button", class: "secondary", id: `browse-${f.name}`, text: "Browse",
              onclick: () => browseBuckets(control) }))
          // A Cloud KMS key field (#1881) chooses an ENCRYPT_DECRYPT key of
          // this instance's Cloud KMS in the location the form's own
          // location fields hold, as Google's key picker does.
          : f.browse === "kms" ? el("div", { class: "browse-row" }, control,
            el("button", { type: "button", class: "secondary", id: `browse-${f.name}`, text: "Browse",
              onclick: () => browseKeys(control, formKMSLocation(entries)) })) : control,
          templates, upload, help, error);

    return { field: f, control, node, error, helpId, errorId, isCheck };
  });
  // A field's dialog (#1859): a button beside it that opens a table, as
  // Google's Create instance has Compare editions beside the edition.
  for (const e of entries) if (e.field.dialog) e.node.append(fieldDialogButton(e.field));

  // A map field is edited one "key=value" per line and submitted as the JSON
  // object the backend decodes. The lines are the editable form; the JSON is
  // the wire format, and the user should never have to write braces.
  const mapEntries = entries.filter((e) => e.field.type === "map");
  for (const entry of mapEntries) {
    entry.control.value = mapToLines(entry.field.default);
    entry.control.setAttribute("spellcheck", "false");
    const validate = () => {
      const bad = badMapLine(entry.control.value);
      // setCustomValidity is what makes checkValidity() agree with what the
      // field actually accepts, so one validation path covers both.
      entry.control.setCustomValidity(bad
        ? `Line ${bad.line} is not "key=value".`
        : mapRuleError(entry.field, entry.control.value));
    };
    entry.control.addEventListener("input", validate);
    validate();
  }

  const message = ({ field, control }) => {
    if (control.validity.valueMissing) return `${field.label} is required.`;
    // The help text is the constraint in words, so a pattern failure says
    // what the rule is rather than that a rule exists.
    if (control.validity.patternMismatch) {
      return field.help || `${field.label} is not in the expected format.`;
    }
    return control.validationMessage || `${field.label} is not valid.`;
  };

  const markValid = (entry) => {
    entry.error.textContent = "";
    entry.error.hidden = true;
    entry.node.classList.remove("is-invalid");
    entry.control.removeAttribute("aria-invalid");
    if (entry.helpId) entry.control.setAttribute("aria-describedby", entry.helpId);
    else entry.control.removeAttribute("aria-describedby");
  };

  const markInvalid = (entry) => {
    entry.error.textContent = message(entry);
    entry.error.hidden = false;
    entry.node.classList.add("is-invalid");
    entry.control.setAttribute("aria-invalid", "true");
    entry.control.setAttribute("aria-describedby",
      [entry.helpId, entry.errorId].filter(Boolean).join(" "));
  };

  const check = (entry) => {
    // A field its ShowWhen hides is not asked for (#1847).
    if (entry.node.hidden || entry.control.checkValidity()) { markValid(entry); return true; }
    markInvalid(entry);
    return false;
  };

  for (const entry of entries) {
    // Checked when the field is left, and again on every keystroke once it is
    // already marked — so a correction clears the error as soon as it is a
    // correction, rather than at the next submit.
    entry.control.addEventListener("blur", () => check(entry));
    const recheck = () => { if (entry.node.classList.contains("is-invalid")) check(entry); };
    entry.control.addEventListener("input", recheck);
    entry.control.addEventListener("change", recheck);
  }

  // A checkbox that needs others on, or others off: a bucket's hierarchical
  // namespace needs uniform bucket-level access and excludes object
  // versioning (#1203). Checked, it sets them and holds them there, so the
  // form cannot submit what the API refuses; unchecked, they are free again.
  // A radio choice is named "name=value": hierarchical namespace holds the
  // access control on Uniform (#1843).
  const byName = new Map(entries.map((e) => [e.field.name, e]));
  for (const entry of entries) {
    const f = entry.field;
    const tie = (n, value) => {
      const [name, choice] = n.split("=");
      return [byName.get(name), choice === undefined ? value : choice];
    };
    const ties = [...(f.requires || []).map((n) => tie(n, true)), ...(f.excludes || []).map((n) => tie(n, false))]
      .filter(([other]) => other && (other.isCheck || other.isRadio) && !other.field.immutable);
    if (!entry.isCheck || !ties.length) continue;
    const hold = () => {
      const on = entry.control.checked;
      for (const [other, value] of ties) {
        if (other.isRadio) {
          if (on) other.choose(value);
          other.hold(on);
        } else {
          if (on) other.control.checked = value;
          other.control.disabled = on;
        }
        other.node.classList.toggle("is-held", on);
        other.control.title = on ? `Held ${value === true ? "on" : value === false ? "off" : value} by ${f.label}` : "";
      }
    };
    entry.control.addEventListener("change", hold);
    hold();
  }

  // An editor that follows a choice: the inline source editor's files are
  // the chosen runtime's template until they are edited (#1852).
  for (const entry of entries) {
    const other = entry.field.follows && byName.get(entry.field.follows);
    if (!other || !entry.follow) continue;
    other.control.addEventListener("change", () => entry.follow(other.control.value));
    entry.follow(other.control.value);
  }

  // A field shown only while another holds a value (#1847): "name" for a
  // checked checkbox, "name=value" for a radio or select choice, as Google's
  // Create topic reveals the retention period under Enable message retention.
  // Conditions joined by "&" must all hold (#1863): a distribution
  // metric's linear bounds show for "type=Distribution&bucket_type=Linear",
  // a bucket's dataset name for "analytics&link".
  const radioValue = (other) => other.control.value;
  for (const entry of entries) {
    const cond = entry.field.showWhen;
    if (!cond) continue;
    // "name=a|b" holds for either choice: Create table's File format is
    // shown for Google Cloud Storage and for Upload (#1861).
    const terms = cond.split("&").map((t) => {
      const [name, choice] = t.split("=");
      return { other: byName.get(name), choice };
    }).filter((t) => t.other);
    if (!terms.length) continue;
    const show = () => {
      const on = terms.every(({ other, choice }) => choice === undefined
        ? (other.isCheck ? other.control.checked : !!other.control.value)
        : choice.split("|").includes(radioValue(other)));
      entry.node.hidden = !on;
      if (!on) markValid(entry);
    };
    for (const { other } of terms) {
      other.control.addEventListener("change", show);
      other.control.addEventListener("input", show);
    }
    show();
  }

  // A field shown only while others hold given values (visibleWhen,
  // #1860): Cloud SQL's Database version lists the chosen engine's
  // versions. Hidden, it is disabled, so it is neither checked nor sent.
  const valueText = (e) => (e.isCheck ? String(e.control.checked) : e.control.value);
  for (const entry of entries) {
    const when = Object.entries(entry.field.visibleWhen || {});
    if (!when.length) continue;
    const show = () => {
      const on = when.every(([n, v]) => byName.has(n) && valueText(byName.get(n)) === v);
      entry.node.hidden = !on;
      entry.inactive = !on;
      entry.control.disabled = !on || !!entry.field.immutable;
    };
    for (const [n] of when) {
      const other = byName.get(n);
      if (other) other.control.addEventListener("change", show);
    }
    show();
  }

  // A field made from another (#1864): until it is edited by hand it
  // follows that field, lowercased, with every run of other characters a
  // dash, as Google's Create service account makes the ID from the name.
  for (const entry of entries) {
    const from = byName.get(entry.field.deriveFrom || "");
    if (!from) continue;
    let edited = false;
    entry.control.addEventListener("input", () => { edited = entry.control.value !== ""; });
    from.control.addEventListener("input", () => {
      if (edited) return;
      let v = from.control.value.toLowerCase().replace(/[^a-z0-9]+/g, "-").replace(/^[^a-z]+/, "").slice(0, 30)
        .replace(/-+$/, "");
      entry.control.value = v;
      entry.control.dispatchEvent(new Event("change", { bubbles: true }));
    });
  }

  // Fields group under their section heading, in the order the backend gave
  // them. A form whose fields declare no section renders as one block, which
  // is what a two-field form should look like.
  const groups = [];
  for (const entry of entries) {
    const name = entry.field.section || "";
    const last = groups[groups.length - 1];
    if (last && last.name === name) last.entries.push(entry);
    else groups.push({ name, entries: [entry] });
  }
  const nodes = groups.flatMap((g) => g.name
    ? [el("section", { class: "form-section" },
        el("h3", { class: "form-section-title", text: g.name }),
        ...formTabs(g.name, g.entries))]
    : g.entries.map((e) => e.node));

  // Stated once, not once per field: the marker means nothing until something
  // says what it means.
  if (entries.some((e) => e.field.required)) {
    nodes.unshift(el("p", { class: "form-required-note" },
      el("span", { class: "required-mark", "aria-hidden": "true", text: "*" }),
      el("span", { text: " Required" })));
  }

  const defaultOf = (entry) => {
    if (entry.isCheck) return entry.field.default === "true";
    if (entry.field.type === "select") return entry.field.default || (entry.field.options || [])[0] || "";
    if (entry.field.type === "map") return mapToLines(entry.field.default);
    return entry.field.default || "";
  };
  const valueOf = (entry) => (entry.isCheck ? entry.control.checked : entry.control.value);

  return {
    nodes,
    // The first field that can take focus: an edit form's first field is
    // often immutable and disabled, and focus() on a disabled control is a
    // no-op, which left focus on the page behind the dialog, where its
    // Escape handler never hears the key (#700, measured in #765's CI).
    focusFirst() {
      const e = entries.find((x) => !x.control.disabled);
      if (e) (e.focus || (() => e.control.focus()))();
    },
    // Anything the user changed away from what the form offered. A form
    // holding only its own defaults has nothing to lose.
    dirty() { return entries.some((e) => valueOf(e) !== defaultOf(e)); },
    validate() {
      let first = null;
      for (const entry of entries) {
        if (!check(entry) && !first) first = entry;
      }
      if (first) {
        // A field on a tab that is not showing: show its tab first (#1852).
        const panel = first.node.closest && first.node.closest(".form-tab-panel");
        if (panel && panel.hidden) panel.dispatchEvent(new Event("form-tab-show"));
        (first.focus || (() => first.control.focus()))();
      }
      return !first;
    },
    // The files chosen in "file" fields (#999), which are sent as parts of
    // their own rather than as values.
    files() {
      return entries.filter((e) => e.field.type === "file" && e.control.files && e.control.files[0])
        .map((e) => ({ name: e.field.name, file: e.control.files[0] }));
    },
    values() {
      // An immutable field is context, not input. Sending it back would ask
      // the API to set a value to what it already is, which some APIs accept
      // and others reject as an attempt to change an immutable field.
      // A hidden field named "tab:<section>" is sent the tab showing in
      // that section, as Datastore's Query by kind and Query by GQL
      // (#1846).
      const tabsShown = nodes.flatMap((n) => (n.querySelectorAll ? [...n.querySelectorAll(".form-tabs")] : []))
        .filter((t) => hidden.some((h) => h.name === `tab:${t.dataset.section}`))
        .map((t) => [`tab:${t.dataset.section}`, t.dataset.selected || ""]);
      return Object.fromEntries([...hiddenValues(hidden), ...tabsShown, ...entries
        .filter((e) => !e.field.immutable && e.field.type !== "file" && !e.inactive)
        .map((e) => {
          if (e.isCheck) return [e.field.name, String(e.control.checked)];
          if (e.field.type === "map") return [e.field.name, linesToMap(e.control.value)];
          return [e.field.name, e.control.value];
        })]);
    },
  };
}

// uploadIntoTextarea is a textarea's Upload file control (Field.Upload,
// #1850): a file chooser whose text file replaces the textarea's value.
function uploadIntoTextarea(f, control) {
  const input = el("input", { type: "file", id: `up-${f.name}`, class: "upload-into-input",
    "aria-label": `${f.upload} for ${f.label}` });
  const status = el("span", { class: "form-help", role: "status" });
  input.addEventListener("change", () => {
    const file = input.files && input.files[0];
    if (!file) return;
    const r = new FileReader();
    r.onload = () => {
      control.value = String(r.result);
      control.dispatchEvent(new Event("input", { bubbles: true }));
      status.textContent = `Read ${file.name}`;
    };
    r.onerror = () => { status.textContent = `Could not read ${file.name}`; };
    r.readAsText(file);
  });
  return el("div", { class: "upload-into" }, el("span", { class: "upload-into-label", text: f.upload }), input, status);
}

// formTabs lays out one form section's fields: those with no tab, then a
// tab strip over the fields that name a tab (Field.Tab), in the order the
// backend gave them, as Google's Create service page holds Container(s),
// Variables & Secrets, Volumes, Networking and Security (#1852). Every field
// stays in the form whichever tab shows, so Create submits them all.
function formTabs(section, entries) {
  const plain = entries.filter((e) => !e.field.tab).map((e) => e.node);
  const tabs = [];
  for (const e of entries.filter((x) => x.field.tab)) {
    let t = tabs.find((x) => x.name === e.field.tab);
    if (!t) tabs.push(t = { name: e.field.tab, entries: [] });
    t.entries.push(e);
  }
  if (!tabs.length) return plain;
  const slug = (n) => n.toLowerCase().replace(/[^a-z0-9]+/g, "-").replace(/^-|-$/g, "");
  const base = `form-tab-${slug(section)}`;
  const buttons = [];
  const panels = tabs.map((t, i) => el("div", { class: "form-tab-panel tab-panel", role: "tabpanel",
    id: `${base}-${slug(t.name)}`, "aria-labelledby": `${base}-${slug(t.name)}-tab`, hidden: i !== 0 },
    ...t.entries.map((e) => e.node)));
  const strip = el("div", { class: "tab-strip form-tabs", role: "tablist", "aria-label": section,
    "data-section": section, "data-selected": tabs[0].name });
  const select = (i) => {
    strip.dataset.selected = tabs[i].name;
    buttons.forEach((b, j) => {
      b.classList.toggle("is-selected", i === j);
      b.setAttribute("aria-selected", i === j ? "true" : "false");
      b.tabIndex = i === j ? 0 : -1;
    });
    panels.forEach((p, j) => { p.hidden = i !== j; });
  };
  tabs.forEach((t, i) => {
    buttons.push(el("button", { type: "button", class: "tab" + (i === 0 ? " is-selected" : ""), role: "tab",
      id: `${base}-${slug(t.name)}-tab`, "aria-controls": panels[i].id,
      "aria-selected": i === 0 ? "true" : "false", tabindex: i === 0 ? "0" : "-1", text: t.name,
      onclick: () => select(i),
      onkeydown: (ev) => {
        const step = ev.key === "ArrowRight" ? 1 : ev.key === "ArrowLeft" ? -1 : 0;
        if (!step) return;
        ev.preventDefault();
        const j = (i + step + tabs.length) % tabs.length;
        select(j);
        buttons[j].focus();
      } }));
    panels[i].addEventListener("form-tab-show", () => select(i));
  });
  strip.append(...buttons);
  return [...plain, strip, ...panels];
}

// radioEntry is a "radio" field: one radio button per option, the choice
// kept in a hidden input whose value is submitted (#1843).
// fieldDialogButton is a field's dialog (Field.Dialog): its button, and the
// table it opens.
function fieldDialogButton(f) {
  const d = f.dialog;
  const titleId = `dialog-${f.name}-title`;
  return el("button", { type: "button", class: "link-button is-inline", id: `f-${f.name}-dialog`, text: d.button,
    onclick: () => {
      const { dialog, close } = openModal({ labelledBy: titleId });
      dialog.append(el("div", { class: "modal-body field-dialog" },
        el("h2", { id: titleId, text: d.title }),
        d.intro ? el("p", { class: "form-help", text: d.intro }) : null,
        el("div", { class: "table-wrap" }, el("table", { class: "field-dialog-table" },
          el("thead", {}, el("tr", {}, ...(d.columns || []).map((c) => el("th", { scope: "col", text: c })))),
          el("tbody", {}, ...(d.rows || []).map((r) => el("tr", {},
            ...r.map((v, i) => (i === 0 ? el("th", { scope: "row", text: v }) : el("td", { text: v })))))))),
        el("div", { class: "modal-actions" },
          el("button", { type: "button", class: "primary", text: "Close", onclick: () => close() }))));
      dialog.querySelector(".modal-actions button").focus();
    } });
}

function radioEntry(f, id, errorId, helpId) {
  const control = el("input", { type: "hidden", id, name: f.name });
  control.value = f.default || (f.options || [])[0] || "";
  // A choice this instance cannot make is drawn disabled with its reason
  // (#1847), never chosen.
  const off = f.unavailableOptions || {};
  const radios = (f.options || []).map((o, i) => {
    const r = el("input", { type: "radio", id: `${id}-${i}`, name: `${id}-choice`, value: o });
    r.checked = o === control.value;
    if (off[o]) {
      r.disabled = true;
      r.setAttribute("aria-describedby", `${id}-${i}-unavailable`);
    }
    r.addEventListener("change", () => {
      if (!r.checked) return;
      control.value = o;
      control.dispatchEvent(new Event("change", { bubbles: true }));
    });
    // An option drawn but not choosable, such as SQL Server where
    // CloudBurrow does not run it (#1860).
    if (f.immutable || (f.disabledOptions || []).includes(o)) r.disabled = true;
    return r;
  });
  const help = f.help ? el("p", { id: helpId, class: "form-help", text: f.help }) : null;
  const error = el("p", { id: errorId, class: "form-field-error", hidden: true });
  const node = el("fieldset", { class: "form-row is-radio", role: "radiogroup",
                                "aria-describedby": helpId || null },
    el("legend", { text: f.label }),
    control,
    ...radios.map((r, i) => el("div", { class: "check-line" + (off[f.options[i]] ? " is-unavailable" : "") }, r,
      el("label", { for: r.id, text: (f.optionLabels || {})[f.options[i]] || f.options[i] }),
      off[f.options[i]] ? el("p", { id: `${r.id}-unavailable`, class: "form-help form-unavailable",
                                    text: off[f.options[i]] }) : null)),
    help, error);
  const choose = (value) => {
    control.value = value;
    for (const r of radios) r.checked = r.value === value;
  };
  return { field: f, control, node, error, helpId, errorId, isCheck: false, isRadio: true,
           choose, hold: (on) => {
             for (const r of radios) {
               r.disabled = on || !!f.immutable || !!off[r.value] || (f.disabledOptions || []).includes(r.value);
             }
           },
           focus: () => (radios.find((r) => r.checked) || radios[0] || control).focus() };
}

// trafficEntry is a "traffic" field, Google's Manage traffic (#1852): one
// row per target, a revision chosen among the field's options (named by its
// optionLabels) and a percent, a Remove on each row and Add revision under
// them, with the total. The rows are written into a hidden textarea as
// [{revision, percent}], which is invalid unless every percent is a whole
// number from 0 to 100 and they sum to 100, so the form's one validation
// path reports it before anything is sent.
function trafficEntry(f, id, errorId, helpId) {
  const control = el("textarea", { id, name: f.name, hidden: true, class: "traffic-value" });
  const labels = f.optionLabels || {};
  const options = f.options || [];
  const list = el("div", { class: "traffic-rows", role: "list" });
  const total = el("p", { class: "traffic-total", "aria-live": "polite" });
  const rows = [];
  const sync = () => {
    const value = rows.map((r) => ({ revision: r.revision.value, percent: Number(r.percent.value || "0") }));
    control.value = JSON.stringify(value);
    const sum = value.reduce((n, v) => n + v.percent, 0);
    total.textContent = `Total: ${sum}%`;
    const bad = rows.find((r) => !/^[0-9]{1,3}$/.test(r.percent.value.trim()) || Number(r.percent.value) > 100);
    control.setCustomValidity(bad ? "Each revision's traffic is a whole percent from 0 to 100."
      : sum !== 100 ? `The revisions' percentages sum to ${sum}; they must sum to 100.` : "");
    control.dispatchEvent(new Event("input", { bubbles: true }));
  };
  const addRow = (revision, percent) => {
    const n = rows.length + 1;
    const row = {
      revision: el("select", { class: "traffic-revision", "aria-label": `Revision ${n}` },
        ...options.map((o) => el("option", { value: o, text: labels[o] || o }))),
      percent: el("input", { type: "text", inputmode: "numeric", class: "traffic-percent",
                             "aria-label": `Revision ${n} traffic percent`, autocomplete: "off" }),
    };
    row.revision.value = revision || options[0] || "";
    row.percent.value = String(percent === undefined ? 0 : percent);
    row.remove = el("button", { type: "button", class: "secondary traffic-remove", text: "Remove",
      "aria-label": `Remove revision ${n}`,
      onclick: () => {
        rows.splice(rows.indexOf(row), 1);
        row.node.remove();
        if (!rows.length) addRow(options[0], 100);
        sync();
      } });
    row.node = el("div", { class: "traffic-row", role: "listitem" }, row.revision,
      el("span", { class: "traffic-percent-box" }, row.percent, el("span", { "aria-hidden": "true", text: "%" })),
      row.remove);
    for (const c of [row.revision, row.percent]) {
      c.addEventListener("input", sync);
      c.addEventListener("change", sync);
    }
    rows.push(row);
    list.append(row.node);
    return row;
  };
  let initial = [];
  try { initial = JSON.parse(f.default || "[]"); } catch (_) { initial = []; }
  for (const r of initial) addRow(r.revision, r.percent);
  if (!rows.length) addRow(options[0], 100);
  sync();
  // The value the form was drawn with is its default, so an untouched
  // dialog is not dirty.
  f = Object.assign({}, f, { default: control.value });
  const add = el("button", { type: "button", class: "secondary traffic-add", text: "Add revision",
    onclick: () => {
      const used = new Set(rows.map((r) => r.revision.value));
      addRow(options.find((o) => !used.has(o)) || options[0], 0).revision.focus();
      sync();
    } });
  const help = f.help ? el("p", { id: helpId, class: "form-help", text: f.help }) : null;
  const error = el("p", { id: errorId, class: "form-field-error", hidden: true });
  const node = el("fieldset", { class: "form-row traffic-field" },
    el("legend", { text: f.label }),
    el("div", { class: "traffic-head", "aria-hidden": "true" },
      el("span", { text: "Revision" }), el("span", { text: "Traffic" })),
    list, add, total, control, help, error);
  return { field: f, control, node, error, helpId, errorId, isCheck: false,
           focus: () => rows[0].revision.focus() };
}

// sourceFilesEntry is a "sourcefiles" field, Google's inline source editor
// (#1852): the files of a function's source, one open at a time in a code
// editor, chosen from the file list beside it, with Add file. A field that
// follows another (Field.Follows, the runtime) starts from that choice's
// template (Field.Templates) and takes the next choice's template until a
// file is edited. The files are written into a hidden textarea as a JSON
// object of name to contents, which must hold at least one file.
function sourceFilesEntry(f, id, errorId, helpId) {
  const control = el("textarea", { id, name: f.name, hidden: true, class: "sourcefiles-value" });
  let files = {};
  try { files = JSON.parse(f.default || "{}") || {}; } catch (_) { files = {}; }
  let current = Object.keys(files)[0] || "";
  let edited = false;
  const tabs = el("div", { class: "sourcefiles-list", role: "tablist", "aria-label": `${f.label} files` });
  const editor = el("textarea", { class: "sourcefiles-editor code", id: `${id}-editor`, spellcheck: "false",
                                  autocomplete: "off", rows: "16", "aria-label": "File contents" });
  const newName = el("input", { type: "text", class: "sourcefiles-new", "aria-label": "New file name",
                                placeholder: "file name", autocomplete: "off", spellcheck: "false" });
  const sync = () => {
    control.value = JSON.stringify(files);
    control.setCustomValidity(Object.keys(files).length ? "" : "The source needs at least one file.");
    control.dispatchEvent(new Event("input", { bubbles: true }));
  };
  const draw = () => {
    setChildren(tabs, ...Object.keys(files).map((name) => el("button", { type: "button", role: "tab",
      class: "sourcefiles-file" + (name === current ? " is-selected" : ""), text: name,
      "aria-selected": name === current ? "true" : "false",
      onclick: () => { current = name; draw(); editor.focus(); } })));
    editor.value = current ? files[current] : "";
    editor.disabled = !current;
    editor.setAttribute("aria-label", current ? `Contents of ${current}` : "File contents");
  };
  editor.addEventListener("input", () => {
    if (!current) return;
    files[current] = editor.value;
    edited = true;
    sync();
  });
  // Tab indents, as a code editor's does; Escape leaves the editor first.
  editor.addEventListener("keydown", (e) => {
    if (e.key !== "Tab" || e.shiftKey || e.metaKey || e.ctrlKey || e.altKey) return;
    e.preventDefault();
    const { selectionStart: a, selectionEnd: b } = editor;
    editor.value = editor.value.slice(0, a) + "  " + editor.value.slice(b);
    editor.selectionStart = editor.selectionEnd = a + 2;
    editor.dispatchEvent(new Event("input"));
  });
  const add = el("button", { type: "button", class: "secondary sourcefiles-add", text: "Add file",
    onclick: () => {
      const name = newName.value.trim();
      if (!name || files[name] !== undefined || name.startsWith("/") || name.split("/").includes("..")) {
        newName.focus();
        return;
      }
      files[name] = "";
      current = name;
      newName.value = "";
      edited = true;
      draw();
      sync();
      editor.focus();
    } });
  const remove = el("button", { type: "button", class: "secondary sourcefiles-remove", text: "Delete file",
    onclick: () => {
      if (!current) return;
      delete files[current];
      current = Object.keys(files)[0] || "";
      edited = true;
      draw();
      sync();
    } });
  const follow = (choice) => {
    const t = (f.templates || {})[choice];
    if (edited || !t) return;
    files = Object.assign({}, t);
    current = Object.keys(files)[0] || "";
    draw();
    sync();
    // A template the form chose is not an edit.
    entry.field = Object.assign({}, entry.field, { default: control.value });
  };
  draw();
  sync();
  const help = f.help ? el("p", { id: helpId, class: "form-help", text: f.help }) : null;
  const error = el("p", { id: errorId, class: "form-field-error", hidden: true });
  const node = el("fieldset", { class: "form-row sourcefiles-field" },
    el("legend", { text: f.label }),
    el("div", { class: "sourcefiles-body" },
      el("div", { class: "sourcefiles-side" }, tabs,
        el("div", { class: "sourcefiles-actions" }, newName, add, remove)),
      editor),
    control, help, error);
  const entry = { field: Object.assign({}, f, { default: control.value }), control, node, error, helpId, errorId,
                  isCheck: false, follow, focus: () => editor.focus() };
  return entry;
}

// schemaEntry is a "schema" field: one row per column, each a name, a type
// from the field's options and a mode, with Add field and Remove. A RECORD
// row holds its nested fields in a list of its own under it, with Add nested
// field, to BigQuery's depth of 15 (#874). The rows are written into a
// hidden textarea as [{name, type, mode, fields}], and a schema the API
// would refuse — a name outside the field's pattern, two names at one level
// that differ only in case, or a RECORD with no fields — makes that textarea
// invalid, so the form's one validation path reports it before anything is
// sent.
const SCHEMA_RECORD_DEPTH = 15;

function schemaEntry(f, id, errorId, helpId) {
  const control = el("textarea", { id, name: f.name, required: f.required, hidden: true,
                                   class: "schema-value" });
  const namePattern = f.pattern ? new RegExp(f.pattern, "v") : null;
  const types = f.options || [];
  // problem is why a list of fields, at the dotted path given, is not a
  // schema, or "".
  const problem = (cols, path) => {
    const seen = new Set();
    for (const c of cols) {
      const full = path + c.name;
      if (!c.name) return `A RECORD${path ? ` in "${path.slice(0, -1)}"` : ""} has nested fields but no name.`;
      if (namePattern && !namePattern.test(c.name)) return `"${full}" is not a field name. ${f.help || ""}`.trim();
      const key = c.name.toLowerCase();
      if (seen.has(key)) return `"${full}" is named twice: field names are case-insensitive.`;
      seen.add(key);
      if (c.type === "RECORD") {
        if (!c.fields || !c.fields.length) return `"${full}" is a RECORD with no fields: add a nested field under it.`;
        const inner = problem(c.fields, full + ".");
        if (inner) return inner;
      }
    }
    return "";
  };
  let top;
  const sync = () => {
    const named = top.value();
    control.value = named.length ? JSON.stringify(named) : "";
    control.setCustomValidity(problem(named, ""));
    control.dispatchEvent(new Event("input", { bubbles: true }));
  };
  // fieldList is one level of the schema: the top, or a RECORD's fields.
  // label is the row numbers above it, so a nested row reads "Field 2.1".
  const fieldList = (depth, label) => {
    const list = el("div", { class: "schema-rows", role: "list" });
    const rows = [];
    const offered = depth < SCHEMA_RECORD_DEPTH ? types : types.filter((t) => t !== "RECORD");
    const addRow = () => {
      const n = label + (rows.length + 1);
      const row = {
        name: el("input", { type: "text", class: "schema-name", "aria-label": `Field ${n} name`,
                            autocomplete: "off", spellcheck: "false" }),
        type: el("select", { class: "schema-type", "aria-label": `Field ${n} type` },
          ...offered.map((o) => el("option", { value: o, text: o }))),
        mode: el("select", { class: "schema-mode", "aria-label": `Field ${n} mode` },
          ...["NULLABLE", "REQUIRED", "REPEATED"].map((o) => el("option", { value: o, text: o }))),
        nested: null,
        box: el("div", { class: "schema-nested", role: "group", "aria-label": `Field ${n}'s nested fields`,
                         hidden: true }),
      };
      row.remove = el("button", { type: "button", class: "secondary schema-remove", text: "Remove",
        "aria-label": `Remove field ${n}`,
        onclick: () => {
          rows.splice(rows.indexOf(row), 1);
          row.node.remove();
          if (!rows.length) addRow();
          sync();
        } });
      // A RECORD's list is made the first time it is chosen, with one row,
      // and kept if the type changes back and forth, so nothing typed into
      // it is lost to a slip of the select.
      row.type.addEventListener("change", () => {
        const isRecord = row.type.value === "RECORD";
        if (isRecord && !row.nested) {
          row.nested = fieldList(depth + 1, n + ".");
          row.box.append(row.nested.list, el("button", { type: "button", class: "secondary schema-add",
            text: "Add nested field", "aria-label": `Add a nested field to field ${n}`,
            onclick: () => { row.nested.addRow().name.focus(); sync(); } }));
        }
        row.box.hidden = !isRecord;
      });
      row.node = el("div", { class: "schema-row", role: "listitem" },
        row.name, row.type, row.mode, row.remove, row.box);
      for (const c of [row.name, row.type, row.mode]) {
        c.addEventListener("input", sync);
        c.addEventListener("change", sync);
      }
      rows.push(row);
      list.append(row.node);
      return row;
    };
    // value is the level's fields, less rows with nothing in them.
    const value = () => rows.map((r) => {
      const c = { name: r.name.value.trim(), type: r.type.value, mode: r.mode.value };
      if (c.type === "RECORD" && r.nested) c.fields = r.nested.value();
      return c;
    }).filter((c) => c.name || (c.fields && c.fields.length));
    addRow();
    return { list, rows, addRow, value };
  };
  top = fieldList(1, "");
  const add = el("button", { type: "button", class: "secondary schema-add", text: "Add field",
    onclick: () => { top.addRow().name.focus(); sync(); } });
  // Edit as text (#1861): the schema as BigQuery's JSON array, as Google's
  // schema editor offers it; turning it off draws the rows from the text.
  const fill = (lvl, cols) => {
    for (const r of lvl.rows) r.node.remove();
    lvl.rows.length = 0;
    for (const c of (cols.length ? cols : [{}])) {
      const r = lvl.addRow();
      r.name.value = c.name || "";
      if (c.type) r.type.value = String(c.type).toUpperCase();
      if (c.mode) r.mode.value = String(c.mode).toUpperCase();
      if (r.type.value === "RECORD") {
        r.type.dispatchEvent(new Event("change"));
        if (r.nested) fill(r.nested, c.fields || []);
      }
    }
  };
  const asText = el("textarea", { class: "schema-text mono", rows: "8", hidden: true, spellcheck: "false",
                                  "aria-label": `${f.label} as text` });
  const parseText = () => {
    try {
      const cols = JSON.parse(asText.value.trim() || "[]");
      if (!Array.isArray(cols)) throw new Error("it is not a JSON array of fields");
      return { cols };
    } catch (err) {
      return { problem: `The schema text is not a JSON array of fields: ${err.message}` };
    }
  };
  asText.addEventListener("input", () => {
    const { cols, problem: bad } = parseText();
    control.value = cols && cols.length ? JSON.stringify(cols) : "";
    control.setCustomValidity(bad || problem(cols || [], ""));
    control.dispatchEvent(new Event("input", { bubbles: true }));
  });
  const textToggle = el("input", { type: "checkbox", role: "switch", class: "schema-text-toggle" });
  textToggle.addEventListener("change", () => {
    if (textToggle.checked) {
      asText.value = JSON.stringify(top.value(), null, 2);
    } else {
      const { cols, problem: bad } = parseText();
      if (bad) { textToggle.checked = true; control.setCustomValidity(bad); return; }
      fill(top, cols);
      sync();
    }
    asText.hidden = !textToggle.checked;
    top.list.hidden = textToggle.checked;
    add.hidden = textToggle.checked;
  });
  const textSwitch = el("label", { class: "schema-text-switch" }, textToggle, el("span", { text: " Edit as text" }));
  const help = f.help ? el("p", { id: helpId, class: "form-help", text: f.help }) : null;
  const error = el("p", { id: errorId, class: "form-field-error", hidden: true });
  const node = el("fieldset", { class: "form-row schema-field" },
    el("legend", {},
      el("span", { text: f.label }),
      f.required ? el("span", { class: "required-mark", "aria-hidden": "true", text: "*" }) : null),
    el("div", { class: "schema-head", "aria-hidden": "true" },
      el("span", { text: "Name" }), el("span", { text: "Type" }), el("span", { text: "Mode" })),
    textSwitch, top.list, asText, add, control, help, error);
  if (helpId) top.rows[0].name.setAttribute("aria-describedby", helpId);
  return { field: f, control, node, error, helpId, errorId, isCheck: false,
           focus: () => top.rows[0].name.focus() };
}

// groupEntry is a repeatable group of fields (Field.Group, #1863): a row
// of controls per item, Add (its addLabel) for another and Remove on each,
// as Google's Create sink has a row per exclusion filter with its name,
// filter and Disable. The value is a hidden textarea holding the JSON
// array of rows, so validation, dirtiness and submission are every other
// field's; a row's own text fields are checked against their patterns.
function groupEntry(f, id, errorId, helpId) {
  const control = el("textarea", { id, name: f.name, hidden: true, class: "group-value" });
  const sub = f.group || [];
  const rows = [];
  let initial = [];
  try { initial = f.default ? JSON.parse(f.default) : []; } catch { initial = []; }
  if (!Array.isArray(initial)) initial = [];
  const list = el("div", { class: "group-rows", role: "list" });
  const rowValue = (r) => Object.fromEntries(sub.map((s, i) =>
    [s.name, s.type === "checkbox" ? String(r.controls[i].checked) : r.controls[i].value.trim()]));
  // A row is blank when its text is: a checkbox checked by default (the
  // Datastore Create entity's Indexed, #1846) or a select's first choice
  // does not make it a row.
  const blank = (v) => sub.every((s) => s.type === "checkbox" || s.type === "select" || v[s.name] === "");
  const sync = () => {
    const values = rows.map(rowValue).filter((v) => !blank(v));
    control.value = values.length ? JSON.stringify(values) : "";
    let problem = "";
    rows.forEach((r, n) => {
      if (problem || blank(rowValue(r))) return;
      sub.forEach((s, i) => {
        const c = r.controls[i];
        if (problem || s.type === "checkbox") return;
        if (s.required && !c.value.trim()) problem = `${f.label} ${n + 1}: ${s.label} is required.`;
        else if (s.pattern && c.value && !new RegExp(`^(?:${s.pattern})$`, "v").test(c.value)) {
          problem = `${f.label} ${n + 1}: ${s.label} ${s.help ? `must be ${s.help}` : "is not valid"}.`;
        }
      });
    });
    control.setCustomValidity(problem);
    control.dispatchEvent(new Event("input", { bubbles: true }));
  };
  const addRow = (value = {}) => {
    const n = rows.length + 1;
    const r = { controls: [] };
    const cells = sub.map((s) => {
      const label = `${s.label} ${n}`;
      // A select subfield is a dropdown of its options (#1845: a Query
      // builder WHERE clause's operator; #1855: a cleanup policy's Policy
      // type and Tag state), the first chosen when nothing else is.
      // A select's options are its Options; one in UnavailableOptions is
      // drawn disabled with its reason (#1858).
      const c = s.type === "select"
        ? el("select", { "aria-label": label }, ...(s.options || []).map((o) => {
          const why = (s.unavailableOptions || {})[o];
          return el("option", { value: o, text: (s.optionLabels || {})[o] || o || "None", disabled: why ? true : null, title: why || null });
        }))
        : s.type === "textarea"
        ? el("textarea", { rows: "2", "aria-label": label, spellcheck: "false" })
        : el("input", { type: s.type === "checkbox" ? "checkbox" : "text", "aria-label": label,
                        autocomplete: "off", spellcheck: "false" });
      if (s.type === "checkbox") c.checked = (value[s.name] ?? s.default) === "true";
      else c.value = value[s.name] || s.default || (s.type === "select" ? (s.options || [""])[0] : "");
      c.addEventListener("input", sync);
      c.addEventListener("change", sync);
      r.controls.push(c);
      return s.type === "checkbox"
        ? el("label", { class: "group-check" }, c, el("span", { text: s.label }))
        : el("label", { class: "group-cell" }, el("span", { class: "group-cell-label", text: s.label }), c);
    });
    const remove = el("button", { type: "button", class: "secondary group-remove", text: "Remove",
      "aria-label": `Remove ${f.label.toLowerCase()} ${n}`,
      onclick: () => { rows.splice(rows.indexOf(r), 1); r.node.remove(); sync(); } });
    r.node = el("div", { class: "group-row", role: "listitem" }, ...cells, remove);
    rows.push(r);
    list.append(r.node);
    return r;
  };
  for (const v of initial) addRow(v);
  const add = el("button", { type: "button", class: "secondary group-add", text: f.addLabel || "Add",
    onclick: () => { const r = addRow(); r.controls[0].focus(); sync(); } });
  const help = f.help ? el("p", { id: helpId, class: "form-help", text: f.help }) : null;
  const error = el("p", { id: errorId, class: "form-field-error", hidden: true });
  const node = el("fieldset", { class: "form-row group-field" },
    el("legend", {}, el("span", { text: f.label })), list, add, control, help, error);
  control.value = f.default || "";
  sync();
  // The value as drawn is the default, so an untouched group is not dirty.
  f.default = control.value;
  return { field: f, control, node, error, helpId, errorId, isCheck: false,
           focus: () => (rows[0] ? rows[0].controls[0] : add).focus() };
}

// mapToLines renders a map field's JSON value as one "key=value" per line.
function mapToLines(value) {
  if (!value) return "";
  let parsed;
  try {
    parsed = JSON.parse(value);
  } catch {
    // A value the backend sent that is not JSON is shown as-is rather than
    // silently replaced with nothing: losing the operator's data to a parse
    // failure is worse than showing them something odd.
    return value;
  }
  if (!parsed || typeof parsed !== "object") return value;
  return Object.entries(parsed).map(([k, v]) => `${k}=${v}`).join("\n");
}

// mapRuleError is why a map field's lines break the rules the API holds
// its keys and values to (keyPattern, valuePattern, maxEntries: a label
// Google refuses, #962), or "" when they keep them. The patterns are
// compiled with the v flag, as a pattern attribute is, so they count
// characters, not UTF-16 units, and match Unicode categories.
function mapRuleError(field, text) {
  const keyRe = field.keyPattern ? new RegExp(field.keyPattern, "v") : null;
  const valueRe = field.valuePattern ? new RegExp(field.valuePattern, "v") : null;
  const lines = String(text).split("\n");
  const keys = new Set();
  for (let i = 0; i < lines.length; i++) {
    const line = lines[i].trim();
    if (!line) continue;
    const at = line.indexOf("=");
    if (at <= 0) continue;
    const key = line.slice(0, at).trim();
    const value = line.slice(at + 1).trim();
    keys.add(key);
    if (keyRe && !keyRe.test(key)) {
      return `Line ${i + 1}: the key "${key}" is not allowed. ${field.keyHelp || ""}`.trim();
    }
    if (valueRe && !valueRe.test(value)) {
      return `Line ${i + 1}: the value "${value}" is not allowed. ${field.valueHelp || ""}`.trim();
    }
  }
  if (field.maxEntries && keys.size > field.maxEntries) {
    return `${keys.size} entries; at most ${field.maxEntries} are allowed.`;
  }
  return "";
}

// badMapLine returns the first line that is not "key=value", or null.
function badMapLine(text) {
  const lines = String(text).split("\n");
  for (let i = 0; i < lines.length; i++) {
    const line = lines[i].trim();
    if (!line) continue;
    const at = line.indexOf("=");
    // A key is required; an empty value is legal, because an empty label value
    // is legal.
    if (at <= 0) return { line: i + 1, text: line };
  }
  return null;
}

// linesToMap encodes "key=value" lines as the JSON object the backend parses.
function linesToMap(text) {
  const out = {};
  for (const raw of String(text).split("\n")) {
    const line = raw.trim();
    if (!line) continue;
    const at = line.indexOf("=");
    if (at <= 0) continue;
    // Only the first "=" splits, so a value may contain one.
    out[line.slice(0, at).trim()] = line.slice(at + 1).trim();
  }
  return Object.keys(out).length ? JSON.stringify(out) : "";
}

// submitCreate posts the form and records the operation around it.
//
// A value the API returns once, such as an HMAC key's secret (#792), is handed
// to onOneTime and to nothing else: the operation is recorded with the name.
async function submitCreate(route, spec, values, onOneTime = () => {}) {
  const op = recordOperation(`${spec.label} in ${route.title}`);
  try {
    const res = await send(
      `/api/resources/${route.service}?project=${encodeURIComponent(currentProject())}`,
      "POST", values);
    // The id is what lets the local entry and the server's record be
    // recognised as the same operation rather than shown twice.
    op.succeeded(res.name, res.operation);
    if (res.oneTime) onOneTime(res.oneTime);
    return res.name;
  } catch (err) {
    op.failed(err.message, err.operation);
    throw err;
  }
}

// startCreate opens the form wherever the backend says it belongs.
//
// The threshold is the server's, not the client's, so "long enough to deserve
// a page" has one definition rather than one per caller.
function startCreate(route, create, onDone) {
  if (create.page) return navigate(`${route.path}/create`);
  openCreateForm(route, create, onDone);
}

function openCreateForm(route, spec, onDone) {
  const fields = buildCreateForm(spec);
  const error = el("p", { class: "form-error", role: "alert", hidden: true });
  const discard = el("div", { class: "discard-prompt", role: "alert", hidden: true });

  let submitting = false;
  let discarding = false;

  const { dialog, close } = openModal({
    labelledBy: "create-title",
    canClose: (reason) => {
      if (discarding) return true;
      // A request is in flight. Dismissing now would leave the operation
      // completing against a form that no longer exists, and the user with no
      // idea whether it happened.
      if (submitting) return false;
      if (!fields.dirty()) return true;
      // A stray click on the backdrop is not a decision to throw away typed
      // input, so it does nothing at all. Escape can be pressed by accident,
      // so it asks. Cancel is the decision itself and never asks: a second
      // "Discard your changes?" after pressing it was one question too many.
      if (reason === "backdrop") return false;
      discard.hidden = false;
      discard.querySelector("button").focus();
      return false;
    },
  });

  setChildren(discard,
    el("span", { text: "Discard your changes?" }),
    el("button", { type: "button", class: "secondary", text: "Keep editing",
      onclick: () => { discard.hidden = true; fields.focusFirst(); } }),
    el("button", { type: "button", class: "secondary danger", text: "Discard",
      onclick: () => { discarding = true; close(); } }));

  const cancel = el("button", { type: "button", class: "secondary", text: "Cancel",
                                onclick: () => { discarding = true; close(); } });
  const primary = el("button", { type: "submit", class: "primary", text: spec.label });

  const submit = async (e) => {
    e.preventDefault();
    error.hidden = true;
    // novalidate below, so this is the only validation there is and the
    // console owns the message rather than the browser.
    if (!fields.validate()) return;

    submitting = true;
    setBusy(primary, true);
    cancel.disabled = true;
    try {
      let once = null;
      const name = await submitCreate(route, spec, fields.values(), (o) => { once = o; });
      announce(`Created ${name}`);
      submitting = false;
      discarding = true;
      close();
      // The name is passed on so a caller can act on what was just made —
      // the project picker selects it. Existing callers ignore it.
      onDone(name);
      if (once) showOneTime(once);
    } catch (err) {
      // The banner carries the API's own rejection. A message about one
      // field belongs under that field, and is put there by validate().
      submitting = false;
      setBusy(primary, false);
      cancel.disabled = false;
      error.textContent = err.message;
      error.hidden = false;
    }
  };

  dialog.append(el("form", { class: "modal-body", novalidate: true, onsubmit: submit },
    el("h2", { id: "create-title", text: spec.label }),
    error,
    ...fields.nodes,
    discard,
    el("div", { class: "modal-actions" }, cancel, primary)));
  fields.focusFirst();
}

// renderCreatePage draws the same form at an address of its own.
//
// A dialog has no URL: a refresh, a back button or a link shared with a
// colleague loses whatever was in it. A form long enough to be worth filling
// in is a form worth being able to return to.
// renderNewProject is Google's New project page (#1842): Project name
// first, the Project ID made from it (editable until the project exists,
// "It cannot be changed later"), and its Location, the organization or a
// folder, or none. It creates through Resource Manager's own create form,
// so its rules are that form's, and selects the new project.
// https://docs.cloud.google.com/resource-manager/docs/creating-managing-projects
function projectIdFrom(name, suffix) {
  let id = name.toLowerCase().replace(/[^a-z0-9]+/g, "-").replace(/^[^a-z]+/, "").replace(/-+$/, "");
  id = id.slice(0, 23).replace(/-+$/, "");
  if (!id) id = "my-project";
  if (id.length < 6 || suffix) id = `${id}-${suffix || "000000"}`;
  return id.slice(0, 30);
}

async function renderNewProject(view) {
  const route = ROUTES.find((r) => r.service === "projects");
  const caps = capabilityOf("projects");
  const listHref = "/projects" + scopeSearch();
  const header = pageHeader("New project", "", { back: listHref, backLabel: "Back to Manage resources" });
  if (!route || !caps.create) {
    return setChildren(view, header,
      emptyState("Projects cannot be created here", "This instance does not offer Resource Manager's Create project."));
  }
  const spec = caps.create;
  const idField = (spec.fields || []).find((f) => f.name === "projectId") || {};
  const parentField = (spec.fields || []).find((f) => f.name === "parent") || { options: [""] };
  const suffix = String(100000 + Math.floor(Math.random() * 900000));

  const name = el("input", { id: "new-project-name", type: "text", required: true, maxlength: "30",
    value: `My Project ${suffix.slice(0, 5)}`, "aria-describedby": "new-project-id-line" });
  const id = el("input", { id: "new-project-id", type: "text", required: true, maxlength: "30",
    pattern: idField.pattern || "", spellcheck: "false", value: projectIdFrom(name.value, suffix) });
  let idEdited = false;
  const idText = el("strong", { class: "mono", text: id.value });
  const idRow = el("div", { class: "form-row", hidden: true },
    el("label", { for: "new-project-id", text: "Project ID" }), id,
    el("p", { class: "form-help", text: idField.help || "" }));
  const idLine = el("p", { class: "form-help", id: "new-project-id-line" },
    "Project ID: ", idText, ". It cannot be changed later. ",
    el("button", { type: "button", class: "link-button is-inline", text: "Edit",
      onclick: (e) => { idRow.hidden = false; e.target.remove(); id.focus(); } }));
  name.addEventListener("input", () => {
    if (!idEdited) { id.value = projectIdFrom(name.value, suffix); idText.textContent = id.value; }
  });
  id.addEventListener("input", () => { idEdited = true; idText.textContent = id.value; });

  const parentLabel = (v) => (v ? v : "No organization");
  const parent = el("select", { id: "new-project-parent" },
    ...(parentField.options || [""]).map((v) => el("option", { value: v, text: parentLabel(v) })));
  const error = el("p", { class: "form-error", role: "alert", hidden: true });
  const primary = el("button", { type: "submit", class: "primary", text: "Create" });

  const submit = async (e) => {
    e.preventDefault();
    error.hidden = true;
    if (!name.value.trim()) { error.textContent = "Enter a project name."; error.hidden = false; name.focus(); return; }
    if (!id.checkValidity()) {
      idRow.hidden = false;
      error.textContent = idField.help || "The project ID is not valid.";
      error.hidden = false;
      id.focus();
      return;
    }
    setBusy(primary, true);
    try {
      const made = await submitCreate(route, spec,
        { projectId: id.value.trim(), displayName: name.value.trim(), parent: parent.value });
      notify(`Created project ${made}`);
      announce(`Created project ${made}`);
      window.dispatchEvent(new CustomEvent("cb-project-select", { detail: made }));
      navigate("/?project=" + encodeURIComponent(made));
    } catch (err) {
      setBusy(primary, false);
      error.textContent = err.message;
      error.hidden = false;
    }
  };

  setChildren(view, header,
    el("form", { class: "create-page new-project", novalidate: true, onsubmit: submit },
      error,
      el("div", { class: "form-row" },
        el("label", { for: "new-project-name", text: "Project name *" }), name, idLine),
      idRow,
      el("div", { class: "form-row" },
        el("label", { for: "new-project-parent", text: "Location *" }), parent,
        el("p", { class: "form-help", text: "Parent organization or folder" })),
      el("div", { class: "form-actions" }, primary,
        el("a", { class: "button secondary", href: listHref, text: "Cancel" }))));
  name.focus();
  announce("New project form");
}

async function renderCreatePage(view, route) {
  const target = route.of;
  const project = currentProject();
  const caps = capabilityOf(target.service);
  const back = new URL(target.path, location.origin);
  if (project) back.searchParams.set("project", project);
  carryScope(back, target.service);
  const listHref = back.pathname + back.search;

  // A full page with a back arrow to the list, as Google's create flows are
  // (#1842).
  const header = [pageHeader(caps.create ? (caps.create.title || caps.create.label) : "Create",
    project ? `Project ${project}` : "All projects",
    { back: listHref, backLabel: `Back to ${target.title}` })];
  header[0].prepend(el("nav", { class: "breadcrumb", "aria-label": "Breadcrumb" },
    el("a", { href: listHref, text: target.title }),
    el("span", { "aria-hidden": "true", text: "/" }),
    el("span", { text: caps.create ? (caps.create.title || caps.create.label) : "Create" })));

  if (!caps.create) {
    setChildren(view, ...header,
      emptyState(`${target.title} cannot be created from the console`,
        "The local instance does not offer this operation."));
    return;
  }

  // A field named in the address starts with that value: a topic row's
  // Create subscription opens this page with ?topic= (#1847).
  const given = new URLSearchParams(location.search);
  // Fields that follow the selected project (#1854): Create trigger's
  // Repository lists this project's linked repositories, read now rather
  // than once per console load.
  let formFields = caps.create.fields;
  if (caps.create.perProject) {
    try {
      formFields = (await api(`/api/createform/${target.service}?project=${encodeURIComponent(project)}`)).fields || formFields;
    } catch (err) {
      notify(`The form's choices could not be read for ${project}: ${err.message}`, "error");
    }
  }
  const fields = buildCreateForm({ ...caps.create, fields: formFields.map((f) =>
    f.type !== "hidden" && f.name !== "project" && given.has(f.name) ? { ...f, default: given.get(f.name) } : f) });
  const error = el("p", { class: "form-error", role: "alert", hidden: true });
  const cancel = el("a", { class: "button secondary", href: listHref, text: "Cancel" });
  const primary = el("button", { type: "submit", class: "primary", text: caps.create.submit || caps.create.label });

  // Numbered steps, as Google's Cloud Storage and Cloud SQL create pages
  // are, or one page under headings, as Pub/Sub's are (#1847).
  const steps = caps.create.layout === "page"
    ? { nodes: fields.nodes, revealInvalid() {} }
    : createSteps(fields.nodes, caps.create.layout === "next" ? "Next" : "Continue");
  const checks = createChecks(target, caps.create, fields);

  const submit = async (e) => {
    e.preventDefault();
    error.hidden = true;
    if (!fields.validate()) { steps.revealInvalid(); return; }

    setBusy(primary, true);
    try {
      let once = null;
      const name = await submitCreate(target, caps.create, fields.values(), (o) => { once = o; });
      notify(`Created ${name}`);
      announce(`Created ${name}`);
      // A create that goes on to an action of what it made (#1851): Create
      // key ring opens the ring's page with Create key.
      if (caps.create.then && name) {
        const next = new URL(detailHref(target, [name]), location.origin);
        next.searchParams.set("action", caps.create.then);
        navigate(next.pathname + next.search);
      } else navigate(target.path);
      if (once) showOneTime(once);
    } catch (err) {
      setBusy(primary, false);
      error.textContent = err.message;
      error.hidden = false;
    }
  };

  // Create, then Cancel, at the bottom, as Google's forms end.
  setChildren(view, ...header,
    el("form", { class: "create-page" + (caps.create.layout === "page" ? " is-single-page" : ""),
                 novalidate: true, onsubmit: submit },
      error,
      ...steps.nodes,
      checks,
      el("div", { class: "form-actions" }, primary, cancel)));
  fields.focusFirst();
  announce(`${caps.create.title || caps.create.label} form`);
}

// createChecks draws a create page's checks (#1847): buttons that send
// what the form holds to the provider without creating anything, as
// Google's Create schema has Validate definition and Test message. A check
// with fields asks for them in a dialog, whose button sends them with the
// form's values; the verdict, or the API's refusal, is shown under the
// buttons (or in the dialog). Null when the page has none.
function createChecks(target, spec, fields) {
  if (!(spec.checks || []).length) return null;
  const verdict = el("p", { class: "create-check-result", id: "create-check-result", role: "status", hidden: true });
  const show = (node, ok, text) => {
    node.textContent = text;
    node.hidden = false;
    node.classList.toggle("is-error", !ok);
  };
  const run = (check, extra) => send(
    `/api/resources/${target.service}/check?project=${encodeURIComponent(currentProject())}`,
    "POST", { check: check.id, values: { ...fields.values(), ...extra } });
  const open = (check) => {
    const own = buildCreateForm({ label: check.label, fields: check.fields });
    const result = el("p", { class: "create-check-result", role: "status", hidden: true });
    const { dialog, close } = openModal({ labelledBy: "create-check-title" });
    const go = el("button", { type: "submit", class: "primary", text: check.submit || check.label });
    dialog.append(el("form", { class: "modal-body", novalidate: true, onsubmit: async (e) => {
      e.preventDefault();
      if (!own.validate()) return;
      setBusy(go, true);
      try {
        show(result, true, (await run(check, own.values())).message);
      } catch (err) {
        show(result, false, err.message);
      }
      setBusy(go, false);
    } },
      el("h2", { id: "create-check-title", text: check.label }),
      ...own.nodes,
      result,
      el("div", { class: "modal-actions" },
        el("button", { type: "button", class: "secondary", text: "Close", onclick: () => close() }), go)));
    own.focusFirst();
  };
  const buttons = spec.checks.map((check) => el("button", {
    type: "button", class: "secondary", id: `create-check-${check.id}`, text: check.label,
    onclick: async (e) => {
      if ((check.fields || []).length) { open(check); return; }
      verdict.hidden = true;
      setBusy(e.currentTarget, true);
      const b = e.currentTarget;
      try {
        const answer = (await run(check, {})).message;
        // A check that opens a page (#1863): Preview logs opens the Logs
        // Explorer on the form's filter in a new tab, and says so with a
        // link, in case the browser held the tab back.
        if (check.opens && answer.startsWith("/")) {
          window.open(answer, "_blank", "noopener");
          setChildren(verdict, document.createTextNode(`${check.label} opened in a new tab: `),
            el("a", { href: answer, target: "_blank", rel: "noopener", text: answer }));
          verdict.hidden = false;
          verdict.classList.remove("is-error");
          setBusy(b, false);
          return;
        }
        show(verdict, true, answer);
      } catch (err) {
        show(verdict, false, err.message);
      }
      setBusy(b, false);
    },
  }));
  return el("div", { class: "create-checks" }, el("div", { class: "create-check-buttons" }, ...buttons), verdict);
}

// createSteps lays a create form out as Google's create pages are (#1842):
// numbered sections, each with Continue, which folds the finished section to
// its title and opens the next. A folded section opens again from its title.
// Every field stays in the form, folded or not, so Create submits them all
// and a refusal opens the section holding the field it names.
//
// nodes are buildCreateForm's: its .form-section elements become the steps,
// and fields before the first section become "Get started". A form with no
// sections is one block, unnumbered, as a two-field form should be.
//
//   const steps = createSteps(fields.nodes);
//   form.append(...steps.nodes);
//   if (!fields.validate()) steps.revealInvalid();
// The button is Continue, or Next where Google's page says Next, as
// Create log bucket does (CreateLayoutNext, #1863).
function createSteps(nodes, continueLabel = "Continue", { foldRest = false } = {}) {
  const sections = nodes.filter((n) => n.classList && n.classList.contains("form-section"));
  if (!sections.length) return { nodes, revealInvalid() {} };
  const lead = [];
  const loose = [];
  for (const n of nodes) {
    if (n.classList.contains("form-required-note")) lead.push(n);
    else if (!n.classList.contains("form-section")) loose.push(n);
  }
  const groups = [];
  if (loose.length) groups.push({ title: "Get started", body: loose });
  for (const sec of sections) {
    const title = sec.querySelector(".form-section-title");
    if (title) title.remove();
    groups.push({ title: title ? title.textContent : "", body: [...sec.childNodes] });
  }
  const steps = groups.map((g, i) => {
    const n = i + 1;
    const bodyId = `create-step-${n}`;
    const head = el("button", { type: "button", class: "create-step-head", "aria-expanded": "true",
      "aria-controls": bodyId },
      el("span", { class: "create-step-number", "aria-hidden": "true", text: String(n) }),
      el("span", { class: "create-step-title", text: g.title }));
    const body = el("div", { class: "create-step-body", id: bodyId }, ...g.body);
    const step = el("section", { class: "create-step", "aria-label": `${n}. ${g.title}` }, head, body);
    return { step, head, body };
  });
  const setOpen = (s, open) => {
    s.body.hidden = !open;
    s.head.setAttribute("aria-expanded", open ? "true" : "false");
    s.step.classList.toggle("is-folded", !open);
  };
  if (foldRest) steps.forEach((s, i) => { if (i > 0) setOpen(s, false); });
  steps.forEach((s, i) => {
    s.head.addEventListener("click", () => setOpen(s, s.body.hidden));
    if (i < steps.length - 1) {
      s.body.append(el("div", { class: "create-step-continue" },
        el("button", { type: "button", class: "secondary", text: continueLabel,
          onclick: () => {
            setOpen(s, false);
            s.step.classList.add("is-done");
            const next = steps[i + 1];
            setOpen(next, true);
            const first = next.body.querySelector("input:not([disabled]), select:not([disabled]), textarea:not([disabled])");
            (first || next.head).focus();
          } })));
    }
  });
  return {
    nodes: [...lead, ...steps.map((s) => s.step)],
    revealInvalid() {
      for (const s of steps) if (s.body.querySelector(".is-invalid")) setOpen(s, true);
    },
  };
}

// KNOWN_PROJECTS is the registry as the picker last read it.
//
// Held so that a delete anywhere in the console can say whether it removed the
// project the toolbar is pointing at, without a second read.
let KNOWN_PROJECTS = [];

// currentProject is the scope every mutation is made in.
function currentProject() {
  return new URLSearchParams(location.search).get("project") || "";
}

// notify shows the outcome of something the user did.
//
// window.alert blocks the page, cannot be styled, cannot be dismissed by
// anything but a click, and says nothing at all when an action succeeds — so
// the only feedback the console gave was for failure, and it stopped the
// world to give it.
const NOTIFY_MS = 6000;

function notify(message, kind = "info") {
  let host = document.getElementById("snackbars");
  if (!host) {
    host = el("div", { id: "snackbars", class: "snackbars" });
    // Polite, not assertive: an outcome is worth announcing but not worth
    // interrupting whatever the user is reading.
    host.setAttribute("aria-live", "polite");
    document.body.append(host);
  }

  const bar = el("div", { class: `snackbar is-${kind}`, role: kind === "error" ? "alert" : null },
    el("span", { class: "snackbar-text", text: message }),
    el("button", {
      class: "snackbar-close", "aria-label": "Dismiss",
      onclick: () => bar.remove(),
      html: '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M6 6l12 12M18 6L6 18"/></svg>',
    }));
  host.append(bar);

  // An error stays until dismissed: it is the one outcome the user may need
  // to read twice, or copy.
  if (kind !== "error") setTimeout(() => bar.remove(), NOTIFY_MS);
  announce(message);
  return bar;
}

// confirmDestructive asks for the name back before doing something
// irreversible.
//
// window.confirm cannot say which resource, cannot be styled and is one
// reflexive Enter away from deleting the wrong thing. Typing the name is the
// pattern the console this mirrors uses for exactly that reason: it makes the
// subject of the sentence something the user has to produce.
//
// It resolves once the dialog is gone, either way, so a caller can keep a row
// marked as busy for exactly as long as something is actually happening to it
// — which is not the same interval as "the dialog is open".
//
// `consequence` is the closing sentence. A Seek passes none: it is not a
// delete, and another Seek can move the subscription again (#787); nor does
// removing a principal from a role, which can be granted back (#793).
function confirmDestructive({ title, detail, confirmWord, onConfirm, confirmLabel = "Delete", consequence = "This cannot be undone." }) {
  return new Promise((settle) => {
    const error = el("p", { class: "form-error", role: "alert", hidden: true });
    const input = el("input", { type: "text", autocomplete: "off", id: "confirm-input" });

    let running = false;
    const { dialog, close } = openModal({
      labelledBy: "confirm-title",
      // Dismissing mid-delete would hide an operation that is still going.
      canClose: () => !running,
    });

    const confirm = el("button", { type: "submit", class: "primary danger", text: confirmLabel });
    const cancel = el("button", { type: "button", class: "secondary", text: "Cancel",
                                  onclick: () => close() });

    const submit = async (e) => {
      e.preventDefault();
      if (input.value.trim() !== confirmWord) {
        error.textContent = `Type ${confirmWord} exactly to confirm.`;
        error.hidden = false;
        input.focus();
        return;
      }
      running = true;
      setBusy(confirm, true);
      cancel.disabled = true;
      try {
        await onConfirm();
        running = false;
        close();
      } catch (err) {
        running = false;
        setBusy(confirm, false);
        cancel.disabled = false;
        error.textContent = err.message;
        error.hidden = false;
      }
    };

    dialog.append(el("form", { class: "modal-body", onsubmit: submit },
      el("h2", { id: "confirm-title", text: title }),
      detail ? el("p", { class: "confirm-detail", text: detail }) : null,
      consequence ? el("p", { text: consequence }) : null,
      error,
      el("label", { for: "confirm-input" },
        el("span", { text: `Type ` }),
        el("strong", { text: confirmWord }),
        el("span", { text: ` to confirm` })),
      input,
      el("div", { class: "modal-actions" }, cancel, confirm)));

    dialog.addEventListener("cb-closed", () => settle());
    input.focus();
  });
}

// overflowMenu puts a row's actions behind one control.
function overflowMenu(actions, name) {
  const menu = el("div", { class: "overflow-menu", hidden: true, role: "menu" },
    ...actions.map((a) =>
      el("button", {
        class: "overflow-item" + (a.destructive ? " is-destructive" : ""),
        role: "menuitem", text: a.label,
        // An item Google's menu has that this instance cannot perform is
        // drawn disabled with its reason, rather than left out (#1863:
        // Create log alert without Cloud Monitoring).
        disabled: a.unavailable ? "disabled" : null,
        title: a.unavailable || null,
        "aria-description": a.unavailable || null,
        onclick: () => { menu.hidden = true; if (!a.unavailable) a.run(); },
      })));

  const button = el("button", {
    class: "icon-button overflow-trigger", "aria-label": `Actions for ${name}`,
    "aria-haspopup": "menu", "aria-expanded": "false",
    onclick: (e) => {
      e.stopPropagation();
      // One menu at a time, or two rows' actions sit on screen together and
      // it stops being obvious which row you are acting on.
      for (const other of document.querySelectorAll(".overflow-menu")) {
        if (other !== menu) other.hidden = true;
      }
      menu.hidden = !menu.hidden;
      button.setAttribute("aria-expanded", menu.hidden ? "false" : "true");
    },
    html: '<svg viewBox="0 0 24 24" aria-hidden="true"><circle cx="12" cy="5" r="1.6"/><circle cx="12" cy="12" r="1.6"/><circle cx="12" cy="19" r="1.6"/></svg>',
  });

  const close = () => {
    menu.hidden = true;
    button.setAttribute("aria-expanded", "false");
  };
  document.addEventListener("click", close);

  // Escape closes the menu and gives focus back to its trigger, as it does for
  // every dialog and panel: whether focus is on the trigger or on an item, the
  // user is left where they opened it. Stopped here, so the one keystroke
  // closes the menu and not also whatever the table sits in.
  const wrap = el("div", { class: "overflow" }, button, menu);
  wrap.addEventListener("keydown", (e) => {
    if (e.key !== "Escape" || menu.hidden) return;
    e.preventDefault();
    e.stopPropagation();
    close();
    button.focus();
  });
  return wrap;
}

// A row that is not on screen — the project picker's delete, say — has
// nowhere to show progress, so the handle does nothing.
const NO_ROW = { start() {}, end() {} };

// DELETE_DETAIL explains, per product, what a delete actually removes.
//
// A confirmation that only says "Delete demo-project?" invites the reader to
// assume the resources under it go too. They do not: the registry holds the
// registration and each service holds its own data, so the one operation the
// wording must be exact about is the one it was silent on.
const DELETE_DETAIL = {
  bigquery: "Every table in the dataset, and every row in them, is deleted with it.",
  "bigquery-jobs": "The job's record is deleted: Job history, jobs.get and jobs.list no longer find it. " +
                   "Nothing the job wrote is deleted.",
  "run-jobs": "Every execution of the job is deleted with it, a running one included, " +
              "and its tasks are stopped.",
  "run-instances": "The instance's container is stopped and its configuration deleted.",
  "run-worker-pools": "Every revision of the worker pool is deleted with it, and its instances are stopped.",
  "pubsub-subscriptions": "Messages waiting on this subscription are discarded with it. " +
            "The topic, and its other subscriptions, are not affected.",
  "pubsub-snapshots": "Subscriptions can no longer seek to this snapshot. " +
            "No message is removed, and no subscription's delivery changes.",
  "logging-metrics": "The metric's definition is deleted. No log entry is removed.",
  "logging-sinks": "The sink's definition is deleted. It delivered nothing, so nothing downstream changes.",
  "logging-exclusions": "Entries it matches are stored again from now on. Entries it dropped are not brought back.",
  "logging-saved-queries": "The saved query is deleted. No log entry is removed.",
  "logging-log-scopes": "The log scope is deleted. The projects and views it named are not changed.",
  "logging-buckets": "The bucket is kept as Delete requested for 7 days and can be undeleted until then. No log entry is removed.",
  "pubsub-schemas": "Every revision is deleted with it. A topic that uses it keeps its " +
            "schema settings, which then name _deleted-schema_.",
  projects: "This removes the project's registration only. Buckets, topics, " +
            "queues, secrets and services created under the identifier stay " +
            "where they are, in the services that own them — nothing here " +
            "reaches into those. Re-registering the same identifier makes them " +
            "visible again.",
};

async function deleteResource(route, name, onDone, row = NO_ROW) {
  // Awaited, so the caller knows when the row stops being busy. The interval
  // that matters starts when the request goes out, not when the dialog opens:
  // a row marked busy while someone reads a confirmation would be lying.
  await confirmDestructive({
    title: `Delete ${name}?`,
    detail: DELETE_DETAIL[route.service],
    confirmWord: name,
    onConfirm: async () => {
      const op = recordOperation(`Delete ${name}`);
      row.start();
      try {
        const res = await send(
          `/api/resources/${route.service}?project=${encodeURIComponent(currentProject())}` +
          `&name=${encodeURIComponent(name)}`, "DELETE");
        op.succeeded("", res.operation);
        row.end();
        notify(`Deleted ${name}`);
        // The picker holds a copy of the registry, and a project it still lists
        // after the delete is one the toolbar will keep offering. Announced
        // rather than reached into, so the picker owns its own state.
        if (route.service === "projects") {
          window.dispatchEvent(new CustomEvent("cb-projects-changed"));
        }
        onDone();
      } catch (err) {
        op.failed(err.message, err.operation);
        row.end();
        notify(`Could not delete ${name}: ${err.message}`, "error");
        throw err;
      }
    },
  });
}

// runAction performs one action on a resource.
//
// `target` is either a name, which is how a list row addresses itself, or an
// array of path segments, which is how a detail page addresses a resource
// inside a resource. The backend distinguishes the two, so the client does
// not have to flatten one into the other.
// hiddenValues is hidden fields as [name, value] pairs: what the page was
// drawn from, sent back as it came (#923).
const hiddenValues = (fields) => fields.map((f) => [f.name, f.default || ""]);

// showActionResult opens a dialog with the rows an action performed on
// click answered (#1592), as openActionForm shows a form's.
function showActionResult(title, result, at = null) {
  const { dialog, close } = openModal({ labelledBy: "action-title" });
  const done = el("button", { type: "button", class: "primary", text: "Close", onclick: () => close() });
  dialog.append(el("div", { class: "modal-body", id: "action-result" },
    el("h2", { id: "action-title", text: title }),
    resultTable(result, at),
    el("div", { class: "modal-actions" }, done)));
  done.focus();
}

async function runAction(route, target, action, onDone, row = NO_ROW, title = null) {
  // An action that is a link opens its page (#1847): a topic row's Create
  // subscription.
  if (action.href) return navigate(action.href);
  const path = Array.isArray(target) ? target : null;
  // A page's own heading, when its last segment is an address no reader
  // recognises: a Datastore entity's encoded key (#875). A delete is
  // confirmed by typing back what the page is headed, not the address.
  const name = title || (path ? path[path.length - 1] : target);
  // An action that declares fields needs a value before it can be performed,
  // so it asks for one rather than firing on click. The form is the create
  // form: one implementation, so an action's inputs validate the way every
  // other input does.
  if ((action.fields || []).some((f) => f.type !== "hidden")) {
    return openActionForm(route, path || [name], action, onDone, title);
  }
  const body = path
    ? { Path: path, Action: action.id }
    : { Name: name, Action: action.id };
  // An action with only hidden fields is still performed on click, and
  // carries what it was drawn from: Remove value, the property it addresses
  // a value in (#923).
  const carried = hiddenValues((action.fields || []).filter((f) => f.type === "hidden"));
  if (carried.length) body.Values = Object.fromEntries(carried);
  const apply = async () => {
    const op = recordOperation(`${action.label} ${name}`);
    row.start();
    try {
      const res = await send(
        `/api/actions/${route.service}?project=${encodeURIComponent(currentProject())}`,
        "POST", body);
      op.succeeded("", res.operation);
      row.end();
      notify(`${action.label} applied to ${name}`);
      onDone();
      // An action performed on click can answer with rows too: Storage
      // shrink config's minimal size (#1592). They were dropped; they are
      // shown as a form's action shows them. A destructive action's answer
      // is its operation, which the notification and the Operations tab
      // already say: Google's console opens no dialog after Delete folder
      // and contents, so only an action whose answer is data opens one.
      if (res.result && !action.destructive) {
        showActionResult(`${action.label}: ${name}`, res.result, path ? { route, path } : null);
      }
    } catch (err) {
      op.failed(err.message, err.operation);
      row.end();
      notify(`${action.label} failed for ${name}: ${err.message}`, "error");
      throw err;
    }
  };

  if (!action.destructive && !action.confirm) {
    try { await apply(); } catch { /* reported by notify */ }
    return;
  }
  // An action that says what it puts at stake — Lock retention policy,
  // which cannot be undone (#789) — says it, and its button is its own verb.
  // One that changes state without deleting anything, and has no form to
  // submit — Rotate server CA, Restart, Set as default — is confirmed the
  // same way before it is sent, as its Confirm promises, without the
  // closing "cannot be undone".
  await confirmDestructive({
    title: `${action.label} ${name}?`,
    detail: action.confirm || undefined,
    confirmWord: name,
    confirmLabel: action.confirm ? action.label : undefined,
    consequence: action.destructive ? undefined : "",
    onConfirm: apply,
  });
}

// stateOf classifies a status word into a colour.
//
// It used to be three exact-match lists, so every word nobody had thought of
// rendered grey — and the words nobody had thought of were the ones that
// matter. A pod in CrashLoopBackOff or ImagePullBackOff, a Warning event, a
// container that exited non-zero: each of those came back "" and was drawn as
// neutral, which is the console saying nothing is wrong while something is.
//
// The suffix rules are what make it hold up against a word that has not been
// invented yet. Kubernetes names its container-waiting reasons consistently —
// anything ending in BackOff or beginning with Err or Failed is a problem —
// and a classifier that only knows today's list will be wrong again tomorrow.
function stateOf(status) {
  if (!status) return "";
  const s = String(status).toLowerCase();

  if (["ready", "running", "enabled", "active", "succeeded", "completed",
       "normal", "true"].includes(s)) return "ok";

  if (["failed", "error", "destroyed", "evicted", "oomkilled",
       "deadlineexceeded"].includes(s)) return "error";
  // CrashLoopBackOff, ImagePullBackOff, ErrImagePull,
  // CreateContainerConfigError, InvalidImageName — named by pattern rather
  // than listed, because the list is the cluster's to extend, not ours.
  if (s.endsWith("backoff") || s.startsWith("err") || s.startsWith("failed") ||
      s.endsWith("error")) return "error";

  if (["pending", "paused", "disabled", "unknown", "warning", "terminating",
       "containercreating", "podinitializing", "notready"].includes(s)) return "warn";

  return "";
}

function notFound(view, path) {
  setChildren(view, 
    el("h1", { text: "Page not found" }),
    emptyState("No such screen", `${path} does not exist in this console.`)
  );
}

// --- router ----------------------------------------------------------

// route dispatches, and does not let a screen fail silently.
//
// dispatch() returns the render's promise, which nobody used to await: a
// throw inside a render function left the skeleton on screen permanently.
function route() {
  const view = document.getElementById("view");
  const title = () => {
    const match = ROUTES.find((r) => r.path === location.pathname);
    return match ? match.title : "This screen";
  };
  try {
    const pending = dispatch(view);
    if (pending && typeof pending.catch === "function") {
      pending.then(syncStickyOffsets, (err) => screenFailed(err, title()));
    } else {
      syncStickyOffsets();
    }
    return pending;
  } catch (err) {
    screenFailed(err, title());
    return undefined;
  }
}

// renderUnavailable is a page Google's console has whose service
// CloudBurrow does not run (#1843): its place in the menu, and why.
// Log Analytics (#1863), as Google's page: the Log views panel beside a
// SQL editor whose FROM names a view as PROJECT_ID.LOCATION.BUCKET_ID.
// LOG_VIEW_ID. The logging-analytics provider runs it over the bucket's
// linked dataset in this instance's BigQuery; a view's name in the panel
// puts Google's default query on it in the editor.
async function renderLogAnalytics(view, match) {
  const caps = capabilityOf("logging-analytics");
  if (!caps.id) return renderUnavailable(view, match);
  const project = currentProject();
  const route = { service: "logging-analytics", title: "Log Analytics" };
  const header = pageHeader("Log Analytics", project ? `Project ${project}` : "All projects");
  setChildren(view, header, loadingState(3, { what: "log views" }));
  let data;
  try {
    data = await api(`/api/resources/logging-analytics?project=${encodeURIComponent(project)}`);
  } catch (err) {
    return setChildren(view, header, errorState("Log views unavailable", String(err.message),
      () => renderLogAnalytics(view, match)));
  }
  const views = data.items || [];
  // The query route asks for a resource: the page's is the project's log
  // views, which the statement itself names.
  const pane = queryPane(route, ["log-views"], caps.query, () => {});
  const editor = pane.querySelector("textarea.query-editor");
  editor.setAttribute("aria-label", "Query");
  const run = [...pane.querySelectorAll("button")].find((b) => b.textContent === "Run");
  if (run) run.textContent = "Run query";
  const defaultQuery = (name) => "SELECT\n  timestamp, severity, log_name, resource.type, text_payload, json_payload\n" +
    `FROM\n  \`${name}\`\nORDER BY timestamp DESC\nLIMIT 1000`;
  if (!editor.value && views.length) {
    const first = views.find((v) => v.name.endsWith("._Default._AllLogs")) || views[0];
    editor.value = defaultQuery(first.name);
  }
  const panel = el("aside", { class: "card log-views", "aria-label": "Log views" },
    el("h2", { text: "Log views" }),
    data.note ? el("p", { class: "unavailable", text: data.note }) : null,
    views.length
      ? el("ul", { class: "log-view-list" }, ...views.map((v) => el("li", {},
          el("button", { type: "button", class: "link-button mono", text: v.name,
            title: `Query ${v.name}`,
            onclick: () => { editor.value = defaultQuery(v.name); editor.dispatchEvent(new Event("input")); editor.focus(); } }),
          el("span", { class: "unavailable", text: ` Linked dataset: ${(v.fields || {})["Linked dataset in BigQuery"] || "—"}` }))))
      : el("p", { class: "unavailable", text: data.prompt ||
          "No bucket of this project is upgraded to use Log Analytics. Upgrade one in Logs Storage, and Create link to query it here." }));
  setChildren(view, header, el("div", { class: "log-analytics" }, panel, pane));
  announce("Log Analytics");
}

function renderUnavailable(view, route) {
  setChildren(view, pageHeader(route.title, "Not available locally"),
    el("div", { class: "state" },
      el("h2", { text: "Not available locally" }),
      el("p", { text: route.unavailable })));
  announce(`${route.title}: not available locally`);
}

// routeFor resolves an address to a screen and, past it, a resource path.
//
// docs/console-parity.md section 5 promises /storage/browser/{bucket} and
// /run/{service}: a resource lives at its own address, not in a query
// parameter. The exact match is tried first so /run/create stays the create
// form rather than a service called "create".
function routeFor(pathname) {
  const exact = ROUTES.find((r) => r.path === pathname);
  if (exact) return { route: exact, path: [] };

  // The longest prefix wins, so /kubernetes/workloads beats /kubernetes.
  let best = null;
  for (const r of ROUTES) {
    if (!r.service || r.screen === "create") continue;
    if (pathname === r.path || !pathname.startsWith(r.path + "/")) continue;
    if (!best || r.path.length > best.path.length) best = r;
  }
  if (!best) return { route: null, path: [] };
  const rest = pathname.slice(best.path.length + 1);
  // Each segment was encoded on the way out, so an object key containing a
  // slash survives as its own segment rather than splitting into two.
  return { route: best, path: rest.split("/").filter(Boolean).map(decodeURIComponent) };
}

function dispatch(view) {
  // A page that moved (#1843): /storage/deleted is Buckets' View options
  // now. The project, and anything else in the address, comes along.
  // A moved product's pages keep their resources' addresses below the
  // old path (redirectTree): /run/builds/ID is /cloud-build/builds/ID (#1854).
  const moved = ROUTES.find((r) => r.redirect && (r.path === location.pathname ||
    (r.redirectTree && location.pathname.startsWith(r.path + "/"))));
  if (moved) {
    const to = new URL(moved.redirect + location.pathname.slice(moved.path.length), location.origin);
    for (const [k, v] of new URLSearchParams(location.search)) to.searchParams.set(k, v);
    history.replaceState(history.state, "", to.pathname + to.search);
  }
  markCurrent();
  drawProductNav();

  const { route: match, path: resourcePath } = routeFor(location.pathname);
  document.title = match ? `${match.title} — CloudBurrow` : "CloudBurrow Console";

  stopStream();
  stopMetrics();
  stopMonitoring();
  stopListPoll();
  METRICS_TICK = null;
  stopActivityPolling();
  stopFaultsPoll();
  stopPrediction();
  stopLocalModelsPoll();
  stopGardenPoll();
  REVEAL_SUSPENDED = false;
  if (!match) return notFound(view, location.pathname);
  if (match.screen === "search") return renderSearch(view);
  if (match.screen === "playground") return renderPlayground(view);
  if (match.screen === "predict") return renderPredict(view);
  if (match.screen === "embeddings") return renderEmbeddings(view);
  if (match.screen === "local-models") return renderLocalModels(view);
  if (match.screen === "model-garden") return renderModelGarden(view);
  if (match.screen === "ap-endpoints") return renderHFEndpoints(view);
  if (match.screen === "agent-garden") return renderAgentGarden(view);
  if (match.screen === "not-served") return renderNotServed(view, match);
  if (match.screen === "ap-overview") return renderAgentPlatformOverview(view);
  if (match.screen === "instance") return renderInstance(view);
  if (match.screen === "monitoring") return renderMonitoring(view);
  if (match.screen === "logs") return renderLogs(view);
  if (match.screen === "logs-dashboard") return renderLogsDashboard(view);
  if (match.screen === "log-analytics") return renderLogAnalytics(view, match);
  if (match.screen === "activity") return renderActivity(view);
  if (match.screen === "requests") return renderRequests(view);
  if (match.screen === "faults") return renderFaults(view);
  if (match.screen === "create") return renderCreatePage(view, match);
  if (match.screen === "products") return renderProducts(view);
  if (match.screen === "connect") return renderConnect(view);
  if (match.screen === "unavailable") return renderUnavailable(view, match);
  if (match.screen === "iam-troubleshooter") return renderIAMTroubleshooter(view);
  if (match.screen === "iam-analyzer") return renderIAMAnalyzer(view);
  if (match.screen === "bigquery-studio") return renderBigQueryStudio(view);
  if (match.screen === "new-project") return renderNewProject(view);
  if (!match.service) return renderDashboard(view);
  if (match.screen === "iam-page" && !resourcePath.length) return renderIAMPage(view, match);
  // Custom constraints (#1891): the list, Create custom constraint, and
  // each one's edit page.
  if (match.service === "iam-orgpolicies" && resourcePath[0] === "custom-constraints") {
    if (resourcePath.length === 1) return renderOrgPolicyCustomList(view);
    if (resourcePath.length === 2 && resourcePath[1] === "create") return renderOrgPolicyCustomEdit(view, null);
    if (resourcePath.length === 3 && resourcePath[2] === "edit") return renderOrgPolicyCustomEdit(view, resourcePath[1]);
  }
  if (match.service === "iam-orgpolicies" && resourcePath.length === 2 && resourcePath[1] === "edit") return renderOrgPolicyEdit(view, resourcePath[0]);
  if (match.service === "iam-orgpolicies" && !resourcePath.length) {
    // Google's page carries Custom constraint on its title line.
    return Promise.resolve(renderList(view, match)).then(() => {
      const slot = headerActions(Array.from(view.children));
      if (slot && !view.querySelector("#orgpolicy-custom-constraints")) {
        slot.prepend(el("a", { class: "button secondary", id: "orgpolicy-custom-constraints",
          href: "/iam-admin/orgpolicies/custom-constraints", text: "Custom constraints" }));
      }
    });
  }
  if (resourcePath.length) return renderDetail(view, match, resourcePath);
  if (match.service === "firestore") return renderFirestoreData(view, match);
  // The old address still works, so a link someone saved keeps resolving.
  const legacy = new URLSearchParams(location.search).get("resource");
  if (legacy) return renderDetail(view, match, [legacy]);
  return renderList(view, match);
}

// --- Instance (#801) ---------------------------------------------------
//
// `cloudburrow state save|load`, `reset` and `seed`, from the console: Save
// state downloads the archive `state save` writes, with the manifest's list of
// what it captures and what it does not; Load state uploads one, confirmed by
// the instance's name and naming the services it replaces; Reset clears every
// service, or those chosen, optionally in one project, with Reseed when up
// was given a seed file, confirmed by typing the scope; Seed uploads a seed
// document, with If not exists. What the form offers is what GET
// /admin/instance says the admin API accepts, so no control can only fail.
//
// Every call goes to the admin API, in process, through /api/instance, with
// the admin token the developer pastes here once per tab. The console adds
// no token of its own: a workload in the cluster can reach this console on
// Docker Desktop, so one that it added would be a way around the token
// (#553). The session key is the one the fault screen and the diagnose
// download use, so a token pasted on any of them serves all three.
const INSTANCE_TOKEN_KEY = "cb-admin-token";
// Held in memory as well, for a browser that refuses session storage.
let INSTANCE_TOKEN = "";
// A save or a load streams the whole archive; the server bounds either at
// ten minutes.
const INSTANCE_DEADLINE_MS = 11 * 60 * 1000;

function instanceToken() {
  if (INSTANCE_TOKEN) return INSTANCE_TOKEN;
  try { INSTANCE_TOKEN = sessionStorage.getItem(INSTANCE_TOKEN_KEY) || ""; } catch { /* storage refused */ }
  return INSTANCE_TOKEN;
}

function setInstanceToken(value) {
  INSTANCE_TOKEN = value;
  try {
    if (value) sessionStorage.setItem(INSTANCE_TOKEN_KEY, value);
    else sessionStorage.removeItem(INSTANCE_TOKEN_KEY);
  } catch { /* storage refused */ }
}

// instanceFetch is one request to /api/instance with the page's token. A
// refusal throws the admin API's message, with its status and body.
async function instanceFetch(path, { method = "GET", body, contentType, deadline = READ_DEADLINE_MS } = {}) {
  const headers = { Accept: "application/json" };
  const token = instanceToken();
  if (token) headers.Authorization = `Bearer ${token}`;
  if (contentType) headers["Content-Type"] = contentType;
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), deadline);
  let res;
  try {
    res = await fetch(path, { method, headers, body, signal: controller.signal });
  } catch (err) {
    if (controller.signal.aborted) throw new Error(`${path} did not answer within ${Math.round(deadline / 1000)}s`);
    throw err;
  } finally {
    clearTimeout(timer);
  }
  if (!res.ok) {
    let parsed = {};
    try { parsed = await res.json(); } catch { /* not JSON */ }
    const failure = new Error(parsed.error || `${path} responded ${res.status} ${res.statusText}`);
    failure.status = res.status;
    failure.body = parsed;
    if (parsed.operation) failure.operation = parsed.operation;
    throw failure;
  }
  return res;
}

// stateManifest reads manifest.json, an archive's first entry, in the
// browser, so Load state can name what the archive replaces before anything
// is sent. The admin API reads it again and decides.
async function stateManifest(file) {
  if (typeof DecompressionStream === "undefined") throw new Error("this browser cannot read a gzip file");
  const reader = file.stream().pipeThrough(new DecompressionStream("gzip")).getReader();
  let buf = new Uint8Array(0);
  const need = async (n) => {
    while (buf.length < n) {
      const { value, done } = await reader.read();
      if (done) throw new Error("the archive ends before its manifest");
      const next = new Uint8Array(buf.length + value.length);
      next.set(buf);
      next.set(value, buf.length);
      buf = next;
    }
  };
  try {
    await need(512);
    const text = (from, len) => new TextDecoder().decode(buf.subarray(from, from + len)).replace(/\0.*$/s, "").trim();
    if (text(0, 100) !== "manifest.json") throw new Error("manifest.json is not its first entry");
    const size = parseInt(text(124, 12), 8);
    if (!(size > 0 && size <= (1 << 20))) throw new Error("its manifest has no readable size");
    await need(512 + size);
    return JSON.parse(new TextDecoder().decode(buf.subarray(512, 512 + size)));
  } finally {
    reader.cancel().catch(() => {});
  }
}

const INSTANCE_TITLE = "Instance";
const INSTANCE_SUBTITLE =
  "Save, load, reset and seed this instance's state, as cloudburrow state, reset and seed do. " +
  "Each action is the admin API's, with the instance's admin token.";

async function renderInstance(view) {
  // Reached from Settings and utilities too, whose panel the link leaves open.
  const settings = document.getElementById("settings-panel");
  if (settings && overlayOpen(settings)) {
    hideOverlay(settings);
    document.getElementById("settings").setAttribute("aria-expanded", "false");
  }
  const header = () => pageHeader(INSTANCE_TITLE, INSTANCE_SUBTITLE);
  setChildren(view, header(), loadingState(4));
  let info;
  try {
    info = await (await instanceFetch("/api/instance")).json();
  } catch (err) {
    if (location.pathname !== "/instance") return;
    if (err.status === 401) return renderInstanceToken(view, err);
    setChildren(view, header(), errorState("Instance unavailable", err.message, () => renderInstance(view)));
    return;
  }
  if (location.pathname !== "/instance") return;
  const instance = (info.state && info.state.instance) || "this instance";
  // A refusal of the token mid-session goes back to the token form.
  const refused = (err) => {
    if (err.status !== 401) return false;
    renderInstanceToken(view, err);
    return true;
  };
  setChildren(view, header(),
    instanceSaveCard(info, refused),
    instanceLoadCard(info, instance, refused),
    instanceResetCard(info, refused),
    instanceSeedCard(info, refused));
}

// instanceResult is a card's outcome line.
function instanceResult(id) {
  return el("p", { class: "muted", id, role: "status" });
}

function instanceSaveCard(info, refused) {
  const services = (info.state && info.state.services) || [];
  const captured = services.filter((s) => s.captured);
  const left = services.filter((s) => !s.captured);
  const result = instanceResult("instance-save-result");
  const error = el("p", { class: "form-error", role: "alert", id: "instance-save-error", hidden: true });
  const button = el("button", { class: "primary", id: "instance-save", text: "Save state" });
  button.addEventListener("click", async () => {
    error.hidden = true;
    result.textContent = "";
    setBusy(button, true);
    const op = recordOperation("Save state");
    try {
      const res = await instanceFetch("/api/instance/save", { method: "POST", deadline: INSTANCE_DEADLINE_MS });
      const blob = await res.blob();
      const match = /filename="([^"]+)"/.exec(res.headers.get("Content-Disposition") || "");
      const name = match ? match[1] : "cloudburrow-state.tar.gz";
      const url = URL.createObjectURL(blob);
      // Not attached to the document, so the router never sees the click
      // and the browser saves the file.
      el("a", { href: url, download: name }).click();
      setTimeout(() => URL.revokeObjectURL(url), 60000);
      op.succeeded(name, res.headers.get("X-Cloudburrow-Operation") || undefined);
      result.textContent = `Downloaded ${name} (${formatBytes(blob.size)}).`;
      notify(`Saved state as ${name}`);
    } catch (err) {
      op.failed(err.message, err.operation);
      if (refused(err)) return;
      error.textContent = `The state was not saved: ${err.message}`;
      error.hidden = false;
    } finally {
      setBusy(button, false);
    }
  });
  return el("section", { class: "card", id: "instance-save-card", "aria-labelledby": "instance-save-title" },
    el("h2", { id: "instance-save-title", text: "Save state" }),
    el("p", { text: "Downloads the archive cloudburrow state save writes: the state of each service it captures, " +
      "which Load state, or cloudburrow state load, puts back." }),
    el("ul", { id: "instance-captured" },
      ...captured.map((s) => el("li", { "data-service": s.name }, el("strong", { text: s.name }), el("span", { text: ": captured" }))),
      ...left.map((s) => el("li", { "data-service": s.name },
        el("strong", { text: s.name }), el("span", { text: `: not captured, ${s.reason}` })))),
    info.state && info.state.contains_secret_values
      ? el("p", { class: "form-help", id: "instance-secret-warning",
          text: "The archive holds secret values in plain form: keep it as you would a secret." })
      : null,
    error,
    el("div", { class: "form-actions" }, button),
    result);
}

function instanceLoadCard(info, instance, refused) {
  const input = el("input", { id: "instance-load-file", type: "file", accept: ".gz,.tgz,application/gzip" });
  const result = instanceResult("instance-load-result");
  const error = el("p", { class: "form-error", role: "alert", id: "instance-load-error", hidden: true });
  const button = el("button", { class: "primary danger", id: "instance-load", text: "Load state", disabled: true });
  input.addEventListener("change", () => { button.disabled = !input.files.length; });
  button.addEventListener("click", async () => {
    const file = input.files[0];
    if (!file) return;
    error.hidden = true;
    result.textContent = "";
    let replaced;
    try {
      const m = await stateManifest(file);
      replaced = (m.services || []).filter((s) => s.captured).map((s) => s.name);
    } catch (err) {
      // Named rather than guessed at; the admin API reads it and decides.
      replaced = null;
      result.textContent = `The archive's manifest could not be read here (${err.message}).`;
    }
    const detail = replaced
      ? `Replaces the state of ${replaced.length ? replaced.join(", ") : "no service"} with what ${file.name} holds. ` +
        "What those services hold now is gone afterwards; services the archive does not capture are left as they are."
      : `Replaces the state of every service ${file.name} captures with what it holds.`;
    await confirmDestructive({
      confirmLabel: "Load",
      title: `Load state into ${instance}`,
      detail,
      confirmWord: instance,
      onConfirm: async () => {
        const op = recordOperation("Load state");
        let res;
        try {
          res = await (await instanceFetch("/api/instance/load", { method: "POST", body: file,
            contentType: "application/gzip", deadline: INSTANCE_DEADLINE_MS })).json();
          op.succeeded((res.loaded || []).join(", "), res.operation);
        } catch (err) {
          op.failed(err.message, err.operation);
          if (refused(err)) return;
          throw err;
        }
        const left = (res.not_captured || []).map((s) => s.name);
        result.textContent = `Loaded ${(res.loaded || []).join(", ") || "nothing"} from ${file.name}.` +
          (left.length ? ` Not in the archive: ${left.join(", ")}.` : "");
        notify(`Loaded state from ${file.name}`);
      },
    });
  });
  return el("section", { class: "card", id: "instance-load-card", "aria-labelledby": "instance-load-title" },
    el("h2", { id: "instance-load-title", text: "Load state" }),
    el("p", { text: "Uploads an archive from Save state or cloudburrow state save, and replaces the state of each service " +
      "it captures with it, as cloudburrow state load does." }),
    el("div", { class: "form-row" }, el("label", { for: "instance-load-file", text: "State archive" }), input,
      el("p", { class: "form-help", text: "Up to 5 GiB, whatever the upload limit in Settings and utilities; " +
        "a larger archive loads with cloudburrow state load." })),
    error,
    el("div", { class: "form-actions" }, button),
    result);
}

function instanceResetCard(info, refused) {
  const targets = info.reset || [];
  const reseedable = (info.reseed || []).length > 0;
  const boxes = targets.map((t) => el("input", { type: "checkbox", id: `instance-reset-${t.name}`,
    value: t.name, checked: true, "data-by-project": t.byProject ? "true" : "false" }));
  const project = el("input", { id: "instance-reset-project", type: "text", autocomplete: "off", spellcheck: "false" });
  const reseed = el("input", { type: "checkbox", id: "instance-reseed", disabled: !reseedable });
  const note = el("p", { class: "form-help", id: "instance-reset-note" });
  const result = instanceResult("instance-reset-result");
  const error = el("p", { class: "form-error", role: "alert", id: "instance-reset-error", hidden: true });
  const button = el("button", { class: "primary danger", id: "instance-reset", text: "Reset" });

  // What the admin API refuses is not offered: a component that cannot be
  // scoped to a project is unchecked and disabled while a project is named,
  // and Reseed is disabled with a project, since a seed file is not scoped to
  // one, and without a startup seed.
  const sync = () => {
    const scoped = project.value.trim() !== "";
    for (const b of boxes) {
      const can = !scoped || b.dataset.byProject === "true";
      b.disabled = !can;
      if (!can) b.checked = false;
    }
    reseed.disabled = !reseedable || scoped;
    if (reseed.disabled) reseed.checked = false;
    const cannot = targets.filter((t) => !t.byProject).map((t) => t.name);
    note.textContent = scoped && cannot.length
      ? `${cannot.join(", ")} cannot be reset by project, so ${cannot.length === 1 ? "it is" : "they are"} left out.`
      : "";
    button.disabled = !boxes.some((b) => b.checked);
  };
  project.addEventListener("input", sync);
  for (const b of boxes) b.addEventListener("change", sync);
  sync();

  button.addEventListener("click", async () => {
    error.hidden = true;
    result.textContent = "";
    const p = project.value.trim();
    const chosen = boxes.filter((b) => b.checked).map((b) => b.value);
    const every = !p && chosen.length === targets.length;
    const q = new URLSearchParams();
    if (!every) for (const s of chosen) q.append("service", s);
    if (p) q.set("project", p);
    if (reseed.checked) q.set("reseed", "true");
    // The scope typed back is the one the sentence names: the project when
    // there is one, else the services, else all.
    const word = p || (every ? "all" : chosen.join(","));
    const what = every ? "every service" : chosen.join(", ");
    await confirmDestructive({
      confirmLabel: "Reset",
      title: p ? `Reset project ${p}` : every ? "Reset every service" : `Reset ${what}`,
      detail: `Deletes the state of ${what}${p ? ` in project ${p}; other projects are left alone` : ""}.` +
        (reseed.checked ? " Then re-applies the startup seed to the services reset." : "") +
        " Fault rules on the services reset are cleared too. The instance keeps serving.",
      confirmWord: word,
      onConfirm: async () => {
        const op = recordOperation(`Reset ${what}${p ? ` in ${p}` : ""}`);
        let res;
        try {
          res = await (await instanceFetch(`/api/instance/reset${q.toString() ? "?" + q : ""}`,
            { method: "POST", deadline: INSTANCE_DEADLINE_MS })).json();
          op.succeeded((res.reset || []).join(", "), res.operation);
        } catch (err) {
          op.failed(err.message, err.operation);
          if (refused(err)) return;
          throw err;
        }
        result.textContent = `Reset ${(res.reset || []).join(", ") || "nothing"}${p ? ` in project ${p}` : ""}.` +
          ((res.reseeded || []).length ? ` Reseeded ${res.reseeded.join(", ")}.` : "");
        notify(p ? `Reset project ${p}` : `Reset ${what}`);
      },
    });
  });

  return el("section", { class: "card", id: "instance-reset-card", "aria-labelledby": "instance-reset-title" },
    el("h2", { id: "instance-reset-title", text: "Reset" }),
    el("p", { text: "Deletes state through each service's own API, as cloudburrow reset does under a running up. " +
      "The cluster and its pods are untouched." }),
    el("fieldset", { class: "form-row", id: "instance-reset-services" },
      el("legend", { text: "Services" }),
      ...boxes.map((b) => el("div", { class: "form-row is-check" }, el("div", { class: "check-line" }, b,
        el("label", { for: b.id, text: b.value + (b.dataset.byProject === "true" ? "" : " (not by project)") }))))),
    el("div", { class: "form-row" },
      el("label", { for: "instance-reset-project", text: "Project" }), project,
      el("p", { class: "form-help", text: "Only this project's state. Empty resets every project." })),
    note,
    el("div", { class: "form-row is-check" },
      el("div", { class: "check-line" }, reseed, el("label", { for: "instance-reseed", text: "Reseed" })),
      el("p", { class: "form-help", id: "instance-reseed-help", text: reseedable
        ? `Re-applies the seed file up was started with (${info.reseed.join(", ")}) to the services reset. Not with a project.`
        : "Unavailable: up was not started with a seed file." })),
    error,
    el("div", { class: "form-actions" }, button),
    result);
}

function instanceSeedCard(info, refused) {
  const input = el("input", { id: "instance-seed-file", type: "file", accept: ".json,application/json" });
  const skip = el("input", { type: "checkbox", id: "instance-seed-if-not-exists" });
  const result = instanceResult("instance-seed-result");
  const error = el("p", { class: "form-error", role: "alert", id: "instance-seed-error", hidden: true });
  const button = el("button", { class: "primary", id: "instance-seed", text: "Seed", disabled: true });
  input.addEventListener("change", () => { button.disabled = !input.files.length; });
  button.addEventListener("click", async () => {
    const file = input.files[0];
    if (!file) return;
    error.hidden = true;
    result.textContent = "";
    setBusy(button, true);
    const op = recordOperation(`Seed from ${file.name}`);
    try {
      const res = await (await instanceFetch(`/api/instance/seed${skip.checked ? "?ifNotExists=true" : ""}`,
        { method: "POST", body: file, contentType: "application/json", deadline: WRITE_DEADLINE_MS })).json();
      op.succeeded((res.seeded || []).join(", "), res.operation);
      result.textContent = `Seeded ${(res.seeded || []).join(", ") || "nothing"} from ${file.name}.`;
      notify(`Seeded from ${file.name}`);
    } catch (err) {
      op.failed(err.message, err.operation);
      if (refused(err)) return;
      const before = err.body && (err.body.seeded || []).length ? ` Seeded before the failure: ${err.body.seeded.join(", ")}.` : "";
      error.textContent = `The seed failed: ${err.message}.${before}`;
      error.hidden = false;
    } finally {
      setBusy(button, false);
      button.disabled = !input.files.length;
    }
  });
  return el("section", { class: "card", id: "instance-seed-card", "aria-labelledby": "instance-seed-title" },
    el("h2", { id: "instance-seed-title", text: "Seed" }),
    el("p", { text: "Uploads a seed document, as cloudburrow seed does: every component is checked before any is created." }),
    el("div", { class: "form-row" }, el("label", { for: "instance-seed-file", text: "Seed document" }), input,
      el("p", { class: "form-help", text: (info.seed || []).length
        ? `Its components may be ${info.seed.join(", ")}.`
        : "No component on this instance can be seeded." })),
    el("div", { class: "form-row is-check" },
      el("div", { class: "check-line" }, skip, el("label", { for: "instance-seed-if-not-exists", text: "If not exists" })),
      el("p", { class: "form-help", text: "Skips what already exists, so the same document can be applied again." })),
    error,
    el("div", { class: "form-actions" }, button),
    result);
}

// renderInstanceToken asks for the admin token, which the admin API refused
// the page without (or with a wrong one).
function renderInstanceToken(view, err) {
  const hadToken = !!instanceToken();
  setInstanceToken("");
  const file = err.body && err.body.token_file;
  const input = el("input", { id: "instance-token", type: "password", autocomplete: "off", spellcheck: "false" });
  const error = el("p", { class: "form-error", role: "alert", id: "instance-token-error", hidden: !hadToken,
    text: hadToken ? `The admin API refused that token: ${err.message}` : "" });
  const submit = (e) => {
    e.preventDefault();
    const value = input.value.trim();
    if (!value) {
      error.textContent = "Paste the instance's admin token.";
      error.hidden = false;
      input.focus();
      return;
    }
    setInstanceToken(value);
    renderInstance(view);
  };
  setChildren(view, pageHeader(INSTANCE_TITLE, INSTANCE_SUBTITLE),
    el("form", { class: "card", id: "instance-token-form", novalidate: true, onsubmit: submit },
      el("h2", { text: "Admin token required" }),
      el("p", { text: "Saving, loading, resetting and seeding need the instance's admin token, as /admin does. " +
        "A workload in the cluster can reach this console, so the console adds no token of its own." }),
      file ? el("p", {}, el("span", { text: "The token is the contents of " }), el("code", { text: file }), el("span", { text: "." })) : null,
      error,
      el("div", { class: "form-row" },
        el("label", { for: "instance-token", text: "Admin token" }),
        input,
        el("p", { class: "form-help", text: "Kept for this tab only, and sent with each request this page makes." })),
      el("div", { class: "form-actions" }, el("button", { type: "submit", class: "primary", text: "Use token" }))));
  input.focus();
  announce("Admin token required");
}

// syncStickyOffsets measures the pinned blocks so the ones below them know
// where to stop.
//
// The header block's height is not a constant: a list screen has a title and
// a subtitle, a detail screen adds a breadcrumb and may add a tab strip.
// Guessing it leaves either a gap under the toolbar or a bar hidden behind
// the header, and the header is the thing that moves.
function syncStickyOffsets() {
  const root = document.documentElement;
  // A header that scrolls with the page (a resource's, with its details,
  // #1842) holds nothing over the table, so it offsets nothing.
  const height = (sel) => {
    const node = document.querySelector(sel);
    if (!node || getComputedStyle(node).position !== "sticky") return "0px";
    return `${Math.round(node.getBoundingClientRect().height)}px`;
  };
  root.style.setProperty("--page-header-h", height("#view .page-header"));
  root.style.setProperty("--action-bar-h", height("#view .action-bar"));
}

// drawProductNav draws the left-hand menu of the product the current screen
// belongs to (#1842): the product's mark and name at the top, its pages
// listed below, and a control that collapses the pane to a rail.
//
// The drawer answers "which products exist"; this answers "which pages does
// this product have", which is how somebody who uses the real console looks
// for something: product first, page second. It is drawn on a product's list
// screens, on its resources' pages and on its create pages, so the menu stays
// put while the developer works inside the product, as Google's does.
//
// A category screen adds nothing here: a product's pages are its ROUTES
// entries sharing a `product` key, so a new page appears in the menu by being
// declared there. `navTitle` names a page in the menu when its screen title
// is the product's own name (Cloud Tasks' only page is "Queues").
const PRODUCT_NAV_KEY = "cloudburrow.productnav";

function productOfLocation() {
  const { route: match } = routeFor(location.pathname);
  if (!match) return null;
  const owner = match.screen === "create" ? match.of : match;
  return owner && owner.section ? owner : null;
}

function drawProductNav() {
  const host = document.getElementById("product-nav");
  if (!host) return;

  const here = productOfLocation();
  // Only the pages this instance serves, as the drawer lists only those,
  // and the Agent Platform pages that draw their own off state
  // (listedWhenOff), which Google's menu always lists.
  const served = new Set(SERVICES.map((sv) => sv.id));
  const pages = here
    ? pagesOf(productKey(here)).filter((r) => (!r.service || served.has(r.service) || r.listedWhenOff || r === here) && !r.navHidden &&
        (!r.requires || served.has(r.requires) || r === here))
    : [];
  if (!pages.length) {
    host.hidden = true;
    document.documentElement.removeAttribute("data-product-nav");
    setChildren(host);
    return;
  }

  const collapsed = readStored(PRODUCT_NAV_KEY, "open") === "collapsed";
  host.hidden = false;
  document.documentElement.setAttribute("data-product-nav", collapsed ? "collapsed" : "open");
  const title = productTitle(here);
  const toggle = el("button", {
    class: "icon-button product-nav-toggle", type: "button",
    "aria-expanded": collapsed ? "false" : "true", "aria-controls": "product-nav-pages",
    "aria-label": collapsed ? `Expand ${title} navigation menu` : `Collapse ${title} navigation menu`,
    title: collapsed ? "Expand left pane" : "Collapse left pane",
    html: collapsed
      ? '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M9 6l6 6-6 6"/></svg>'
      : '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M15 6l-6 6 6 6"/></svg>',
    onclick: () => {
      writeStored(PRODUCT_NAV_KEY, collapsed ? "open" : "collapsed");
      drawProductNav();
      const again = host.querySelector(".product-nav-toggle");
      if (again) again.focus();
    },
  });
  // The page this screen is on: the list itself, or the list a resource or
  // a create form belongs to.
  // A `tabOf` entry (#1845) is current on its page's tab, and links to
  // the tab of the resource being looked at.
  const tabHere = new URLSearchParams(location.search).get("tab");
  const ownerPath = (page) => page.tabOf ? page.tabOf.route : null;
  const resourceHere = (page) => {
    const prefix = ownerPath(page) + "/";
    if (!location.pathname.startsWith(prefix)) return "(default)";
    return decodeURIComponent(location.pathname.slice(prefix.length).split("/")[0]) || "(default)";
  };
  const current = pages.find((page) => page.tabOf && here.path === page.tabOf.route &&
      location.pathname.startsWith(page.tabOf.route + "/") && tabHere === page.tabOf.tab) ||
    pages.find((page) => page.path === here.path && !page.tabOf);
  const hrefOf = (page) => page.tabOf
    ? `${page.tabOf.route}/${encodeURIComponent(resourceHere(page))}${scopeSearch()}${scopeSearch() ? "&" : "?"}tab=${page.tabOf.tab}`
    : page.path + scopeSearch();
  setChildren(host,
    el("div", { class: "product-nav-head" },
      el("span", { class: "nav-icon", "aria-hidden": "true" }, markFor(here)),
      el("span", { class: "product-nav-title", text: title }),
      toggle),
    el("ul", { id: "product-nav-pages", hidden: collapsed ? true : null }, ...pages.map((page) =>
      el("li", {},
        el("a", {
          href: hrefOf(page),
          class: page === current ? "is-current" : null,
          "aria-current": page === current ? "page" : null,
          text: page.navTitle || page.title,
        })))));
}

// navigate goes to a path inside the console, keeping the project scope.
//
// The same thing a link click does, for the places where the control is a
// button because it is an action rather than a destination.
function navigate(path) {
  const url = new URL(path, location.origin);
  const project = currentProject();
  if (project) url.searchParams.set("project", project);
  const scoped = scopeServiceOfPath(url.pathname);
  if (scoped && scopeServiceOfPath(location.pathname) === scoped) carryScope(url, scoped);
  history.pushState({}, "", url);
  // Focus lands on the main region first and the screen is drawn after, so a
  // screen that wants focus somewhere specific — a form's first field — is
  // the last one to move it rather than the first.
  document.getElementById("main").focus();
  route();
}

function initRouting() {
  document.addEventListener("click", (e) => {
    // A link whose own handler took the click (BigQuery Studio's Explorer
    // opens a resource as a tab, #1861) is not also followed.
    if (e.defaultPrevented) return;
    const link = e.target.closest("a[href]");
    if (!link) return;
    const url = new URL(link.href, location.origin);
    if (url.origin !== location.origin) return;
    e.preventDefault();
    history.pushState({}, "", url);
    document.getElementById("main").focus();
    route();
  });
  window.addEventListener("popstate", route);
}

// rescopeLinks moves every in-console link scoped to the project being left
// onto the one just chosen. Links carry the project in their href from when
// they were drawn (scopeSearch), and the navigation is drawn once, so after
// the picker changed project every nav link still named the old one: a
// click went back to it, or to "All projects" when it had none, and every
// screen looked pinned to the first project. A link that names some other
// project on purpose (a search hit, a project's own page) is left alone.
function rescopeLinks(from, to) {
  for (const a of document.querySelectorAll("a[href]")) {
    const u = new URL(a.getAttribute("href"), location.origin);
    if (u.origin !== location.origin) continue;
    // No project counts as the one being left: the navigation is drawn
    // before the default project is put in the address, so its links start
    // with none.
    const p = u.searchParams.get("project") || "";
    if (p !== "" && p !== (from || "")) continue;
    if (to) u.searchParams.set("project", to);
    else u.searchParams.delete("project");
    a.setAttribute("href", u.pathname + u.search + u.hash);
  }
}

// The project picker.
//
// Projects come from the registry rather than from scanning resources that
// happen to exist. Scanning could not show a project with nothing in it, and
// could not offer to make one — so the console could never answer "which
// projects are there?" or "give me a new one".
// The Select a project dialog's Recent and Starred views (#1842).
const RECENT_PROJECTS_KEY = "cloudburrow.projects.recent";
const STARRED_PROJECTS_KEY = "cloudburrow.projects.starred";
const rememberRecent = (id) => {
  if (id) writeStored(RECENT_PROJECTS_KEY, [id, ...readStored(RECENT_PROJECTS_KEY, []).filter((x) => x !== id)].slice(0, 10));
};

async function initProjects() {
  const button = document.getElementById("project-button");
  const panel = document.getElementById("project-panel");
  const current = document.getElementById("project-current");
  const list = document.getElementById("project-list");
  const filter = document.getElementById("project-filter");
  const newButton = document.getElementById("project-new");

  let selected = new URLSearchParams(location.search).get("project") || "";
  if (!selected && DEFAULT_PROJECT) {
    selected = DEFAULT_PROJECT;
    const url = new URL(location.href);
    url.searchParams.set("project", selected);
    history.replaceState({}, "", url);
  }

  // Each opening is numbered, so a list read that finishes after the picker
  // was closed (New project, a choice, Escape) does not open it again.
  let opening = 0;
  const closePicker = () => {
    opening++;
    hideOverlay(panel);
    button.setAttribute("aria-expanded", "false");
  };

  const select = (id) => {
    rememberRecent(id);
    const url = new URL(location.href);
    if (id) url.searchParams.set("project", id);
    else url.searchParams.delete("project");
    history.pushState({}, "", url);
    rescopeLinks(selected, id);
    selected = id;
    current.textContent = id || "Select a project";
    closePicker();
    route();
    // The terminal follows the toolbar's project (#781).
    window.dispatchEvent(new CustomEvent("cb-project-selected"));
  };

  let projects = [];
  // Recent, Starred and All, as Google's dialog offers (#1842); recents and
  // stars are this browser's own, kept in localStorage.
  let view = "all";
  const views = document.getElementById("project-views");
  const starred = () => readStored(STARRED_PROJECTS_KEY, []);
  const recent = () => readStored(RECENT_PROJECTS_KEY, []);
  views.addEventListener("click", (e) => {
    const b = e.target.closest("[data-view]");
    if (!b) return;
    view = b.dataset.view;
    views.querySelectorAll("[data-view]").forEach((x) =>
      x.setAttribute("aria-pressed", x === b ? "true" : "false"));
    draw();
  });
  const toggleStar = (id) => {
    const now = starred();
    writeStored(STARRED_PROJECTS_KEY, now.includes(id) ? now.filter((x) => x !== id) : [...now, id]);
    draw();
  };
  const draw = () => {
    const q = filter.value.trim().toLowerCase();
    const stars = starred();
    const recents = recent();
    let pool = projects;
    if (view === "starred") pool = projects.filter((p) => stars.includes(p.id));
    if (view === "recent") {
      pool = recents.map((id) => projects.find((p) => p.id === id)).filter(Boolean);
    }
    const shown = pool.filter((p) =>
      !q || p.id.toLowerCase().includes(q) || (p.name || "").toLowerCase().includes(q));

    // No "All projects" entry: Google's console always works in one
    // selected project (#1842).
    setChildren(list,
      ...shown.map((p) =>
        el("li", { class: "picker-row" },
          el("button", {
            class: "picker-item" + (selected === p.id ? " is-selected" : ""),
            onclick: () => select(p.id),
          },
            el("span", { class: "picker-item-name", text: p.name || p.id }),
            el("span", { class: "picker-item-id", text: p.id })),
          el("button", {
            type: "button", class: "icon-button picker-star",
            "aria-pressed": stars.includes(p.id) ? "true" : "false",
            "aria-label": `Star ${p.name || p.id}`, title: stars.includes(p.id) ? "Unstar" : "Star",
            html: '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M12 3l2.7 5.6 6.1.9-4.4 4.3 1 6.1L12 17l-5.4 2.9 1-6.1-4.4-4.3 6.1-.9z"/></svg>',
            onclick: () => toggleStar(p.id),
          }))));

    if (!shown.length) {
      list.append(el("li", {}, el("p", { class: "panel-empty",
        text: q ? "No matching projects."
          : view === "starred" ? "No starred projects. Star a project from All." : "No recent projects." })));
    }
  };

  const load = async () => {
    try {
      const data = await api("/api/resources/projects");
      projects = (data.items || []).map((i) => ({
        id: i.name, name: (i.fields || {}).Name || i.name,
      }));
      KNOWN_PROJECTS = projects.map((p) => p.id);
    } catch {
      // The picker still shows what is selected; it simply cannot offer
      // alternatives, which is better than showing none at all. KNOWN_PROJECTS
      // is deliberately left alone: a failed read is not evidence that a
      // project does not exist.
      projects = selected ? [{ id: selected, name: selected }] : [];
    }
    draw();
  };

  // Whether the selected project exists.
  //
  // The URL's ?project= was taken on faith, so a typo or a project someone had
  // deleted left every per-project screen reporting an error apiece — a wall of
  // red on a working instance, with nothing anywhere saying the project was the
  // problem. Checked once at startup and again whenever the registry is read.
  const validate = async () => {
    if (!selected) return;
    await load();
    if (!KNOWN_PROJECTS.length || KNOWN_PROJECTS.includes(selected)) return;
    button.classList.add("is-invalid");
    notify(`Project ${selected} is not in this instance's registry` +
           (DEFAULT_PROJECT ? `. Switching to ${DEFAULT_PROJECT}.` : "."), "error");
    // Switched rather than left broken: leaving it selected means every screen
    // keeps failing, and the user has to work out that the toolbar is lying.
    if (DEFAULT_PROJECT && DEFAULT_PROJECT !== selected) {
      select(DEFAULT_PROJECT);
    } else {
      select(KNOWN_PROJECTS[0] || "");
    }
    button.classList.remove("is-invalid");
  };

  current.textContent = selected || "Select a project";
  filter.addEventListener("input", draw);

  // Re-checked whenever anything deletes a project, which is how the picker
  // stops showing one that is gone. The Resource Manager screen fires this
  // rather than reaching into the picker's own state.
  window.addEventListener("cb-projects-changed", () => { validate(); load(); });
  validate();

  button.addEventListener("click", async () => {
    if (overlayOpen(panel)) return closePicker();
    const mine = ++opening;
    // The project in use is a recent one however it was chosen (a link, the
    // address).
    rememberRecent(selected);
    await load();
    if (mine !== opening) return;
    showOverlay(panel);
    button.setAttribute("aria-expanded", "true");
    filter.focus();
  });
  panel.addEventListener("keydown", (e) => {
    if (e.key === "Escape") { closePicker(); button.focus(); }
  });
  document.addEventListener("click", (e) => {
    // A control the click redrew away (a row's star) is not outside.
    if (!e.target.isConnected) return;
    if (overlayOpen(panel) && !panel.contains(e.target) && !button.contains(e.target)) {
      closePicker();
    }
  });

  // Manage resources is a link; the picker closes behind it.
  document.getElementById("project-manage").addEventListener("click", () => closePicker());
  // New project opens the New project page, as Google's picker does
  // (#1842); the page makes it through the same Resource Manager form, so
  // there is one definition of what a project needs.
  newButton.addEventListener("click", () => {
    closePicker();
    if (!capabilityOf("projects").create) {
      announce("Creating projects is not available");
      return;
    }
    navigate("/projectcreate" + scopeSearch());
  });
  // The New project page selects what it made.
  window.addEventListener("cb-project-select", async (e) => {
    await load();
    select(e.detail);
  });

  await load();
  // A project is always selected (#1842): with no default project, the
  // first in the registry is chosen, as Google's console opens on one.
  if (!selected && KNOWN_PROJECTS.length) select(KNOWN_PROJECTS[0]);
}

// The toolbar search looks across every service.
//
// It used to copy its text into whatever filter happened to be on screen,
// which meant the most prominent control in the console could only narrow the
// page already open — and found nothing at all on a screen without a table.
// SUGGEST_DEBOUNCE_MS is how long the toolbar waits after a keystroke.
//
// A search fans out to every provider, and several shell out to kubectl. Firing
// on each character would mean one fan-out per letter typed; 250ms is long enough
// that a typed word is one request and short enough that the list feels like it
// is following the keyboard.
const SUGGEST_DEBOUNCE_MS = 250;
const SUGGEST_LIMIT = 8;

// matchingPages lists the served pages whose page or product name holds q
// (#1842), products' first pages first: "logs" finds Logs Explorer, Log
// router and the rest of Cloud Logging's pages.
function matchingPages(q) {
  const needle = q.trim().toLowerCase();
  if (!needle) return [];
  const served = new Set(SERVICES.map((sv) => sv.id));
  const out = [];
  for (const r of ROUTES) {
    if (!r.section && r.path !== "/activity") continue;
    if (r.service && !served.has(r.service)) continue;
    const page = r.navTitle || r.title;
    const product = r.productTitle || "";
    const words = `${page} ${product} ${r.title}`.toLowerCase();
    if (!words.includes(needle)) continue;
    out.push({
      path: r.path,
      label: product && product !== page ? `${page} – ${product}` : page,
      kind: "Page",
      rank: page.toLowerCase().startsWith(needle) ? 0 : 1,
    });
  }
  return out.sort((a, b) => a.rank - b.rank).slice(0, 4);
}

function initSearch() {
  const search = document.getElementById("search");

  // Results as you type.
  //
  // Search was Enter-only, so finding anything meant a full navigation and a
  // full page render — and getting the query slightly wrong meant doing it
  // again. The panel shows the first few hits and the products that match, which
  // is what most searches in a console are actually for.
  const panel = el("div", { class: "suggest", id: "search-suggest", hidden: true,
                            role: "listbox", "aria-label": "Search suggestions" });
  search.parentElement.append(panel);
  search.setAttribute("role", "combobox");
  search.setAttribute("aria-expanded", "false");
  search.setAttribute("aria-controls", "search-suggest");
  search.setAttribute("aria-autocomplete", "list");

  let timer = null;
  let inFlight = null;
  let active = -1;

  const hide = () => {
    panel.hidden = true;
    search.setAttribute("aria-expanded", "false");
    active = -1;
  };

  const options = () => [...panel.querySelectorAll("a")];

  const highlight = (i) => {
    const items = options();
    if (!items.length) return;
    // Wrapping, because a list you cannot get out of the bottom of is a list you
    // have to reach for the mouse to leave.
    active = (i + items.length) % items.length;
    items.forEach((a, j) => {
      a.classList.toggle("is-active", j === active);
      a.setAttribute("aria-selected", j === active ? "true" : "false");
    });
    items[active].scrollIntoView({ block: "nearest" });
  };

  const suggest = async () => {
    const q = search.value.trim();
    if (q.length < 2) return hide();
    // The previous request is abandoned rather than awaited: its answer is for a
    // query the user has already moved past, and rendering it would make the
    // panel show results for a prefix of what is on screen.
    if (inFlight) inFlight.abort();
    const cancel = new AbortController();
    inFlight = cancel;
    const project = new URLSearchParams(location.search).get("project") || "";
    let data;
    try {
      data = await api(`/api/search?q=${encodeURIComponent(q)}` +
                       `&project=${encodeURIComponent(project)}`, { signal: cancel.signal });
    } catch {
      return hide();
    }
    if (inFlight !== cancel) return;

    const rows = [];
    for (const product of data.products || []) {
      const r = ROUTES.find((x) => x.service === product.service);
      if (r) rows.push({ href: r.path + scopeSearch(), label: r.title, kind: "Product" });
    }
    // Pages across products (#1842): Google's search lists products and
    // their pages ("Log router", "Key rings") beside resources. Matched here
    // from the routes this instance serves, by page and product name.
    for (const page of matchingPages(q)) {
      if (rows.some((row) => row.href.split("?")[0] === page.path)) continue;
      rows.push({ href: page.path + scopeSearch(), label: page.label, kind: page.kind });
    }
    for (const hit of (data.hits || []).slice(0, SUGGEST_LIMIT)) {
      const r = ROUTES.find((x) => x.service === hit.service);
      if (!r) continue;
      rows.push({
        href: capabilityOf(hit.service).detail ? detailHref(r, hit.name) : r.path + scopeSearch(),
        label: hit.name,
        kind: hit.matchedColumn ? `${hit.title} · ${hit.matchedColumn}` : hit.title,
      });
    }
    if (!rows.length) return hide();

    // Everything the results page would list, products included. Counting only
    // resources and logs put "4 matches" under five visible rows.
    const total = (data.hits || []).length + (data.logs || []).length +
                  (data.products || []).length;
    setChildren(panel,
      ...rows.slice(0, SUGGEST_LIMIT).map((row) =>
        el("a", { href: row.href, class: "suggest-row", role: "option",
                  "aria-selected": "false" },
          el("span", { class: "suggest-label", text: row.label }),
          el("span", { class: "suggest-kind", text: row.kind }))),
      // Always reachable: the panel is a shortcut, never the whole answer, and a
      // reader who wants the full set must be able to get to it.
      el("a", { href: `/search?q=${encodeURIComponent(q)}${project ? `&project=${encodeURIComponent(project)}` : ""}`,
                class: "suggest-row suggest-all", role: "option", "aria-selected": "false" },
        el("span", { class: "suggest-label", text: `All results for “${q}”` }),
        el("span", { class: "suggest-kind", text: `${total} match${total === 1 ? "" : "es"}` })));
    panel.hidden = false;
    search.setAttribute("aria-expanded", "true");
    active = -1;
  };

  search.addEventListener("input", () => {
    if (timer) clearTimeout(timer);
    timer = setTimeout(suggest, SUGGEST_DEBOUNCE_MS);
  });
  // Clicks inside the panel are navigations, so the panel must survive losing
  // focus long enough for the link to be followed.
  search.addEventListener("blur", () => setTimeout(hide, 150));
  panel.addEventListener("click", hide);

  search.addEventListener("keydown", (e) => {
    if (e.key === "Escape" && !panel.hidden) {
      e.preventDefault();
      e.stopPropagation();
      return hide();
    }
    if (e.key === "ArrowDown" || e.key === "ArrowUp") {
      if (panel.hidden) return;
      e.preventDefault();
      return highlight(active + (e.key === "ArrowDown" ? 1 : -1));
    }
    if (e.key !== "Enter") return;
    // A highlighted suggestion wins over the query: the user chose it.
    if (!panel.hidden && active >= 0) {
      e.preventDefault();
      options()[active].click();
      return;
    }
    const q = search.value.trim();
    if (!q) return;
    hide();
    const url = new URL("/search", location.origin);
    url.searchParams.set("q", q);
    const project = new URLSearchParams(location.search).get("project");
    if (project) url.searchParams.set("project", project);
    history.pushState({}, "", url);
    route();
  });
}

async function renderSearch(view) {
  const params = new URLSearchParams(location.search);
  const q = params.get("q") || "";
  const project = params.get("project") || "";

  const header = el("div", { class: "page-header" },
    el("h1", { text: "Search results" }),
    el("p", { class: "subtitle", text: q ? `for “${q}”` : "Type a query in the toolbar." }));

  document.getElementById("search").value = q;
  if (!q) return setChildren(view, header);

  setChildren(view, header, loadingState(3));

  let data;
  try {
    data = await api(`/api/search?q=${encodeURIComponent(q)}&project=${encodeURIComponent(project)}`);
  } catch (err) {
    return setChildren(view, header,
      errorState("Search failed", String(err.message), () => renderSearch(view)));
  }

  // A hit links to the thing it found, not to the screen that lists it.
  //
  // Finding a bucket among two hundred and being dropped on the bucket list is
  // asking the user to search twice. Where the provider can open a row, the
  // result goes straight to it.
  const linkFor = (hit) => {
    const r = ROUTES.find((x) => x.service === hit.service);
    if (!r) return "/";
    const caps = capabilityOf(hit.service);
    return caps.detail ? detailHref(r, hit.name) : r.path + scopeSearch();
  };

  // Grouped by service, because "where is it" is half of what a search
  // across services is being asked.
  const groups = new Map();
  for (const hit of data.hits || []) {
    if (!groups.has(hit.title)) groups.set(hit.title, []);
    groups.get(hit.title).push(hit);
  }

  const blocks = [...groups.entries()].map(([title, hits]) =>
    el("div", { class: "card" },
      el("h2", { text: `${title} (${hits.length})` }),
      el("ul", { class: "search-hits" },
        ...hits.map((h) =>
          el("li", {},
            el("a", { href: linkFor(h), class: "search-hit" },
              el("span", { class: "search-hit-name", text: h.name }),
              // Why this row matched, when it was not the name. Without it a
              // search for an image tag returned a list of pod names with no
              // indication of why any of them was there.
              h.matchedColumn
                ? el("span", { class: "search-hit-why",
                               text: `${h.matchedColumn}: ${h.matchedValue}` })
                : null,
              h.detail ? el("span", { class: "search-hit-detail", text: h.detail }) : null,
              h.status
                ? el("span", { class: "status", "data-state": stateOf(h.status) },
                    el("span", { text: h.status }))
                : null))))));

  // Matching log entries, from the server. An error message is the thing people
  // most often paste into a search box, and a search that covered resources and
  // not logs was missing everything the instance had said.
  const logHits = data.logs || [];
  const logBlock = logHits.length
    ? el("div", { class: "card" },
        el("h2", { text: `Log entries (${logHits.length}${data.logsTruncated ? "+" : ""})` }),
        el("ul", { class: "search-hits" },
          ...logHits.slice().reverse().map((e) =>
            el("li", {},
              el("a", {
                class: "search-hit",
                href: `/logs?contains=${encodeURIComponent(q)}` +
                      (project ? `&project=${encodeURIComponent(project)}` : ""),
              },
                el("span", { class: "search-hit-name mono",
                             text: new Date(e.timestamp).toLocaleTimeString() }),
                el("span", { class: "search-hit-why", text: e.source || "—" }),
                el("span", { class: "search-hit-detail", text: e.message }),
                el("span", { class: "status", "data-state": stateOf(e.severity) },
                  el("span", { text: e.severity })))))))
    : null;

  // Products and pages match too. A console's search box is the fastest way to
  // reach a screen, and one that only looked at resource names could not
  // answer "where is Cloud Tasks" — the question a newcomer asks first.
  //
  // The client matches route titles and sections, which it knows and the server
  // does not; the server matches provider titles, which it knows and the client
  // only knows through the capability list. Merged by path so a screen that both
  // agree on appears once.
  const needle = q.toLowerCase();
  const screens = ROUTES.filter((r) =>
    r.title.toLowerCase().includes(needle) ||
    (r.section || "").toLowerCase().includes(needle) ||
    (r.productTitle || "").toLowerCase().includes(needle));
  for (const product of data.products || []) {
    const route = ROUTES.find((x) => x.service === product.service);
    if (route && !screens.includes(route)) screens.push(route);
  }

  const failed = Object.entries(data.failed || {});
  const screenBlock = screens.length
    ? el("div", { class: "card" },
        el("h2", { text: `Products and pages (${screens.length})` }),
        el("ul", { class: "search-hits" },
          ...screens.map((r) =>
            el("li", {},
              el("a", { href: r.path + scopeSearch(), class: "search-hit" },
                el("span", { class: "search-hit-name", text: r.title }),
                r.section
                  ? el("span", { class: "search-hit-detail", text: r.section })
                  : null)))))
    : null;

  // Narrowing the results page itself. Twenty hits across six services is a page
  // someone has to read, and the query that produced them is the wrong control
  // for it: re-searching throws the whole set away to ask a smaller question.
  const narrow = el("input", { class: "filter", type: "search", id: "narrow-results",
    placeholder: "Narrow these results", "aria-label": "Narrow these results" });
  const narrowIn = (text, term) => String(text || "").toLowerCase().includes(term);
  const applyNarrow = () => {
    const term = narrow.value.trim().toLowerCase();
    for (const li of view.querySelectorAll(".search-hits > li")) {
      li.hidden = Boolean(term) && !narrowIn(li.textContent, term);
    }
    // A group whose every row is hidden is hidden too, so the page does not fill
    // with headings over nothing.
    for (const card of view.querySelectorAll(".cards > .card")) {
      const rows = [...card.querySelectorAll(".search-hits > li")];
      card.hidden = rows.length > 0 && rows.every((li) => li.hidden);
    }
  };
  narrow.addEventListener("input", applyNarrow);

  setChildren(view, header,
    el("p", { class: "subtitle",
              text: `${(data.hits || []).length} resource(s) across ${data.searched} service(s)` +
                    (screens.length ? `, ${screens.length} product(s) or page(s)` : "") +
                    (logHits.length ? `, ${logHits.length} log entr${logHits.length === 1 ? "y" : "ies"}` : "") +
                    (data.truncated ? " — more exist than are shown" : "") }),
    el("div", { class: "actions" }, narrow),
    // What was and was not covered, in the server's own words. The screen used to
    // report "searched N services" and leave the reader to conclude that was
    // everything.
    data.scope ? el("p", { class: "unavailable", text: data.scope }) : null,
    // A service that could not be searched is named. Otherwise "no results"
    // would be indistinguishable from "could not look".
    failed.length
      ? el("div", { class: "card" },
          el("h2", { text: "Not searched" }),
          el("ul", { class: "search-hits" },
            ...failed.map(([name, why]) =>
              el("li", {}, el("span", { class: "unavailable", text: `${name}: ${why}` })))))
      : null,
    blocks.length || screenBlock || logBlock
      ? el("div", { class: "cards" }, screenBlock, ...blocks, logBlock)
      : el("div", { class: "state" },
          el("h2", { text: "No matches" }),
          el("p", { text: `Nothing matching “${q}” in the services that answered.` })));

  announce(`${(data.hits || []).length} search results`);
}

// SHORTCUTS are the keys the console binds, and the list its help overlay shows.
//
// One table, so the overlay cannot promise a key nothing implements and a key
// cannot exist without being documented. That was the previous state in both
// directions: "/" worked and was written down nowhere, and there was no way for a
// reader to discover any of it.
const SHORTCUTS = [
  { keys: "/", what: "Focus the search box" },
  { keys: "?", what: "Show this list" },
  { keys: "g then d", what: "Go to the Welcome page", go: "/" },
  { keys: "g then l", what: "Go to the Logs Explorer", go: "/logs" },
  { keys: "g then a", what: "Go to Activity", go: "/activity" },
  { keys: "g then m", what: "Go to Monitoring", go: "/monitoring" },
  { keys: "g then p", what: "Go to Resource Manager", go: "/projects" },
  { keys: "Enter", what: "Inspect the focused table row" },
  { keys: "⌘/Ctrl + Enter", what: "Run the statement in a query editor" },
  { keys: "Escape", what: "Close a dialog, menu or panel" },
];

// GO_PREFIX_MS is how long a "g" stays armed.
//
// Long enough for a deliberate two-key sequence and short enough that a "g"
// typed for some other reason does not lie in wait to swallow the next letter.
const GO_PREFIX_MS = 1200;

function initShortcuts() {
  let armed = null;

  // A key pressed while typing belongs to what is being typed. Without this the
  // shortcuts would fire inside every text field in the console.
  const typing = (target) =>
    target instanceof Element &&
    (target.matches("input, textarea, select") || target.isContentEditable);

  document.addEventListener("keydown", (e) => {
    if (e.metaKey || e.ctrlKey || e.altKey) return;
    if (typing(e.target)) return;

    if (armed) {
      clearTimeout(armed);
      armed = null;
      const match = SHORTCUTS.find((sc) => sc.go && sc.keys === `g then ${e.key.toLowerCase()}`);
      if (match) {
        e.preventDefault();
        // The project travels with the navigation, or a shortcut would silently
        // drop the scope the user selected.
        navigate(match.go + scopeSearch());
        return;
      }
      // An unrecognised second key is not an error: the "g" is simply spent, and
      // this key is treated as if it had been pressed on its own.
    }

    if (e.key === "g") {
      e.preventDefault();
      armed = setTimeout(() => { armed = null; }, GO_PREFIX_MS);
      announce("Go to… press d, l, a, m or p");
      return;
    }
    // Both "?" and Shift+/ on layouts where they differ.
    if (e.key === "?") {
      e.preventDefault();
      openShortcutHelp();
      return;
    }
    // "/" focuses search, as it does in most consoles. Bound here rather than
    // beside the search box so that every shortcut the overlay lists is
    // implemented in the one place the overlay reads from.
    if (e.key === "/") {
      const search = document.getElementById("search");
      if (!search) return;
      e.preventDefault();
      search.focus();
      search.select();
    }
  });
}

// openShortcutHelp lists the bindings, from the same table that implements them.
function openShortcutHelp() {
  if (document.getElementById("shortcut-help")) return;
  const { dialog, close } = openModal({ labelledBy: "shortcut-title" });
  dialog.append(el("div", { class: "modal-body", id: "shortcut-help" },
    el("h2", { id: "shortcut-title", text: "Keyboard shortcuts" }),
    el("dl", { class: "shortcut-list" },
      ...SHORTCUTS.flatMap((sc) => [
        el("dt", {}, el("kbd", { text: sc.keys })),
        el("dd", { text: sc.what }),
      ])),
    el("div", { class: "modal-actions" },
      el("button", { type: "button", class: "primary", text: "Close",
                     onclick: () => close() }))));
  dialog.querySelector(".modal-actions button").focus();
}

// drawIdentity fills the account menu from what the instance actually issued.
//
// The account name was written into the HTML, so on any instance whose project
// was not the default the console displayed an identity no client would ever
// present — and a developer debugging an auth problem was reading a constant.
function drawIdentity(identity) {
  const account = document.getElementById("account-sa");
  const project = document.getElementById("account-project");
  const path = document.getElementById("account-credentials");
  if (!account) return;
  if (!identity) {
    // Said rather than left as the loading text: an instance that reports no
    // identity is a fact about the instance, not a slow read.
    account.textContent = "This instance reports no generated identity.";
    return;
  }
  account.textContent = identity.serviceAccount || "no service account";
  if (project) {
    project.textContent = identity.project ? `Project ${identity.project}` : "";
  }
  if (path) {
    // The path, not the key. A developer pointing tooling at the fixture needs
    // to know where it is; nothing here reads what is in it.
    path.textContent = identity.credentialsPath || "";
  }
  // The avatar initials follow the account, so the control and the panel agree.
  const avatar = document.querySelector("#account .avatar");
  if (avatar && identity.project) {
    avatar.textContent = identity.project.slice(0, 2).toUpperCase();
  }
}

async function main() {
  installFailureSurfaces();
  installVisibilityPause();
  // A resize changes the header's height — a wrapped title, a tab strip that
  // gains a row — so the offsets are remeasured rather than fixed at render.
  window.addEventListener("resize", syncStickyOffsets);
  initTheme();
  initPanel("settings", "settings-panel");
  // Help (#1842): the keyboard shortcuts, Connect and All products. The
  // shortcut list opens from it, from Settings at a width with no Help, and
  // from "?".
  initPanel("help", "help-panel");
  document.getElementById("help-panel").addEventListener("click", (e) => {
    if (e.target.closest("a")) {
      hideOverlay(document.getElementById("help-panel"));
      document.getElementById("help").setAttribute("aria-expanded", "false");
    }
  });
  for (const id of ["help-shortcuts", "settings-shortcuts"]) {
    document.getElementById(id).addEventListener("click", () => {
      document.dispatchEvent(new KeyboardEvent("keydown", { key: "Escape", bubbles: true }));
      openShortcutHelp();
    });
  }
  initUploadSetting();
  initAbout();
  initPanel("account", "account-panel");
  // Opening the bell is what "seen" means, and it is also when the panel is
  // worth the round trip.
  initPanel("notifications", "notifications-panel", {
    focusPanel: true,
    onOpen: () => { refreshOperations().then(markOperationsSeen); },
  });
  initNavToggle();
  initRouting();
  initSearch();
  initSearchToggle();
  initShortcuts();
  initTerminal();

  // Neither failure is fatal, but neither is discarded: a console that starts
  // with an empty navigation and says nothing about why is indistinguishable
  // from one that has no services.
  try {
    SERVICES = (await api("/api/services")).services || [];
  } catch (err) {
    notify(`The service list could not be read: ${err.message}`, "error");
  }
  try {
    const status = await api("/api/status");
    DEFAULT_PROJECT = status.defaultProject || "";
    drawIdentity(status.identity);
  } catch (err) {
    notify(`The instance status could not be read: ${err.message}`, "error");
  }
  buildNav(SERVICES);

  // The panel is populated before it is ever opened, so a reload does not
  // erase the record of what just happened. What is already there at load is
  // marked seen, or the badge would announce the whole history every time the
  // page is refreshed.
  await refreshOperations();
  markOperationsSeen();
  // The relative times go stale on their own; nothing else would move them.
  setInterval(() => {
    const panel = document.getElementById("notifications-panel");
    if (panel && overlayOpen(panel)) renderOperations();
  }, 30000);

  // The project is resolved before the first screen renders, so a per-project
  // screen is not painted once with no project and again with one.
  await initProjects();
  route();
}

// --- Terminal ------------------------------------------------------------
//
// Cloud Shell's place in the top bar (#781): a drawer at the foot of the page
// holding a shell. The shell is not on this machine. It runs in a pod in the
// instance's cluster, from a pinned Cloud SDK image, with the instance's pod
// environment and gcloud configuration and no credential; the console bridges
// it over a WebSocket on this page's own origin.
//
// The drawer has tabs, as Cloud Shell's does (#834): + opens another shell in
// the same pod, so files and tools are shared between them, and each tab is
// its own session with its own connection, output and idle timeout. A tab is
// renamed by double-clicking it or with F2, closed with its x or Delete, and
// chosen by click, by the arrow keys in the tab strip, or with Alt+PageUp and
// Alt+PageDown from inside the terminal.
//
// Closing the drawer detaches rather than ends the shells. Each tab's session
// id and name are kept for this browser tab (sessionStorage, which another
// browser tab does not see), so reopening, or reloading, returns to every
// shell with its recent output replayed, until the server ends it after a
// quarter of an hour detached, the shell exits, or the pod is recycled.
// Closing a tab ends its shell at once.
//
// When no shell can be had the drawer says why, the way every screen's
// unavailable state does. A blank terminal would look exactly like a shell
// that is merely slow to print its prompt.

const TERMINAL_HEIGHT_KEY = "cloudburrow.terminal.height";
// The drawer's tabs: { tabs: [{ id, name }], active }.
const TERMINAL_TABS_KEY = "cloudburrow.terminal.tabs";
// #781's one session, read once so a shell open across the upgrade is kept.
const TERMINAL_SESSION_KEY = "cloudburrow.terminal.session";
const TERMINAL_MIN_HEIGHT = 120;

const TERMINAL = {
  // Each tab: { key, id, name, term, fit, pane, tab, label, socket,
  //             project, stateText, notice, starting, closing }.
  tabs: [], active: null, next: 1, max: 8, loading: null, state: "closed", ready: false,
  // The once-a-second redraw of the starting panel's elapsed time.
  startTimer: null,
};

function loadTerminalTabs() {
  try {
    const saved = JSON.parse(sessionStorage.getItem(TERMINAL_TABS_KEY) || "null");
    if (saved && Array.isArray(saved.tabs)) {
      const tabs = saved.tabs.filter((t) => t && typeof t === "object")
        .map((t) => ({ id: typeof t.id === "string" ? t.id : "", name: typeof t.name === "string" ? t.name : "" }));
      return { tabs, active: Number.isInteger(saved.active) ? saved.active : 0 };
    }
    const single = sessionStorage.getItem(TERMINAL_SESSION_KEY);
    sessionStorage.removeItem(TERMINAL_SESSION_KEY);
    if (single) return { tabs: [{ id: single, name: "" }], active: 0 };
  } catch { /* private mode: a reload opens new shells */ }
  return { tabs: [], active: 0 };
}

function saveTerminalTabs() {
  try {
    sessionStorage.setItem(TERMINAL_TABS_KEY, JSON.stringify({
      tabs: TERMINAL.tabs.map((t) => ({ id: t.id, name: t.name })),
      active: Math.max(0, TERMINAL.tabs.indexOf(TERMINAL.active)),
    }));
  } catch { /* private mode */ }
}

// The emulator is loaded when the drawer is first opened, not with the page:
// it is most of a megabyte that nobody who never opens a terminal needs.
function loadTerminalEmulator() {
  if (TERMINAL.loading) return TERMINAL.loading;
  const script = (src) => new Promise((resolve, reject) => {
    const s = document.createElement("script");
    s.src = src;
    s.onload = resolve;
    s.onerror = () => reject(new Error(`${src} could not be loaded`));
    document.head.append(s);
  });
  document.head.append(el("link", { rel: "stylesheet", href: "/vendor/xterm/xterm.css" }));
  TERMINAL.loading = script("/vendor/xterm/xterm.js").then(() => script("/vendor/xterm/addon-fit.js"));
  // A failed load is not cached, so Try again retries it.
  TERMINAL.loading.catch(() => { TERMINAL.loading = null; });
  return TERMINAL.loading;
}

function terminalTheme() {
  const css = getComputedStyle(document.documentElement);
  const token = (name) => css.getPropertyValue(name).trim();
  return { background: token("--terminal-bg"), foreground: token("--terminal-fg"),
           cursor: token("--terminal-fg"), selectionBackground: token("--terminal-selection") };
}

// The drawer's notice and state line show the selected tab's; each tab keeps
// its own, so switching tabs shows where that shell stands.
function drawTerminalNotice(n) {
  const notice = document.getElementById("terminal-notice");
  const button = document.getElementById("terminal-retry");
  document.getElementById("terminal-notice-text").textContent = (n && n.text) || "";
  notice.hidden = !(n && n.text);
  button.hidden = !(n && n.retry);
  if (n && n.retry) {
    button.textContent = n.retry.label;
    button.onclick = n.retry.run;
  }
}

function terminalNotice(tab, text, retry) {
  const n = text ? { text, retry } : null;
  if (tab) tab.notice = n;
  if (!tab || tab === TERMINAL.active) drawTerminalNotice(n);
}

// --- The starting panel (#1838) ---
//
// While a tab's pod is made ready the drawer shows one panel, as Cloud Shell
// shows "Provisioning your Cloud Shell machine" on its first start: a
// spinner, a progress bar, and one short line. During the image pull the bar
// is the bytes the node reports, and the line says how many of how many,
// the time the pull has taken, and the time left when the rate gives an
// honest estimate. Each status message replaces the panel's contents; none
// is written anywhere else, so nothing is said twice.
//
// tab.starting: { message, step, fetched, total, elapsed, at, since, samples }
// — the latest report, the server's elapsed seconds and when it arrived,
// when this connection began, and the (elapsed, fetched) pairs the rate is
// read from.

// The steps of a first start: these take minutes, the rest seconds.
const TERMINAL_FIRST_START = new Set(["import", "pull", "unpack"]);

// pullBytes is a byte count in the decimal units the node's numbers are
// read in, rounded down, so it never says more has arrived than has.
function pullBytes(b) {
  if (b >= 1e9) return `${(Math.floor(b / 1e8) / 10).toFixed(1)} GB`;
  return `${Math.floor(b / 1e6)} MB`;
}

function elapsedText(seconds) {
  const s = Math.max(0, Math.floor(seconds));
  if (s < 60) return `${s}s`;
  return `${Math.floor(s / 60)}m ${String(s % 60).padStart(2, "0")}s`;
}

// remainingText is the time left at the rate of the last minute of the pull,
// or "" when that rate is not yet a fair guess: under 15 seconds of samples,
// or no bytes in them.
function remainingText(st) {
  if (st.step !== "pull" || !(st.total > 0) || st.samples.length < 2) return "";
  const first = st.samples[0];
  const last = st.samples[st.samples.length - 1];
  const span = last.elapsed - first.elapsed;
  const got = last.fetched - first.fetched;
  if (span < 15 || got <= 0) return "";
  const since = (performance.now() - st.at) / 1000;
  const left = (st.total - last.fetched) / (got / span) - since;
  if (left < 60) return "less than a minute left";
  return `about ${Math.round(left / 60)} min left`;
}

function startingElapsed(st) {
  const now = performance.now();
  return st.elapsed > 0 ? st.elapsed + (now - st.at) / 1000 : (now - st.since) / 1000;
}

function drawTerminalStarting(tab) {
  const panel = document.getElementById("terminal-starting");
  const st = tab && tab.starting;
  panel.hidden = !st;
  if (!st) {
    if (TERMINAL.startTimer) { clearInterval(TERMINAL.startTimer); TERMINAL.startTimer = null; }
    return;
  }
  if (!TERMINAL.startTimer) {
    TERMINAL.startTimer = setInterval(() => {
      if (TERMINAL.state === "closed" || !TERMINAL.active || !TERMINAL.active.starting) {
        drawTerminalStarting(null);
        return;
      }
      drawTerminalStarting(TERMINAL.active);
    }, 1000);
  }
  const bar = document.getElementById("terminal-starting-bar");
  const known = (st.step === "pull" || st.step === "unpack") && st.total > 0;
  const elapsed = `${elapsedText(startingElapsed(st))} elapsed`;
  let line;
  if (st.step === "pull" && st.total > 0) {
    const of = `${pullBytes(st.fetched)} of ${pullBytes(st.total)}`;
    bar.setAttribute("aria-valuetext", of);
    line = [of, elapsed, remainingText(st)];
  } else if (st.step === "unpack") {
    line = ["Unpacking the image", elapsed];
  } else if (st.step === "pull") {
    line = ["Downloading the image, about 1 GB", elapsed];
  } else if (st.step === "import") {
    line = ["Copying the image from the offline cache", elapsed];
  } else {
    line = [st.message || "Connecting…"];
  }
  document.getElementById("terminal-starting-line").textContent = line.filter(Boolean).join(" · ");
  document.getElementById("terminal-starting-note").hidden = !TERMINAL_FIRST_START.has(st.step);
  // Determinate only while bytes are arriving; unpacking has no count.
  const determinate = known && st.step === "pull";
  bar.classList.toggle("is-indeterminate", !determinate);
  if (determinate) {
    const pct = Math.min(100, Math.floor((st.fetched / st.total) * 100));
    bar.setAttribute("aria-valuenow", String(pct));
    bar.firstElementChild.style.width = `${pct}%`;
  } else {
    bar.removeAttribute("aria-valuenow");
    bar.removeAttribute("aria-valuetext");
    bar.firstElementChild.style.width = "";
  }
}

// terminalStarting records a status message for tab, or clears the panel
// when m is null.
function terminalStarting(tab, m) {
  if (!m) {
    tab.starting = null;
  } else {
    const prev = tab.starting;
    const st = { message: m.message || "", step: m.step || "", fetched: m.fetched || 0, total: m.total || 0,
      elapsed: m.elapsed || 0, at: performance.now(), since: prev ? prev.since : performance.now(),
      samples: prev && prev.step === "pull" ? prev.samples : [] };
    if (st.step === "pull" && st.total > 0) {
      st.samples = st.samples.concat({ elapsed: st.elapsed, fetched: st.fetched })
        .filter((x) => st.elapsed - x.elapsed <= 60);
    }
    tab.starting = st;
  }
  if (tab === TERMINAL.active) drawTerminalStarting(tab);
}

function terminalState(tab, text) {
  if (tab) tab.stateText = text;
  if (!tab || tab === TERMINAL.active) document.getElementById("terminal-state").textContent = text;
}

function drawTerminalProject() {
  const p = TERMINAL.active ? TERMINAL.active.project : null;
  document.getElementById("terminal-project").textContent =
    p === null ? "" : `Project: ${p || "none"}`;
}

function terminalSize(tab) {
  if (tab && tab.fit && tab.term) {
    if (tab === TERMINAL.active) {
      try { tab.fit.fit(); } catch { /* not laid out yet */ }
    }
    return { cols: tab.term.cols, rows: tab.term.rows };
  }
  return { cols: 80, rows: 24 };
}

function sendTerminalControl(tab, message) {
  const ws = tab && tab.socket;
  if (ws && ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify(message));
}

// A line of the console's own in the terminal, dimmed so it cannot be taken for
// the shell's output.
function terminalSay(tab, text) {
  if (tab.term) tab.term.write(`\r\n\x1b[2m[cloudburrow] ${text}\x1b[0m\r\n`);
}

function connectTerminal(tab) {
  if (tab.socket || tab.closing) return;
  terminalNotice(tab, "");
  tab.starting = null;
  terminalStarting(tab, { message: "Connecting to the terminal…" });
  terminalState(tab, "Connecting…");
  const { cols, rows } = terminalSize(tab);
  const q = new URLSearchParams({ project: currentProject(), cols, rows });
  if (tab.id) q.set("session", tab.id);
  const scheme = location.protocol === "https:" ? "wss:" : "ws:";
  const ws = new WebSocket(`${scheme}//${location.host}/api/terminal/socket?${q}`);
  ws.binaryType = "arraybuffer";
  tab.socket = ws;
  let ended = false;
  let attached = false;

  ws.onmessage = (event) => {
    if (typeof event.data !== "string") {
      tab.term.write(new Uint8Array(event.data));
      return;
    }
    let m = {};
    try { m = JSON.parse(event.data); } catch { return; }
    if (tab.closing) return;
    switch (m.type) {
      case "status":
        // The message goes to the starting panel only. It used to be written
        // to the header's state line and to the notice both, so the drawer
        // showed every status twice (#1838).
        terminalState(tab, "Starting…");
        terminalStarting(tab, m);
        break;
      case "session":
        attached = true;
        tab.id = m.id;
        saveTerminalTabs();
        tab.project = m.project || "";
        if (tab === TERMINAL.active) drawTerminalProject();
        terminalStarting(tab, null);
        terminalNotice(tab, "");
        terminalState(tab, m.resumed ? "Reattached" : "Connected");
        // A reattach replays the recent output onto a clean screen, so it is
        // not drawn twice.
        if (m.resumed) tab.term.reset();
        sendTerminalControl(tab, { type: "resize", ...terminalSize(tab) });
        if (tab === TERMINAL.active) tab.term.focus();
        if (tab.project !== currentProject()) followProjectIn(tab);
        break;
      case "project":
        tab.project = m.project || "";
        if (tab === TERMINAL.active) drawTerminalProject();
        terminalSay(tab, m.message);
        break;
      case "unavailable":
        ended = true;
        terminalStarting(tab, null);
        terminalState(tab, "Unavailable");
        terminalNotice(tab, `The terminal is unavailable: ${m.message}`,
          { label: "Retry", run: () => connectTerminal(tab) });
        break;
      case "exit":
        ended = true;
        terminalStarting(tab, null);
        tab.id = "";
        saveTerminalTabs();
        terminalState(tab, "Ended");
        terminalNotice(tab, `${m.message}.`, { label: "Start a new session", run: () => {
          tab.term.reset();
          connectTerminal(tab);
        } });
        break;
      case "detached":
        ended = true;
        terminalStarting(tab, null);
        terminalState(tab, "Detached");
        terminalNotice(tab, `${m.message}.`, { label: "Use it here", run: () => connectTerminal(tab) });
        break;
    }
  };
  ws.onclose = () => {
    if (tab.socket === ws) tab.socket = null;
    if (!ended && !tab.closing && TERMINAL.state !== "closed") {
      terminalStarting(tab, null);
      terminalState(tab, "Disconnected");
      // Before a shell was attached the pod was still starting, which the
      // cluster carries on with: an image pull goes on without the drawer
      // (#824), and reconnecting picks up where it is.
      terminalNotice(tab, attached
        ? "The connection to the terminal closed. The shell is kept for a while, so reconnecting returns to it."
        : "The connection to the terminal closed while it was starting. The cluster carries on pulling or starting it, so reconnecting picks up where it is.",
        { label: "Reconnect", run: () => connectTerminal(tab) });
    }
  };
}

// Every tab's shell follows the toolbar's project, and says so in its own
// terminal. A tab that is not connected follows when it next attaches.
function followProjectIn(tab) {
  if (tab.project === null || tab.project === currentProject()) return;
  sendTerminalControl(tab, { type: "project", project: currentProject() });
}

function followProject() {
  TERMINAL.tabs.forEach(followProjectIn);
}

function terminalTabName(tab) {
  return tab.name;
}

// A new tab is named "Terminal n", for the smallest n no open tab uses.
function defaultTerminalTabName() {
  const used = new Set(TERMINAL.tabs.map((t) => t.name));
  let n = 1;
  while (used.has(`Terminal ${n}`)) n++;
  return `Terminal ${n}`;
}

function drawTerminalTabs() {
  const full = TERMINAL.tabs.length >= TERMINAL.max;
  const add = document.getElementById("terminal-new-tab");
  add.setAttribute("aria-disabled", String(full));
  add.title = full
    ? `A console keeps at most ${TERMINAL.max} terminal tabs; close one to open another`
    : "New tab";
  for (const t of TERMINAL.tabs) {
    const on = t === TERMINAL.active;
    t.tab.classList.toggle("is-selected", on);
    t.tab.setAttribute("aria-selected", String(on));
    t.tab.tabIndex = on ? 0 : -1;
    t.pane.hidden = !on;
  }
}

function selectTerminalTab(tab, focus = true) {
  if (!tab) return;
  TERMINAL.active = tab;
  drawTerminalTabs();
  drawTerminalNotice(tab.notice);
  drawTerminalStarting(tab);
  document.getElementById("terminal-state").textContent = tab.stateText || "";
  drawTerminalProject();
  saveTerminalTabs();
  terminalSize(tab);
  if (focus) tab.term.focus();
}

function stepTerminalTab(step) {
  const n = TERMINAL.tabs.length;
  if (n < 2) return;
  const i = TERMINAL.tabs.indexOf(TERMINAL.active);
  const next = TERMINAL.tabs[(i + step + n) % n];
  selectTerminalTab(next);
  announce(`${terminalTabName(next)} selected`);
}

function renameTerminalTab(tab) {
  if (tab.renaming) return;
  tab.renaming = true;
  const input = el("input", { class: "terminal-tab-rename", type: "text", value: terminalTabName(tab),
    "aria-label": "Tab name", maxlength: "40" });
  tab.label.replaceWith(input);
  input.select();
  input.focus();
  let done = false;
  const finish = (keep) => {
    if (done) return;
    done = true;
    tab.renaming = false;
    // An empty name keeps the one the tab had.
    const name = input.value.trim();
    if (keep && name) {
      tab.name = name;
      tab.label.textContent = terminalTabName(tab);
      tab.tab.setAttribute("aria-label", terminalTabName(tab));
      saveTerminalTabs();
    }
    input.replaceWith(tab.label);
    tab.tab.focus();
  };
  input.addEventListener("keydown", (e) => {
    e.stopPropagation();
    if (e.key === "Enter") { e.preventDefault(); finish(true); }
    if (e.key === "Escape") { e.preventDefault(); finish(false); }
  });
  input.addEventListener("click", (e) => e.stopPropagation());
  input.addEventListener("blur", () => finish(true));
}

function createTerminalTab(saved = {}) {
  const key = TERMINAL.next++;
  const tab = { key, id: saved.id || "", name: saved.name || defaultTerminalTabName(), socket: null, project: null,
    stateText: "", notice: null, starting: null, closing: false };
  tab.pane = el("div", { class: "terminal-pane", role: "tabpanel", id: `terminal-pane-${key}`,
    "aria-labelledby": `terminal-tab-${key}`, hidden: true });
  tab.label = el("span", { class: "terminal-tab-label", text: terminalTabName(tab) });
  // The x is for the pointer; from the keyboard a tab is closed with Delete,
  // so the tab strip holds tabs and nothing else.
  const x = el("span", { class: "terminal-tab-close", "aria-hidden": "true", title: "Close tab",
    html: '<svg viewBox="0 0 24 24"><path d="M7 7l10 10M17 7L7 17"/></svg>',
    onclick: (e) => { e.stopPropagation(); closeTerminalTab(tab); } });
  tab.tab = el("div", { class: "terminal-tab", role: "tab", id: `terminal-tab-${key}`,
    "aria-controls": tab.pane.id, "aria-selected": "false", "aria-label": terminalTabName(tab),
    "aria-keyshortcuts": "Delete F2", tabindex: "-1",
    title: "Double-click or F2 to rename; Delete to close",
    onclick: () => selectTerminalTab(tab),
    ondblclick: () => renameTerminalTab(tab) }, tab.label, x);
  document.getElementById("terminal-tabs").append(tab.tab);
  document.getElementById("terminal-screen").append(tab.pane);

  tab.term = new window.Terminal({
    cursorBlink: true, fontFamily: getComputedStyle(document.documentElement).getPropertyValue("--mono"),
    fontSize: parseInt(getComputedStyle(document.documentElement).getPropertyValue("--text-body-size"), 10) || 14,
    scrollback: 5000, theme: terminalTheme(),
  });
  tab.fit = new window.FitAddon.FitAddon();
  tab.term.loadAddon(tab.fit);
  tab.pane.hidden = false;
  tab.term.open(tab.pane);
  tab.pane.hidden = true;
  tab.term.onData((data) => {
    const ws = tab.socket;
    if (ws && ws.readyState === WebSocket.OPEN) ws.send(new TextEncoder().encode(data));
  });
  tab.term.onResize(({ cols, rows }) => sendTerminalControl(tab, { type: "resize", cols, rows }));
  // Alt+PageUp and Alt+PageDown switch tabs from inside the terminal; every
  // other key is the shell's. The switch is made on the key's release, so
  // this terminal sees both halves of the key before focus moves: xterm.js
  // drops the next typed text in a terminal left holding a key down.
  tab.term.attachCustomKeyEventHandler((e) => {
    if (e.altKey && !e.ctrlKey && !e.metaKey && (e.key === "PageUp" || e.key === "PageDown")) {
      e.preventDefault();
      if (e.type === "keyup") stepTerminalTab(e.key === "PageUp" ? -1 : 1);
      return false;
    }
    return true;
  });
  TERMINAL.tabs.push(tab);
  return tab;
}

function newTerminalTab() {
  if (TERMINAL.tabs.length >= TERMINAL.max) {
    const why = `A console keeps at most ${TERMINAL.max} terminal tabs; close one to open another.`;
    notify(why);
    return;
  }
  const tab = createTerminalTab();
  saveTerminalTabs();
  selectTerminalTab(tab);
  connectTerminal(tab);
}

// Closing a tab ends its shell, not the others'. The last tab closed closes
// the drawer, and the next open starts a new shell.
function closeTerminalTab(tab) {
  const i = TERMINAL.tabs.indexOf(tab);
  if (i < 0) return;
  tab.closing = true;
  const ws = tab.socket;
  tab.socket = null;
  if (ws) {
    if (ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify({ type: "close" }));
    ws.close();
  }
  TERMINAL.tabs.splice(i, 1);
  tab.term.dispose();
  tab.tab.remove();
  tab.pane.remove();
  announce(`${terminalTabName(tab)} closed`);
  if (!TERMINAL.tabs.length) {
    TERMINAL.active = null;
    saveTerminalTabs();
    closeTerminal();
    return;
  }
  if (TERMINAL.active === tab) {
    selectTerminalTab(TERMINAL.tabs[Math.min(i, TERMINAL.tabs.length - 1)]);
  } else {
    drawTerminalTabs();
    saveTerminalTabs();
  }
}

async function openTerminal() {
  const drawer = document.getElementById("terminal-drawer");
  const toggle = document.getElementById("terminal-toggle");
  TERMINAL.state = "open";
  drawer.hidden = false;
  drawer.classList.remove("is-minimised");
  document.getElementById("terminal-minimise").setAttribute("aria-pressed", "false");
  document.documentElement.setAttribute("data-terminal", "open");
  toggle.setAttribute("aria-expanded", "true");
  const height = readStored(TERMINAL_HEIGHT_KEY, 0);
  if (height) drawer.style.height = `${height}px`;

  if (!TERMINAL.ready) {
    terminalState(null, "Loading…");
    try {
      const status = await api("/api/terminal");
      if (!status.available) {
        terminalState(null, "Unavailable");
        terminalNotice(null, `The terminal is unavailable: ${status.reason}`,
          { label: "Try again", run: () => openTerminal() });
        return;
      }
      if (status.maxSessions > 0) TERMINAL.max = status.maxSessions;
      await loadTerminalEmulator();
    } catch (err) {
      terminalState(null, "Unavailable");
      terminalNotice(null, `The terminal is unavailable: ${err.message}`,
        { label: "Try again", run: () => openTerminal() });
      return;
    }
    TERMINAL.ready = true;
    new ResizeObserver(() => { if (TERMINAL.state === "open") terminalSize(TERMINAL.active); })
      .observe(document.getElementById("terminal-screen"));
    const saved = loadTerminalTabs();
    for (const s of saved.tabs.slice(0, TERMINAL.max)) createTerminalTab(s);
    TERMINAL.active = TERMINAL.tabs[Math.min(saved.active, TERMINAL.tabs.length - 1)] || null;
  }
  if (!TERMINAL.tabs.length) TERMINAL.active = createTerminalTab();
  saveTerminalTabs();
  selectTerminalTab(TERMINAL.active);
  TERMINAL.tabs.forEach(connectTerminal);
}

function closeTerminal() {
  const drawer = document.getElementById("terminal-drawer");
  TERMINAL.state = "closed";
  drawer.hidden = true;
  document.documentElement.removeAttribute("data-terminal");
  const toggle = document.getElementById("terminal-toggle");
  toggle.setAttribute("aria-expanded", "false");
  // Closing detaches: the server keeps every tab's shell, and reopening
  // reattaches them.
  for (const tab of TERMINAL.tabs) {
    const ws = tab.socket;
    tab.socket = null;
    if (ws) ws.close();
  }
  toggle.focus();
}

function initTerminal() {
  const toggle = document.getElementById("terminal-toggle");
  const drawer = document.getElementById("terminal-drawer");
  const grip = document.getElementById("terminal-grip");
  const minimise = document.getElementById("terminal-minimise");
  if (!toggle || !drawer) return;

  toggle.addEventListener("click", () => {
    if (TERMINAL.state === "open" && !drawer.classList.contains("is-minimised")) closeTerminal();
    else openTerminal();
  });
  document.getElementById("terminal-close").addEventListener("click", closeTerminal);
  document.getElementById("terminal-new-tab").addEventListener("click", () => {
    if (TERMINAL.ready) newTerminalTab();
  });
  minimise.addEventListener("click", () => {
    const min = drawer.classList.toggle("is-minimised");
    minimise.setAttribute("aria-pressed", String(min));
    if (!min && TERMINAL.active) { terminalSize(TERMINAL.active); TERMINAL.active.term.focus(); }
  });

  // The tab strip is a tablist: the arrow keys, Home and End move between
  // tabs, Delete closes one and F2 renames it.
  document.getElementById("terminal-tabs").addEventListener("keydown", (e) => {
    const tab = TERMINAL.tabs.find((t) => t.tab === e.target);
    if (!tab) return;
    const n = TERMINAL.tabs.length;
    const i = TERMINAL.tabs.indexOf(tab);
    let to = null;
    if (e.key === "ArrowRight") to = (i + 1) % n;
    else if (e.key === "ArrowLeft") to = (i - 1 + n) % n;
    else if (e.key === "Home") to = 0;
    else if (e.key === "End") to = n - 1;
    else if (e.key === "Delete") { e.preventDefault(); closeTerminalTab(tab); return; }
    else if (e.key === "F2") { e.preventDefault(); renameTerminalTab(tab); return; }
    else if (e.key === "Enter" || e.key === " ") { e.preventDefault(); selectTerminalTab(tab); return; }
    if (to === null) return;
    e.preventDefault();
    selectTerminalTab(TERMINAL.tabs[to], false);
    TERMINAL.tabs[to].tab.focus();
  });

  // Dragged by the grip, or moved with the arrow keys once it has focus.
  const setHeight = (px) => {
    const max = Math.max(TERMINAL_MIN_HEIGHT, window.innerHeight - 120);
    const h = Math.round(Math.min(max, Math.max(TERMINAL_MIN_HEIGHT, px)));
    drawer.style.height = `${h}px`;
    writeStored(TERMINAL_HEIGHT_KEY, h);
    terminalSize(TERMINAL.active);
  };
  grip.addEventListener("pointerdown", (e) => {
    e.preventDefault();
    grip.setPointerCapture(e.pointerId);
    const move = (ev) => setHeight(window.innerHeight - ev.clientY);
    const up = () => {
      grip.removeEventListener("pointermove", move);
      grip.removeEventListener("pointerup", up);
    };
    grip.addEventListener("pointermove", move);
    grip.addEventListener("pointerup", up);
  });
  grip.addEventListener("keydown", (e) => {
    const step = e.shiftKey ? 80 : 20;
    if (e.key === "ArrowUp") { e.preventDefault(); setHeight(drawer.offsetHeight + step); }
    if (e.key === "ArrowDown") { e.preventDefault(); setHeight(drawer.offsetHeight - step); }
  });

  window.addEventListener("cb-project-selected", followProject);
  window.addEventListener("popstate", followProject);
  // The theme can change under an open terminal.
  new MutationObserver(() => {
    for (const t of TERMINAL.tabs) t.term.options.theme = terminalTheme();
  }).observe(document.documentElement, { attributes: true, attributeFilter: ["data-theme"] });
}

document.addEventListener("DOMContentLoaded", main);

// --- Monitoring -------------------------------------------------------
//
// Charts over the history the instance retained, and nothing else. Every
// series here is a series this cluster demonstrably holds: node CPU, node
// memory and the pod count, read from the kubelet. What is deliberately
// absent is as much the point — per-request latency, cost, quota and SLO data
// do not exist locally, and a chart of them would be invented.

let MONITORING_TIMER = null;
const MONITORING_REDRAW_MS = 5000;

function stopMonitoring() {
  if (MONITORING_TIMER) { clearInterval(MONITORING_TIMER); MONITORING_TIMER = null; }
}

// productActivity charts how much each product is saying, over time.
//
// The console already computes which product every log entry came from — the
// Logs Explorer's source filter is built on it — and charted none of it, so
// "which product is generating all this" had no answer anywhere. One line per
// product, from the same bucketing the Logs Explorer's timeline uses, so the two
// screens cannot disagree about what happened when.
async function productActivity() {
  let data;
  try {
    data = await api("/api/logs?limit=1000");
  } catch {
    // The node charts above already report an unreachable instance; a second
    // error box would be saying it twice.
    return null;
  }
  const buckets = data.histogram || [];
  if (!buckets.length) return null;

  // Every product that said anything in the window, busiest first, so the chart
  // is ordered by what the reader came to find out.
  const totals = new Map();
  for (const b of buckets) {
    for (const [product, n] of Object.entries(b.sources || {})) {
      totals.set(product, (totals.get(product) || 0) + n);
    }
  }
  if (!totals.size) return null;
  const products = [...totals.entries()].sort((a, b) => b[1] - a[1]).slice(0, 6);

  return el("div", { class: "charts" }, ...products.map(([product, total]) =>
    chartCard(product, buckets.map((b) => ({
      at: b.at,
      // A bucket with no entries from this product is a zero, not a gap: the
      // bucket was measured, and the product said nothing in it. That is a
      // reading, and drawing it as a break would hide a product going quiet.
      value: (b.sources || {})[product] || 0,
    })), {
      label: `Log entries from ${product} over the retained window`,
      current: `${total} entr${total === 1 ? "y" : "ies"}`,
      foot: "Counted from the entries this instance still holds; the Logs " +
            "Explorer's timeline uses the same buckets.",
    })));
}

async function renderMonitoring(view) {
  const header = [pageHeader("Monitoring",
    "Charts over the readings this instance has taken since it started.")];
  const cancel = new AbortController();
  setChildren(view, ...header,
    loadingState(3, { what: "metric history", onCancel: () => cancel.abort() }));

  // Remembered per viewer, because a range is a preference and re-selecting it
  // on every visit is the kind of friction that stops a screen being used.
  let rangeSeconds = Number(readStored(RANGE_KEY, String(RANGES[2].seconds)));
  if (!RANGES.some((r) => r.seconds === rangeSeconds)) rangeSeconds = RANGES[2].seconds;

  const draw = async () => {
    let data;
    try {
      data = await api("/api/metrics/series", { signal: cancel.signal });
    } catch (err) {
      return setChildren(view, ...header,
        isCancelled(err)
          ? cancelledState("Monitoring not loaded", () => renderMonitoring(view))
          : errorState("Monitoring unavailable", String(err.message),
                       () => renderMonitoring(view)));
    }

    if (data.unavailable) {
      return setChildren(view, ...header, emptyState("No history", data.unavailable));
    }

    const all = data.samples || [];
    const samples = withinRange(all, rangeSeconds);
    const latest = [...samples].reverse().find((s) => !s.unavailable && (s.nodes || []).length);
    const node = latest ? latest.nodes[0] : null;

    const rangeControl = el("div", { class: "actions", role: "group",
                                     "aria-label": "Time range" },
      ...RANGES.map((r) => el("button", {
        class: r.seconds === rangeSeconds ? "primary" : "secondary",
        "aria-pressed": r.seconds === rangeSeconds ? "true" : "false",
        text: r.label,
        onclick: () => {
          rangeSeconds = r.seconds;
          writeStored(RANGE_KEY, String(r.seconds));
          draw();
        },
      })));

    // What the window actually covers, said rather than implied. An instance
    // up for thirty seconds shows thirty seconds.
    //
    // The count and the span have to describe the same thing. With a range
    // selected the count is of the filtered samples, so pairing it with the
    // series' own start read as "30 readings since 6 minutes ago" while showing
    // the last five — two true numbers making one false sentence.
    const started = data.startedAt ? new Date(data.startedAt) : null;
    const oldest = samples.length ? new Date(samples[0].at) : null;
    const every = `one reading every ${data.intervalSeconds}s`;
    const covered = oldest ? `since ${relativeTime(oldest)}` : "";
    // Said only when the range is hiding something, so the ordinary case stays
    // short.
    const held = rangeSeconds && all.length > samples.length
      ? ` · ${all.length} held, ${started ? `from ${relativeTime(started)}` : ""}`
      : "";
    const foot = `${samples.length} readings, ${every}${covered ? ", " + covered : ""} · ` +
                 `${data.retention || "in memory only"}${held}`;

    const gaps = samples.filter((s) => s.unavailable).length;

    setChildren(view, ...header,
      rangeControl,
      gaps
        ? el("p", { class: "unavailable",
            text: `${gaps} of ${samples.length} readings could not be taken and are drawn as gaps.` })
        : null,
      el("div", { class: "charts" },
        chartCard("Node CPU", seriesPoints(samples, (n) => n.cpuUsedCores), {
          label: "Node CPU cores used over the retained window",
          max: node ? node.cpuCapacityCores : undefined,
          current: node ? `${node.cpuUsedCores.toFixed(3)} of ${node.cpuCapacityCores} vCPU` : "",
          foot: `Kubelet instantaneous usage · ${foot}`,
        }),
        chartCard("Node memory", seriesPoints(samples, (n) => n.memoryUsedBytes), {
          label: "Node memory working set over the retained window",
          max: node ? node.memoryTotalBytes : undefined,
          current: node ? `${formatBytes(node.memoryUsedBytes)} of ${formatBytes(node.memoryTotalBytes)}` : "",
          foot: `Kubelet working set · ${foot}`,
        }),
        chartCard("Pods", seriesPoints(samples, (n) => n.pods), {
          label: "Pods running on the node over the retained window",
          current: node ? `${node.pods} pods` : "",
          foot: `Counted by the kubelet · ${foot}`,
        }),
        // Network and filesystem were being read from the kubelet and charted
        // nowhere. Both counters are cumulative, so both are charted as rates:
        // "12 GB received since boot" is not an answer to "is the network busy".
        chartCard("Network in", ratePoints(samples, (n) => n.networkRxBytes), {
          label: "Bytes per second received by the node",
          current: node ? `${formatBytes(node.networkRxBytes)} total` : "",
          foot: `Rate between readings of the kubelet's counter · ${foot}`,
        }),
        chartCard("Network out", ratePoints(samples, (n) => n.networkTxBytes), {
          label: "Bytes per second sent by the node",
          current: node ? `${formatBytes(node.networkTxBytes)} total` : "",
          foot: `Rate between readings of the kubelet's counter · ${foot}`,
        }),
        chartCard("Node filesystem", seriesPoints(samples, (n) => n.filesystemUsedBytes), {
          label: "Node filesystem bytes used over the retained window",
          max: node ? node.filesystemCapacityBytes : undefined,
          current: node && node.filesystemCapacityBytes
            ? `${formatBytes(node.filesystemUsedBytes)} of ${formatBytes(node.filesystemCapacityBytes)}`
            : "",
          // Not a rate: this one is a level, and a level is what "will the
          // node run out of disk" asks about.
          foot: `Kubelet node filesystem · ${foot}`,
        })),
      await productActivity(),
      await requestCharts(samples),
      // Absence, stated. The alternative is a reader assuming these charts
      // are missing rather than impossible.
      el("div", { class: "card" },
        el("h2", { text: "Not charted here" }),
        el("p", { class: "unavailable", text:
          "Cloud Run workloads' own request count and latency need Knative's " +
          "queue-proxy metrics, which are off in this instance (#181). Cost, quota " +
          "and SLO data do not exist locally at all and are not approximated." })));
  };

  await draw();
  stopMonitoring();
  // The same interval the server samples at: drawing faster than the data
  // changes is motion without information.
  // The server samples on its own clock; redrawing faster than that is
  // motion without information.
  MONITORING_TIMER = setInterval(draw, MONITORING_REDRAW_MS);
}

// --- Logs Explorer ----------------------------------------------------
//
// Live by default, bounded, and pausable. A log view that cannot be paused is
// unusable the moment something interesting scrolls past.

let STREAM = null;
let METRICS_TIMER = null;

// The list poll.
//
// Every screen backed by /api/resources or /api/detail re-reads on a bounded
// interval, through the table's own refresh so sort, filter, page and scroll
// survive it. Without this a list went quietly stale: the console showed a
// world that had stopped existing and said nothing about it.
let LIST_POLL_TIMER = null;
let LIST_POLL_TICK = null;
const LIST_POLL_MS = 15000;

function registerListPoll(tick) {
  stopListPoll();
  LIST_POLL_TICK = tick;
  if (!document.hidden) LIST_POLL_TIMER = setInterval(tick, LIST_POLL_MS);
}

function stopListPoll() {
  if (LIST_POLL_TIMER) { clearInterval(LIST_POLL_TIMER); LIST_POLL_TIMER = null; }
  LIST_POLL_TICK = null;
}

// METRICS_TICK is the current dashboard's poll, or null when no dashboard is
// on screen. Held so the visibility listener below can resume the one that
// belongs to the screen the user is actually looking at.
let METRICS_TICK = null;

function stopMetrics() {
  if (METRICS_TIMER) { clearInterval(METRICS_TIMER); METRICS_TIMER = null; }
}

// A hidden tab is polling a cluster nobody is looking at.
//
// Suspended rather than slowed, and refreshed on return, so the first thing
// the user sees on coming back is current rather than however old the tab is.
function installVisibilityPause() {
  document.addEventListener("visibilitychange", () => {
    if (document.hidden) {
      stopMetrics();
      if (LIST_POLL_TIMER) { clearInterval(LIST_POLL_TIMER); LIST_POLL_TIMER = null; }
      return;
    }
    // Back on screen: one immediate tick each, so the first thing the reader
    // sees is current rather than however old the tab is.
    if (METRICS_TICK && !METRICS_TIMER) {
      METRICS_TICK();
      METRICS_TIMER = setInterval(METRICS_TICK, 5000);
    }
    if (LIST_POLL_TICK && !LIST_POLL_TIMER) {
      LIST_POLL_TICK();
      LIST_POLL_TIMER = setInterval(LIST_POLL_TICK, LIST_POLL_MS);
    }
  });
}

// Activity refreshes itself while anything on it is still running.
//
// It used to fetch once, so an operation that was running when the screen
// opened stayed RUNNING on it forever — the screen most likely to be watched
// during a slow deploy was the one that never updated. Cleared on route
// change with the same discipline as the metrics timer.
let ACTIVITY_TIMER = null;
const ACTIVITY_POLL_MS = 3000;

function stopActivityPolling() {
  if (ACTIVITY_TIMER) { clearTimeout(ACTIVITY_TIMER); ACTIVITY_TIMER = null; }
}

function stopStream() {
  if (STREAM) { STREAM.close(); STREAM = null; }
}

const MAX_RENDERED_LINES = 1000;

// A stream that has said nothing at all for this long is treated as stalled.
//
// The server sends a keepalive every 20 seconds, so two missed heartbeats is
// the threshold. Before this the client could not tell a healthy idle stream
// from one whose socket was open and dead — and on the Logs screen those two
// states say opposite things about whether the application is running.
const STREAM_SILENCE_MS = 45000;

// The Query results columns (#1863), as Google's Logs Explorer draws each
// entry: severity, timestamp and summary, the entry expanding to its fields.
// The Resource column is what makes an unattributed entry legible: a line
// with no project still says which pod or service produced it.
const LOG_COLUMNS = ["Severity", "Timestamp", "Resource", "Summary"];

// The time-range picker's presets, as Google's Logs Explorer offers them
// ("Last 1 hour" first shown), in minutes.
const LOG_TIME_RANGES = [["5m", "Last 5 minutes", 5], ["15m", "Last 15 minutes", 15], ["30m", "Last 30 minutes", 30],
  ["1h", "Last 1 hour", 60], ["3h", "Last 3 hours", 180], ["6h", "Last 6 hours", 360], ["12h", "Last 12 hours", 720],
  ["1d", "Last 1 day", 1440], ["2d", "Last 2 days", 2880], ["7d", "Last 7 days", 10080], ["14d", "Last 14 days", 20160],
  ["30d", "Last 30 days", 43200]];

// The Severity menu's levels, the Logging query language's.
const LOG_SEVERITIES = ["DEFAULT", "DEBUG", "INFO", "NOTICE", "WARNING", "ERROR", "CRITICAL", "ALERT", "EMERGENCY"];

// logQueryValue quotes a value for the Logging query language.
const logQueryValue = (v) => JSON.stringify(String(v));

// logJSONTree draws an entry's LogEntry as Google's expanded entry does:
// nested fields that fold, each value with Show matching entries and Hide
// matching entries.
function logJSONTree(value, path, onMatch) {
  if (value && typeof value === "object") {
    const keys = Object.keys(value).sort();
    return el("ul", { class: "log-json" }, ...keys.map((k) => {
      const child = value[k];
      const p = path ? `${path}.${k}` : k;
      if (child && typeof child === "object") {
        return el("li", {}, el("details", { class: "log-json-node", open: true },
          el("summary", {}, el("span", { class: "log-json-key", text: k }), el("span", { text: ": {…}" })),
          logJSONTree(child, p, onMatch)));
      }
      return el("li", { class: "log-json-leaf", "data-field": p },
        el("span", { class: "log-json-key", text: k }), el("span", { text: ": " }),
        el("span", { class: "log-json-value mono", text: String(child) }),
        overflowMenu([
          { label: "Show matching entries", run: () => onMatch(`${p}=${logQueryValue(child)}`) },
          { label: "Hide matching entries", run: () => onMatch(`-${p}=${logQueryValue(child)}`) },
        ], `${p} ${child}`));
    }));
  }
  return el("span", { class: "mono", text: String(value) });
}

async function renderLogs(view) {
  const params = new URLSearchParams(location.search);
  const project = params.get("project") || "";
  // The toolbar's project was being forwarded as a hard filter, and a pod's
  // log line carries no project — the Pub/Sub emulator serves every project
  // from one container, so there is nothing to attribute it to. The result
  // was a Logs Explorer that showed nothing at all on an instance holding
  // hundreds of lines, which reads as "my application is silent".
  //
  // The scope is now the user's, stated on screen and carried in the URL.
  // All sources is the default, because the alternative is a screen that
  // hides the logs it exists to show.
  // Or a folder or the organization (#1395), as Google's Logs Explorer
  // refines its scope to one: that parent's own Cloud Logging entries.
  // Or through a Cloud Logging log view (#1410): a bucket and one of its
  // views, of a project, folder, organization or billing account, read with
  // ListLogEntries and tailed with TailLogEntries, so the view's filter and
  // the bucket's retention apply.
  let scope = ["project", "parent", "view"].includes(params.get("scope")) ? params.get("scope") : "all";
  // Followed from a failed operation in Activity. The server has always
  // supported the filter; the client simply never read it, so the one path
  // built to explain a failure landed on the unfiltered stream of the whole
  // instance.
  let operation = params.get("operation") || "";

  // Google's Refine scope: Project logs, a log view, or (CloudBurrow's own,
  // for the pod lines no project can be claimed for) every source.
  const scopeSelect = el("select", { id: "log-scope", "aria-label": "Refine scope" },
    el("option", { value: "all", text: "All sources" }),
    el("option", { value: "project", text: project ? `Project logs (${project})` : "Project logs",
                   disabled: project ? null : "disabled" }),
    el("option", { value: "parent", text: "A folder or organization" }),
    el("option", { value: "view", text: "Through a log view" }));
  scopeSelect.value = scope;

  // The view scope's pickers: whose buckets, which bucket, which view.
  const initialView = params.get("view") || "";
  const viewParentInput = el("input", { class: "filter", type: "search", id: "log-view-parent",
    placeholder: "projects/…, folders/…, organizations/… or billingAccounts/…",
    "aria-label": "Parent whose log buckets to read" });
  viewParentInput.value = initialView.includes("/locations/")
    ? initialView.slice(0, initialView.indexOf("/locations/"))
    : (params.get("viewParent") || (project ? `projects/${project}` : ""));
  const bucketSelect = el("select", { id: "log-bucket", "aria-label": "Log bucket" });
  const viewSelect = el("select", { id: "log-view", "aria-label": "Log view" });
  const viewNote = el("p", { class: "unavailable", id: "log-view-note" });
  const viewControls = el("span", { class: "log-view-controls" }, viewParentInput, bucketSelect, viewSelect);
  let viewBuckets = [];
  const showViewControls = () => {
    viewControls.hidden = scope !== "view";
    viewNote.hidden = scope !== "view";
  };
  const drawViewNote = (error) => {
    if (error) return setChildren(viewNote, el("span", { text: error }));
    const b = viewBuckets.find((x) => x.name === bucketSelect.value);
    const v = b && (b.views || []).find((x) => x.name === viewSelect.value);
    if (!b) return setChildren(viewNote, el("span", { text: "No log bucket under this parent." }));
    setChildren(viewNote, el("span", { text:
      `Bucket ${b.name} keeps entries ${b.retentionDays} days; older entries are not returned. ` +
      (v ? (v.filter ? `View filter: ${v.filter}` : "This view has no filter: every entry of the bucket.") : "") }));
  };
  const fillViews = (want) => {
    const b = viewBuckets.find((x) => x.name === bucketSelect.value);
    const views = b ? (b.views || []) : [];
    setChildren(viewSelect, ...views.map((v) =>
      el("option", { value: v.name, text: v.name.slice(v.name.lastIndexOf("/") + 1) })));
    if (want && views.some((v) => v.name === want)) viewSelect.value = want;
    drawViewNote();
  };
  const loadBuckets = async (want) => {
    const parent = viewParentInput.value.trim();
    viewBuckets = [];
    setChildren(bucketSelect);
    setChildren(viewSelect);
    if (!parent) return drawViewNote("Name a parent: projects/…, folders/…, organizations/… or billingAccounts/….");
    try {
      viewBuckets = (await api(`/api/logs/buckets?${new URLSearchParams({ parent })}`)).buckets || [];
    } catch (err) {
      return drawViewNote(err.message);
    }
    setChildren(bucketSelect, ...viewBuckets.map((b) =>
      el("option", { value: b.name, text: `${b.name.split("/")[3]}/${b.name.slice(b.name.lastIndexOf("/") + 1)}` })));
    const wantBucket = want && want.includes("/views/") ? want.slice(0, want.indexOf("/views/")) : "";
    if (wantBucket && viewBuckets.some((b) => b.name === wantBucket)) bucketSelect.value = wantBucket;
    fillViews(want || (bucketSelect.value ? `${bucketSelect.value}/views/_AllLogs` : ""));
  };
  const parentInput = el("input", { class: "filter", type: "search", id: "log-parent",
    placeholder: "folders/123 or organizations/…", "aria-label": "Folder or organization to scope to" });
  parentInput.value = params.get("parent") || "";
  parentInput.hidden = scope !== "parent";

  // The query pane (#1863), as Google's Logs Explorer lays it out: Search
  // all fields, the Resource, Log name and Severity menus that write their
  // clause into the query, the query editor in the Logging query language
  // (internal/console/logquery.go says which fields and operators), and Run
  // query. Source and resource are the address's own filters, from a
  // resource's Logs tab, shown as chips.
  const source = el("input", { type: "hidden", id: "source" });
  const resource = el("input", { type: "hidden", id: "resource" });
  const contains = el("input", { class: "filter", type: "search", id: "contains",
    placeholder: "Search all fields", "aria-label": "Search all fields" });
  const queryEditor = el("textarea", { id: "log-query", class: "mono log-query-editor", rows: "3", spellcheck: "false",
    "aria-label": "Query editor", placeholder: 'severity>=WARNING\nresource.type="run"\ntextPayload:"timeout"' });
  const queryError = el("p", { class: "form-error", id: "log-query-error", role: "alert", hidden: true });
  const severity = el("select", { id: "severity", "aria-label": "Severity" },
    el("option", { value: "", text: "All severities" }),
    ...LOG_SEVERITIES.map((v) => el("option", { value: v, text: v })));
  const resourceType = el("select", { id: "log-resource-type", "aria-label": "Resource" },
    el("option", { value: "", text: "All resources" }));
  const timeRange = el("select", { id: "log-time-range", "aria-label": "Time range" },
    ...LOG_TIME_RANGES.map(([v, label]) => el("option", { value: v, text: label })),
    el("option", { value: "custom", text: "Custom", hidden: true }));
  // One clause per menu, on a line of its own, replaced when the menu
  // changes: the editor stays the one statement of the query.
  const setClause = (field, clause) => {
    const lines = queryEditor.value.split("\n").filter((l) => !new RegExp(`^\\s*${field.replace(".", "\\.")}\\s*(>=|=)`).test(l));
    if (clause) lines.push(clause);
    queryEditor.value = lines.filter((l, i) => l.trim() || i < lines.length - 1).join("\n").trim();
  };
  const addClause = (clause) => {
    queryEditor.value = queryEditor.value.trim() ? `${queryEditor.value.trim()}\n${clause}` : clause;
  };

  // Query autocomplete (#1863): the field paths the entries read so far
  // carry, and, after a path and = or :, the values they hold for it,
  // offered under the editor as the word at the caret is typed.
  const knownFields = new Map(); // path -> Set of values
  const learnFields = (obj, prefix) => {
    if (!obj || typeof obj !== "object" || knownFields.size > 2000) return;
    for (const [k, v] of Object.entries(obj)) {
      const path = prefix ? `${prefix}.${k}` : k;
      if (v && typeof v === "object" && !Array.isArray(v)) { learnFields(v, path); continue; }
      if (Array.isArray(v)) continue;
      if (!knownFields.has(path)) knownFields.set(path, new Set());
      const vals = knownFields.get(path);
      if (vals.size < 50 && v !== "" && v !== null && v !== undefined) vals.add(String(v));
    }
  };
  const suggestions = el("ul", { class: "log-suggestions", id: "log-query-suggestions", role: "listbox",
    "aria-label": "Query suggestions", hidden: true });
  queryEditor.setAttribute("aria-autocomplete", "list");
  queryEditor.setAttribute("aria-controls", "log-query-suggestions");
  const wordAtCaret = () => {
    const upto = queryEditor.value.slice(0, queryEditor.selectionStart);
    const m = /[^\s()]*$/.exec(upto);
    return { word: m ? m[0] : "", start: upto.length - (m ? m[0].length : 0) };
  };
  const insertSuggestion = (text) => {
    const { start } = wordAtCaret();
    const end = queryEditor.selectionStart;
    queryEditor.value = queryEditor.value.slice(0, start) + text + queryEditor.value.slice(end);
    const caret = start + text.length;
    queryEditor.setSelectionRange(caret, caret);
    suggestions.hidden = true;
    queryEditor.focus();
  };
  const suggest = () => {
    const { word } = wordAtCaret();
    let items = [];
    const op = /^([A-Za-z0-9_."\/-]+?)(=|:|!=|>=|<=)(.*)$/.exec(word);
    if (op) {
      const vals = knownFields.get(op[1]);
      const typed = op[3].replace(/^"/, "");
      if (vals) items = [...vals].filter((v) => v.startsWith(typed) && v !== typed).sort().slice(0, 12)
        .map((v) => ({ label: v, text: `${op[1]}${op[2]}${logQueryValue(v)}` }));
    } else if (word.length >= 1) {
      items = [...knownFields.keys()].filter((k) => k.startsWith(word) && k !== word).sort().slice(0, 12)
        .map((k) => ({ label: k, text: k }));
    }
    if (!items.length) { suggestions.hidden = true; return; }
    setChildren(suggestions, ...items.map((it, i) => el("li", { role: "option", id: `log-suggestion-${i}`,
      class: "log-suggestion mono", text: it.label, tabindex: "-1",
      onmousedown: (e) => { e.preventDefault(); insertSuggestion(it.text); },
      onkeydown: (e) => {
        if (e.key === "Enter") { e.preventDefault(); insertSuggestion(it.text); }
        else if (e.key === "ArrowDown") { e.preventDefault(); (e.target.nextElementSibling || e.target).focus(); }
        else if (e.key === "ArrowUp") { e.preventDefault(); (e.target.previousElementSibling || queryEditor).focus(); }
        else if (e.key === "Escape") { e.preventDefault(); suggestions.hidden = true; queryEditor.focus(); }
      } })));
    suggestions.hidden = false;
  };
  queryEditor.addEventListener("input", suggest);
  queryEditor.addEventListener("blur", () => setTimeout(() => {
    if (!suggestions.contains(document.activeElement)) suggestions.hidden = true;
  }, 150));
  queryEditor.addEventListener("keydown", (e) => {
    if (e.key === "ArrowDown" && !suggestions.hidden) { e.preventDefault(); suggestions.firstElementChild.focus(); }
    else if (e.key === "Escape" && !suggestions.hidden) { e.preventDefault(); e.stopPropagation(); suggestions.hidden = true; }
  });

  // Summary fields (#1863), Google's Preferences > Manage summary fields:
  // custom fields drawn as chips at the start of each entry's summary,
  // optionally truncated; and Show summary chips. Per viewer.
  const SUMMARY_KEY = "cloudburrow.logs.summary";
  let summary = { fields: [], truncate: false, chars: 30, fromEnd: false, show: true };
  try { summary = { ...summary, ...JSON.parse(readStored(SUMMARY_KEY, "{}")) }; } catch { /* the defaults */ }
  const saveSummary = () => writeStored(SUMMARY_KEY, JSON.stringify(summary));
  const fieldValue = (entry, path) => path.split(".").map((k) => k.replace(/^"|"$/g, ""))
    .reduce((o, k) => (o && typeof o === "object" ? o[k] : undefined), entry.logEntry || {});
  const summaryChips = (entry) => {
    if (!summary.show) return [];
    return summary.fields.map((f) => {
      let v = fieldValue(entry, f);
      if (v === undefined || v === null || v === "") return null;
      v = typeof v === "object" ? JSON.stringify(v) : String(v);
      if (summary.truncate && v.length > summary.chars) {
        v = summary.fromEnd ? `…${v.slice(v.length - summary.chars)}` : `${v.slice(0, summary.chars)}…`;
      }
      return el("span", { class: "log-summary-chip", title: f, text: v });
    }).filter(Boolean);
  };

  // Highlight in results (#1863): the text is marked wherever an entry's
  // summary holds it.
  let highlight = "";
  const highlighted = (text) => {
    if (!highlight) return [document.createTextNode(text)];
    const out = [];
    const lower = text.toLowerCase(), h = highlight.toLowerCase();
    let i = 0;
    for (let j = lower.indexOf(h); j >= 0; j = lower.indexOf(h, i)) {
      if (j > i) out.push(document.createTextNode(text.slice(i, j)));
      out.push(el("mark", { class: "log-highlight", text: text.slice(j, j + h.length) }));
      i = j + h.length;
    }
    if (i < text.length) out.push(document.createTextNode(text.slice(i)));
    return out;
  };

  // Pinned entries (#1863): Pin on an entry darkens it and puts a pin on
  // the Timeline at its time, whose menu has Scroll to log entry, Zoom to
  // log entry and Unpin; the entry's own pin menu has Same resource.type,
  // Same resource.labels, Same trace and Show all.
  const pinned = new Map();
  const pinStrip = el("div", { class: "log-pins", id: "log-pins", "aria-label": "Pinned log entries" });
  // The Log name picker (#1554), as Google's Logs Explorer lists the scope's
  // log names: Cloud Logging's ListLogs of the project, folder or
  // organization in scope. A log's full name is its entries' resource, so
  // picking one is the resource filter, and the picker follows that field.
  const logName = el("select", { id: "log-name", "aria-label": "Log name", hidden: true },
    el("option", { value: "", text: "All log names" }));
  const logNameParent = () => {
    if (scope === "parent") return parentInput.value.trim();
    if (scope === "view") return viewParentInput.value.trim();
    return project ? `projects/${project}` : "";
  };
  const syncLogName = () => {
    logName.value = [...logName.options].some((o) => o.value === resource.value) ? resource.value : "";
  };
  const loadLogNames = async () => {
    const parent = logNameParent();
    let names = [];
    if (parent) {
      try {
        names = (await api(`/api/logs/names?${new URLSearchParams({ parent })}`)).logNames || [];
      } catch {
        // No Logging API, or a parent it refuses: there is nothing to pick.
        names = null;
      }
    }
    if (parent !== logNameParent()) return; // the scope moved on meanwhile
    logName.hidden = names === null || !parent;
    setChildren(logName, el("option", { value: "", text: names && names.length ? "All log names" : "No logs in scope" }),
      ...(names || []).map((n) => el("option", { value: n,
        text: decodeURIComponent(n.slice(n.indexOf("/logs/") + "/logs/".length)) })));
    logName.setAttribute("data-parent", parent);
    syncLogName();
  };
  logName.addEventListener("change", () => {
    resource.value = logName.value;
    writeFilters();
    drawScope();
    connect();
    announce(logName.value ? `Showing log ${logName.value}` : "Showing every log name");
  });

  // Every filter is read from the URL and written back to it.
  //
  // They lived only in the DOM, so a filtered view could not be reloaded,
  // bookmarked or sent to a colleague — and the one thing a person does with a
  // log query they got right is send it to someone else.
  const FILTER_CONTROLS = [
    { control: source, param: "source" },
    { control: resource, param: "resource" },
    { control: contains, param: "contains" },
    { control: queryEditor, param: "query" },
    { control: timeRange, param: "timeRange" },
  ];
  timeRange.value = "1h";
  for (const { control, param } of FILTER_CONTROLS) {
    const initial = params.get(param);
    if (initial) control.value = initial;
  }
  // A custom range is the timeline's Zoom to time, carried in the address.
  let customFrom = params.get("from") || "", customTo = params.get("to") || "";
  if (customFrom) timeRange.value = "custom";
  // An address from before the query editor (?severity=ERROR) is its clause.
  if (params.get("severity") && !queryEditor.value) {
    setClause("severity", `severity>=${params.get("severity").toUpperCase()}`);
  }
  severity.value = (/^\s*severity\s*>=\s*(\w+)/m.exec(queryEditor.value) || [])[1] || "";

  const writeFilters = () => {
    const url = new URL(location.href);
    for (const { control, param } of FILTER_CONTROLS) {
      if (control.value && !(param === "timeRange" && control.value === "1h")) url.searchParams.set(param, control.value);
      else url.searchParams.delete(param);
    }
    url.searchParams.delete("severity");
    if (timeRange.value === "custom") {
      url.searchParams.set("from", customFrom);
      url.searchParams.set("to", customTo);
      url.searchParams.delete("timeRange");
    } else {
      url.searchParams.delete("from");
      url.searchParams.delete("to");
    }
    history.replaceState({}, "", url);
  };

  // The severity timeline.
  //
  // A list of the most recent lines cannot answer "when did the errors start",
  // which is the first question anyone brings to a log viewer. The counts come
  // from the server over the same filtered set the table is showing, so the
  // histogram and the rows can never disagree about what is being looked at.
  const timeline = el("div", { class: "card log-timeline", id: "log-timeline" });
  let timelineCollapsed = false;
  // The Fields pane (#1863): the values of the entries the query matches,
  // each with its count, as Google's Log fields pane lists them; choosing one
  // adds its clause to the query and runs it.
  const fieldsPane = el("aside", { class: "card log-fields", id: "log-fields", "aria-label": "Log fields" });
  const LOG_FIELD_GROUPS = [["resource.type", "Resource type"], ["severity", "Severity"],
    ["logName", "Log name"], ["resource.labels.project_id", "Project ID"]];
  const drawFields = (entries) => {
    const groups = LOG_FIELD_GROUPS.map(([path, label]) => {
      const counts = new Map();
      for (const e of entries) {
        const v = path.split(".").reduce((o, k) => (o && typeof o === "object" ? o[k] : undefined), e.logEntry || {});
        if (v !== undefined && v !== "") counts.set(v, (counts.get(v) || 0) + 1);
      }
      return { path, label, values: [...counts.entries()].sort((a, b) => b[1] - a[1]).slice(0, 20) };
    });
    const types = groups[0].values.map(([v]) => v);
    const keep = resourceType.value;
    setChildren(resourceType, el("option", { value: "", text: "All resources" }),
      ...[...new Set([...types, ...(keep ? [keep] : [])])].sort().map((v) => el("option", { value: v, text: v })));
    resourceType.value = keep;
    setChildren(fieldsPane,
      el("div", { class: "log-fields-head" }, el("h2", { text: "Log fields" }),
        el("button", { class: "link-button is-inline", text: "Clear", onclick: () => {
          queryEditor.value = ""; severity.value = ""; resourceType.value = ""; runQuery(); } })),
      ...groups.map((g) => el("section", { class: "log-field-group", "data-field": g.path },
        el("h3", { text: g.label }),
        g.values.length
          ? el("ul", {}, ...g.values.map(([v, n]) => el("li", {},
              el("button", { class: "log-field-value", "data-value": v,
                title: `Add ${g.path}=${logQueryValue(v)} to the query`,
                onclick: () => { addClause(`${g.path}=${logQueryValue(v)}`); runQuery(); } },
                el("span", { class: "mono", text: g.path === "logName" ? decodeURIComponent(v.slice(v.indexOf("/logs/") + 6)) : v }),
                el("span", { class: "log-field-count", text: String(n) })))))
          : el("p", { class: "unavailable", text: "No values" }))));
  };

  // The Timeline's drag handles (#1863): the start and end of the range,
  // dragged (or moved with the arrow keys) inward to narrow it or outward
  // to widen it, then Run, as on Google's Timeline.
  const timelineHandles = (buckets, width) => {
    const n = buckets.length;
    if (!n) return null;
    const start = el("input", { type: "range", min: "0", max: String(n), value: "0", step: "1",
      class: "timeline-handle", id: "timeline-start", "aria-label": "Start of the time range" });
    const end = el("input", { type: "range", min: "0", max: String(n), value: String(n), step: "1",
      class: "timeline-handle", id: "timeline-end", "aria-label": "End of the time range" });
    const at = (i) => new Date(new Date(buckets[0].at).getTime() + i * width);
    const label = el("span", { class: "unavailable", id: "timeline-selection" });
    const run = el("button", { type: "button", class: "secondary", id: "timeline-run", text: "Run", disabled: true });
    const shade = el("div", { class: "timeline-selection-shade", "aria-hidden": "true" });
    const update = () => {
      if (+start.value >= +end.value) {
        if (document.activeElement === start) start.value = String(+end.value - 1);
        else end.value = String(+start.value + 1);
      }
      start.setAttribute("aria-valuetext", at(+start.value).toLocaleTimeString());
      end.setAttribute("aria-valuetext", at(+end.value).toLocaleTimeString());
      label.textContent = `${at(+start.value).toLocaleTimeString()} to ${at(+end.value).toLocaleTimeString()}`;
      shade.style.left = `${(+start.value / n) * 100}%`;
      shade.style.width = `${((+end.value - +start.value) / n) * 100}%`;
      run.disabled = +start.value === 0 && +end.value === n;
    };
    start.addEventListener("input", update);
    end.addEventListener("input", update);
    run.addEventListener("click", () => {
      customFrom = at(+start.value).toISOString();
      customTo = at(+end.value).toISOString();
      timeRange.value = "custom";
      runQuery();
      announce(`Time range ${label.textContent}`);
    });
    update();
    return el("div", { class: "timeline-range" },
      el("div", { class: "timeline-handles" }, shade, start, end),
      el("div", { class: "actions" }, label, run));
  };

  const drawPins = () => {
    if (!pinned.size) return setChildren(pinStrip);
    setChildren(pinStrip, el("span", { class: "unavailable", text: "Pinned:" }),
      ...[...pinned.values()].map((entry) => {
        const when = new Date(entry.timestamp);
        const menu = overflowMenu([
          { label: "Scroll to log entry", run: () => {
            const row = body.querySelector(`tr.log-entry[data-insert-id="${CSS.escape(String(entry.id))}"]`);
            if (row) { row.scrollIntoView({ block: "center" }); row.querySelector("button").focus(); }
          } },
          { label: "Zoom to log entry", run: () => {
            customFrom = new Date(when.getTime() - 30000).toISOString();
            customTo = new Date(when.getTime() + 30000).toISOString();
            timeRange.value = "custom";
            runQuery();
          } },
          { label: "Unpin", run: () => togglePin(entry) },
        ], `pinned entry at ${when.toLocaleTimeString()}`);
        return el("span", { class: "log-pin-mark" },
          el("span", { class: "mono", text: when.toLocaleTimeString() }), menu);
      }));
  };
  const togglePin = (entry) => {
    const id = String(entry.id);
    if (pinned.has(id)) pinned.delete(id); else pinned.set(id, entry);
    for (const row of body.querySelectorAll(`tr.log-entry[data-insert-id="${CSS.escape(id)}"]`)) {
      row.classList.toggle("is-pinned", pinned.has(id));
      const b = row.querySelector(".log-pin");
      if (b) b.setAttribute("aria-pressed", pinned.has(id) ? "true" : "false");
    }
    drawPins();
    announce(pinned.has(id) ? "Pinned the log entry" : "Unpinned the log entry");
  };

  const drawTimeline = async () => {
    const query = filterQuery();
    query.set("limit", "1000");
    let data;
    try {
      data = await api(`/api/logs?${query}`);
    } catch {
      // The stream's own status already says whether the instance is
      // reachable; a second red box here would be saying it twice.
      drawFields([]);
      return setChildren(timeline);
    }
    drawFields(data.entries || []);
    const buckets = data.histogram || [];
    const total = buckets.reduce((n, b) => n + (b.total || 0), 0);
    const collapse = el("button", { class: "link-button is-inline", id: "log-timeline-toggle",
      "aria-expanded": timelineCollapsed ? "false" : "true",
      text: timelineCollapsed ? "Expand timeline" : "Collapse timeline",
      onclick: () => { timelineCollapsed = !timelineCollapsed; drawTimeline(); } });
    const head = el("div", { class: "log-timeline-head" }, el("h2", { text: "Timeline" }), collapse,
      timeRange.value === "custom"
        ? el("button", { class: "link-button is-inline", text: "Jump to now", onclick: () => {
            timeRange.value = "1h"; runQuery(); } })
        : null);
    if (!total) return setChildren(timeline, head, el("p", { class: "unavailable", text: "No entries in this time range" }));
    if (timelineCollapsed) return setChildren(timeline, head);
    const width = buckets.length > 1 ? new Date(buckets[1].at) - new Date(buckets[0].at) : 60000;

    const peak = Math.max(...buckets.map((b) => b.total || 0));
    const first = buckets[0] && new Date(buckets[0].at);
    const last = buckets[buckets.length - 1] && new Date(buckets[buckets.length - 1].at);
    const errors = buckets.reduce((n, b) => n + ((b.counts || {}).ERROR || 0), 0);

    setChildren(timeline,
      head,
      el("div", { class: "timeline-bars", role: "img",
        "aria-label": `${total} entries over ${first && last ? relativeTime(first) : "the retained window"}` +
                      `, ${errors} of them errors` },
        ...buckets.map((b) => {
          const counts = b.counts || {};
          // Zoom to time: a column's click narrows the range to it.
          const column = el("div", { class: "timeline-column",
            title: `${new Date(b.at).toLocaleTimeString()} · ${b.total || 0} entries · click to zoom to time`,
            onclick: () => {
              customFrom = new Date(b.at).toISOString();
              customTo = new Date(new Date(b.at).getTime() + Math.max(width, 1000)).toISOString();
              timeRange.value = "custom";
              runQuery();
              announce(`Zoomed to ${new Date(b.at).toLocaleTimeString()}`);
            } });
          // Stacked in severity order with the worst on top, so a column that
          // contains an error is unmistakable however few of them there are.
          for (const [name, state] of [["ERROR", "error"], ["WARNING", "warn"],
                                       ["INFO", "ok"], ["DEFAULT", "muted"]]) {
            const n = counts[name] || 0;
            if (!n) continue;
            column.append(el("div", {
              class: `timeline-bar is-${state}`,
              // Scaled against the busiest bucket, so a quiet window still has
              // a readable shape rather than four invisible pixels.
              style: `height: ${Math.max(2, (n / peak) * 100)}%`,
            }));
          }
          return column;
        })),
      timelineHandles(buckets, width),
      pinStrip,
      el("p", { class: "unavailable",
        text: `${total} entries in the retained window` +
              (first && last ? `, from ${first.toLocaleTimeString()} to ${last.toLocaleTimeString()}` : "") +
              (errors ? ` · ${errors} error${errors === 1 ? "" : "s"}` : "") }));
  };

  // The filters the stream and the histogram both use, built once so the two
  // cannot describe different sets of entries.
  const filterQuery = () => {
    const query = new URLSearchParams();
    if (scope === "project" && project) query.set("project", project);
    if (scope === "parent" && parentInput.value.trim()) query.set("parent", parentInput.value.trim());
    if (scope === "view" && viewSelect.value) query.set("view", viewSelect.value);
    if (operation) query.set("operation", operation);
    if (source.value) query.set("source", source.value);
    if (resource.value) query.set("resource", resource.value);
    // Search all fields is the query's global restriction, a quoted value.
    const text = [queryEditor.value.trim(), contains.value.trim() ? logQueryValue(contains.value.trim()) : ""]
      .filter(Boolean).join("\n");
    if (text) query.set("query", text);
    if (timeRange.value === "custom" && customFrom) {
      query.set("from", customFrom);
      if (customTo) query.set("to", customTo);
    } else {
      const preset = LOG_TIME_RANGES.find(([v]) => v === timeRange.value) || LOG_TIME_RANGES[3];
      query.set("from", new Date(Date.now() - preset[2] * 60000).toISOString());
    }
    return query;
  };

  // Delete log (#799) is offered only when the instance serves the Cloud
  // Logging API, and only on its entries: the server says which, so the page
  // never draws a button the backend would refuse.
  let logDelete = false;
  try {
    logDelete = !!(await api("/api/logs?limit=1")).logDelete;
  } catch { /* the stream's own state reports an unreachable instance */ }
  const columns = (logDelete ? LOG_COLUMNS.length + 1 : LOG_COLUMNS.length) + 1;

  // Stream logs / Stop stream (#1863), as Google's toolbar: the query's
  // results are drawn when it runs, and later entries only while streaming;
  // stopped, they are held and shown when streaming starts again.
  const pauseButton = el("button", { class: "secondary", id: "log-stream", text: "Stream logs" });
  const reconnectButton = el("button", { class: "secondary", text: "Reconnect",
                                         hidden: true, onclick: () => connect() });
  // A .status, like every other state in this console. A muted grey string
  // was the one state indicator on the page with no colour, on the screen
  // people open when something is already wrong.
  const status = el("span", { class: "status", "data-state": "warn" },
    el("span", { text: "connecting…" }));
  const chips = el("div", { class: "filter-chips" });
  const body = el("tbody");
  const table = el("table", {},
    el("thead", {}, el("tr", {},
      el("th", { scope: "col" }, el("span", { class: "sr-only", text: "Expand" })),
      LOG_COLUMNS.map((c) => el("th", { scope: "col", text: c })),
      logDelete ? el("th", { scope: "col" }, el("span", { class: "sr-only", text: "Actions" })) : null)),
    body);

  let paused = true;
  let ready = false;
  let buffered = [];
  let attempt = 0;
  let rows = 0;
  let silenceTimer = null;
  let stalled = false;

  const setStatus = (text, state) => {
    status.setAttribute("data-state", state);
    setChildren(status, el("span", { text }));
  };

  // An empty table is a claim, and which claim depends on why it is empty.
  // "There are no logs" sends a developer to debug their own application;
  // "the stream is down" sends them here. The list screens have said this
  // properly for a while — this screen was the outlier.
  // How many entries the instance holds, and how many of them no project can
  // be claimed for. Fetched only when the table is empty, which is the one
  // moment the numbers explain anything.
  let census = null;

  const drawEmpty = () => {
    if (rows) return;
    const disconnected = stalled || attempt;
    const hiddenByScope = !disconnected && scope === "project" && census &&
      census.unattributed > 0;

    setChildren(body, el("tr", {},
      el("td", { colspan: String(columns) },
        el("div", { class: "state state-inline" },
          el("h2", { text: disconnected
            ? "The log stream is not connected"
            : hiddenByScope
              ? `No entries are attributed to project ${project}`
              : "No entries match these filters" }),
          el("p", { text: disconnected
            ? "Nothing can be shown until the stream is back."
            : hiddenByScope
              ? `${census.unattributed} of the ${census.held} entries this instance holds ` +
                "carry no project — a pod's log line usually cannot be attributed to one."
              : "Nothing has been logged that matches. Widen the filters, or wait." }),
          hiddenByScope
            ? el("button", { class: "primary", text: "Show all sources",
                             onclick: () => setScope("all") })
            : null))));
  };

  // Asked for only when the table is empty: the answer is what turns "nothing
  // here" into which kind of nothing.
  const takeCensus = async () => {
    if (rows || census) return;
    try {
      const data = await api("/api/logs?limit=1");
      census = { held: data.held || 0, unattributed: data.unattributed || 0 };
      drawEmpty();
    } catch { /* the stream's own state already says the instance is unreachable */ }
  };

  const setScope = (next) => {
    scope = next;
    scopeSelect.value = next;
    census = null;
    const url = new URL(location.href);
    if (next === "all") url.searchParams.delete("scope");
    else url.searchParams.set("scope", next);
    if (next === "parent" && parentInput.value.trim()) url.searchParams.set("parent", parentInput.value.trim());
    else url.searchParams.delete("parent");
    if (next === "view" && viewSelect.value) url.searchParams.set("view", viewSelect.value);
    else url.searchParams.delete("view");
    history.replaceState({}, "", url);
    parentInput.hidden = next !== "parent";
    showViewControls();
    loadLogNames();
    connect();
    announce(next === "project" ? `Scoped to project ${project}`
      : next === "parent" ? `Scoped to ${parentInput.value.trim() || "a folder or organization"}` : "Showing all sources");
  };
  scopeSelect.addEventListener("change", async () => {
    if (scopeSelect.value === "view" && !viewBuckets.length) await loadBuckets("");
    setScope(scopeSelect.value);
  });
  parentInput.addEventListener("change", () => setScope("parent"));
  viewParentInput.addEventListener("change", async () => { await loadBuckets(""); setScope("view"); });
  bucketSelect.addEventListener("change", () => { fillViews(`${bucketSelect.value}/views/_AllLogs`); setScope("view"); });
  viewSelect.addEventListener("change", () => { drawViewNote(); setScope("view"); });

  // A `logging/<log>` entry of the toolbar's project is one Cloud Logging
  // holds, and its log can be deleted through DeleteLog. Nothing else can:
  // pod lines and CloudBurrow's own records have no API that removes them.
  const deletableLog = (entry) =>
    (entry.source || "").startsWith("logging/") && project && entry.project === project &&
    (entry.resource || "").startsWith(`projects/${project}/logs/`);

  const logRowActions = (entry) => {
    if (!deletableLog(entry)) return null;
    const name = entry.resource;
    let id = name.slice(`projects/${project}/logs/`.length);
    try { id = decodeURIComponent(id); } catch { /* shown as written */ }
    return overflowMenu([{
      label: "Delete log", destructive: true,
      run: () => confirmDestructive({
        title: `Delete log ${id}?`,
        detail: `Every entry of ${name} is deleted through Cloud Logging's DeleteLog. ` +
                "Entries of other logs, pod logs and CloudBurrow's own records are not touched.",
        confirmWord: id,
        confirmLabel: "Delete log",
        onConfirm: async () => {
          const op = recordOperation(`Delete log ${id}`);
          try {
            const q = new URLSearchParams({ project, log: name });
            const res = await send(`/api/logs?${q}`, "DELETE");
            op.succeeded("", res.operation);
            notify(`Deleted log ${id}`);
            connect();
          } catch (err) {
            op.failed(err.message, err.operation);
            throw err;
          }
        },
      }),
    }], `log ${id}`);
  };

  // Copy link: this entry's own address, its insertId as the query over
  // the minute around it, in the scope it was read in.
  const entryLink = (entry) => {
    const url = new URL(location.href);
    url.searchParams.set("query", `insertId=${logQueryValue(entry.id)}`);
    const at = new Date(entry.timestamp).getTime();
    url.searchParams.set("from", new Date(at - 30000).toISOString());
    url.searchParams.set("to", new Date(at + 30000).toISOString());
    for (const p of ["contains", "timeRange", "severity"]) url.searchParams.delete(p);
    return url.toString();
  };
  const copy = async (value, what) => {
    try { await navigator.clipboard.writeText(value); notify(`Copied ${what}`); } catch (err) {
      notify(`Could not copy: ${err.message}`, "error");
    }
  };
  const onMatch = (clause) => { addClause(clause); runQuery(); };

  const append = (entry) => {
    if (!rows) body.replaceChildren();
    rows++;
    learnFields(entry.logEntry, "");
    const isPinned = pinned.has(String(entry.id));
    const pin = el("button", { class: "icon-button log-pin", "aria-pressed": isPinned ? "true" : "false",
      "aria-label": `Pin entry ${entry.id}`, title: "Pin",
      html: '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M14 3l7 7-3 1-4 4 1 5-2 1-4-5-5 5-1-1 5-5-5-4 1-2 5 1 4-4z"/></svg>',
      onclick: () => togglePin(entry) });
    // The pinned entry's menu: queries for its neighbours.
    const le = entry.logEntry || {};
    const pinMenu = overflowMenu([
      { label: "Same resource.type", run: () => onMatch(`resource.type=${logQueryValue((le.resource || {}).type || "")}`) },
      { label: "Same resource.labels", run: () => onMatch(Object.entries((le.resource || {}).labels || {})
          .map(([k, v]) => `resource.labels.${k}=${logQueryValue(String(v))}`).join("\n") || `resource.type=${logQueryValue((le.resource || {}).type || "")}`) },
      { label: "Same trace", unavailable: le.trace ? "" : "This entry has no trace.",
        run: () => onMatch(`trace=${logQueryValue(le.trace || "")}`) },
      { label: "Show all", run: () => { queryEditor.value = ""; runQuery(); } },
    ], `pinned entry ${entry.id}`);
    pinMenu.classList.add("log-pin-menu");
    const expand = el("button", { class: "icon-button log-expand", "aria-expanded": "false",
      "aria-label": `Expand entry ${entry.id}`,
      html: '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M9 6l6 6-6 6"/></svg>' });
    let detail = null;
    expand.addEventListener("click", () => {
      if (detail) {
        detail.remove();
        detail = null;
        expand.setAttribute("aria-expanded", "false");
        return;
      }
      const tree = logJSONTree(entry.logEntry || {}, "", onMatch);
      const setOpen = (open) => { for (const d of tree.querySelectorAll("details")) d.open = open; };
      detail = el("tr", { class: "log-detail" }, el("td", { colspan: String(columns) },
        el("div", { class: "log-detail-actions" },
          el("button", { class: "link-button is-inline", text: "Expand nested fields", onclick: () => setOpen(true) }),
          el("button", { class: "link-button is-inline", text: "Collapse nested fields", onclick: () => setOpen(false) }),
          el("button", { class: "link-button is-inline", text: "Copy link", onclick: () => copy(entryLink(entry), "link to the entry") }),
          el("button", { class: "link-button is-inline", text: "Copy as JSON",
            onclick: () => copy(JSON.stringify(entry.logEntry || {}, null, 2), "entry as JSON") })),
        tree));
      row.after(detail);
      expand.setAttribute("aria-expanded", "true");
    });
    const row = el("tr", { class: "log-entry" + (isPinned ? " is-pinned" : ""), "data-insert-id": String(entry.id) },
      el("td", { class: "log-entry-controls" }, expand, pin, pinMenu),
      el("td", {}, el("span", { class: "status",
        "data-state": entry.severity === "ERROR" ? "error"
          : entry.severity === "WARNING" ? "warn" : "ok" },
        el("span", { text: entry.severity }))),
      el("td", { class: "mono", text: new Date(entry.timestamp).toLocaleString() }),
      // Project and operation ride along in the title, so an entry that
      // carries them can be traced without a column per field.
      el("td", { class: "mono", text: entry.resource || "—",
                 title: [entry.project ? `project ${entry.project}` : "no project",
                         entry.operationId ? `operation ${entry.operationId}` : null]
                   .filter(Boolean).join(" · ") }),
      el("td", { class: "mono log-summary", "data-message": entry.message || "" },
        ...summaryChips(entry), ...highlighted(entry.message || "")),
      logDelete ? el("td", {}, logRowActions(entry)) : null);
    row.logEntryData = entry;
    body.append(row);
    // Bounded: an unbounded log view eventually becomes the reason the tab
    // stops responding.
    while (body.querySelectorAll("tr.log-entry").length > MAX_RENDERED_LINES) {
      const first = body.querySelector("tr.log-entry");
      if (first.nextElementSibling && first.nextElementSibling.classList.contains("log-detail")) first.nextElementSibling.remove();
      first.remove();
      rows--;
    }
    resultsCount.textContent = `${rows} entr${rows === 1 ? "y" : "ies"}`;
  };

  // Any traffic at all — an entry or a heartbeat — means the stream is alive.
  const heard = () => {
    stalled = false;
    if (silenceTimer) clearTimeout(silenceTimer);
    silenceTimer = setTimeout(() => {
      stalled = true;
      setStatus("stalled — no data for 45s", "error");
      reconnectButton.hidden = false;
      drawEmpty();
    }, STREAM_SILENCE_MS);
  };

  // The address's own filters, each a chip with a remove control.
  const filterChip = (label, clear) => el("span", { class: "chip" },
    el("span", { text: label }),
    el("button", { class: "chip-clear", "aria-label": `Clear the ${label} filter`, onclick: clear,
      html: '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M6 6l12 12M18 6L6 18"/></svg>' }));
  const drawScope = () => {
    setChildren(chips,
      source.value ? filterChip(`source ${source.value}`, () => { source.value = ""; writeFilters(); drawScope(); connect(); }) : null,
      resource.value ? filterChip(`resource ${resource.value}`, () => {
        resource.value = ""; syncLogName(); writeFilters(); drawScope(); connect(); }) : null,
      operation
      ? el("span", { class: "chip" },
          el("span", { text: `operation ${operation}` }),
          el("button", {
            class: "chip-clear", "aria-label": "Clear the operation filter",
            onclick: () => {
              operation = "";
              // The address changes with the filter, so the scope survives a
              // reload and a cleared scope is not re-applied by one.
              const url = new URL(location.href);
              url.searchParams.delete("operation");
              history.replaceState({}, "", url);
              drawScope();
              connect();
            },
            html: '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M6 6l12 12M18 6L6 18"/></svg>',
          }))
      : null);
  };

  pauseButton.addEventListener("click", () => {
    paused = !paused;
    pauseButton.textContent = paused ? "Stream logs" : "Stop stream";
    if (paused) {
      setStatus(`stream stopped — ${buffered.length} new`, "warn");
      announce("Log stream stopped");
    } else {
      // Resume shows what happened while paused rather than skipping it:
      // the lines you paused to read are usually next to the ones you need.
      for (const e of buffered) append(e);
      buffered = [];
      setStatus("streaming", "ok");
      announce("Streaming logs");
    }
  });

  const connect = () => {
    stopStream();
    if (silenceTimer) clearTimeout(silenceTimer);
    body.replaceChildren();
    rows = 0;
    resultsCount.textContent = "";
    ready = false;
    buffered = [];
    queryError.hidden = true;
    stalled = false;
    reconnectButton.hidden = true;
    drawEmpty();

    const query = filterQuery();
    query.set("limit", "200");
    if (scope === "view" && !viewSelect.value) {
      setStatus("choose a log view", "warn");
      return;
    }

    setStatus(attempt ? `reconnecting (attempt ${attempt})` : "connecting…", "warn");
    drawTimeline();

    const stream = new EventSource(`/api/stream?${query}`);
    STREAM = stream;
    stream.addEventListener("open", () => {
      // The backlog arrives immediately after open; anything still empty a
      // moment later is genuinely empty and worth explaining.
      setTimeout(takeCensus, 500);
      attempt = 0;
      setStatus(paused ? "query results" : "streaming", "ok");
      heard();
    });
    // The end of the backlog: what follows is new, drawn while streaming.
    stream.addEventListener("ready", () => { ready = true; });
    stream.addEventListener("keepalive", heard);
    // A read the Logging API refused (a view filter it does not evaluate, a
    // parent that does not exist): its own message, and no reconnect.
    stream.addEventListener("failure", (e) => {
      let msg = "the log view could not be read";
      try { msg = JSON.parse(e.data).error || msg; } catch { /* shown as is */ }
      stream.close();
      setStatus("refused", "error");
      if (msg.startsWith("Invalid query")) {
        queryError.textContent = msg;
        queryError.hidden = false;
      } else {
        drawViewNote(msg);
      }
    });
    stream.addEventListener("log", (e) => {
      heard();
      let entry;
      try { entry = JSON.parse(e.data); } catch { return; }
      if (paused && ready) {
        buffered.push(entry);
        if (buffered.length > MAX_RENDERED_LINES) buffered.shift();
        setStatus(`stream stopped — ${buffered.length} new`, "warn");
        return;
      }
      append(entry);
    });
    stream.addEventListener("error", () => {
      // EventSource reconnects on its own and resumes from Last-Event-ID, so
      // this reports rather than rebuilds — except once the browser has
      // closed the connection for good, which is the one case it will not
      // recover from and the only one worth a button.
      if (stream.readyState === EventSource.CLOSED) {
        setStatus("disconnected", "error");
        reconnectButton.hidden = false;
      } else {
        attempt++;
        setStatus(`reconnecting (attempt ${attempt})`, "warn");
      }
      drawEmpty();
    });
  };

  // Run query: the editor, the menus and the time range, read once.
  const runQuery = () => {
    severity.value = (/^\s*severity\s*>=\s*(\w+)/m.exec(queryEditor.value) || [])[1] || "";
    writeFilters();
    connect();
    announce("Query run");
  };
  const runButton = el("button", { class: "primary", id: "log-run-query", text: "Run query", onclick: runQuery });
  queryEditor.addEventListener("keydown", (e) => {
    if (e.key === "Enter" && (e.ctrlKey || e.metaKey)) { e.preventDefault(); runQuery(); }
  });
  contains.addEventListener("change", runQuery);
  severity.addEventListener("change", () => { setClause("severity", severity.value ? `severity>=${severity.value}` : ""); runQuery(); });
  resourceType.addEventListener("change", () => {
    setClause("resource.type", resourceType.value ? `resource.type=${logQueryValue(resourceType.value)}` : "");
    runQuery();
  });
  timeRange.addEventListener("change", runQuery);
  const resultsCount = el("span", { class: "unavailable", id: "log-results-count" });

  // The query's Actions, as Google's: Create metric and Create sink take the
  // query as their filter, Download the results.
  const queryActions = overflowMenu([
    { label: "Create metric", run: () => navigate(`/logs/metrics/create?${new URLSearchParams({ filter: queryEditor.value.trim() })}`) },
    { label: "Create sink", run: () => navigate(`/logs/sinks/create?${new URLSearchParams({ filter: queryEditor.value.trim() })}`) },
    // Google's Create log alert makes a Cloud Monitoring alerting policy,
    // which this instance does not serve (#1863).
    { label: "Create log alert", run: () => {},
      unavailable: "Not available locally: a log alert is a Cloud Monitoring alerting policy, and this instance does not serve Cloud Monitoring." },
    { label: "Highlight in results", run: () => openHighlight() },
    { label: "Download", run: async () => {
      const query = filterQuery();
      query.set("limit", "1000");
      const data = await api(`/api/logs?${query}`);
      const blob = new Blob([JSON.stringify((data.entries || []).map((e) => e.logEntry), null, 2)], { type: "application/json" });
      const a = el("a", { href: URL.createObjectURL(blob), download: "downloaded-logs.json" });
      document.body.append(a);
      a.click();
      a.remove();
    } },
  ], "the query results");
  queryActions.querySelector(".overflow-trigger").setAttribute("aria-label", "Actions");

  const redrawSummaries = () => {
    for (const row of body.querySelectorAll("tr.log-entry")) {
      const cell = row.querySelector("td.log-summary");
      if (cell && row.logEntryData) setChildren(cell, ...summaryChips(row.logEntryData), ...highlighted(cell.dataset.message));
    }
  };
  const openHighlight = () => {
    const { dialog, close } = openModal({ labelledBy: "log-highlight-title" });
    const input = el("input", { type: "text", id: "log-highlight-text", value: highlight, "aria-label": "Text to highlight" });
    dialog.append(el("form", { class: "modal-body", onsubmit: (e) => {
        e.preventDefault(); highlight = input.value.trim(); redrawSummaries(); close();
        announce(highlight ? `Highlighting ${highlight}` : "Highlight cleared"); } },
      el("h2", { id: "log-highlight-title", text: "Highlight in results" }),
      el("label", { for: "log-highlight-text", text: "Text to highlight" }), input,
      el("div", { class: "modal-actions" },
        el("button", { type: "button", class: "secondary", text: "Cancel", onclick: () => close() }),
        el("button", { type: "submit", class: "primary", text: "Highlight" }))));
    input.focus();
  };
  const openSummaryFields = () => {
    const { dialog, close } = openModal({ labelledBy: "log-summary-title" });
    const fields = [...summary.fields];
    const list = el("div", { class: "filter-chips", id: "log-summary-fields" });
    const drawList = () => setChildren(list, ...fields.map((f) => el("span", { class: "chip" },
      el("span", { class: "mono", text: f }),
      el("button", { type: "button", class: "chip-remove", "aria-label": `Remove ${f}`, text: "×",
        onclick: () => { fields.splice(fields.indexOf(f), 1); drawList(); } }))));
    const options = el("datalist", { id: "log-summary-options" },
      ...[...knownFields.keys()].sort().map((k) => el("option", { value: k })));
    const input = el("input", { type: "text", id: "log-summary-input", list: "log-summary-options",
      class: "mono", placeholder: "jsonPayload.method" });
    const quote = (path) => path.split(".").map((p) => /^[A-Za-z_][A-Za-z0-9_]*$/.test(p) || /^".*"$/.test(p) ? p : `"${p}"`).join(".");
    const add = el("button", { type: "button", class: "secondary", text: "Add", onclick: () => {
      const v = quote(input.value.trim());
      if (v && !fields.includes(v)) fields.push(v);
      input.value = ""; drawList(); input.focus(); } });
    const truncate = el("input", { type: "checkbox", id: "log-summary-truncate" });
    truncate.checked = summary.truncate;
    const chars = el("input", { type: "number", id: "log-summary-chars", min: "1", max: "1000", value: String(summary.chars),
      "aria-label": "Characters" });
    const fromEnd = el("select", { id: "log-summary-from", "aria-label": "Truncate from" },
      el("option", { value: "start", text: "Show the beginning" }), el("option", { value: "end", text: "Show the end" }));
    fromEnd.value = summary.fromEnd ? "end" : "start";
    drawList();
    dialog.append(el("form", { class: "modal-body", onsubmit: (e) => {
        e.preventDefault();
        summary = { ...summary, fields, truncate: truncate.checked, chars: Math.max(1, +chars.value || 30), fromEnd: fromEnd.value === "end" };
        saveSummary(); redrawSummaries(); close(); announce("Summary fields applied"); } },
      el("h2", { id: "log-summary-title", text: "Manage summary fields" }),
      el("h3", { text: "Custom summary fields" }), list,
      el("div", { class: "actions" },
        el("label", { for: "log-summary-input", class: "sr-only", text: "Field name" }), input, options, add),
      el("div", { class: "check-line" }, truncate, el("label", { for: "log-summary-truncate", text: "Truncate custom summary fields" })),
      el("div", { class: "actions" }, chars, fromEnd),
      el("div", { class: "modal-actions" },
        el("button", { type: "button", class: "secondary", text: "Cancel", onclick: () => close() }),
        el("button", { type: "submit", class: "primary", text: "Apply" }))));
    input.focus();
  };
  const preferences = overflowMenu([
    { label: "Manage summary fields", run: () => openSummaryFields() },
    { label: "Show summary chips", run: () => {
      summary.show = !summary.show; saveSummary(); redrawSummaries();
      notify(summary.show ? "Summary chips shown" : "Summary chips hidden"); } },
  ], "the query results' preferences");
  preferences.querySelector(".overflow-trigger").setAttribute("aria-label", "Preferences");

  drawScope();
  const shareLink = el("button", { class: "secondary", text: "Share link",
    onclick: () => copy(location.href, "link to this query") });
  const queryLibrary = el("a", { class: "button secondary", href: "/logs/saved-queries", text: "Query library",
    onclick: (e) => { e.preventDefault(); navigate("/logs/saved-queries"); } });
  setChildren(view,
    pageHeader("Logs Explorer",
      "Live from the local stack. Credentials are redacted before an entry is stored.",
      { actions: [queryLibrary, shareLink] }),
    logDelete
      ? el("p", { class: "unavailable", id: "log-delete-note",
          text: "Delete log is on the Cloud Logging entries (source logging/…) of the selected project. " +
                "Pod logs and CloudBurrow's own request log are not held by the Logging API, so they cannot be deleted here." })
      : null,
    el("div", { class: "actions log-toolbar" }, scopeSelect, parentInput, viewControls, timeRange,
       pauseButton, reconnectButton, status),
    viewNote,
    el("section", { class: "card log-query", "aria-label": "Query" },
      el("div", { class: "actions" }, contains, resourceType, logName, severity, source, resource),
      queryEditor,
      suggestions,
      queryError,
      el("div", { class: "actions" }, runButton)),
    chips,
    timeline,
    el("div", { class: "log-panes" },
      fieldsPane,
      el("section", { class: "log-results", "aria-label": "Query results" },
        el("div", { class: "log-results-head" }, el("h2", { text: "Query results" }), resultsCount, queryActions, preferences),
        el("div", { class: "table-wrap" }, table))));

  showViewControls();
  if (scope === "view") await loadBuckets(initialView);
  loadLogNames();
  connect();
  announce(operation ? `Logs Explorer opened, scoped to operation ${operation}`
                     : "Logs Explorer opened");
}

// --- Logs Dashboard (#1863) ------------------------------------------
//
// Google's Logs Dashboard charts the project's entries by severity and by
// resource type. Here they are the entries the console's log store holds in
// the time range, from the same /api/logs the Logs Explorer reads; each row
// opens the Explorer with its query.
async function renderLogsDashboard(view) {
  const range = el("select", { id: "logs-dashboard-range", "aria-label": "Time range" },
    ...LOG_TIME_RANGES.map(([v, label]) => el("option", { value: v, text: label })));
  range.value = new URLSearchParams(location.search).get("timeRange") || "1h";
  const body = el("div", { class: "logs-dashboard" });
  const explore = (query) => {
    const q = new URLSearchParams({ query });
    if (range.value !== "1h") q.set("timeRange", range.value);
    return `/logs?${q}`;
  };
  const draw = async () => {
    const preset = LOG_TIME_RANGES.find(([v]) => v === range.value) || LOG_TIME_RANGES[3];
    const q = new URLSearchParams({ limit: "1000", from: new Date(Date.now() - preset[2] * 60000).toISOString() });
    let data;
    try {
      data = await api(`/api/logs?${q}`);
    } catch (err) {
      return setChildren(body, el("div", { class: "state" }, el("h2", { text: "The log store could not be read" }),
        el("p", { text: err.message })));
    }
    const entries = data.entries || [];
    const count = (key) => {
      const m = new Map();
      for (const e of entries) {
        const k = key(e);
        m.set(k, (m.get(k) || 0) + 1);
      }
      return [...m.entries()].sort((a, b) => b[1] - a[1]);
    };
    const bySeverity = count((e) => e.severity);
    const byType = count((e) => (e.logEntry || {}).resource ? e.logEntry.resource.type : "unattributed");
    const errorsByType = count((e) => e.severity === "ERROR" ? ((e.logEntry || {}).resource || {}).type || "unattributed" : null)
      .filter(([k]) => k !== null);
    const card = (title, rows, query, empty) => el("section", { class: "card", "aria-label": title },
      el("h2", { text: title }),
      rows.length
        ? el("table", {}, el("tbody", {}, ...rows.map(([k, n]) => el("tr", {},
            el("td", {}, el("a", { href: explore(query(k)), text: k,
              onclick: (e) => { e.preventDefault(); navigate(explore(query(k))); } })),
            el("td", { class: "mono", text: String(n) })))))
        : el("p", { class: "unavailable", text: empty }));
    setChildren(body,
      card("Log entries by severity", bySeverity, (k) => `severity=${k}`, "No entries in this time range"),
      card("Log entries by resource type", byType, (k) => `resource.type=${logQueryValue(k)}`, "No entries in this time range"),
      card("Errors by resource type", errorsByType, (k) => `severity>=ERROR\nresource.type=${logQueryValue(k)}`, "No errors in this time range"));
  };
  range.addEventListener("change", () => {
    const url = new URL(location.href);
    if (range.value === "1h") url.searchParams.delete("timeRange"); else url.searchParams.set("timeRange", range.value);
    history.replaceState({}, "", url);
    draw();
  });
  setChildren(view, pageHeader("Logs Dashboard", "The entries this instance's log store holds, by severity and resource type.",
    { actions: [refreshButton(draw)] }),
    el("div", { class: "actions" }, range), body);
  await draw();
  announce("Logs Dashboard opened");
}

// --- Activity ---------------------------------------------------------

// activityListing shapes operations as a listing, in one place, so the first
// render and every poll after it cannot disagree about the columns.
function activityListing(ops) {
  return {
    nameColumn: "Operation",
    columns: ["Started", "Kind", "Resource", "Detail"],
    noun: "operations",
    alwaysStatus: true,
    total: ops.length,
    items: ops.map((op) => ({
      name: op.id,
      status: op.state,
      // A failed operation's id links to its own logs: "it failed" without
      // the reason is the least useful thing a console can say.
      link: op.state === "FAILED"
        ? `/logs?operation=${encodeURIComponent(op.id)}` : "",
      fields: {
        Started: new Date(op.started).toLocaleTimeString(),
        Kind: op.kind,
        Resource: op.resource || "—",
        Detail: op.error || "—",
      },
    })),
  };
}

async function renderActivity(view) {
  const project = new URLSearchParams(location.search).get("project") || "";
  setChildren(view, 
    pageHeader("Activity", "Operations this console performed."),
    loadingState(4));

  let data;
  try {
    data = await api(`/api/operations?project=${encodeURIComponent(project)}`);
  } catch (err) {
    setChildren(view, pageHeader("Activity", "Operations this console performed."),
      errorState("Activity unavailable", String(err.message), () => renderActivity(view)));
    return;
  }

  const ops = data.operations || [];
  if (!ops.length) {
    setChildren(view, 
      pageHeader("Activity", "Operations this console performed."),
      emptyState("No operations yet",
        "Create or delete something in the console and it will appear here."));
    return;
  }

  const running = ops.filter((op) => op.state !== "SUCCEEDED" && op.state !== "FAILED").length;

  // Activity is the screen someone opens after something failed, so it is
  // where "only the failures" and "sort by time" matter most — and it has the
  // most rows, since every console mutation appends one. It used to hand-build
  // its own table and get none of that, which is exactly the drift the one
  // shared renderer exists to prevent.
  const listing = activityListing(ops);

  const header = [
    pageHeader("Activity", "Operations this console performed."),
    running
      ? el("div", { class: "actions" },
          el("span", { class: "status is-working" },
            spinner(), el("span", { text: `${running} still running` })))
      : null,
  ];
  // A refetch rather than a screen re-render: Activity is the screen most
  // likely to be watched while something is running, and re-rendering it
  // threw away a typed filter, a chosen sort and the caret on every tick.
  // The running count is updated in place on the same pass.
  const runningLabel = el("span", { class: "status is-working" },
    spinner(), el("span", { text: `${running} still running` }));
  if (!running) runningLabel.hidden = true;

  renderTableInto(view, [...header.slice(0, 1), el("div", { class: "actions" }, runningLabel)],
    listing, "operations",
    () => renderActivity(view), { path: "/activity", service: null, title: "Activity" },
    {
      refetch: async () => {
        const fresh = await api(`/api/operations?project=${encodeURIComponent(project)}`);
        const ops = fresh.operations || [];
        const still = ops.filter((op) => op.state !== "SUCCEEDED" && op.state !== "FAILED").length;
        runningLabel.hidden = still === 0;
        setChildren(runningLabel, spinner(),
          el("span", { text: `${still} still running` }));
        return activityListing(ops);
      },
    });
  announce(`${ops.length} operations`);

  // No separate Activity timer any more: the table's own poll re-reads the
  // operations through refetch, which is what keeps a typed filter, a chosen
  // sort and the caret across a tick. A screen-level re-render threw all
  // three away every three seconds, on the screen most likely to be watched
  // while something is running.
  stopActivityPolling();
}

// --- AI Playground ----------------------------------------------------
//
// Real inference through the same HTTP API the official SDK drives. The
// screen never talks to a model directly, so it cannot work while the API is
// broken, and it cannot show a result the API did not produce.
//
// History is held in memory for this page only. It is never written to
// storage and never leaves the browser, which is why there is no setting to
// turn that off: there is nothing to turn off.

// The screen's own name matches its route, its navigation entry and the
// browser tab. "AI Playground" in the heading and "Vertex AI Studio" (now Agent Studio, #1839)
// everywhere else meant the tab disagreed with the page.
const PLAYGROUND_TITLE = "Agent Studio";
const PLAYGROUND_SUBTITLE = "Real inference through the same HTTP API the official SDK drives.";

const PLAYGROUND_HISTORY_LIMIT = 20;
let PLAYGROUND_HISTORY = [];
let PLAYGROUND_ABORT = null;
let PLAYGROUND_STREAM = true;
let PLAYGROUND_RAW = false;

function stopGeneration() {
  if (PLAYGROUND_ABORT) { PLAYGROUND_ABORT.abort(); PLAYGROUND_ABORT = null; }
}

async function renderPlayground(view) {
  stopGeneration();
  setChildren(view, pageHeader(PLAYGROUND_TITLE, PLAYGROUND_SUBTITLE), loadingState(3));

  let status;
  try {
    status = await api("/api/ai/playground");
  } catch (err) {
    return setChildren(view, 
      pageHeader(PLAYGROUND_TITLE, PLAYGROUND_SUBTITLE),
      errorState("Playground unavailable", String(err.message), () => renderPlayground(view)));
  }

  if (!status.configured) {
    return setChildren(view, 
      pageHeader(PLAYGROUND_TITLE, PLAYGROUND_SUBTITLE),
      emptyState("Local AI is not configured", status.note || ""));
  }

  const header = el("div", { class: "card" },
    el("dl", {},
      el("dt", { text: "Model" }), el("dd", { class: "mono", text: status.model }),
      el("dt", { text: "Publisher" }), el("dd", { text: status.publisher || "unknown" }),
      el("dt", { text: "Endpoint" }), el("dd", { class: "mono", text: status.endpoint }),
      el("dt", { text: "Readiness" }),
      el("dd", {}, el("span", { class: "status", "data-state": status.ready ? "ok" : "error" },
        el("span", { text: status.ready ? "Ready" : "Not answering" })))));

  if (status.unavailable) {
    header.append(el("p", { class: "unavailable", text: status.unavailable }));
  }
  if (status.community) {
    // The requirement is explicit: Gemma results must not be labelled as
    // Gemini results. The provenance is stated on the screen that shows the
    // output, not only in the documentation.
    header.append(el("p", { class: "unavailable", text: status.note }));
  }

  const prompt = el("textarea", { id: "pg-prompt", rows: "4",
    placeholder: "Ask the local model something…", "aria-label": "Prompt" });
  const send = el("button", { text: "Run" });
  const cancel = el("button", { class: "secondary", text: "Cancel", disabled: "disabled" });
  // Stream (#1257): on, the answer arrives as streamGenerateContent sends it
  // (StreamGenerateContent over gRPC); off, it is one generateContent answer
  // (GenerateContent). Kept for the page's lifetime, as the history is.
  const stream = el("input", { type: "checkbox", id: "pg-stream" });
  stream.checked = PLAYGROUND_STREAM;
  stream.addEventListener("change", () => { PLAYGROUND_STREAM = stream.checked; });
  const streamLabel = el("label", { for: "pg-stream", title:
    "On: streamGenerateContent, shown as each chunk arrives. Off: generateContent, one answer." },
    stream, " Stream");
  // Raw (#1284, #1285): the same body sent as RawPredict sends it, to
  // :streamRawPredict (StreamRawPredict) or :rawPredict (RawPredict).
  const raw = el("input", { type: "checkbox", id: "pg-raw" });
  raw.checked = PLAYGROUND_RAW;
  raw.addEventListener("change", () => { PLAYGROUND_RAW = raw.checked; });
  const rawLabel = el("label", { for: "pg-raw", title:
    "On: the body is sent as RawPredict sends it, to :streamRawPredict (Stream on) or :rawPredict (Stream off)." },
    raw, " Raw");
  const timing = el("span", { class: "unavailable", text: "" });
  const output = el("pre", { class: "mono pg-output", id: "pg-output", "aria-live": "polite" });
  const historyBody = el("tbody");

  const refused = el("details", { class: "card" },
    el("summary", { text: `Options the endpoint refuses rather than ignores (${status.refused.length})` }),
    el("p", { text:
      "The runtime applies none of these, and a request carrying one is rejected rather than " +
      "answered as though it applied:" }),
    el("p", { class: "mono", text: status.refused.join(", ") }));

  // Generation options (#1797, #1779): sent as generationConfig, only the
  // fields given. An empty field is absent, not zero: temperature 0 must be
  // typed to mean greedy decoding.
  const supported = new Set(status.supported || []);
  const optInput = (id, label, attrs) => {
    const input = el("input", Object.assign({ id: "pg-opt-" + id, "aria-label": label }, attrs));
    return { id, input, field: el("label", { for: "pg-opt-" + id }, label + " ", input) };
  };
  const numOpts = [
    optInput("temperature", "Temperature", { type: "number", step: "0.05", min: "0", max: "2" }),
    optInput("topP", "Top-p", { type: "number", step: "0.05", min: "0", max: "1" }),
    optInput("topK", "Top-k", { type: "number", step: "1", min: "1" }),
    optInput("seed", "Seed", { type: "number", step: "1" }),
    optInput("maxOutputTokens", "Max output tokens", { type: "number", step: "1", min: "1" }),
  ].filter((o) => supported.has(o.id));
  const stops = optInput("stopSequences", "Stop sequences (comma-separated)", { type: "text" });
  const schema = el("textarea", { id: "pg-opt-responseSchema", rows: "2", class: "mono",
    placeholder: '{"type":"BOOLEAN"}', "aria-label": "Response schema" });
  const jsonOut = el("input", { type: "checkbox", id: "pg-opt-json" });
  const options = el("details", { class: "card", id: "pg-options" },
    el("summary", { text: "Generation options" }),
    el("div", { class: "card-actions" }, ...numOpts.map((o) => o.field),
      ...(supported.has("stopSequences") ? [stops.field] : [])),
    ...(supported.has("responseMimeType") ? [el("div", {},
      el("label", { for: "pg-opt-json", title: "responseMimeType application/json; the schema, if given, constrains the answer" },
        jsonOut, " JSON response (application/json)"),
      el("label", { for: "pg-opt-responseSchema" }, " Response schema "), schema)] : []));
  const generationConfig = () => {
    const cfg = {};
    for (const o of numOpts) {
      const v = o.input.value.trim();
      if (v !== "") cfg[o.id] = Number(v);
    }
    const st = stops.input.value.split(",").map((x) => x.trim()).filter((x) => x);
    if (st.length) cfg.stopSequences = st;
    if (jsonOut.checked) {
      cfg.responseMimeType = "application/json";
      const sc = schema.value.trim();
      if (sc) cfg.responseSchema = JSON.parse(sc);
    }
    return cfg;
  };

  const renderHistory = () => {
    historyBody.replaceChildren();
    for (const h of PLAYGROUND_HISTORY) {
      historyBody.append(el("tr", {},
        el("td", { class: "mono", text: h.at }),
        el("td", { text: h.prompt.length > 60 ? h.prompt.slice(0, 60) + "…" : h.prompt }),
        el("td", { text: h.method }),
        el("td", { class: "mono", text: h.ttft }),
        el("td", { class: "mono", text: h.total }),
        el("td", {}, el("span", {
          class: "status",
          "data-state": h.ok ? "ok" : h.cancelled ? "warn" : "error",
        }, el("span", { text: h.ok ? "OK" : h.cancelled ? "Cancelled" : "Failed" })))));
    }
    if (!PLAYGROUND_HISTORY.length) {
      historyBody.append(el("tr", {},
        el("td", { colspan: "6", class: "unavailable", text: "No requests yet this session." })));
    }
  };

  const run = async () => {
    const text = prompt.value.trim();
    if (!text) { announce("A prompt is required"); prompt.focus(); return; }

    stopGeneration();
    output.textContent = "";
    timing.textContent = "running…";
    send.disabled = true;
    cancel.disabled = false;

    const streaming = stream.checked;
    const rawBody = raw.checked;
    const controller = new AbortController();
    PLAYGROUND_ABORT = controller;
    const started = performance.now();
    let firstAt = null;
    let ok = true;
    let cancelled = false;

    try {
      const resp = await fetch("/api/ai/playground", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ prompt: text, stream: streaming, raw: rawBody, generationConfig: generationConfig() }),
        signal: controller.signal,
      });
      if (!resp.ok) {
        const body = await resp.text();
        throw new Error(errorMessageOf(body) || `HTTP ${resp.status}`);
      }
      if (!streaming) {
        // One GenerateContentResponse: its first token is the whole answer.
        const answer = await resp.json();
        firstAt = performance.now();
        output.textContent = (answer.candidates?.[0]?.content?.parts || []).map((p) => p.text || "").join("");
        return;
      }

      const reader = resp.body.getReader();
      const decoder = new TextDecoder();
      let buffer = "";
      for (;;) {
        const { done, value } = await reader.read();
        if (done) break;
        buffer += decoder.decode(value, { stream: true });
        // Server-sent events are separated by a blank line; a partial event
        // is kept in the buffer rather than parsed as truncated JSON.
        let split;
        while ((split = buffer.indexOf("\n\n")) !== -1) {
          const chunk = buffer.slice(0, split);
          buffer = buffer.slice(split + 2);
          for (const line of chunk.split("\n")) {
            if (!line.startsWith("data: ")) continue;
            let event;
            try { event = JSON.parse(line.slice(6)); } catch { continue; }
            if (event.error) { ok = false; throw new Error(event.error.message || "generation failed"); }
            const part = event.candidates?.[0]?.content?.parts?.[0]?.text;
            if (part) {
              if (firstAt === null) firstAt = performance.now();
              output.textContent += part;
            }
          }
        }
      }
    } catch (err) {
      ok = false;
      if (err.name === "AbortError") {
        // Cancelling is not a failure. Recording it as one would teach a
        // user to distrust the history, since every run they stopped would
        // read as something going wrong.
        cancelled = true;
        output.textContent += "\n[cancelled]";
      } else {
        output.textContent += `\n[error] ${err.message}`;
      }
    } finally {
      const total = performance.now() - started;
      const ttft = firstAt === null ? "—" : `${Math.round(firstAt - started)} ms`;
      timing.textContent = `first token ${ttft} · total ${Math.round(total)} ms`;
      send.disabled = false;
      cancel.disabled = true;
      PLAYGROUND_ABORT = null;

      PLAYGROUND_HISTORY.unshift({
        at: new Date().toLocaleTimeString(), prompt: text, method: (rawBody ? "Raw " : "") + (streaming ? "Stream" : "Unary"),
        ttft, total: `${Math.round(total)} ms`, ok, cancelled,
      });
      PLAYGROUND_HISTORY = PLAYGROUND_HISTORY.slice(0, PLAYGROUND_HISTORY_LIMIT);
      renderHistory();
    }
  };

  send.addEventListener("click", run);
  cancel.addEventListener("click", stopGeneration);

  renderHistory();
  const models = await playgroundModelsCard();
  setChildren(view, 
    pageHeader(PLAYGROUND_TITLE, PLAYGROUND_SUBTITLE),
    header,
    ...(models ? [models] : []),
    refused,
    options,
    el("div", { class: "card" },
      prompt,
      el("div", { class: "card-actions" }, send, cancel, streamLabel, rawLabel, timing),
      output),
    el("div", { class: "card" },
      el("h2", { text: "This session" }),
      el("p", { class: "unavailable", text:
        `Bounded to ${PLAYGROUND_HISTORY_LIMIT} entries, held in memory for this page only. ` +
        "Nothing is written to storage and nothing leaves the browser." }),
      el("table", {},
        el("thead", {}, el("tr", {},
          ["Time", "Prompt", "Method", "First token", "Total", "Result"].map((c) =>
            el("th", { scope: "col", text: c })))),
        historyBody)));
}

// errorMessageOf pulls the API's own message out of an error body, so the
// screen shows which field was refused rather than a status code.
function errorMessageOf(body) {
  try {
    const parsed = JSON.parse(body);
    return parsed.error?.message || parsed.error || "";
  } catch {
    return body;
  }
}


// --- Online prediction (#869) -----------------------------------------
//
// A Vertex AI custom prediction request, sent to a contract container this
// instance runs as a Cloud Run service. The console relays it through the
// cluster ingress and shows the container's status and body verbatim: an
// error the container returned is shown in its own words, and a console
// refusal (malformed JSON, no such endpoint) is shown as the console's.
// Nothing here is Vertex's Endpoint resource or PredictionService, which
// CloudBurrow does not serve; the page names them rather than drawing them.

const PREDICT_TITLE = "Online prediction";
const PREDICT_SUBTITLE =
  "Send a custom prediction request — instances and parameters — to a deployed prediction container.";
let PREDICT_ABORT = null;

function stopPrediction() {
  if (PREDICT_ABORT) { PREDICT_ABORT.abort(); PREDICT_ABORT = null; }
}

// Embeddings (#41) sends text-embedding :predict, EmbedContent's
// :embedContent (#1244), or :rawPredict with the :predict body (#1284), for
// the one embedding model served, a community ONNX
// conversion of EmbeddingGemma. The provenance banner is on the screen that
// shows the vectors, not only in the documentation.
async function renderEmbeddings(view) {
  const title = "Embeddings";
  const subtitle = "Text-embedding :predict, :embedContent and :rawPredict against the local endpoint";
  setChildren(view, pageHeader(title, subtitle), loadingState(3));
  let status;
  try {
    status = await api("/api/ai/embeddings");
  } catch (err) {
    return setChildren(view, pageHeader(title, subtitle),
      errorState("Embeddings unavailable", String(err.message), () => renderEmbeddings(view)));
  }
  if (!status.configured) {
    return setChildren(view, pageHeader(title, subtitle),
      emptyState("Embeddings are not configured", status.note || ""));
  }
  const header = el("div", { class: "card" },
    el("dl", {},
      el("dt", { text: "Model" }), el("dd", { class: "mono", text: status.model }),
      el("dt", { text: "Official Google artifact" }), el("dd", { text: "No — community conversion" }),
      el("dt", { text: "Endpoint" }), el("dd", { class: "mono", id: "emb-endpoint", text: status.endpoint })),
    el("p", { class: "unavailable", text: status.note }));

  const texts = el("textarea", { id: "emb-texts", rows: "4",
    placeholder: "One text per line", "aria-label": "Texts, one per line" });
  const task = el("select", { id: "emb-task", "aria-label": "Task type" },
    ...status.taskTypes.map((t) => el("option", { value: t, text: t })));
  const dim = el("select", { id: "emb-dim", "aria-label": "Output dimensionality" },
    ...status.dimensions.map((d) => el("option", { value: String(d), text: String(d) })));
  // The API method: :predict embeds every line in one request; EmbedContent
  // (:embedContent) takes one content, so each line is its own request.
  const methods = status.methods || ["predict"];
  const method = el("select", { id: "emb-method", "aria-label": "API method" },
    ...methods.map((m) => el("option", { value: m,
      text: m === "embedContent" ? "EmbedContent (:embedContent, one request per text)"
        : m === "rawPredict" ? "RawPredict (:rawPredict, the :predict body unwrapped)" : "Predict (:predict, one batch)" })));
  method.addEventListener("change", () => showEndpoint());
  const send = el("button", { text: "Embed" });
  const result = el("div", { "aria-live": "polite" });

  // The model the request names (#1305). Any other ID is sent as typed, and
  // the API's 404 — it substitutes nothing — is what the screen shows.
  const model = el("input", { id: "emb-model", value: status.model, list: "emb-models",
    "aria-label": "Model ID", class: "mono" });
  const models = el("datalist", { id: "emb-models" },
    el("option", { value: status.model, text: "served (community conversion)" }),
    ...(status.imageModel ? [el("option", { value: status.imageModel,
      text: "text and images, once loaded on Local models (community conversion)" })] : []),
    ...(status.notServed || []).map((m) => el("option", { value: m, text: "not served: answers 404" })));
  const endpointFor = (m) => {
    const ep = (status.endpoints && status.endpoints[m]) || status.endpoint;
    const name = model.value.trim() || status.model;
    return ep.replace("/models/" + status.model + ":", "/models/" + name + ":");
  };
  const showEndpoint = () => {
    const ep = document.getElementById("emb-endpoint");
    if (ep) ep.textContent = endpointFor(method.value);
  };
  model.addEventListener("input", showEndpoint);
  // Images (#1454), and videos (#1722) and PDFs (#1723): each chosen file
  // is embedded with its own EmbedContent request, as one inline_data part,
  // by the model that embeds images, so its vector can be compared with the
  // texts'.
  const images = el("input", { id: "emb-images", type: "file", multiple: "",
    // The extensions too: a browser that knows no image/heic or image/avif
    // type (#1724, #1729) matches the picker's filter by name.
    accept: [...(status.imageMimeTypes || []), ".heic", ".heif", ".avif", ...(status.videoMimeTypes || []),
      ...(status.documentMimeTypes || [])].join(","),
    "aria-label": "Images, videos and PDFs to embed" });
  // Audio clips (#1651) go the same way, and Cloud Storage objects as
  // file_data parts, one gs:// URI per line, read by the instance's own
  // Cloud Storage.
  const audio = el("input", { id: "emb-audio", type: "file", multiple: "",
    accept: (status.audioMimeTypes || []).join(","), "aria-label": "Audio clips to embed" });
  // A video's audio track (#1732): embed_content_config.audio_track_extraction
  // on each video's request, off by default as on Google.
  const audioTrack = el("input", { id: "emb-audio-track", type: "checkbox",
    "aria-label": "Embed each video's audio track (audio_track_extraction)" });
  // A PDF's text (#1733): embed_content_config.document_ocr on each PDF's
  // request, off by default as on Google; a scanned page is read with OCR.
  const documentOcr = el("input", { id: "emb-document-ocr", type: "checkbox",
    "aria-label": "Embed each PDF's text, read with OCR from a scanned page (document_ocr)" });
  const gcs = el("textarea", { id: "emb-gcs", rows: 2, cols: 60, class: "mono",
    placeholder: "gs://bucket/object.wav, one per line", "aria-label": "Cloud Storage objects to embed (file_data)" });
  const toMultimodal = () => {
    if (!status.imageModel) return;
    if ((model.value.trim() || status.model) === status.model) model.value = status.imageModel;
    method.value = "embedContent";
    showEndpoint();
  };
  images.addEventListener("change", () => { if (images.files.length) toMultimodal(); });
  audio.addEventListener("change", () => { if (audio.files.length) toMultimodal(); });
  gcs.addEventListener("input", () => { if (gcs.value.trim()) toMultimodal(); });
  // A media type from a file name's extension, for a gs:// URI or a file
  // the browser gives no type (#1651: WebP and BMP; #1721: MP3; #1724: HEIC
  // and HEIF; #1722, #1723: video and PDF; #1729: AVIF).
  const mediaTypeOf = (name) => ({ wav: "audio/wav", mp3: "audio/mpeg", png: "image/png", jpg: "image/jpeg", jpeg: "image/jpeg",
    webp: "image/webp", bmp: "image/bmp", heic: "image/heic", heif: "image/heif", avif: "image/avif", mp4: "video/mp4", mov: "video/quicktime",
    pdf: "application/pdf" })[(String(name).match(/\.([a-z0-9]+)$/i) || [])[1]?.toLowerCase()];
  const readBase64 = (file) => new Promise((resolve, reject) => {
    const r = new FileReader();
    r.onload = () => resolve(String(r.result).replace(/^data:[^,]*,/, ""));
    r.onerror = () => reject(r.error || new Error("could not read " + file.name));
    r.readAsDataURL(file);
  });
  const sent = [];
  const post = async (m, payload) => {
    sent.push({ url: endpointFor(m), body: payload });
    const resp = await fetch("/api/ai/embeddings?" + new URLSearchParams({ method: m, model: model.value.trim() || status.model }), {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(payload),
    });
    const body = await resp.text();
    if (!resp.ok) throw new Error(errorMessageOf(body) || `HTTP ${resp.status}`);
    return JSON.parse(body);
  };

  const run = async () => {
    const lines = texts.value.split("\n").map((l) => l.trim()).filter(Boolean);
    const files = [...(images.files || [])];
    const clips = [...(audio.files || [])];
    const uris = gcs.value.split("\n").map((l) => l.trim()).filter(Boolean);
    if (!lines.length && !files.length && !clips.length && !uris.length) {
      announce("At least one text, image, audio clip or Cloud Storage object is required"); texts.focus(); return;
    }
    if ((files.length || clips.length || uris.length) && method.value !== "embedContent") {
      announce("Images, audio and Cloud Storage objects are sent with EmbedContent"); method.focus(); return;
    }
    send.disabled = true;
    sent.length = 0;
    setChildren(result, el("p", { class: "unavailable", text: "running…" }));
    try {
      // Each result as [values, token count, truncated], as the API
      // returned them (#1396): EmbedContent's usageMetadata.promptTokenCount
      // and truncated, :predict's statistics.token_count and truncated.
      let got, displayName = "";
      if (method.value === "embedContent") {
        got = [];
        for (const text of lines) {
          const data = await post("embedContent", {
            content: { role: "user", parts: [{ text }] },
            embedContentConfig: { taskType: task.value, outputDimensionality: Number(dim.value) },
          });
          got.push([data.embedding?.values || [], data.usageMetadata?.promptTokenCount ?? "",
            // proto3 JSON leaves a false truncated out.
            data.truncated === true]);
        }
        for (const file of files) {
          const isVideo = (file.type || mediaTypeOf(file.name) || "").startsWith("video/");
          const isPDF = (file.type || mediaTypeOf(file.name)) === "application/pdf";
          const data = await post("embedContent", {
            content: { role: "user", parts: [{ inlineData: { mimeType: file.type || mediaTypeOf(file.name), data: await readBase64(file) } }] },
            embedContentConfig: { outputDimensionality: Number(dim.value),
              ...(isVideo && audioTrack.checked ? { audioTrackExtraction: true } : {}),
              ...(isPDF && documentOcr.checked ? { documentOcr: true } : {}) },
          });
          got.push([data.embedding?.values || [], data.usageMetadata?.promptTokenCount ?? "", data.truncated === true]);
          const type = file.type || mediaTypeOf(file.name) || "";
          lines.push((type.startsWith("video/") ? "[video] " : type === "application/pdf" ? "[pdf] " : "[image] ") + file.name);
        }
        const media = [
          ...clips.map((file) => ["[audio] " + file.name, async () => ({ inlineData: { mimeType: file.type || mediaTypeOf(file.name) || "audio/wav", data: await readBase64(file) } })]),
          // Google's file_data carries a mime_type; it is named from the
          // object's extension when known, else the object's content type is used.
          ...uris.map((uri) => ["[file_data] " + uri, async () => {
            const mimeType = mediaTypeOf(uri);
            return { fileData: { fileUri: uri, ...(mimeType ? { mimeType } : {}) } };
          }]),
        ];
        for (const [label, part] of media) {
          const p = await part();
          const isPDF = p.fileData?.mimeType === "application/pdf";
          const data = await post("embedContent", {
            content: { role: "user", parts: [p] },
            embedContentConfig: { outputDimensionality: Number(dim.value),
              ...(isPDF && documentOcr.checked ? { documentOcr: true } : {}) },
          });
          got.push([data.embedding?.values || [], data.usageMetadata?.promptTokenCount ?? "", data.truncated === true]);
          lines.push(label);
        }
        const used = model.value.trim() || status.model;
        displayName = "EmbedContent: " + used + " — " + (used === status.imageModel ? status.imageModelNote : status.note);
      } else {
        // RawPredict sends the same body unwrapped, and gets the same answer.
        const data = await post(method.value === "rawPredict" ? "rawPredict" : "predict", {
          instances: lines.map((content) => ({ content, task_type: task.value })),
          parameters: { outputDimensionality: Number(dim.value) },
        });
        got = (data.predictions || []).map((p) => [p.embeddings?.values || [],
          p.embeddings?.statistics?.token_count ?? "", p.embeddings?.statistics?.truncated === true]);
        displayName = data.modelDisplayName || "";
      }
      const rows = got.map(([v, tokens, truncated], i) => {
        return el("tr", {},
          el("td", { text: lines[i].length > 60 ? lines[i].slice(0, 60) + "…" : lines[i] }),
          el("td", { class: "mono", text: String(v.length) }),
          el("td", { class: "mono", text: String(tokens) }),
          el("td", { text: truncated ? "Yes" : "No" }),
          el("td", { class: "mono", text: v.length ? embeddingNorm(v).toFixed(4) : "" }),
          el("td", { class: "mono", text: v.slice(0, 6).map((x) => x.toFixed(4)).join(", ") + (v.length > 6 ? ", …" : "") }));
      });
      // The whole vector of each text, not only its first values (#1305),
      // with a copy of it as the JSON array a client would receive.
      const vectors = got.map(([v], i) => {
        const json = JSON.stringify(v);
        const copy = el("button", { class: "secondary", text: "Copy vector" });
        copy.addEventListener("click", async () => {
          try { await navigator.clipboard.writeText(json); announce("Vector copied"); }
          catch (err) { announce("Copy failed: " + err.message); }
        });
        return el("details", { class: "emb-vector" },
          el("summary", { text: `Vector ${i + 1}: ${v.length} values — ${lines[i].slice(0, 40)}` }),
          copy, el("pre", { class: "mono", text: json }));
      });
      // Cosine similarity of every pair, when there is more than one text.
      const similarity = got.length > 1 ? el("table", { id: "emb-similarity" },
        el("caption", { text: "Cosine similarity" }),
        el("thead", {}, el("tr", {}, el("th", { text: "" }),
          ...got.map((_, j) => el("th", { class: "mono", text: String(j + 1) })))),
        el("tbody", {}, ...got.map(([a], i) => el("tr", {},
          el("th", { class: "mono", text: String(i + 1) }),
          ...got.map(([b]) => el("td", { class: "mono", text: embeddingCosine(a, b).toFixed(4) })))))) : null;
      // An image's base64 is shortened in the request shown.
      const shown = (body) => JSON.stringify(body, (k, v) => k === "data" && typeof v === "string" && v.length > 80
        ? v.slice(0, 80) + `… (${v.length} base64 characters)` : v, 2);
      const request = el("details", { id: "emb-request" },
        el("summary", { text: `Request${sent.length > 1 ? "s" : ""} sent (${sent.length})` }),
        ...sent.map((r) => el("pre", { class: "mono", text: "POST " + r.url + "\n" + shown(r.body) })));
      setChildren(result,
        el("p", { class: "unavailable", text: displayName }),
        el("table", { id: "emb-results" },
          el("thead", {}, el("tr", {},
            el("th", { text: "Text" }), el("th", { text: "Dimensions" }),
            el("th", { text: "Tokens" }), el("th", { text: "Truncated" }),
            el("th", { text: "L2 norm" }), el("th", { text: "First values" }))),
          el("tbody", {}, ...rows)),
        ...(similarity ? [similarity] : []),
        ...vectors, request);
    } catch (err) {
      setChildren(result, el("p", { class: "unavailable", text: `[error] ${err.message}` }));
    } finally {
      send.disabled = false;
    }
  };
  send.addEventListener("click", run);

  setChildren(view, pageHeader(title, subtitle), header,
    el("div", { class: "card" }, texts,
      ...(status.imageModel ? [el("div", {}, el("label", { for: "emb-images", text: "Images " }), images,
        el("label", { for: "emb-audio", text: " Audio " }), audio,
        el("label", { for: "emb-audio-track", text: " Videos' audio track " }), audioTrack,
        el("label", { for: "emb-document-ocr", text: " PDFs' text (OCR) " }), documentOcr),
        el("div", {}, el("label", { for: "emb-gcs", text: "Cloud Storage objects " }), gcs),
        el("p", { class: "unavailable", id: "emb-image-note", text: status.imageModelNote })] : []),
      el("div", {}, el("label", { text: "Model " }), model, models, el("label", { text: " Method " }), method, el("label", { text: " Task type " }), task,
        el("label", { text: " Dimensions " }), dim, " ", send)),
    result);
}

// playgroundModelsCard is the Studio's view of the local models (#1396):
// each one's state, as the Local models page reports it, with the way to
// that page — where Download, Load, Unload and Delete are — and to the
// Embeddings test view. null when the instance offers no model management.
async function playgroundModelsCard() {
  let status;
  try { status = await api("/api/ai/local-models"); } catch { return null; }
  if (!status.configured) return null;
  const embeddings = (SERVICES || []).some((s) => s.id === "ai-embed");
  const rows = status.models.filter((m) => m.pinned || m.loaded).map((m) => el("tr", { "data-model": m.id },
    el("td", { class: "mono", text: m.id }),
    el("td", { text: m.modality }),
    el("td", { "data-state": m.state, text: (LOCAL_MODEL_STATES[m.state] || m.state) +
      (m.state === "downloading" && m.bytes ? ` ${Math.floor((100 * m.downloaded) / m.bytes)}%` : "") }),
    el("td", { text: m.loaded ? "Loaded" : (m.loadable ? "Not loaded" : "—") })));
  const others = status.models.length - rows.length;
  return el("div", { class: "card", id: "pg-local-models" },
    el("h2", { text: "Local models" }),
    el("table", {},
      el("thead", {}, el("tr", {}, ["Model", "Modality", "Download state", "Runtime"].map((c) =>
        el("th", { scope: "col", text: c })))),
      el("tbody", {}, ...rows)),
    others > 0 ? el("p", { class: "unavailable",
      text: `${others} more catalogued model${others === 1 ? " is" : "s are"} not pinned, so never downloaded here.` }) : null,
    el("p", {},
      el("a", { href: AP_PATHS.localModels, text: "Download, load, unload or delete a model" }),
      ...(embeddings ? [" · ", el("a", { href: AP_PATHS.embeddings, text: "Test embeddings" })] : [])));
}

// Local models (#1306) lists the catalogue with each model's download state
// and size. Download is offered only for a pinned, ungated model, and only
// after the user confirms its exact size; the server refuses any other size.
// Load and Unload are offered where the runtime supports them.
let LOCAL_MODELS_POLL = null;

function stopLocalModelsPoll() {
  if (LOCAL_MODELS_POLL) { clearTimeout(LOCAL_MODELS_POLL); LOCAL_MODELS_POLL = null; }
}

const LOCAL_MODEL_STATES = {
  "not-pinned": "Not pinned", absent: "Not downloaded", partial: "Partly downloaded",
  corrupt: "Digest mismatch", downloading: "Downloading", downloaded: "Downloaded",
};

async function localModelAction(id, action, body) {
  const resp = await fetch(`/api/ai/local-models/${encodeURIComponent(id)}/${action}`, {
    method: "POST", headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body || {}),
  });
  const text = await resp.text();
  if (!resp.ok) throw new Error(errorMessageOf(text) || `HTTP ${resp.status}`);
  return JSON.parse(text);
}

async function renderLocalModels(view) {
  stopLocalModelsPoll();
  const title = "Local models";
  const subtitle = "Download, load, unload and delete the local AI models this instance can serve";
  const header = () => pageHeader(title, subtitle);
  let status;
  try {
    status = await api("/api/ai/local-models");
  } catch (err) {
    return setChildren(view, header(),
      errorState("Local models unavailable", String(err.message), () => renderLocalModels(view)));
  }
  if (!status.configured) {
    return setChildren(view, header(), emptyState("Local models are not available", status.note || ""));
  }
  const message = el("p", { id: "local-models-message", class: "unavailable", role: "status" });
  const fail = (err) => { message.textContent = `[error] ${err.message}`; announce(err.message); };
  const again = () => renderLocalModels(view);

  const actionsFor = (m) => {
    const out = [];
    const btn = (label, run, cls = "secondary") => {
      const b = el("button", { class: cls, text: label, "data-action": label.toLowerCase() });
      b.addEventListener("click", async () => {
        b.disabled = true;
        try { await run(); } catch (err) { fail(err); } finally { b.disabled = false; }
      });
      out.push(b);
    };
    const canDownload = m.pinned && !m.downloadNote && m.state !== "downloaded" && m.state !== "downloading";
    if (canDownload) {
      btn("Download", () => confirmDestructive({
        title: `Download ${m.id}?`,
        detail: `${formatBytes(m.bytes)} (${m.bytes.toLocaleString()} bytes) from huggingface.co/${m.repo} ` +
          `at revision ${m.revision.slice(0, 12)}: ${m.files.map((f) => f.path).join(", ")}. ` +
          (m.official ? "" : "This is a COMMUNITY conversion, NOT an official Google artifact. ") +
          `Licence: ${m.license}.`,
        confirmWord: m.id, confirmLabel: "Download",
        consequence: "Each file is checked against its pinned SHA-256 before it is kept.",
        onConfirm: async () => { await localModelAction(m.id, "download", { confirmBytes: m.bytes }); again(); },
      }), "primary");
    }
    if (m.state === "downloading") btn("Cancel", async () => { await localModelAction(m.id, "cancel"); again(); });
    if (m.loadable && m.state === "downloaded" && !m.loaded) {
      btn("Load", async () => { await localModelAction(m.id, "load"); announce(`${m.id} loaded`); again(); });
    }
    if (m.loadable && m.loaded) {
      btn("Unload", async () => { await localModelAction(m.id, "unload"); announce(`${m.id} unloaded`); again(); });
    }
    if (m.pinned && !m.loaded && m.state !== "downloading" && m.state !== "absent") {
      btn("Delete", () => confirmDestructive({
        title: `Delete the local copy of ${m.id}?`,
        detail: `${formatBytes(m.downloaded)} in the instance's model cache. It can be downloaded again.`,
        confirmWord: m.id, consequence: "",
        onConfirm: async () => { await localModelAction(m.id, "delete"); again(); },
      }), "secondary danger");
    }
    return out;
  };

  const rows = status.models.map((m) => {
    const state = LOCAL_MODEL_STATES[m.state] || m.state;
    const progress = m.state === "downloading" && m.bytes
      ? el("progress", { max: String(m.bytes), value: String(m.downloaded),
          "aria-label": `${m.id} download progress` }) : null;
    const notes = [m.error ? `Last download failed: ${m.error}` : "", m.downloadNote || "",
      m.loadable ? "" : (m.loadNote || "")].filter(Boolean);
    return el("tr", { "data-model": m.id },
      el("td", {}, el("span", { class: "mono", text: m.id }),
        el("div", { class: "unavailable", text: `${m.publisher}${m.official ? "" : " — not an official Google artifact"}` })),
      el("td", { text: m.modality }),
      el("td", { class: "mono", text: m.pinned ? formatBytes(m.bytes) : "—" }),
      el("td", { "data-state": m.state },
        el("span", { text: state + (m.state === "downloading" && m.bytes
          ? ` ${Math.floor((100 * m.downloaded) / m.bytes)}%` : "") }), progress),
      el("td", { text: m.loaded ? "Loaded" : (m.loadable ? "Not loaded" : "—") }),
      el("td", {}, ...actionsFor(m),
        ...notes.map((n) => el("div", { class: "unavailable", text: n }))));
  });

  setChildren(view, header(),
    el("div", { class: "card" }, el("p", { class: "unavailable", text: status.note })),
    message,
    // In a region of its own that scrolls, so at 320px it is the table that
    // scrolls sideways rather than the page (#1604).
    el("div", { class: "table-wrap" }, el("table", { id: "local-models" },
      el("thead", {}, el("tr", {},
        el("th", { text: "Model" }), el("th", { text: "Modality" }), el("th", { text: "Size" }),
        el("th", { text: "Download state" }), el("th", { text: "Runtime" }), el("th", { text: "Actions" }))),
      el("tbody", {}, ...rows))));

  if (status.models.some((m) => m.state === "downloading")) {
    LOCAL_MODELS_POLL = setTimeout(() => {
      if (location.pathname === AP_PATHS.localModels) renderLocalModels(view);
    }, 1000);
  }
}

function embeddingNorm(v) {
  let s = 0;
  for (const x of v) s += x * x;
  return Math.sqrt(s);
}

function embeddingCosine(a, b) {
  const n = Math.min(a.length, b.length);
  let dot = 0, na = 0, nb = 0;
  for (let i = 0; i < n; i++) { dot += a[i] * b[i]; na += a[i] * a[i]; nb += b[i] * b[i]; }
  return na && nb ? dot / Math.sqrt(na * nb) : 0;
}

// --- Agent Platform: Model Garden, Agent Garden and the pages with no backend (#1839)
//
// Model Garden is a catalogue: every card Google lists, with its filters.
// Deploy is enabled only where /api/ai/model-garden says the model runs on
// this machine, and it is the real local download and load, the same calls
// the Local models page makes. A card with no local runtime keeps its Deploy
// button, disabled, beside the reason: a button that pretended to create an
// endpoint would be the one thing this console must never do.
let GARDEN_POLL = null;

function stopGardenPoll() {
  if (GARDEN_POLL) { clearTimeout(GARDEN_POLL); GARDEN_POLL = null; }
  if (HF_POLL) { clearTimeout(HF_POLL); HF_POLL = null; }
}

const GARDEN_FILTER_KEY = { "Model collections": "collection", Tasks: "tasks", Providers: "provider" };

function gardenMatches(m, chosen, text) {
  for (const [group, values] of Object.entries(chosen)) {
    if (!values.size) continue;
    const key = GARDEN_FILTER_KEY[group];
    const have = Array.isArray(m[key]) ? m[key] : [m[key]];
    if (!have.some((v) => values.has(v))) return false;
  }
  if (!text) return true;
  const hay = [m.name, m.id, m.provider, m.collection, m.summary, ...(m.tasks || [])].join(" ").toLowerCase();
  return hay.includes(text.toLowerCase());
}

// gardenDeploy runs the next real step for a local model: download it (after
// its exact size is confirmed), then load it where the runtime can.
function gardenDeployAction(m, again, fail) {
  const st = m.localState;
  if (!m.deployable || !st) return null;
  if (st.state === "downloading") return null;
  if (st.state !== "downloaded") {
    return () => confirmDestructive({
      title: `Deploy ${m.name} locally?`,
      detail: `Deploying downloads ${formatBytes(st.bytes)} (${st.bytes.toLocaleString()} bytes) from ` +
        `huggingface.co/${st.repo} at revision ${(st.revision || "").slice(0, 12)} into this instance's model ` +
        `cache, then serves it from the local endpoint. ` +
        (st.official ? "" : "The file is a COMMUNITY conversion, NOT an official Google artifact. ") +
        `Licence: ${st.license}.`,
      confirmWord: st.id, confirmLabel: "Deploy",
      consequence: "Each file is checked against its pinned SHA-256 before it is kept.",
      onConfirm: async () => { await localModelAction(st.id, "download", { confirmBytes: st.bytes }); again(); },
    }).catch(fail);
  }
  if (st.loadable && !st.loaded) {
    return async () => {
      try { await localModelAction(st.id, "load"); announce(`${m.name} deployed locally`); again(); } catch (err) { fail(err); }
    };
  }
  return null;
}

function gardenStatusOf(m) {
  const st = m.localState;
  if (!m.deployable || !st) return null;
  if (st.state === "downloading") {
    const pct = st.bytes ? Math.floor((100 * st.downloaded) / st.bytes) : 0;
    return el("div", { class: "garden-status", "data-state": "downloading" },
      el("span", { text: `Downloading ${pct}% (${formatBytes(st.downloaded)} of ${formatBytes(st.bytes)})` }),
      el("progress", { max: String(st.bytes || 1), value: String(st.downloaded || 0),
        "aria-label": `${m.name} download progress` }));
  }
  if (st.state === "downloaded" && (st.loaded || !st.loadable)) {
    return el("div", { class: "garden-status", "data-state": "deployed" },
      el("span", { text: st.loaded ? "Deployed locally: served by the local endpoint" :
        "Downloaded: served when CloudBurrow runs with this model (see Local models)" }));
  }
  if (st.state === "downloaded") {
    return el("div", { class: "garden-status", "data-state": "downloaded" }, el("span", { text: "Downloaded, not loaded" }));
  }
  return null;
}

// The Hugging Face section of Model Garden (#1840): look up a model ID on
// huggingface.co, see its licence, gated status and size against the free
// disk, download it with a progress bar, and delete it. A gated model needs
// HF_TOKEN in CloudBurrow's own environment; there is no token field here.
let HF_POLL = null;

async function hfAction(action, id) {
  const resp = await fetch(`/api/ai/hf-models/${action}`, {
    method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ id }),
  });
  const text = await resp.text();
  if (!resp.ok) throw new Error(errorMessageOf(text) || `HTTP ${resp.status}`);
  return JSON.parse(text);
}

async function drawHFPanel(host) {
  if (HF_POLL) { clearTimeout(HF_POLL); HF_POLL = null; }
  let data;
  try {
    data = await api("/api/ai/hf-models");
  } catch (err) {
    return setChildren(host, el("h2", { text: "Deploy from Hugging Face" }), el("p", { class: "unavailable", text: String(err.message) }));
  }
  if (!data.configured) {
    return setChildren(host, el("h2", { text: "Deploy from Hugging Face" }), el("p", { class: "unavailable", text: data.note }));
  }
  const again = () => { if (host.isConnected) drawHFPanel(host); };
  const message = el("p", { id: "hf-message", class: "unavailable", role: "status" });
  const fail = (err) => { message.textContent = `[error] ${err.message}`; announce(err.message); };
  const input = el("input", { type: "text", id: "hf-model-id", placeholder: "author/model, e.g. Qwen/Qwen2.5-0.5B-Instruct",
    "aria-label": "Hugging Face model ID", autocomplete: "off" });
  const details = el("div", { id: "hf-lookup" });
  const lookup = el("button", { class: "secondary", type: "submit", text: "Check model" });
  const form = el("form", { class: "garden-actions", onsubmit: async (e) => {
    e.preventDefault();
    const id = input.value.trim();
    if (!id) return;
    setChildren(details, el("p", { class: "unavailable", text: `Reading ${id} from huggingface.co…` }));
    try {
      const r = await api("/api/ai/hf-models/lookup?" + new URLSearchParams({ id }));
      const m = r.model;
      const fits = r.free < 0 || r.free - m.bytes >= r.floor;
      const download = el("button", { class: "primary", text: "Download", "data-action": "download",
        disabled: fits ? null : true });
      download.addEventListener("click", () => confirmDestructive({
        title: `Download ${m.id}?`,
        detail: `${formatBytes(m.bytes)} in ${m.files.length} files from huggingface.co/${m.id} at revision ` +
          `${m.revision.slice(0, 12)}. Licence: ${m.license}.`,
        confirmWord: m.id, confirmLabel: "Download",
        consequence: "Each LFS file is checked against its SHA-256 before it is kept.",
        onConfirm: async () => { await hfAction("download", m.id); again(); },
      }).catch(fail));
      setChildren(details, el("dl", { id: "hf-facts" },
        el("dt", { text: "Model" }), el("dd", { text: m.id }),
        el("dt", { text: "Licence" }), el("dd", { text: m.license }),
        el("dt", { text: "Access" }), el("dd", { text: m.gated ? `Gated (${m.gated} approval): accept the licence on huggingface.co and set HF_TOKEN in CloudBurrow's environment` : "Public" }),
        el("dt", { text: "Task" }), el("dd", { text: m.pipelineTag || "not stated" }),
        el("dt", { text: "Size" }), el("dd", { text: `${formatBytes(m.bytes)} in ${m.files.length} files` }),
        el("dt", { text: "Free disk" }), el("dd", { text: r.free < 0 ? "unknown" : `${formatBytes(r.free)} (a download must leave ${formatBytes(r.floor)})` })),
        fits ? null : el("p", { class: "unavailable", text: "Does not fit: downloading it would leave less than the free space a download must leave. Delete a model or free disk space." }),
        download);
    } catch (err) {
      setChildren(details, el("p", { class: "unavailable", text: `[error] ${err.message}` }));
    }
  } }, input, lookup);

  const rows = data.models.map((m) => {
    const pct = m.bytes ? Math.floor((100 * m.downloaded) / m.bytes) : 0;
    const actions = [];
    if (m.state === "downloading") {
      actions.push(el("button", { class: "secondary", text: "Cancel", "data-action": "cancel",
        onclick: () => hfAction("cancel", m.id).then(again, fail) }));
    } else {
      if (m.state === "downloaded") {
        actions.push(el("button", { class: "primary", text: "Deploy", "data-action": "deploy",
          onclick: () => hfDeploy(m).catch(fail) }));
      }
      actions.push(el("button", { class: "secondary danger", text: "Delete files", "data-action": "delete",
        onclick: () => confirmDestructive({
          title: `Delete ${m.id}?`, detail: `${formatBytes(m.bytes)} in this instance's model cache. It can be downloaded again.`,
          confirmWord: m.id, consequence: "",
          onConfirm: async () => { await hfAction("delete", m.id); again(); },
        }).catch(fail) }));
    }
    return el("tr", { "data-hf-model": m.id },
      el("td", { class: "mono", text: m.id }),
      el("td", { text: m.license }),
      el("td", { text: m.gated ? "Gated" : "Public" }),
      el("td", { class: "mono", text: formatBytes(m.bytes) }),
      el("td", { "data-state": m.state },
        el("span", { text: m.state === "downloading" ? `Downloading ${pct}%` : m.state === "failed" ? `Failed: ${m.error}` : "Downloaded" }),
        m.state === "downloading" ? el("progress", { max: String(m.bytes || 1), value: String(m.downloaded),
          "aria-label": `${m.id} download progress` }) : null),
      el("td", {}, ...actions));
  });
  // A redraw while a download runs keeps what was typed and looked up.
  const keep = host.querySelector("#hf-keep") || el("div", { id: "hf-keep" }, form, details, message);
  setChildren(host,
    el("h2", { text: "Deploy from Hugging Face" }),
    el("p", { class: "unavailable", text: data.note }),
    keep,
    rows.length ? el("div", { class: "table-wrap" }, el("table", { id: "hf-models" },
      el("thead", {}, el("tr", {}, ...["Model", "Licence", "Access", "Size", "State", "Actions"].map((c) => el("th", { scope: "col", text: c })))),
      el("tbody", {}, ...rows))) : null);
  if (data.models.some((m) => m.state === "downloading")) {
    HF_POLL = setTimeout(again, 1000);
  }
}

// Deploy runs ModelGardenService.Deploy on a downloaded Hugging Face model
// (#1840) and opens its endpoint, which shows the deployment's progress
// through to Ready. A gated model's licence is accepted here, as the
// request's model_config.accept_eula.
async function hfDeploy(m) {
  const engine = /^(feature-extraction|sentence-similarity)$/.test(m.pipelineTag || "")
    ? "Text Embeddings Inference" : "vLLM";
  const go = async () => {
    const resp = await fetch("/api/ai/hf-deployments/deploy", {
      method: "POST", headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ id: m.id, acceptEula: !!m.gated }),
    });
    const text = await resp.text();
    if (!resp.ok) throw new Error(errorMessageOf(text) || `HTTP ${resp.status}`);
    const r = JSON.parse(text);
    navigate(`${AP_PATHS.endpoints}?endpoint=${encodeURIComponent(r.endpoint)}`);
  };
  if (!m.gated) return go();
  return confirmDestructive({
    title: `Deploy ${m.id}?`, detail: `${m.id} is gated on Hugging Face. Deploying accepts its licence (${m.license}).`,
    confirmWord: m.id, confirmLabel: "Deploy", consequence: `It is served by ${engine} on a local endpoint.`, onConfirm: go,
  });
}

let HF_EP_POLL = null;

const HF_STATE_LABEL = { DEPLOYING: "Deploying", READY: "Ready", FAILED: "Failed" };

// The Endpoints page (#1840): Model Garden's deployments with their state,
// and the selected endpoint's details and test panel.
async function renderHFEndpoints(view) {
  if (HF_EP_POLL) { clearTimeout(HF_EP_POLL); HF_EP_POLL = null; }
  const header = () => pageHeader("Endpoints", "Models deployed from Model Garden, served on this instance.");
  let data;
  try {
    data = await api("/api/ai/hf-deployments");
  } catch (err) {
    return setChildren(view, header(), errorState("Endpoints unavailable", String(err.message), () => renderHFEndpoints(view)));
  }
  if (!data.configured) return setChildren(view, header(), el("div", { class: "card" }, el("p", { class: "unavailable", text: data.note })));
  const again = () => { if (view.isConnected && location.pathname === AP_PATHS.endpoints) renderHFEndpoints(view); };
  const message = el("p", { id: "hf-ep-message", class: "unavailable", role: "status" });
  const fail = (err) => { message.textContent = `[error] ${err.message}`; announce(err.message); };
  const post = async (action, body) => {
    const resp = await fetch(`/api/ai/hf-deployments/${action}`, {
      method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body) });
    const text = await resp.text();
    if (!resp.ok) throw new Error(errorMessageOf(text) || `HTTP ${resp.status}`);
    return text;
  };
  const selected = new URLSearchParams(location.search).get("endpoint");
  const stateOf = (e) => (e.deployed ? e.deployed.state : "UNDEPLOYED");
  const rows = data.endpoints.map((e) => {
    const st = stateOf(e);
    const d = e.deployed;
    return el("tr", { "data-endpoint": e.name, class: e.name === selected ? "selected" : null },
      el("td", {}, el("a", { href: `${AP_PATHS.endpoints}?endpoint=${encodeURIComponent(e.name)}`, text: e.displayName,
        onclick: (ev) => { ev.preventDefault(); navigate(ev.currentTarget.getAttribute("href")); } })),
      el("td", { class: "mono", text: e.id }),
      el("td", { class: "mono", text: e.model ? e.model.hfModelId : "—" }),
      el("td", { text: e.model ? (e.model.engine === "tei" ? "Text Embeddings Inference" : "vLLM") : "—" }),
      el("td", { "data-state": st },
        el("span", { text: st === "UNDEPLOYED" ? "No deployed model" : HF_STATE_LABEL[st] || st }),
        st === "DEPLOYING" ? el("span", { class: "unavailable", text: ` · ${d.message}` }) : null,
        st === "DEPLOYING" && d.progress > 0 ? el("progress", { max: "100", value: String(d.progress), "aria-label": `${e.displayName} deploy progress` }) : null,
        st === "FAILED" ? el("p", { class: "unavailable", text: d.message }) : null));
  });
  const table = rows.length ? el("div", { class: "table-wrap" }, el("table", { id: "hf-endpoints" },
    el("thead", {}, el("tr", {}, ...["Name", "ID", "Model", "Serving container", "State"].map((c) => el("th", { scope: "col", text: c })))),
    el("tbody", {}, ...rows)))
    : el("p", { class: "unavailable", text: "No endpoints yet. Download a Hugging Face model in Model Garden, then Deploy it." });

  let detail = null;
  const ep = data.endpoints.find((e) => e.name === selected);
  if (ep) {
    const st = stateOf(ep);
    const rest = `/v1/${ep.name}`;
    const actions = el("div", { class: "garden-actions" });
    if (ep.deployed && st !== "DEPLOYING") {
      actions.append(el("button", { class: "secondary", text: "Undeploy", "data-action": "undeploy",
        onclick: () => post("undeploy", { endpoint: ep.name }).then(again, fail) }));
    }
    actions.append(el("button", { class: "secondary danger", text: "Delete", "data-action": "delete-endpoint",
      onclick: () => confirmDestructive({
        title: `Delete ${ep.displayName}?`, detail: "Undeploys the model, freeing its container, and deletes the endpoint and its model. The downloaded files stay in Model Garden.",
        confirmWord: ep.displayName, onConfirm: async () => { await post("delete", { endpoint: ep.name }); navigate(AP_PATHS.endpoints); },
      }).catch(fail) }));
    const facts = el("dl", { id: "hf-endpoint-facts" },
      el("dt", { text: "Endpoint" }), el("dd", { class: "mono", text: ep.name }),
      el("dt", { text: "State" }), el("dd", { id: "hf-endpoint-state", text: st === "UNDEPLOYED" ? "No deployed model" : HF_STATE_LABEL[st] || st }),
      ep.model ? el("dt", { text: "Model" }) : null, ep.model ? el("dd", { class: "mono", text: `${ep.model.hfModelId} @ ${ep.model.revision.slice(0, 12)} (${ep.model.license})` }) : null,
      ep.model ? el("dt", { text: "Container" }) : null, ep.model ? el("dd", { class: "mono", text: ep.model.image }) : null,
      el("dt", { text: "Predict" }), el("dd", { class: "mono", text: `POST ${rest}:predict, :rawPredict, :streamRawPredict` }),
      ep.model && ep.model.engine === "vllm" ? el("dt", { text: "OpenAI-compatible" }) : null,
      ep.model && ep.model.engine === "vllm" ? el("dd", { class: "mono", text: `POST /v1beta1/${ep.name}/chat/completions` }) : null);
    let test = null;
    if (st === "READY") {
      const tei = ep.model.engine === "tei";
      const input = el("textarea", { id: "hf-test-input", rows: "3", "aria-label": "Test input",
        placeholder: tei ? "Text to embed" : "Prompt" });
      input.value = tei ? "What is deep learning?" : "What is the capital of France?";
      const out = el("pre", { id: "hf-test-output", class: "mono", "aria-live": "polite" });
      const run = (mode) => {
        const text = input.value;
        const body = mode === "chat"
          ? { messages: [{ role: "user", content: text }], max_tokens: 64 }
          : { instances: [tei ? { inputs: text } : { prompt: text, max_tokens: 64, temperature: 0 }] };
        out.textContent = "…";
        post("test", { endpoint: ep.name, mode, body }).then((t) => {
          const r = JSON.parse(t);
          if (mode === "chat") out.textContent = (r.choices && r.choices[0] && r.choices[0].message ? r.choices[0].message.content : t);
          else if (tei) {
            const v = r.predictions[0][0];
            out.textContent = `${v.length}-dimensional embedding: [${v.slice(0, 6).map((x) => x.toFixed(4)).join(", ")}, …]`;
          } else out.textContent = r.predictions[0];
        }, (err) => { out.textContent = `[error] ${err.message}`; });
      };
      test = el("section", { class: "card", id: "hf-test", "aria-label": "Test your model" },
        el("h2", { text: "Test your model" }), input,
        el("div", { class: "garden-actions" },
          el("button", { class: "primary", text: "Predict", "data-action": "test-predict", onclick: () => run("predict") }),
          tei ? null : el("button", { class: "secondary", text: "Chat completion", "data-action": "test-chat", onclick: () => run("chat") })),
        out);
    }
    detail = el("section", { class: "card", id: "hf-endpoint", "aria-label": ep.displayName },
      el("h2", { text: ep.displayName }), facts, actions, test);
  }
  // A redraw while deploying keeps the test panel's input.
  const keptTest = view.querySelector("#hf-test");
  setChildren(view, header(), message, el("div", { class: "card" }, table), detail);
  if (keptTest && detail && detail.querySelector("#hf-test")) detail.querySelector("#hf-test").replaceWith(keptTest);
  if (data.endpoints.some((e) => stateOf(e) === "DEPLOYING")) HF_EP_POLL = setTimeout(again, 2000);
}

async function renderModelGarden(view) {
  stopGardenPoll();
  const header = () => pageHeader("Model Garden",
    "Discover, test, customize and deploy models. Deploy is enabled for the models this machine runs.");
  let data;
  try {
    data = await api("/api/ai/model-garden");
  } catch (err) {
    return setChildren(view, header(), errorState("Model Garden unavailable", String(err.message), () => renderModelGarden(view)));
  }
  const message = el("p", { id: "garden-message", class: "unavailable", role: "status" });
  const fail = (err) => { message.textContent = `[error] ${err.message}`; announce(err.message); };
  const again = () => { if (view.isConnected) renderModelGarden(view); };

  const params = new URLSearchParams(location.search);
  const chosen = {};
  for (const f of data.filters) {
    const key = GARDEN_FILTER_KEY[f.name];
    chosen[f.name] = new Set(params.getAll(key));
  }
  const search = el("input", { type: "search", id: "garden-search", placeholder: "Search models",
    "aria-label": "Search models", value: params.get("q") || "" });
  const grid = el("div", { class: "garden-cards", id: "garden-cards" });
  const count = el("p", { class: "unavailable", id: "garden-count", role: "status" });
  const hfPanel = el("section", { class: "card", id: "hf-panel", "aria-label": "Deploy from Hugging Face" });

  const card = (m) => {
    const deploy = gardenDeployAction(m, again, fail);
    const status = gardenStatusOf(m);
    const local = m.deployable && m.localState;
    const button = el("button", {
      class: "primary", text: "Deploy", "data-action": "deploy",
      disabled: deploy ? null : true,
      "aria-describedby": m.unavailable ? `garden-why-${m.id}` : null,
    });
    if (deploy) button.addEventListener("click", deploy);
    return el("article", { class: "card garden-card", "data-model": m.id, "data-deployable": m.deployable ? "true" : "false" },
      el("h2", { text: m.name }),
      el("p", { class: "garden-provider", text: `${m.provider} · ${m.collection}` }),
      el("p", { text: m.summary }),
      el("p", { class: "garden-tags" },
        ...(m.tasks || []).map((t) => el("span", { class: "garden-tag", text: t }))),
      el("dl", {},
        el("dt", { text: "Licence" }), el("dd", { text: m.licence }),
        local ? el("dt", { text: "Runs locally as" }) : null,
        local ? el("dd", { class: "mono", text: m.localState.id }) : null),
      status,
      el("div", { class: "garden-actions" }, button,
        el("a", { href: m.docs, target: "_blank", rel: "noopener noreferrer", text: "Model card on Google Cloud" }),
        local ? el("a", { href: AP_PATHS.localModels, text: "Local models" }) : null),
      m.unavailable ? el("p", { class: "unavailable", id: `garden-why-${m.id}`, text: m.unavailable }) : null);
  };

  const draw = () => {
    const text = search.value.trim();
    const shown = data.models.filter((m) => gardenMatches(m, chosen, text));
    setChildren(grid, ...shown.map(card));
    count.textContent = `${shown.length} of ${data.models.length} models`;
    const url = new URL(location.href);
    for (const f of data.filters) {
      const key = GARDEN_FILTER_KEY[f.name];
      url.searchParams.delete(key);
      for (const v of chosen[f.name]) url.searchParams.append(key, v);
    }
    if (text) url.searchParams.set("q", text); else url.searchParams.delete("q");
    history.replaceState(history.state, "", url);
  };
  search.addEventListener("input", draw);

  const filters = el("aside", { class: "garden-filters", "aria-label": "Filters" },
    el("h2", { text: "Filters" }),
    ...data.filters.map((f) => el("fieldset", { "data-filter": f.name },
      el("legend", { text: f.name }),
      ...f.values.map((v) => {
        const id = `garden-f-${GARDEN_FILTER_KEY[f.name]}-${v.replace(/[^A-Za-z0-9]+/g, "-")}`;
        const box = el("input", { type: "checkbox", id, value: v, checked: chosen[f.name].has(v) ? true : null });
        box.addEventListener("change", () => {
          if (box.checked) chosen[f.name].add(v); else chosen[f.name].delete(v);
          draw();
        });
        return el("div", { class: "garden-filter" }, box, el("label", { for: id, text: v }));
      }))));

  setChildren(view, header(),
    el("div", { class: "card" }, el("p", { class: "unavailable", id: "garden-note", text: data.note })),
    message,
    el("div", { class: "garden" }, filters,
      el("div", { class: "garden-main" }, search, count, grid)),
    hfPanel);
  draw();
  drawHFPanel(hfPanel);

  if (data.models.some((m) => m.localState && m.localState.state === "downloading")) {
    GARDEN_POLL = setTimeout(() => {
      if (location.pathname === AP_PATHS.modelGarden) again();
    }, 1000);
  }
}

async function renderAgentGarden(view) {
  const header = () => pageHeader("Agent Garden",
    "Agents, samples and tools to start an agent from, built with the Agent Development Kit");
  let data;
  try {
    data = await api("/api/ai/agent-garden");
  } catch (err) {
    return setChildren(view, header(), errorState("Agent Garden unavailable", String(err.message), () => renderAgentGarden(view)));
  }
  const kinds = ["Agents", "Samples", "Tools"].filter((k) => data.agents.some((a) => a.kind + "s" === k));
  const params = new URLSearchParams(location.search);
  let kind = kinds.includes(params.get("tab")) ? params.get("tab") : kinds[0];
  const grid = el("div", { class: "garden-cards", id: "agent-cards" });
  const tabs = kinds.map((k) => el("button", { role: "tab", class: "tab", id: `agent-tab-${k}`, text: k,
    onclick: () => { kind = k; draw(); } }));
  const card = (a) => el("article", { class: "card garden-card", "data-agent": a.id },
    el("h2", { text: a.name }),
    el("p", { class: "garden-provider", text: [a.kind, a.language].filter(Boolean).join(" · ") }),
    el("p", { text: a.summary }),
    el("p", { class: "garden-tags" }, ...(a.tags || []).map((t) => el("span", { class: "garden-tag", text: t }))),
    el("div", { class: "garden-actions" },
      el("button", { class: "primary", text: "Deploy", "data-action": "deploy", disabled: true,
        "aria-describedby": `agent-why-${a.id}` }),
      el("a", { href: a.source, target: "_blank", rel: "noopener noreferrer", text: "View code" })),
    el("p", { class: "unavailable", id: `agent-why-${a.id}`,
      text: "Deploy is disabled: it deploys to Agent Runtime (formerly Agent Engine), which CloudBurrow does not serve." }));
  const draw = () => {
    for (const t of tabs) {
      const on = t.textContent === kind;
      t.setAttribute("aria-selected", on ? "true" : "false");
      t.classList.toggle("is-selected", on);
    }
    setChildren(grid, ...data.agents.filter((a) => a.kind + "s" === kind).map(card));
    const url = new URL(location.href);
    url.searchParams.set("tab", kind);
    history.replaceState(history.state, "", url);
  };
  setChildren(view, header(),
    el("div", { class: "card" }, el("p", { class: "unavailable", id: "agent-garden-note", text: data.note })),
    el("div", { class: "tab-strip", role: "tablist", "aria-label": "Agent Garden sections" }, ...tabs),
    grid);
  draw();
}

// The Agent Platform overview (#1839): every page of Google's navigation,
// under its group, saying which ones run on this instance. A page whose
// backend is off here (local AI not started, no Cloud Run) says that,
// which is a different thing from a page CloudBurrow does not serve at all.
function renderAgentPlatformOverview(view) {
  const available = new Set(SERVICES.map((s) => s.id));
  const pages = pagesOf("vertexai").filter((p) => p.path !== AP_PATHS.overview);
  const groups = new Map();
  for (const p of pages) {
    const g = p.group || "Agent Platform";
    if (!groups.has(g)) groups.set(g, []);
    groups.get(g).push(p);
  }
  const stateOfPage = (p) => p.notServed ? ["absent", "Not available locally"]
    : (!p.service || available.has(p.service)) ? ["served", "Runs on this instance"]
    : ["off", "Off on this instance: its local backend was not started"];
  setChildren(view,
    pageHeader("Agent Platform", "Gemini Enterprise Agent Platform (formerly Vertex AI): what runs on this instance"),
    el("div", { class: "card" }, el("p", { class: "unavailable",
      text: "Google renamed Vertex AI to Gemini Enterprise Agent Platform in April 2026. The API is unchanged: " +
        "clients still call aiplatform.googleapis.com with the same resource names, so they reach this instance " +
        "through the same endpoint override as before." })),
    el("div", { class: "cards", id: "ap-overview" }, ...[...groups].map(([g, list]) =>
      el("section", { class: "card" }, el("h2", { text: g }),
        el("ul", { class: "ap-pages" }, ...list.map((p) => {
          const [state, label] = stateOfPage(p);
          return el("li", { "data-state": state },
            el("a", { href: p.path + scopeSearch(), text: p.title }),
            el("span", { class: "unavailable", text: ` — ${label}` }));
        }))))));
}

// renderNotServed is every Agent Platform page CloudBurrow has no backend
// for. It names what the page does on Google Cloud and the API behind it,
// and says plainly that this instance does not serve it, rather than
// drawing an empty table that would read as "you have none".
function renderNotServed(view, match) {
  setChildren(view, pageHeader(match.title, match.about || ""),
    el("div", { class: "card", id: "not-served" },
      el("h2", { text: "Not available locally" }),
      el("p", { text: `CloudBurrow does not serve ${match.title}. ` +
        (match.api ? `Its API, ${match.api}, is not emulated, so there is nothing to list or create here, ` +
          "and a client calling it gets an error rather than an empty answer." :
          "There is no local backend for this page.") }),
      match.instead ? el("p", { text: match.instead }) : null,
      match.docs ? el("p", {}, el("a", { href: match.docs, target: "_blank", rel: "noopener noreferrer",
        text: `${match.title} on Google Cloud` })) : null));
}

async function renderPredict(view) {
  stopPrediction();
  const header = () => pageHeader(PREDICT_TITLE, PREDICT_SUBTITLE);
  setChildren(view, header(), loadingState(3));

  const project = currentProject();
  let status;
  try {
    status = await api("/api/ai/predict?" + new URLSearchParams({ project }));
  } catch (err) {
    return setChildren(view, header(),
      errorState("Prediction endpoints unavailable", String(err.message), () => renderPredict(view)));
  }

  const notServed = el("details", { class: "card", id: "predict-not-served" },
    el("summary", { text: `Not served by CloudBurrow (${(status.notServed || []).length})` }),
    el("p", { text: "Predict calls the container directly, through the cluster ingress; Explain goes through " +
      "the local AI endpoint's PredictionService. These Agent Platform surfaces are not implemented, so this page " +
      "offers none of them:" }),
    el("ul", {}, ...(status.notServed || []).map((n) => el("li", { text: n }))));

  if (!status.configured) {
    return setChildren(view, header(), emptyState("Online prediction is not available", status.note || ""), notServed);
  }
  if (status.unavailable && !(status.endpoints || []).length) {
    return setChildren(view, header(),
      errorState("Prediction endpoints unavailable", status.unavailable, () => renderPredict(view)), notServed);
  }
  const endpoints = status.endpoints || [];
  if (!endpoints.length) {
    return setChildren(view, header(),
      el("div", { class: "state", id: "predict-empty" },
        el("h2", { text: "No prediction endpoint is deployed" }),
        el("p", { text: status.deploy || "" }),
        el("a", { class: "button secondary", href: "/run/create" + location.search, text: "Deploy container" })),
      notServed);
  }

  const select = el("select", { id: "predict-endpoint", "aria-describedby": "predict-endpoint-help" },
    ...endpoints.map((e) => el("option", { value: e.name, text: `${e.id} (${e.state})` })));
  const facts = el("dl", { id: "predict-endpoint-facts" });
  const drawFacts = () => {
    const e = endpoints.find((x) => x.name === select.value) || endpoints[0];
    const rows = [["Resource", e.name], ["State", e.state], ["Predict URL", e.predictUrl || "none yet"],
      ["Predict route", e.predictRoute], ["Health route", e.healthRoute],
      ["gRPC port", e.grpcPort ? `${e.grpcPort} (h2c)` : "none (the model declares no grpcPorts)"]];
    if (e.message) rows.push(["Detail", e.message]);
    setChildren(facts, ...rows.flatMap(([k, v]) =>
      [el("dt", { text: k }), el("dd", { class: k === "State" || k === "Detail" ? null : "mono", text: v })]));
    // DirectRawPredict is offered only on a model that serves gRPC (#1477).
    directRaw.hidden = !e.grpcPort;
  };
  select.addEventListener("change", drawFacts);

  const instances = el("textarea", { id: "predict-instances", rows: "6", spellcheck: "false",
    class: "mono", "aria-describedby": "predict-instances-help" });
  instances.value = "[\n  \n]";
  const parameters = el("textarea", { id: "predict-parameters", rows: "3", spellcheck: "false",
    class: "mono", "aria-describedby": "predict-parameters-help" });
  const formError = el("p", { class: "form-error", role: "alert", id: "predict-error", hidden: true });
  const send = el("button", { class: "primary", id: "predict-send", text: "Predict" });
  const cancel = el("button", { class: "secondary", id: "predict-cancel", text: "Cancel", disabled: "disabled" });
  const timing = el("span", { class: "unavailable", id: "predict-timing", text: "" });

  const httpStatus = el("p", { id: "predict-status" });
  const contract = el("p", { class: "unavailable", id: "predict-contract", role: "alert", hidden: true });
  const requestText = el("pre", { class: "mono", id: "predict-request" });
  const responseText = el("pre", { class: "mono pg-output", id: "predict-response", "aria-live": "polite" });
  const result = el("section", { class: "card", id: "predict-result", hidden: true, "aria-labelledby": "predict-result-title" },
    el("h2", { id: "predict-result-title", text: "Response" }),
    httpStatus, contract, responseText,
    el("h3", { text: "Request sent" }), requestText);

  // Explain (#1454): Sampled Shapley attributions, computed by the local AI
  // endpoint's PredictionService from this container's predictions. The
  // explanation spec is the request's explanation_spec_override: the path
  // count, and each feature to attribute with its input_baselines.
  const pathCount = el("input", { id: "explain-path-count", type: "number", min: "1", max: "50", step: "1",
    value: "10", "aria-describedby": "explain-path-count-help" });
  const baselines = el("textarea", { id: "explain-baselines", rows: "3", spellcheck: "false",
    class: "mono", "aria-describedby": "explain-baselines-help" });
  baselines.value = "{\n  \"x\": []\n}";
  const explainSend = el("button", { class: "secondary", id: "explain-send", text: "Explain" });
  if (!status.explain) explainSend.disabled = true;
  const explainStatus = el("p", { id: "explain-status" });
  const explainError = el("p", { class: "unavailable", id: "explain-error", role: "alert", hidden: true });
  const attributions = el("div", { id: "explain-attributions" });
  const explainRequestText = el("pre", { class: "mono", id: "explain-request" });
  const explainResponseText = el("pre", { class: "mono pg-output", id: "explain-response" });
  const explainResult = el("section", { class: "card", id: "explain-result", hidden: true, "aria-labelledby": "explain-result-title" },
    el("h2", { id: "explain-result-title", text: "Explanation" }),
    explainStatus, explainError, attributions,
    el("h3", { text: "Response" }), explainResponseText,
    el("h3", { text: "Request sent" }), explainRequestText);

  const drawAttributions = (r) => {
    const blocks = (r.explanations || []).map((ex) => {
      const rows = (ex.attributions || []).map((a) => {
        const out = a.outputIndex && a.outputIndex.length ? `Output ${a.outputIndex.join(",")}` : "Output";
        return el("div", { class: "explain-attribution" },
          el("p", { class: "mono", text: `${out}: baseline ${a.baselineOutputValue} → instance ${a.instanceOutputValue}` }),
          el("table", { class: "table explain-features" },
            el("thead", {}, el("tr", {}, el("th", { text: "Feature" }), el("th", { text: "Attribution" }))),
            el("tbody", {}, ...(a.features || []).map((f) =>
              el("tr", {}, el("td", { class: "mono", text: f.feature }),
                el("td", { class: "mono", text: JSON.stringify(f.attribution) }))))));
      });
      return el("div", { class: "explain-instance" },
        el("h3", { text: `Instance ${ex.instance}` + (ex.prediction !== undefined ? ` — prediction ${JSON.stringify(ex.prediction)}` : "") }),
        ...rows);
    });
    setChildren(attributions, ...blocks);
  };

  const runExplain = async () => {
    formError.hidden = true;
    stopPrediction();
    const controller = new AbortController();
    PREDICT_ABORT = controller;
    send.disabled = true;
    explainSend.disabled = true;
    cancel.disabled = false;
    timing.textContent = "explaining…";
    try {
      const res = await fetch("/api/ai/predict/explain?" + new URLSearchParams({ project }), {
        method: "POST",
        headers: { "Content-Type": "application/json", Accept: "application/json" },
        body: JSON.stringify({ endpoint: select.value, instances: instances.value, parameters: parameters.value,
          pathCount: Number(pathCount.value), baselines: baselines.value }),
        signal: controller.signal,
      });
      const text = await res.text();
      if (!res.ok) {
        formError.textContent = errorMessageOf(text) || `HTTP ${res.status}`;
        formError.hidden = false;
        timing.textContent = "";
        return;
      }
      const r = JSON.parse(text);
      setChildren(explainStatus,
        el("span", { class: "status", "data-state": r.status >= 200 && r.status < 300 ? "ok" : "error" },
          el("span", { text: `HTTP ${r.status}` + (r.errorStatus ? ` ${r.errorStatus}` : "") })),
        el("span", { class: "mono", text: r.deployedModelId ? ` deployedModelId ${r.deployedModelId}` : "" }));
      explainError.hidden = !r.error;
      explainError.textContent = r.error || "";
      drawAttributions(r);
      explainResponseText.textContent = r.body + (r.truncated ? "\n[truncated]" : "");
      explainRequestText.textContent = `POST ${r.url}\n${r.request}`;
      timing.textContent = `${r.durationMs} ms`;
      result.hidden = true;
      explainResult.hidden = false;
    } catch (err) {
      if (err.name === "AbortError") {
        timing.textContent = "cancelled";
      } else {
        formError.textContent = String(err.message);
        formError.hidden = false;
        timing.textContent = "";
      }
    } finally {
      send.disabled = false;
      explainSend.disabled = !status.explain;
      cancel.disabled = true;
      if (PREDICT_ABORT === controller) PREDICT_ABORT = null;
    }
  };
  explainSend.addEventListener("click", runExplain);

  const run = async () => {
    formError.hidden = true;
    stopPrediction();
    const controller = new AbortController();
    PREDICT_ABORT = controller;
    send.disabled = true;
    cancel.disabled = false;
    timing.textContent = "sending…";
    try {
      const res = await fetch("/api/ai/predict?" + new URLSearchParams({ project }), {
        method: "POST",
        headers: { "Content-Type": "application/json", Accept: "application/json" },
        body: JSON.stringify({ endpoint: select.value, instances: instances.value, parameters: parameters.value }),
        signal: controller.signal,
      });
      const text = await res.text();
      if (!res.ok) {
        // The console could not send it: its own reason, not a response.
        formError.textContent = errorMessageOf(text) || `HTTP ${res.status}`;
        formError.hidden = false;
        timing.textContent = "";
        return;
      }
      const r = JSON.parse(text);
      setChildren(httpStatus,
        el("span", { class: "status", "data-state": r.status >= 200 && r.status < 300 ? "ok" : "error" },
          el("span", { text: `HTTP ${r.status}` })),
        el("span", { class: "mono", text: ` ${r.contentType || ""}` }));
      contract.hidden = !r.contractError;
      contract.textContent = r.contractError ? `Contract violation: ${r.contractError}` : "";
      // Verbatim: the container's body as it sent it, not reformatted.
      responseText.textContent = r.body + (r.truncated ? "\n[truncated]" : "");
      requestText.textContent = `POST ${r.url}\n${r.request}`;
      timing.textContent = `${r.durationMs} ms`;
      explainResult.hidden = true;
      result.hidden = false;
    } catch (err) {
      if (err.name === "AbortError") {
        timing.textContent = "cancelled";
      } else {
        formError.textContent = String(err.message);
        formError.hidden = false;
        timing.textContent = "";
      }
    } finally {
      send.disabled = false;
      cancel.disabled = true;
      if (PREDICT_ABORT === controller) PREDICT_ABORT = null;
    }
  };
  send.addEventListener("click", run);
  cancel.addEventListener("click", stopPrediction);

  // DirectRawPredict (#1477): method_name and the input bytes, sent to the
  // model's gRPC server as the local AI endpoint's DirectRawPredict sends
  // them; the output bytes or the container's status as they came back.
  const rawMethod = el("input", { id: "predict-raw-method", class: "mono", type: "text", spellcheck: "false",
    placeholder: "/tensorflow.serving.PredictionService/Predict", "aria-describedby": "predict-raw-method-help" });
  const rawInput = el("textarea", { id: "predict-raw-input", rows: "3", spellcheck: "false", class: "mono",
    "aria-describedby": "predict-raw-input-help" });
  const rawError = el("p", { class: "form-error", role: "alert", id: "predict-raw-error", hidden: true });
  const rawSend = el("button", { class: "primary", id: "predict-raw-send", text: "DirectRawPredict" });
  const rawTiming = el("span", { class: "unavailable", id: "predict-raw-timing", text: "" });
  const rawStatus = el("p", { id: "predict-raw-status" });
  const rawOutput = el("pre", { class: "mono pg-output", id: "predict-raw-output", "aria-live": "polite" });
  const rawResult = el("div", { id: "predict-raw-result", hidden: true }, rawStatus, rawOutput);
  rawSend.addEventListener("click", async () => {
    rawError.hidden = true;
    rawSend.disabled = true;
    rawTiming.textContent = "sending…";
    try {
      const res = await fetch("/api/ai/predict/direct-raw?" + new URLSearchParams({ project }), {
        method: "POST",
        headers: { "Content-Type": "application/json", Accept: "application/json" },
        body: JSON.stringify({ endpoint: select.value, methodName: rawMethod.value, input: rawInput.value }),
      });
      const text = await res.text();
      if (!res.ok) {
        rawError.textContent = errorMessageOf(text) || `HTTP ${res.status}`;
        rawError.hidden = false;
        rawTiming.textContent = "";
        return;
      }
      const r = JSON.parse(text);
      setChildren(rawStatus,
        el("span", { class: "status", "data-state": r.code === "OK" ? "ok" : "error" }, el("span", { text: r.code })),
        el("span", { class: "mono", text: ` ${r.method} on ${r.authority}` }));
      rawOutput.textContent = r.code === "OK"
        ? `output (base64): ${r.output}\noutput (hex): ${r.outputHex}`
        : r.message;
      rawTiming.textContent = `${r.durationMs} ms`;
      rawResult.hidden = false;
    } catch (err) {
      rawError.textContent = String(err.message);
      rawError.hidden = false;
      rawTiming.textContent = "";
    } finally {
      rawSend.disabled = false;
    }
  });
  const directRaw = el("section", { class: "card", id: "predict-direct-raw", "aria-labelledby": "predict-direct-raw-title" },
    el("h2", { id: "predict-direct-raw-title", text: "DirectRawPredict" }),
    el("p", { class: "form-help", text: "This model declares grpcPorts, so its container's gRPC server is reached " +
      "on its h2c port. The input bytes are the method's request message; the output is its response message." }),
    el("div", { class: "form-row" },
      el("label", { for: "predict-raw-method", text: "Method name" }), rawMethod,
      el("p", { class: "form-help", id: "predict-raw-method-help", text:
        "The fully qualified gRPC method, /namespace.Service/Method." })),
    el("div", { class: "form-row" },
      el("label", { for: "predict-raw-input", text: "Input (base64)" }), rawInput,
      el("p", { class: "form-help", id: "predict-raw-input-help", text:
        "The serialized request message, base64. Leave empty to send the empty message." })),
    rawError,
    el("div", { class: "form-actions" }, rawSend, rawTiming),
    rawResult);
  drawFacts();

  setChildren(view, header(),
    el("section", { class: "card", id: "predict-form", "aria-label": "Prediction request" },
      el("div", { class: "form-row" },
        el("label", { for: "predict-endpoint", text: "Endpoint" }), select,
        el("p", { class: "form-help", id: "predict-endpoint-help", text:
          "Cloud Run services configured with the Vertex prediction contract (an AIP_* variable). " +
          `Reached through the cluster ingress${status.ingress ? " at " + status.ingress : ""}.` })),
      facts,
      status.unavailable ? el("p", { class: "unavailable", text: status.unavailable }) : null,
      el("div", { class: "form-row" },
        el("label", { for: "predict-instances", text: "Instances" }), instances,
        el("p", { class: "form-help", id: "predict-instances-help", text:
          "A JSON array, one element per instance, in whatever shape the container expects." })),
      el("div", { class: "form-row" },
        el("label", { for: "predict-parameters", text: "Parameters" }), parameters,
        el("p", { class: "form-help", id: "predict-parameters-help", text:
          "Optional JSON, sent as the request's parameters field. Leave empty to send none." })),
      el("fieldset", { class: "form-row", id: "explain-spec" },
        el("legend", { text: "Explanation spec (Explain, Sampled Shapley)" }),
        el("label", { for: "explain-path-count", text: "Path count" }), pathCount,
        el("p", { class: "form-help", id: "explain-path-count-help", text:
          "sampled_shapley_attribution.path_count: the feature orderings sampled, 1 to 50." }),
        el("label", { for: "explain-baselines", text: "Feature baselines" }), baselines,
        el("p", { class: "form-help", id: "explain-baselines-help", text:
          "metadata.inputs: a JSON object naming each feature to attribute (a key of a struct instance, or any one " +
          "name for a non-struct instance) with its input_baselines list; [] means a baseline of 0 for a number." }),
        status.explain ? null : el("p", { class: "unavailable", id: "explain-unavailable", text: status.explainNote || "" })),
      formError,
      el("div", { class: "form-actions" }, send, explainSend, cancel, timing)),
    result,
    explainResult,
    directRaw,
    notServed);
}

// The Request Log (#291): API calls CloudBurrow served, from the recorder
// /admin/events reads. The backlog is fetched once; new calls arrive over
// /api/stream?stream=requests. Services whose traffic is forwarded straight
// to their own server are named as unobservable rather than shown as an empty
// list, which would read as "nothing called them".
const MAX_REQUEST_ROWS = 500;

async function renderRequests(view) {
  const params = new URLSearchParams(location.search);
  const header = () => pageHeader("Request Log", "API calls this instance served, newest first.");
  const service = el("select", { id: "req-service", "aria-label": "Filter by service" },
    ...[["", "All services"], ["tasks", "Cloud Tasks"], ["secretmanager", "Secret Manager"], ["run", "Cloud Run"]]
      .map(([v, t]) => el("option", { value: v, text: t })));
  const code = el("input", { class: "filter", type: "search", id: "req-code",
    placeholder: "Code, e.g. NOT_FOUND or 404", "aria-label": "Filter by status code" });
  const project = el("input", { class: "filter", type: "search", id: "req-project",
    placeholder: "Project", "aria-label": "Filter by project" });
  service.value = params.get("service") || "";
  code.value = params.get("code") || "";
  project.value = params.get("project") || "";

  const query = () => {
    const q = new URLSearchParams();
    if (service.value) q.set("service", service.value);
    if (code.value.trim()) q.set("code", code.value.trim().toUpperCase());
    if (project.value.trim()) q.set("project", project.value.trim());
    return q;
  };
  const status = el("span", { class: "status", id: "req-stream-status", text: "connecting…" });
  const body = el("tbody", { id: "req-rows" });
  const table = el("table", { class: "table", id: "req-table" },
    el("thead", {}, el("tr", {},
      ...["Time", "Service", "Method", "Resource", "Code", "Duration"].map((h) => el("th", { scope: "col", text: h })))),
    body);
  const notice = el("div", { class: "notice", id: "req-unobserved" });
  const empty = el("p", { class: "muted", id: "req-empty", text: "No requests yet. Calls to Cloud Tasks, Secret Manager and Cloud Run appear here as they are served." });

  const row = (r) => el("tr", { class: r.code === "OK" || /^2/.test(r.code) ? "" : "is-error" },
    el("td", { text: new Date(r.time).toLocaleTimeString() }),
    el("td", { text: r.service }),
    el("td", { text: r.method.slice(r.method.lastIndexOf("/") + 1), title: r.method }),
    el("td", { text: r.resource || "—" }),
    el("td", { text: r.code }),
    el("td", { text: `${r.duration_ms} ms` }));

  setChildren(view, header(),
    el("div", { class: "actions" }, service, code, project, status),
    // In its own scrolling region, as every other table is, so a narrow
    // window scrolls the table rather than the page (#1604).
    notice, empty, el("div", { class: "table-wrap" }, table));

  const load = async () => {
    stopStream();
    const q = query();
    history.replaceState(null, "", q.toString() ? `/requests?${q}` : "/requests");
    let data;
    try {
      data = await api(`/api/requests?${q}`);
    } catch (err) {
      setChildren(view, header(), errorState("Request Log unavailable", String(err.message), () => renderRequests(view)));
      return;
    }
    setChildren(body, ...(data.requests || []).map(row));
    empty.hidden = (data.requests || []).length > 0;
    const unobserved = data.unobserved || [];
    notice.hidden = unobserved.length === 0;
    setChildren(notice, ...unobserved.map((u) =>
      el("p", { class: "unobserved", text: `${u.service}: ${u.label}` })));

    q.set("stream", "requests");
    const stream = new EventSource(`/api/stream?${q}`);
    STREAM = stream;
    stream.addEventListener("open", () => { status.textContent = "live"; });
    stream.addEventListener("error", () => { status.textContent = "reconnecting…"; });
    stream.addEventListener("request", (ev) => {
      const r = JSON.parse(ev.data);
      body.prepend(row(r));
      empty.hidden = true;
      while (body.children.length > MAX_REQUEST_ROWS) body.lastElementChild.remove();
    });
  };
  for (const c of [service, code, project]) c.addEventListener("change", load);
  await load();
}

// Fault injection (#800): the rules /admin/faults holds, a form that takes
// exactly the fields a rule has, Delete per rule and Clear all, which
// services accept a rule and why the others are refused, and the faults the
// recorder saw, refreshed while the screen is open.
//
// Every call goes to the admin API, in process, through /api/faults, with
// the admin token the developer pastes here. The console adds no token of its
// own: a workload in the cluster can reach this console on Docker Desktop, so
// one that it added would make the console a way around the token (#553).
const ADMIN_TOKEN_KEY = "cb-admin-token";
const FAULTS_POLL_MS = 2000;
let FAULTS_POLL_TIMER = null;
// Held in memory as well, for a browser that refuses session storage.
let ADMIN_TOKEN = "";

function stopFaultsPoll() {
  if (FAULTS_POLL_TIMER) { clearInterval(FAULTS_POLL_TIMER); FAULTS_POLL_TIMER = null; }
}

function adminToken() {
  if (ADMIN_TOKEN) return ADMIN_TOKEN;
  try { ADMIN_TOKEN = sessionStorage.getItem(ADMIN_TOKEN_KEY) || ""; } catch { /* private mode */ }
  return ADMIN_TOKEN;
}

function setAdminToken(value) {
  ADMIN_TOKEN = value;
  try {
    if (value) sessionStorage.setItem(ADMIN_TOKEN_KEY, value);
    else sessionStorage.removeItem(ADMIN_TOKEN_KEY);
  } catch { /* private mode */ }
}

// faultsApi is api() with the admin token the page holds.
function faultsApi(path, method = "GET", body) {
  const headers = {};
  const token = adminToken();
  if (token) headers.Authorization = `Bearer ${token}`;
  if (body) headers["Content-Type"] = "application/json";
  return api(path, {
    method, headers,
    deadline: method === "GET" ? READ_DEADLINE_MS : WRITE_DEADLINE_MS,
    body: body ? JSON.stringify(body) : undefined,
  });
}

const FAULTS_TITLE = "Fault injection";
const FAULTS_SUBTITLE =
  "Rules that make calls to the services this instance serves fail or slow down, " +
  "so a client's retry and deadline handling can be exercised with the SDK it uses.";

async function renderFaults(view) {
  const header = () => pageHeader(FAULTS_TITLE, FAULTS_SUBTITLE);
  setChildren(view, header(), loadingState(4));
  let data;
  try {
    data = await faultsApi("/api/faults");
  } catch (err) {
    if (location.pathname !== "/faults") return;
    if (err.status === 401) return renderFaultsToken(view, err);
    setChildren(view, header(), errorState("Fault injection unavailable", err.message, () => renderFaults(view)));
    return;
  }
  if (location.pathname !== "/faults") return;

  const interposed = data.interposed || [];
  const refused = data.refused || [];
  const status = el("span", { class: "status", id: "fault-status", text: "live" });
  const add = el("button", { class: "primary", id: "fault-add", text: "Add rule",
    disabled: interposed.length ? null : "disabled",
    title: interposed.length ? null : "No service on this instance accepts fault rules" });
  const clear = el("button", { class: "secondary danger", id: "fault-clear", text: "Clear all" });
  const rulesBody = el("tbody", { id: "fault-rule-rows" });
  const rulesEmpty = el("p", { class: "muted", id: "fault-rules-empty",
    text: "No rules. Calls are served normally." });
  const recentBody = el("tbody", { id: "fault-recent-rows" });
  const recentEmpty = el("p", { class: "muted", id: "fault-recent-empty",
    text: "No faults injected yet. Each one a rule injects appears here." });

  const faultOf = (r) => {
    if (r.httpStatus) return `HTTP ${r.httpStatus} (${r.code})`;
    return r.code || "none: delay only";
  };
  const ruleRow = (r) => el("tr", { "data-rule": r.id },
    el("td", { text: r.id }),
    el("td", { text: r.service }),
    el("td", { text: r.method || "*" }),
    el("td", { text: r.project || "every project" }),
    el("td", { text: String(r.probability) }),
    el("td", { text: faultOf(r) }),
    el("td", { text: r.latencyMs ? `${r.latencyMs} ms` : "—" }),
    el("td", { text: r.count ? `${r.remaining} of ${r.count}` : "no limit" }),
    el("td", { text: r.seed === undefined || r.seed === null ? "—" : String(r.seed) }),
    el("td", { text: String(r.injected) }),
    el("td", {}, el("button", { class: "secondary danger", text: "Delete",
      "aria-label": `Delete rule ${r.id}`, onclick: () => deleteRule(r) })));
  const recentRow = (e) => el("tr", { class: e.code ? "is-error" : "" },
    el("td", { text: new Date(e.time).toLocaleTimeString() }),
    el("td", { text: e.service }),
    el("td", { text: e.method.slice(e.method.lastIndexOf("/") + 1), title: e.method }),
    el("td", { text: e.rule }),
    el("td", { text: [e.code, e.latency_ms ? `after ${e.latency_ms} ms` : ""].filter(Boolean).join(" ") || "delayed" }));

  let rules = [];
  const draw = (d) => {
    rules = d.faults || [];
    setChildren(rulesBody, ...rules.map(ruleRow));
    rulesEmpty.hidden = rules.length > 0;
    clear.disabled = rules.length === 0;
    const recent = d.recent || [];
    setChildren(recentBody, ...recent.map(recentRow));
    recentEmpty.hidden = recent.length > 0;
  };

  const refresh = async () => {
    try {
      const d = await faultsApi("/api/faults");
      if (location.pathname !== "/faults") return;
      draw(d);
      status.textContent = "live";
    } catch (err) {
      if (location.pathname !== "/faults") return;
      if (err.status === 401) { stopFaultsPoll(); renderFaultsToken(view, err); return; }
      status.textContent = `refresh failed: ${err.message}`;
    }
  };

  const deleteRule = (r) => confirmDestructive({
    title: `Delete rule ${r.id}`,
    detail: `${r.service} ${r.method || "*"}, ${faultOf(r)}. Matching calls are served normally again.`,
    confirmWord: r.id,
    onConfirm: async () => {
      const op = recordOperation(`Delete fault rule ${r.id}`);
      try {
        await faultsApi(`/api/faults?id=${encodeURIComponent(r.id)}`, "DELETE");
        op.succeeded(r.id);
      } catch (err) {
        op.failed(err.message);
        throw err;
      }
      notify(`Deleted rule ${r.id}`);
      await refresh();
    },
  });

  clear.addEventListener("click", () => confirmDestructive({
    title: "Delete every fault rule",
    detail: `${rules.length} ${rules.length === 1 ? "rule" : "rules"}, on every service. Calls are served normally again.`,
    confirmWord: "all",
    onConfirm: async () => {
      const op = recordOperation("Clear fault rules");
      try {
        await faultsApi("/api/faults?all=true", "DELETE");
        op.succeeded("");
      } catch (err) {
        op.failed(err.message);
        throw err;
      }
      notify("Deleted every fault rule");
      await refresh();
    },
  }));
  add.addEventListener("click", () => openFaultForm(data, async (rule) => {
    notify(`Added rule ${rule.id}`);
    await refresh();
  }));

  const services = el("div", { class: "card", id: "fault-services" },
    el("h2", { text: "Services" }),
    el("p", { id: "fault-interposed", text: interposed.length
      ? `Rules apply to ${interposed.join(", ")}: the services this instance serves in its own process.`
      : "No service on this instance accepts fault rules." }),
    refused.length
      ? el("ul", { class: "unmeasured", id: "fault-refused" }, ...refused.map((r) =>
          el("li", { "data-service": r.service }, el("strong", { text: r.service }), el("span", { text: `: ${r.reason}` }))))
      : null);

  setChildren(view, header(),
    el("div", { class: "actions" }, add, clear, status),
    services,
    el("div", { class: "card" },
      el("h2", { text: "Rules" }),
      rulesEmpty,
      el("table", { class: "table", id: "fault-rules" },
        el("thead", {}, el("tr", {},
          ...["Rule", "Service", "Method", "Project", "Probability", "Fault", "Latency", "Remaining", "Seed", "Injected", ""]
            .map((h) => el("th", { scope: "col", text: h })))),
        rulesBody)),
    el("div", { class: "card" },
      el("h2", { text: "Recent faults" }),
      recentEmpty,
      el("table", { class: "table", id: "fault-recent" },
        el("thead", {}, el("tr", {},
          ...["Time", "Service", "Method", "Rule", "Result"].map((h) => el("th", { scope: "col", text: h })))),
        recentBody)));
  draw(data);
  stopFaultsPoll();
  FAULTS_POLL_TIMER = setInterval(refresh, FAULTS_POLL_MS);
}

// renderFaultsToken asks for the admin token, which the admin API refused
// the page without (or with a wrong one).
function renderFaultsToken(view, err) {
  const hadToken = !!adminToken();
  setAdminToken("");
  const file = err.body && err.body.token_file;
  const input = el("input", { id: "fault-token", type: "password", autocomplete: "off", spellcheck: "false" });
  const error = el("p", { class: "form-error", role: "alert", id: "fault-token-error", hidden: !hadToken,
    text: hadToken ? `The admin API refused that token: ${err.message}` : "" });
  const submit = (e) => {
    e.preventDefault();
    const value = input.value.trim();
    if (!value) {
      error.textContent = "Paste the instance's admin token.";
      error.hidden = false;
      input.focus();
      return;
    }
    setAdminToken(value);
    renderFaults(view);
  };
  setChildren(view, pageHeader(FAULTS_TITLE, FAULTS_SUBTITLE),
    el("form", { class: "card", id: "fault-token-form", novalidate: true, onsubmit: submit },
      el("h2", { text: "Admin token required" }),
      el("p", { text: "Fault rules are admin state, so this screen needs the instance's admin token, as " +
        "/admin/faults does. A workload in the cluster can reach this console, so the console adds no token of its own." }),
      file ? el("p", {}, el("span", { text: "The token is the contents of " }), el("code", { text: file }), el("span", { text: "." })) : null,
      error,
      el("div", { class: "form-row" },
        el("label", { for: "fault-token", text: "Admin token" }),
        input,
        el("p", { class: "form-help", text: "Kept for this tab only, and sent with each fault request." })),
      el("div", { class: "form-actions" }, el("button", { type: "submit", class: "primary", text: "Use token" }))));
  input.focus();
  announce("Admin token required");
}

// openFaultForm is the Add rule dialog: exactly the fields a rule has, with
// the services, codes and statuses the admin API says it accepts, so nothing
// is offered that it would refuse. Cancel discards without asking (#783);
// Escape, which can be pressed by accident, asks first.
function openFaultForm(data, onDone) {
  const row = (id, label, control, help) => el("div", { class: "form-row" },
    el("label", { for: id, text: label }), control,
    help ? el("p", { class: "form-help", text: help }) : null);
  const select = (id, options, value) => {
    const s = el("select", { id }, ...options.map(([v, t]) => el("option", { value: v, text: t })));
    s.value = value;
    return s;
  };
  const interposed = data.interposed || [];
  const controls = {
    service: select("fault-service", interposed.map((s) => [s, s]), interposed[0] || ""),
    method: el("input", { id: "fault-method", type: "text", placeholder: "*", autocomplete: "off" }),
    project: el("input", { id: "fault-project", type: "text", autocomplete: "off" }),
    failWith: select("fault-fail-with",
      [["code", "A gRPC code"], ["httpStatus", "An HTTP status"], ["none", "Nothing: delay only"]], "code"),
    code: select("fault-code", (data.codes || []).map((c) => [c, c]), "UNAVAILABLE"),
    httpStatus: select("fault-http-status", (data.httpStatuses || []).map((c) => [String(c), String(c)]), "503"),
    probability: el("input", { id: "fault-probability", type: "number", min: "0", max: "1", step: "any" }),
    latencyMs: el("input", { id: "fault-latency", type: "number", min: "0", max: "600000", step: "1" }),
    count: el("input", { id: "fault-count", type: "number", min: "0", step: "1" }),
    seed: el("input", { id: "fault-seed", type: "text", inputmode: "numeric", autocomplete: "off" }),
  };
  controls.probability.value = "1";
  controls.latencyMs.value = "0";
  controls.count.value = "0";
  const defaults = Object.fromEntries(Object.entries(controls).map(([k, c]) => [k, c.value]));
  const dirty = () => Object.entries(controls).some(([k, c]) => c.value !== defaults[k]);

  const codeRow = row("fault-code", "Code", controls.code, "The gRPC code the call fails with.");
  const statusRow = row("fault-http-status", "HTTP status", controls.httpStatus,
    "Mapped to its gRPC code the way Google's APIs map it.");
  const showFault = () => {
    codeRow.hidden = controls.failWith.value !== "code";
    statusRow.hidden = controls.failWith.value !== "httpStatus";
  };
  controls.failWith.addEventListener("change", showFault);
  showFault();

  const error = el("p", { class: "form-error", role: "alert", id: "fault-form-error", hidden: true });
  const discard = el("div", { class: "discard-prompt", role: "alert", hidden: true });
  let submitting = false;
  let discarding = false;
  const { dialog, close } = openModal({
    labelledBy: "fault-form-title",
    canClose: (reason) => {
      if (discarding) return true;
      if (submitting) return false;
      if (!dirty()) return true;
      if (reason === "backdrop") return false;
      discard.hidden = false;
      discard.querySelector("button").focus();
      return false;
    },
  });
  setChildren(discard,
    el("span", { text: "Discard your changes?" }),
    el("button", { type: "button", class: "secondary", text: "Keep editing",
      onclick: () => { discard.hidden = true; controls.method.focus(); } }),
    el("button", { type: "button", class: "secondary danger", text: "Discard",
      onclick: () => { discarding = true; close(); } }));
  // Cancel is the decision to discard, so it does not ask again (#783).
  const cancel = el("button", { type: "button", class: "secondary", text: "Cancel",
    onclick: () => { discarding = true; close(); } });
  const primary = el("button", { type: "submit", class: "primary", text: "Add rule" });

  // The limits validate() applies, so an obvious mistake needs no round trip;
  // the admin API still decides, and its message is shown when it refuses.
  const buildRule = () => {
    const rule = { service: controls.service.value };
    const method = controls.method.value.trim();
    const project = controls.project.value.trim();
    if (method) rule.method = method;
    if (project) rule.project = project;
    const p = Number(controls.probability.value);
    if (!(p > 0 && p <= 1)) throw new Error("Probability must be more than 0 and at most 1.");
    rule.probability = p;
    const latency = Number(controls.latencyMs.value || "0");
    if (!Number.isInteger(latency) || latency < 0 || latency > 600000) {
      throw new Error("Latency must be a whole number of milliseconds from 0 to 600000.");
    }
    if (latency) rule.latencyMs = latency;
    const count = Number(controls.count.value || "0");
    if (!Number.isInteger(count) || count < 0) throw new Error("Count must be a whole number, 0 for no limit.");
    if (count) rule.count = count;
    const seed = controls.seed.value.trim();
    if (seed) {
      const n = Number(seed);
      if (!/^-?\d+$/.test(seed) || !Number.isSafeInteger(n)) throw new Error("Seed must be a whole number.");
      rule.seed = n;
    }
    switch (controls.failWith.value) {
      case "code": rule.code = controls.code.value; break;
      case "httpStatus": rule.httpStatus = Number(controls.httpStatus.value); break;
      default:
        if (!latency) throw new Error("A rule that does not fail a call has to delay it: set Latency.");
    }
    return rule;
  };

  const submit = async (e) => {
    e.preventDefault();
    error.hidden = true;
    let rule;
    try {
      rule = buildRule();
    } catch (err) {
      error.textContent = err.message;
      error.hidden = false;
      return;
    }
    submitting = true;
    setBusy(primary, true);
    cancel.disabled = true;
    const op = recordOperation(`Add fault rule on ${rule.service}`);
    try {
      const created = await faultsApi("/api/faults", "POST", rule);
      op.succeeded(created.id);
      submitting = false;
      discarding = true;
      close();
      await onDone(created);
    } catch (err) {
      op.failed(err.message);
      submitting = false;
      setBusy(primary, false);
      cancel.disabled = false;
      error.textContent = err.message;
      error.hidden = false;
    }
  };

  dialog.append(el("form", { class: "modal-body", id: "fault-form", novalidate: true, onsubmit: submit },
    el("h2", { id: "fault-form-title", text: "Add rule" }),
    error,
    row("fault-service", "Service", controls.service, "The services this instance applies rules to."),
    row("fault-method", "Method", controls.method,
      "A glob over the method name, such as AccessSecretVersion or Get*. Empty matches every method."),
    row("fault-project", "Project", controls.project,
      "Only calls whose resource is under projects/{project}. Empty applies to every project."),
    row("fault-fail-with", "Fail with", controls.failWith),
    codeRow,
    statusRow,
    el("div", { class: "form-pair" },
      row("fault-probability", "Probability", controls.probability, "The chance a matching call is faulted, up to 1."),
      row("fault-latency", "Latency (ms)", controls.latencyMs,
        "A delay first. With a code or status the call then fails; without, it proceeds.")),
    el("div", { class: "form-pair" },
      row("fault-count", "Count", controls.count, "Faults before the rule stops; 0 means no limit."),
      row("fault-seed", "Seed", controls.seed, "Makes a probabilistic rule reproducible. Empty for none.")),
    discard,
    el("div", { class: "modal-actions" }, cancel, primary)));
  controls.service.focus();
}

// Request charts on /monitoring (#292): calls CloudBurrow served itself or
// saw pass, from the counters /metrics exposes, sampled on the server's own
// tick: Pub/Sub's and Bigtable's through their fronts (#1266, #1267), and
// Spanner's, Firestore's and Datastore's through the call fronts in their
// pods, a pod's calls as well as the host's (#1288). A service reached by a
// raw port-forward with no front is named as not measured, never charted at
// zero, which would read as "nobody called it".
//
// Beside them, each in-cluster emulator's container CPU and memory, from the
// kubelet's per-pod readings the node charts above come from (#1267).
const EMULATOR_PODS = ["pubsub", "firestore", "datastore", "bigtable", "spanner"];

// emulatorPodSeries picks, per emulator, its pod's readings out of the
// series: a Deployment's pod is named <service>-<hash>-<id>.
function emulatorPodSeries(samples) {
  const out = new Map();
  for (const svc of EMULATOR_PODS) {
    const match = new RegExp(`^${svc}-[a-z0-9]+-[a-z0-9]+$`);
    const points = samples.map((s) => {
      const pod = (s.pods || []).find((p) => match.test(p.name));
      return pod ? { at: s.at, cpu: pod.cpuUsedCores, memory: pod.memoryWorkingSetBytes } : { at: s.at };
    });
    if (points.some((p) => p.cpu !== undefined)) out.set(svc, points);
  }
  return out;
}

async function requestCharts(samples = []) {
  let data;
  try {
    data = await api("/api/metrics/requests");
  } catch (err) {
    return el("div", { class: "card" }, el("h2", { text: "API requests" }),
      el("p", { class: "unavailable", text: `Request metrics unavailable: ${err.message}` }));
  }
  const pct = (v) => (v === null || v === undefined ? null : v * 100);
  const cards = [];
  for (const svc of data.services || []) {
    const pts = svc.points || [];
    const last = [...pts].reverse().find((p) => p.p95_ms !== null && p.p95_ms !== undefined);
    cards.push(
      chartCard(`${svc.service}: requests`, pts.map((p) => ({ value: p.rate })), {
        label: `${svc.service} requests per second`,
        current: pts.length ? `${pts[pts.length - 1].rate.toFixed(2)}/s` : "",
        foot: "Calls per second between samples · /metrics",
      }),
      chartCard(`${svc.service}: errors`, pts.map((p) => ({ value: pct(p.error_rate) })), {
        label: `${svc.service} error rate`, max: 100,
        current: last && last.error_rate !== null ? `${(last.error_rate * 100).toFixed(1)}% failed` : "",
        foot: "Share of calls that failed (any code but OK, or HTTP 4xx and 5xx)",
      }),
      chartCard(`${svc.service}: latency`, pts.map((p) => ({ value: p.p95_ms })), {
        label: `${svc.service} p95 latency in milliseconds`,
        current: last ? `p50 ${last.p50_ms.toFixed(1)} ms · p95 ${last.p95_ms.toFixed(1)} ms` : "",
        foot: "p95 estimated from the histogram, as histogram_quantile does",
      }));
  }
  const containers = [];
  for (const [svc, pts] of emulatorPodSeries(samples)) {
    const last = [...pts].reverse().find((p) => p.cpu !== undefined);
    containers.push(
      chartCard(`${svc}: container CPU`, pts.map((p) => ({ at: p.at, value: p.cpu === undefined ? null : p.cpu })), {
        label: `${svc} pod CPU cores used`,
        current: last ? `${last.cpu.toFixed(3)} vCPU` : "",
        foot: "Kubelet instantaneous usage of the emulator's pod",
      }),
      chartCard(`${svc}: container memory`, pts.map((p) => ({ at: p.at, value: p.memory === undefined ? null : p.memory })), {
        label: `${svc} pod memory working set`,
        current: last ? formatBytes(last.memory) : "",
        foot: "Kubelet working set of the emulator's pod",
      }));
  }
  const unmeasured = data.unmeasured || [];
  return el("div", { class: "card", id: "request-metrics" },
    el("h2", { text: "API requests" }),
    cards.length
      ? el("div", { class: "charts" }, ...cards)
      : el("p", { class: "unavailable", text: data.unavailable ||
          "No API calls yet. Charts appear once there are." }),
    containers.length
      ? el("div", { class: "charts", id: "emulator-containers" }, ...containers)
      : null,
    unmeasured.length
      ? el("ul", { class: "unmeasured" }, ...unmeasured.map((s) =>
          el("li", { text: `${s}: ${data.unmeasured_label}` })))
      : null);
}

// browseBuckets is a gs:// field's Browse (#1845): the project's buckets in
// this instance's Cloud Storage, one of which fills the field as
// gs://BUCKET/. It is drawn over the form it was opened from.
async function browseBuckets(control) {
  const { dialog, close } = openModal({ labelledBy: "browse-title" });
  const body = el("div", { class: "browse-list" }, loadingState(3, { what: "buckets" }));
  dialog.append(el("div", { class: "modal-body" },
    el("h2", { id: "browse-title", text: "Select a bucket" }),
    body,
    el("div", { class: "modal-actions" },
      el("button", { type: "button", class: "secondary", text: "Cancel", onclick: () => close() }))));
  try {
    const list = await api(`/api/resources/storage?project=${encodeURIComponent(currentProject())}`);
    const items = list.items || [];
    if (list.unavailable || list.prompt) return setChildren(body, el("p", { class: "form-error", text: list.unavailable || list.prompt }));
    if (!items.length) return setChildren(body, el("p", { class: "unavailable", text: "This project has no buckets. Create one in Cloud Storage first." }));
    setChildren(body, el("ul", { class: "fs-rows", role: "list" }, ...items.map((b) => el("li", { class: "fs-row" },
      el("button", { type: "button", class: "fs-row-link", text: b.name, onclick: () => {
        control.value = `gs://${b.name}/`;
        control.dispatchEvent(new Event("input", { bubbles: true }));
        control.dispatchEvent(new Event("change", { bubbles: true }));
        close();
        control.focus();
      } })))));
    body.querySelector("button").focus();
  } catch (err) {
    setChildren(body, el("p", { class: "form-error", text: `Cloud Storage cannot be read: ${err.message}` }));
  }
}

// formKMSLocation is the Cloud KMS location of the location a create
// form's fields hold (#1881): its locationType, multiRegion and region, as
// Create a Firestore database has them. A multi-region's keys are in
// Cloud KMS's matching multi-region: us for nam5, europe for eur3
// (https://docs.cloud.google.com/firestore/native/docs/use-cmek).
function formKMSLocation(entries) {
  const value = (name) => {
    const e = entries.find((x) => x.field.name === name);
    return e ? e.control.value : "";
  };
  const loc = value("locationType") === "region" ? value("region") : value("multiRegion");
  return ({ nam5: "us", eur3: "europe" })[loc] || loc;
}

// browseKeys is a Cloud KMS key field's Browse (#1881): the project's
// ENCRYPT_DECRYPT keys in location, from this instance's Cloud KMS, one of
// which fills the field with its resource name.
async function browseKeys(control, location) {
  const { dialog, close } = openModal({ labelledBy: "browse-title" });
  const body = el("div", { class: "browse-list" }, loadingState(3, { what: "keys" }));
  dialog.append(el("div", { class: "modal-body" },
    el("h2", { id: "browse-title", text: "Select a Cloud KMS key" }),
    el("p", { class: "form-help", text: `Keys in ${location}, the database's Cloud KMS location.` }),
    body,
    el("div", { class: "modal-actions" },
      el("button", { type: "button", class: "secondary", text: "Cancel", onclick: () => close() }))));
  const project = currentProject();
  try {
    const list = await api(`/api/resources/kms?project=${encodeURIComponent(project)}`);
    if (list.unavailable || list.prompt) return setChildren(body, el("p", { class: "form-error", text: list.unavailable || list.prompt }));
    const rings = (list.items || []).filter((r) => (r.fields || {}).Location === location);
    const keys = [];
    for (const r of rings) {
      const d = await api(`/api/detail/kms?project=${encodeURIComponent(project)}&name=${encodeURIComponent(r.name)}`);
      const sec = (d.sections || []).find((x) => x.id === "keys");
      for (const k of (sec && sec.listing && sec.listing.items) || []) {
        if ((k.fields || {}).Purpose === "ENCRYPT_DECRYPT") keys.push({ name: `${r.name}/cryptoKeys/${k.name}`, state: k.status });
      }
    }
    if (!keys.length) {
      return setChildren(body, el("p", { class: "unavailable",
        text: `This project has no symmetric keys in ${location}. Create a key ring and key there in Cloud KMS first.` }));
    }
    setChildren(body, el("ul", { class: "fs-rows", role: "list" }, ...keys.map((k) => el("li", { class: "fs-row" },
      el("button", { type: "button", class: "fs-row-link", text: k.name, title: k.state || "", onclick: () => {
        control.value = k.name;
        control.dispatchEvent(new Event("input", { bubbles: true }));
        control.dispatchEvent(new Event("change", { bubbles: true }));
        close();
        control.focus();
      } })))));
    body.querySelector("button").focus();
  } catch (err) {
    setChildren(body, el("p", { class: "form-error", text: `Cloud KMS cannot be read: ${err.message}` }));
  }
}

// --- Firestore's Data panel (#1845, round 2) ------------------------------
//
// Google's Firestore Studio Data viewer is a panel of three columns
// (https://docs.cloud.google.com/firestore/native/docs/using-console, read
// 2026-11-19): the database's collections (or, inside a subcollection, the
// parent document's), the selected collection's documents, and the selected
// document's subcollections and fields. Start collection, Add document and
// Add field are at the top of their columns; a collection's menu has Delete
// collection and its filter button Add filters; a document's menu has Delete
// document and Delete document fields; a field is edited by clicking it and
// deleted with the icon beside it. Edit path opens any path, and Panel
// settings is Google's Project panel settings dialog (Show non-existent
// parents, Realtime updates enabled). The database travels as ?database=,
// so every database of the project is browsed here, and the selection as
// ?path=. Each read and change goes through the routes the drill-down pages
// use, which stay reachable from each column's Open link.
const FS_PANEL_SETTINGS_KEY = "cb-firestore-panel-settings";
const FS_PANEL_FILTERS = new Map();
let FS_PANEL_TIMER = null;

function fsPanelSettings() {
  try {
    return { showMissing: true, realtime: true,
      ...JSON.parse(localStorage.getItem(FS_PANEL_SETTINGS_KEY) || "{}") };
  } catch { return { showMissing: true, realtime: true }; }
}

function fsPanelPath() {
  const raw = new URLSearchParams(location.search).get("path") || "";
  return raw.split("/").filter(Boolean);
}

function fsPanelGo(segments) {
  const url = new URL(location.href);
  if (segments.length) url.searchParams.set("path", segments.join("/"));
  else url.searchParams.delete("path");
  history.pushState(history.state, "", url.pathname + url.search);
}

async function renderFirestoreData(view, route) {
  if (FS_PANEL_TIMER) { clearInterval(FS_PANEL_TIMER); FS_PANEL_TIMER = null; }
  const project = currentProject();
  const settings = fsPanelSettings();
  const redraw = () => renderFirestoreData(view, route);
  const q = (segments) => `?project=${encodeURIComponent(project)}` +
    segments.map((s) => `&name=${encodeURIComponent(s)}`).join("");

  const editPath = el("button", { class: "header-button", type: "button", id: "fs-edit-path", text: "Edit path",
    onclick: () => fsEditPath(redraw) });
  const panelSettings = el("button", { class: "header-button", type: "button", id: "fs-panel-settings",
    text: "Panel settings", onclick: () => fsPanelSettingsDialog(redraw) });
  const header = pageHeader("Data", project ? `Project ${project}` + scopeSubtitle(route.service) : "All projects",
    { actions: [editPath, panelSettings, refreshButton(async () => redraw())] });
  setChildren(view, header, loadingState(4, { what: "collections" }));

  let list;
  try {
    list = await api(`/api/resources/${route.service}?project=${encodeURIComponent(project)}`);
  } catch (err) {
    return setChildren(view, header, errorState("Data unavailable", String(err.message), redraw));
  }
  if (list.prompt) return setChildren(view, header, emptyState("Choose a project", list.prompt));
  if (list.unavailable) return setChildren(view, header, errorState("Data unavailable", list.unavailable, redraw));
  const picker = list.scope ? scopePicker(list.scope, redraw) : null;
  const database = (list.scope && list.scope.selected) || "(default)";

  // The selection: P alternates collection and document IDs. The middle
  // column is the deepest collection selected, the right one its document.
  const P = fsPanelPath();
  const colEnd = P.length % 2 === 1 ? P.length : P.length - 1;
  const colPath = colEnd > 0 ? P.slice(0, colEnd).join("/") : "";
  const docId = P.length % 2 === 0 && P.length ? P[P.length - 1] : "";
  const parentDoc = colEnd >= 3 ? [P.slice(0, colEnd - 2).join("/"), P[colEnd - 2]] : null;

  const visible = (items) => (items || []).filter((i) => settings.showMissing || !i.absent);
  const column = (id, title, actions, body, menu) => el("section", { class: "fs-column card", id, "aria-label": title },
    el("div", { class: "fs-column-head" },
      el("h2", { class: "fs-column-title", text: title }),
      menu || null),
    actions ? el("div", { class: "fs-column-actions" }, ...actions) : null,
    body);
  const rowList = (items, selected, onPick, extra) => el("ul", { class: "fs-rows", role: "list" },
    ...items.map((item) => el("li", { class: "fs-row" + (item.name === selected ? " is-selected" : "") +
        (item.absent ? " is-absent" : "") },
      el("button", { type: "button", class: "fs-row-link", text: item.name, "aria-current": item.name === selected ? "true" : null,
        onclick: () => onPick(item) }),
      extra ? extra(item) : null)));
  const menuOf = (label, entries) => {
    const sel = el("select", { class: "fs-menu", "aria-label": label },
      el("option", { value: "", text: "⋮" }),
      ...entries.map((e) => el("option", { value: e.id, text: e.label })));
    sel.addEventListener("change", () => {
      const e = entries.find((x) => x.id === sel.value);
      sel.value = "";
      if (e) e.run();
    });
    return sel;
  };
  const act = (path, action, after) => runAction(route, path, action, after || redraw, NO_ROW);

  // Left: the database's collections, or the parent document's subcollections.
  let leftItems, leftTitle, leftStart;
  if (parentDoc) {
    const d = await api(`/api/detail/${route.service}${q(parentDoc)}`).catch((err) => ({ unavailable: err.message }));
    const subs = (d.sections || []).find((s) => s.id === "collections");
    leftItems = ((subs && subs.listing && subs.listing.items) || []).map((i) => ({ name: i.name }));
    leftTitle = parentDoc[1];
    leftStart = (d.actions || []).find((a) => a.id === "startcollection");
  } else {
    leftItems = list.items || [];
    leftTitle = database;
  }
  const selectedCol = colPath ? colPath.split("/").pop() : "";
  const startRoot = capabilityOf(route.service).create;
  const left = column("fs-collections", leftTitle, [
    el("button", { type: "button", class: "secondary", id: "fs-start-collection", text: "Start collection",
      onclick: () => parentDoc && leftStart
        ? act(parentDoc, leftStart, (name) => { redraw(); })
        : startRoot && openCreateForm(route, startRoot, (name) => { if (name) fsPanelGo([name]); redraw(); }) }),
  ], leftItems.length
    ? rowList(leftItems, selectedCol, (item) => {
        fsPanelGo(parentDoc ? [...P.slice(0, colEnd - 1), item.name] : [item.name]);
        redraw();
      })
    : el("p", { class: "unavailable", text: parentDoc ? "This document has no subcollections." : "This database has no collections." }));

  const columns = [left];
  // Middle: the collection's documents, with its filter and its menu.
  if (colPath) {
    const filter = FS_PANEL_FILTERS.get(database + "|" + colPath);
    let docs = [];
    let failure = "";
    let addDoc = null;
    try {
      const d = await api(`/api/detail/${route.service}${q([colPath])}`);
      if (d.unavailable) failure = d.unavailable;
      const sec = (d.sections || []).find((s) => s.id === "documents");
      docs = (sec && sec.listing && sec.listing.items) || [];
      addDoc = (d.actions || []).find((a) => a.id === "adddocument");
      if (filter) {
        const res = await send(`/api/query/${route.service}?project=${encodeURIComponent(project)}`, "POST",
          { Path: [colPath], Values: { field: filter.field, op: filter.op, value: filter.value, limit: "500" } });
        const names = new Set(((res.listing || {}).items || []).map((i) => i.name));
        docs = docs.filter((x) => names.has(x.name));
      }
    } catch (err) { failure = String(err.message); }
    const filterButton = el("button", { type: "button", class: "header-button" + (filter ? " is-active" : ""),
      id: "fs-filter", text: filter ? `Filter: ${filter.field} ${filter.op} ${filter.value}` : "Filter",
      "aria-label": "Filter list", onclick: () => fsFilterDialog(database + "|" + colPath, filter, redraw) });
    const deleteCollection = { id: "deletecollection", label: "Delete collection", destructive: true,
      confirm: "Every document in the collection, and in each of their subcollections, is deleted." };
    const menu = menuOf(`Collection ${selectedCol} menu`, [
      { id: "deletecollection", label: "Delete collection",
        run: () => act([colPath], deleteCollection, () => { fsPanelGo(P.slice(0, colEnd - 1)); redraw(); }) },
      { id: "open", label: "Open collection page", run: () => navigate(detailHref(route, [colPath])) },
    ]);
    const docsShown = visible(docs);
    columns.push(column("fs-documents", selectedCol,
      [addDoc ? el("button", { type: "button", class: "secondary", id: "fs-add-document", text: "Add document",
        onclick: () => act([colPath], addDoc) }) : null, filterButton].filter(Boolean),
      failure ? el("p", { class: "form-error", text: failure })
        : docsShown.length ? rowList(docsShown, docId, (item) => { fsPanelGo([...P.slice(0, colEnd), item.name]); redraw(); })
          : el("p", { class: "unavailable", text: filter ? "No documents match the filter." : "This collection has no documents." }),
      menu));
  }

  // Right: the document's subcollections and fields.
  if (colPath && docId) {
    const docPath = [colPath, docId];
    let d;
    try { d = await api(`/api/detail/${route.service}${q(docPath)}`); } catch (err) { d = { unavailable: String(err.message) }; }
    const fields = ((d.sections || []).find((s) => s.id === "fields") || {}).listing || {};
    const subs = ((d.sections || []).find((s) => s.id === "collections") || {}).listing || {};
    const acts = d.actions || [];
    const find = (id) => acts.find((a) => a.id === id);
    const menu = menuOf(`Document ${docId} menu`, [
      find("deletedocument") && { id: "deletedocument", label: "Delete document",
        run: () => act(docPath, find("deletedocument"), () => { fsPanelGo(P.slice(0, colEnd)); redraw(); }) },
      find("deletefields") && { id: "deletefields", label: "Delete document fields", run: () => act(docPath, find("deletefields")) },
      { id: "open", label: "Open document page", run: () => navigate(detailHref(route, docPath)) },
    ].filter(Boolean));
    const editField = async (name) => {
      try {
        const f = await api(`/api/detail/${route.service}${q([...docPath, name])}`);
        if (f.edit) return openEditForm(route, [...docPath, name], f.edit, redraw);
        navigate(detailHref(route, [...docPath, name]));
      } catch (err) { notify(`Cannot edit ${name}: ${err.message}`, "error"); }
    };
    const fieldRows = (fields.items || []).map((item) => el("li", { class: "fs-row fs-field" },
      el("button", { type: "button", class: "fs-row-link", title: "Edit field", "aria-label": `Edit field ${item.name}`,
        onclick: () => editField(item.name) },
        el("span", { class: "fs-field-name", text: item.name + ": " }),
        el("span", { class: "fs-field-value", text: (item.fields || {}).Value || "" }),
        el("span", { class: "fs-field-type", text: ` (${(item.fields || {}).Type || ""})` })),
      el("button", { type: "button", class: "icon-button danger fs-field-delete", "aria-label": `Delete field ${item.name}`,
        text: "✕", onclick: () => act([...docPath, item.name],
          { id: "deletefield", label: "Delete field", destructive: true }) })));
    columns.push(column("fs-document", docId, [
      find("startcollection") ? el("button", { type: "button", class: "secondary", id: "fs-start-subcollection",
        text: "Start collection", onclick: () => act(docPath, find("startcollection")) }) : null,
      find("addfield") ? el("button", { type: "button", class: "secondary", id: "fs-add-field", text: "Add field",
        onclick: () => act(docPath, find("addfield")) }) : null,
    ].filter(Boolean),
    d.unavailable ? el("p", { class: "form-error", text: d.unavailable }) : el("div", {},
      (subs.items || []).length
        ? rowList(subs.items, "", (item) => { fsPanelGo([...P.slice(0, colEnd), docId, item.name]); redraw(); })
        : null,
      fieldRows.length ? el("ul", { class: "fs-rows fs-fields", role: "list" }, ...fieldRows)
        : el("p", { class: "unavailable", text: fields.note || "This document has no fields." })),
    menu));
  }

  const crumbs = el("nav", { class: "breadcrumb fs-path", "aria-label": "Path" },
    el("button", { type: "button", class: "fs-row-link", text: database, onclick: () => { fsPanelGo([]); redraw(); } }),
    ...P.flatMap((s, i) => [el("span", { "aria-hidden": "true", text: " > " }),
      el("button", { type: "button", class: "fs-row-link", text: s, onclick: () => { fsPanelGo(P.slice(0, i + 1)); redraw(); } })]));
  setChildren(view, header, picker, crumbs, el("div", { class: "fs-panel", id: "fs-panel" }, ...columns));
  announce(`${database} data opened`);
  // Realtime updates (Panel settings): the panel is read again every few
  // seconds while it is on screen, as Google's listens for changes.
  if (settings.realtime) {
    const panel = view.querySelector("#fs-panel");
    FS_PANEL_TIMER = setInterval(() => {
      if (!panel.isConnected) { clearInterval(FS_PANEL_TIMER); FS_PANEL_TIMER = null; return; }
      if (document.querySelector(".modal") || document.hidden) return;
      redraw();
    }, 10000);
  }
}

function fsEditPath(redraw) {
  const { dialog, close } = openModal({ labelledBy: "fs-edit-path-title" });
  const input = el("input", { type: "text", id: "fs-path-input", value: fsPanelPath().join("/"),
    "aria-label": "Path", placeholder: "users/alice" });
  dialog.append(el("form", { class: "modal-body", onsubmit: (e) => {
    e.preventDefault();
    fsPanelGo(input.value.split("/").filter(Boolean));
    close();
    redraw();
  } },
  el("h2", { id: "fs-edit-path-title", text: "Edit path" }),
  el("p", { class: "form-help", text: "A collection or document path, such as users or users/alice/orders/o1." }),
  input,
  el("div", { class: "modal-actions" },
    el("button", { type: "button", class: "secondary", text: "Cancel", onclick: () => close() }),
    el("button", { type: "submit", class: "primary", text: "Go" }))));
  input.focus();
}

function fsPanelSettingsDialog(redraw) {
  const s = fsPanelSettings();
  const { dialog, close } = openModal({ labelledBy: "fs-settings-title" });
  const box = (id, label, on) => el("label", { class: "checkbox-row" },
    el("input", { type: "checkbox", id, checked: on }), el("span", { text: " " + label }));
  dialog.append(el("form", { class: "modal-body", onsubmit: (e) => {
    e.preventDefault();
    const next = { showMissing: dialog.querySelector("#fs-show-missing").checked,
      realtime: dialog.querySelector("#fs-realtime").checked };
    try { localStorage.setItem(FS_PANEL_SETTINGS_KEY, JSON.stringify(next)); } catch { /* storage refused */ }
    close();
    redraw();
  } },
  el("h2", { id: "fs-settings-title", text: "Project panel settings" }),
  box("fs-show-missing", "Show non-existent parents", s.showMissing),
  box("fs-realtime", "Realtime updates enabled", s.realtime),
  el("div", { class: "modal-actions" },
    el("button", { type: "button", class: "secondary", text: "Cancel", onclick: () => close() }),
    el("button", { type: "submit", class: "primary", text: "Save" }))));
}

function fsFilterDialog(key, current, redraw) {
  const { dialog, close } = openModal({ labelledBy: "fs-filter-title" });
  const field = el("input", { type: "text", id: "fs-filter-field", value: (current || {}).field || "", "aria-label": "Field" });
  const op = el("select", { id: "fs-filter-op", "aria-label": "Operator" },
    ...["==", "!=", "<", "<=", ">", ">=", "in", "array-contains"].map((o) =>
      el("option", { value: o, text: o, selected: o === ((current || {}).op || "==") })));
  const value = el("input", { type: "text", id: "fs-filter-value", value: (current || {}).value || "", "aria-label": "Value" });
  dialog.append(el("form", { class: "modal-body", onsubmit: (e) => {
    e.preventDefault();
    if (field.value.trim()) FS_PANEL_FILTERS.set(key, { field: field.value.trim(), op: op.value, value: value.value });
    else FS_PANEL_FILTERS.delete(key);
    close();
    redraw();
  } },
  el("h2", { id: "fs-filter-title", text: "Add filters" }),
  el("label", { for: "fs-filter-field", text: "Filter by field" }), field,
  el("label", { for: "fs-filter-op", text: "Condition" }), op,
  el("label", { for: "fs-filter-value", text: "Value" }), value,
  el("div", { class: "modal-actions" },
    el("button", { type: "button", class: "secondary", id: "fs-filter-clear", text: "Clear filter",
      onclick: () => { FS_PANEL_FILTERS.delete(key); close(); redraw(); } }),
    el("button", { type: "submit", class: "primary", text: "Apply" }))));
  field.focus();
}


// --- Connect and About (#802) -----------------------------------------
//
// What `cloudburrow env` exports for this instance, in every format it
// prints, from the same function; client-library snippets from the pages
// the suites run; the gcloud-setup and terraform commands with what each
// writes on the host (the console runs neither and writes no host
// configuration); the build, as `cloudburrow version` prints it; and the
// `cloudburrow diagnose` bundle as a download.
//
// Nothing here is a credential. The ADC fixture's path is shown, as `env`
// exports it; a variable whose value is one (the generated MySQL password) is
// named and withheld by the server. The diagnose download needs the admin
// token, as /admin does, because the bundle carries the admin API's events:
// the page asks for it, keeps it for this tab only, and the console passes it
// to the admin API, which decides. The console holds no token of its own.

const CONNECT_TITLE = "Connect";
const CONNECT_SUBTITLE =
  "Point client libraries, gcloud and Terraform at this instance. " +
  "Everything here is what the cloudburrow command prints for it; the console changes nothing on your machine.";
// The same session key the fault screen uses (#800), so a token pasted on
// either works on both, and only in this tab.
const CONNECT_TOKEN_KEY = "cb-admin-token";
let CONNECT_TOKEN = "";
// diagnose runs doctor, kubectl and a log tail; the server bounds the whole
// at two minutes.
const DIAGNOSE_DEADLINE_MS = 150000;

function connectToken() {
  if (CONNECT_TOKEN) return CONNECT_TOKEN;
  try { CONNECT_TOKEN = sessionStorage.getItem(CONNECT_TOKEN_KEY) || ""; } catch { /* storage refused */ }
  return CONNECT_TOKEN;
}

function setConnectToken(value) {
  CONNECT_TOKEN = value;
  try {
    if (value) sessionStorage.setItem(CONNECT_TOKEN_KEY, value);
    else sessionStorage.removeItem(CONNECT_TOKEN_KEY);
  } catch { /* storage refused */ }
}

// initAbout fills About in Settings and utilities from /api/about.
async function initAbout() {
  const version = document.getElementById("about-version");
  if (!version) return;
  try {
    const about = await api("/api/about");
    version.textContent = about.version;
    document.getElementById("about-commit").textContent = about.commit;
    document.getElementById("about-date").textContent = about.buildDate;
  } catch (err) {
    version.textContent = `unavailable: ${err.message}`;
  }
}

// connectCommand is one command to copy and what it writes.
function connectCommand(id, cmd) {
  return el("div", { class: "connect-block", id },
    el("div", { class: "connect-command" },
      el("code", { text: cmd.command }),
      copyButton(cmd.command, "the command")),
    el("p", { class: "form-help", text: cmd.writes }));
}

// connectChooser is a labelled select over options, showing one block at a
// time, with a Copy for what is shown.
function connectChooser(id, label, options, render) {
  const select = el("select", { id: `${id}-select` },
    ...options.map((o, i) => el("option", { value: String(i), text: o.label })));
  const body = el("div", { id: `${id}-body` });
  const show = () => setChildren(body, render(options[Number(select.value)]));
  select.addEventListener("change", show);
  show();
  return el("div", { class: "connect-block" },
    el("div", { class: "form-row" }, el("label", { for: `${id}-select`, text: label }), select),
    body);
}

async function renderConnect(view) {
  const header = () => pageHeader(CONNECT_TITLE, CONNECT_SUBTITLE);
  setChildren(view, header(), loadingState(4));
  let data;
  let about;
  try {
    [data, about] = await Promise.all([api("/api/connect"), api("/api/about")]);
  } catch (err) {
    if (location.pathname !== "/connect") return;
    setChildren(view, header(), errorState("Connect unavailable", err.message, () => renderConnect(view)));
    return;
  }
  if (location.pathname !== "/connect") return;

  // The environment, one format at a time, and the variables as a table.
  const envCard = el("section", { class: "card", id: "connect-env", "aria-labelledby": "connect-env-title" },
    el("h2", { id: "connect-env-title", text: "Environment" }),
    el("p", {}, el("span", { text: "What " }), el("code", { text: data.env.command }),
      el("span", { text: ` exports for instance ${data.instance}, project ${data.project}.` })),
    connectChooser("connect-format", "Format", data.formats || [], (f) => el("div", {},
      el("pre", { id: "connect-format-text", "data-format": f.id, text: f.text }),
      el("div", { class: "connect-copy" }, copyButton(f.text, `the ${f.label} environment`)))),
    (data.withheld || []).length
      ? el("ul", { class: "unmeasured", id: "connect-withheld" }, ...data.withheld.map((w) =>
          el("li", { "data-name": w.name }, el("strong", { text: w.name }),
            el("span", { text: ` is not shown: ${w.reason}.` }))))
      : null,
    (data.warnings || []).length
      ? el("ul", { class: "unmeasured", id: "connect-warnings" }, ...data.warnings.map((w) => el("li", { text: w })))
      : null,
    el("div", { class: "table-wrap" }, el("table", { class: "table", id: "connect-vars" },
      el("thead", {}, el("tr", {}, ...["Variable", "Value", "Read by"].map((h) => el("th", { scope: "col", text: h })))),
      el("tbody", {}, ...(data.variables || []).map((v) => el("tr", { "data-name": v.name },
        el("td", { class: "mono", text: v.name }),
        el("td", { class: "mono", text: v.value }),
        el("td", { text: v.comment })))))));

  // Client libraries: only those whose variables this instance exports.
  const langs = [...new Set((data.snippets || []).map((s) => s.language))];
  const snippetCard = el("section", { class: "card", id: "connect-snippets", "aria-labelledby": "connect-snippets-title" },
    el("h2", { id: "connect-snippets-title", text: "Client libraries" }),
    el("p", { text: "With the environment above exported, these are the clients that need something in code. " +
      "The Go, Python and Node.js clients for Cloud Storage and Pub/Sub read STORAGE_EMULATOR_HOST and PUBSUB_EMULATOR_HOST " +
      "themselves, except Node's Storage client, below." }),
    langs.length
      ? connectChooser("connect-language", "Language", langs.map((l) => ({ label: l })), (l) => el("div", {},
          ...(data.snippets || []).filter((s) => s.language === l.label).map((s) => el("div", { class: "connect-block", "data-snippet": `${s.language} ${s.title}` },
            el("h3", { text: s.title }),
            s.note ? el("p", { class: "form-help", text: s.note }) : null,
            el("pre", { text: s.code }),
            el("div", { class: "connect-copy" }, copyButton(s.code, `the ${s.language} ${s.title} snippet`),
              el("span", { class: "muted", text: `From ${s.source}` }))))))
      : el("p", { class: "muted", text: "This instance serves no service whose client needs more than the environment." }));

  const toolsCard = el("section", { class: "card", id: "connect-tools", "aria-labelledby": "connect-tools-title" },
    el("h2", { id: "connect-tools-title", text: "gcloud and Terraform" }),
    el("p", { text: "Run these in a terminal. The console does not write gcloud or Terraform configuration itself." }),
    el("h3", { text: "gcloud" }),
    connectCommand("connect-gcloud-setup", data.gcloudSetup),
    connectCommand("connect-gcloud-teardown", data.gcloudTeardown),
    el("h3", { text: "Terraform" }),
    connectCommand("connect-terraform", data.terraform));

  const aboutCard = el("section", { class: "card", id: "connect-about", "aria-labelledby": "connect-about-title" },
    el("h2", { id: "connect-about-title", text: "About" }),
    el("dl", { class: "connect-about" },
      el("dt", { text: "Version" }), el("dd", { id: "connect-version", text: about.version }),
      el("dt", { text: "Commit" }), el("dd", { id: "connect-commit", text: about.commit }),
      el("dt", { text: "Built" }), el("dd", { id: "connect-build-date", text: about.buildDate }),
      el("dt", { text: "Go" }), el("dd", { text: about.goVersion }),
      el("dt", { text: "Platform" }), el("dd", { text: about.platform })),
    el("div", { class: "connect-copy" }, copyButton(about.line, "the version"),
      el("span", { class: "muted", text: "As cloudburrow version prints it." })));

  setChildren(view, header(), envCard, snippetCard, toolsCard, aboutCard, diagnoseCard(data));
}

// diagnoseCard offers the bundle `cloudburrow diagnose` writes, as a download.
function diagnoseCard(data) {
  const status = el("p", { class: "muted", id: "diagnose-status", role: "status" });
  const error = el("p", { class: "form-error", role: "alert", id: "diagnose-error", hidden: true });
  const tokenInput = el("input", { id: "diagnose-token", type: "password", autocomplete: "off", spellcheck: "false" });
  const tokenRow = el("div", { class: "form-row", id: "diagnose-token-row" },
    el("label", { for: "diagnose-token", text: "Admin token" }),
    tokenInput,
    el("p", { class: "form-help" },
      el("span", { text: "The contents of " }), el("code", { text: data.adminTokenFile || "the instance's admin-token file" }),
      el("span", { text: ". Kept for this tab only, and sent with the download." })));
  tokenRow.hidden = !!connectToken();
  const button = el("button", { type: "submit", class: "primary", id: "diagnose-download", text: "Download bundle" });

  const download = async (e) => {
    e.preventDefault();
    error.hidden = true;
    const typed = tokenInput.value.trim();
    if (typed) setConnectToken(typed);
    const token = connectToken();
    if (!token) {
      tokenRow.hidden = false;
      error.textContent = "Paste the instance's admin token: the bundle holds the admin API's events, which need it.";
      error.hidden = false;
      tokenInput.focus();
      return;
    }
    setBusy(button, true);
    status.textContent = "Collecting: doctor, readiness, status, pods, events and logs…";
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), DIAGNOSE_DEADLINE_MS);
    try {
      const res = await fetch("/api/diagnose", { headers: { Authorization: `Bearer ${token}` }, signal: controller.signal });
      if (!res.ok) {
        let body = {};
        try { body = await res.json(); } catch { /* not JSON */ }
        if (res.status === 401) {
          setConnectToken("");
          tokenInput.value = "";
          tokenRow.hidden = false;
          tokenInput.focus();
        }
        throw new Error(body.error || `/api/diagnose responded ${res.status} ${res.statusText}`);
      }
      const blob = await res.blob();
      const disposition = res.headers.get("Content-Disposition") || "";
      const match = /filename="([^"]+)"/.exec(disposition);
      const name = match ? match[1] : "cloudburrow-diagnose.tar.gz";
      const url = URL.createObjectURL(blob);
      // Not attached to the document, so the console's router never sees
      // the click and the browser saves the file.
      el("a", { href: url, download: name }).click();
      setTimeout(() => URL.revokeObjectURL(url), 60000);
      tokenRow.hidden = true;
      tokenInput.value = "";
      status.textContent = `Downloaded ${name} (${formatBytes(blob.size)}).`;
      notify(`Downloaded ${name}`);
    } catch (err) {
      status.textContent = "";
      error.textContent = controller.signal.aborted
        ? `/api/diagnose did not answer within ${Math.round(DIAGNOSE_DEADLINE_MS / 1000)}s`
        : `The bundle was not built: ${err.message}`;
      error.hidden = false;
    } finally {
      clearTimeout(timer);
      setBusy(button, false);
    }
  };

  return el("form", { class: "card", id: "connect-diagnose", novalidate: true, onsubmit: download,
    "aria-labelledby": "connect-diagnose-title" },
    el("h2", { id: "connect-diagnose-title", text: "Diagnose bundle" }),
    el("p", {}, el("span", { text: "The redacted bundle " }), el("code", { text: data.diagnose.command }),
      el("span", { text: " writes, for a bug report: version, configuration, doctor output, readiness, status, recent admin events, pods, events and logs." })),
    el("p", { class: "form-help", text: `Never collected: ${(data.diagnoseExcluded || []).join(", ")}.` }),
    tokenRow,
    error,
    el("div", { class: "form-actions" }, button),
    status);
}

// --- BigQuery Studio (#1861) -------------------------------------------
//
// Google's BigQuery Studio: the Explorer pane (Home, the project, its saved
// queries under Queries, its datasets and their tables, views, routines and
// models, with search, stars and each node's View actions menu) beside a
// tabbed workspace. The Home tab opens first; a SQL query tab has Run, Save
// (Save query, Save query as), Share, Schedule, Edit (Query settings) and a
// validator that dry-runs the statement ("This query will process 1.2 KB
// when run."), and Query results with Job information, Results,
// Visualization, JSON, Execution details and Execution graph. A resource
// opened from the Explorer is a tab too, with its page's own tabs; a plain
// click replaces the preview tab (its name in italics), Ctrl or Command
// click and a double click on the tab keep it, as Google's do
// (https://docs.cloud.google.com/bigquery/docs/bigquery-web-ui). The
// standalone pages stay as deep links. Personal history and Project history
// are below. Every call goes to POST /api/studio/bigquery
// (cmd/cloudburrow/consolebigquerystudio.go).
//
// Saved queries are CloudBurrow's own: Google keeps them as Dataform code
// assets, which CloudBurrow does not serve, so they are kept in the
// instance's state directory (consolebigquerysavedqueries.go) and say so.
// Schedule (the BigQuery Data Transfer Service's) is drawn where Google
// draws it, disabled, with the reason.

const BQ_STUDIO_STARS = "cloudburrow.bigquery.stars";
const BQ_STUDIO_TABS = "cloudburrow.bigquery.tabs";
const BQ_STUDIO_RECENT = "cloudburrow.bigquery.recent";
const BQ_SAVED_NOTE = "CloudBurrow-only: Google keeps a saved query as a Dataform code asset, which CloudBurrow does not serve. " +
  "This one is kept in the instance's state directory, listed under Queries in the Explorer, and reachable by no API.";
const BQ_UNAVAILABLE_SCHEDULE = "Not available locally: a scheduled query is a BigQuery Data Transfer Service transfer, " +
  "which CloudBurrow does not run.";
const BQ_UNAVAILABLE_PERMISSIONS = "Not available locally: a saved query's permissions (Code Owner, Code Editor, Code Viewer) " +
  "are Dataform's IAM, which CloudBurrow does not serve. Copy link shares it with anyone using this instance.";

const bqStudioCall = (call, body = {}) =>
  send(`/api/studio/bigquery?call=${encodeURIComponent(call)}&project=${encodeURIComponent(currentProject())}`,
    "POST", body);

function bqStars() {
  try { return new Set(JSON.parse(readStored(BQ_STUDIO_STARS, "[]"))); } catch { return new Set(); }
}

function bqReadTabs() {
  try {
    const raw = sessionStorage.getItem(BQ_STUDIO_TABS);
    const tabs = raw ? JSON.parse(raw) : null;
    if (Array.isArray(tabs) && tabs.length) return tabs;
  } catch { /* storage refused: start fresh */ }
  return null;
}

function bqWriteTabs(tabs) {
  try { sessionStorage.setItem(BQ_STUDIO_TABS, JSON.stringify(tabs.map(({ id, kind, title, query, settings, path, savedId, preview, subtab, viz }) =>
    ({ id, kind, title, query, settings, path, savedId, preview, subtab, viz })))); } catch { /* not kept */ }
}

// bqRecent are the ten resources most recently opened, newest first, for
// the Home tab's Recent.
function bqRecent() {
  const r = readStored(BQ_STUDIO_RECENT, []);
  return Array.isArray(r) ? r : [];
}

function bqRemember(item) {
  const key = JSON.stringify([item.kind, item.path || item.savedId]);
  const rest = bqRecent().filter((r) => JSON.stringify([r.kind, r.path || r.savedId]) !== key);
  writeStored(BQ_STUDIO_RECENT, [item, ...rest].slice(0, 10));
}

// bqSide runs fn so that the dialog its synchronous part opens is a
// right-side panel.
function bqSide(fn) {
  MODAL_SIDE_NEXT = true;
  try { return fn(); } finally { MODAL_SIDE_NEXT = false; }
}

// BQ_LOCATIONS are Google's BigQuery locations, as Create dataset lists
// them (cmd/cloudburrow/consolebigquerycreatedataset.go).
const BQ_LOCATIONS = ["US", "EU",
  "us-east5", "us-south1", "us-central1", "us-west2", "us-west4", "northamerica-south1", "northamerica-northeast1",
  "us-east4", "us-central2", "us-west1", "us-west3", "southamerica-east1", "southamerica-west1", "us-east1",
  "northamerica-northeast2",
  "europe-west1", "europe-west10", "europe-north1", "europe-west3", "europe-west2", "europe-southwest1", "europe-west8",
  "europe-west4", "europe-west9", "europe-north2", "europe-west12", "europe-central2", "europe-west6",
  "asia-southeast3", "asia-south2", "asia-east2", "asia-southeast2", "australia-southeast2", "asia-south1",
  "asia-northeast2", "asia-northeast3", "asia-southeast1", "australia-southeast1", "asia-east1", "asia-northeast1",
  "me-central2", "me-central1", "me-west1", "africa-south1"];

// bqMenu is a "View actions" button and its menu of items.
function bqMenu(label, items) {
  const menu = el("div", { class: "bq-menu", role: "menu", hidden: true });
  const button = el("button", {
    class: "icon-button bq-node-menu", type: "button", "aria-label": label, title: "View actions",
    "aria-haspopup": "menu", "aria-expanded": "false",
    html: '<svg viewBox="0 0 24 24" aria-hidden="true"><circle cx="12" cy="5" r="1.6"/><circle cx="12" cy="12" r="1.6"/><circle cx="12" cy="19" r="1.6"/></svg>',
  });
  const close = () => { menu.hidden = true; button.setAttribute("aria-expanded", "false"); };
  for (const item of items) {
    menu.append(el("button", {
      type: "button", role: "menuitem", class: "bq-menu-item" + (item.danger ? " danger" : ""),
      text: item.label, disabled: item.disabled || null, title: item.title || null,
      onclick: (e) => { e.stopPropagation(); close(); item.run(); },
    }));
  }
  button.addEventListener("click", (e) => {
    e.stopPropagation();
    const open = menu.hidden;
    document.querySelectorAll(".bq-menu").forEach((m) => { m.hidden = true; });
    menu.hidden = !open;
    button.setAttribute("aria-expanded", open ? "true" : "false");
    if (open) {
      const first = menu.querySelector("button:not([disabled])");
      if (first) first.focus();
    }
  });
  menu.addEventListener("keydown", (e) => { if (e.key === "Escape") { close(); button.focus(); } });
  // A click anywhere else closes it, as a menu does.
  const outside = (e) => {
    if (!menu.isConnected) { document.removeEventListener("click", outside); return; }
    if (!menu.hidden && !menu.contains(e.target)) close();
  };
  document.addEventListener("click", outside);
  return el("span", { class: "bq-menu-wrap" }, button, menu);
}

async function bqCopy(value, what) {
  try {
    await navigator.clipboard.writeText(value);
    notify(`Copied ${what}`);
  } catch (err) {
    notify(`Could not copy: ${err.message}`, "error");
  }
}

// bqDetailAction performs one of a resource page's own actions (Create
// table, Delete table, ...) from the Explorer, with the page's own form,
// drawn as a right-side panel when side is set.
async function bqDetailAction(path, actionID, onDone, side = false) {
  const route = ROUTES.find((r) => r.path === "/bigquery");
  const q = path.map((s) => `&name=${encodeURIComponent(s)}`).join("");
  try {
    const detail = await api(`/api/detail/bigquery?project=${encodeURIComponent(currentProject())}${q}`);
    const action = (detail.actions || []).find((a) => a.id === actionID);
    if (!action) {
      notify(`${actionID} is not offered on ${path.join(".")}`, "error");
      return;
    }
    if (action.destructive) {
      await confirmDestructive({
        title: `${action.label}?`, detail: path[path.length - 1], confirmWord: path[path.length - 1],
        confirmLabel: action.label,
        onConfirm: () => send(`/api/actions/bigquery?project=${encodeURIComponent(currentProject())}`,
          "POST", { Path: path, Action: action.id }),
      });
      onDone();
      return;
    }
    if (side) bqSide(() => runAction(route, path, action, onDone));
    else runAction(route, path, action, onDone);
  } catch (err) {
    notify(err.message, "error");
  }
}

// bqCreateDataset is Create dataset as Google's Studio draws it: a panel
// on the right, with the form of the Create dataset page (/bigquery/create,
// which stays as its deep link).
function bqCreateDataset(onDone) {
  const route = ROUTES.find((r) => r.path === "/bigquery");
  const spec = capabilityOf("bigquery").create;
  if (!spec) {
    notify("Create dataset is not offered on this instance", "error");
    return;
  }
  bqSide(() => openCreateForm(route, spec, (name) => {
    notify(`Created ${name}`);
    onDone(name);
  }));
}

async function bqDeleteDataset(id, onDone) {
  await confirmDestructive({
    title: "Delete dataset?", detail: id, confirmWord: id,
    consequence: "Every table in the dataset, and every row in them, is deleted with it.",
    onConfirm: () => send(`/api/resources/bigquery?project=${encodeURIComponent(currentProject())}&name=${encodeURIComponent(id)}`,
      "DELETE"),
  });
  onDone();
}

// bqResourceKind names a resource tab's kind from its path.
function bqResourceKind(path, type) {
  if (path.length === 1) return "dataset";
  if (path[1] === "routines") return "routine";
  if (path[1] === "models") return "model";
  return type === "VIEW" || type === "MATERIALIZED_VIEW" ? "view" : "table";
}

function renderBigQueryStudio(view) {
  const project = currentProject();
  const stored = bqReadTabs();
  const tabs = stored || [{ id: 1, kind: "home", title: "Home" }];
  const params = new URLSearchParams(location.search);
  const linked = params.get("query");
  const linkedSaved = params.get("savedQuery");
  let counter = tabs.reduce((n, t) => Math.max(n, t.id), 0);
  const queryCount = () => tabs.filter((t) => (t.kind || "query") === "query" && !t.savedId).length;
  const newTab = (query = "", title = null, extra = {}) => {
    counter += 1;
    const n = queryCount() + 1;
    const tab = { id: counter, kind: "query", title: title || (n === 1 ? "Untitled query" : `Untitled query ${n}`),
                  query, settings: {}, ...extra };
    tabs.push(tab);
    return tab;
  };
  if (linked !== null) newTab(linked);
  if (!tabs.length) tabs.push({ id: ++counter, kind: "home", title: "Home" });
  let current = tabs[tabs.length - 1];

  const explorerBody = el("div", { class: "bq-explorer-body" });
  const search = el("input", { type: "search", class: "bq-explorer-search", placeholder: "Search BigQuery resources",
                               "aria-label": "Search BigQuery resources" });
  const starredOnly = el("input", { type: "checkbox", id: "bq-starred-only" });
  const strip = el("div", { class: "tab-strip bq-tabs", role: "tablist", "aria-label": "Editor tabs" });
  const panel = el("div", { class: "bq-tab-panel" });
  const history = el("section", { class: "bq-history card", "aria-label": "Job history" });

  let tree = null;
  let saved = [];
  const expanded = new Set([project]);

  const loadSaved = async () => {
    try {
      saved = (await bqStudioCall("savedqueries")).queries || [];
    } catch (err) {
      saved = [];
      notify(`Saved queries: ${err.message}`, "error");
    }
  };
  const loadExplorer = async () => {
    setChildren(explorerBody, el("p", { class: "unavailable" }, spinner("Loading the Explorer"), " Loading…"));
    try {
      [tree] = await Promise.all([bqStudioCall("explorer"), loadSaved()]);
      drawTree();
    } catch (err) {
      setChildren(explorerBody, el("p", { class: "form-error", role: "alert", text: err.message }));
    }
  };

  const select = (tab) => { current = tab; drawTabs(); };
  const openQuery = (sql, title = null) => select(newTab(sql, title));
  const openHome = () => {
    const home = tabs.find((t) => t.kind === "home");
    if (home) return select(home);
    counter += 1;
    const tab = { id: counter, kind: "home", title: "Home" };
    tabs.unshift(tab);
    select(tab);
  };
  // openSaved opens a saved query in its tab, or the tab it is open in.
  const openSaved = (sq) => {
    bqRemember({ kind: "query", savedId: sq.id, title: sq.name });
    const open = tabs.find((t) => t.savedId === sq.id);
    if (open) return select(open);
    select(newTab(sq.query, sq.name, { savedId: sq.id }));
  };
  // openResource opens a dataset, table, view, routine or model in a tab:
  // in the preview tab unless keep, as a click in Google's Explorer does.
  const openResource = (path, kind, keep = false) => {
    const title = path[path.length - 1];
    bqRemember({ kind, path, title });
    const same = tabs.find((t) => t.kind === "resource" && JSON.stringify(t.path) === JSON.stringify(path));
    if (same) {
      if (keep) same.preview = false;
      return select(same);
    }
    const tab = { id: ++counter, kind: "resource", resourceKind: kind, title, path, preview: !keep };
    const preview = keep ? -1 : tabs.findIndex((t) => t.kind === "resource" && t.preview);
    if (preview >= 0) tabs.splice(preview, 1, tab);
    else tabs.push(tab);
    select(tab);
  };

  const drawTree = () => {
    if (!tree) return;
    const stars = bqStars();
    const q = search.value.trim().toLowerCase();
    const onlyStarred = starredOnly.checked;
    const proj = tree.project;
    const starButton = (key, label) => {
      const on = stars.has(key);
      return el("button", {
        type: "button", class: "icon-button bq-star" + (on ? " is-starred" : ""),
        "aria-pressed": on ? "true" : "false", "aria-label": (on ? "Unstar " : "Star ") + label,
        title: on ? "Unstar" : "Star",
        html: `<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M12 3.5l2.6 5.3 5.9.9-4.25 4.1 1 5.8L12 16.9l-5.25 2.7 1-5.8L3.5 9.7l5.9-.9z"${on ? ' fill="currentColor"' : ""}/></svg>`,
        onclick: (e) => {
          e.stopPropagation();
          const s = bqStars();
          if (s.has(key)) s.delete(key); else s.add(key);
          writeStored(BQ_STUDIO_STARS, JSON.stringify([...s]));
          drawTree();
        },
      });
    };
    // node is one row of the tree. open, when set, is the standalone page
    // (the link's address, a deep link) and onOpen what a click does: it
    // opens the resource as a Studio tab; Ctrl or Command click keeps it.
    const node = (key, label, kind, level, menuItems, children = null, open = null, onOpen = null) => {
      const hasChildren = children !== null;
      const isOpen = hasChildren && (expanded.has(key) || !!q);
      const toggle = hasChildren
        ? el("button", { type: "button", class: "icon-button bq-twisty", "aria-label": (isOpen ? "Collapse " : "Expand ") + label,
            html: isOpen ? '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M6 9l6 6 6-6"/></svg>'
                         : '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M9 6l6 6-6 6"/></svg>',
            onclick: () => { if (expanded.has(key)) expanded.delete(key); else expanded.add(key); drawTree(); } })
        : el("span", { class: "bq-twisty-space" });
      const name = open
        ? el("a", { href: open + (open.includes("?") ? "" : scopeSearch()), class: "bq-node-name", text: label,
            onclick: onOpen ? (e) => {
              if (e.shiftKey || e.altKey || e.button !== 0) return;
              e.preventDefault();
              onOpen(e.metaKey || e.ctrlKey);
            } : null })
        : el("span", { class: "bq-node-name", text: label });
      return el("li", { role: "treeitem", "aria-level": String(level), "aria-expanded": hasChildren ? String(isOpen) : null,
                        "data-node": key, class: "bq-node bq-" + kind },
        el("div", { class: "bq-node-row", style: `padding-left:${(level - 1) * 16}px` },
          toggle, name,
          kind === "folder" ? null : starButton(key, label), menuItems ? bqMenu(`View actions for ${label}`, menuItems) : null),
        hasChildren && isOpen ? el("ul", { role: "group" }, ...children) : null);
    };
    const matches = (key, label) => (!q || label.toLowerCase().includes(q)) && (!onlyStarred || stars.has(key));
    const href = (path) => "/bigquery/" + path.map(encodeURIComponent).join("/");

    // Queries: the project's saved queries (CloudBurrow-only, see above).
    const queryKids = saved.filter((sq) => matches(`${proj}.queries.${sq.id}`, sq.name)).map((sq) =>
      node(`${proj}.queries.${sq.id}`, sq.name, "query", 3, [
        { label: "Open", run: () => openSaved(sq) },
        { label: "Copy link", run: () => bqCopy(bqSavedLink(sq.id), "the link to this saved query") },
        { label: "Manage permissions", disabled: true, title: BQ_UNAVAILABLE_PERMISSIONS, run: () => {} },
        { label: "Delete", danger: true, run: () => bqDeleteSaved(sq, async () => { await loadSaved(); drawTree(); }) },
      ], null, "/bigquery-studio?" + new URLSearchParams({ project: proj, savedQuery: sq.id }), () => openSaved(sq)));
    const queriesNode = (!q && !onlyStarred) || queryKids.length
      ? node(`${proj}.queries`, "Queries", "folder", 2, null, queryKids.length ? queryKids
          : [el("li", { class: "bq-node-empty unavailable", text: "No saved queries. Save a query from its tab." })])
      : null;

    const dsNodes = [];
    for (const ds of tree.datasets) {
      const dsKey = `${proj}.${ds.id}`;
      const kids = [];
      const leaf = (id, kind, path, isTable) => {
        const key = `${dsKey}.${id}`;
        if (!matches(key, id)) return;
        const items = [
          { label: "Open", run: () => openResource(path, kind) },
          { label: "Open in new tab", run: () => openResource(path, kind, true) },
        ];
        if (isTable) {
          items.push({ label: "Query", run: () => openQuery(`SELECT * FROM \`${proj}.${ds.id}.${id}\` LIMIT 1000`, id) });
        }
        items.push({ label: "Copy ID", run: () => bqCopy(key, "ID") });
        if (isTable) items.push({ label: "Delete", danger: true, run: () => bqDetailAction(path, "deletetable", loadExplorer) });
        if (kind === "routine") items.push({ label: "Delete", danger: true, run: () => bqDetailAction(path, "deleteroutine", loadExplorer) });
        if (kind === "model") items.push({ label: "Delete", danger: true, run: () => bqDetailAction(path, "deletemodel", loadExplorer) });
        kids.push(node(key, id, kind, 3, items, null, href(path), (keep) => openResource(path, kind, keep)));
      };
      for (const t of ds.tables) {
        const kind = t.type === "VIEW" || t.type === "MATERIALIZED_VIEW" ? "view" : t.type === "MODEL" ? "model" : "table";
        leaf(t.id, kind, [ds.id, t.id], true);
      }
      for (const r of ds.routines) leaf(r, "routine", [ds.id, "routines", r], false);
      for (const m of ds.models) leaf(m, "model", [ds.id, "models", m], false);
      if (!kids.length && !matches(dsKey, ds.id)) continue;
      dsNodes.push(node(dsKey, ds.id, "dataset", 2, [
        { label: "Create table", run: () => bqDetailAction([ds.id], "createtable", loadExplorer, true) },
        { label: "Open", run: () => openResource([ds.id], "dataset") },
        { label: "Open in new tab", run: () => openResource([ds.id], "dataset", true) },
        { label: "Query", run: () => openQuery(`SELECT * FROM \`${proj}.${ds.id}.INFORMATION_SCHEMA.TABLES\``, ds.id) },
        { label: "Copy ID", run: () => bqCopy(dsKey, "ID") },
        { label: "Delete", danger: true, run: () => bqDeleteDataset(ds.id, loadExplorer) },
      ], kids, href([ds.id]), (keep) => openResource([ds.id], "dataset", keep)));
    }
    const projectNode = node(proj, proj, "project", 1, [
      { label: "Create dataset", run: () => bqCreateDataset(() => loadExplorer()) },
      { label: "Refresh contents", run: () => loadExplorer() },
      { label: "Copy ID", run: () => bqCopy(proj, "ID") },
    ], [queriesNode, ...dsNodes].filter(Boolean), "/bigquery");
    setChildren(explorerBody,
      el("ul", { class: "bq-tree", role: "tree", "aria-label": "Explorer" }, projectNode),
      tree.datasets.length ? null
        : el("p", { class: "unavailable", text: "No datasets yet. Create dataset from the project's View actions." }),
      (q || onlyStarred) && !dsNodes.length && !queryKids.length ? el("p", { class: "unavailable", text: "No resources match." }) : null);
  };
  search.addEventListener("input", drawTree);
  starredOnly.addEventListener("change", drawTree);

  const closeTab = (t) => {
    tabs.splice(tabs.indexOf(t), 1);
    if (!tabs.length) openHome();
    else if (current === t) current = tabs[tabs.length - 1];
    drawTabs();
  };

  const drawTabs = () => {
    bqWriteTabs(tabs);
    setChildren(strip, ...tabs.map((t) => el("span", { class: "bq-tab-wrap" },
      el("button", {
        type: "button", role: "tab", class: "tab bq-tab-" + (t.kind || "query") + (t === current ? " is-selected" : "") +
          (t.preview ? " is-preview" : ""),
        "aria-selected": t === current ? "true" : "false", text: t.title,
        title: t.preview ? "Preview tab: double-click to keep it open" : null,
        onclick: () => select(t),
        ondblclick: () => { if (t.preview) { t.preview = false; drawTabs(); } },
      }),
      el("button", {
        type: "button", class: "icon-button bq-tab-close", "aria-label": `Close ${t.title}`, title: "Close tab",
        html: '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M6 6l12 12M18 6L6 18"/></svg>',
        onclick: () => closeTab(t),
      }))),
      el("button", { type: "button", class: "secondary bq-new-query", text: "SQL query",
        "aria-label": "SQL query: open a new query tab", onclick: () => openQuery("") }));
    const kind = current.kind || "query";
    if (kind === "home") {
      setChildren(panel, bqHomeTab({
        tree: () => tree, openQuery, openResource, openSaved, saved: () => saved,
        createDataset: () => bqCreateDataset(() => loadExplorer()),
        createTable: (ds) => bqDetailAction([ds], "createtable", loadExplorer, true),
        loadFile: (ds) => bqDetailAction([ds], "loadfile", loadExplorer, true),
      }));
    } else if (kind === "resource") {
      const host = el("div", { class: "bq-resource-tab", "data-embedded": "true" });
      if (current.subtab) host.dataset.tab = current.subtab;
      const tab = current;
      host.addEventListener("cb-subtab", (e) => { tab.subtab = e.detail; bqWriteTabs(tabs); });
      setChildren(panel, host);
      renderDetail(host, ROUTES.find((r) => r.path === "/bigquery"), current.path);
    } else {
      setChildren(panel, bqQueryTab(current, {
        save: () => bqWriteTabs(tabs),
        onRan: () => drawHistory(),
        saved: () => saved,
        onSaved: async (sq) => {
          await loadSaved();
          drawTree();
          bqRemember({ kind: "query", savedId: sq.id, title: sq.name });
          drawTabs();
        },
      }));
    }
  };

  let historyAll = false;
  const drawHistory = async () => {
    const tabsRow = el("div", { class: "tab-strip bq-history-tabs", role: "tablist", "aria-label": "Job history" },
      ...[["Personal history", false], ["Project history", true]].map(([label, all]) => el("button", {
        type: "button", role: "tab", class: "tab" + (historyAll === all ? " is-selected" : ""),
        "aria-selected": historyAll === all ? "true" : "false", text: label,
        onclick: () => { historyAll = all; drawHistory(); },
      })),
      el("button", { type: "button", class: "secondary", text: "Refresh", onclick: () => drawHistory() }));
    const body = el("div", { class: "bq-history-body" }, spinner("Loading job history"));
    setChildren(history, tabsRow, body);
    try {
      const data = await bqStudioCall("jobs", { all: historyAll });
      if (!data.jobs.length) {
        setChildren(body, el("p", { class: "unavailable", text: "No jobs yet." }));
        return;
      }
      setChildren(body, el("table", { class: "bq-history-table" },
        el("thead", {}, el("tr", {}, ...["Job ID", "Type", "Created", "User", "Bytes processed", "Status", ""].map((h) =>
          el("th", { scope: "col", text: h })))),
        el("tbody", {}, ...data.jobs.map((j) => el("tr", {},
          el("td", { class: "mono" }, el("a", { href: "/bigquery-jobs/" + encodeURIComponent(j.id) + scopeSearch(), text: j.id })),
          el("td", { text: j.type }), el("td", { text: j.created }), el("td", { text: j.user }),
          el("td", { text: j.bytes }), el("td", {}, statusIcon(j.state), el("span", { text: " " + j.state })),
          el("td", {}, bqMenu(`View actions for job ${j.id}`, [
            { label: "Show job details", run: () => navigate("/bigquery-jobs/" + encodeURIComponent(j.id)) },
            j.query ? { label: "Open as new query", run: () => openQuery(j.query) } : null,
          ].filter(Boolean))))))));
    } catch (err) {
      setChildren(body, el("p", { class: "form-error", role: "alert", text: err.message }));
    }
  };

  setChildren(view,
    pageHeader("BigQuery Studio", null),
    el("div", { class: "bq-studio" },
      el("aside", { class: "bq-explorer card", "aria-label": "Explorer" },
        el("div", { class: "bq-explorer-head" },
          el("h2", { text: "Explorer" }),
          el("button", { type: "button", class: "secondary", text: "+ Add data", title: "Create dataset",
                         onclick: () => bqCreateDataset(() => loadExplorer()) })),
        el("button", { type: "button", class: "bq-explorer-home", text: "Home", onclick: () => openHome() }),
        search,
        el("label", { class: "bq-starred", for: "bq-starred-only" }, starredOnly, el("span", { text: " Show starred resources only" })),
        explorerBody),
      el("main", { class: "bq-workspace" },
        strip, panel, history)));
  drawTabs();
  loadExplorer().then(() => {
    // A link to a saved query (Copy link) opens it once the list is read.
    if (linkedSaved) {
      const sq = saved.find((x) => x.id === linkedSaved);
      if (sq) openSaved(sq);
      else notify(`No saved query ${linkedSaved} in ${project}`, "error");
    }
    const home = panel.querySelector(".bq-home");
    if (home && home.bqRefresh) home.bqRefresh();
  });
  drawHistory();
  announce("BigQuery Studio");
}

function bqSavedLink(id) {
  const url = new URL("/bigquery-studio", location.origin);
  url.searchParams.set("project", currentProject());
  url.searchParams.set("savedQuery", id);
  return url.toString();
}

async function bqDeleteSaved(sq, onDone) {
  await confirmDestructive({
    title: "Delete saved query?", detail: sq.name, confirmWord: sq.name,
    onConfirm: () => bqStudioCall("deletequery", { id: sq.id }),
  });
  onDone();
}

// bqHomeTab is Studio's Home tab, in Google's sections: Check out what's
// new in Studio, Create new, Recent, Try with templates and Add your own
// data. What CloudBurrow does not run is drawn disabled with its reason.
function bqHomeTab(ctx) {
  const card = (title, ...body) => el("section", { class: "bq-home-card card" }, el("h3", { text: title }), ...body);
  const action = (label, run, why = null) => el("button", {
    type: "button", class: "secondary bq-home-action", text: label, disabled: why ? true : null,
    title: why, "aria-description": why, onclick: why ? null : run,
  });
  const notebook = "Not available locally: notebooks are Colab Enterprise's, which CloudBurrow does not run.";
  // A dataset chooser for Create new's Table and Add your own data's
  // Load from a file, which act on a dataset.
  // The datasets arrive with the Explorer, after the tab is drawn: refresh
  // fills them in place, so a click on the tab is never lost to a redraw.
  const dsPick = el("select", { class: "bq-home-dataset", "aria-label": "Dataset" });
  const dsLabel = el("label", { class: "bq-home-dataset-label" }, el("span", { text: "Dataset for Table and Load from a file " }), dsPick);
  const tableButton = action("Table", () => ctx.createTable(dsPick.value));
  const loadButton = action("Load from a file", () => ctx.loadFile(dsPick.value));
  const refresh = () => {
    const datasets = ((ctx.tree() || {}).datasets || []).map((d) => d.id);
    const keep = dsPick.value;
    setChildren(dsPick, ...datasets.map((d) => el("option", { value: d, text: d })));
    if (datasets.includes(keep)) dsPick.value = keep;
    const why = datasets.length ? null : "Create a dataset first: a table belongs to one.";
    for (const b of [tableButton, loadButton]) {
      b.disabled = !!why;
      if (why) { b.title = why; b.setAttribute("aria-description", why); } else { b.removeAttribute("title"); b.removeAttribute("aria-description"); }
    }
    dsLabel.hidden = !datasets.length;
  };
  const recent = bqRecent();
  const templates = [
    ["Tables in this project", "SELECT table_schema, table_name, table_type, creation_time\nFROM `region-us`.INFORMATION_SCHEMA.TABLES\nORDER BY creation_time DESC"],
    ["Jobs by user", "SELECT user_email, COUNT(*) AS jobs, SUM(total_bytes_processed) AS bytes\nFROM `region-us`.INFORMATION_SCHEMA.JOBS_BY_PROJECT\nGROUP BY user_email\nORDER BY jobs DESC"],
    ["Generate a date series", "SELECT day, EXTRACT(DAYOFWEEK FROM day) AS weekday\nFROM UNNEST(GENERATE_DATE_ARRAY(DATE '2026-01-01', DATE '2026-01-31')) AS day"],
  ];
  const home = el("div", { class: "bq-home" },
    el("h2", { class: "bq-home-title", text: "Welcome to BigQuery Studio" }),
    card("Check out what's new in Studio",
      el("p", { class: "unavailable", text: "Not available locally: Google's list of new Studio features is not shipped with CloudBurrow. " +
        "This Studio has the Explorer, SQL query tabs with Query settings, saved queries, query results with a Visualization, " +
        "resource tabs and job history." })),
    card("Create new",
      el("div", { class: "bq-home-actions" },
        action("SQL query", () => ctx.openQuery("")),
        action("Notebook", null, notebook),
        action("Apache Spark notebook", null, notebook),
        action("Data canvas", null, "Not available locally: data canvas is a Gemini in BigQuery feature, which CloudBurrow does not run."),
        action("Data preparation", null, "Not available locally: data preparation files are Dataform's, which CloudBurrow does not serve."),
        action("Pipeline", null, "Not available locally: pipelines are Dataform's, which CloudBurrow does not serve."),
        tableButton),
      dsLabel),
    card("Recent",
      recent.length
        ? el("ul", { class: "bq-home-recent" }, ...recent.map((r) => el("li", {},
            el("button", { type: "button", class: "link-button", text: r.title, onclick: () => {
              if (r.path) return ctx.openResource(r.path, r.kind);
              const sq = ctx.saved().find((x) => x.id === r.savedId);
              if (sq) ctx.openSaved(sq); else notify(`The saved query ${r.title} is gone`, "error");
            } }),
            el("span", { class: "unavailable", text: " " + (r.kind === "query" ? "saved query" : r.kind) }))))
        : el("p", { class: "unavailable", text: "The tables, saved queries, models and routines you open are listed here." })),
    card("Try with templates",
      el("ul", { class: "bq-home-templates" }, ...templates.map(([title, sql]) => el("li", {},
        el("button", { type: "button", class: "link-button", text: title, onclick: () => ctx.openQuery(sql, title) })))),
      el("p", { class: "unavailable", text: "Google's templates over its public datasets are not available locally; these run against this instance." })),
    card("Add your own data",
      el("div", { class: "bq-home-actions" },
        action("Create dataset", () => ctx.createDataset()),
        loadButton)));
  refresh();
  home.bqRefresh = refresh;
  return home;
}

// bqQueryTab is one SQL query tab: the toolbar, the editor and its
// validator, and Query results. A tab opened from a saved query carries
// its id, and Save query saves a new version of it.
function bqQueryTab(tab, ctx) {
  const save = ctx.save;
  const settings = tab.settings || (tab.settings = {});
  const editor = el("textarea", { class: "query-editor mono bq-editor", rows: "10", spellcheck: "false",
                                  "aria-label": "Query editor", placeholder: "SELECT 1" });
  editor.value = tab.query || "";
  const validator = el("p", { class: "bq-validator", role: "status" });
  const results = el("section", { class: "bq-results", "aria-label": "Query results" });
  const run = el("button", { class: "primary", type: "button", text: "Run" });

  const settingsBody = () => {
    const legacy = settings.legacySql === true;
    return { legacySql: legacy, priority: settings.priority || "INTERACTIVE", location: settings.location || "",
             destinationDataset: settings.destinationDataset || "", destinationTable: settings.destinationTable || "",
             writeDisposition: settings.writeDisposition || "", defaultDataset: "" };
  };

  let timer = null;
  let seq = 0;
  const validate = () => {
    clearTimeout(timer);
    const sql = editor.value.trim();
    if (!sql) { setChildren(validator); validator.className = "bq-validator"; return; }
    timer = setTimeout(async () => {
      const mine = ++seq;
      try {
        const res = await bqStudioCall("query", { query: sql, dryRun: true, ...settingsBody() });
        if (mine !== seq) return;
        if (res.error) {
          validator.className = "bq-validator is-invalid";
          validator.textContent = `! ${res.error}`;
          return;
        }
        validator.className = "bq-validator is-valid";
        validator.textContent = `✓ This query will process ${bqBytes(res.bytesProcessed)} when run.`;
      } catch (err) {
        if (mine !== seq) return;
        validator.className = "bq-validator is-invalid";
        validator.textContent = `! ${err.message}`;
      }
    }, 500);
  };
  editor.addEventListener("input", () => { tab.query = editor.value; save(); validate(); });

  const execute = async (write = false) => {
    const sql = editor.value.trim();
    if (!sql) return;
    setBusy(run, true);
    setChildren(results, el("p", { class: "unavailable" }, spinner("Running"), " Running…"));
    try {
      const res = await bqStudioCall("query", { query: sql, write, ...settingsBody() });
      if (res.confirm) {
        setChildren(results);
        setBusy(run, false);
        const ok = await bqConfirmWrite(sql, res.confirm);
        if (ok) return execute(true);
        return;
      }
      drawResults(results, res, tab, save);
      announce(`Query complete: ${res.totalRows} rows`);
    } catch (err) {
      setChildren(results, el("p", { class: "form-error", role: "alert", text: err.message }));
    } finally {
      setBusy(run, false);
    }
    ctx.onRan();
  };
  run.addEventListener("click", () => execute());

  // Save > Save query (a new version of this saved query, or a new one)
  // and Save query as (always a new one), with Google's Name and Region.
  const savedOne = () => (tab.savedId ? ctx.saved().find((q) => q.id === tab.savedId) : null);
  const saveQuery = async (asNew) => {
    const current = asNew ? null : savedOne();
    if (current) {
      try {
        const sq = await bqStudioCall("savequery", { id: current.id, name: current.name, query: editor.value });
        notify(`Saved ${sq.name}, version ${sq.version}`);
        await ctx.onSaved(sq);
      } catch (err) {
        notify(`Save query failed: ${err.message}`, "error");
      }
      return;
    }
    const sq = await bqSaveDialog(asNew ? "Save query as" : "Save query",
      asNew && savedOne() ? `${savedOne().name} (copy)` : (tab.title.startsWith("Untitled") ? "" : tab.title), editor.value);
    if (!sq) return;
    tab.savedId = sq.id;
    tab.title = sq.name;
    save();
    notify(`Saved ${sq.name}`);
    await ctx.onSaved(sq);
  };
  editor.addEventListener("keydown", (e) => {
    if ((e.metaKey || e.ctrlKey) && e.key === "Enter") { e.preventDefault(); execute(); }
    if ((e.metaKey || e.ctrlKey) && (e.key === "s" || e.key === "S")) { e.preventDefault(); saveQuery(false); }
  });
  const saveMenu = bqMenu("Save", [
    { label: "Save query", run: () => saveQuery(false) },
    { label: "Save query as", run: () => saveQuery(true) },
  ]);
  bqMenuButton(saveMenu, "Save", "Save");

  const share = bqMenu("Share", [
    { label: "Copy link", run: () => {
      if (tab.savedId) return bqCopy(bqSavedLink(tab.savedId), "the link to this saved query");
      const url = new URL("/bigquery-studio", location.origin);
      url.searchParams.set("project", currentProject());
      url.searchParams.set("query", editor.value);
      bqCopy(url.toString(), "the link to this query");
    } },
    { label: "Manage permissions", disabled: true, title: BQ_UNAVAILABLE_PERMISSIONS, run: () => {} },
  ]);
  bqMenuButton(share, "Share", "Share");

  // Edit > Query settings, where Google's docs put it ("click Edit > Query
  // settings", https://docs.cloud.google.com/bigquery/docs/running-queries,
  // read 2026-11-19).
  const edit = bqMenu("Edit", [
    { label: "Query settings", run: () => bqQuerySettings(settings, () => { save(); validate(); drawDialect(); }) },
    { label: "Clear query", run: () => { editor.value = ""; tab.query = ""; save(); validate(); setChildren(results); } },
  ]);
  bqMenuButton(edit, "Edit", "Edit");

  const dialect = el("span", { class: "unavailable bq-dialect" });
  const drawDialect = () => { dialect.textContent = settings.legacySql ? "Legacy SQL" : "GoogleSQL"; };
  drawDialect();
  const toolbar = el("div", { class: "bq-toolbar", role: "toolbar", "aria-label": "Query editor actions" },
    run, saveMenu, share,
    el("button", { class: "secondary", type: "button", text: "Schedule", disabled: true, title: BQ_UNAVAILABLE_SCHEDULE,
                   "aria-description": BQ_UNAVAILABLE_SCHEDULE }),
    edit, dialect);
  validate();
  return el("div", { class: "bq-query-tab" },
    toolbar, editor, validator,
    el("p", { class: "unavailable bq-hint",
      text: (tab.savedId ? BQ_SAVED_NOTE + " " : "") +
        "Schedule is not available locally (the Data Transfer Service is not run). ⌘/Ctrl + Enter runs; ⌘/Ctrl + S saves." }),
    results);
}

// bqMenuButton turns a bqMenu's button into a labelled text button, as
// Google's Save, Share and Edit are.
function bqMenuButton(menu, label, title) {
  const b = menu.querySelector("button");
  b.classList.remove("icon-button", "bq-node-menu");
  b.classList.add("secondary", "bq-menu-button");
  b.setAttribute("aria-label", label);
  b.setAttribute("title", title);
  b.textContent = label + " ▾";
}

// bqSaveDialog asks for a saved query's Name and Region, saves it, and
// settles with it (null when cancelled).
function bqSaveDialog(title, name, query) {
  return new Promise((settle) => {
    let result = null;
    const { dialog, close } = openModal({ labelledBy: "bq-save-title" });
    const input = el("input", { type: "text", id: "bq-save-name", value: name, required: true, maxlength: "256" });
    const regions = ["", ...BQ_LOCATIONS];
    const region = el("select", { id: "bq-save-region" },
      ...regions.map((r) => el("option", { value: r, text: r || "Default" })));
    const error = el("p", { class: "form-error", role: "alert", hidden: true });
    const primary = el("button", { type: "submit", class: "primary", text: "Save" });
    dialog.append(el("form", { class: "modal-body", novalidate: true, onsubmit: async (e) => {
      e.preventDefault();
      if (!input.value.trim()) { error.textContent = "A saved query needs a name."; error.hidden = false; return; }
      setBusy(primary, true);
      try {
        result = await bqStudioCall("savequery", { name: input.value, region: region.value, query });
        close();
      } catch (err) {
        setBusy(primary, false);
        error.textContent = err.message;
        error.hidden = false;
      }
    } },
      el("h2", { id: "bq-save-title", text: title }),
      error,
      el("label", { for: "bq-save-name", text: "Name" }), input,
      el("label", { for: "bq-save-region", text: "Region" }), region,
      el("p", { class: "form-help", text: "Optional. A saved query's region cannot be changed after it is created." }),
      el("p", { class: "unavailable", text: BQ_SAVED_NOTE }),
      el("div", { class: "modal-actions" },
        el("button", { type: "button", class: "secondary", text: "Cancel", onclick: () => close() }), primary)));
    dialog.addEventListener("cb-closed", () => settle(result));
    input.focus();
  });
}

function bqBytes(n) {
  const units = ["B", "KB", "MB", "GB", "TB", "PB"];
  let f = Number(n) || 0;
  let i = 0;
  while (f >= 1024 && i < units.length - 1) { f /= 1024; i++; }
  return i === 0 ? `${f} B` : `${f.toFixed(2)} ${units[i]}`;
}

function bqConfirmWrite(sql, text) {
  return new Promise((settle) => {
    let decided = false;
    const { dialog, close } = openModal({ labelledBy: "bq-write-title" });
    const confirm = el("button", { type: "submit", class: "primary danger", text: "Run statement" });
    dialog.append(el("form", { class: "modal-body", onsubmit: (e) => { e.preventDefault(); decided = true; close(); } },
      el("h2", { id: "bq-write-title", text: "Run a statement that changes data?" }),
      el("p", { text }),
      el("pre", { class: "mono confirm-detail", text: sql }),
      el("div", { class: "modal-actions" },
        el("button", { type: "button", class: "secondary", text: "Cancel", onclick: () => close() }), confirm)));
    dialog.addEventListener("cb-closed", () => settle(decided));
    confirm.focus();
  });
}

// bqQuerySettings is Edit > Query settings: Destination, Resource
// management (Job priority), Additional settings (SQL dialect, Data
// location), in Google's words.
function bqQuerySettings(settings, onSave) {
  const { dialog, close } = openModal({ labelledBy: "bq-settings-title" });
  const radio = (name, value, label, checked) => {
    const input = el("input", { type: "radio", name, value });
    input.checked = checked;
    return el("label", { class: "bq-radio" }, input, el("span", { text: " " + label }));
  };
  const hasDest = !!settings.destinationTable;
  const dataset = el("input", { type: "text", id: "bq-dest-dataset", value: settings.destinationDataset || "" });
  const table = el("input", { type: "text", id: "bq-dest-table", value: settings.destinationTable || "" });
  const disposition = el("select", { id: "bq-dest-write" },
    ...[["WRITE_EMPTY", "Write if empty"], ["WRITE_APPEND", "Append to table"], ["WRITE_TRUNCATE", "Overwrite table"]]
      .map(([v, l]) => { const o = el("option", { value: v, text: l }); o.selected = (settings.writeDisposition || "WRITE_EMPTY") === v; return o; }));
  const destFields = el("div", { class: "bq-dest-fields", hidden: hasDest ? null : true },
    el("label", { for: "bq-dest-dataset", text: "Dataset" }), dataset,
    el("label", { for: "bq-dest-table", text: "Table ID" }), table,
    el("label", { for: "bq-dest-write", text: "Destination table write preference" }), disposition);
  const location = el("input", { type: "text", id: "bq-location", value: settings.location || "",
                                 placeholder: "Automatic location selection" });
  const form = el("form", { class: "modal-body bq-settings", onsubmit: (e) => {
    e.preventDefault();
    const fd = new FormData(form);
    const dest = fd.get("bq-dest") === "table";
    settings.destinationDataset = dest ? dataset.value.trim() : "";
    settings.destinationTable = dest ? table.value.trim() : "";
    settings.writeDisposition = dest ? disposition.value : "";
    settings.priority = fd.get("bq-priority");
    settings.legacySql = fd.get("bq-dialect") === "legacy";
    settings.location = location.value.trim();
    close();
    onSave();
  } },
    el("h2", { id: "bq-settings-title", text: "Query settings" }),
    el("fieldset", {}, el("legend", { text: "Destination" }),
      radio("bq-dest", "temp", "Save query results in a temporary table", !hasDest),
      radio("bq-dest", "table", "Set a destination table for query results", hasDest),
      destFields),
    el("fieldset", {}, el("legend", { text: "Resource management" }),
      el("p", { class: "form-help", text: "Job priority" }),
      radio("bq-priority", "INTERACTIVE", "Interactive", (settings.priority || "INTERACTIVE") === "INTERACTIVE"),
      radio("bq-priority", "BATCH", "Batch", settings.priority === "BATCH")),
    el("fieldset", {}, el("legend", { text: "Additional settings" }),
      el("p", { class: "form-help", text: "SQL dialect" }),
      radio("bq-dialect", "standard", "GoogleSQL", !settings.legacySql),
      radio("bq-dialect", "legacy", "Legacy SQL", !!settings.legacySql),
      el("label", { for: "bq-location", text: "Data location" }), location),
    el("div", { class: "modal-actions" },
      el("button", { type: "button", class: "secondary", text: "Cancel", onclick: () => close() }),
      el("button", { type: "submit", class: "primary", text: "Save" })));
  form.addEventListener("change", () => {
    destFields.hidden = new FormData(form).get("bq-dest") !== "table";
  });
  dialog.append(form);
}

// drawResults is Query results: Job information, Results, Visualization,
// JSON, Execution details and Execution graph, opening on Results.
function drawResults(host, res, tab = {}, save = () => {}) {
  const names = ["Job information", "Results", "Visualization", "JSON", "Execution details", "Execution graph"];
  let shown = "Results";
  const body = el("div", { class: "bq-results-body", role: "tabpanel" });
  const strip = el("div", { class: "tab-strip bq-results-tabs", role: "tablist", "aria-label": "Query results" });
  const columns = (res.schema || []).map((f) => f.name);
  const cell = (v) => v === null || v === undefined ? "null" : typeof v === "object" ? JSON.stringify(v) : String(v);
  const draw = () => {
    setChildren(strip, ...names.map((n) => el("button", {
      type: "button", role: "tab", class: "tab" + (n === shown ? " is-selected" : ""),
      "aria-selected": n === shown ? "true" : "false", text: n, onclick: () => { shown = n; draw(); },
    })));
    if (shown === "Job information") {
      setChildren(body, el("dl", { class: "bq-job-info" }, ...(res.job || []).flatMap((p) =>
        [el("dt", { text: p.label }), el("dd", { text: p.value })])));
    } else if (shown === "Results") {
      setChildren(body,
        res.message ? el("p", { class: "unavailable", text: res.message }) : null,
        columns.length
          ? el("div", { class: "bq-results-scroll" }, el("table", { class: "bq-results-table" },
              el("thead", {}, el("tr", {}, el("th", { scope: "col", text: "Row" }),
                ...columns.map((c) => el("th", { scope: "col", text: c })))),
              el("tbody", {}, ...(res.rows || []).map((r, i) => el("tr", {},
                el("td", { text: String(i + 1) }), ...columns.map((c) => el("td", { class: "mono", text: cell(r[c]) })))))))
          : (res.message ? null : el("p", { class: "unavailable", text: "This query returned no results." })),
        columns.length ? el("p", { class: "unavailable", text: `${(res.rows || []).length} of ${res.totalRows} rows` }) : null);
    } else if (shown === "Visualization") {
      setChildren(body, bqVisualization(res, tab, save));
    } else if (shown === "JSON") {
      setChildren(body, el("pre", { class: "mono bq-json", text: JSON.stringify(res.rows || [], null, 2) }));
    } else if (shown === "Execution details") {
      setChildren(body, (res.stages || []).length
        ? el("table", { class: "bq-results-table" },
            el("thead", {}, el("tr", {}, ...["Stage", "Status", "Records read", "Records written"].map((h) => el("th", { scope: "col", text: h })))),
            el("tbody", {}, ...res.stages.map((s) => el("tr", {},
              el("td", { text: s.name }), el("td", { text: s.status || "—" }),
              el("td", { text: String(s.recordsRead || 0) }), el("td", { text: String(s.recordsWritten || 0) })))))
        : el("p", { class: "unavailable",
            text: "The job reports no query plan: the local engine runs the query in one step, so there are no stages, timings or slot time to show." }));
    } else {
      setChildren(body, (res.stages || []).length
        ? el("ol", { class: "bq-graph" }, ...res.stages.map((s) => el("li", { text: `${s.name}${(s.inputStages || []).length ? " ← " + s.inputStages.join(", ") : ""}` })))
        : el("p", { class: "unavailable",
            text: "The job reports no query plan, so there is no execution graph: the local engine runs the query in one step." }));
    }
  };
  draw();
  setChildren(host, el("h2", { class: "bq-results-title", text: "Query results" }), strip, body);
}

// The column types Google's Visualization charts: a dimension of any of
// them, a measure of the numeric ones
// (https://docs.cloud.google.com/bigquery/docs/table-explorer, read
// 2026-11-19).
const BQ_MEASURE_TYPES = ["INTEGER", "INT64", "FLOAT", "FLOAT64", "NUMERIC", "BIGNUMERIC"];
const BQ_DIMENSION_TYPES = [...BQ_MEASURE_TYPES, "TIMESTAMP", "DATE", "DATETIME", "TIME", "STRING"];
const BQ_SERIES_COLOURS = ["#1a73e8", "#e8710a", "#188038", "#d93025", "#9334e6", "#12b5cb"];

// bqVisualization is the Visualization tab: the chart of the result set,
// drawn here as SVG from the rows the query returned, and the
// Visualization configuration pane (Visualization type, Dimension,
// Measures), prefilled from the schema and kept on the tab across runs.
// The chart zooms, toggles its legend and downloads as a PNG.
function bqVisualization(res, tab, save) {
  const schema = (res.schema || []).filter((f) => (f.mode || "NULLABLE") !== "REPEATED");
  const dims = schema.filter((f) => BQ_DIMENSION_TYPES.includes(f.type));
  const measures = schema.filter((f) => BQ_MEASURE_TYPES.includes(f.type));
  const geo = schema.some((f) => f.type === "GEOGRAPHY");
  if (!dims.length || !measures.length) {
    return el("p", { class: "unavailable", text: geo
      ? "Not available locally: a GEOGRAPHY result is drawn on Google Maps' map, which CloudBurrow does not reach. Line, Bar and Scatter need a numeric measure."
      : "There is nothing to chart: a chart needs a dimension column (a number, a date or time, or a string) and a numeric measure column." });
  }
  const viz = tab.viz || (tab.viz = {});
  if (!dims.some((f) => f.name === viz.dimension)) {
    const firstOther = dims.find((f) => !BQ_MEASURE_TYPES.includes(f.type)) || dims[0];
    viz.dimension = firstOther.name;
  }
  viz.measures = (viz.measures || []).filter((m) => measures.some((f) => f.name === m));
  if (!viz.measures.length) {
    const m = measures.find((f) => f.name !== viz.dimension) || measures[0];
    viz.measures = [m.name];
  }
  if (!["Bar", "Line", "Scatter"].includes(viz.type)) viz.type = "Bar";
  if (viz.legend === undefined) viz.legend = true;
  viz.zoom = viz.zoom || 1;

  const chartHost = el("div", { class: "bq-chart-host" });
  const redraw = () => { save(); drawChart(); };
  const typeSelect = el("select", { id: "bq-viz-type", onchange: (e) => { viz.type = e.target.value; redraw(); } },
    ...["Bar", "Line", "Scatter", "Map"].map((t) => {
      const o = el("option", { value: t, text: t });
      if (t === "Map") {
        o.disabled = true;
        o.title = "Not available locally: a map is drawn on Google Maps, which CloudBurrow does not reach.";
      }
      o.selected = t === viz.type;
      return o;
    }));
  const dimSelect = el("select", { id: "bq-viz-dimension", onchange: (e) => { viz.dimension = e.target.value; redraw(); } },
    ...dims.map((f) => { const o = el("option", { value: f.name, text: `${f.name} (${f.type})` }); o.selected = f.name === viz.dimension; return o; }));
  const measureBoxes = el("fieldset", { class: "bq-viz-measures" }, el("legend", { text: "Measures" }),
    ...measures.map((f) => {
      const box = el("input", { type: "checkbox", value: f.name, id: `bq-viz-m-${f.name}` });
      box.checked = viz.measures.includes(f.name);
      box.addEventListener("change", () => {
        const on = [...measureBoxes.querySelectorAll("input:checked")].map((b) => b.value);
        if (!on.length) { box.checked = true; return; }
        viz.measures = on;
        redraw();
      });
      return el("label", { class: "bq-radio", for: `bq-viz-m-${f.name}` }, box, el("span", { text: " " + f.name }));
    }));
  const tools = el("div", { class: "bq-viz-tools", role: "toolbar", "aria-label": "Chart" },
    el("button", { type: "button", class: "secondary", text: "Zoom in", onclick: () => { viz.zoom = Math.min(4, viz.zoom * 1.5); redraw(); } }),
    el("button", { type: "button", class: "secondary", text: "Zoom out", onclick: () => { viz.zoom = Math.max(1, viz.zoom / 1.5); redraw(); } }),
    el("button", { type: "button", class: "secondary", text: "Toggle legend", "aria-pressed": String(viz.legend),
      onclick: (e) => { viz.legend = !viz.legend; e.target.setAttribute("aria-pressed", String(viz.legend)); redraw(); } }),
    el("button", { type: "button", class: "secondary", text: "Download PNG", onclick: () => bqChartPNG(chartHost.querySelector("svg")) }));

  const drawChart = () => {
    const rows = res.rows || [];
    const dimType = (dims.find((f) => f.name === viz.dimension) || {}).type;
    const numericDim = BQ_MEASURE_TYPES.includes(dimType);
    const timeDim = ["TIMESTAMP", "DATE", "DATETIME"].includes(dimType);
    const xOf = (v) => numericDim ? Number(v) : timeDim ? Date.parse(String(v).replace(" UTC", "Z").replace(" ", "T")) : NaN;
    const continuous = viz.type !== "Bar" && (numericDim || timeDim);
    const points = rows.map((r) => ({ label: cellText(r[viz.dimension]), x: xOf(r[viz.dimension]),
      ys: viz.measures.map((m) => (r[m] === null || r[m] === undefined ? NaN : Number(r[m]))) }))
      .filter((p) => !continuous || Number.isFinite(p.x));
    if (continuous) points.sort((a, b) => a.x - b.x);
    // As wide as its column (once drawn), times the zoom.
    const fit = chartHost.isConnected && chartHost.clientWidth > 0 ? Math.max(320, chartHost.clientWidth - 2) : 640;
    const W = Math.round(fit * viz.zoom), H = 320, L = 56, R = 16, T = 16, B = 56;
    const ys = points.flatMap((p) => p.ys).filter(Number.isFinite);
    let yMin = Math.min(0, ...ys), yMax = Math.max(0, ...ys);
    if (yMin === yMax) yMax = yMin + 1;
    const sy = (v) => T + (H - T - B) * (1 - (v - yMin) / (yMax - yMin));
    let sx;
    if (continuous) {
      const xs = points.map((p) => p.x);
      const x0 = Math.min(...xs), x1 = Math.max(...xs);
      sx = (p) => L + (W - L - R) * (x1 === x0 ? 0.5 : (p.x - x0) / (x1 - x0));
    } else {
      const step = (W - L - R) / Math.max(1, points.length);
      sx = (p, i) => L + step * (i + 0.5);
    }
    const NS = "http://www.w3.org/2000/svg";
    const svg = document.createElementNS(NS, "svg");
    svg.setAttribute("viewBox", `0 0 ${W} ${H}`);
    svg.setAttribute("width", String(W));
    svg.setAttribute("height", String(H));
    // Its own size, which the console's icon rules for svg would shrink.
    svg.setAttribute("style", `width:${W}px;height:${H}px;max-width:none`);
    svg.setAttribute("role", "img");
    svg.setAttribute("class", "bq-chart");
    svg.setAttribute("aria-label", `${viz.type} chart of ${viz.measures.join(", ")} by ${viz.dimension}, ${points.length} points`);
    const add = (tag, attrs, text) => {
      const n = document.createElementNS(NS, tag);
      for (const [k, v] of Object.entries(attrs)) n.setAttribute(k, String(v));
      if (text !== undefined) n.textContent = text;
      svg.append(n);
      return n;
    };
    add("rect", { x: 0, y: 0, width: W, height: H, fill: "#ffffff" });
    // Axes and four gridlines with their values.
    for (let i = 0; i <= 4; i++) {
      const v = yMin + (yMax - yMin) * i / 4;
      add("line", { x1: L, x2: W - R, y1: sy(v), y2: sy(v), stroke: "#e0e0e0" });
      add("text", { x: L - 6, y: sy(v) + 4, "text-anchor": "end", "font-size": 11, fill: "#5f6368" }, bqShortNumber(v));
    }
    add("line", { x1: L, x2: W - R, y1: sy(0), y2: sy(0), stroke: "#5f6368" });
    const labelEvery = Math.max(1, Math.ceil(points.length / Math.floor((W - L - R) / 72)));
    points.forEach((p, i) => {
      if (i % labelEvery) return;
      add("text", { x: sx(p, i), y: H - B + 16, "text-anchor": "middle", "font-size": 11, fill: "#5f6368" },
        p.label.length > 14 ? p.label.slice(0, 13) + "…" : p.label);
    });
    add("text", { x: (L + W - R) / 2, y: H - 8, "text-anchor": "middle", "font-size": 12, fill: "#202124" }, viz.dimension);
    const n = viz.measures.length;
    viz.measures.forEach((m, k) => {
      const colour = BQ_SERIES_COLOURS[k % BQ_SERIES_COLOURS.length];
      if (viz.type === "Bar") {
        const step = (W - L - R) / Math.max(1, points.length);
        const bw = Math.max(1, (step * 0.8) / n);
        points.forEach((p, i) => {
          const v = p.ys[k];
          if (!Number.isFinite(v)) return;
          const x = sx(p, i) - (step * 0.8) / 2 + bw * k;
          add("rect", { x, y: Math.min(sy(v), sy(0)), width: bw, height: Math.abs(sy(v) - sy(0)), fill: colour },
          ).append(Object.assign(document.createElementNS(NS, "title"), { textContent: `${p.label}: ${m} ${v}` }));
        });
      } else {
        const pts = points.map((p, i) => [sx(p, i), p.ys[k]]).filter(([, v]) => Number.isFinite(v));
        if (viz.type === "Line" && pts.length > 1) {
          add("polyline", { points: pts.map(([x, v]) => `${x},${sy(v)}`).join(" "), fill: "none", stroke: colour, "stroke-width": 2 });
        }
        pts.forEach(([x, v]) => add("circle", { cx: x, cy: sy(v), r: viz.type === "Scatter" ? 4 : 2.5, fill: colour }));
      }
    });
    const legend = viz.legend
      ? el("ul", { class: "bq-chart-legend" }, ...viz.measures.map((m, k) => el("li", {},
          el("span", { class: "bq-swatch", style: `background:${BQ_SERIES_COLOURS[k % BQ_SERIES_COLOURS.length]}` }),
          el("span", { text: " " + m }))))
      : null;
    setChildren(chartHost, el("div", { class: "bq-chart-scroll" }, svg), legend,
      rows.length < res.totalRows
        ? el("p", { class: "unavailable", text: `Charted from the ${rows.length} rows read of ${res.totalRows}.` }) : null);
  };
  drawChart();
  requestAnimationFrame(() => { if (chartHost.isConnected) drawChart(); });
  return el("div", { class: "bq-viz" },
    el("div", { class: "bq-viz-chart" }, tools, chartHost),
    el("aside", { class: "bq-viz-config", "aria-label": "Visualization configuration" },
      el("h3", { text: "Visualization configuration" }),
      el("label", { for: "bq-viz-type", text: "Visualization type" }), typeSelect,
      el("label", { for: "bq-viz-dimension", text: "Dimension" }), dimSelect,
      measureBoxes));
}

function cellText(v) {
  return v === null || v === undefined ? "null" : typeof v === "object" ? JSON.stringify(v) : String(v);
}

function bqShortNumber(v) {
  const a = Math.abs(v);
  if (a >= 1e9) return (v / 1e9).toFixed(1) + "B";
  if (a >= 1e6) return (v / 1e6).toFixed(1) + "M";
  if (a >= 1e4) return (v / 1e3).toFixed(1) + "K";
  return Number.isInteger(v) ? String(v) : v.toFixed(2);
}

// bqChartPNG downloads the chart as a PNG, drawn from its SVG on a canvas.
function bqChartPNG(svg) {
  if (!svg) return;
  const data = new XMLSerializer().serializeToString(svg);
  const img = new Image();
  img.onload = () => {
    const canvas = document.createElement("canvas");
    canvas.width = Number(svg.getAttribute("width"));
    canvas.height = Number(svg.getAttribute("height"));
    canvas.getContext("2d").drawImage(img, 0, 0);
    canvas.toBlob((blob) => {
      if (!blob) { notify("Could not draw the chart as a PNG", "error"); return; }
      const a = el("a", { href: URL.createObjectURL(blob), download: "chart.png" });
      document.body.append(a);
      a.click();
      a.remove();
      setTimeout(() => URL.revokeObjectURL(a.href), 1000);
    }, "image/png");
  };
  img.onerror = () => notify("Could not draw the chart as a PNG", "error");
  img.src = "data:image/svg+xml;charset=utf-8," + encodeURIComponent(data);
}

// --- IAM & Admin > IAM (#1864) -----------------------------------------
//
// Google's IAM page: "Permissions for project <id>", View by principals and
// View by roles, Grant access and Remove access on the title line, Include
// Google-provided role grants, and each principal's Edit principal. The
// roles a folder or the organization grants are listed with the
// principal's, marked with where they come from, as Google's Inheritance
// column marks them, and are changed on that folder's or the
// organization's page. Every change is one SetIamPolicy with the etag the
// page read (cmd/cloudburrow/consoleiampage.go).

function iamCall(call, body = {}) {
  return send(`/api/studio/iam?call=${encodeURIComponent(call)}&project=${encodeURIComponent(currentProject())}`,
    "POST", body);
}

// iamGoogleProvided says whether a principal is a Google-managed service
// agent, whose grants Google's page hides until Include Google-provided
// role grants is checked.
function iamGoogleProvided(member) {
  return /^serviceAccount:(service-\d+@|\d+@cloudservices\.gserviceaccount\.com$|.*@gcp-sa-[^.]+\.iam\.gserviceaccount\.com$|.*@(container-engine-robot|compute-system|cloud-ml\.google|cloudbuild)\.(iam\.)?gserviceaccount\.com$)/.test(member)
    || /@gcp-sa-[^.]+\.iam\.gserviceaccount\.com$/.test(member);
}

// iamPrincipalType is Google's Type column's word for a member's prefix.
function iamPrincipalType(member) {
  const kind = member.split(":")[0];
  return ({ user: "User", group: "Group", serviceAccount: "Service account", domain: "Domain",
    allUsers: "All users", allAuthenticatedUsers: "All authenticated users", principal: "Principal",
    principalSet: "Principal set", deleted: "Deleted" })[kind] || kind;
}

function iamRoleLabel(roles, name) {
  const r = (roles || []).find((x) => x.name === name);
  return r && r.title ? r.title : name;
}

// iamRows is the policy by principal: each principal's own roles and the
// roles it is granted above the project.
function iamRows(data) {
  const byMember = new Map();
  const add = (member, entry) => {
    if (!byMember.has(member)) byMember.set(member, { member, roles: [] });
    byMember.get(member).roles.push(entry);
  };
  for (const b of data.bindings || []) {
    for (const m of b.members || []) add(m, { role: b.role, condition: b.conditional || null, from: "" });
  }
  for (const a of data.inherited || []) {
    for (const b of a.bindings || []) {
      for (const m of b.members || []) {
        add(m, { role: b.role, condition: b.conditional || null, from: a.display, fromResource: a.resource });
      }
    }
  }
  return [...byMember.values()].sort((a, b) => a.member.localeCompare(b.member));
}

function renderIAMPage(view, route) {
  const project = currentProject();
  if (!project) {
    setChildren(view, pageHeader("IAM", ""), emptyState("No project is selected", "Choose a project in the toolbar."));
    return;
  }
  let data = null;
  let roles = null;
  let byRoles = new URLSearchParams(location.search).get("view") === "roles";
  let denyView = new URLSearchParams(location.search).get("view") === "deny";
  let includeGoogle = false;
  const selected = new Set();

  const body = el("div", { id: "iam-body" }, el("p", { class: "unavailable", text: "reading…" }));
  const grant = createButton("Grant access", () => openIAMGrantPanel(data, rolesNow, reload));
  grant.id = "iam-grant-access";
  const removeButton = el("button", { class: "header-button", type: "button", id: "iam-remove-access",
    disabled: "disabled", text: "Remove access", onclick: () => removeSelected() });
  const header = pageHeader("IAM", "", { actions: [grant, removeButton, refreshButton(() => reload())] });

  const rolesNow = async () => {
    if (!roles) roles = await iamCall("roles");
    return roles;
  };

  const tabs = el("div", { class: "tab-strip", role: "tablist", "aria-label": "IAM views" });
  const tabSelected = (id) => id === "deny" ? denyView : !denyView && (id === "roles") === byRoles;
  const drawTabs = () => setChildren(tabs, ...[["principals", "View by principals"], ["roles", "View by roles"], ["deny", "Deny"]].map(([id, label]) =>
    el("button", { type: "button", role: "tab", id: `iam-tab-${id}`, class: "tab" + (tabSelected(id) ? " is-selected" : ""),
      "aria-selected": String(tabSelected(id)), text: label,
      onclick: () => {
        byRoles = id === "roles";
        denyView = id === "deny";
        const u = new URL(location.href);
        if (byRoles || denyView) u.searchParams.set("view", id); else u.searchParams.delete("view");
        history.replaceState(history.state, "", u.pathname + u.search);
        drawTabs();
        draw();
      } })));
  drawTabs();

  const google = el("input", { type: "checkbox", id: "iam-include-google",
    onchange: () => { includeGoogle = google.checked; draw(); } });
  const bar = filterBar({ noun: "", columns: () => byRoles ? ["Role", "Principal"] : ["Type", "Principal", "Name", "Role", "Inheritance"],
    onChange: () => draw() });
  bar.node.querySelector("input.filter").placeholder = "Filter";
  bar.node.querySelector("input.filter").setAttribute("aria-label", "Filter");

  const removeSelected = () => {
    const members = [...selected];
    if (!members.length || !data) return;
    const own = iamRows(data).filter((r) => members.includes(r.member));
    confirmDestructive({
      title: members.length === 1 ? `Remove access for ${members[0]}?` : `Remove access for ${members.length} principals?`,
      detail: `Every role ${members.length === 1 ? "this principal holds" : "these principals hold"} on project ${project} is removed. ` +
        "Roles inherited from a folder or the organization stay, and are removed there.",
      confirmWord: members.length === 1 ? members[0] : "remove",
      confirmLabel: "Remove",
      consequence: "",
      onConfirm: async () => {
        for (const r of own) {
          const removes = r.roles.filter((x) => !x.from).map((x) => ({ role: x.role, condition: x.condition }));
          if (!removes.length) continue;
          const op = recordOperation(`Remove access for ${r.member} on ${project}`);
          try {
            await iamCall("change", { etag: data.etag, members: [r.member], add: [], remove: removes });
            op.succeeded("");
          } catch (err) {
            op.failed(err.message);
            throw err;
          }
          data = await iamCall("policy");
        }
        selected.clear();
        notify(`Removed access for ${members.join(", ")}`);
        reload();
      },
    });
  };

  const roleCell = (r) => el("td", { class: "iam-roles" }, ...r.roles.map((x) =>
    el("div", { class: "iam-role" },
      el("span", { text: iamRoleLabel(roles, x.role) }),
      x.condition ? el("span", { class: "form-help", text: ` (condition: ${x.condition.title})` }) : null)));
  const inheritCell = (r) => el("td", {}, ...r.roles.map((x) =>
    el("div", { class: "iam-role", text: x.from || "" })));

  const allowOnly = [];
  const draw = () => {
    for (const n of allowOnly) n.hidden = denyView;
    grant.hidden = denyView;
    removeButton.hidden = denyView;
    if (denyView) {
      renderIAMDenyTab(body, project);
      return;
    }
    if (!data) {
      setChildren(body, el("p", { class: "unavailable", text: "reading…" }));
      reload();
      return;
    }
    selected.clear();
    removeButton.disabled = true;
    let rows = iamRows(data).filter((r) => includeGoogle || !iamGoogleProvided(r.member));
    const name = (m) => (data.names || {})[m] || "";
    if (!byRoles) {
      rows = rows.filter((r) => bar.matches((col) => {
        const all = { Type: iamPrincipalType(r.member), Principal: r.member, Name: name(r.member),
          Role: r.roles.map((x) => `${x.role} ${iamRoleLabel(roles, x.role)}`).join(" "),
          Inheritance: r.roles.map((x) => x.from).join(" ") };
        return col === null ? Object.values(all) : all[col] || "";
      }));
      const table = el("table", { id: "iam-principals-table" },
        el("thead", {}, el("tr", {},
          el("th", {}, el("span", { class: "sr-only", text: "Select" })),
          el("th", { text: "Type" }), el("th", { text: "Principal" }), el("th", { text: "Name" }),
          el("th", { text: "Role" }), el("th", { text: "Inheritance" }),
          el("th", {}, el("span", { class: "sr-only", text: "Edit" })))),
        el("tbody", {}, ...rows.map((r) => {
          const own = r.roles.some((x) => !x.from);
          const check = el("input", { type: "checkbox", "aria-label": `Select ${r.member}`,
            disabled: own ? null : "disabled",
            onchange: () => {
              if (check.checked) selected.add(r.member); else selected.delete(r.member);
              removeButton.disabled = selected.size === 0;
            } });
          return el("tr", { "data-member": r.member },
            el("td", {}, check),
            el("td", { text: iamPrincipalType(r.member) }),
            el("td", { class: "mono", text: r.member.replace(/^[a-zA-Z]+:/, "") }),
            el("td", { text: name(r.member) }),
            roleCell(r), inheritCell(r),
            el("td", { class: "row-actions" },
              el("button", { type: "button", class: "secondary", text: "Edit principal",
                "aria-label": `Edit principal ${r.member}`,
                onclick: () => openIAMEditPanel(data, r, rolesNow, reload) })));
        })));
      setChildren(body, rows.length ? el("div", { class: "table-wrap" }, table)
        : emptyState("No principals", "No principal matches. Grant access adds one."));
      return;
    }
    // View by roles: each role, then its principals.
    const byRole = new Map();
    for (const r of rows) {
      for (const x of r.roles) {
        const key = x.role;
        if (!byRole.has(key)) byRole.set(key, []);
        byRole.get(key).push({ member: r.member, from: x.from, condition: x.condition });
      }
    }
    const names = [...byRole.keys()].sort((a, b) => iamRoleLabel(roles, a).localeCompare(iamRoleLabel(roles, b)))
      .filter((role) => bar.matches((col) => {
        const all = { Role: `${role} ${iamRoleLabel(roles, role)}`, Principal: byRole.get(role).map((m) => m.member).join(" ") };
        return col === null ? Object.values(all) : all[col] || "";
      }));
    const tbody = el("tbody");
    for (const role of names) {
      const members = byRole.get(role);
      const sub = members.map((m) => el("tr", { class: "iam-role-member", hidden: true },
        el("td", {}), el("td", { class: "mono", text: m.member }),
        el("td", { text: (m.from ? `Inherited from ${m.from}` : "") + (m.condition ? ` (condition: ${m.condition.title})` : "") })));
      const toggle = el("button", { type: "button", class: "secondary", "aria-expanded": "false",
        "aria-label": `Show the principals of ${role}`, text: "▸",
        onclick: () => {
          const open = toggle.getAttribute("aria-expanded") !== "true";
          toggle.setAttribute("aria-expanded", String(open));
          toggle.textContent = open ? "▾" : "▸";
          for (const s of sub) s.hidden = !open;
        } });
      tbody.append(el("tr", { "data-role": role },
        el("td", {}, toggle),
        el("td", {}, el("span", { text: `${iamRoleLabel(roles, role)} ` }), el("span", { class: "mono form-help", text: `(${members.length})` }),
          el("div", { class: "mono form-help", text: role })),
        el("td", { text: members.length === 1 ? "1 principal" : `${members.length} principals` })), ...sub);
    }
    setChildren(body, names.length
      ? el("div", { class: "table-wrap" }, el("table", { id: "iam-roles-table" },
          el("thead", {}, el("tr", {}, el("th", {}, el("span", { class: "sr-only", text: "Expand" })),
            el("th", { text: "Role / Principal" }), el("th", { text: "Principals" }))), tbody))
      : emptyState("No roles", "No role is granted. Grant access adds one."));
  };

  const reload = async () => {
    if (denyView) {
      draw();
      return;
    }
    try {
      const [d, r] = await Promise.all([iamCall("policy"), rolesNow()]);
      data = d;
      roles = r;
      draw();
    } catch (err) {
      setChildren(body, errorState("IAM unavailable", String(err.message), () => reload()));
    }
  };

  const actionBar = el("div", { class: "action-bar" }, bar.node,
    el("label", { class: "check-line" }, google, el("span", { text: "Include Google-provided role grants" })));
  allowOnly.push(actionBar, bar.chips);
  setChildren(view, header,
    el("h2", { id: "iam-scope-title", text: `Permissions for project "${project}"` }),
    el("p", { class: "subtitle", text: "These permissions affect this project and all of its resources." }),
    noteBanner(PROJECT_IAM_NOTE_TEXT()),
    tabs,
    actionBar,
    bar.chips,
    body,
    el("p", { class: "form-help" },
      el("span", { text: "Test permissions and the condition linter are on " }),
      el("a", { href: `/iam-admin/iam/${encodeURIComponent(project)}?tab=permissions`, text: "the policy's Permissions tab" }),
      el("span", { text: ". " }),
      el("a", { href: "/iam-admin/troubleshooter", text: "Policy Troubleshooter" }),
      el("span", { text: " explains a principal's access, and " }),
      el("a", { href: "/iam-admin/analyzer", text: "Policy Analyzer" }),
      el("span", { text: " finds who holds a permission." })));
  announce("IAM");
  reload();
}

// renderIAMDenyTab is the IAM page's Deny tab (#1889), as Google lays it
// out (https://docs.cloud.google.com/iam/docs/deny-access): the deny
// policies attached to the project, or to a folder or the organization
// above it picked in the resource selector, by Policy ID; Create deny
// policy; a policy's details with Edit and Delete. Every call goes to the
// registry IAM v2's Policies API serves (cmd/cloudburrow/consoleiamdeny.go).
let IAM_DENY_RESOURCE = "";
function renderIAMDenyTab(into, project) {
  setChildren(into, el("p", { class: "unavailable", text: "reading…" }));
  const load = async () => {
    let list;
    try {
      list = await iamCall("deny-list", { resource: IAM_DENY_RESOURCE });
    } catch (err) {
      if (IAM_DENY_RESOURCE) { IAM_DENY_RESOURCE = ""; return load(); }
      setChildren(into, errorState("Deny policies unavailable", String(err.message), () => load()));
      return;
    }
    IAM_DENY_RESOURCE = list.resource;
    const picker = el("select", { id: "iam-deny-resource", "aria-label": "Resource",
      onchange: () => { IAM_DENY_RESOURCE = picker.value; load(); } },
      ...list.resources.map((r) => el("option", { value: r.resource, text: `${r.kind}: ${r.display}`,
        selected: r.resource === list.resource ? "selected" : null })));
    const create = el("button", { type: "button", class: "primary", id: "iam-deny-create", text: "Create deny policy",
      onclick: () => openIAMDenyPanel(list.resource, null, load) });
    const rows = list.policies.map((p) => el("tr", { "data-deny-policy": p.id },
      el("td", {}, el("button", { type: "button", class: "link-button is-inline", text: p.id, "aria-label": `Open deny policy ${p.id}`,
        onclick: () => openIAMDenyDetails(list.resource, p.name, load) })),
      el("td", { text: p.displayName || "" }),
      el("td", { text: String(p.ruleCount) }),
      el("td", { text: p.updated || "" })));
    setChildren(into,
      el("h3", { text: "Deny policies" }),
      el("p", { class: "form-help", text: "Deny policies prevent principals from using certain permissions, regardless of the roles they're granted. " +
        "A deny policy attached to a folder or the organization applies to every project below it." }),
      el("div", { class: "action-bar" }, el("label", { class: "check-line" }, el("span", { text: "Resource " }), picker), create),
      rows.length ? el("div", { class: "table-wrap" }, el("table", { id: "iam-deny-table" },
        el("thead", {}, el("tr", {}, el("th", { text: "Policy ID" }), el("th", { text: "Display name" }),
          el("th", { text: "Deny rules" }), el("th", { text: "Last updated" }))),
        el("tbody", {}, ...rows)))
        : emptyState("No deny policies", "No deny policy is attached to this resource. Create deny policy adds one."));
  };
  load();
}

// iamDenyLines splits a field of principals or permissions, one per line
// or comma-separated.
function iamDenyLines(v) {
  return v.split(/[\n,]+/).map((x) => x.trim()).filter(Boolean);
}

async function openIAMDenyDetails(resource, name, onDone) {
  let p;
  try { p = await iamCall("deny-get", { name }); } catch (err) { notify(err.message, "error"); return; }
  MODAL_SIDE_NEXT = true;
  const { dialog, close } = openModal({ labelledBy: "iam-deny-details-title" });
  const list = (label, xs) => el("div", {}, el("span", { class: "form-label", text: label }),
    xs.length ? el("ul", {}, ...xs.map((x) => el("li", { class: "mono", text: x }))) : el("p", { class: "form-help", text: "None" }));
  const edit = el("button", { type: "button", class: "primary", id: "iam-deny-edit", text: "Edit",
    onclick: () => { close(); openIAMDenyPanel(resource, p, onDone); } });
  const del = el("button", { type: "button", class: "secondary", id: "iam-deny-delete", text: "Delete",
    onclick: () => {
      close();
      confirmDestructive({
        title: `Delete deny policy ${p.id}?`,
        detail: "The principals it denies regain the permissions their roles grant.",
        confirmWord: p.id, confirmLabel: "Delete", consequence: "",
        onConfirm: async () => {
          const op = recordOperation(`Delete deny policy ${p.id}`);
          try { await iamCall("deny-delete", { name: p.name, etag: p.etag }); op.succeeded(""); } catch (err) { op.failed(err.message); throw err; }
          notify(`Deleted deny policy ${p.id}`);
          onDone();
        },
      });
    } });
  dialog.append(el("div", { class: "modal-body", id: "iam-deny-details" },
    el("h2", { id: "iam-deny-details-title", text: `Deny policy ${p.id}` }),
    el("dl", { class: "summary" },
      el("dt", { text: "Policy ID" }), el("dd", { class: "mono", text: p.id }),
      el("dt", { text: "Display name" }), el("dd", { text: p.displayName || "" }),
      el("dt", { text: "Created" }), el("dd", { text: p.created || "" }),
      el("dt", { text: "Last updated" }), el("dd", { text: p.updated || "" })),
    el("h3", { text: "Deny rules" }),
    ...(p.rules || []).map((r, i) => el("div", { class: "iam-deny-rule", "data-rule": String(i + 1) },
      el("h4", { text: `Deny rule ${i + 1}` + (r.description ? `: ${r.description}` : "") }),
      list("Denied principals", r.deniedPrincipals), list("Exception principals", r.exceptionPrincipals),
      list("Denied permissions", r.deniedPermissions), list("Exception permissions", r.exceptionPermissions),
      r.condition ? el("div", {}, el("span", { class: "form-label", text: "Denial condition" }),
        el("p", { text: r.condition.title || "" }), el("pre", { class: "mono", text: r.condition.expression })) : null)),
    el("div", { class: "modal-actions" },
      el("button", { type: "button", class: "secondary", text: "Close", onclick: () => close() }), del, edit)));
}

// openIAMDenyPanel is Create deny policy (policy null) or its Edit.
function openIAMDenyPanel(resource, policy, onDone) {
  MODAL_SIDE_NEXT = true;
  let submitting = false;
  const { dialog, close } = openModal({ labelledBy: "iam-deny-panel-title", canClose: () => !submitting });
  const error = el("p", { class: "form-error", role: "alert", hidden: true });
  const display = el("input", { type: "text", id: "iam-deny-display-name", value: policy ? policy.displayName || "" : "" });
  const id = el("input", { type: "text", id: "iam-deny-id", value: policy ? policy.id : "", disabled: policy ? "disabled" : null });
  let idTouched = !!policy;
  id.addEventListener("input", () => { idTouched = true; });
  display.addEventListener("input", () => {
    if (!idTouched) id.value = display.value.toLowerCase().replace(/[^a-z0-9.-]+/g, "-").replace(/^[^a-z]+/, "").slice(0, 63);
  });
  const rulesBox = el("div", { id: "iam-deny-rules" });
  const rules = [];
  const field = (label, cls, values, help) => {
    const t = el("textarea", { class: cls, rows: 2, "aria-label": label });
    t.value = (values || []).join("\n");
    return { node: el("label", { class: "form-label" }, el("span", { text: label }), t, help ? el("span", { class: "form-help", text: help }) : null), t };
  };
  const addRule = (r) => {
    r = r || {};
    const dp = field("Denied principals", "iam-deny-denied-principals", r.deniedPrincipals,
      "v2 principal identifiers, one per line, such as principal://goog/subject/ada@example.com or principalSet://goog/group/admins@example.com.");
    const ep = field("Exception principals", "iam-deny-exception-principals", r.exceptionPrincipals);
    const dperm = field("Denied permissions", "iam-deny-denied-permissions", r.deniedPermissions,
      "Fully qualified permissions, one per line, such as cloudresourcemanager.googleapis.com/projects.delete.");
    const eperm = field("Exception permissions", "iam-deny-exception-permissions", r.exceptionPermissions);
    const c = r.condition || {};
    const ctitle = el("input", { type: "text", class: "iam-deny-condition-title", "aria-label": "Condition title", value: c.title || "" });
    const cdesc = el("input", { type: "text", class: "iam-deny-condition-description", "aria-label": "Condition description", value: c.description || "" });
    const cexpr = el("textarea", { class: "iam-deny-condition", rows: 2, "aria-label": "Denial condition" });
    cexpr.value = c.expression || "";
    const cond = el("details", { class: "iam-deny-condition-box", open: c.expression ? "open" : null },
      el("summary", { text: "Add denial condition" }),
      el("label", { class: "form-label" }, el("span", { text: "Title" }), ctitle),
      el("label", { class: "form-label" }, el("span", { text: "Description" }), cdesc),
      el("label", { class: "form-label" }, el("span", { text: "Condition editor" }), cexpr,
        el("span", { class: "form-help", text: "Resource tag functions only, such as resource.matchTag('123456789012/env', 'prod')." })));
    const entry = {};
    const node = el("fieldset", { class: "iam-deny-rule-form" },
      el("legend", { text: "Deny rule" }),
      dp.node, ep.node, dperm.node, eperm.node, cond,
      el("button", { type: "button", class: "secondary iam-deny-rule-delete", text: "Delete",
        onclick: () => { rules.splice(rules.indexOf(entry), 1); node.remove(); } }));
    entry.value = () => ({ deniedPrincipals: iamDenyLines(dp.t.value), exceptionPrincipals: iamDenyLines(ep.t.value),
      deniedPermissions: iamDenyLines(dperm.t.value), exceptionPermissions: iamDenyLines(eperm.t.value),
      condition: cexpr.value.trim() ? { title: ctitle.value, description: cdesc.value, expression: cexpr.value } : null,
      description: r.description || "" });
    rules.push(entry);
    rulesBox.append(node);
  };
  for (const r of (policy && policy.rules) || []) addRule(r);
  if (!rules.length) addRule(null);
  const primary = el("button", { type: "submit", class: "primary", id: "iam-deny-save", text: policy ? "Save" : "Create" });
  const submit = async (e) => {
    e.preventDefault();
    if (submitting) return;
    error.hidden = true;
    submitting = true;
    primary.disabled = true;
    const body = { resource, displayName: display.value, rules: rules.map((r) => r.value()) };
    const label = policy ? `Edit deny policy ${policy.id}` : `Create deny policy ${id.value}`;
    const op = recordOperation(label);
    try {
      if (policy) await iamCall("deny-update", { ...body, name: policy.name, etag: policy.etag });
      else await iamCall("deny-create", { ...body, id: id.value.trim() });
      op.succeeded("");
      notify(policy ? `Saved deny policy ${policy.id}` : `Created deny policy ${id.value.trim()}`);
      submitting = false;
      close();
      onDone();
    } catch (err) {
      op.failed(err.message);
      error.textContent = err.message;
      error.hidden = false;
      submitting = false;
      primary.disabled = false;
    }
  };
  dialog.append(el("form", { class: "modal-body", novalidate: true, onsubmit: submit },
    el("h2", { id: "iam-deny-panel-title", text: policy ? `Edit deny policy ${policy.id}` : "Create deny policy" }),
    error,
    el("h3", { text: "Policy name" }),
    el("label", { class: "form-label", for: "iam-deny-display-name", text: "Display name" }), display,
    el("label", { class: "form-label", for: "iam-deny-id", text: "ID" }), id,
    el("p", { class: "form-help", text: "3 to 63 lowercase letters, numbers, dashes and periods, starting with a letter. It cannot be changed later." }),
    el("h3", { text: "Deny rules" }),
    rulesBox,
    el("button", { type: "button", class: "secondary", id: "iam-deny-add-rule", text: "Add deny rule", onclick: () => addRule(null) }),
    el("div", { class: "modal-actions" },
      el("button", { type: "button", class: "secondary", text: "Cancel", onclick: () => close() }), primary)));
  display.focus();
}

// renderOrgPolicyEdit is the Organization Policies page's Edit policy
// (#1889), as Google lays it out: Policy source (Inherit parent's policy,
// Google-managed default, Override parent's policy), a list constraint's
// Policy enforcement (Merge with parent or Replace), its rules (Add a
// rule: Enforcement for a boolean constraint; Policy values, Policy type
// and custom values for a list one; each with an optional condition), and
// Set policy; the Dry run tab edits the dry-run policy the same way. Over
// the Org Policy v2 registry (cmd/cloudburrow/consoleorgpolicyedit.go).
function renderOrgPolicyEdit(view, constraint) {
  const project = currentProject();
  const back = `/iam-admin/orgpolicies/${encodeURIComponent(constraint)}`;
  if (!project) {
    setChildren(view, pageHeader("Edit policy", ""), emptyState("No project is selected", "Choose a project in the toolbar."));
    return;
  }
  const call = (name, body) => send(`/api/studio/iam-orgpolicies?call=${encodeURIComponent(name)}&project=${encodeURIComponent(project)}`, "POST", body);
  let dryRun = new URLSearchParams(location.search).get("tab") === "dryrun";
  const body = el("div", { id: "orgpolicy-edit" }, el("p", { class: "unavailable", text: "reading…" }));
  setChildren(view, pageHeader("Edit policy", "", { back, backLabel: "Policy details" }), body);
  announce("Edit policy");
  const draw = (d) => {
    const spec = dryRun ? d.dryRunSpec : d.spec;
    const error = el("p", { class: "form-error", role: "alert", hidden: true });
    const radios = (name, choices, current, onchange) => el("div", { class: "radio-group", role: "radiogroup" },
      ...choices.map(([value, label]) => {
        const r = el("input", { type: "radio", name, value, id: `${name}-${value}`, checked: value === current ? "checked" : null, onchange });
        return el("label", { class: "check-line", for: `${name}-${value}` }, r, el("span", { text: label }));
      }));
    const picked = (name) => (body.querySelector(`input[name="${name}"]:checked`) || {}).value;
    const rulesBox = el("div", { id: "orgpolicy-rules" });
    const rules = [];
    let n = 0;
    const addRule = (r) => {
      r = r || { enforce: true, values: "allowAll", type: "allow", custom: [] };
      const i = n++;
      const entry = {};
      const custom = el("textarea", { class: "orgpolicy-custom-values", rows: 2, "aria-label": "Custom values" });
      custom.value = (r.custom || []).join("\n");
      const c = r.condition || {};
      const ctitle = el("input", { type: "text", class: "orgpolicy-condition-title", "aria-label": "Condition title", value: c.title || "" });
      const cexpr = el("textarea", { class: "orgpolicy-condition", rows: 2, "aria-label": "Condition" });
      cexpr.value = c.expression || "";
      const customBox = el("div", { class: "orgpolicy-custom" },
        el("span", { class: "form-label", text: "Policy type" }),
        radios(`orgpolicy-type-${i}`, [["allow", "Allow"], ["deny", "Deny"]], r.type || "allow"),
        el("label", { class: "form-label" }, el("span", { text: "Custom values" }), custom,
          el("span", { class: "form-help", text: "One per line, such as in:us-locations or us-east1." })));
      const showCustom = () => { customBox.hidden = picked(`orgpolicy-values-${i}`) !== "custom"; };
      const kind = d.boolean
        ? el("div", {}, el("span", { class: "form-label", text: "Enforcement" }),
            radios(`orgpolicy-enforce-${i}`, [["on", "On"], ["off", "Off"]], r.enforce ? "on" : "off"))
        : el("div", {}, el("span", { class: "form-label", text: "Policy values" }),
            radios(`orgpolicy-values-${i}`, [["allowAll", "Allow all"], ["denyAll", "Deny all"], ["custom", "Custom"]], r.values || "allowAll", showCustom),
            customBox);
      const node = el("fieldset", { class: "orgpolicy-rule", "data-rule": String(i) },
        el("legend", { text: "Rule" }), kind,
        el("details", { class: "orgpolicy-condition-box", open: c.expression ? "open" : null },
          el("summary", { text: "Add condition" }),
          el("label", { class: "form-label" }, el("span", { text: "Title" }), ctitle),
          el("label", { class: "form-label" }, el("span", { text: "Condition" }), cexpr,
            el("span", { class: "form-help", text: "A CEL expression over the resource's tags, such as resource.matchTag('123456789012/env', 'prod')." }))),
        el("button", { type: "button", class: "secondary orgpolicy-rule-delete", text: "Delete rule",
          onclick: () => { rules.splice(rules.indexOf(entry), 1); node.remove(); } }));
      entry.value = () => ({
        enforce: picked(`orgpolicy-enforce-${i}`) === "on",
        values: picked(`orgpolicy-values-${i}`), type: picked(`orgpolicy-type-${i}`),
        custom: iamDenyLines(custom.value),
        condition: cexpr.value.trim() ? { title: ctitle.value, expression: cexpr.value } : null,
      });
      rules.push(entry);
      rulesBox.append(node);
      if (!d.boolean) showCustom();
    };
    const overrideBox = el("div", { id: "orgpolicy-override" });
    const showOverride = () => { overrideBox.hidden = picked("orgpolicy-source") !== "override"; };
    const source = radios("orgpolicy-source", [["inherit", "Inherit parent's policy"], ["default", "Google-managed default"],
      ["override", "Override parent's policy"]], spec.source, showOverride);
    setChildren(overrideBox,
      d.boolean ? null : el("div", {}, el("h3", { text: "Policy enforcement" }),
        radios("orgpolicy-enforcement", [["merge", "Merge with parent"], ["replace", "Replace"]], spec.merge ? "merge" : "replace")),
      el("h3", { text: "Rules" }), rulesBox,
      el("button", { type: "button", class: "secondary", id: "orgpolicy-add-rule", text: "Add a rule", onclick: () => addRule(null) }));
    for (const r of spec.rules) addRule(r);
    const save = el("button", { type: "submit", class: "primary", id: "orgpolicy-set", text: dryRun ? "Set dry run policy" : "Set policy" });
    const tabs = el("div", { class: "tab-strip", role: "tablist", "aria-label": "Policy" },
      ...[["policy", "Policy"], ["dryrun", "Dry run"]].map(([id, label]) => el("button", { type: "button", role: "tab", id: `orgpolicy-tab-${id}`,
        class: "tab" + ((id === "dryrun") === dryRun ? " is-selected" : ""), "aria-selected": String((id === "dryrun") === dryRun), text: label,
        onclick: () => { dryRun = id === "dryrun"; draw(d); } })));
    const form = el("form", { class: "create-form", novalidate: true, onsubmit: async (e) => {
      e.preventDefault();
      error.hidden = true;
      save.disabled = true;
      const op = recordOperation(`Set ${dryRun ? "dry run " : ""}policy ${d.constraint} on ${project}`);
      try {
        await call("policy-set", { constraint: d.constraint, dryRun, etag: d.etag, source: picked("orgpolicy-source"),
          merge: picked("orgpolicy-enforcement") === "merge", rules: rules.map((r) => r.value()) });
        op.succeeded("");
        notify(`Set the policy for ${d.constraint}`);
        navigate(back);
      } catch (err) {
        op.failed(err.message);
        error.textContent = err.message;
        error.hidden = false;
        save.disabled = false;
      }
    } },
      error,
      el("h3", { text: "Policy source" }), source, overrideBox,
      el("div", { class: "form-actions" }, save,
        el("button", { type: "button", class: "secondary", text: "Cancel", onclick: () => navigate(back) })));
    setChildren(body,
      el("h2", { id: "orgpolicy-title", text: d.displayName }),
      el("p", { class: "mono form-help", text: `${d.constraint} · ${d.boolean ? "Boolean" : "List"} constraint · applies to ${d.resource}` }),
      d.description ? el("p", { class: "form-help", text: d.description }) : null,
      noteBanner(d.note, "orgpolicy-edit"),
      tabs, form);
    showOverride();
  };
  call("policy-get", { constraint }).then(draw).catch((err) =>
    setChildren(body, errorState("Edit policy unavailable", String(err.message), () => renderOrgPolicyEdit(view, constraint))));
}

// orgPolicyCustomCall calls the Organization Policies page's custom
// constraint methods (cmd/cloudburrow/consoleorgpolicycustom.go, #1891).
function orgPolicyCustomCall(name, body) {
  const project = currentProject() || "";
  return send(`/api/studio/iam-orgpolicies?call=${encodeURIComponent(name)}&project=${encodeURIComponent(project)}`, "POST", body || {});
}

// renderOrgPolicyCustomList is the organization's custom constraints
// (#1891): each one's ID, display name, resource type, enforcement
// methods, action and last update, with Edit and Delete, and Custom
// constraint, which opens Create custom constraint.
function renderOrgPolicyCustomList(view) {
  const back = "/iam-admin/orgpolicies";
  const body = el("div", { id: "orgpolicy-custom-list" }, el("p", { class: "unavailable", text: "reading…" }));
  const header = pageHeader("Custom constraints", "Your organization's constraints: CEL conditions over a service's resources, enforced where a policy enforces them.",
    { back, backLabel: "Organization Policies", actions: [
      el("a", { class: "button primary", id: "orgpolicy-custom-create", href: "/iam-admin/orgpolicies/custom-constraints/create", text: "Custom constraint" })] });
  setChildren(view, header, body);
  announce("Custom constraints");
  const reload = () => renderOrgPolicyCustomList(view);
  orgPolicyCustomCall("custom-list").then((d) => {
    if (!d.constraints.length) {
      setChildren(body, emptyState("No custom constraints yet",
        "Create one with Custom constraint, or with gcloud org-policies set-custom-constraint; it will appear either way."));
      return;
    }
    const rows = d.constraints.map((c) => el("tr", { "data-constraint": c.id },
      el("td", {}, el("a", { href: `/iam-admin/orgpolicies/custom-constraints/${encodeURIComponent(c.id)}/edit`, class: "mono", text: c.id })),
      el("td", { text: c.displayName || "-" }),
      el("td", { class: "mono", text: c.resourceType }),
      el("td", { text: (c.methodTypes || []).join(", ") }),
      el("td", { text: c.actionType }),
      el("td", { class: "mono", text: c.condition }),
      el("td", { text: c.updateTime || "" }),
      el("td", {},
        el("a", { class: "button secondary", href: `/iam-admin/orgpolicies/custom-constraints/${encodeURIComponent(c.id)}/edit`, text: "Edit" }),
        el("button", { type: "button", class: "secondary danger", text: "Delete", "aria-label": `Delete ${c.id}`,
          onclick: () => confirmDestructive({
            title: `Delete ${c.id}?`,
            detail: `The custom constraint and every policy set on it are deleted.`,
            confirmWord: c.id,
            onConfirm: async () => {
              const op = recordOperation(`Delete custom constraint ${c.id}`);
              try {
                await orgPolicyCustomCall("custom-delete", { name: c.name });
                op.succeeded("");
              } catch (err) {
                op.failed(err.message);
                throw err;
              }
              notify(`Deleted ${c.id}`);
              reload();
            },
          }) }))));
    setChildren(body, el("table", { class: "data-table", id: "orgpolicy-custom-table" },
      el("thead", {}, el("tr", {}, ...["Constraint ID", "Display name", "Resource type", "Enforcement method", "Action", "Condition", "Last updated", ""]
        .map((h) => el("th", { text: h })))),
      el("tbody", {}, ...rows)));
  }).catch((err) => setChildren(body, errorState("Custom constraints unavailable", String(err.message), reload)));
}

// renderOrgPolicyCustomEdit is Create custom constraint (id null) or its
// edit page (#1891), as Google lays it out: Display name, Constraint ID,
// Description, Resource type, Enforcement method, Condition and Action.
function renderOrgPolicyCustomEdit(view, id) {
  const back = "/iam-admin/orgpolicies/custom-constraints";
  const title = id ? "Edit custom constraint" : "Create custom constraint";
  const body = el("div", { id: "orgpolicy-custom-edit" }, el("p", { class: "unavailable", text: "reading…" }));
  setChildren(view, pageHeader(title, "", { back, backLabel: "Custom constraints" }), body);
  announce(title);
  const draw = (types, c) => {
    const error = el("p", { class: "form-error", role: "alert", hidden: true });
    const field = (label, input, help) => el("label", { class: "form-field" }, el("span", { class: "form-label", text: label }), input,
      help ? el("span", { class: "form-help", text: help }) : null);
    const display = el("input", { type: "text", id: "custom-display-name", value: c.displayName || "", maxlength: 200 });
    const cid = el("input", { type: "text", id: "custom-id", value: (c.id || "").replace(/^custom\./, ""), disabled: id ? "disabled" : null });
    const desc = el("textarea", { id: "custom-description", rows: 2, maxlength: 2000 });
    desc.value = c.description || "";
    const rtype = el("select", { id: "custom-resource-type" }, ...types.map((t) => el("option", { value: t, text: t, selected: t === c.resourceType ? "selected" : null })));
    const updating = (c.methodTypes || []).includes("UPDATE");
    const methods = el("div", { class: "radio-group", role: "radiogroup", "aria-label": "Enforcement method" },
      ...[["create", "Enforce on create"], ["update", "Enforce on create and update"]].map(([v, label]) => el("label", { class: "check-line", for: `custom-method-${v}` },
        el("input", { type: "radio", name: "custom-method", value: v, id: `custom-method-${v}`, checked: (v === "update") === updating ? "checked" : null }),
        el("span", { text: label }))));
    const cond = el("textarea", { id: "custom-condition", rows: 4, class: "mono", maxlength: 1500 });
    cond.value = c.condition || "";
    const action = el("div", { class: "radio-group", role: "radiogroup", "aria-label": "Action" },
      ...[["ALLOW", "Allow"], ["DENY", "Deny"]].map(([v, label]) => el("label", { class: "check-line", for: `custom-action-${v}` },
        el("input", { type: "radio", name: "custom-action", value: v, id: `custom-action-${v}`, checked: v === (c.actionType || "ALLOW") ? "checked" : null }),
        el("span", { text: label }))));
    const save = el("button", { type: "submit", class: "primary", id: "custom-save", text: id ? "Save" : "Create constraint" });
    const picked = (name) => (body.querySelector(`input[name="${name}"]:checked`) || {}).value;
    const form = el("form", { class: "create-form", novalidate: true, onsubmit: async (e) => {
      e.preventDefault();
      error.hidden = true;
      const name = id || ("custom." + cid.value.trim());
      if (!id && !/^[A-Za-z0-9]{1,70}$/.test(cid.value.trim())) {
        error.textContent = "The constraint ID is up to 70 letters and digits, after custom.";
        error.hidden = false;
        return;
      }
      save.disabled = true;
      const op = recordOperation(`${id ? "Update" : "Create"} custom constraint ${name}`);
      try {
        await orgPolicyCustomCall("custom-set", { name, create: !id, displayName: display.value, description: desc.value,
          resourceType: rtype.value, methodTypes: picked("custom-method") === "update" ? ["CREATE", "UPDATE"] : ["CREATE"],
          condition: cond.value, actionType: picked("custom-action") });
        op.succeeded("");
        notify(`${id ? "Saved" : "Created"} ${name}`);
        navigate(back);
      } catch (err) {
        op.failed(err.message);
        error.textContent = err.message;
        error.hidden = false;
        save.disabled = false;
      }
    } },
      error,
      field("Display name", display),
      field("Constraint ID", el("div", { class: "input-prefix" }, el("span", { class: "mono", text: "custom." }), cid),
        "Up to 70 letters and digits. It cannot be changed later."),
      field("Description", desc, "Shown as the error message when the policy is violated."),
      field("Resource type", rtype),
      el("h3", { text: "Enforcement method" }), methods,
      field("Condition", cond, "A CEL condition over the resource, such as resource.versioning.enabled == true."),
      el("h3", { text: "Action" }), action,
      el("div", { class: "form-actions" }, save,
        el("button", { type: "button", class: "secondary", text: "Cancel", onclick: () => navigate(back) })));
    setChildren(body, form);
  };
  const loads = [orgPolicyCustomCall("custom-list")];
  if (id) loads.push(orgPolicyCustomCall("custom-get", { name: id }));
  Promise.all(loads).then(([list, c]) => draw(list.resourceTypes, c || {}))
    .catch((err) => setChildren(body, errorState(`${title} unavailable`, String(err.message), () => renderOrgPolicyCustomEdit(view, id))));
}

// iamEnforcementNote says whether this instance enforces what the
// Troubleshooter and Analyzer explain (#1879).
function iamEnforcementNote(into) {
  iamCall("enforcement").then((r) => {
    setChildren(into, noteBanner(r.mode === "on"
      ? "IAM enforcement is on (up --iam-enforcement=on): the mapped methods of Secret Manager, Cloud KMS and Cloud Tasks are refused as this explains."
      : "IAM enforcement is off: policies are stored and explained here, and no call is refused because of them. Start the instance with up --iam-enforcement=on to enforce them.",
      "iam-enforcement-" + r.mode));
  }).catch(() => {});
}

// renderIAMTroubleshooter is Policy Troubleshooter (#1879): a principal, a
// resource and a permission in; whether the principal has it, and each
// allow policy's bindings with why each grants it or not.
function renderIAMTroubleshooter(view) {
  const project = currentProject();
  const note = el("div", { id: "iam-enforcement-note" });
  const fields = buildCreateForm({ label: "Troubleshoot access", fields: [
    { name: "principal", label: "Principal email", required: true,
      help: "The email of a user or service account, such as worker@my-project.iam.gserviceaccount.com." },
    { name: "resource", label: "Resource", required: true, default: project ? `//cloudresourcemanager.googleapis.com/projects/${project}` : "",
      help: "A full resource name, such as //secretmanager.googleapis.com/projects/my-project/secrets/my-secret." },
    { name: "permission", label: "Permission", required: true, help: "Such as secretmanager.versions.access." },
  ] });
  const error = el("p", { class: "form-error", role: "alert", hidden: true });
  const results = el("div", { id: "troubleshoot-result", "aria-live": "polite" });
  const check = el("button", { type: "submit", class: "primary", id: "troubleshoot-check", text: "Check access" });
  const submit = async (e) => {
    e.preventDefault();
    error.hidden = true;
    if (!fields.validate()) return;
    check.disabled = true;
    try {
      const r = await iamCall("troubleshoot", fields.values());
      const granted = r.overallAccessState === "CAN_ACCESS";
      const rows = [];
      for (const p of (r.allowPolicyExplanation || {}).explainedPolicies || []) {
        for (const b of p.bindingExplanations || []) {
          const matched = Object.entries(b.memberships || {}).filter(([, m]) => m.membership === "MEMBERSHIP_MATCHED").map(([k]) => k);
          rows.push(el("tr", {},
            el("td", { class: "mono", text: p.fullResourceName }),
            el("td", { class: "mono", text: b.role }),
            el("td", { text: b.rolePermission === "ROLE_PERMISSION_INCLUDED" ? "Yes" : "No" }),
            el("td", { class: "mono", text: matched.join(", ") || "No" }),
            el("td", { class: "mono", text: b.condition ? `${b.condition.expression} (${b.conditionExplanation && b.conditionExplanation.value ? "true" : "false"})` : "" }),
            el("td", { text: b.allowAccessState === "ALLOW_ACCESS_STATE_GRANTED" ? "Grants access" : "Does not grant" })));
        }
      }
      setChildren(results,
        el("h2", { id: "troubleshoot-verdict", text: granted ? "Access granted" : "Access not granted" }),
        el("p", { class: "subtitle", text: `${fields.values().principal} ${granted ? "has" : "does not have"} ${fields.values().permission} on ${fields.values().resource}. No deny policy applies: deny policies are not stored locally.` }),
        rows.length ? el("table", { class: "table", id: "troubleshoot-bindings" },
          el("thead", {}, el("tr", {}, ...["Policy", "Role", "Role has permission", "Principal included", "Condition", "Result"].map((h) => el("th", { text: h })))),
          el("tbody", {}, ...rows)) : el("p", { class: "unavailable", text: "No allow policy on the resource or its ancestors has a binding." }));
      announce(granted ? "Access granted" : "Access not granted");
    } catch (err) {
      setChildren(results);
      error.textContent = err.message;
      error.hidden = false;
    }
    check.disabled = false;
  };
  setChildren(view, pageHeader("Policy Troubleshooter", ""), note,
    el("p", { class: "subtitle", text: "Check whether a principal has a permission on a resource, and which allow policies decide it." }),
    el("form", { class: "card", novalidate: true, onsubmit: submit }, error, ...fields.nodes, el("div", { class: "modal-actions" }, check)),
    results);
  iamEnforcementNote(note);
  announce("Policy Troubleshooter");
  fields.focusFirst();
}

// renderIAMAnalyzer is Policy Analyzer (#1879): who has what in the
// project's policies and its ancestors', by principal, permission or role.
function renderIAMAnalyzer(view) {
  const project = currentProject();
  const note = el("div", { id: "iam-enforcement-note" });
  const fields = buildCreateForm({ label: "Analyze", fields: [
    { name: "scope", label: "Scope", default: project ? `projects/${project}` : "", required: true,
      help: "projects/PROJECT_ID, folders/NUMBER or organizations/NUMBER." },
    { name: "identity", label: "Principal", help: "Such as serviceAccount:worker@my-project.iam.gserviceaccount.com, user:… or group:…" },
    { name: "permissions", label: "Permissions", help: "Comma-separated, such as secretmanager.versions.access." },
    { name: "roles", label: "Roles", help: "Comma-separated, such as roles/secretmanager.secretAccessor." },
    { name: "resource", label: "Resource", help: "A full resource name, to include its own policies." },
  ] });
  const error = el("p", { class: "form-error", role: "alert", hidden: true });
  const results = el("div", { id: "analyze-result", "aria-live": "polite" });
  const run = el("button", { type: "submit", class: "primary", id: "analyze-run", text: "Analyze" });
  const list = (v) => (v || "").split(/[\s,]+/).map((x) => x.trim()).filter(Boolean);
  const submit = async (e) => {
    e.preventDefault();
    error.hidden = true;
    if (!fields.validate()) return;
    const v = fields.values();
    run.disabled = true;
    try {
      const r = await iamCall("analyze", { scope: v.scope, identity: v.identity, permissions: list(v.permissions), roles: list(v.roles), resource: v.resource });
      const res = (r.mainAnalysis || {}).analysisResults || [];
      setChildren(results, res.length ? el("table", { class: "table", id: "analyze-table" },
        el("thead", {}, el("tr", {}, ...["Policy", "Role", "Principals", "Access", "Condition"].map((h) => el("th", { text: h })))),
        el("tbody", {}, ...res.map((x) => {
          const acl = (x.accessControlLists || [])[0] || {};
          return el("tr", {},
            el("td", { class: "mono", text: x.attachedResourceFullName }),
            el("td", { class: "mono", text: x.iamBinding.role }),
            el("td", { class: "mono", text: ((x.identityList || {}).identities || []).map((i) => i.name).join(", ") }),
            el("td", { class: "mono", text: (acl.accesses || []).map((a) => a.permission || a.role).join(", ") }),
            el("td", { class: "mono", text: x.iamBinding.condition ? `${x.iamBinding.condition.expression} (${(acl.conditionEvaluation || {}).evaluationValue || ""})` : "" }));
        }))) : el("p", { class: "unavailable", id: "analyze-empty", text: "No binding matches the query." }));
      announce(`${res.length} results`);
    } catch (err) {
      setChildren(results);
      error.textContent = err.message;
      error.hidden = false;
    }
    run.disabled = false;
  };
  setChildren(view, pageHeader("Policy Analyzer", ""), note,
    el("p", { class: "subtitle", text: "Find which bindings give a principal, a permission or a role, in the scope's policies and its ancestors'." }),
    el("form", { class: "card", novalidate: true, onsubmit: submit }, error, ...fields.nodes, el("div", { class: "modal-actions" }, run)),
    results);
  iamEnforcementNote(note);
  announce("Policy Analyzer");
  fields.focusFirst();
}

function PROJECT_IAM_NOTE_TEXT() {
  return "The project's IAM policy, stored and never enforced (ADR-0006). Nothing inherits it: a resource's own " +
    "Permissions tab shows only that resource's bindings. Google's Security insights (excess permissions) come from " +
    "the Recommender API, which CloudBurrow does not serve, so the column is not drawn.";
}

// iamRolePicker is Select a role: a searchable list of every role, by
// title and name, Google's role picker without its product groups.
function iamRolePicker(roles, initial, id) {
  let value = initial || "";
  const button = el("button", { type: "button", class: "secondary iam-role-button", id,
    "aria-haspopup": "listbox", "aria-expanded": "false" });
  const label = () => { button.textContent = value ? iamRoleLabel(roles, value) : "Select a role"; button.title = value; };
  label();
  const search = el("input", { type: "search", class: "iam-role-search", placeholder: "Filter", "aria-label": "Filter roles", autocomplete: "off" });
  const list = el("ul", { class: "iam-role-list", role: "listbox", "aria-label": "Roles" });
  const pop = el("div", { class: "iam-role-pop", hidden: true }, search, list);
  const drawList = () => {
    const q = search.value.trim().toLowerCase();
    const hits = roles.filter((r) => !q || r.name.toLowerCase().includes(q) || (r.title || "").toLowerCase().includes(q)
      || (r.type || "").toLowerCase() === q).slice(0, 100);
    setChildren(list, ...hits.map((r) => el("li", { role: "presentation" },
      el("button", { type: "button", role: "option", class: "iam-role-option", "data-role": r.name,
        "aria-selected": String(r.name === value),
        onclick: () => { value = r.name; label(); close(); onChange(); } },
        el("span", { class: "iam-role-title", text: r.title || r.name }),
        el("span", { class: "mono form-help", text: ` ${r.name}` }),
        el("span", { class: "form-help", text: r.type === "Custom" ? " Custom" : r.type === "Basic" ? " Basic" : "" })))),
      hits.length ? null : el("li", { class: "unavailable", text: "No role matches." }));
  };
  const close = () => { pop.hidden = true; button.setAttribute("aria-expanded", "false"); };
  let onChange = () => {};
  button.addEventListener("click", () => {
    pop.hidden = !pop.hidden;
    button.setAttribute("aria-expanded", String(!pop.hidden));
    if (!pop.hidden) { drawList(); search.focus(); }
  });
  search.addEventListener("input", drawList);
  search.addEventListener("keydown", (e) => { if (e.key === "Escape") { e.stopPropagation(); close(); button.focus(); } });
  return { node: el("div", { class: "iam-role-picker" }, button, pop), value: () => value,
    onChange: (fn) => { onChange = fn; } };
}

// iamRoleRow is one role of Grant access or Edit principal: Select a role,
// Add IAM condition and the Delete control.
function iamRoleRow(roles, entry, n, onDelete) {
  const picker = iamRolePicker(roles, entry ? entry.role : "", `iam-role-${n}`);
  const c = entry && entry.condition;
  const title = el("input", { type: "text", id: `iam-cond-title-${n}`, value: c ? c.title : "" });
  const desc = el("input", { type: "text", id: `iam-cond-desc-${n}`, value: c ? (c.description || "") : "" });
  const expr = el("textarea", { id: `iam-cond-expr-${n}`, rows: 3 });
  expr.value = c ? c.expression : "";
  const condBox = el("div", { class: "iam-condition", hidden: !c },
    el("label", { for: `iam-cond-title-${n}`, text: "Title" }), title,
    el("label", { for: `iam-cond-desc-${n}`, text: "Description" }), desc,
    el("label", { for: `iam-cond-expr-${n}`, text: "Condition editor (CEL)" }), expr,
    el("p", { class: "form-help", text: 'Such as request.time < timestamp("2027-01-01T00:00:00Z"). Evaluated on resource.name, ' +
      "resource.type, resource.service and request.time." }));
  const condToggle = el("button", { type: "button", class: "link-button", text: c ? "Remove IAM condition" : "Add IAM condition",
    onclick: () => {
      condBox.hidden = !condBox.hidden;
      condToggle.textContent = condBox.hidden ? "Add IAM condition" : "Remove IAM condition";
      if (condBox.hidden) { title.value = ""; desc.value = ""; expr.value = ""; }
    } });
  const del = el("button", { type: "button", class: "secondary danger", "aria-label": `Delete role ${n + 1}`, text: "Delete",
    onclick: () => onDelete(node) });
  const node = el("div", { class: "iam-role-row card", "data-n": String(n) },
    el("div", { class: "iam-role-line" }, el("span", { class: "form-label", text: "Role" }), picker.node, del),
    condToggle, condBox);
  return {
    node,
    value: () => {
      const cond = condBox.hidden ? null : { title: title.value.trim(), description: desc.value.trim(), expression: expr.value.trim() };
      return { role: picker.value(), condition: cond && (cond.title || cond.expression) ? cond : null };
    },
  };
}

// iamPanel draws Grant access or Edit principal as Google's side panel.
function iamPanel({ title, intro, principals, entries, inherited, onSave }) {
  MODAL_SIDE_NEXT = true;
  let submitting = false;
  const { dialog, close } = openModal({ labelledBy: "iam-panel-title", canClose: () => !submitting });
  const error = el("p", { class: "form-error", role: "alert", hidden: true });
  const rowsBox = el("div", { class: "iam-role-rows" });
  let n = 0;
  const rows = [];
  const addRow = (entry) => {
    const r = iamRoleRow(entry.roles, entry.entry, n++, (node) => {
      const i = rows.indexOf(r);
      if (i >= 0) rows.splice(i, 1);
      node.remove();
    });
    rows.push(r);
    rowsBox.append(r.node);
  };
  for (const e of entries.list) addRow({ roles: entries.roles, entry: e });
  if (!entries.list.length) addRow({ roles: entries.roles, entry: null });
  const another = el("button", { type: "button", class: "secondary", id: "iam-add-another-role", text: "Add another role",
    onclick: () => addRow({ roles: entries.roles, entry: null }) });
  const primary = el("button", { type: "submit", class: "primary", id: "iam-save", text: "Save" });
  const cancel = el("button", { type: "button", class: "secondary", text: "Cancel", onclick: () => close() });
  const submit = async (e) => {
    e.preventDefault();
    error.hidden = true;
    if (submitting) return;
    submitting = true;
    primary.disabled = true;
    try {
      await onSave(rows.map((r) => r.value()));
      submitting = false;
      close();
    } catch (err) {
      error.textContent = err.message;
      error.hidden = false;
      submitting = false;
      primary.disabled = false;
    }
  };
  dialog.append(el("form", { class: "modal-body", novalidate: true, onsubmit: submit },
    el("h2", { id: "iam-panel-title", text: title }),
    intro ? el("p", { class: "form-help", text: intro }) : null,
    error,
    principals,
    el("h3", { text: "Assign roles" }),
    el("p", { class: "form-help", text: "Roles are composed of sets of permissions and determine what the principal can do with this resource." }),
    rowsBox, another,
    inherited,
    el("div", { class: "modal-actions" }, cancel, primary)));
  const first = dialog.querySelector("textarea, .iam-role-button");
  if (first) first.focus();
}

async function openIAMGrantPanel(data, rolesNow, onDone) {
  if (!data) return;
  let roles;
  try { roles = await rolesNow(); } catch (err) { notify(err.message, "error"); return; }
  const members = el("textarea", { id: "iam-new-principals", rows: 2, required: true });
  iamPanel({
    title: `Grant access to "${data.project}"`,
    intro: "Grant principals access to this resource and add roles to specify what actions the principals can take.",
    principals: el("div", {},
      el("h3", { text: "Add principals" }),
      el("p", { class: "form-help", text: "Principals are users, groups, domains, or service accounts." }),
      el("label", { for: "iam-new-principals", class: "form-label", text: "New principals" }), members,
      el("p", { class: "form-help", text: "Such as user:ada@example.com, group:admins@example.com, " +
        "serviceAccount:sa@PROJECT.iam.gserviceaccount.com or domain:example.com; separate several with commas." })),
    entries: { roles, list: [] },
    inherited: null,
    onSave: async (entries) => {
      const list = members.value.split(/[\s,]+/).map((m) => m.trim()).filter(Boolean);
      if (!list.length) throw new Error("Add at least one principal.");
      const add = entries.filter((e) => e.role);
      if (!add.length) throw new Error("Select a role.");
      const op = recordOperation(`Grant access to ${list.join(", ")} on ${data.project}`);
      try {
        await iamCall("change", { etag: data.etag, members: list, add, remove: [] });
        op.succeeded("");
      } catch (err) {
        op.failed(err.message);
        throw err;
      }
      notify(`Granted access to ${list.join(", ")}`);
      onDone();
    },
  });
}

async function openIAMEditPanel(data, row, rolesNow, onDone) {
  let roles;
  try { roles = await rolesNow(); } catch (err) { notify(err.message, "error"); return; }
  const own = row.roles.filter((x) => !x.from);
  const inherited = row.roles.filter((x) => x.from);
  iamPanel({
    title: `Edit access to "${data.project}"`,
    intro: "",
    principals: el("div", {},
      el("span", { class: "form-label", text: "Principal" }),
      el("p", { class: "mono", id: "iam-edit-principal", text: row.member })),
    entries: { roles, list: own },
    inherited: inherited.length ? el("div", { class: "iam-inherited" },
      el("h3", { text: "Inherited roles" }),
      el("p", { class: "form-help", text: "Granted on a folder or the organization above the project; change them there." }),
      el("ul", {}, ...inherited.map((x) => el("li", {},
        el("span", { text: `${iamRoleLabel(roles, x.role)} ` }),
        el("span", { class: "form-help", text: `(${x.role}), inherited from ${x.from}` }))))) : null,
    onSave: async (entries) => {
      const add = entries.filter((e) => e.role);
      const op = recordOperation(`Edit principal ${row.member} on ${data.project}`);
      try {
        await iamCall("change", { etag: data.etag, members: [row.member], add,
          remove: own.map((x) => ({ role: x.role, condition: x.condition })) });
        op.succeeded("");
      } catch (err) {
        op.failed(err.message);
        throw err;
      }
      notify(`Saved the roles of ${row.member}`);
      onDone();
    },
  });
}

// --- Bigtable Studio (#1858) ------------------------------------------------
//
// An instance's Bigtable Studio, as Google's console draws it
// (https://docs.cloud.google.com/bigtable/docs/manage-data-using-console):
// the Explorer (tables with their column families and authorized views,
// logical and materialized views, each table with View actions), the Query
// builder (a table, a row-key filter, column filters and a limit, run as
// ReadRows), the Query editor (GoogleSQL for Bigtable, ExecuteQuery), and
// the results with a Row details pane and Export. Its calls are the
// provider's Studio (POST /api/studio/bigtable-instances).

function renderBigtableStudio(panel, route, segments, project) {
  const [instance] = segments;
  const call = (name, body = {}) => send(
    `/api/studio/bigtable-instances?project=${encodeURIComponent(project)}&call=${name}`, "POST", { instance, ...body });
  const key = `cb.bigtable.studio.${project}.${instance}`;
  // Google's Studio has query tabs (#1858): each a builder or an editor of
  // its own, opened by New tab or by a table's Query in the Explorer, kept
  // for the browser tab. state is the current tab; store holds them all.
  let store;
  try { store = JSON.parse(sessionStorage.getItem(key) || "null"); } catch { store = null; }
  if (!store || typeof store !== "object") store = {};
  if (!Array.isArray(store.tabs) || !store.tabs.length) {
    store = { tabs: [{ title: "Untitled query", mode: store.mode || "builder", sql: store.sql || "", table: store.table || "" }], current: 0 };
  }
  store.current = Math.min(Math.max(0, store.current | 0), store.tabs.length - 1);
  let state = store.tabs[store.current];
  const save = () => { try { sessionStorage.setItem(key, JSON.stringify(store)); } catch { /* per-tab only */ } };
  let catalog = { tables: [] };
  let last = null;
  const lastOf = new Map();
  const queryTabs = el("div", { class: "tab-strip spanner-editor-tabs", id: "bigtable-query-tabs", role: "tablist", "aria-label": "Query tabs" });

  const explorer = el("nav", { class: "spanner-explorer bigtable-explorer", id: "bigtable-explorer", "aria-label": "Explorer" },
    el("h3", { text: "Explorer" }), loadingState(3, { what: "explorer" }));
  const modes = el("div", { class: "tab-strip", role: "tablist", "aria-label": "Query mode" });
  const body = el("div", { class: "bigtable-studio-body" });
  const results = el("div", { class: "spanner-results", id: "bigtable-results", role: "region", "aria-label": "Query results" });
  const details = el("aside", { class: "bigtable-row-details", id: "bigtable-row-details", "aria-label": "Row details", hidden: true });

  // The builder's controls.
  const tableSelect = el("select", { id: "bigtable-builder-table", "aria-label": "Table" });
  const keyMode = el("select", { id: "bigtable-builder-keymode", "aria-label": "Row key filter" },
    ...[["all", "All rows"], ["prefix", "Row key prefix"], ["range", "Row key range"], ["keys", "Row keys"]].map(([v, t]) => el("option", { value: v, text: t })));
  const prefix = el("input", { id: "bigtable-builder-prefix", type: "text", "aria-label": "Row key prefix", placeholder: "Row key prefix" });
  const startKey = el("input", { id: "bigtable-builder-start", type: "text", "aria-label": "Start row key (inclusive)", placeholder: "Start (inclusive)" });
  const endKey = el("input", { id: "bigtable-builder-end", type: "text", "aria-label": "End row key (exclusive)", placeholder: "End (exclusive)" });
  const keys = el("input", { id: "bigtable-builder-keys", type: "text", "aria-label": "Row keys", placeholder: "key1, key2" });
  const family = el("select", { id: "bigtable-builder-family", "aria-label": "Column family" });
  const qualifier = el("input", { id: "bigtable-builder-qualifier", type: "text", "aria-label": "Column qualifier", placeholder: "Column qualifier" });
  const latest = el("input", { id: "bigtable-builder-latest", type: "checkbox" });
  const limit = el("input", { id: "bigtable-builder-limit", type: "number", min: "1", max: "200", value: "100", "aria-label": "Limit" });
  const runBuilder = el("button", { class: "primary", id: "bigtable-builder-run", type: "button", text: "Run" });
  const keyInputs = el("div", { class: "bigtable-key-inputs" });
  const drawKeyInputs = () => setChildren(keyInputs, ...({ all: [], prefix: [prefix], range: [startKey, endKey], keys: [keys] }[keyMode.value]));
  keyMode.addEventListener("change", drawKeyInputs);
  const drawFamilies = () => {
    const t = (catalog.tables || []).find((x) => x.name === tableSelect.value);
    setChildren(family, el("option", { value: "", text: "All column families" }),
      ...((t && t.families) || []).map((f) => el("option", { value: f.name, text: f.name })));
  };
  tableSelect.addEventListener("change", () => { state.table = tableSelect.value; save(); drawFamilies(); });

  // The editor.
  const editor = el("textarea", { class: "mono spanner-editor", id: "bigtable-editor", rows: "8", spellcheck: "false",
    "aria-label": "Query editor", placeholder: "SELECT _key, cf1 FROM `my-table` LIMIT 10" });
  editor.value = state.sql || "";
  editor.addEventListener("input", () => { state.sql = editor.value; save(); });
  const runEditor = el("button", { class: "primary", id: "bigtable-editor-run", type: "button", text: "Run" });
  const clear = el("button", { class: "secondary", id: "bigtable-editor-clear", type: "button", text: "Clear",
    onclick: () => { editor.value = ""; state.sql = ""; save(); editor.focus(); } });

  const showDetails = (row) => {
    details.hidden = false;
    setChildren(details, el("h3", { text: "Row details" }),
      el("p", { class: "mono", id: "bigtable-row-key", text: row.key }),
      el("table", {}, el("thead", {}, el("tr", {}, el("th", { text: "Column" }), el("th", { text: "Value" }), el("th", { text: "Timestamp" }))),
        el("tbody", {}, ...(row.cells || []).map((c) => el("tr", {}, el("td", { class: "mono", text: c.column }),
          el("td", { class: "mono", text: c.value }), el("td", { class: "mono", text: c.timestamp }))))),
      el("button", { class: "secondary", type: "button", text: "Close", onclick: () => { details.hidden = true; } }));
  };
  const exportAs = (kind) => {
    if (!last) return;
    const cols = last.columns || [];
    const objects = (last.rows || []).map((r) => Object.fromEntries(cols.map((c, i) => [c, r[i]])));
    const csv = [cols, ...(last.rows || [])].map((r) => r.map((v) => `"${String(v ?? "").replaceAll('"', '""')}"`).join(",")).join("\n");
    if (kind === "clipboard") {
      navigator.clipboard?.writeText(csv).then(() => announce("Copied to clipboard"), () => announce("Copy failed"));
      return;
    }
    const blob = new Blob([kind === "json" ? JSON.stringify(objects, null, 2) : csv], { type: kind === "json" ? "application/json" : "text/csv" });
    const a = el("a", { href: URL.createObjectURL(blob), download: `bigtable-results.${kind}` });
    document.body.append(a); a.click(); a.remove();
  };
  const drawResults = () => {
    if (!last) return setChildren(results, el("p", { class: "unavailable", text: "Run a query to see its results." }));
    if (last.error) return setChildren(results, el("p", { class: "form-error", role: "alert", text: last.error }));
    const cols = last.columns || [];
    const exportMenu = bqMenu("Export", [
      { label: "Export to CSV", run: () => exportAs("csv") },
      { label: "Export to JSON", run: () => exportAs("json") },
      { label: "Copy to clipboard", run: () => exportAs("clipboard") },
      { label: "Export to Google Sheets", disabled: true, title: "Not available locally: Google Sheets is not served.", run: () => {} },
    ]);
    bqMenuButton(exportMenu, "Export", "Export results");
    setChildren(results,
      el("div", { class: "action-bar" },
        el("p", { class: "spanner-message", id: "bigtable-message", text: `${last.message || ""} (${last.elapsed || "—"})` }),
        exportMenu),
      el("div", { class: "table-wrap" }, el("table", { id: "bigtable-results-table" },
        el("thead", {}, el("tr", {}, ...cols.map((c) => el("th", { text: c })))),
        el("tbody", {}, ...(last.rows || []).map((row, i) => el("tr", {
          class: last.details ? "is-clickable" : "", tabindex: last.details ? "0" : null,
          onclick: () => last.details && showDetails(last.details[i]),
          onkeydown: (e) => { if (e.key === "Enter" && last.details) showDetails(last.details[i]); },
        }, ...row.map((v) => el("td", { class: "mono", text: v }))))))));
  };
  const execute = async (button, name, payload) => {
    // The query's tab: its results stay with it if another is chosen.
    const tab = state;
    setBusy(button, true);
    details.hidden = true;
    setChildren(results, el("p", { class: "unavailable" }, spinner("Running"), " Running…"));
    let res;
    try {
      res = await call(name, payload);
      announce(`Query complete: ${(res.rows || []).length} rows`);
    } catch (err) {
      res = { error: err.message };
    } finally {
      setBusy(button, false);
    }
    lastOf.set(tab, res);
    if (tab !== state) return;
    last = res;
    drawResults();
  };
  runBuilder.addEventListener("click", () => execute(runBuilder, "read", {
    table: tableSelect.value, latest: latest.checked, limit: Number(limit.value) || 100,
    family: family.value, qualifier: qualifier.value.trim(),
    prefix: keyMode.value === "prefix" ? prefix.value : "",
    start: keyMode.value === "range" ? startKey.value : "", end: keyMode.value === "range" ? endKey.value : "",
    keys: keyMode.value === "keys" ? keys.value : "",
  }));
  runEditor.addEventListener("click", () => { if (editor.value.trim()) execute(runEditor, "sql", { sql: editor.value }); });

  const label = (text, control) => el("label", { class: "form-row" }, el("span", { class: "form-label", text }), control);
  const drawMode = () => {
    setChildren(modes, ...[["builder", "Query builder"], ["editor", "Query editor"]].map(([id, text]) =>
      el("button", { class: "tab" + (state.mode === id ? " is-selected" : ""), role: "tab", type: "button", id: `bigtable-mode-${id}`,
        "aria-selected": state.mode === id ? "true" : "false", text,
        onclick: () => { state.mode = id; save(); drawMode(); } })));
    if (state.mode === "editor") {
      setChildren(body, el("p", { class: "form-help", text: "GoogleSQL for Bigtable: SELECT over a table, a logical view or a materialized view, read with ExecuteQuery." }),
        editor, el("div", { class: "action-bar spanner-editor-actions" }, runEditor, clear));
    } else {
      setChildren(body, el("div", { class: "bigtable-builder", id: "bigtable-builder" },
        label("Table", tableSelect),
        label("Row key filter", keyMode), keyInputs,
        label("Column family", family), label("Column qualifier", qualifier),
        el("label", { class: "check-line" }, latest, el("span", { text: "Latest cell versions only" })),
        label("Limit", limit),
        el("div", { class: "action-bar spanner-editor-actions" }, runBuilder)));
    }
  };
  // A tab's builder controls are kept with it, and put back when it is
  // chosen again.
  const keepBuilder = () => {
    state.builder = { keyMode: keyMode.value, prefix: prefix.value, start: startKey.value, end: endKey.value, keys: keys.value,
      family: family.value, qualifier: qualifier.value, latest: latest.checked, limit: limit.value };
  };
  const loadTab = () => {
    const b = state.builder || {};
    if ((catalog.tables || []).some((t) => t.name === state.table)) tableSelect.value = state.table;
    drawFamilies();
    keyMode.value = b.keyMode || "all"; prefix.value = b.prefix || ""; startKey.value = b.start || ""; endKey.value = b.end || "";
    keys.value = b.keys || ""; family.value = b.family || ""; qualifier.value = b.qualifier || "";
    latest.checked = !!b.latest; limit.value = b.limit || "100";
    drawKeyInputs();
    editor.value = state.sql || "";
    last = lastOf.get(state) || null;
    details.hidden = true;
    drawMode();
    drawResults();
  };
  const selectTab = (i) => {
    keepBuilder();
    lastOf.set(state, last);
    store.current = i;
    state = store.tabs[i];
    save();
    drawQueryTabs();
    loadTab();
  };
  const openTab = (tab) => {
    keepBuilder();
    lastOf.set(state, last);
    store.tabs.push(tab);
    selectTab(store.tabs.length - 1);
  };
  const drawQueryTabs = () => {
    setChildren(queryTabs, ...store.tabs.map((t, i) => el("span", { class: "spanner-editor-tab" },
      el("button", { class: "tab" + (i === store.current ? " is-selected" : ""), role: "tab", type: "button",
        "aria-selected": i === store.current ? "true" : "false", text: t.title, onclick: () => selectTab(i) }),
      store.tabs.length > 1 ? el("button", { class: "icon-button", type: "button", "aria-label": `Close ${t.title}`, text: "×",
        onclick: () => {
          lastOf.delete(t);
          store.tabs.splice(i, 1);
          const next = Math.min(store.current >= i && store.current > 0 ? store.current - 1 : store.current, store.tabs.length - 1);
          store.current = next;
          state = store.tabs[next];
          save();
          drawQueryTabs();
          loadTab();
        } }) : null)),
    el("button", { class: "secondary", id: "bigtable-new-tab", type: "button", text: "New tab",
      onclick: () => openTab({ title: "Untitled query", mode: "builder", sql: "", table: tableSelect.value }) }));
  };
  const openBuilder = (table) => openTab({ title: table, mode: "builder", sql: "", table });
  const openEditor = (sql) => { openTab({ title: "Untitled query", mode: "editor", sql, table: state.table }); editor.focus(); };

  const group = (title, items, row) => el("details", { class: "spanner-explorer-group", open: true },
    el("summary", { text: `${title} (${items.length})` }),
    items.length ? el("ul", {}, ...items.map(row)) : el("p", { class: "unavailable", text: `No ${title.toLowerCase()}` }));
  const quote = (n) => `\`${n.replaceAll("`", "\\`")}\``;
  const loadExplorer = async () => {
    try {
      catalog = await call("explorer");
      const tables = catalog.tables || [];
      setChildren(tableSelect, ...tables.map((t) => el("option", { value: t.name, text: t.name })));
      loadTab();
      const tableRow = (t) => el("li", { class: "spanner-explorer-table" },
        el("details", {},
          el("summary", {}, el("span", { class: "mono", text: t.name }),
            bqMenu(`View actions for ${t.name}`, [
              { label: "Query", run: () => openBuilder(t.name) },
              { label: "Open in query editor", run: () => openEditor(`SELECT * FROM ${quote(t.name)} LIMIT 100`) },
              { label: "Open table page", run: () => navigate(`/bigtable/${encodeURIComponent(t.name)}?project=${encodeURIComponent(project)}&instance=${encodeURIComponent(instance)}`) },
            ])),
          el("ul", { class: "spanner-columns" },
            ...t.families.map((f) => el("li", {}, el("span", { class: "mono", text: f.name }), el("span", { class: "unavailable", text: ` ${f.gc}` }))),
            ...t.authorizedViews.map((v) => el("li", {}, el("span", { class: "mono", text: v }), el("span", { class: "unavailable", text: " authorized view" }))))));
      const viewRow = (v) => el("li", {}, el("button", { class: "link-button mono", type: "button", text: v,
        onclick: () => openEditor(`SELECT * FROM ${quote(v)} LIMIT 100`) }));
      setChildren(explorer, el("h3", { text: "Explorer" }),
        el("p", { class: "spanner-explorer-db mono", text: instance }),
        group("Tables", tables, tableRow),
        group("Logical views", catalog.logicalViews || [], viewRow),
        group("Materialized views", catalog.materializedViews || [], viewRow),
        ...Object.entries(catalog.errors || {}).map(([k, v]) => el("p", { class: "unavailable", text: `${k}: ${v}` })));
    } catch (err) {
      setChildren(explorer, el("h3", { text: "Explorer" }), el("p", { class: "form-error", role: "alert", text: err.message }));
    }
  };

  drawQueryTabs();
  loadTab();
  setChildren(panel, el("div", { class: "spanner-studio bigtable-studio", id: "bigtable-studio" },
    explorer,
    el("div", { class: "spanner-workspace" }, queryTabs, modes, body, results, details)));
  loadExplorer();
}

// --- Spanner Studio (#1859) -------------------------------------------------
//
// A database's Spanner Studio, as Google's console draws it
// (https://docs.cloud.google.com/spanner/docs/create-query-database-console):
// the Explorer pane (Tables, each with its columns and View actions: Preview
// data and Insert data; Views, Indexes, Change streams, Sequences), editor
// tabs ("Untitled query", New tab), Run, Explain and Clear, and the
// Results and Explanation tabs under the editor. Its calls are the
// provider's Studio (POST /api/studio/spanner). A statement that changes
// data or schema runs after a confirmation that names the database.

const STUDIO_SECTIONS = { spanner: renderSpannerStudio, "bigtable-instances": renderBigtableStudio };

function renderSpannerStudio(panel, route, segments, project) {
  const [instance, database] = segments;
  const call = (name, body = {}) => send(
    `/api/studio/spanner?project=${encodeURIComponent(project)}&call=${name}`, "POST",
    { instance, database, ...body });
  const key = `cb.spanner.studio.${project}.${instance}.${database}`;
  let tabs;
  try { tabs = JSON.parse(sessionStorage.getItem(key) || "null"); } catch { tabs = null; }
  if (!Array.isArray(tabs) || !tabs.length) tabs = [{ title: "Untitled query", sql: "" }];
  let current = 0;
  const save = () => { try { sessionStorage.setItem(key, JSON.stringify(tabs)); } catch { /* per-tab only */ } };

  const explorer = el("nav", { class: "spanner-explorer", id: "spanner-explorer", "aria-label": "Explorer" },
    el("h3", { text: "Explorer" }), loadingState(3, { what: "explorer" }));
  const strip = el("div", { class: "tab-strip spanner-editor-tabs", role: "tablist", "aria-label": "Editor tabs" });
  const editor = el("textarea", { class: "mono spanner-editor", id: "spanner-editor", rows: "10", spellcheck: "false",
    "aria-label": "Query editor", placeholder: "SELECT * FROM ..." });
  const run = el("button", { class: "primary", id: "spanner-run", type: "button", text: "Run" });
  const explain = el("button", { class: "secondary", id: "spanner-explain", type: "button", text: "Explain" });
  const clear = el("button", { class: "secondary", id: "spanner-clear", type: "button", text: "Clear" });
  const resultTabs = el("div", { class: "tab-strip spanner-result-tabs", role: "tablist", "aria-label": "Query results" });
  const results = el("div", { class: "spanner-results", id: "spanner-results", role: "tabpanel" });
  const last = { results: null, explanation: null };
  let resultTab = "results";

  const drawResultTabs = () => {
    setChildren(resultTabs, ...[["results", "Results"], ["explanation", "Explanation"]].map(([id, label]) =>
      el("button", { class: "tab" + (resultTab === id ? " is-selected" : ""), role: "tab", id: `spanner-tab-${id}`,
        "aria-selected": resultTab === id ? "true" : "false", text: label,
        onclick: () => { resultTab = id; drawResultTabs(); drawResults(); } })));
  };
  const drawResults = () => {
    const r = last[resultTab];
    if (!r) {
      return setChildren(results, el("p", { class: "unavailable",
        text: resultTab === "results" ? "Run a statement to see its results." : "Explain a query to see its query plan." }));
    }
    if (r.error) return setChildren(results, el("p", { class: "form-error", role: "alert", text: r.error }));
    if (resultTab === "explanation") {
      // Google's Explanation tab: the Query summary (PROFILE mode's
      // statistics, after Run) above the plan, which the emulator lacks.
      const st = r.stats;
      const summary = st ? el("section", { class: "spanner-query-summary", id: "spanner-query-summary", "aria-label": "Query summary" },
        el("h3", { text: "Query summary" }),
        el("dl", { class: "resource-summary" },
          ...[["Rows returned", st.rowsReturned], ["Elapsed time", st.elapsedTime],
            ["CPU time", st.cpuTime || "Not reported by the Spanner emulator"], ["Rows scanned", st.rowsScanned || "Not reported by the Spanner emulator"]]
            .flatMap(([k, v]) => [el("dt", { text: k }), el("dd", { class: "mono", id: `spanner-stat-${k.toLowerCase().replaceAll(" ", "-")}`, text: v || "—" })]))) : null;
      const plan = (r.plan || []).length
        ? el("ol", { class: "spanner-plan" }, ...r.plan.map((n) =>
          el("li", { text: `${n.index}: ${n.name} (${n.kind})` + ((n.children || []).length ? ` → ${n.children.join(", ")}` : "") })))
        : el("p", { class: "unavailable", id: "spanner-plan-note", text: r.planNote || "No plan." });
      return setChildren(results, summary, plan);
    }
    if (r.kind !== "query") {
      return setChildren(results, el("p", { class: "spanner-message", id: "spanner-message", text: `${r.message || "Done."} ${r.elapsed ? `(${r.elapsed})` : ""}` }));
    }
    const cols = r.columns || [];
    setChildren(results,
      el("p", { class: "spanner-message", id: "spanner-message", text: `${(r.rows || []).length} rows in ${r.elapsed || "—"}. ${r.message || ""}` }),
      el("div", { class: "table-wrap" }, el("table", { id: "spanner-results-table" },
        el("thead", {}, el("tr", {}, ...cols.map((c) => el("th", { text: c })))),
        el("tbody", {}, ...(r.rows || []).map((row) => el("tr", {}, ...row.map((v) => el("td", { class: "mono", text: v }))))))));
  };

  const drawTabs = () => {
    const buttons = tabs.map((t, i) => el("span", { class: "spanner-editor-tab" },
      el("button", { class: "tab" + (i === current ? " is-selected" : ""), role: "tab", type: "button",
        "aria-selected": i === current ? "true" : "false", text: t.title,
        onclick: () => { current = i; editor.value = tabs[i].sql; drawTabs(); } }),
      tabs.length > 1 ? el("button", { class: "icon-button", type: "button", "aria-label": `Close ${t.title}`, text: "×",
        onclick: () => { tabs.splice(i, 1); current = Math.min(current, tabs.length - 1); editor.value = tabs[current].sql; save(); drawTabs(); } }) : null));
    setChildren(strip, ...buttons, el("button", { class: "secondary", id: "spanner-new-tab", type: "button", text: "New tab",
      onclick: () => openTab("") }));
  };
  const openTab = (sql, title = "Untitled query") => {
    tabs.push({ title, sql });
    current = tabs.length - 1;
    editor.value = sql;
    save();
    drawTabs();
    editor.focus();
  };
  editor.addEventListener("input", () => { tabs[current].sql = editor.value; save(); });

  const execute = async (write = false) => {
    const sql = editor.value.trim();
    if (!sql) return;
    setBusy(run, true);
    resultTab = "results"; drawResultTabs();
    setChildren(results, el("p", { class: "unavailable" }, spinner("Running"), " Running…"));
    try {
      const res = await call("run", { sql, write });
      if (res.confirm) {
        setBusy(run, false);
        setChildren(results);
        if (await bqConfirmWrite(sql, res.confirm)) return execute(true);
        last.results = null; drawResults();
        return;
      }
      last.results = res;
      // Run is PROFILE mode: its statistics are the Explanation tab's.
      if (res.kind === "query") last.explanation = res;
      if (res.kind !== "query") loadExplorer();
      announce(res.kind === "query" ? `Query complete: ${(res.rows || []).length} rows` : (res.message || "Done"));
    } catch (err) {
      last.results = { error: err.message };
    } finally {
      setBusy(run, false);
    }
    drawResults();
  };
  run.addEventListener("click", () => execute());
  explain.addEventListener("click", async () => {
    const sql = editor.value.trim();
    if (!sql) return;
    setBusy(explain, true);
    try {
      last.explanation = await call("explain", { sql });
    } catch (err) {
      last.explanation = { error: err.message };
    } finally {
      setBusy(explain, false);
    }
    resultTab = "explanation"; drawResultTabs(); drawResults();
  });
  clear.addEventListener("click", () => { editor.value = ""; tabs[current].sql = ""; save(); editor.focus(); });

  const quote = (dialect, n) => (dialect === "POSTGRESQL" ? `"${n.replaceAll('"', '""')}"` : `\`${n.replaceAll("`", "\\`")}\``);
  const group = (title, items, row) => el("details", { class: "spanner-explorer-group", open: true },
    el("summary", { text: `${title} (${items.length})` }),
    items.length ? el("ul", {}, ...items.map(row)) : el("p", { class: "unavailable", text: `No ${title.toLowerCase()}` }));
  const loadExplorer = async () => {
    try {
      const cat = await call("explorer");
      const tableRow = (t) => el("li", { class: "spanner-explorer-table" },
        el("details", {},
          el("summary", {}, el("span", { class: "mono", text: t.name }),
            bqMenu(`View actions for ${t.name}`, [
              { label: "Preview data", run: () => {
                openTab(`SELECT * FROM ${quote(cat.dialect, t.name)} LIMIT 100;`, t.name);
                execute();
              } },
              { label: "Insert data", run: () => {
                const cols = t.columns.map((c) => quote(cat.dialect, c[0]));
                openTab(`INSERT INTO ${quote(cat.dialect, t.name)} (${cols.join(", ")})\nVALUES (${t.columns.map((c) => `/* ${c[1]} */`).join(", ")});`, `Insert into ${t.name}`);
              } },
              { label: "Open table page", run: () => navigate(detailHref(route, [instance, database, t.name])) },
            ])),
          el("ul", { class: "spanner-columns" }, ...t.columns.map((c) =>
            el("li", {}, el("span", { class: "mono", text: c[0] }), el("span", { class: "unavailable", text: ` ${c[1]}` }))))));
      setChildren(explorer, el("h3", { text: "Explorer" }),
        el("p", { class: "spanner-explorer-db mono", text: `${database} (${cat.dialect === "POSTGRESQL" ? "PostgreSQL" : "GoogleSQL"})` }),
        group("Tables", cat.tables || [], tableRow),
        group("Views", cat.views || [], (v) => el("li", { class: "mono", text: v[0] })),
        group("Indexes", cat.indexes || [], (i) => el("li", { class: "mono", text: `${i[1]} (${i[0]})` })),
        group("Change streams", cat.changeStreams || [], (c) => el("li", { class: "mono", text: c[0] })),
        group("Sequences", cat.sequences || [], (q) => el("li", { class: "mono", text: q[0] })),
        ...Object.entries(cat.errors || {}).map(([k, v]) => el("p", { class: "unavailable", text: `${k}: ${v}` })));
    } catch (err) {
      setChildren(explorer, el("h3", { text: "Explorer" }), el("p", { class: "form-error", role: "alert", text: err.message }));
    }
  };

  editor.value = tabs[current].sql;
  drawTabs();
  drawResultTabs();
  drawResults();
  setChildren(panel, el("div", { class: "spanner-studio" },
    explorer,
    el("div", { class: "spanner-workspace" },
      strip,
      editor,
      el("div", { class: "action-bar spanner-editor-actions" }, run, explain, clear),
      resultTabs,
      results)));
  loadExplorer();
}
