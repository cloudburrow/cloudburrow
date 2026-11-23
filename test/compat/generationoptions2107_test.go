//go:build compat

package compat

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	aiplatform "cloud.google.com/go/aiplatform/apiv1"
	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/genai"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// The generation options #2107 serves on the local model, through Google's
// own clients: candidateCount, thinkingConfig and context caching.

// covers: google.cloud.aiplatform.v1beta1.PredictionService/GenerateContent
//
// TestGenerateContentCandidateCountThroughTheOfficialSDK: candidateCount 3
// answers three candidates, indexed 0 to 2, each a complete answer and not
// all the same (each its own sample); the
// prompt is counted once (the same count as countTokens) and the candidates'
// tokens summed; a fixed seed repeats candidate by candidate; streaming with
// more than one candidate is refused, as Vertex documents.
func TestGenerateContentCandidateCountThroughTheOfficialSDK(t *testing.T) {
	h := New(t)
	cl := genaiClient(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	prompt := genai.Text("Invent a name for a new colour. Answer with the name only.")
	// No temperature, topP or topK: Google's defaults apply (Gemma 4's
	// documented temperature 1.0, topP 0.95, topK 64), which sample, as
	// Vertex does by default (#2107 round 2).
	cfg := &genai.GenerateContentConfig{CandidateCount: 3, MaxOutputTokens: 16, Seed: genai.Ptr[int32](11)}
	r, err := cl.Models.GenerateContent(ctx, localModel, prompt, cfg)
	if err != nil {
		t.Fatalf("GenerateContent: %v", err)
	}
	if len(r.Candidates) != 3 {
		t.Fatalf("%d candidates, want 3", len(r.Candidates))
	}
	var answers []string
	for i, c := range r.Candidates {
		if c.Index != int32(i) || c.Content == nil || len(c.Content.Parts) == 0 || strings.TrimSpace(c.Content.Parts[0].Text) == "" {
			t.Errorf("candidate %d: index %d, content %+v", i, c.Index, c.Content)
			continue
		}
		answers = append(answers, c.Content.Parts[0].Text)
	}
	t.Logf("three candidates (recorded): %q", answers)
	distinct := map[string]bool{}
	for _, a := range answers {
		distinct[a] = true
	}
	if len(distinct) < 2 {
		t.Errorf("three candidates at the default sampling are one answer %q: they are not independent samples", answers)
	}
	u := r.UsageMetadata
	count, err := cl.Models.CountTokens(ctx, localModel, prompt, nil)
	if err != nil {
		t.Fatalf("CountTokens: %v", err)
	}
	if u == nil || u.PromptTokenCount != count.TotalTokens || u.CandidatesTokenCount < 3 || u.TotalTokenCount != u.PromptTokenCount+u.CandidatesTokenCount {
		t.Errorf("usage %+v, countTokens %d", u, count.TotalTokens)
	}
	again, err := cl.Models.GenerateContent(ctx, localModel, prompt, cfg)
	if err != nil {
		t.Fatalf("GenerateContent again: %v", err)
	}
	for i := range again.Candidates {
		if i < len(answers) && again.Candidates[i].Content.Parts[0].Text != answers[i] {
			t.Errorf("seed 11, candidate %d: %q then %q", i, answers[i], again.Candidates[i].Content.Parts[0].Text)
		}
	}
	// Temperature 0 stays deterministic: greedy decoding, every candidate
	// the same answer, with or without a seed.
	greedy, err := cl.Models.GenerateContent(ctx, localModel, prompt, &genai.GenerateContentConfig{CandidateCount: 3,
		MaxOutputTokens: 16, Temperature: genai.Ptr[float32](0)})
	if err != nil {
		t.Fatalf("GenerateContent temperature 0: %v", err)
	}
	var greedyAnswers []string
	for _, c := range greedy.Candidates {
		greedyAnswers = append(greedyAnswers, c.Content.Parts[0].Text)
	}
	t.Logf("three candidates at temperature 0 (recorded): %q", greedyAnswers)
	if len(greedyAnswers) != 3 || greedyAnswers[0] != greedyAnswers[1] || greedyAnswers[1] != greedyAnswers[2] {
		t.Errorf("temperature 0 candidates differ: %q", greedyAnswers)
	}
	for _, err := range []error{
		func() error {
			for _, err := range cl.Models.GenerateContentStream(ctx, localModel, prompt, &genai.GenerateContentConfig{CandidateCount: 2}) {
				if err != nil {
					return err
				}
			}
			return nil
		}(),
		func() error {
			_, err := cl.Models.GenerateContent(ctx, localModel, prompt, &genai.GenerateContentConfig{CandidateCount: 9})
			return err
		}(),
	} {
		if err == nil || !strings.Contains(err.Error(), "candidateCount") {
			t.Errorf("refusal: %v", err)
		}
	}
}

// covers: google.cloud.aiplatform.v1beta1.PredictionService/GenerateContent, google.cloud.aiplatform.v1beta1.PredictionService/StreamGenerateContent
//
// TestGenerateContentThinkingConfigThroughTheOfficialSDK: thinkingConfig
// turns on Gemma 4's thinking mode. With includeThoughts the thoughts come
// first, as parts marked thought, and usageMetadata counts them apart
// (thoughtsTokenCount, within the budget); thinkingBudget 0 answers without
// thinking; the stream carries thought parts too; thinkingLevel, which this
// model has no levels for, is refused.
func TestGenerateContentThinkingConfigThroughTheOfficialSDK(t *testing.T) {
	h := New(t)
	cl := genaiClient(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	prompt := genai.Text("What is 17 times 23? Answer with the number only.")
	r, err := cl.Models.GenerateContent(ctx, localModel, prompt, &genai.GenerateContentConfig{
		Temperature: genai.Ptr[float32](0), MaxOutputTokens: 256,
		ThinkingConfig: &genai.ThinkingConfig{IncludeThoughts: true, ThinkingBudget: genai.Ptr[int32](64)}})
	if err != nil {
		t.Fatalf("GenerateContent: %v", err)
	}
	parts := r.Candidates[0].Content.Parts
	if len(parts) < 2 || !parts[0].Thought || strings.TrimSpace(parts[0].Text) == "" || parts[len(parts)-1].Thought {
		t.Fatalf("parts %+v", parts)
	}
	u := r.UsageMetadata
	if u.ThoughtsTokenCount <= 0 || u.ThoughtsTokenCount > 64 || u.TotalTokenCount != u.PromptTokenCount+u.CandidatesTokenCount+u.ThoughtsTokenCount {
		t.Errorf("usage %+v", u)
	}
	t.Logf("thought (recorded): %q; answer: %q; usage prompt %d, thoughts %d, candidates %d",
		parts[0].Text, r.Text(), u.PromptTokenCount, u.ThoughtsTokenCount, u.CandidatesTokenCount)

	off, err := cl.Models.GenerateContent(ctx, localModel, prompt, &genai.GenerateContentConfig{
		Temperature: genai.Ptr[float32](0), MaxOutputTokens: 64, ThinkingConfig: &genai.ThinkingConfig{ThinkingBudget: genai.Ptr[int32](0)}})
	if err != nil {
		t.Fatalf("budget 0: %v", err)
	}
	for _, p := range off.Candidates[0].Content.Parts {
		if p.Thought {
			t.Errorf("thinkingBudget 0 returned a thought: %q", p.Text)
		}
	}
	if off.UsageMetadata.ThoughtsTokenCount != 0 || !strings.Contains(off.Text(), "391") {
		t.Errorf("budget 0: thoughts %d, answer %q", off.UsageMetadata.ThoughtsTokenCount, off.Text())
	}

	// Greedy, like the calls above: whether to think at all is the model's
	// choice on its first token (LiteRT-LM's ThinkingBudgetConstraint
	// allows the thought channel's start or an answer token there), so a
	// sampled stream may skip the thinking. At temperature 0 the first token
	// is the one the unary call above took, which opened a thought.
	thoughts := 0
	var streamed strings.Builder
	for resp, err := range cl.Models.GenerateContentStream(ctx, localModel, prompt, &genai.GenerateContentConfig{
		Temperature: genai.Ptr[float32](0), MaxOutputTokens: 48,
		ThinkingConfig: &genai.ThinkingConfig{IncludeThoughts: true, ThinkingBudget: genai.Ptr[int32](16)}}) {
		if err != nil {
			t.Fatalf("stream: %v", err)
		}
		for _, c := range resp.Candidates {
			if c.Content != nil {
				for _, p := range c.Content.Parts {
					if p.Thought {
						thoughts++
					} else {
						streamed.WriteString(p.Text)
					}
				}
			}
		}
	}
	if thoughts == 0 {
		t.Errorf("the stream carried no thought parts; its answer: %q", streamed.String())
	}

	_, err = cl.Models.GenerateContent(ctx, localModel, prompt, &genai.GenerateContentConfig{
		ThinkingConfig: &genai.ThinkingConfig{ThinkingLevel: genai.ThinkingLevelHigh}})
	if err == nil || !strings.Contains(err.Error(), "thinkingLevel") {
		t.Errorf("thinkingLevel: %v", err)
	}
}

// cacheDocument is a document of at least the 2,048 tokens a cache needs,
// with one fact the model can only know from it.
func cacheDocument() string {
	var b strings.Builder
	b.WriteString("Inventory of the Burrow warehouse.\n")
	for i := 0; i < 160; i++ {
		fmt.Fprintf(&b, "Shelf %d holds crate %d of tin buttons.\n", i, i*3)
	}
	b.WriteString("The warehouse keeper's name is Thessaly Quorn.\n")
	return b.String()
}

// covers: google.cloud.aiplatform.v1beta1.GenAiCacheService/CreateCachedContent, google.cloud.aiplatform.v1beta1.GenAiCacheService/GetCachedContent, google.cloud.aiplatform.v1beta1.GenAiCacheService/ListCachedContents, google.cloud.aiplatform.v1beta1.GenAiCacheService/UpdateCachedContent, google.cloud.aiplatform.v1beta1.GenAiCacheService/DeleteCachedContent, google.cloud.aiplatform.v1.GenAiCacheService/GetCachedContent, google.cloud.aiplatform.v1.GenAiCacheService/ListCachedContents, google.cloud.aiplatform.v1.GenAiCacheService/DeleteCachedContent, google.cloud.aiplatform.v1beta1.PredictionService/GenerateContent
//
// TestCachedContentsThroughTheOfficialSDK: the Gen AI SDK's Caches create,
// get, list, update (ttl and expireTime) and delete a cached content; a
// generation that names it answers from its system instruction and
// contents, reporting its tokens as cachedContentTokenCount within
// promptTokenCount; a request that also sets a system instruction, and a
// cache under the minimum, are refused; the v1 GenAiCacheClient reads, lists
// and deletes the same caches over gRPC.
func TestCachedContentsThroughTheOfficialSDK(t *testing.T) {
	h := New(t)
	cl := genaiClient(t, h)
	addr := h.Endpoint(EnvLocalAI)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	sys := &genai.Content{Parts: []*genai.Part{{Text: "You answer questions about the inventory below, in a few words."}}}
	before := time.Now()
	cache, err := cl.Caches.Create(ctx, localModel, &genai.CreateCachedContentConfig{
		DisplayName:       "burrow inventory",
		SystemInstruction: sys,
		Contents:          []*genai.Content{genai.NewContentFromText(cacheDocument(), genai.RoleUser)},
		TTL:               10 * time.Minute,
	})
	if err != nil {
		t.Fatalf("Caches.Create: %v", err)
	}
	t.Cleanup(func() { _, _ = cl.Caches.Delete(context.Background(), cache.Name, nil) })
	if !strings.Contains(cache.Name, "/cachedContents/") || cache.DisplayName != "burrow inventory" || !strings.HasSuffix(cache.Model, "/models/"+localModel) ||
		cache.UsageMetadata == nil || cache.UsageMetadata.TotalTokenCount < 2048 ||
		cache.ExpireTime.Before(before.Add(9*time.Minute)) || cache.ExpireTime.After(time.Now().Add(11*time.Minute)) {
		t.Fatalf("created %+v (usage %+v)", cache, cache.UsageMetadata)
	}

	got, err := cl.Caches.Get(ctx, cache.Name, nil)
	if err != nil || got.Name != cache.Name || got.UsageMetadata.TotalTokenCount != cache.UsageMetadata.TotalTokenCount {
		t.Errorf("Caches.Get: %v %+v", err, got)
	}
	found := false
	for c, err := range cl.Caches.All(ctx) {
		if err != nil {
			t.Fatalf("Caches.All: %v", err)
		}
		found = found || c.Name == cache.Name
	}
	if !found {
		t.Errorf("Caches.All does not list %s", cache.Name)
	}
	upd, err := cl.Caches.Update(ctx, cache.Name, &genai.UpdateCachedContentConfig{TTL: 2 * time.Hour})
	if err != nil || upd.ExpireTime.Before(time.Now().Add(119*time.Minute)) {
		t.Errorf("Caches.Update ttl: %v %+v", err, upd)
	}
	at := time.Now().Add(3 * time.Hour).UTC().Truncate(time.Second)
	upd, err = cl.Caches.Update(ctx, cache.Name, &genai.UpdateCachedContentConfig{ExpireTime: at})
	if err != nil || !upd.ExpireTime.Equal(at) {
		t.Errorf("Caches.Update expireTime: %v %+v", err, upd)
	}

	r, err := cl.Models.GenerateContent(ctx, localModel, genai.Text("What is the warehouse keeper's name?"),
		&genai.GenerateContentConfig{CachedContent: cache.Name, Temperature: genai.Ptr[float32](0), MaxOutputTokens: 24})
	if err != nil {
		t.Fatalf("GenerateContent with the cache: %v", err)
	}
	u := r.UsageMetadata
	if u == nil || u.CachedContentTokenCount != cache.UsageMetadata.TotalTokenCount || u.PromptTokenCount <= u.CachedContentTokenCount ||
		u.TotalTokenCount != u.PromptTokenCount+u.CandidatesTokenCount {
		t.Errorf("usage %+v, cache %d tokens", u, cache.UsageMetadata.TotalTokenCount)
	}
	if !strings.Contains(r.Text(), "Quorn") {
		t.Errorf("the answer did not come from the cached document: %q", r.Text())
	}
	t.Logf("answer (asserted to name Quorn): %q; prompt %d, cached %d", r.Text(), u.PromptTokenCount, u.CachedContentTokenCount)

	_, err = cl.Models.GenerateContent(ctx, localModel, genai.Text("Hi"),
		&genai.GenerateContentConfig{CachedContent: cache.Name, SystemInstruction: sys})
	if err == nil || !strings.Contains(err.Error(), "system_instruction, tools or tool_config") {
		t.Errorf("a system instruction with a cache: %v", err)
	}
	_, err = cl.Caches.Create(ctx, localModel, &genai.CreateCachedContentConfig{
		Contents: []*genai.Content{genai.NewContentFromText("Too short to cache.", genai.RoleUser)}})
	if err == nil || !strings.Contains(err.Error(), "minimum token count") {
		t.Errorf("a cache under the minimum: %v", err)
	}

	// The v1 GenAiCacheClient over gRPC sees the same cache.
	grpcClient, err := aiplatform.NewGenAiCacheClient(ctx, option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatalf("NewGenAiCacheClient: %v", err)
	}
	defer grpcClient.Close()
	gc, err := grpcClient.GetCachedContent(ctx, &aiplatformpb.GetCachedContentRequest{Name: cache.Name})
	if err != nil || gc.GetDisplayName() != "burrow inventory" || !gc.GetExpireTime().AsTime().Equal(at) {
		t.Errorf("gRPC GetCachedContent: %v %v", err, gc)
	}
	parent := cache.Name[:strings.Index(cache.Name, "/cachedContents/")]
	it := grpcClient.ListCachedContents(ctx, &aiplatformpb.ListCachedContentsRequest{Parent: parent})
	listed := false
	for {
		c, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("gRPC ListCachedContents: %v", err)
		}
		listed = listed || c.GetName() == cache.Name
	}
	if !listed {
		t.Errorf("gRPC ListCachedContents does not list %s", cache.Name)
	}
	if err := grpcClient.DeleteCachedContent(ctx, &aiplatformpb.DeleteCachedContentRequest{Name: cache.Name}); err != nil {
		t.Fatalf("gRPC DeleteCachedContent: %v", err)
	}
	if _, err := cl.Caches.Get(ctx, cache.Name, nil); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("Caches.Get after delete: %v", err)
	}
	_, err = grpcClient.GetCachedContent(ctx, &aiplatformpb.GetCachedContentRequest{Name: cache.Name})
	if status.Code(err) != codes.NotFound {
		t.Errorf("gRPC Get after delete: %v", err)
	}
}

