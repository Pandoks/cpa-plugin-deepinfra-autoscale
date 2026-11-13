package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"math"
	"net/http"
	"strings"
	"sync"
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

// rewriteModel sets the upstream "model" field in a chat payload to the DeepInfra deployment name,
// preserving all other fields and their numeric precision (UseNumber avoids turning large integer
// fields like seed into lossy float64). For a model with thinking off it also turns thinking off in
// the request, since DeepInfra takes thinking from each request rather than the deployment default.
func rewriteModel(payload []byte, deepInfraName string, thinkingOff, anthropic bool) []byte {
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.UseNumber()
	var body map[string]any
	if err := dec.Decode(&body); err != nil {
		return payload // not a JSON object; leave as-is, upstream will report the error
	}
	body["model"] = deepInfraName
	if thinkingOff && anthropic {
		body["thinking"] = map[string]any{"type": "disabled"}
	} else if thinkingOff {
		kwargs, _ := body["chat_template_kwargs"].(map[string]any)
		if kwargs == nil {
			kwargs = map[string]any{}
		}
		kwargs["enable_thinking"] = false
		body["chat_template_kwargs"] = kwargs
	}
	out, err := json.Marshal(body)
	if err != nil {
		return payload
	}
	return out
}

func isAnthropic(r *executorRequest) bool {
	return r.SourceFormat == "claude" || r.SourceFormat == "anthropic"
}

// prepareUpstream resolves the model, records activity, ensures it is running, and builds the
// outbound request, returning the model's loop guard setting with it. A non-nil error-envelope
// return means "stop; return this to the client" (e.g. warming up or unknown model).
func prepareUpstream(r *executorRequest) (url string, headers map[string][]string, body []byte, loopGuard string, errResp []byte) {
	id := normalizeModelID(r.Model)
	if id == "" {
		id = normalizeModelID(r.Alias())
	}
	if id == "" || !modelStore.has(id) {
		return "", nil, nil, "", errorEnvelopeStatus("unknown_model", fmt.Sprintf("model %q is not managed by deepinfra-autoscale", id), http.StatusNotFound)
	}
	if m, ok := modelStore.get(id); ok && isAnthropic(r) && m.MaxInputTokens > 0 {
		n := estimateTokens(r.Payload)
		if limit := inputLimit(claudeMessages(r.Payload), n, m.MaxInputTokens); n > limit {
			// Claude Code compacts the conversation and retries on exactly this message.
			return "", nil, nil, "", errorEnvelopeStatus("invalid_request_error",
				fmt.Sprintf("prompt is too long: %d tokens > %d maximum", n, limit), http.StatusBadRequest)
		}
	}
	modelStore.markUsed(id, time.Now().Unix())

	status, err := ensureRunning(r.HostCallbackID, id)
	if errors.Is(err, errNoGPU) {
		return "", nil, nil, "", errorEnvelopeStatus("no_gpu", err.Error(), http.StatusServiceUnavailable)
	}
	if err != nil {
		return "", nil, nil, "", errorEnvelopeStatus("deepinfra_error", err.Error(), http.StatusBadGateway)
	}
	if status != "running" {
		return "", nil, nil, "", errorEnvelopeStatus("warming_up",
			fmt.Sprintf("model %q is warming up (status %s; cold start ~12 min). Retry shortly.", id, status),
			http.StatusServiceUnavailable)
	}

	// Re-read to get the deployment name learned during ensureRunning.
	name, thinkingOff := id, false
	if m, ok := modelStore.get(id); ok {
		if m.DeepInfraName != "" {
			name = m.DeepInfraName
		}
		thinkingOff = !m.EnableThinking
		loopGuard = m.LoopGuard
	}

	url, extra := upstreamFor(r.SourceFormat)
	headers = map[string][]string{
		"Authorization": {"Bearer " + apiKey()},
		"Content-Type":  {"application/json"},
	}
	for k, v := range extra {
		headers[k] = v
	}
	body = rewriteModel(r.Payload, name, thinkingOff, isAnthropic(r))
	return url, headers, body, loopGuard, nil
}

// bytesPerToken turns a request's size into a token estimate. Claude Code requests to RED-SNOW took
// 3.65 to 7.55 bytes per GLM token, and 3.15 at the least per input token billed, so 3 overestimates.
const bytesPerToken = 3

