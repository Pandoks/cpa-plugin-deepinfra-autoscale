package main

import (
	"encoding/json"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParseGPU(t *testing.T) {
	cases := []struct {
		in      string
		wantGPU string
		wantNum int
		wantErr bool
	}{
		{"2xH200-141GB", "H200-141GB", 2, false},
		{"1xA100-80GB", "A100-80GB", 1, false},
		{"8xB200-180GB", "B200-180GB", 8, false},
		{"H200-141GB", "", 0, true},
		{"0xH200-141GB", "", 0, true},
		{"", "", 0, true},
	}
	for _, c := range cases {
		gpu, num, err := parseGPU(c.in)
		if (err != nil) != c.wantErr {
			t.Errorf("parseGPU(%q) err=%v wantErr=%v", c.in, err, c.wantErr)
			continue
		}
		if err == nil && (gpu != c.wantGPU || num != c.wantNum) {
			t.Errorf("parseGPU(%q)=(%q,%d) want (%q,%d)", c.in, gpu, num, c.wantGPU, c.wantNum)
		}
	}
}

func TestNormalizeModelID(t *testing.T) {
	cases := map[string]string{
		"red-snow":       "red-snow",
		"red-snow[1m]":   "red-snow",
		"red-snow[200k]": "red-snow",
		"a[1m][x]":       "a[1m]",
		"":               "",
	}
	for in, want := range cases {
		if got := normalizeModelID(in); got != want {
			t.Errorf("normalizeModelID(%q)=%q want %q", in, got, want)
		}
	}
}

func TestModelValidate(t *testing.T) {
	good := &Model{ID: "red-snow", HFRepo: "Owner/Model", GPUs: []string{"2xH200-141GB", "1xB300-270GB"}, Context: 1048576, LoopGuard: loopGuardOn}
	good.normalize()
	if err := good.validate(); err != nil {
		t.Fatalf("good model rejected: %v", err)
	}
	if good.MaxBatchSize != defaultMaxBatchSize {
		t.Errorf("normalize MaxBatchSize=%d want %d", good.MaxBatchSize, defaultMaxBatchSize)
	}

	bad := []*Model{
		{ID: "bad id!", HFRepo: "Owner/Model", GPUs: []string{"2xH200-141GB"}, Context: 1},
		{ID: "ok", HFRepo: "noslash", GPUs: []string{"2xH200-141GB"}, Context: 1},
		{ID: "ok", HFRepo: "a/b/c", GPUs: []string{"2xH200-141GB"}, Context: 1},
		{ID: "ok", HFRepo: "Owner/Model", GPUs: []string{"H200"}, Context: 1},
		{ID: "ok", HFRepo: "Owner/Model", GPUs: []string{"2xH200-141GB"}, Context: 0},
		{ID: "ok", HFRepo: "Owner/Model", GPUs: []string{"2xH200-141GB"}, Context: 1, GenerationConfig: map[string]any{"presence": 1.5}},
		{ID: "ok", HFRepo: "Owner/Model", GPUs: []string{"2xH200-141GB"}, Context: 1, GenerationConfig: map[string]any{"top_p": "0.9"}},
		{ID: "ok", HFRepo: "Owner/Model", GPUs: []string{"2xH200-141GB"}, Context: 1, SpeculativeConfig: map[string]any{"model": "Owner/Draft"}},
		{ID: "ok", HFRepo: "Owner/Model", GPUs: []string{"2xH200-141GB"}, Context: 1, LoopGuard: "auto"},
		{ID: "ok", HFRepo: "Owner/Model", Context: 1},
		{ID: "ok", HFRepo: "Owner/Model", GPUs: []string{"1xA100-80GB", "1xA100-80GB"}, Context: 1},
	}
	for i, m := range bad {
		if err := m.validate(); err == nil {
			t.Errorf("bad model %d accepted", i)
		}
	}
}

func newTestStore(t *testing.T) *store {
	t.Helper()
	return &store{
		models:       map[string]*Model{},
		lastUsed:     map[string]int64{},
		runStart:     map[string]int64{},
		failedGPUs:   map[string]map[string]int64{},
		provisionErr: map[string]string{},
		path:         filepath.Join(t.TempDir(), "state.json"),
	}
}

func TestStoreAddRemovePersist(t *testing.T) {
	s := newTestStore(t)
	m := &Model{ID: "red-snow", HFRepo: "Owner/Model", GPU: "2xH200-141GB", Context: 1048576}
	m.normalize()
	if err := s.add(m); err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := s.add(m); err == nil {
		t.Error("duplicate add should fail")
	}
	if !s.has("red-snow") {
		t.Error("has(red-snow) false after add")
	}

	// get returns a copy (mutation must not leak into the store).
	got, _ := s.get("red-snow")
	got.HFRepo = "Mutated/X"
	again, _ := s.get("red-snow")
	if again.HFRepo != "Owner/Model" {
		t.Error("get did not return an independent copy")
	}

	// Persistence round-trip.
	s2 := &store{models: map[string]*Model{}, lastUsed: map[string]int64{}, runStart: map[string]int64{}}
	s2.load(s.path)
	if !s2.has("red-snow") {
		t.Fatal("reloaded store missing model")
	}
	reloaded, _ := s2.get("red-snow")
	if len(reloaded.GPUs) != 1 || reloaded.GPUs[0] != "2xH200-141GB" || reloaded.Context != 1048576 {
		t.Errorf("reloaded model wrong: %+v", reloaded)
	}

	if _, ok := s.remove("red-snow"); !ok {
		t.Error("remove returned not-ok")
	}
	if s.has("red-snow") {
		t.Error("model still present after remove")
	}
	s3 := &store{models: map[string]*Model{}, lastUsed: map[string]int64{}, runStart: map[string]int64{}}
	s3.load(s.path)
	if s3.has("red-snow") {
		t.Error("removed model persisted")
	}
}

func TestStoreActivity(t *testing.T) {
	s := newTestStore(t)
	m := &Model{ID: "m", HFRepo: "O/M", GPU: "1xA100-80GB", Context: 1024}
	m.normalize()
	_ = s.add(m)
	s.markUsed("m", 100)
	s.markRunning("m", 90)
	last, run := s.activity("m")
	if last != 100 || run != 90 {
		t.Errorf("activity=(%d,%d) want (100,90)", last, run)
	}
	s.clearRunning("m")
	_, run = s.activity("m")
	if run != 0 {
		t.Errorf("runStart=%d after clear want 0", run)
	}
	// markUsed for an unknown model is a no-op.
	s.markUsed("ghost", 500)
	if l, _ := s.activity("ghost"); l != 0 {
		t.Error("markUsed recorded an unknown model")
	}
}

func TestApplyConfigDefaults(t *testing.T) {
	cfg := applyConfigYAML(nil)
	if cfg.IdleMinutes != defaultIdleMinutes {
		t.Errorf("defaults not applied: %+v", cfg)
	}
	if cfg.StateFile == "" {
		t.Error("state file default empty")
	}

	cfg = applyConfigYAML([]byte("idle-minutes: 5\n"))
	if cfg.IdleMinutes != 5 {
		t.Errorf("idle-minutes=%d want 5", cfg.IdleMinutes)
	}
}

// DeepInfra reserves a deleted deployment's name, so re-creating "red-snow" under the same name
// failed with "Model name 'Pandoks/red-snow' is already in use".
func TestDeploymentNameIsFreshAndSafe(t *testing.T) {
	first := deploymentName("Red-Snow_5.3", time.Unix(1_800_000_000, 0))
	second := deploymentName("Red-Snow_5.3", time.Unix(1_800_000_001, 0))
	if first == second {
		t.Errorf("names repeat across creates: %q", first)
	}
	if !regexp.MustCompile(`^red-snow-5-3-[a-z0-9]+$`).MatchString(first) {
		t.Errorf("unsafe deployment name %q", first)
	}
}

func TestDICreateBody(t *testing.T) {
	m := &Model{
		ID: "red-snow", HFRepo: "Owner/Model", Revision: "abc123", GPU: "2xH200-141GB",
		Context: 1048576, ToolParser: "glm47", ReasoningParser: "glm45", EnableThinking: true,
		GenerationConfig:  map[string]any{"presence_penalty": 1.5},
		SpeculativeConfig: map[string]any{"method": "dflash", "model": "Owner/Draft", "num_speculative_tokens": 8},
	}
	m.normalize()
	body := diCreateBody(m, "2xH200-141GB", "red-snow-abc")

	if body["model_name"] != "red-snow-abc" || body["gpu"] != "H200-141GB" || body["num_gpus"] != 2 {
		t.Errorf("base fields wrong: %+v", body)
	}
	hf := body["hf"].(map[string]any)
	if hf["repo"] != "Owner/Model" || hf["revision"] != "abc123" {
		t.Errorf("hf wrong: %+v", hf)
	}
	sa := body["standard_args"].(map[string]any)
	if sa["max_context_size"] != int64(1048576) {
		t.Errorf("max_context_size=%v", sa["max_context_size"])
	}
	if _, ok := sa["quantization"]; ok {
		t.Error("quantization should be absent when unset")
	}
	ea := body["extra_args"].([]string)
	joined := ""
	for _, a := range ea {
		joined += a + " "
	}
	for _, want := range []string{"--enable-auto-tool-choice", "glm47", "--reasoning-parser", "glm45", "enable_thinking",
		`--override-generation-config {"presence_penalty":1.5}`,
		`--speculative-config {"method":"dflash","model":"Owner/Draft","num_speculative_tokens":8}`} {
		if !contains(joined, want) {
			t.Errorf("extra_args missing %q: %v", want, ea)
		}
	}

	// With quantization set and no parsers: extra_args omitted, quantization present.
	m2 := &Model{ID: "x", HFRepo: "O/M", GPU: "1xA100-80GB", Context: 4096, Quantization: "fp8"}
	m2.normalize()
	b2 := diCreateBody(m2, "1xA100-80GB", "x-abc")
	if _, ok := b2["extra_args"]; ok {
		t.Error("extra_args should be omitted with no parsers")
	}
	if b2["standard_args"].(map[string]any)["quantization"] != "fp8" {
		t.Error("quantization not set")
	}
}

func TestDashboardStatus(t *testing.T) {
	if dashboardStatus(nil) != "scaled-to-zero" {
		t.Error("nil should be scaled-to-zero")
	}
	if dashboardStatus(&diDeployment{Status: "stopped"}) != "scaled-to-zero" {
		t.Error("stopped should map to scaled-to-zero")
	}
	if dashboardStatus(&diDeployment{Status: "running"}) != "running" {
		t.Error("running mismatch")
	}
	if dashboardStatus(&diDeployment{Status: "failed"}) != "failed" {
		t.Error("failed mismatch")
	}
	if dashboardStatus(&diDeployment{Status: "downloading"}) != "downloading" {
		t.Error("downloading mismatch")
	}
}

func TestRewriteModel(t *testing.T) {
	out := rewriteModel([]byte(`{"model":"red-snow[1m]","messages":[]}`), "Pandoks/red-snow", false, true)
	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("output not json: %v", err)
	}
	if parsed["model"] != "Pandoks/red-snow" {
		t.Errorf("model=%v want Pandoks/red-snow", parsed["model"])
	}
	if _, ok := parsed["messages"]; !ok {
		t.Error("messages dropped")
	}
	// Invalid JSON is returned unchanged.
	bad := []byte(`not json`)
	if string(rewriteModel(bad, "x", false, true)) != "not json" {
		t.Error("invalid json should be unchanged")
	}
	if _, ok := parsed["thinking"]; ok {
		t.Error("thinking set for a model with thinking on")
	}

	// A model with thinking off turns it off in each request, in the client's format.
	json.Unmarshal(rewriteModel([]byte(`{"messages":[]}`), "x", true, true), &parsed)
	if thinking, _ := parsed["thinking"].(map[string]any); thinking["type"] != "disabled" {
		t.Errorf("anthropic thinking=%v want disabled", parsed["thinking"])
	}
	parsed = nil
	json.Unmarshal(rewriteModel([]byte(`{"messages":[],"chat_template_kwargs":{"a":1}}`), "x", true, false), &parsed)
	if kwargs, _ := parsed["chat_template_kwargs"].(map[string]any); kwargs["enable_thinking"] != false || kwargs["a"] != float64(1) {
		t.Errorf("openai chat_template_kwargs=%v", parsed["chat_template_kwargs"])
	}
}

