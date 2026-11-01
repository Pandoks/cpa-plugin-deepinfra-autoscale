package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// executorRequest embeds the public ExecutorRequest plus the RPC-only stream/callback ids.
type executorRequest struct {
	pluginapi.ExecutorRequest
	StreamID       string `json:"stream_id,omitempty"`
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// execStreamResponse matches the host's rpcExecutorStreamResponse wire shape.
type execStreamResponse struct {
	Headers map[string][]string `json:"headers,omitempty"`
	Chunks  []any               `json:"chunks,omitempty"`
}

func handleExecutorIdentifier() []byte {
	return envelope(map[string]string{"identifier": "deepinfra"}, nil)
}

// upstreamFor returns the DeepInfra endpoint and extra headers for a client source format.
func upstreamFor(sourceFormat string) (url string, extraHeaders map[string][]string) {
	switch sourceFormat {
	case "claude", "anthropic":
		return deepInfraAPI + "/anthropic/v1/messages", map[string][]string{"anthropic-version": {"2023-06-01"}}
	default: // openai and anything else we declared
		return deepInfraAPI + "/v1/openai/chat/completions", nil
	}
}

// rewriteModel sets the upstream "model" field in a chat payload to the DeepInfra deployment name.
func rewriteModel(payload []byte, deepInfraName string) []byte {
	var body map[string]any
	if err := json.Unmarshal(payload, &body); err != nil {
		return payload // leave as-is; upstream will report the error
	}
	body["model"] = deepInfraName
	out, err := json.Marshal(body)
	if err != nil {
		return payload
	}
	return out
}

// prepareUpstream resolves the model, records activity, ensures it is running, and builds the
// outbound request. A non-nil error-envelope return means "stop; return this to the client"
// (e.g. warming up or unknown model).
func prepareUpstream(r *executorRequest) (url string, headers map[string][]string, body []byte, errResp []byte) {
	id := normalizeModelID(r.Model)
	if id == "" {
		id = normalizeModelID(r.Alias())
	}
	m, ok := modelStore.get(id)
	if !ok {
		return "", nil, nil, errorEnvelopeStatus("unknown_model", fmt.Sprintf("model %q is not managed by deepinfra-autoscale", id), http.StatusNotFound)
	}
	modelStore.markUsed(id, time.Now().Unix())

	status, err := ensureRunning(r.HostCallbackID, m)
	if err != nil {
		return "", nil, nil, errorEnvelopeStatus("deepinfra_error", err.Error(), http.StatusBadGateway)
	}
	if status != "running" {
		return "", nil, nil, errorEnvelopeStatus("warming_up",
			fmt.Sprintf("model %q is warming up (status %s; cold start ~12 min). Retry shortly.", id, status),
			http.StatusServiceUnavailable)
	}

	// Re-read to get the deployment name learned during ensureRunning.
	m, _ = modelStore.get(id)
	name := m.DeepInfraName
	if name == "" {
		name = id
	}

	url, extra := upstreamFor(r.SourceFormat)
	headers = map[string][]string{
		"Authorization": {"Bearer " + apiKey()},
		"Content-Type":  {"application/json"},
	}
	for k, v := range extra {
		headers[k] = v
	}
	body = rewriteModel(r.Payload, name)
	return url, headers, body, nil
}

func handleExecutorExecute(req []byte) []byte {
	var r executorRequest
	if err := json.Unmarshal(req, &r); err != nil {
		return errorEnvelope("bad_request", "decode executor request: "+err.Error())
	}
	url, headers, body, errResp := prepareUpstream(&r)
	if errResp != nil {
		return errResp
	}
	resp, err := httpDo(r.HostCallbackID, "POST", url, headers, body)
	if err != nil {
		return errorEnvelopeStatus("deepinfra_error", err.Error(), http.StatusBadGateway)
	}
	if resp.StatusCode >= 400 {
		return errorEnvelopeStatus("upstream_error", truncate(string(resp.Body), 500), resp.StatusCode)
	}
	return envelope(pluginapi.ExecutorResponse{Payload: resp.Body, Headers: resp.Headers}, nil)
}

func handleExecutorExecuteStream(req []byte) []byte {
	var r executorRequest
	if err := json.Unmarshal(req, &r); err != nil {
		return errorEnvelope("bad_request", "decode executor request: "+err.Error())
	}
	url, headers, body, errResp := prepareUpstream(&r)
	if errResp != nil {
		return errResp
	}
	stream, err := httpDoStream(r.HostCallbackID, "POST", url, headers, body)
	if err != nil {
		return errorEnvelopeStatus("deepinfra_error", err.Error(), http.StatusBadGateway)
	}
	if stream.StatusCode >= 400 {
		// Drain and surface the upstream error.
		msg := drainStreamError(stream.StreamID)
		return errorEnvelopeStatus("upstream_error", truncate(msg, 500), stream.StatusCode)
	}

	downstream := r.StreamID
	hostStream := stream.StreamID
	go pumpStream(hostStream, downstream)

	return envelope(execStreamResponse{Headers: stream.Headers}, nil)
}

// pumpStream forwards upstream SSE chunks to the downstream bridge until the upstream ends.
func pumpStream(hostStream, downstream string) {
	defer func() {
		if v := recover(); v != nil {
			hostLog("warn", fmt.Sprintf("deepinfra-autoscale: stream pump panic: %v", v))
			streamClose(downstream, "stream pump panic")
		}
	}()
	for {
		chunk, err := httpStreamRead(hostStream)
		if err != nil {
			httpStreamClose(hostStream)
			streamClose(downstream, err.Error())
			return
		}
		if len(chunk.Payload) > 0 {
			if emitErr := streamEmit(downstream, chunk.Payload); emitErr != nil {
				httpStreamClose(hostStream)
				streamClose(downstream, "")
				return
			}
		}
		if chunk.Error != "" {
			httpStreamClose(hostStream)
			streamClose(downstream, chunk.Error)
			return
		}
		if chunk.Done {
			httpStreamClose(hostStream)
			streamClose(downstream, "")
			return
		}
	}
}

// drainStreamError reads a failed upstream stream into a single error string.
func drainStreamError(hostStream string) string {
	var buf []byte
	for i := 0; i < 64; i++ {
		chunk, err := httpStreamRead(hostStream)
		if err != nil {
			break
		}
		buf = append(buf, chunk.Payload...)
		if chunk.Done || chunk.Error != "" {
			break
		}
	}
	httpStreamClose(hostStream)
	return string(buf)
}

func handleExecutorCountTokens(req []byte) []byte {
	// DeepInfra's chat endpoints expose no token-count API; let the host fall back.
	return errorEnvelope("unsupported", "count_tokens is not supported by deepinfra-autoscale")
}

// --- helpers ---

func (r *executorRequest) Alias() string {
	if r.Metadata != nil {
		if v, ok := r.Metadata["requested_model"].(string); ok {
			return v
		}
		if v, ok := r.Metadata["alias"].(string); ok {
			return v
		}
	}
	return ""
}

func errorEnvelope(code, message string) []byte {
	env := pluginabi.Envelope{Error: &pluginabi.Error{Code: code, Message: message}}
	out, _ := json.Marshal(env)
	return out
}

func errorEnvelopeStatus(code, message string, status int) []byte {
	env := pluginabi.Envelope{Error: &pluginabi.Error{Code: code, Message: message, HTTPStatus: status}}
	out, _ := json.Marshal(env)
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