func estimateTokens(payload []byte) int64 {
	return int64(len(payload)) / bytesPerToken
}

// Claude Code's prompt asking for a summary when it compacts a conversation, and how the first
// message of a conversation it has compacted starts.
const (
	compactPrompt  = "Your task is to create a detailed summary of the conversation so far"
	compactedStart = "This session is being continued from a previous conversation"
)

// compactedFloors holds, per compacted conversation (keyed by its summary), the estimate of the first
// request seen with it: the request Claude Code sends right after compacting.
var compactedFloors = struct {
	sync.Mutex
	m map[uint64]int64
}{m: map[uint64]int64{}}

type claudeMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

func claudeMessages(payload []byte) []claudeMessage {
	var body struct {
		Messages []claudeMessage `json:"messages"`
	}
	_ = json.Unmarshal(payload, &body)
	return body.Messages
}

// texts returns a message's text blocks.
func (m claudeMessage) texts() []string {
	var s string
	if json.Unmarshal(m.Content, &s) == nil {
		return []string{s}
	}
	var blocks []struct{ Type, Text string }
	_ = json.Unmarshal(m.Content, &blocks)
	var out []string
	for _, b := range blocks {
		if b.Type == "text" {
			out = append(out, b.Text)
		}
	}
	return out
}

// isCompaction reports whether a request is Claude Code compacting the conversation. Its prompt is
// in the last user message; a system message may follow it.
func isCompaction(msgs []claudeMessage) bool {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			for _, t := range msgs[i].texts() {
				if strings.Contains(t, compactPrompt) {
					return true
				}
			}
			return false
		}
	}
	return false
}

// inputLimit returns the input cap for a request of n estimated tokens. Claude Code's compaction
// request is never capped, or it could never compact. After a compaction Claude Code cannot go below
// the summary, the last turn it keeps and the files it re-reads (62-71k estimated tokens in testing),
// so a compacted conversation may grow by a quarter of the cap past its first request; otherwise
// every turn after a compaction would be rejected.
func inputLimit(msgs []claudeMessage, n, limit int64) int64 {
	if isCompaction(msgs) {
		return math.MaxInt64
	}
	if len(msgs) == 0 {
		return limit
	}
	for _, t := range msgs[0].texts() {
		if strings.HasPrefix(t, compactedStart) {
			h := fnv.New64a()
			h.Write([]byte(t))
			key := h.Sum64()
			compactedFloors.Lock()
			defer compactedFloors.Unlock()
			if len(compactedFloors.m) >= 4096 {
				clear(compactedFloors.m)
			}
			floor, ok := compactedFloors.m[key]
			if !ok {
				floor, compactedFloors.m[key] = n, n
			}
			return max(limit, floor+limit/4)
		}
	}
	return limit
}

func handleExecutorExecute(req []byte) []byte {
	var r executorRequest
	if err := json.Unmarshal(req, &r); err != nil {
		return errorEnvelope("bad_request", "decode executor request: "+err.Error())
	}
	url, headers, body, _, errResp := prepareUpstream(&r)
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
	url, headers, body, loopGuard, errResp := prepareUpstream(&r)
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
	if loopGuard == loopGuardOn || loopGuard == loopGuardObserve {
		var decoded map[string]any
		_ = json.Unmarshal(body, &decoded)
		g := &guard{id: guardSeq.Add(1), r: &r, url: url, headers: headers, body: decoded, downstream: downstream, mode: loopGuard, anthropic: isAnthropic(&r)}
		g.compaction = g.anthropic && isCompaction(claudeMessages(body))
		go g.pump(hostStream, 0)
	} else {
		go pumpStream(hostStream, downstream)
	}

	return envelope(execStreamResponse{Headers: stream.Headers}, nil)
}

// pumpStream forwards upstream SSE chunks to the downstream bridge until the upstream ends.
func pumpStream(hostStream, downstream string) {
	defer func() {
		if v := recover(); v != nil {
			hostLog("warn", fmt.Sprintf("deepinfra-autoscale: stream pump panic: %v", v))
			httpStreamClose(hostStream)
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