func TestShouldStop(t *testing.T) {
	const idle = int64(1800) // 30 min
	now := int64(1_000_000)
	cases := []struct {
		name               string
		lastUsed, runStart int64
		want               bool
	}{
		{"active recently", now - 10, now - 100, false},
		{"idle past window", now - 3600, now - 4000, true},
		{"woken never used, past window", 0, now - 3600, true},
		{"woken never used, within window", 0, now - 10, false},
		{"busy for days stays up", now - 5, now - 999999, false},
		{"no deployment record", 0, 0, false},
	}
	for _, c := range cases {
		if got := shouldStop(c.lastUsed, c.runStart, now, idle); got != c.want {
			t.Errorf("%s: shouldStop=%v want %v", c.name, got, c.want)
		}
	}
}

func TestMatchesModel(t *testing.T) {
	m := &Model{ID: "flash", DeployID: "DEP1", DeepInfraName: "Pandoks/flash"}
	// Exact deploy id.
	if !(&diDeployment{DeployID: "DEP1", ModelName: "Pandoks/flash", Status: "running"}).matchesModel(m) {
		t.Error("exact deploy id should match")
	}
	// Exact name (different/empty deploy id).
	if !(&diDeployment{DeployID: "other", ModelName: "Pandoks/flash", Status: "running"}).matchesModel(m) {
		t.Error("exact name should match")
	}
	// Foreign deployment sharing the last segment must NOT match.
	if (&diDeployment{DeployID: "x", ModelName: "someorg/flash", Status: "running"}).matchesModel(m) {
		t.Error("foreign suffix must NOT match")
	}
	// Gone status never matches.
	if (&diDeployment{DeployID: "DEP1", ModelName: "Pandoks/flash", Status: "deleting"}).matchesModel(m) {
		t.Error("gone status must not match")
	}
	// A never-provisioned model (no id, no name) matches nothing.
	none := &Model{ID: "flash"}
	if (&diDeployment{ModelName: "someorg/flash", Status: "running"}).matchesModel(none) {
		t.Error("never-provisioned model must not suffix-match a foreign deployment")
	}
}

