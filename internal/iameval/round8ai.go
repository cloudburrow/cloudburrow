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
// Google's wording of a denial is UNVERIFIED for each (re-read
// 2026-11-20: the troubleshooting pages quote only "403: Permission
// denied"): the newer APIs'.

func init() {
	for _, v := range []string{"v1", "v1beta1"} {
		for _, s := range []string{"PredictionService", "LlmUtilityService", "ModelGardenService", "EndpointService", "ModelService"} {
			Services["google.cloud.aiplatform."+v+"."+s] = VertexAI
		}
	}
	Services["google.cloud.documentai.v1.DocumentProcessorService"] = DocumentAI
	Services["google.cloud.speech.v2.Speech"] = Speech
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
// are checked as the predict they stream (UNVERIFIED). ModelGardenService
// Deploy needs aiplatform.endpoints.create, .deploy and
// aiplatform.models.upload on its destination. Vertex AI keeps no
// resource-level policies on what is served.
var VertexAI = &Service{
	Name: "aiplatform.googleapis.com",
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
			deploy := m[p+"ModelGardenService/Deploy"]
			deploy.Also = []Also{{Field: "destination", Permission: "aiplatform.endpoints.deploy"}, {Field: "destination", Permission: "aiplatform.models.upload"}}
			m[p+"ModelGardenService/Deploy"] = deploy
		}
		return m
	}(),
	Deny: newerDenyOf,
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
// read 2026-11-20). Document AI keeps no resource-level policies.
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
		})
		const p = "/google.cloud.documentai.v1.DocumentProcessorService/"
		m[p+"ProcessDocument"] = Method{Field: "name", PermissionFor: docAIByType("processOnline")}
		m[p+"BatchProcessDocuments"] = Method{Field: "name", PermissionFor: docAIByType("processBatch")}
		return m
	}(),
	Deny: newerDenyOf,
}

// Speech is Speech-to-Text v2's table: its RPC reference
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
		return sm(r, "/google.cloud.speech.v2.Speech/")
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
