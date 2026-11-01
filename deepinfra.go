package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
)

const deepInfraAPI = "https://api.deepinfra.com"

// diDeployment is the subset of a DeepInfra deployment object we use.
type diDeployment struct {
	DeployID  string `json:"deploy_id"`
	ModelName string `json:"model_name"`
	Status    string `json:"status"`
	CreatedAt string `json:"created_at"`
	Settings  *struct {
		MinInstances int `json:"min_instances"`
		MaxInstances int `json:"max_instances"`
	} `json:"settings"`
}

// diGone lists statuses meaning the deployment is already gone; never adopt one of these.
var diGone = map[string]bool{"deleted": true, "deleting": true}

func diAuthHeaders() map[string][]string {
	return map[string][]string{
		"Authorization": {"Bearer " + apiKey()},
		"Content-Type":  {"application/json"},
	}
}

func diError(status int, body []byte) error {
	msg := strings.TrimSpace(string(body))
	if len(msg) > 300 {
		msg = msg[:300]
	}
	return fmt.Errorf("deepinfra %d: %s", status, msg)
}

// diList returns all non-gone deployments on the account.
func diList(callbackID string) ([]diDeployment, error) {
	if apiKey() == "" {
		return nil, fmt.Errorf("DeepInfra API key not configured")
	}
	resp, err := httpDo(callbackID, "GET", deepInfraAPI+"/deploy/list", diAuthHeaders(), nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, diError(resp.StatusCode, resp.Body)
	}
	var all []diDeployment
	if err := json.Unmarshal(resp.Body, &all); err != nil {
		return nil, fmt.Errorf("decode deploy list: %w", err)
	}
	return all, nil
}

// diFind locates the live deployment for a model, matching by deploy id first, then by the
// "<owner>/<id>" model-name suffix. Dead (deleted/deleting) deployments are ignored.
func diFind(callbackID string, m *Model) (*diDeployment, error) {
	all, err := diList(callbackID)
	if err != nil {
		return nil, err
	}
	// Prefer an exact deploy-id match.
	if m.DeployID != "" {
		for i := range all {
			if all[i].DeployID == m.DeployID && !diGone[all[i].Status] {
				return &all[i], nil
			}
		}
	}
	suffix := "/" + m.ID
	for i := range all {
		if diGone[all[i].Status] {
			continue
		}
		if all[i].ModelName == m.ID || strings.HasSuffix(all[i].ModelName, suffix) {
			return &all[i], nil
		}
	}
	return nil, nil
}

// diCreateBody builds the POST /deploy/llm payload for a model.
func diCreateBody(m *Model) map[string]any {
	gpu, num, _ := parseGPU(m.GPU)
	hf := map[string]any{"repo": m.HFRepo}
	if m.Revision != "" {
		hf["revision"] = m.Revision
	}
	standardArgs := map[string]any{
		"max_context_size": m.Context,
		"kv_cache_dtype":   "fp8",
	}
	if m.Quantization != "" {
		standardArgs["quantization"] = m.Quantization
	}
	extraArgs := []string{}
	if m.ToolParser != "" {
		extraArgs = append(extraArgs, "--enable-auto-tool-choice", "--tool-call-parser", m.ToolParser)
	}
	if m.ReasoningParser != "" {
		extraArgs = append(extraArgs, "--reasoning-parser", m.ReasoningParser)
	}
	if m.EnableThinking {
		extraArgs = append(extraArgs, "--default-chat-template-kwargs", `{"enable_thinking":true}`)
	}
	body := map[string]any{
		"model_name":     m.ID,
		"gpu":            gpu,
		"num_gpus":       num,
		"max_batch_size": m.MaxBatchSize,
		"hf":             hf,
		"standard_args":  standardArgs,
		"settings":       map[string]any{"min_instances": 1, "max_instances": 1},
	}
	if len(extraArgs) > 0 {
		body["extra_args"] = extraArgs
	}
	return body
}

