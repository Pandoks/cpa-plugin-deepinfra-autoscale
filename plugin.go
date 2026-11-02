package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// dispatch serves one ABI call. It never panics.
func dispatch(method string, req []byte) (out []byte) {
	defer func() {
		if v := recover(); v != nil {
			hostLog("warn", fmt.Sprintf("deepinfra-autoscale: recovered from a panic in %s: %v", method, v))
			out = errorEnvelope("panic", fmt.Sprintf("%v", v))
		}
	}()

	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		return handleLifecycle(req)

	case pluginabi.MethodModelStatic:
		return handleModelStatic()
	case pluginabi.MethodModelForAuth:
		return envelope(pluginapi.ModelResponse{Provider: "deepinfra"}, nil)

	case pluginabi.MethodModelRoute:
		return handleModelRoute(req)

	case pluginabi.MethodExecutorIdentifier:
		return handleExecutorIdentifier()
	case pluginabi.MethodExecutorExecute:
		return handleExecutorExecute(req)
	case pluginabi.MethodExecutorExecuteStream:
		return handleExecutorExecuteStream(req)
	case pluginabi.MethodExecutorCountTokens:
		return handleExecutorCountTokens(req)

	case pluginabi.MethodManagementRegister:
		return handleManagementRegister()
	case pluginabi.MethodManagementHandle:
		return handleManagementHandle(req)

	case pluginabi.MethodUsageHandle:
		return handleUsage(req)

	case pluginabi.MethodPluginQuiesce, pluginabi.MethodPluginShutdown:
		// Stop the ticker cleanly while the host is still alive, so it can't fire after the plugin
		// is unloaded/replaced. Running deployments are intentionally left as-is: a config reload
		// quiesces too, and the replacement instance re-adopts them (bounded by the idle window);
		// a real CPA shutdown relies on that re-adoption on restart plus a DeepInfra spending limit.
		stopAutoscaler()
		return envelope(struct{}{}, nil)
	}
	return errorEnvelope("unknown_method", fmt.Sprintf("unknown method %q", method))
}

// handleLifecycle processes register/reconfigure: parse delivered config, (re)load the model
// state file, and start the autoscaler. Both methods return the same registration.
func handleLifecycle(req []byte) []byte {
	var life struct {
		ConfigYAML    []byte `json:"config_yaml"`
		SchemaVersion uint32 `json:"schema_version"`
	}
	_ = json.Unmarshal(req, &life)
	cfg := applyConfigYAML(life.ConfigYAML)
	modelStore.load(cfg.StateFile)
	loadAPIKey(cfg.StateFile)
	startAutoscaler()
	return envelope(registration(), nil)
}

// registration describes the plugin's identity and capabilities to the host.
func registration() map[string]any {
	return map[string]any{
		"schema_version": pluginabi.SchemaVersion,
		"metadata": pluginapi.Metadata{
			Name:             "deepinfra-autoscale",
			Version:          strings.TrimPrefix(version, "v"), // the panel prefixes its own "v"
			Author:           "Pandoks",
			GitHubRepository: "https://github.com/Pandoks/cpa-plugin-deepinfra-autoscale",
		},
		"capabilities": capabilities{
			ModelProvider:         true,
			ModelRouter:           true,
			Executor:              true,
			ExecutorModelScope:    "both",
			ExecutorInputFormats:  []string{"openai", "claude"},
			ExecutorOutputFormats: []string{"openai", "claude"},
			ManagementAPI:         true,
			UsagePlugin:           true,
		},
	}
}

// capabilities mirrors the host's rpcCapabilities JSON keys (only the ones we enable).
type capabilities struct {
	ModelProvider         bool     `json:"model_provider"`
	ModelRouter           bool     `json:"model_router"`
	Executor              bool     `json:"executor"`
	ExecutorModelScope    string   `json:"executor_model_scope"`
	ExecutorInputFormats  []string `json:"executor_input_formats,omitempty"`
	ExecutorOutputFormats []string `json:"executor_output_formats,omitempty"`
	ManagementAPI         bool     `json:"management_api"`
	UsagePlugin           bool     `json:"usage_plugin"`
}

// envelope wraps a successful result in the ABI envelope.
func envelope(result any, err error) []byte {
	raw, errMarshal := json.Marshal(result)
	if err == nil {
		err = errMarshal
	}
	if err != nil {
		return errorEnvelope("deepinfra_autoscale", err.Error())
	}
	out, _ := json.Marshal(pluginabi.Envelope{OK: true, Result: raw})
	return out
}

// handleModelStatic returns the current managed model set for /v1/models.
func handleModelStatic() []byte {
	models := modelStore.list()
	infos := make([]pluginapi.ModelInfo, 0, len(models))
	for _, m := range models {
		infos = append(infos, m.toModelInfo())
	}
	return envelope(pluginapi.ModelResponse{Provider: "deepinfra", Models: infos}, nil)
}
