//go:build cshared

// Command cpa-quota builds the CLIProxyAPI plugin library:
//
//	go build -tags cshared -buildmode=c-shared -o cpa-quota.so ./cmd/cpa-quota
//
// CLIProxyAPI loads the library, calls cliproxy_plugin_init with its host
// API, and then sends every RPC through the plugin call function. The plugin
// ID is the library's file name without its extension.
package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

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

extern int cpaQuotaCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cpaQuotaFree(void*, size_t);
extern void cpaQuotaShutdown(void);

static const cliproxy_host_api* host_api;

static void set_host_api(const cliproxy_host_api* host) { host_api = host; }

static int call_host(const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	if (host_api == NULL || host_api->call == NULL) {
		return 1;
	}
	return host_api->call(host_api->host_ctx, method, request, request_len, response);
}

static void free_host_buffer(void* ptr, size_t len) {
	if (host_api != NULL && host_api->free_buffer != NULL && ptr != NULL) {
		host_api->free_buffer(ptr, len);
	}
}

static void fill_plugin_api(cliproxy_plugin_api* plugin, uint32_t abi_version) {
	plugin->abi_version = abi_version;
	plugin->call = (cliproxy_plugin_call_fn)cpaQuotaCall;
	plugin->free_buffer = (cliproxy_plugin_free_fn)cpaQuotaFree;
	plugin->shutdown = (cliproxy_plugin_shutdown_fn)cpaQuotaShutdown;
}
*/
import "C"

import (
	"encoding/json"
	"unsafe"

	"github.com/sketchain/cpa-quota/internal/plugin"
)

// abiVersion is the native C ABI version the plugin implements.
const abiVersion = 1

var app = plugin.NewApp()

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, api *C.cliproxy_plugin_api) C.int {
	if api == nil {
		return 1
	}
	C.set_host_api(host)
	C.fill_plugin_api(api, C.uint32_t(abiVersion))
	app.SetHostLogger(hostLog)
	return 0
}

//export cpaQuotaCall
func cpaQuotaCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr, response.len = nil, 0
	}
	if method == nil {
		return 1
	}
	var payload []byte
	if request != nil && requestLen > 0 {
		payload = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	raw := app.HandleMethod(C.GoString(method), payload)
	if response != nil && len(raw) > 0 {
		response.ptr = C.CBytes(raw)
		response.len = C.size_t(len(raw))
	}
	return 0
}

//export cpaQuotaFree
func cpaQuotaFree(ptr unsafe.Pointer, _ C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cpaQuotaShutdown
func cpaQuotaShutdown() { app.Shutdown() }

// hostLog forwards a log line to CLIProxyAPI's log through host.log.
func hostLog(level, message string) {
	payload, err := json.Marshal(map[string]string{"level": level, "message": message})
	if err != nil {
		return
	}
	method := C.CString("host.log")
	defer C.free(unsafe.Pointer(method))
	request := C.CBytes(payload)
	defer C.free(request)
	var response C.cliproxy_buffer
	if C.call_host(method, (*C.uint8_t)(request), C.size_t(len(payload)), &response) == 0 && response.ptr != nil {
		C.free_host_buffer(response.ptr, response.len)
	}
}
