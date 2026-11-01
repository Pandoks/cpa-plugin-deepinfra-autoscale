package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct { void* ptr; size_t len; } cliproxy_buffer;
typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);
typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	cliproxy_host_call_fn call;
	cliproxy_host_free_fn free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);
typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);

static const cliproxy_host_api* host_api;

static void set_host_api(const cliproxy_host_api* host) { host_api = host; }

// call_host invokes a host method and returns its response buffer in *out (caller frees via
// free_host_buffer). Returns non-zero when the host API is unavailable or the call fails.
static int call_host(const char* method, const uint8_t* req, size_t len, cliproxy_buffer* out) {
	if (out != NULL) { out->ptr = NULL; out->len = 0; }
	if (host_api == NULL || host_api->call == NULL) return 1;
	return host_api->call(host_api->host_ctx, method, req, len, out);
}

static void free_host_buffer(void* ptr, size_t len) {
	if (host_api != NULL && host_api->free_buffer != NULL && ptr != NULL) host_api->free_buffer(ptr, len);
}
*/
import "C"

import (
	"encoding/json"
	"fmt"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

var version = "dev" // set by `make build`

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, api *C.cliproxy_plugin_api) C.int {
	if api == nil {
		return 1
	}
	C.set_host_api(host)
	api.abi_version = C.uint32_t(pluginabi.ABIVersion)
	api.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	api.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	api.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, req *C.uint8_t, n C.size_t, resp *C.cliproxy_buffer) C.int {
	if method == nil || resp == nil {
		return 1
	}
	out := dispatch(C.GoString(method), C.GoBytes(unsafe.Pointer(req), C.int(n)))
	resp.ptr, resp.len = C.CBytes(out), C.size_t(len(out))
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, _ C.size_t) { C.free(ptr) }

// cliproxyPluginShutdown is the C unload hook. It must be a no-op: it runs during the host's
// dlclose while the Go runtime is being torn down, so doing any Go-runtime work here (locks,
// channel ops, goroutine scheduling) risks a SIGILL. The ticker is stopped earlier, in the
// plugin.quiesce RPC handler, which runs while the host is fully alive. On a process exit the
// ticker goroutine simply dies with the process.
//
//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {}

// callHost invokes a host callback method with a JSON request and returns the decoded envelope
// result bytes. The C host-call shim is only visible to this file's cgo preamble, so every host
// call funnels through here.
func callHost(method string, req []byte) ([]byte, error) {
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))

	var reqPtr *C.uint8_t
	if len(req) > 0 {
		cReq := C.CBytes(req)
		defer C.free(cReq)
		reqPtr = (*C.uint8_t)(cReq)
	}

	var out C.cliproxy_buffer
	rc := C.call_host(cMethod, reqPtr, C.size_t(len(req)), &out)
	var raw []byte
	if out.ptr != nil {
		raw = C.GoBytes(out.ptr, C.int(out.len))
		C.free_host_buffer(out.ptr, out.len)
	}
	if rc != 0 {
		return nil, fmt.Errorf("host call %s failed (rc=%d)", method, int(rc))
	}

	var env pluginabi.Envelope
	if len(raw) == 0 {
		return nil, nil
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("host call %s: decode envelope: %w", method, err)
	}
	if env.Error != nil {
		return nil, fmt.Errorf("host call %s: %s: %s", method, env.Error.Code, env.Error.Message)
	}
	return env.Result, nil
}

// hostLog emits a structured log line through the host.
func hostLog(level, msg string) {
	raw, _ := json.Marshal(map[string]string{"level": level, "message": msg})
	_, _ = callHost(pluginabi.MethodHostLog, raw)
}