func TestAddRollbackOnSaveFailure(t *testing.T) {
	// An unwritable state path makes saveLocked fail; add must roll back so the model isn't left
	// half-added in memory.
	s := &store{models: map[string]*Model{}, lastUsed: map[string]int64{}, runStart: map[string]int64{},
		idLocks: map[string]*sync.Mutex{}, path: filepath.Join(t.TempDir(), "nonexistent-dir\x00bad", "s.json")}
	m := &Model{ID: "x", HFRepo: "O/M", GPU: "1xA100-80GB", Context: 1024}
	m.normalize()
	if err := s.add(m); err == nil {
		t.Error("add should fail when persistence fails")
	}
	if s.has("x") {
		t.Error("add must roll back the in-memory entry on persistence failure")
	}
}

// State saved before GPU lists has one "gpu", which is both the choice and the deployment's GPU.
func TestNormalizeKeepsSingleGPUState(t *testing.T) {
	var m Model
	if err := json.Unmarshal([]byte(`{"id":"m","hf_repo":"O/M","gpu":"1xB300-270GB","context":1024,"deploy_id":"D1"}`), &m); err != nil {
		t.Fatal(err)
	}
	m.normalize()
	if err := m.validate(); err != nil {
		t.Fatalf("old state rejected: %v", err)
	}
	if len(m.GPUs) != 1 || m.GPUs[0] != "1xB300-270GB" || m.GPU != "1xB300-270GB" {
		t.Errorf("GPUs=%v GPU=%q", m.GPUs, m.GPU)
	}
}

