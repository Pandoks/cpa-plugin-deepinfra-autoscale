package main

import (
	"encoding/json"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// handleUsage records model activity from a completed request. The executor already stamps
// activity first-hand; this is a secondary signal that also covers terminal outcomes.
func handleUsage(req []byte) []byte {
	var rec pluginapi.UsageRecord
	if err := json.Unmarshal(req, &rec); err != nil {
		return envelope(struct{}{}, nil)
	}
	id := normalizeModelID(rec.Alias)
	if id == "" || !modelStore.has(id) {
		id = normalizeModelID(rec.Model)
	}
	if id != "" && modelStore.has(id) {
		when := time.Now().Unix()
		if !rec.RequestedAt.IsZero() {
			when = rec.RequestedAt.Unix()
		}
		modelStore.markUsed(id, when)
	}
	return envelope(struct{}{}, nil)
}
