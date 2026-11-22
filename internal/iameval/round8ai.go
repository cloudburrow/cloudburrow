package iameval

import (
	"context"
	"regexp"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

// The eighth round's AI tables (#1879): Vertex AI's served methods,
// Document AI and Speech-to-Text v2, over gRPC and JSON, and Translation
// v3's two JSON methods, each with the permission and field Google's
// reference names (read 2026-11-20). Not checked, with the reason:
//
//   - Cloud Vision's images:annotate and Natural Language's documents.*
//     methods: Google documents no permission of their own (their REST
//     pages have no IAM section; Natural Language's setup page asks only
//     for Service Usage Consumer on the quota project), and their paths
//     name no project.
//   - Speech-to-Text v1's Recognize, LongRunningRecognize and
//     StreamingRecognize: no page names a permission, and the requests
//     name no resource.
//   - ModelGardenService.GetPublisherModel (no page names one; publisher
//     models are Google's, readable by every caller) and the endpoint's
//     shared Operations service (no page names a permission for get or
//     wait; the index has only aiplatform.operations.list).
//
// Google's wording of a denial, the newer APIs', is UNVERIFIED for each
// (re-read 2026-11-20: the troubleshooting pages quote only "403:
// Permission denied").

func init() {
	for _, v := range []string{"v1", "v1beta1"} {
		for _, s := range []string{"PredictionService", "LlmUtilityService", "ModelGardenService", "EndpointService", "ModelService", "GenAiCacheService"} {
			Services["google.cloud.aiplatform."+v+"."+s] = VertexAI
		}
	}
	// Colab Enterprise (#2105), v1 only as served.
	for _, s := range []string{"NotebookService", "ScheduleService", "PipelineService"} {
		Services["google.cloud.aiplatform.v1."+s] = VertexAI
	}
	Services["google.cloud.documentai.v1.DocumentProcessorService"] = DocumentAI
	Services["google.cloud.documentai.v1beta3.DocumentService"] = DocumentAI
	Services["google.cloud.speech.v2.Speech"] = Speech
	Services["google.cloud.speech.v1.Adaptation"] = Speech
}

const aiPredict = "aiplatform.endpoints.predict"

// VertexAI is Vertex AI's table: each method's REST reference ("Requires
// the following IAM permission on the X resource",
// https://docs.cloud.google.com/gemini-enterprise-agent-platform/reference/rest/v1/...,
// read 2026-11-20). Every prediction method, generateContent,
// embedContent, countTokens and computeTokens is aiplatform.endpoints.predict
// on its endpoint or model (a publisher model's name for Google's
// models); Explain aiplatform.endpoints.explain. The bidirectional
// streaming methods (StreamingPredict, StreamingRawPredict,
// StreamDirectPredict, StreamDirectRawPredict), which have no REST page,
// are checked as the predict they stream (UNVERIFIED, re-read 2026-11-20:
// no page names one). ModelGardenService
// Deploy needs aiplatform.endpoints.create, .deploy and
// aiplatform.models.upload on its destination. Of what is served, only
// Colab Enterprise's runtime templates keep resource-level policies
// (#2105).
var VertexAI = &Service{
	Name:              "aiplatform.googleapis.com",
	PolicyCollections: []string{"notebookRuntimeTemplates"},
	Types:             map[string]string{"notebookRuntimeTemplates": "aiplatform.googleapis.com/NotebookRuntimeTemplate"},
	Methods: func() map[string]Method {
		m := map[string]Method{}
		for _, v := range []string{"v1", "v1beta1"} {
			p := "/google.cloud.aiplatform." + v + "."
			for k, r := range rows(map[string]map[string][2]string{
				p + "PredictionService/": {
					"Predict": {aiPredict, "endpoint"}, "RawPredict": {aiPredict, "endpoint"}, "StreamRawPredict": {aiPredict, "endpoint"},
					"DirectPredict": {aiPredict, "endpoint"}, "DirectRawPredict": {aiPredict, "endpoint"},
					"ServerStreamingPredict": {aiPredict, "endpoint"}, "StreamingPredict": {aiPredict, "endpoint"},
					"StreamingRawPredict": {aiPredict, "endpoint"}, "StreamDirectPredict": {aiPredict, "endpoint"},
					"StreamDirectRawPredict": {aiPredict, "endpoint"}, "Explain": {"aiplatform.endpoints.explain", "endpoint"},
					"GenerateContent": {aiPredict, "model"}, "StreamGenerateContent": {aiPredict, "model"},
					"EmbedContent": {aiPredict, "model"},
				},
				p + "LlmUtilityService/":  {"CountTokens": {aiPredict, "endpoint"}, "ComputeTokens": {aiPredict, "endpoint"}},
				p + "ModelGardenService/": {"Deploy": {"aiplatform.endpoints.create", "destination"}},
				// Context caching (#2107): aiplatform.cachedContents.* on the
				// cache, or on its parent location to create and list.
				p + "GenAiCacheService/": {
					"CreateCachedContent": {"aiplatform.cachedContents.create", "parent"},
					"GetCachedContent":    {"aiplatform.cachedContents.get", "name"},
					"ListCachedContents":  {"aiplatform.cachedContents.list", "parent"},
					"UpdateCachedContent": {"aiplatform.cachedContents.update", "cached_content.name"},
					"DeleteCachedContent": {"aiplatform.cachedContents.delete", "name"},
				},
				p + "EndpointService/": {
					"CreateEndpoint": {"aiplatform.endpoints.create", "parent"}, "GetEndpoint": {"aiplatform.endpoints.get", "name"},
					"ListEndpoints": {"aiplatform.endpoints.list", "parent"}, "UpdateEndpoint": {"aiplatform.endpoints.update", "endpoint.name"},
					"UpdateEndpointLongRunning": {"aiplatform.endpoints.update", "endpoint.name"},
					"DeleteEndpoint":            {"aiplatform.endpoints.delete", "name"}, "DeployModel": {"aiplatform.endpoints.deploy", "endpoint"},
					"UndeployModel": {"aiplatform.endpoints.undeploy", "endpoint"}, "MutateDeployedModel": {"aiplatform.endpoints.deploy", "endpoint"},
				},
				p + "ModelService/": {
					"UploadModel": {"aiplatform.models.upload", "parent"}, "GetModel": {"aiplatform.models.get", "name"},
					"ListModels": {"aiplatform.models.list", "parent"}, "UpdateModel": {"aiplatform.models.update", "model.name"},
					"DeleteModel": {"aiplatform.models.delete", "name"}, "ListModelVersions": {"aiplatform.models.get", "name"},
					"DeleteModelVersion": {"aiplatform.models.delete", "name"}, "MergeVersionAliases": {"aiplatform.models.update", "name"},
					"CopyModel": {"aiplatform.models.upload", "parent"}, "ExportModel": {"aiplatform.models.export", "name"},
					"ImportModelEvaluation": {"aiplatform.modelEvaluations.import", "parent"},
					"GetModelEvaluation":    {"aiplatform.modelEvaluations.get", "name"},
					"ListModelEvaluations":  {"aiplatform.modelEvaluations.list", "parent"},
				},
			}) {
				m[k] = r
			}
			// v1beta1's ChatCompletions and CountTokens are PredictionService's;
			// its LlmUtilityService has ComputeTokens alone.
			if v == "v1beta1" {
				delete(m, p+"LlmUtilityService/CountTokens")
				m[p+"PredictionService/ChatCompletions"] = Method{Permission: aiPredict, Field: "endpoint"}
				m[p+"PredictionService/CountTokens"] = Method{Permission: aiPredict, Field: "endpoint"}
			}
			if v == "v1" {
				for k, r := range colabMethods() {
					m[k] = r
				}
			}
			deploy := m[p+"ModelGardenService/Deploy"]
			deploy.Also = []Also{{Field: "destination", Permission: "aiplatform.endpoints.deploy"}, {Field: "destination", Permission: "aiplatform.models.upload"}}
			m[p+"ModelGardenService/Deploy"] = deploy
		}
		return m
	}(),
	Deny: newerDenyOf,
}

// colabMethods are Colab Enterprise's (#2105): each method's REST
// reference ("Requires the following IAM permission on the X resource",
// https://docs.cloud.google.com/gemini-enterprise-agent-platform/reference/rest/v1/projects.locations.notebookRuntimes/assign
// and its siblings under notebookRuntimeTemplates, notebookExecutionJobs
// and schedules, read 2026-11-22): assign is aiplatform.notebookRuntimes.assign
// on the parent; a schedule's pause, resume and patch are
// aiplatform.schedules.update; the templates' IAM methods
// aiplatform.notebookRuntimeTemplates.getIamPolicy and .setIamPolicy.
// StopNotebookRuntime's page names no permission (re-read 2026-11-22:
// .../projects.locations.notebookRuntimes/stop has no IAM Permissions
// section); it is checked as aiplatform.notebookRuntimes.update on the
// runtime (#2173), the one permission of IAM's notebookRuntimes list
// (https://docs.cloud.google.com/iam/docs/roles-permissions/aiplatform:
// assign, delete, get, list, start, update, upgrade) no other method
// names, held by the Colab Enterprise Admin, Notebook Runtime Admin and
// Vertex AI User roles but not by Colab Enterprise User (UNVERIFIED).
// Making a runtime or an execution from a template also needs
// aiplatform.notebookRuntimeTemplates.apply on it, the permission the
// Notebook Runtime User role (roles/aiplatform.notebookRuntimeUser)
// "to create runtime resources using a runtime template" holds; the
// templates keep their own policies (UNVERIFIED, re-read 2026-11-22: the
// assign and create pages name only the parent's permission).
func colabMethods() map[string]Method {
	const p = "/google.cloud.aiplatform.v1."
	m := rows(map[string]map[string][2]string{
		p + "NotebookService/": {
			"CreateNotebookRuntimeTemplate": {"aiplatform.notebookRuntimeTemplates.create", "parent"},
			"GetNotebookRuntimeTemplate":    {"aiplatform.notebookRuntimeTemplates.get", "name"},
			"ListNotebookRuntimeTemplates":  {"aiplatform.notebookRuntimeTemplates.list", "parent"},
			"UpdateNotebookRuntimeTemplate": {"aiplatform.notebookRuntimeTemplates.update", "notebook_runtime_template.name"},
			"DeleteNotebookRuntimeTemplate": {"aiplatform.notebookRuntimeTemplates.delete", "name"},
			"AssignNotebookRuntime":         {"aiplatform.notebookRuntimes.assign", "parent"},
			"GetNotebookRuntime":            {"aiplatform.notebookRuntimes.get", "name"},
			"ListNotebookRuntimes":          {"aiplatform.notebookRuntimes.list", "parent"},
			"DeleteNotebookRuntime":         {"aiplatform.notebookRuntimes.delete", "name"},
			"StartNotebookRuntime":          {"aiplatform.notebookRuntimes.start", "name"},
			"StopNotebookRuntime":           {"aiplatform.notebookRuntimes.update", "name"},
			"UpgradeNotebookRuntime":        {"aiplatform.notebookRuntimes.upgrade", "name"},
			"CreateNotebookExecutionJob":    {"aiplatform.notebookExecutionJobs.create", "parent"},
			"GetNotebookExecutionJob":       {"aiplatform.notebookExecutionJobs.get", "name"},
			"ListNotebookExecutionJobs":     {"aiplatform.notebookExecutionJobs.list", "parent"},
			"DeleteNotebookExecutionJob":    {"aiplatform.notebookExecutionJobs.delete", "name"},
		},
		p + "ScheduleService/": {
			"CreateSchedule": {"aiplatform.schedules.create", "parent"}, "GetSchedule": {"aiplatform.schedules.get", "name"},
			"ListSchedules": {"aiplatform.schedules.list", "parent"}, "DeleteSchedule": {"aiplatform.schedules.delete", "name"},
			"PauseSchedule": {"aiplatform.schedules.update", "name"}, "ResumeSchedule": {"aiplatform.schedules.update", "name"},
			"UpdateSchedule": {"aiplatform.schedules.update", "schedule.name"},
		},
		// Vertex AI Pipelines (#2173): each pipelineJobs method's page,
		// aiplatform.pipelineJobs.create on the parent, .get, .delete and
		// .cancel on the job, .list on the parent; the batches the
		// permission of each job on the parent (UNVERIFIED, re-read
		// 2026-11-22: the batchDelete and batchCancel pages name
		// aiplatform.pipelineJobs.delete and .cancel on the parent).
		p + "PipelineService/": {
			"CreatePipelineJob": {"aiplatform.pipelineJobs.create", "parent"}, "GetPipelineJob": {"aiplatform.pipelineJobs.get", "name"},
			"ListPipelineJobs": {"aiplatform.pipelineJobs.list", "parent"}, "DeletePipelineJob": {"aiplatform.pipelineJobs.delete", "name"},
			"CancelPipelineJob": {"aiplatform.pipelineJobs.cancel", "name"}, "BatchDeletePipelineJobs": {"aiplatform.pipelineJobs.delete", "parent"},
			"BatchCancelPipelineJobs": {"aiplatform.pipelineJobs.cancel", "parent"},
		},
		"/google.iam.v1.IAMPolicy/": {
			"GetIamPolicy": {"aiplatform.notebookRuntimeTemplates.getIamPolicy", "resource"},
			"SetIamPolicy": {"aiplatform.notebookRuntimeTemplates.setIamPolicy", "resource"},
		},
	})
	const apply = "aiplatform.notebookRuntimeTemplates.apply"
	a := m[p+"NotebookService/AssignNotebookRuntime"]
	a.Also = []Also{{Field: "notebook_runtime_template", Permission: apply}}
	m[p+"NotebookService/AssignNotebookRuntime"] = a
	c := m[p+"NotebookService/CreateNotebookExecutionJob"]
	c.Also = []Also{{Field: "notebook_execution_job.notebook_runtime_template_resource_name", Permission: apply}}
	// The run's service account needs actAs (#2173, runas.go): an
	// execution's, a scheduled request's and a pipeline job's.
	c.ActAs = []string{"notebook_execution_job.service_account"}
	m[p+"NotebookService/CreateNotebookExecutionJob"] = c
	for _, k := range []string{"CreateSchedule", "UpdateSchedule"} {
		r := m[p+"ScheduleService/"+k]
		r.ActAs = []string{"schedule.create_notebook_execution_job_request.notebook_execution_job.service_account",
			"schedule.create_pipeline_job_request.pipeline_job.service_account"}
		m[p+"ScheduleService/"+k] = r
	}
	pj := m[p+"PipelineService/CreatePipelineJob"]
	pj.ActAs = []string{"pipeline_job.service_account"}
	m[p+"PipelineService/CreatePipelineJob"] = pj
	return m
}

// docAIByType is a Document AI permission on a processor, or on a
// processor version: "one of documentai.processors.P /
// documentai.processorVersions.P, depending on the resource type".
func docAIByType(verb string) func(string) string {
	return func(resource string) string {
		if strings.Contains(resource, "/processorVersions/") {
			return "documentai.processorVersions." + verb
		}
		return "documentai.processors." + verb
	}
}

// DocumentAI is Document AI's table: its RPC reference
// (https://docs.cloud.google.com/document-ai/docs/reference/rpc/google.cloud.documentai.v1,
// read 2026-11-20), and v1beta3's DocumentService, whose permissions the
// IAM permissions page does not list but the roles page does
// (https://docs.cloud.google.com/iam/docs/roles-permissions/clouddocumentai,
// read 2026-11-21: documentai.datasets.* and documentai.datasetSchemas.*,
// in Viewer the get and list ones, in Editor and Admin all). Document AI
// keeps no resource-level policies.
var DocumentAI = &Service{
	Name: "documentai.googleapis.com",
	Methods: func() map[string]Method {
		m := rows(map[string]map[string][2]string{
			"/google.cloud.documentai.v1.DocumentProcessorService/": {
				"FetchProcessorTypes": {"documentai.processorTypes.list", "parent"}, "ListProcessorTypes": {"documentai.processorTypes.list", "parent"},
				"GetProcessorType": {"documentai.processorTypes.get", "name"}, "ListProcessors": {"documentai.processors.list", "parent"},
				"GetProcessor": {"documentai.processors.get", "name"}, "CreateProcessor": {"documentai.processors.create", "parent"},
				"DeleteProcessor": {"documentai.processors.delete", "name"}, "EnableProcessor": {"documentai.processors.update", "name"},
				"DisableProcessor": {"documentai.processors.update", "name"}, "SetDefaultProcessorVersion": {"documentai.processors.update", "processor"},
				"GetProcessorVersion": {"documentai.processorVersions.get", "name"}, "ListProcessorVersions": {"documentai.processorVersions.list", "parent"},
				"DeleteProcessorVersion": {"documentai.processorVersions.delete", "name"}, "DeployProcessorVersion": {"documentai.processorVersions.update", "name"},
				"UndeployProcessorVersion": {"documentai.processorVersions.update", "name"}, "TrainProcessorVersion": {"documentai.processorVersions.create", "parent"},
				"EvaluateProcessorVersion": {"documentai.evaluations.create", "processor_version"}, "GetEvaluation": {"documentai.evaluations.get", "name"},
				"ListEvaluations": {"documentai.evaluations.list", "parent"}, "ReviewDocument": {"documentai.humanReviewConfigs.review", "human_review_config"},
				"ProcessDocument": {"", "name"}, "BatchProcessDocuments": {"", "name"},
			},
			// The datasets of custom processors (#2124): each method's v1beta3
			// REST reference, "Requires the following IAM permission on the
			// dataset resource" (.../reference/rest/v1beta3/projects.locations.processors.dataset/importDocuments
			// and its siblings, and .../projects.locations.processors/updateDataset,
			// read 2026-11-21).
			"/google.cloud.documentai.v1beta3.DocumentService/": {
				"UpdateDataset": {"documentai.datasets.update", "dataset.name"}, "ImportDocuments": {"documentai.datasets.createDocuments", "dataset"},
				"GetDocument": {"documentai.datasets.getDocuments", "dataset"}, "ListDocuments": {"documentai.datasets.listDocuments", "dataset"},
				"BatchDeleteDocuments": {"documentai.datasets.deleteDocuments", "dataset"},
				"GetDatasetSchema":     {"documentai.datasetSchemas.get", "name"}, "UpdateDatasetSchema": {"documentai.datasetSchemas.update", "dataset_schema.name"},
			},
		})
		const p = "/google.cloud.documentai.v1.DocumentProcessorService/"
		m[p+"ProcessDocument"] = Method{Field: "name", PermissionFor: docAIByType("processOnline")}
		m[p+"BatchProcessDocuments"] = Method{Field: "name", PermissionFor: docAIByType("processBatch")}
		return m
	}(),
	Deny: newerDenyOf,
}

// Speech is Speech-to-Text v2's table, with v1's Adaptation (#2106,
// https://docs.cloud.google.com/speech-to-text/docs/reference/rpc/google.cloud.speech.v1,
// re-read 2026-11-21): its RPC reference
// (https://docs.cloud.google.com/speech-to-text/docs/reference/rpc/google.cloud.speech.v2,
// and "Speech-to-Text audit logging", read 2026-11-20).
var Speech = &Service{
	Name: "speech.googleapis.com",
	Methods: func() map[string]Method {
		r := map[string][2]string{
			"Recognize": {"speech.recognizers.recognize", "recognizer"}, "StreamingRecognize": {"speech.recognizers.recognize", "recognizer"},
			"BatchRecognize": {"speech.recognizers.recognize", "recognizer"},
			"GetConfig":      {"speech.config.get", "name"}, "UpdateConfig": {"speech.config.update", "config.name"},
		}
		for kind, plural := range map[string][2]string{"Recognizer": {"recognizers", "recognizer"}, "CustomClass": {"customClasses", "custom_class"},
			"PhraseSet": {"phraseSets", "phrase_set"}} {
			perm := "speech." + plural[0] + "."
			r["Create"+kind] = [2]string{perm + "create", "parent"}
			r["Get"+kind] = [2]string{perm + "get", "name"}
			r["Update"+kind] = [2]string{perm + "update", plural[1] + ".name"}
			r["Delete"+kind] = [2]string{perm + "delete", "name"}
			r["Undelete"+kind] = [2]string{perm + "undelete", "name"}
		}
		r["ListRecognizers"] = [2]string{"speech.recognizers.list", "parent"}
		r["ListCustomClasses"] = [2]string{"speech.customClasses.list", "parent"}
		r["ListPhraseSets"] = [2]string{"speech.phraseSets.list", "parent"}
		m := sm(r, "/google.cloud.speech.v2.Speech/")
		// v1's Adaptation (#2106): the v1 RPC reference's permissions.
		v1 := map[string][2]string{"ListPhraseSet": {"speech.phraseSets.list", "parent"}, "ListCustomClasses": {"speech.customClasses.list", "parent"}}
		for kind, plural := range map[string][2]string{"CustomClass": {"customClasses", "custom_class"}, "PhraseSet": {"phraseSets", "phrase_set"}} {
			perm := "speech." + plural[0] + "."
			v1["Create"+kind] = [2]string{perm + "create", "parent"}
			v1["Get"+kind] = [2]string{perm + "get", "name"}
			v1["Update"+kind] = [2]string{perm + "update", plural[1] + ".name"}
			v1["Delete"+kind] = [2]string{perm + "delete", "name"}
		}
		for k, x := range sm(v1, "/google.cloud.speech.v1.Adaptation/") {
			m[k] = x
		}
		return m
	}(),
	Deny: newerDenyOf,
}

// Translation is Cloud Translation v3's table, for the two methods served
// (https://docs.cloud.google.com/translate/docs/reference/rest/v3/projects.locations/translateText
// and .../detectLanguage, read 2026-11-20): translateText one of
// cloudtranslate.generalModels.predict (checked), automl.models.predict or
// aiplatform.endpoints.predict on its model, detectLanguage
// cloudtranslate.languageDetectionModels.predict on its model; the
// model's location (the request's parent) is checked, as the general
// models are the only ones served. A glossary's
// cloudtranslate.glossaries.predict is not checked: no glossary is served.
var Translation = &Service{Name: "translate.googleapis.com", Deny: newerDenyOf}

// AIJSON are the local AI endpoint's JSON methods that its own routes
// (not the REST transcoder) serve, by path: Vertex AI's prediction and
// generation verbs (SpeechJSON and TranslationJSON are the other APIs').
var AIJSON = func() []JSONMethod {
	const loc = `projects/[^/]+/locations/[^/:]+`
	model := `^/(?:v1|v1beta1)/(` + loc + `/publishers/[^/]+/models/[^/:]+):`
	endpoint := `^/(?:v1|v1beta1)/(` + loc + `/endpoints/[^/:]+)`
	return []JSONMethod{
		{Method: "POST", Pattern: regexp.MustCompile(endpoint + `:explain$`), Permission: "aiplatform.endpoints.explain"},
		{Method: "POST", Pattern: regexp.MustCompile(endpoint + `(?::[A-Za-z]+|/chat/completions)$`), Permission: aiPredict},
		{Method: "POST", Pattern: regexp.MustCompile(model + `[A-Za-z]+$`), Permission: aiPredict},
		// Colab Enterprise's REST-only notebookRuntimeTemplates:setDefault
		// (#2105), on the template's location.
		{Method: "POST", Pattern: regexp.MustCompile(`^/v1/(` + loc + `)/notebookRuntimeTemplates:setDefault$`), Permission: "aiplatform.notebookRuntimeTemplates.setDefault"},
	}
}()

// SpeechJSON are Speech-to-Text v2's recognize and batchRecognize by path.
var SpeechJSON = []JSONMethod{
	{Method: "POST", Pattern: regexp.MustCompile(`^/v2/(projects/[^/]+/locations/[^/:]+/recognizers/[^/:]+):(?:recognize|batchRecognize)$`), Permission: "speech.recognizers.recognize"},
}

// TranslationJSON are Translation v3's methods by path.
var TranslationJSON = []JSONMethod{
	{Method: "POST", Pattern: regexp.MustCompile(`^/v3/(projects/[^/]+/locations/[^/:]+):translateText$`), Permission: "cloudtranslate.generalModels.predict"},
	{Method: "POST", Pattern: regexp.MustCompile(`^/v3/(projects/[^/]+/locations/[^/:]+):detectLanguage$`), Permission: "cloudtranslate.languageDetectionModels.predict"},
}

// StreamInterceptor checks the streaming methods of svcs' tables on their
// first request message, as Interceptor checks a unary call's request.
func (e *Enforcer) StreamInterceptor(svcs ...*Service) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if e == nil || e.On == nil || !e.On() {
			return handler(srv, ss)
		}
		for _, svc := range svcs {
			if _, m := ForMethod(svc, info.FullMethod); m != nil {
				return handler(srv, &checkedStream{ServerStream: ss, check: func(msg any) error {
					return e.authorizeMessage(ss.Context(), svc, m, msg)
				}})
			}
		}
		return handler(srv, ss)
	}
}

// checkedStream runs check on the first message received.
type checkedStream struct {
	grpc.ServerStream
	check   func(any) error
	checked bool
}

func (s *checkedStream) RecvMsg(m any) error {
	if err := s.ServerStream.RecvMsg(m); err != nil {
		return err
	}
	if !s.checked {
		s.checked = true
		return s.check(m)
	}
	return nil
}

// authorizeMessage checks m's permission on the resources req's field
// names, and its further checks.
func (e *Enforcer) authorizeMessage(ctx context.Context, svc *Service, m *Method, req any) error {
	msg, ok := req.(proto.Message)
	if !ok {
		return nil
	}
	for _, resource := range fieldValues(msg.ProtoReflect(), m.Field) {
		if err := e.Authorize(ctx, svc, bearer(ctx), m.PermissionOf(resource), resource); err != nil {
			return err
		}
	}
	for _, a := range m.Also {
		for _, r := range actAsFields(msg.ProtoReflect(), []string{a.Field}) {
			if err := e.Authorize(ctx, svc, bearer(ctx), a.Permission, r); err != nil {
				return err
			}
		}
	}
	return nil
}