func TestRankGPUs(t *testing.T) {
	catalog := []gpuOption{
		{Name: "1xB300-270GB", USDPerHour: 4.89, Available: false},
		{Name: "2xH200-141GB", USDPerHour: 5.38, Available: true},
		{Name: "1xH200-141GB", USDPerHour: 2.69, Available: true},
		{Name: "2xA100-80GB", USDPerHour: 1.78, Available: true},
		{Name: "1xRTXPRO6000-96GB", USDPerHour: 1.78, Available: true},
	}
	wanted := []string{"2xH200-141GB", "1xB300-270GB", "1xRTXPRO6000-96GB", "1xH200-141GB", "2xA100-80GB", "8xB200-180GB"}
	got := rankGPUs(wanted, catalog, map[string]bool{"1xH200-141GB": true})
	// Unavailable, recently failed and unknown GPUs drop out; equal prices keep the wanted order.
	want := []string{"1xRTXPRO6000-96GB", "2xA100-80GB", "2xH200-141GB"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("rankGPUs=%v want %v", got, want)
	}
	if got := rankGPUs([]string{"1xB300-270GB"}, catalog, nil); len(got) != 0 {
		t.Errorf("only unavailable GPUs gave %v", got)
	}
}

// A start without capacity is accepted, and DeepInfra later returns the deployment to stopped with a
// fail reason, as with "no capacity for 1xB300-270GB right now, try again later".
func TestStartFailed(t *testing.T) {
	started := time.Date(2026, 11, 6, 19, 50, 0, 0, time.UTC).Unix()
	failed := diDeployment{Status: "stopped", FailReason: "no capacity for 1xB300-270GB right now, try again later", UpdatedAt: "2026-11-06T19:56:00+00:00"}
	if !failed.startFailed(started) {
		t.Error("stopped with a fail reason after the start should count as a failed start")
	}
	if failed.startFailed(0) {
		t.Error("a deployment nobody started has no failed start")
	}
	if failed.startFailed(started + 3600) {
		t.Error("a fail reason from before the start is stale")
	}
	idle := diDeployment{Status: "stopped", UpdatedAt: "2026-11-06T19:56:00+00:00"}
	if idle.startFailed(started) {
		t.Error("stopped without a fail reason is a normal stop")
	}
	starting := failed
	starting.Status = "deploying"
	if starting.startFailed(started) {
		t.Error("a deployment still starting has not failed")
	}
}

