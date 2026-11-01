package main

import (
	"encoding/json"
	"regexp"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// bracketSuffix matches a trailing capability tag like "[1m]" that clients (e.g. Claude Code's 1M
// context variant) append to the model id. We route and serve the base id.
var bracketSuffix = regexp.MustCompile(`\[[^\]]*\]$`)

func normalizeModelID(id string) string {
	return bracketSuffix.ReplaceAllString(id, "")
}

// handleModelRoute claims requests for our managed models and routes them to our own executor.
func handleModelRoute(req []byte) []byte {
	var route pluginapi.ModelRouteRequest
	if err := json.Unmarshal(req, &route); err != nil {
		return envelope(pluginapi.ModelRouteResponse{Handled: false}, nil)
	}
	id := normalizeModelID(route.RequestedModel)
	if id == "" || !modelStore.has(id) {
		return envelope(pluginapi.ModelRouteResponse{Handled: false}, nil)
	}
	return envelope(pluginapi.ModelRouteResponse{
		Handled:    true,
		TargetKind: pluginapi.ModelRouteTargetKind("self"),
		Reason:     "deepinfra-autoscale managed model",
	}, nil)
}
