package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Model is a user-defined DeepInfra-hosted model, managed from the dashboard and persisted to the
// state file so it survives restarts.
type Model struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name,omitempty"`
	HFRepo      string `json:"hf_repo"`
	Revision    string `json:"revision,omitempty"`
	// GPUs are the configs the model may run on, e.g. "2xH200-141GB"; provisioning takes the
	// cheapest one DeepInfra has available and moves down the list when one fails.
	GPUs            []string `json:"gpus"`
	Context         int64    `json:"context"`
	Quantization    string   `json:"quantization,omitempty"`
	ToolParser      string   `json:"tool_parser,omitempty"`
	ReasoningParser string   `json:"reasoning_parser,omitempty"`
	EnableThinking  bool     `json:"enable_thinking,omitempty"`
	MaxBatchSize    int      `json:"max_batch_size,omitempty"`
	// GenerationConfig is the deployment's default sampling, for clients like Claude Code that send none.
	GenerationConfig map[string]any `json:"generation_config,omitempty"`
	// SpeculativeConfig is vLLM's --speculative-config, e.g. a draft model that speeds up decoding.
	SpeculativeConfig map[string]any `json:"speculative_config,omitempty"`
	// LoopGuard is "on", "observe" or "off" (the default); see loopguard.go.
	LoopGuard string `json:"loop_guard,omitempty"`

	// Deployment linkage, learned at create/reconcile time and persisted. GPU is the config the
	// deployment was created on.
	DeployID      string `json:"deploy_id,omitempty"`
	DeepInfraName string `json:"deepinfra_name,omitempty"`
	GPU           string `json:"gpu,omitempty"`
}

const defaultMaxBatchSize = 64

var modelIDPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)

// validate checks a model definition submitted from the dashboard.
func (m *Model) validate() error {
	if !modelIDPattern.MatchString(m.ID) {
		return fmt.Errorf("invalid model id %q (allowed: letters, digits, . _ -, max 64)", m.ID)
	}
	if strings.Count(m.HFRepo, "/") != 1 || strings.TrimSpace(m.HFRepo) != m.HFRepo || m.HFRepo == "" {
		return fmt.Errorf("invalid hf_repo %q (expected owner/name)", m.HFRepo)
	}
	if len(m.GPUs) == 0 {
		return fmt.Errorf("select at least one GPU")
	}
	seen := map[string]bool{}
	for _, gpu := range m.GPUs {
		if _, _, err := parseGPU(gpu); err != nil {
			return err
		}
		if seen[gpu] {
			return fmt.Errorf("gpu %q is listed twice", gpu)
		}
		seen[gpu] = true
	}
	if m.Context <= 0 {
		return fmt.Errorf("context must be positive")
	}
	if utf8.RuneCountInString(m.DisplayName) > 64 {
		return fmt.Errorf("display name is longer than 64 characters")
	}
	for key, value := range m.GenerationConfig {
		if !generationConfigKeys[key] {
			return fmt.Errorf("unsupported generation_config key %q", key)
		}
		if _, ok := value.(float64); !ok {
			return fmt.Errorf("generation_config %q must be a number", key)
		}
	}
	if m.SpeculativeConfig != nil {
		if method, _ := m.SpeculativeConfig["method"].(string); method == "" {
			return fmt.Errorf("speculative_config needs a method, e.g. dflash")
		}
	}
	switch m.LoopGuard {
	case "", loopGuardOff, loopGuardObserve, loopGuardOn:
	default:
		return fmt.Errorf("loop_guard must be on, observe or off, not %q", m.LoopGuard)
	}
	return nil
}

// generationConfigKeys are the sampling keys vLLM reads from --override-generation-config; it
// silently drops any other key.
var generationConfigKeys = map[string]bool{
	"repetition_penalty": true, "presence_penalty": true, "frequency_penalty": true, "temperature": true,
	"top_k": true, "top_p": true, "min_p": true, "max_new_tokens": true,
}

// normalize fills derived/default fields.
func (m *Model) normalize() {
	m.DisplayName = strings.TrimSpace(m.DisplayName)
	if len(m.GPUs) == 0 && m.GPU != "" {
		m.GPUs = []string{m.GPU} // state from before GPU lists
	}
	if m.MaxBatchSize <= 0 {
		m.MaxBatchSize = defaultMaxBatchSize
	}
	// An empty map is "unset": the list view omits it, so keep it nil or an edit would look like a change.
	if len(m.GenerationConfig) == 0 {
		m.GenerationConfig = nil
	}
	if len(m.SpeculativeConfig) == 0 {
		m.SpeculativeConfig = nil
	}
}