func TestNeedsRedeploy(t *testing.T) {
	old := &Model{ID: "m", HFRepo: "O/M", GPUs: []string{"1xH200-141GB", "2xA100-80GB"}, Context: 1024,
		GenerationConfig: map[string]any{"presence_penalty": 1.5}, DeployID: "D1", GPU: "1xH200-141GB"}
	old.normalize()
	edit := func(change func(*Model)) *Model {
		m := *old
		m.DeployID, m.GPU = "", ""
		m.GenerationConfig = map[string]any{"presence_penalty": 1.5}
		change(&m)
		return &m
	}
	cases := []struct {
		name   string
		change func(*Model)
		want   bool
	}{
		{"nothing", func(m *Model) {}, false},
		{"name in Claude Code", func(m *Model) { m.DisplayName = "New" }, false},
		{"loop guard", func(m *Model) { m.LoopGuard = loopGuardOn }, false},
		{"GPU added", func(m *Model) { m.GPUs = append([]string{"1xB300-270GB"}, m.GPUs...) }, false},
		{"other GPU removed", func(m *Model) { m.GPUs = []string{"1xH200-141GB"} }, false},
		{"deployed GPU removed", func(m *Model) { m.GPUs = []string{"2xA100-80GB"} }, true},
		{"context", func(m *Model) { m.Context = 2048 }, true},
		{"sampling", func(m *Model) { m.GenerationConfig = map[string]any{"presence_penalty": 1.0} }, true},
		{"repo", func(m *Model) { m.HFRepo = "O/Other" }, true},
	}
	for _, c := range cases {
		if got := edit(c.change).needsRedeploy(old); got != c.want {
			t.Errorf("%s: needsRedeploy=%v want %v", c.name, got, c.want)
		}
	}
}

func TestRecentlyFailedExpires(t *testing.T) {
	s := newTestStore(t)
	m := &Model{ID: "m", HFRepo: "O/M", GPUs: []string{"1xA100-80GB"}, Context: 1024}
	_ = s.add(m)
	s.markGPUFailed("m", "1xA100-80GB", 1000)
	if !s.recentlyFailed("m", 1000+gpuRetryAfter-1)["1xA100-80GB"] {
		t.Error("GPU should be skipped within the retry window")
	}
	if s.recentlyFailed("m", 1000+gpuRetryAfter)["1xA100-80GB"] {
		t.Error("GPU should be retried after the window")
	}
}

func TestStoreUpdate(t *testing.T) {
	s := newTestStore(t)
	m := &Model{ID: "m", HFRepo: "O/M", GPUs: []string{"1xA100-80GB"}, Context: 1024}
	_ = s.add(m)
	if err := s.update(&Model{ID: "ghost", HFRepo: "O/M", GPUs: []string{"1xA100-80GB"}, Context: 1024}); err == nil {
		t.Error("update of an unknown model should fail")
	}
	if err := s.update(&Model{ID: "m", HFRepo: "O/M", GPUs: []string{"2xA100-80GB", "1xA100-80GB"}, Context: 2048}); err != nil {
		t.Fatalf("update: %v", err)
	}
	s2 := newTestStore(t)
	s2.load(s.path)
	if got, _ := s2.get("m"); got.Context != 2048 || len(got.GPUs) != 2 {
		t.Errorf("update not persisted: %+v", got)
	}

	s.path = filepath.Join(t.TempDir(), "nonexistent-dir\x00bad", "s.json")
	if err := s.update(&Model{ID: "m", HFRepo: "O/M", GPUs: []string{"1xA100-80GB"}, Context: 4096}); err == nil {
		t.Error("update should fail when persistence fails")
	}
	if got, _ := s.get("m"); got.Context != 2048 {
		t.Errorf("failed update not rolled back: context=%d", got.Context)
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