// diCreate creates the deployment for a model (starting at one instance).
func diCreate(callbackID string, m *Model) (*diDeployment, error) {
	body, _ := json.Marshal(diCreateBody(m))
	resp, err := httpDo(callbackID, "POST", deepInfraAPI+"/deploy/llm", diAuthHeaders(), body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// A concurrent create may have won; adopt a live existing one.
		if existing, findErr := diFind(callbackID, m); findErr == nil && existing != nil {
			return existing, nil
		}
		return nil, diError(resp.StatusCode, resp.Body)
	}
	var dep diDeployment
	if err := json.Unmarshal(resp.Body, &dep); err != nil {
		return nil, fmt.Errorf("decode create response: %w", err)
	}
	return &dep, nil
}

// diScale sets min_instances (max stays 1).
func diScale(callbackID, deployID string, minInstances int) error {
	body, _ := json.Marshal(map[string]any{
		"settings": map[string]any{"min_instances": minInstances, "max_instances": 1},
	})
	resp, err := httpDo(callbackID, "PUT", deepInfraAPI+"/deploy/"+deployID, diAuthHeaders(), body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return diError(resp.StatusCode, resp.Body)
	}
	return nil
}

// diDelete removes a deployment entirely.
func diDelete(callbackID, deployID string) error {
	resp, err := httpDo(callbackID, "DELETE", deepInfraAPI+"/deploy/"+deployID, diAuthHeaders(), nil)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return diError(resp.StatusCode, resp.Body)
	}
	return nil
}

// --- GPU catalog (cached) ---

type gpuOption struct {
	Name       string  `json:"name"`
	USDPerHour float64 `json:"usd_per_hour"`
	Available  bool    `json:"available"`
}

var (
	gpuMu        sync.Mutex
	gpuCache     []gpuOption
	gpuCacheTime time.Time
)

const gpuCacheTTL = 5 * time.Minute

// diGPUs returns the DeepInfra GPU catalog with prices, cached for gpuCacheTTL.
func diGPUs(callbackID string) ([]gpuOption, error) {
	gpuMu.Lock()
	if time.Since(gpuCacheTime) < gpuCacheTTL && gpuCache != nil {
		cached := gpuCache
		gpuMu.Unlock()
		return cached, nil
	}
	gpuMu.Unlock()

	resp, err := httpDo(callbackID, "GET", deepInfraAPI+"/deploy/llm/gpu_availability", diAuthHeaders(), nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, diError(resp.StatusCode, resp.Body)
	}
	var parsed struct {
		GPUs []struct {
			GPUConfig  string  `json:"gpu_config"`
			USDPerHour float64 `json:"usd_per_hour"`
			Available  bool    `json:"available"`
		} `json:"gpus"`
	}
	if err := json.Unmarshal(resp.Body, &parsed); err != nil {
		return nil, fmt.Errorf("decode gpu availability: %w", err)
	}
	out := make([]gpuOption, 0, len(parsed.GPUs))
	for _, g := range parsed.GPUs {
		out = append(out, gpuOption{Name: g.GPUConfig, USDPerHour: g.USDPerHour, Available: g.Available})
	}

	gpuMu.Lock()
	gpuCache = out
	gpuCacheTime = time.Now()
	gpuMu.Unlock()
	return out, nil
}

// gpuPrice returns the hourly price for a GPU config string, using the cache (no fetch).
func gpuPrice(name string) float64 {
	gpuMu.Lock()
	defer gpuMu.Unlock()
	for _, g := range gpuCache {
		if g.Name == name {
			return g.USDPerHour
		}
	}
	return 0
}

// dashboardStatus maps a DeepInfra deployment to a dashboard-facing status for a model.
func dashboardStatus(dep *diDeployment) string {
	if dep == nil {
		return "scaled-to-zero"
	}
	if dep.Settings != nil && dep.Settings.MinInstances == 0 {
		return "scaled-to-zero"
	}
	switch dep.Status {
	case "running":
		return "running"
	case "downloading", "deploying", "initializing":
		return dep.Status
	case "failed":
		return "failed"
	default:
		return dep.Status
	}
}
