package httpapi

import (
	"net/http"
	"testing"
)

// These tests pin the "API keys never leave the server" contract for GET/PUT /api/config: the
// response carries only the always-present `*_api_key_set` booleans, and the request's omitted /
// null / empty / whitespace-only key values keep the stored key (asserted against the settings
// store, never the response).

var apiKeyRawFields = []string{"google_api_key", "anthropic_api_key", "deepseek_api_key", "openai_api_key"}

var apiKeySetFields = []string{"google_api_key_set", "anthropic_api_key_set", "deepseek_api_key_set", "openai_api_key_set"}

func assertNoRawAPIKeys(t *testing.T, where string, cfg map[string]any) {
	t.Helper()
	for _, key := range apiKeyRawFields {
		if v, ok := cfg[key]; ok {
			t.Errorf("%s config leaked raw key %q = %v", where, key, v)
		}
	}
}

func assertAPIKeySetFields(t *testing.T, where string, cfg map[string]any) {
	t.Helper()
	for _, key := range apiKeySetFields {
		v, ok := cfg[key]
		if !ok {
			t.Errorf("%s config missing %q: %v", where, key, cfg)
			continue
		}
		if _, isBool := v.(bool); !isBool {
			t.Errorf("%s config %q = %T, want bool", where, key, v)
		}
	}
}

// validConfigBody is a PUT body that satisfies SystemSettingsSchema (local mode).
func validConfigBody() map[string]any {
	return map[string]any{
		"language":        "en",
		"input_dir":       "/tmp/raws",
		"output_root_dir": "/tmp/archive",
		"ollama_model":    "qwen3.5:9b",
		"ollama_host":     "http://127.0.0.1:11434",
	}
}

func TestGetConfigNeverReturnsAPIKeys(t *testing.T) {
	env := newTestEnv()
	env.settings.cfg.GoogleAPIKey = "test-google-key"
	env.settings.cfg.AnthropicAPIKey = "   " // whitespace-only, so not "set"
	env.settings.cfg.DeepSeekAPIKey = ""
	env.settings.cfg.OpenAIAPIKey = "test-openai-key"

	body := decodeJSON(t, doJSON(t, env.handler, http.MethodGet, "/api/config", nil, nil))
	assertNoRawAPIKeys(t, "GET", body)
	assertAPIKeySetFields(t, "GET", body)

	if body["google_api_key_set"] != true {
		t.Errorf("google_api_key_set = %v, want true", body["google_api_key_set"])
	}
	if body["openai_api_key_set"] != true {
		t.Errorf("openai_api_key_set = %v, want true", body["openai_api_key_set"])
	}
	if body["anthropic_api_key_set"] != false {
		t.Errorf("anthropic_api_key_set = %v, want false for whitespace-only key", body["anthropic_api_key_set"])
	}
	if body["deepseek_api_key_set"] != false {
		t.Errorf("deepseek_api_key_set = %v, want false", body["deepseek_api_key_set"])
	}
}

func TestPutConfigResponseNeverReturnsAPIKeys(t *testing.T) {
	env := newTestEnv()
	body := validConfigBody()
	body["google_api_key"] = "test-google-key"

	rec := doJSON(t, env.handler, http.MethodPut, "/api/config", body, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	config := decodeJSON(t, rec)["config"].(map[string]any)
	assertNoRawAPIKeys(t, "PUT", config)
	assertAPIKeySetFields(t, "PUT", config)

	if config["google_api_key_set"] != true {
		t.Errorf("google_api_key_set = %v, want true after saving a new key", config["google_api_key_set"])
	}
	if env.settings.cfg.GoogleAPIKey != "test-google-key" {
		t.Errorf("stored GoogleAPIKey = %q, want the new key", env.settings.cfg.GoogleAPIKey)
	}
}

func TestPutConfigEmptyAPIKeyKeepsStoredKey(t *testing.T) {
	cases := []struct {
		name    string
		present bool
		value   any
	}{
		{name: "omitted"},
		{name: "null", present: true, value: nil},
		{name: "empty", present: true, value: ""},
		{name: "whitespace", present: true, value: "   "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv()
			env.settings.cfg.GoogleAPIKey = "stored-google-key"
			env.settings.cfg.AnthropicAPIKey = "stored-anthropic-key"
			env.settings.cfg.DeepSeekAPIKey = "stored-deepseek-key"
			env.settings.cfg.OpenAIAPIKey = "stored-openai-key"

			body := validConfigBody()
			if tc.present {
				body["google_api_key"] = tc.value
				body["anthropic_api_key"] = tc.value
				body["deepseek_api_key"] = tc.value
				body["openai_api_key"] = tc.value
			}

			rec := doJSON(t, env.handler, http.MethodPut, "/api/config", body, nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}

			// The store must receive nil for every key ("keep"), not a pointer to "".
			if len(env.settings.updates) != 1 {
				t.Fatalf("UpdateConfig calls = %d, want 1", len(env.settings.updates))
			}
			patch := env.settings.updates[0]
			for name, ptr := range map[string]*string{
				"google": patch.GoogleAPIKey, "anthropic": patch.AnthropicAPIKey,
				"deepseek": patch.DeepSeekAPIKey, "openai": patch.OpenAIAPIKey,
			} {
				if ptr != nil {
					t.Errorf("%s patch key = %q, want nil so the stored key is kept", name, *ptr)
				}
			}

			// The stored values survive.
			stored := map[string]string{
				"google": env.settings.cfg.GoogleAPIKey, "anthropic": env.settings.cfg.AnthropicAPIKey,
				"deepseek": env.settings.cfg.DeepSeekAPIKey, "openai": env.settings.cfg.OpenAIAPIKey,
			}
			for name, want := range map[string]string{
				"google": "stored-google-key", "anthropic": "stored-anthropic-key",
				"deepseek": "stored-deepseek-key", "openai": "stored-openai-key",
			} {
				if stored[name] != want {
					t.Errorf("stored %s key = %q, want %q", name, stored[name], want)
				}
			}

			// The response still reports the keys as configured.
			config := decodeJSON(t, rec)["config"].(map[string]any)
			assertNoRawAPIKeys(t, "PUT", config)
			for _, key := range apiKeySetFields {
				if config[key] != true {
					t.Errorf("%s = %v, want true", key, config[key])
				}
			}
		})
	}
}

func TestPutConfigNewAPIKeyReplacesStoredKey(t *testing.T) {
	env := newTestEnv()
	env.settings.cfg.GoogleAPIKey = "stored-google-key"

	body := validConfigBody()
	body["google_api_key"] = "new-test-key"
	rec := doJSON(t, env.handler, http.MethodPut, "/api/config", body, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	if env.settings.cfg.GoogleAPIKey != "new-test-key" {
		t.Errorf("stored GoogleAPIKey = %q, want the replacement", env.settings.cfg.GoogleAPIKey)
	}
	patch := env.settings.updates[0]
	if patch.GoogleAPIKey == nil || *patch.GoogleAPIKey != "new-test-key" {
		t.Errorf("patch GoogleAPIKey = %v, want new-test-key", patch.GoogleAPIKey)
	}

	config := decodeJSON(t, rec)["config"].(map[string]any)
	assertNoRawAPIKeys(t, "PUT", config)
	if config["google_api_key_set"] != true {
		t.Errorf("google_api_key_set = %v, want true", config["google_api_key_set"])
	}
}
