package main

import (
	"encoding/json"
	"fmt"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// dispatch serves one ABI call. It never panics.
func dispatch(method string, req []byte) (out []byte) {
	defer func() {
		if v := recover(); v != nil {
			hostLog("warn", fmt.Sprintf("deepinfra-autoscale: recovered from a panic in %s: %v", method, v))
			out = envelope(nil, fmt.Errorf("panic: %v", v))
		}
	}()
	// TODO: model registry, executor, router, management API, and autoscaler handlers.
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		return envelope(registration(), nil)
	}
	return envelope(nil, fmt.Errorf("unknown method %q", method))
}

func registration() map[string]any {
	return map[string]any{
		"schema_version": pluginabi.SchemaVersion,
		"metadata": pluginapi.Metadata{
			Name:             "deepinfra-autoscale",
			Version:          version,
			Author:           "Pandoks",
			GitHubRepository: "https://github.com/Pandoks/cpa-plugin-deepinfra-autoscale",
		},
		// TODO: replace the placeholder with the real capabilities.
		"capabilities": map[string]bool{"management_api": true},
	}
}

func envelope(result any, err error) []byte {
	raw, errMarshal := json.Marshal(result)
	if err == nil {
		err = errMarshal
	}
	env := pluginabi.Envelope{OK: true, Result: raw}
	if err != nil {
		env = pluginabi.Envelope{Error: &pluginabi.Error{Code: "deepinfra_autoscale", Message: err.Error()}}
	}
	out, _ := json.Marshal(env)
	return out
}