var gpuPattern = regexp.MustCompile(`^(\d+)x(.+)$`)

// needsRedeploy reports whether updating a model from old to m invalidates its deployment: any
// setting baked in at creation changed, or its GPU was taken off the list. The name in Claude Code
// and the loop guard apply per request, and a new GPU list applies at the next wake.
func (m *Model) needsRedeploy(old *Model) bool {
	if old.GPU != "" && !slices.Contains(m.GPUs, old.GPU) {
		return true
	}
	return m.settingsChanged(old)
}

// settingsChanged reports whether any deployment setting differs from old's, i.e. the GPU list
// and the other per-request fields are not compared.
func (m *Model) settingsChanged(old *Model) bool {
	return !reflect.DeepEqual(old.deploymentSettings(), m.deploymentSettings())
}

func (m Model) deploymentSettings() Model {
	m.DisplayName, m.LoopGuard, m.GPUs = "", "", nil
	m.DeployID, m.DeepInfraName, m.GPU = "", "", ""
	return m
}

// parseGPU splits a dashboard GPU string like "2xH200-141GB" into the DeepInfra gpu name
// ("H200-141GB") and GPU count (2).
func parseGPU(s string) (gpu string, num int, err error) {
	m := gpuPattern.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return "", 0, fmt.Errorf("invalid gpu %q (expected like 2xH200-141GB)", s)
	}
	n, convErr := strconv.Atoi(m[1])
	if convErr != nil || n <= 0 {
		return "", 0, fmt.Errorf("invalid gpu count in %q", s)
	}
	return m[2], n, nil
}

// store holds the managed models and their live activity, and persists definitions to disk.
type store struct {
	mu       sync.RWMutex
	models   map[string]*Model
	lastUsed map[string]int64 // model id -> unix seconds of last request
	runStart map[string]int64 // model id -> unix seconds the current running instance was woken
	// failedGPUs is model id -> GPU config -> unix seconds a deployment on it last failed to start.
	failedGPUs map[string]map[string]int64
	// provisionErr is model id -> why the last provisioning attempt found no GPU, for the dashboard.
	provisionErr map[string]string
	path         string

	idLocksMu sync.Mutex
	idLocks   map[string]*sync.Mutex
}

var modelStore = &store{
	models:       map[string]*Model{},
	lastUsed:     map[string]int64{},
	runStart:     map[string]int64{},
	failedGPUs:   map[string]map[string]int64{},
	provisionErr: map[string]string{},
	idLocks:      map[string]*sync.Mutex{},
}

// lock serializes provision/wake/sleep/remove for one model id so they can't race into an
// orphaned (billing-but-untracked) deployment. Returns the unlock func.
func (s *store) lock(id string) func() {
	s.idLocksMu.Lock()
	l := s.idLocks[id]
	if l == nil {
		l = &sync.Mutex{}
		s.idLocks[id] = l
	}
	s.idLocksMu.Unlock()
	l.Lock()
	return l.Unlock
}

// load reads persisted models from the state file at the given path. Missing file is not an error.
func (s *store) load(path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.path = path
	data, err := os.ReadFile(path)
	if err != nil {
		return // first run or unreadable; start empty
	}
	var persisted []*Model
	if err := json.Unmarshal(data, &persisted); err != nil {
		hostLog("warn", fmt.Sprintf("deepinfra-autoscale: state file %s is corrupt, ignoring: %v", path, err))
		return
	}
	s.models = make(map[string]*Model, len(persisted))
	for _, m := range persisted {
		m.normalize()
		s.models[m.ID] = m
	}
}

// saveLocked writes the current model set to disk atomically. Caller holds s.mu.
func (s *store) saveLocked() error {
	if s.path == "" {
		return nil
	}
	list := make([]*Model, 0, len(s.models))
	for _, m := range s.models {
		list = append(list, m)
	}
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return fmt.Errorf("mkdir state dir: %w", err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write state: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("rename state: %w", err)
	}
	return nil
}

func (s *store) get(id string) (*Model, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m, ok := s.models[id]
	if !ok {
		return nil, false
	}
	cp := *m
	return &cp, true
}

func (s *store) has(id string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.models[id]
	return ok
}

