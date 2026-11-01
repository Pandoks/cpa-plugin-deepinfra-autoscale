package main

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Wire shapes for host callbacks. These mirror the host's internal rpc structs
// (internal/pluginhost/host_callbacks.go, stream_bridge.go) exactly by JSON tag.

type hostHTTPRequest struct {
	HostCallbackID string                     `json:"host_callback_id,omitempty"`
	OperationID    string                     `json:"operation_id,omitempty"`
	Method         string                     `json:"method,omitempty"`
	URL            string                     `json:"url,omitempty"`
	Headers        map[string][]string        `json:"headers,omitempty"`
	Body           []byte                     `json:"body,omitempty"`
	WireProfile    *pluginapi.HTTPWireProfile `json:"wire_profile,omitempty"`
}

type hostHTTPStreamResponse struct {
	StatusCode int                 `json:"status_code"`
	Headers    map[string][]string `json:"headers,omitempty"`
	StreamID   string              `json:"stream_id,omitempty"`
}

type hostHTTPStreamReadRequest struct {
	StreamID string `json:"stream_id"`
}

type hostHTTPStreamReadResponse struct {
	Payload []byte `json:"payload,omitempty"`
	Error   string `json:"error,omitempty"`
	Done    bool   `json:"done,omitempty"`
}

type hostStreamIDRequest struct {
	StreamID string `json:"stream_id"`
	Error    string `json:"error,omitempty"`
}

type hostStreamEmitRequest struct {
	StreamID string `json:"stream_id"`
	Payload  []byte `json:"payload,omitempty"`
	Error    string `json:"error,omitempty"`
}

// httpDo performs a non-streaming outbound HTTP request through the host (so CPA's request-log
// and proxy settings apply). callbackID scopes the call to the live inbound request; it may be "".
func httpDo(callbackID, method, url string, headers map[string][]string, body []byte) (*pluginapi.HTTPResponse, error) {
	reqBytes, _ := json.Marshal(hostHTTPRequest{
		HostCallbackID: callbackID,
		Method:         method,
		URL:            url,
		Headers:        headers,
		Body:           body,
	})
	raw, err := callHost(pluginabi.MethodHostHTTPDo, reqBytes)
	if err != nil {
		return nil, err
	}
	var resp pluginapi.HTTPResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("decode host http response: %w", err)
	}
	return &resp, nil
}

// httpDoStream starts a streaming outbound HTTP request through the host and returns the host
// stream id to read chunks from.
func httpDoStream(callbackID, method, url string, headers map[string][]string, body []byte) (*hostHTTPStreamResponse, error) {
	reqBytes, _ := json.Marshal(hostHTTPRequest{
		HostCallbackID: callbackID,
		Method:         method,
		URL:            url,
		Headers:        headers,
		Body:           body,
	})
	raw, err := callHost(pluginabi.MethodHostHTTPDoStream, reqBytes)
	if err != nil {
		return nil, err
	}
	var resp hostHTTPStreamResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("decode host http stream response: %w", err)
	}
	return &resp, nil
}

func httpStreamRead(streamID string) (*hostHTTPStreamReadResponse, error) {
	reqBytes, _ := json.Marshal(hostHTTPStreamReadRequest{StreamID: streamID})
	raw, err := callHost(pluginabi.MethodHostHTTPStreamRead, reqBytes)
	if err != nil {
		return nil, err
	}
	var resp hostHTTPStreamReadResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("decode host http stream read: %w", err)
	}
	return &resp, nil
}

func httpStreamClose(streamID string) {
	reqBytes, _ := json.Marshal(hostHTTPStreamCloseRequest{StreamID: streamID})
	_, _ = callHost(pluginabi.MethodHostHTTPStreamClose, reqBytes)
}

type hostHTTPStreamCloseRequest struct {
	StreamID string `json:"stream_id"`
}

// streamEmit pushes one downstream SSE chunk to the inbound request's stream bridge.
func streamEmit(streamID string, payload []byte) error {
	reqBytes, _ := json.Marshal(hostStreamEmitRequest{StreamID: streamID, Payload: payload})
	_, err := callHost(pluginabi.MethodHostStreamEmit, reqBytes)
	return err
}

// streamClose closes the inbound request's stream bridge, optionally with a terminal error.
func streamClose(streamID, errMsg string) {
	reqBytes, _ := json.Marshal(hostStreamIDRequest{StreamID: streamID, Error: errMsg})
	_, _ = callHost(pluginabi.MethodHostStreamClose, reqBytes)
}

// headerValue returns the first value for a canonical header key from an http.Header-shaped map.
func headerValue(h map[string][]string, key string) string {
	if h == nil {
		return ""
	}
	if v, ok := h[http.CanonicalHeaderKey(key)]; ok && len(v) > 0 {
		return v[0]
	}
	if v, ok := h[key]; ok && len(v) > 0 {
		return v[0]
	}
	return ""
}
