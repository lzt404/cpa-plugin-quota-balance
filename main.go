package main

/*
#include <stdint.h>
#include <stdlib.h>
typedef struct { void *ptr; size_t len; } buffer;
typedef int (*host_call)(void*, const char*, const uint8_t*, size_t, buffer*);
typedef void (*free_buffer)(void*, size_t);
typedef struct { uint32_t abi_version; void *host_ctx; host_call call; free_buffer free_buffer; } host_api;
typedef int (*plugin_call)(char*, uint8_t*, size_t, buffer*);
typedef void (*plugin_shutdown)(void);
typedef struct { uint32_t abi_version; plugin_call call; free_buffer free_buffer; plugin_shutdown shutdown; } plugin_api;
extern int quotaBalanceCall(char*, uint8_t*, size_t, buffer*);
extern void quotaBalanceFree(void*, size_t);
extern void quotaBalanceShutdown(void);
static void initialize(plugin_api *out) {
 out->abi_version = 1;
 out->call = quotaBalanceCall;
 out->free_buffer = quotaBalanceFree;
 out->shutdown = quotaBalanceShutdown;
}
static int hostCallInvoke(host_call fn, void *ctx, const char *method, const uint8_t *request, size_t length, buffer *response) {
 return fn(ctx, method, request, length, response);
}
*/
import "C"

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

func main() {}

// hostBridge holds the host call pointer captured from cliproxy_plugin_init.
// The host context pointer is stable for the lifetime of the loaded library, so it
// is captured once and reused for every plugin-to-host RPC.
type hostBridge struct {
	call    C.host_call
	hostCtx unsafe.Pointer
}

var (
	bridgeMu sync.RWMutex
	bridge   *hostBridge
)

func hostCall(ctx context.Context, method string, payload any) (json.RawMessage, error) {
	bridgeMu.RLock()
	handler := bridge
	bridgeMu.RUnlock()
	if handler == nil || handler.call == nil || handler.hostCtx == nil {
		return nil, errors.New("host callback unavailable")
	}
	encoded, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		return nil, errMarshal
	}
	methodName := C.CString(method)
	defer C.free(unsafe.Pointer(methodName))
	var requestPtr *C.uint8_t
	if len(encoded) > 0 {
		requestPtr = (*C.uint8_t)(C.CBytes(encoded))
		defer C.free(unsafe.Pointer(requestPtr))
	}
	var response C.buffer
	if code := C.hostCallInvoke(handler.call, handler.hostCtx, methodName, requestPtr, C.size_t(len(encoded)), &response); code != 0 {
		return nil, fmt.Errorf("host callback %s failed with code %d", method, int(code))
	}
	raw := C.GoBytes(response.ptr, C.int(response.len))
	if response.ptr != nil {
		C.free(response.ptr)
	}
	var envelope pluginabi.Envelope
	if errDecode := json.Unmarshal(raw, &envelope); errDecode != nil {
		return nil, errDecode
	}
	if !envelope.OK {
		if envelope.Error == nil {
			return nil, fmt.Errorf("host callback %s failed", method)
		}
		return nil, errors.New(envelope.Error.Message)
	}
	return envelope.Result, nil
}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.host_api, out *C.plugin_api) C.int {
	if host == nil || out == nil || host.abi_version != 1 || host.call == nil || host.free_buffer == nil || host.host_ctx == nil {
		return 1
	}
	C.initialize(out)
	bridgeMu.Lock()
	bridge = &hostBridge{call: host.call, hostCtx: host.host_ctx}
	bridgeMu.Unlock()
	return 0
}

//export quotaBalanceCall
func quotaBalanceCall(method *C.char, data *C.uint8_t, length C.size_t, out *C.buffer) (code C.int) {
	if out == nil {
		return 1
	}
	out.ptr, out.len = nil, 0
	defer func() {
		if recover() != nil {
			code = 1
		}
	}()
	if method == nil || length > 1<<20 || (length > 0 && data == nil) {
		return 1
	}
	value, err := dispatch(C.GoString(method), C.GoBytes(unsafe.Pointer(data), C.int(length)), hostCall)
	envelope := pluginabi.Envelope{OK: err == nil}
	if err != nil {
		envelope.Error = &pluginabi.Error{Code: "quota_balance", Message: err.Error()}
	} else {
		envelope.Result, err = json.Marshal(value)
		if err != nil {
			return 1
		}
	}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		return 1
	}
	out.ptr, out.len = C.CBytes(encoded), C.size_t(len(encoded))
	return 0
}

//export quotaBalanceFree
func quotaBalanceFree(pointer unsafe.Pointer, length C.size_t) { C.free(pointer) }

//export quotaBalanceShutdown
func quotaBalanceShutdown() {
	bridgeMu.Lock()
	bridge = nil
	bridgeMu.Unlock()
}