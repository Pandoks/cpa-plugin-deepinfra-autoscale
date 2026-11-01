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

static void call_host(const char* method, const uint8_t* req, size_t len) {
	if (host_api == NULL || host_api->call == NULL) return;
	cliproxy_buffer resp = {0, 0};
	host_api->call(host_api->host_ctx, method, req, len, &resp);
	if (resp.ptr != NULL && host_api->free_buffer != NULL) host_api->free_buffer(resp.ptr, resp.len);
}
*/
import "C"

import (
	"encoding/json"
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

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {}

// hostLog lives here because the C host-call shim is only visible to this file's cgo preamble.
func hostLog(level, msg string) {
	raw, _ := json.Marshal(map[string]string{"level": level, "message": msg})
	method := C.CString(pluginabi.MethodHostLog)
	defer C.free(unsafe.Pointer(method))
	req := C.CBytes(raw)
	defer C.free(req)
	C.call_host(method, (*C.uint8_t)(req), C.size_t(len(raw)))
}