// covers: google.cloud.aiplatform.v1beta1.PredictionService/GenerateContent, google.cloud.aiplatform.v1beta1.PredictionService/StreamGenerateContent
//
// TestGenerateContentResponseLogprobsThroughTheOfficialSDK (#2107 round 2):
// responseLogprobs answers each chosen token with its log-probability
// (logprobsResult.chosenCandidates) and avgLogprobs their mean, the tokens
// spelling the answer; the stream carries them chunk by chunk; logprobs (the
// top candidates), which the runtime cannot measure, is refused.
func TestGenerateContentResponseLogprobsThroughTheOfficialSDK(t *testing.T) {
	h := New(t)
	cl := genaiClient(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	prompt := genai.Text("Invent a name for a new colour. Answer with the name only.")
	// Google's default sampling with a fixed seed, so the stream decodes
	// the same tokens as the unary call.
	cfg := &genai.GenerateContentConfig{ResponseLogprobs: true, MaxOutputTokens: 16, Seed: genai.Ptr[int32](7)}
	r, err := cl.Models.GenerateContent(ctx, localModel, prompt, cfg)
	if err != nil {
		t.Fatalf("GenerateContent: %v", err)
	}
	c := r.Candidates[0]
	if c.LogprobsResult == nil || len(c.LogprobsResult.ChosenCandidates) == 0 {
		t.Fatalf("no logprobsResult: %+v", c)
	}
	var spelled strings.Builder
	var sum float64
	negative := false
	for _, lp := range c.LogprobsResult.ChosenCandidates {
		negative = negative || lp.LogProbability < 0
		if lp.LogProbability > 0 || lp.Token == "" {
			t.Errorf("token %+v", lp)
		}
		spelled.WriteString(lp.Token)
		sum += float64(lp.LogProbability)
	}
	t.Logf("answer %q, chosen %+v, avg %v (recorded)", c.Content.Parts[0].Text, c.LogprobsResult.ChosenCandidates, c.AvgLogprobs)
	if !negative {
		t.Errorf("every chosen token has log-probability 0 at temperature 1: %+v", c.LogprobsResult.ChosenCandidates)
	}
	if !strings.Contains(spelled.String(), strings.TrimSpace(c.Content.Parts[0].Text)) {
		t.Errorf("tokens spell %q, answer %q", spelled.String(), c.Content.Parts[0].Text)
	}
	if want := sum / float64(len(c.LogprobsResult.ChosenCandidates)); c.AvgLogprobs > want+1e-4 || c.AvgLogprobs < want-1e-4 {
		t.Errorf("avgLogprobs %v, mean %v", c.AvgLogprobs, want)
	}
	n := 0
	for chunk, err := range cl.Models.GenerateContentStream(ctx, localModel, prompt, cfg) {
		if err != nil {
			t.Fatalf("stream: %v", err)
		}
		if len(chunk.Candidates) > 0 && chunk.Candidates[0].LogprobsResult != nil {
			n += len(chunk.Candidates[0].LogprobsResult.ChosenCandidates)
		}
	}
	if n != len(c.LogprobsResult.ChosenCandidates) {
		t.Errorf("the stream carried %d chosen tokens, the unary answer %d (seed 7)", n, len(c.LogprobsResult.ChosenCandidates))
	}
}

// covers: google.cloud.aiplatform.v1beta1.PredictionService/GenerateContent, google.cloud.aiplatform.v1beta1.PredictionService/StreamGenerateContent
//
// TestGenerateContentLogprobsTopCandidatesThroughTheOfficialSDK (#2107 round
// 3): logprobs N answers each step's N most probable tokens
// (logprobsResult.topCandidates), most probable first, each the model's
// log-softmax before sampling: at temperature 0 the chosen token is the top
// candidate with the same score, and the scores are not all 0 (they are the
// model's, not the greedy sampler's certainty). The stream carries the same
// steps. logprobs outside 1 to 20, or without responseLogprobs, is refused.
func TestGenerateContentLogprobsTopCandidatesThroughTheOfficialSDK(t *testing.T) {
	h := New(t)
	cl := genaiClient(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	prompt := genai.Text("Invent a name for a new colour. Answer with the name only.")
	cfg := &genai.GenerateContentConfig{ResponseLogprobs: true, Logprobs: genai.Ptr[int32](5), Temperature: genai.Ptr[float32](0), MaxOutputTokens: 16}
	r, err := cl.Models.GenerateContent(ctx, localModel, prompt, cfg)
	if err != nil {
		t.Fatalf("GenerateContent: %v", err)
	}
	c := r.Candidates[0]
	lr := c.LogprobsResult
	if lr == nil || len(lr.ChosenCandidates) == 0 || len(lr.TopCandidates) != len(lr.ChosenCandidates) {
		t.Fatalf("logprobsResult %+v", lr)
	}
	negative := false
	for i, step := range lr.TopCandidates {
		chosen := lr.ChosenCandidates[i]
		negative = negative || chosen.LogProbability < 0
		if len(step.Candidates) != 5 {
			t.Errorf("step %d: %d top candidates, want 5", i, len(step.Candidates))
			continue
		}
		for j, tc := range step.Candidates {
			if tc.LogProbability > 0 || (j > 0 && tc.LogProbability > step.Candidates[j-1].LogProbability) {
				t.Errorf("step %d: top candidates not in descending log-probability: %+v", i, step.Candidates)
				break
			}
		}
		if top := step.Candidates[0]; top.TokenID != chosen.TokenID || top.LogProbability != chosen.LogProbability {
			t.Errorf("step %d at temperature 0: chosen %+v, most probable %+v", i, chosen, top)
		}
	}
	if !negative {
		t.Errorf("every chosen token has log-probability 0 at temperature 0: the scores are the greedy sampler's, not the model's")
	}
	var first []string
	for _, tc := range lr.TopCandidates[0].Candidates {
		first = append(first, fmt.Sprintf("%q %.3f", tc.Token, tc.LogProbability))
	}
	t.Logf("answer %q, first step's top candidates %v (recorded)", r.Text(), first)
	n := 0
	for chunk, err := range cl.Models.GenerateContentStream(ctx, localModel, prompt, cfg) {
		if err != nil {
			t.Fatalf("stream: %v", err)
		}
		if len(chunk.Candidates) > 0 && chunk.Candidates[0].LogprobsResult != nil {
			n += len(chunk.Candidates[0].LogprobsResult.TopCandidates)
		}
	}
	if n != len(lr.TopCandidates) {
		t.Errorf("the stream carried %d steps' top candidates, the unary answer %d", n, len(lr.TopCandidates))
	}
	for name, bad := range map[string]*genai.GenerateContentConfig{
		"logprobs 0":          {ResponseLogprobs: true, Logprobs: genai.Ptr[int32](0)},
		"logprobs 21":         {ResponseLogprobs: true, Logprobs: genai.Ptr[int32](21)},
		"no responseLogprobs": {Logprobs: genai.Ptr[int32](3)},
	} {
		_, err := cl.Models.GenerateContent(ctx, localModel, prompt, bad)
		if err == nil || !strings.Contains(err.Error(), "logprobs") || !strings.Contains(err.Error(), "400") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// covers: google.cloud.aiplatform.v1beta1.PredictionService/GenerateContent
//
// TestGenerateContentLogprobsWithOtherFeaturesThroughTheOfficialSDK (#2107
// round 3): responseLogprobs and logprobs combine with every other feature,
// as on Vertex: a JSON response, thinking (the steps are the answer's, not
// the thoughts'), stop sequences (the steps end where the answer is cut),
// function calling and an image part.
func TestGenerateContentLogprobsWithOtherFeaturesThroughTheOfficialSDK(t *testing.T) {
	h := New(t)
	cl := genaiClient(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	base := func() *genai.GenerateContentConfig {
		return &genai.GenerateContentConfig{ResponseLogprobs: true, Logprobs: genai.Ptr[int32](3), Temperature: genai.Ptr[float32](0), MaxOutputTokens: 64}
	}
	// spelled joins the chosen tokens, checking each step on the way.
	spelled := func(t *testing.T, c *genai.Candidate) string {
		t.Helper()
		lr := c.LogprobsResult
		if lr == nil || len(lr.ChosenCandidates) == 0 || len(lr.TopCandidates) != len(lr.ChosenCandidates) {
			t.Fatalf("logprobsResult %+v", lr)
		}
		var b strings.Builder
		for i, ch := range lr.ChosenCandidates {
			if ch.LogProbability > 0 || len(lr.TopCandidates[i].Candidates) != 3 {
				t.Errorf("step %d: chosen %+v, top %d", i, *ch, len(lr.TopCandidates[i].Candidates))
			}
			b.WriteString(ch.Token)
		}
		t.Logf("%d steps spell %q, avgLogprobs %.3f (recorded)", len(lr.ChosenCandidates), b.String(), c.AvgLogprobs)
		return b.String()
	}
	answer := func(c *genai.Candidate) string {
		var b strings.Builder
		for _, p := range c.Content.Parts {
			if !p.Thought {
				b.WriteString(p.Text)
			}
		}
		return strings.TrimSpace(b.String())
	}

	t.Run("json", func(t *testing.T) {
		cfg := base()
		cfg.ResponseMIMEType = "application/json"
		cfg.ResponseSchema = &genai.Schema{Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{"colour": {Type: genai.TypeString}}, Required: []string{"colour"}}
		r, err := cl.Models.GenerateContent(ctx, localModel, genai.Text("Name one colour."), cfg)
		if err != nil {
			t.Fatalf("GenerateContent: %v", err)
		}
		text := answer(r.Candidates[0])
		var v map[string]any
		if err := json.Unmarshal([]byte(text), &v); err != nil || v["colour"] == nil {
			t.Errorf("answer %q is not the schema's JSON: %v", text, err)
		}
		if got := spelled(t, r.Candidates[0]); !strings.Contains(got, text) {
			t.Errorf("tokens spell %q, answer %q", got, text)
		}
	})
	t.Run("thinking", func(t *testing.T) {
		cfg := base()
		cfg.MaxOutputTokens = 256
		cfg.ThinkingConfig = &genai.ThinkingConfig{IncludeThoughts: true, ThinkingBudget: genai.Ptr[int32](64)}
		r, err := cl.Models.GenerateContent(ctx, localModel, genai.Text("What is 17 times 23? Answer with the number only."), cfg)
		if err != nil {
			t.Fatalf("GenerateContent: %v", err)
		}
		c := r.Candidates[0]
		if len(c.Content.Parts) < 2 || !c.Content.Parts[0].Thought {
			t.Fatalf("parts %+v: no thought", c.Content.Parts)
		}
		text := answer(c)
		got := spelled(t, c)
		// The thought is longer than the answer, so tokens that spelled it
		// too would show.
		thought := strings.TrimSpace(c.Content.Parts[0].Text)
		if !strings.Contains(got, text) || (len(thought) > len(text) && strings.Contains(got, thought)) || strings.Contains(got, "<|channel>") {
			t.Errorf("tokens spell %q: answer %q, thought %q", got, text, c.Content.Parts[0].Text)
		}
	})
	t.Run("stopSequences", func(t *testing.T) {
		cfg := base()
		cfg.StopSequences = []string{","}
		r, err := cl.Models.GenerateContent(ctx, localModel, genai.Text("List the colours of the rainbow, separated by commas."), cfg)
		if err != nil {
			t.Fatalf("GenerateContent: %v", err)
		}
		text := answer(r.Candidates[0])
		got := spelled(t, r.Candidates[0])
		if strings.Contains(text, ",") || strings.Contains(got, ",") || !strings.Contains(got, text) {
			t.Errorf("stop at \",\": answer %q, tokens spell %q", text, got)
		}
	})
	t.Run("tools", func(t *testing.T) {
		cfg := base()
		cfg.Tools = []*genai.Tool{{FunctionDeclarations: []*genai.FunctionDeclaration{{
			Name:        "get_weather",
			Description: "Returns the current weather for a city.",
			Parameters: &genai.Schema{Type: genai.TypeObject,
				Properties: map[string]*genai.Schema{"city": {Type: genai.TypeString}}, Required: []string{"city"}},
		}}}}
		cfg.ToolConfig = &genai.ToolConfig{FunctionCallingConfig: &genai.FunctionCallingConfig{Mode: genai.FunctionCallingConfigModeAny}}
		r, err := cl.Models.GenerateContent(ctx, localModel, genai.Text("What is the weather in Paris?"), cfg)
		if err != nil {
			t.Fatalf("GenerateContent: %v", err)
		}
		if calls := r.FunctionCalls(); len(calls) == 0 || calls[0].Name != "get_weather" {
			t.Fatalf("no get_weather call: %+v", r.Candidates[0].Content)
		}
		if got := spelled(t, r.Candidates[0]); !strings.Contains(got, "get_weather") {
			t.Errorf("the call's tokens spell %q", got)
		}
	})
	t.Run("image", func(t *testing.T) {
		contents := []*genai.Content{genai.NewContentFromParts([]*genai.Part{
			genai.NewPartFromBytes(redPNG(t), "image/png"),
			genai.NewPartFromText("What colour is this image? One word."),
		}, genai.RoleUser)}
		r, err := cl.Models.GenerateContent(ctx, localModel, contents, base())
		if err != nil {
			t.Fatalf("GenerateContent: %v", err)
		}
		text := answer(r.Candidates[0])
		if got := spelled(t, r.Candidates[0]); !strings.Contains(got, text) || !strings.Contains(strings.ToLower(text), "red") {
			t.Errorf("answer %q, tokens spell %q", text, got)
		}
	})
}