func (s *store) list() []*Model {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Model, 0, len(s.models))
	for _, m := range s.models {
		cp := *m
		out = append(out, &cp)
	}
	return out
}

// add inserts a new model (error if the id already exists) and persists. If persistence fails the
// add is rolled back, so an add that returns nil is durable.
func (s *store) add(m *Model) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.models[m.ID]; exists {
		return fmt.Errorf("model %q already exists", m.ID)
	}
	s.models[m.ID] = m
	if err := s.saveLocked(); err != nil {
		delete(s.models, m.ID)
		return err
	}
	return nil
}

// update replaces an existing model and persists, rolling back if persistence fails.
func (s *store) update(m *Model) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	old, exists := s.models[m.ID]
	if !exists {
		return fmt.Errorf("model %q not found", m.ID)
	}
	s.models[m.ID] = m
	if err := s.saveLocked(); err != nil {
		s.models[m.ID] = old
		return err
	}
	return nil
}

// remove deletes a model and its activity, and persists. Returns the removed model.
func (s *store) remove(id string) (*Model, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.models[id]
	if !ok {
		return nil, false
	}
	delete(s.models, id)
	delete(s.lastUsed, id)
	delete(s.runStart, id)
	delete(s.failedGPUs, id)
	delete(s.provisionErr, id)
	if err := s.saveLocked(); err != nil {
		hostLog("error", fmt.Sprintf("deepinfra-autoscale: persist after remove %s: %v", id, err))
	}
	return m, true
}

// setDeployment records the DeepInfra deploy linkage for a model and the GPU it runs on, and persists.
func (s *store) setDeployment(id, deployID, deepInfraName, gpu string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m, ok := s.models[id]; ok {
		m.DeployID = deployID
		m.DeepInfraName = deepInfraName
		m.GPU = gpu
		if err := s.saveLocked(); err != nil {
			hostLog("error", fmt.Sprintf("deepinfra-autoscale: persist deploy linkage %s: %v", id, err))
		}
	}
}

func (s *store) markUsed(id string, now int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.models[id]; ok {
		s.lastUsed[id] = now
	}
}

func (s *store) markRunning(id string, now int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.models[id]; ok {
		s.runStart[id] = now
	}
}

func (s *store) clearRunning(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.runStart, id)
}

func (s *store) markGPUFailed(id, gpu string, now int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.models[id]; !ok || gpu == "" {
		return
	}
	if s.failedGPUs[id] == nil {
		s.failedGPUs[id] = map[string]int64{}
	}
	s.failedGPUs[id][gpu] = now
}

// recentlyFailed returns the GPUs a model failed to start on within gpuRetryAfter of now.
func (s *store) recentlyFailed(id string, now int64) map[string]bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := map[string]bool{}
	for gpu, at := range s.failedGPUs[id] {
		if now-at < gpuRetryAfter {
			out[gpu] = true
		}
	}
	return out
}

// clearFailedGPUs forgets a model's failed GPU starts, e.g. when its deployment is deleted for a settings change.
func (s *store) clearFailedGPUs(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.failedGPUs, id)
}

func (s *store) setProvisionError(id, msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if msg == "" {
		delete(s.provisionErr, id)
	} else if _, ok := s.models[id]; ok {
		s.provisionErr[id] = msg
	}
}

func (s *store) provisionError(id string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.provisionErr[id]
}

func (s *store) activity(id string) (lastUsed, runStart int64) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lastUsed[id], s.runStart[id]
}

// toModelInfo renders a managed model as a CPA ModelInfo for /v1/models.
func (m *Model) toModelInfo() pluginapi.ModelInfo {
	name := m.DeepInfraName
	if name == "" {
		name = m.ID
	}
	displayName := m.DisplayName
	if displayName == "" {
		displayName = m.ID
	}
	return pluginapi.ModelInfo{
		ID:                         m.ID,
		Object:                     "model",
		Created:                    time.Now().Unix(),
		OwnedBy:                    "deepinfra",
		Type:                       "chat",
		DisplayName:                displayName,
		Name:                       name,
		Description:                fmt.Sprintf("DeepInfra %s on %s", m.HFRepo, strings.Join(m.GPUs, " or ")),
		ContextLength:              m.Context,
		InputTokenLimit:            m.Context,
		OutputTokenLimit:           m.Context,
		SupportedGenerationMethods: []string{"generateContent"},
		SupportedInputModalities:   []string{"text"},
		SupportedOutputModalities:  []string{"text"},
		UserDefined:                true,
	}
}
