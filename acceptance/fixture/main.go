package main

// Fixture plugin used only by acceptance.py. It registers:
//   * a fake executor that answers /v1/chat/completions with the selected auth ID,
//   * a quota provider that reads per-credential fractions from a JSON state file,
//   * static credentials for the acceptance run via command line registration.
// It is never published or installed by users.

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
extern int fixtureCall(char*, uint8_t*, size_t, buffer*);
extern void fixtureFree(void*, size_t);
extern void fixtureShutdown(void);
static void initialize(plugin_api *out) {
 out->abi_version = 1;
 out->call = fixtureCall;
 out->free_buffer = fixtureFree;
 out->shutdown = fixtureShutdown;
}
*/
import "C"

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"gopkg.in/yaml.v3"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func main() {}

type state struct {
	Quotas      map[string]float64 `json:"quotas"`
	Models      []string           `json:"models"`
	Credentials string             `json:"credentials"`
}

var statePath = "/tmp/fixture-state.json"

// applyFixtureConfig reads the plugin's own config block so acceptance can point the
// fixture at the run directory instead of a shared temp path.
func applyFixtureConfig(raw []byte) {
	var document struct {
		StateFile string `yaml:"state_file"`
	}
	if err := yaml.Unmarshal(raw, &document); err == nil && strings.TrimSpace(document.StateFile) != "" {
		statePath = document.StateFile
	}
}

func loadState() state {
	raw, err := os.ReadFile(statePath)
	if err != nil {
		return state{}
	}
	var parsed state
	_ = json.Unmarshal(raw, &parsed)
	return parsed
}

func fixtureDispatch(method string, raw []byte) (any, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		var request map[string]any
		if err := json.Unmarshal(raw, &request); err != nil {
			return nil, err
		}
		// config_yaml arrives base64-encoded because it is a JSON []byte field.
		if encoded, ok := request["config_yaml"].(string); ok {
			if payload, errDecode := base64.StdEncoding.DecodeString(encoded); errDecode == nil {
				applyFixtureConfig(payload)
			}
		}
		return map[string]any{
			"schema_version": request["schema_version"],
			"metadata": map[string]any{
				"Name":             "fixture",
				"Version":          "0.0.1",
				"Author":           "acceptance",
				"GitHubRepository": "https://example.invalid/fixture",
			},
			"capabilities": map[string]any{
				"auth_provider":            false,
				"executor":                 true,
				"model_provider":           true,
				"executor_model_scope":     "both",
				"executor_input_formats":   []string{"openai"},
				"executor_output_formats":  []string{"openai"},
				"quota_provider":           true,
				"model_registrar":          true,
				"command_line_plugin":      false,
			},
		}, nil
	case pluginabi.MethodModelStatic:
		return map[string]any{"Provider": "claude", "Models": []any{map[string]any{"ID": "fixture-model", "DisplayName": "fixture-model"}}}, nil
	case pluginabi.MethodModelForAuth:
		return map[string]any{"Provider": "claude", "Models": []any{map[string]any{"ID": "fixture-model", "DisplayName": "fixture-model"}}}, nil
	case pluginabi.MethodAuthIdentifier:
		return map[string]any{"identifier": "fixture"}, nil
	case pluginabi.MethodAuthParse:
		var request pluginapi.AuthParseRequest
		if err := json.Unmarshal(raw, &request); err != nil {
			return nil, err
		}
		var file struct {
			Type     string `json:"type"`
			Label    string `json:"label"`
			Priority int    `json:"priority"`
		}
		if err := json.Unmarshal(request.RawJSON, &file); err != nil {
			return nil, err
		}
		if file.Type != "fixture" {
			return pluginapi.AuthParseResponse{Handled: false}, nil
		}
		return pluginapi.AuthParseResponse{Handled: true, Auth: pluginapi.AuthData{
			Provider: "fixture",
			ID:       file.Label,
			Label:    file.Label,
			FileName: request.FileName,
			Metadata: map[string]any{"type": "fixture", "label": file.Label, "priority": file.Priority},
		}}, nil
	case pluginabi.MethodQuotaIdentifier:
		return map[string]any{"identifier": "fixture"}, nil
	case pluginabi.MethodQuotaDescribe:
		return map[string]any{"supported_providers": []string{"fixture"}, "display_name": "Fixture"}, nil
	case pluginabi.MethodQuotaFetch:
		var request pluginapi.QuotaFetchRequest
		if err := json.Unmarshal(raw, &request); err != nil {
			return nil, err
		}
		current := loadState()
		name := strings.TrimSpace(request.AuthID)
		if name == "" {
			name = strings.TrimSpace(request.AuthIndex)
		}
		fraction, ok := current.Quotas[name]
		if !ok {
			return pluginapi.QuotaFetchResponse{}, nil
		}
		return pluginapi.QuotaFetchResponse{Summary: []pluginapi.QuotaMetric{{Key: "remaining", Label: "Remaining", Value: fraction * 100, Unit: "%"}},
			Groups: []pluginapi.QuotaGroup{{DisplayName: "windows", Buckets: []pluginapi.QuotaBucket{{Window: "5h", RemainingFraction: fraction}}}}}, nil
	case pluginabi.MethodExecutorIdentifier:
		return map[string]any{"identifier": "claude", "identifiers": []string{"claude"}}, nil
	case pluginabi.MethodExecutorExecute:
		var request pluginapi.ExecutorRequest
		if err := json.Unmarshal(raw, &request); err != nil {
			return nil, err
		}
		authID := request.AuthID
		if authID == "" {
			authID, _ = request.AuthMetadata["label"].(string)
		}
		if authID == "" {
			authID = "unknown-auth"
		}
		body, _ := json.Marshal(map[string]any{
			"id": "chatcmpl-fixture", "object": "chat.completion", "model": "fixture-model",
			"choices": []any{map[string]any{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": authID},
				"finish_reason": "stop",
			}},
		})
		return pluginapi.ExecutorResponse{Payload: body, Headers: http.Header{"Content-Type": []string{"application/json"}}}, nil
	case pluginabi.MethodPluginQuiesce, pluginabi.MethodPluginShutdown:
		return struct{}{}, nil
	default:
		return nil, errors.New("unsupported method " + method)
	}
}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.host_api, out *C.plugin_api) C.int {
	if host == nil || out == nil || host.abi_version != 1 || host.call == nil || host.free_buffer == nil {
		return 1
	}
	C.initialize(out)
	return 0
}

//export fixtureCall
func fixtureCall(method *C.char, data *C.uint8_t, length C.size_t, out *C.buffer) (code C.int) {
	if out == nil {
		return 1
	}
	out.ptr, out.len = nil, 0
	defer func() {
		if recover() != nil {
			code = 1
		}
	}()
	if method == nil || length > 1<<20 {
		return 1
	}
	value, err := fixtureDispatch(C.GoString(method), C.GoBytes(unsafe.Pointer(data), C.int(length)))
	envelope := pluginabi.Envelope{OK: err == nil}
	if err != nil {
		envelope.Error = &pluginabi.Error{Code: "fixture", Message: err.Error()}
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

//export fixtureFree
func fixtureFree(pointer unsafe.Pointer, length C.size_t) { C.free(pointer) }

//export fixtureShutdown
func fixtureShutdown() {}