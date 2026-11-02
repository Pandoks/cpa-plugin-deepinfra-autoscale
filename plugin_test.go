package main

import (
	"encoding/json"
	"path/filepath"
	"regexp"
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
		"red-snow":      "red-snow",
		"red-snow[1m]":  "red-snow",
		"red-snow[200k]": "red-snow",
		"a[1m][x]":      "a[1m]",
		"":              "",
	}
	for in, want := range cases {
		if got := normalizeModelID(in); got != want {
			t.Errorf("normalizeModelID(%q)=%q want %q", in, got, want)
		}
	}
}

func TestModelValidate(t *testing.T) {
	good := &Model{ID: "red-snow", HFRepo: "Owner/Model", GPU: "2xH200-141GB", Context: 1048576}
	good.normalize()
	if err := good.validate(); err != nil {
		t.Fatalf("good model rejected: %v", err)
	}
	if good.NumGPUs != 2 {
		t.Errorf("normalize NumGPUs=%d want 2", good.NumGPUs)
	}
	if good.MaxBatchSize != defaultMaxBatchSize {
		t.Errorf("normalize MaxBatchSize=%d want %d", good.MaxBatchSize, defaultMaxBatchSize)
	}

	bad := []*Model{
		{ID: "bad id!", HFRepo: "Owner/Model", GPU: "2xH200-141GB", Context: 1},
		{ID: "ok", HFRepo: "noslash", GPU: "2xH200-141GB", Context: 1},
		{ID: "ok", HFRepo: "a/b/c", GPU: "2xH200-141GB", Context: 1},
		{ID: "ok", HFRepo: "Owner/Model", GPU: "H200", Context: 1},
		{ID: "ok", HFRepo: "Owner/Model", GPU: "2xH200-141GB", Context: 0},
		{ID: "ok", HFRepo: "Owner/Model", GPU: "2xH200-141GB", Context: 1, GenerationConfig: map[string]any{"presence": 1.5}},
		{ID: "ok", HFRepo: "Owner/Model", GPU: "2xH200-141GB", Context: 1, GenerationConfig: map[string]any{"top_p": "0.9"}},
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
		models:   map[string]*Model{},
		lastUsed: map[string]int64{},
		runStart: map[string]int64{},
		path:     filepath.Join(t.TempDir(), "state.json"),
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
	if reloaded.NumGPUs != 2 || reloaded.Context != 1048576 {
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
		GenerationConfig: map[string]any{"presence_penalty": 1.5},
	}
	m.normalize()
	body := diCreateBody(m, "red-snow-abc")

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
		`--override-generation-config {"presence_penalty":1.5}`} {
		if !contains(joined, want) {
			t.Errorf("extra_args missing %q: %v", want, ea)
		}
	}

	// With quantization set and no parsers: extra_args omitted, quantization present.
	m2 := &Model{ID: "x", HFRepo: "O/M", GPU: "1xA100-80GB", Context: 4096, Quantization: "fp8"}
	m2.normalize()
	b2 := diCreateBody(m2, "x-abc")
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
	out := rewriteModel([]byte(`{"model":"red-snow[1m]","messages":[]}`), "Pandoks/red-snow")
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
	if string(rewriteModel(bad, "x")) != "not json" {
		t.Error("invalid json should be unchanged")
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
