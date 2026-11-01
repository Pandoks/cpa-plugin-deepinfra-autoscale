package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Model is a user-defined DeepInfra-hosted model, managed from the dashboard and persisted to the
// state file so it survives restarts.
type Model struct {
	ID              string `json:"id"`
	HFRepo          string `json:"hf_repo"`
	Revision        string `json:"revision,omitempty"`
	GPU             string `json:"gpu"` // full config, e.g. "2xH200-141GB"
	NumGPUs         int    `json:"num_gpus"`
	Context         int64  `json:"context"`
	Quantization    string `json:"quantization,omitempty"`
	ToolParser      string `json:"tool_parser,omitempty"`
	ReasoningParser string `json:"reasoning_parser,omitempty"`
	EnableThinking  bool   `json:"enable_thinking,omitempty"`
	MaxBatchSize    int    `json:"max_batch_size,omitempty"`

	// Deployment linkage, learned at create/reconcile time and persisted.
	DeployID      string `json:"deploy_id,omitempty"`
	DeepInfraName string `json:"deepinfra_name,omitempty"`
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
	if _, _, err := parseGPU(m.GPU); err != nil {
		return err
	}
	if m.Context <= 0 {
		return fmt.Errorf("context must be positive")
	}
	return nil
}

// normalize fills derived/default fields.
func (m *Model) normalize() {
	if _, n, err := parseGPU(m.GPU); err == nil {
		m.NumGPUs = n
	}
	if m.MaxBatchSize <= 0 {
		m.MaxBatchSize = defaultMaxBatchSize
	}
}

var gpuPattern = regexp.MustCompile(`^(\d+)x(.+)$`)

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
	path     string
}

var modelStore = &store{
	models:   map[string]*Model{},
	lastUsed: map[string]int64{},
	runStart: map[string]int64{},
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
func (s *store) saveLocked() {
	if s.path == "" {
		return
	}
	list := make([]*Model, 0, len(s.models))
	for _, m := range s.models {
		list = append(list, m)
	}
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		hostLog("error", fmt.Sprintf("deepinfra-autoscale: marshal state: %v", err))
		return
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		hostLog("error", fmt.Sprintf("deepinfra-autoscale: mkdir state dir: %v", err))
		return
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		hostLog("error", fmt.Sprintf("deepinfra-autoscale: write state: %v", err))
		return
	}
	if err := os.Rename(tmp, s.path); err != nil {
		hostLog("error", fmt.Sprintf("deepinfra-autoscale: rename state: %v", err))
	}
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

// add inserts a new model (error if the id already exists) and persists.
func (s *store) add(m *Model) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.models[m.ID]; exists {
		return fmt.Errorf("model %q already exists", m.ID)
	}
	s.models[m.ID] = m
	s.saveLocked()
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
	s.saveLocked()
	return m, true
}

// setDeployment records the DeepInfra deploy linkage for a model and persists.
func (s *store) setDeployment(id, deployID, deepInfraName string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m, ok := s.models[id]; ok {
		m.DeployID = deployID
		m.DeepInfraName = deepInfraName
		s.saveLocked()
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
	s.runStart[id] = now
}

func (s *store) clearRunning(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.runStart, id)
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
	return pluginapi.ModelInfo{
		ID:                         m.ID,
		Object:                     "model",
		Created:                    time.Now().Unix(),
		OwnedBy:                    "deepinfra",
		Type:                       "chat",
		DisplayName:                m.ID,
		Name:                       name,
		Description:                fmt.Sprintf("DeepInfra %s on %s", m.HFRepo, m.GPU),
		ContextLength:              m.Context,
		InputTokenLimit:            m.Context,
		OutputTokenLimit:           m.Context,
		SupportedGenerationMethods: []string{"generateContent"},
		SupportedInputModalities:   []string{"text"},
		SupportedOutputModalities:  []string{"text"},
		UserDefined:                true,
	}
}
