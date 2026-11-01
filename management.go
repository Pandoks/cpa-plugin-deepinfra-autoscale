package main

import (
	_ "embed"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

//go:embed dashboard.html
var dashboardHTML []byte

const pluginID = "deepinfra-autoscale"

// handleManagementRegister declares the JSON API routes (authenticated, under /v0/management/)
// and the HTML dashboard resource (unauthenticated GET, under /v0/resource/plugins/<id>/).
func handleManagementRegister() []byte {
	resp := rpcManagementRegistration{
		Routes: []pluginapi.ManagementRoute{
			{Method: "GET", Path: "/" + pluginID + "/models", Description: "List managed models"},
			{Method: "POST", Path: "/" + pluginID + "/models", Description: "Add a managed model"},
			{Method: "DELETE", Path: "/" + pluginID + "/models", Description: "Remove a managed model"},
			{Method: "POST", Path: "/" + pluginID + "/models/wake", Description: "Start a model's GPUs"},
			{Method: "POST", Path: "/" + pluginID + "/models/sleep", Description: "Scale a model to zero"},
		},
		Resources: []pluginapi.ResourceRoute{
			{Path: "/dashboard", Menu: "DeepInfra Autoscale", Description: "Model dashboard"},
		},
	}
	return envelope(resp, nil)
}

type rpcManagementRegistration struct {
	Routes    []pluginapi.ManagementRoute `json:"routes,omitempty"`
	Resources []pluginapi.ResourceRoute   `json:"resources,omitempty"`
}

// managementRequest mirrors pluginapi.ManagementRequest plus the host callback id.
type managementRequest struct {
	Method         string              `json:"Method"`
	Path           string              `json:"Path"`
	Headers        map[string][]string `json:"Headers"`
	Query          url.Values          `json:"Query"`
	Body           []byte              `json:"Body"`
	HostCallbackID string              `json:"host_callback_id,omitempty"`
}

func handleManagementHandle(req []byte) []byte {
	var r managementRequest
	if err := json.Unmarshal(req, &r); err != nil {
		return envelope(jsonResponse(http.StatusBadRequest, map[string]string{"error": "decode request: " + err.Error()}), nil)
	}
	cb := r.HostCallbackID
	path := r.Path

	switch {
	case strings.HasSuffix(path, "/dashboard"):
		return envelope(htmlResponse(http.StatusOK, dashboardHTML), nil)

	case strings.HasSuffix(path, "/models/wake") && r.Method == "POST":
		return envelope(actionResponse(cb, r.Query.Get("id"), wakeModel), nil)

	case strings.HasSuffix(path, "/models/sleep") && r.Method == "POST":
		return envelope(actionResponse(cb, r.Query.Get("id"), sleepModel), nil)

	case strings.HasSuffix(path, "/models") && r.Method == "GET":
		return envelope(listModelsResponse(cb), nil)

	case strings.HasSuffix(path, "/models") && r.Method == "POST":
		return envelope(addModelResponse(r.Body), nil)

	case strings.HasSuffix(path, "/models") && r.Method == "DELETE":
		return envelope(removeModelResponse(cb, r.Query.Get("id")), nil)
	}
	return envelope(jsonResponse(http.StatusNotFound, map[string]string{"error": "not found"}), nil)
}

// --- handlers ---

type modelView struct {
	ID              string  `json:"id"`
	HFRepo          string  `json:"hf_repo"`
	Revision        string  `json:"revision,omitempty"`
	GPU             string  `json:"gpu"`
	NumGPUs         int     `json:"num_gpus"`
	Context         int64   `json:"context"`
	Quantization    string  `json:"quantization,omitempty"`
	ToolParser      string  `json:"tool_parser,omitempty"`
	ReasoningParser string  `json:"reasoning_parser,omitempty"`
	EnableThinking  bool    `json:"enable_thinking"`
	Status          string  `json:"status"`
	MinInstances    int     `json:"min_instances"`
	DeployID        string  `json:"deploy_id,omitempty"`
	LastUsedUnix    int64   `json:"last_used_unix"`
	USDPerHour      float64 `json:"usd_per_hour"`
}

func listModelsResponse(cb string) pluginapi.ManagementResponse {
	models := modelStore.list()
	gpus, _ := diGPUs(cb) // best-effort; prices/catalog

	// Index live deployments (best-effort).
	depByID := map[string]*diDeployment{}
	if all, err := diList(cb); err == nil {
		for i := range all {
			if diGone[all[i].Status] {
				continue
			}
			dep := &all[i]
			for _, m := range models {
				if dep.DeployID == m.DeployID || dep.ModelName == m.ID || strings.HasSuffix(dep.ModelName, "/"+m.ID) {
					depByID[m.ID] = dep
				}
			}
		}
	}

	views := make([]modelView, 0, len(models))
	for _, m := range models {
		lastUsed, _ := modelStore.activity(m.ID)
		dep := depByID[m.ID]
		v := modelView{
			ID: m.ID, HFRepo: m.HFRepo, Revision: m.Revision, GPU: m.GPU, NumGPUs: m.NumGPUs,
			Context: m.Context, Quantization: m.Quantization, ToolParser: m.ToolParser,
			ReasoningParser: m.ReasoningParser, EnableThinking: m.EnableThinking,
			DeployID: m.DeployID, LastUsedUnix: lastUsed, USDPerHour: gpuPrice(m.GPU),
			Status: "scaled-to-zero", MinInstances: 0,
		}
		if dep != nil {
			v.Status = dashboardStatus(dep)
			v.DeployID = dep.DeployID
			if dep.Settings != nil {
				v.MinInstances = dep.Settings.MinInstances
			}
		}
		views = append(views, v)
	}

	cfg := getConfig()
	return jsonResponse(http.StatusOK, map[string]any{
		"models":       views,
		"gpus":         gpus,
		"idle_minutes": cfg.IdleMinutes,
	})
}

func addModelResponse(body []byte) pluginapi.ManagementResponse {
	var m Model
	if err := json.Unmarshal(body, &m); err != nil {
		return jsonResponse(http.StatusBadRequest, map[string]string{"error": "decode model: " + err.Error()})
	}
	m.normalize()
	if err := m.validate(); err != nil {
		return jsonResponse(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}
	if modelStore.has(m.ID) {
		return jsonResponse(http.StatusConflict, map[string]string{"error": "model " + m.ID + " already exists"})
	}
	// Lazy provisioning: registering a model does not create GPUs. It becomes routable immediately
	// and provisions on first request (or an explicit Wake), so adding costs nothing.
	if err := modelStore.add(&m); err != nil {
		return jsonResponse(http.StatusConflict, map[string]string{"error": err.Error()})
	}
	return jsonResponse(http.StatusOK, map[string]any{"ok": true, "model": m})
}

func removeModelResponse(cb, id string) pluginapi.ManagementResponse {
	if id == "" {
		return jsonResponse(http.StatusBadRequest, map[string]string{"error": "missing id"})
	}
	m, ok := modelStore.get(id)
	if !ok {
		return jsonResponse(http.StatusNotFound, map[string]string{"error": "model " + id + " not found"})
	}
	// Tear down the deployment first so removal also stops billing; ignore "already gone".
	if err := deleteDeployment(cb, m); err != nil {
		return jsonResponse(http.StatusBadGateway, map[string]string{"error": "delete deployment: " + err.Error()})
	}
	modelStore.remove(id)
	return jsonResponse(http.StatusOK, map[string]any{"ok": true})
}

func actionResponse(cb, id string, fn func(string, string) error) pluginapi.ManagementResponse {
	if id == "" {
		return jsonResponse(http.StatusBadRequest, map[string]string{"error": "missing id"})
	}
	if err := fn(cb, id); err != nil {
		return jsonResponse(http.StatusBadGateway, map[string]string{"error": err.Error()})
	}
	return jsonResponse(http.StatusOK, map[string]any{"ok": true})
}

// --- response builders ---

func jsonResponse(status int, payload any) pluginapi.ManagementResponse {
	body, _ := json.Marshal(payload)
	return pluginapi.ManagementResponse{
		StatusCode: status,
		Headers:    http.Header{"Content-Type": {"application/json"}},
		Body:       body,
	}
}

func htmlResponse(status int, body []byte) pluginapi.ManagementResponse {
	return pluginapi.ManagementResponse{
		StatusCode: status,
		Headers:    http.Header{"Content-Type": {"text/html; charset=utf-8"}},
		Body:       body,
	}
}
