package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// catalogueFor reads one listing body the way the plugin does for a site.
func catalogueFor(t *testing.T, site, body string) []qoderModel {
	t.Helper()
	spec, ok := siteFor(site)
	if !ok {
		t.Fatalf("unknown site %q", site)
	}
	entries, err := parseListing([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	return usableModels(entries, spec)
}

func idOfKey(models []qoderModel, key string) string {
	for _, m := range models {
		if strings.EqualFold(m.Key, key) {
			return m.ID
		}
	}
	return ""
}

// The catalogue's display names are what a person recognises, so they are the
// ids a panel lists. Qoder's internal key stays the routing key, not the label.
func TestCataloguePublishesReadableModelIDs(t *testing.T) {
	models := catalogueFor(t, siteQoder, `{"app":[
		{"key":"gmodel","display_name":"GLM-5.3","enable":true},
		{"key":"qmodel_38max","display_name":"Qwen3.8-Max","enable":true},
		{"key":"kmodel_latest","display_name":"Kimi-K3","enable":true},
		{"key":"jmodel","enable":true}
	]}`)
	want := map[string]string{
		"gmodel":        "glm-5.3",
		"qmodel_38max":  "qwen3.8-max",
		"kmodel_latest": "kimi-k3",
		"jmodel":        "jmodel",
	}
	for key, id := range want {
		if got := idOfKey(models, key); got != id {
			t.Fatalf("%s id = %q, want %q", key, got, id)
		}
	}
	info := modelInfo(models[0])
	if info.ID != "glm-5.3" || info.DisplayName != "GLM-5.3" || info.Name != "GLM-5.3" {
		t.Fatalf("info = %+v", info)
	}
}

// The shipped list is what a panel shows when the catalogue cannot be read, so
// it carries the same readable ids the live catalogue publishes.
func TestFallbackCataloguePublishesReadableIDs(t *testing.T) {
	spec, _ := siteFor(siteQoder)
	ids := map[string]bool{}
	for _, m := range fallbackModels(spec) {
		if m.ID != strings.ToLower(m.ID) {
			t.Fatalf("%s id is not lowercase: %q", m.Key, m.ID)
		}
		ids[m.ID] = true
	}
	for _, want := range []string{"glm-5.3", "glm-5.3-flash", "deepseek-v4-pro", "kimi-k3", "kimi-k2.8-preview", "qwen3.8-max"} {
		if !ids[want] {
			t.Fatalf("fallback is missing %q: %v", want, ids)
		}
	}
	if ids["gmodel"] || ids["kmodel_latest"] {
		t.Fatalf("fallback still publishes raw keys: %v", ids)
	}
}

// A catalogue entry with no display name of its own still gets the name the
// shipped list carries for that key.
func TestCatalogueUsesTheShippedNameWhenTheListingHasNone(t *testing.T) {
	models := catalogueFor(t, siteQoder, `{"app":[
		{"key":"gmodel","enable":true},
		{"key":"dmodel","display_name":"dmodel","enable":true},
		{"key":"mystery","display_name":"mystery","enable":true}
	]}`)
	if got := idOfKey(models, "gmodel"); got != "glm-5.3" {
		t.Fatalf("gmodel id = %q", got)
	}
	if got := idOfKey(models, "dmodel"); got != "deepseek-v4-pro" {
		t.Fatalf("dmodel id = %q", got)
	}
	// A display name that only repeats the key is no name at all.
	if got := idOfKey(models, "mystery"); got != "mystery" {
		t.Fatalf("mystery id = %q", got)
	}
}

// Two entries must never publish one id: the later one keeps its own key so a
// request still names exactly one catalogue entry.
func TestAssignIDsKeepsEveryIDUnique(t *testing.T) {
	models := assignIDs([]qoderModel{
		{Key: "gmodel", Name: "GLM-5.3"},
		{Key: "gm_preview", Name: "GLM 5.3"},
	})
	if models[0].ID != "glm-5.3" || models[1].ID != "gm_preview" {
		t.Fatalf("ids = %q %q", models[0].ID, models[1].ID)
	}
	// A display name that would shadow another entry's key yields to that key.
	models = assignIDs([]qoderModel{
		{Key: "glm-5.3", Name: "GLM-5.3"},
		{Key: "gmodel", Name: "GLM-5.3"},
	})
	if models[0].ID != "glm-5.3" || models[1].ID != "gmodel" {
		t.Fatalf("ids = %q %q", models[0].ID, models[1].ID)
	}
}

// A request may name the readable id, the display name in any casing, or the
// old internal key; each resolves to the one catalogue entry behind them.
func TestFindModelResolvesIDsNamesAndKeys(t *testing.T) {
	listingCache.Lock()
	listingCache.entries = nil
	listingCache.Unlock()
	sender := &fakeSender{reply: func(r pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		if !strings.Contains(r.URL, "/model/list") {
			t.Errorf("unexpected url %s", r.URL)
		}
		return jsonResponse(`{"app":[{"key":"gmodel","display_name":"GLM-5.3","enable":true}]}`), nil
	}}
	spec, _ := siteFor(siteQoder)
	c := credential{Site: siteQoder, UID: "user-1"}
	for _, name := range []string{"glm-5.3", "GLM-5.3", "GLM 5.3", "gmodel", "qoder/glm-5.3"} {
		m, ok := findModel(sender, c, spec, trimModelPrefix(name))
		if !ok || m.Key != "gmodel" {
			t.Fatalf("%s = %+v, %v", name, m, ok)
		}
	}
	if m, ok := findModel(sender, c, spec, "not-a-model"); ok {
		t.Fatalf("unknown model resolved to %+v", m)
	}
}

// Execution still speaks Qoder's key upstream while answering under the
// readable id the client asked for.
func TestExecuteMapsReadableIDToTheCatalogueKey(t *testing.T) {
	listingCache.Lock()
	listingCache.entries = nil
	listingCache.Unlock()
	var sent map[string]any
	sender := &fakeSender{reply: func(r pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		switch {
		case strings.Contains(r.URL, "/model/list"):
			return jsonResponse(`{"app":[{"key":"gmodel","display_name":"GLM-5.3","enable":true,"max_input_tokens":200000}]}`), nil
		case strings.Contains(r.URL, "agent_chat_generation"):
			if err := json.Unmarshal(decodeMust(t, string(r.Body)), &sent); err != nil {
				t.Fatalf("wire body: %v", err)
			}
			inner := `{"choices":[{"delta":{"content":"Hello"},"finish_reason":"stop"}]}`
			return pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(sseOf(inner))}, nil
		default:
			t.Errorf("unexpected url %s", r.URL)
			return jsonResponse(`{}`), nil
		}
	}}
	payload := []byte(`{"model":"glm-5.3","messages":[{"role":"user","content":"hi"}]}`)
	resp, err := execute(pluginapi.ExecutorRequest{Model: "glm-5.3", Payload: payload, StorageJSON: testCredential(siteQoder), AuthID: "a"}, sender)
	if err != nil {
		t.Fatal(err)
	}
	business, _ := sent["business"].(map[string]any)
	cfg, _ := sent["model_config"].(map[string]any)
	if business["model_key"] != "gmodel" || cfg["key"] != "gmodel" || cfg["display_name"] != "GLM-5.3" {
		t.Fatalf("upstream model = %v / %v", business["model_key"], cfg)
	}
	var out struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(resp.Payload, &out); err != nil {
		t.Fatal(err)
	}
	if out.Model != "glm-5.3" {
		t.Fatalf("answer model = %q", out.Model)
	}
}

// A job token Qoder calls expired must not cost the account its catalogue: the
// device token beside it reads the same listing, and that listing carries models
// (QoDuck's lmodel among them) the shipped fallback predates.
func TestListingRetriesWithDeviceTokenWhenSignInLapsed(t *testing.T) {
	resetListingCache()
	c, err := readCredential(testCredential(siteQoder))
	if err != nil {
		t.Fatal(err)
	}
	spec, _ := siteFor(c.Site)
	calls := 0
	sender := &fakeSender{reply: func(pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		calls++
		if calls == 1 {
			return pluginapi.HTTPResponse{StatusCode: 403, Body: []byte(`{"code":"105","message":"Login expired"}`)}, nil
		}
		return jsonResponse(`{"app":[{"key":"lmodel","display_name":"QoDuck","enable":true,"is_vl":true,"max_input_tokens":180000}]}`), nil
	}}
	models, err := readListing(sender, c, spec)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("catalogue calls = %d", calls)
	}
	if got := idOfKey(models, "lmodel"); got != "qoduck" {
		t.Fatalf("models = %+v", models)
	}
}

// The extra call belongs to a lapsed token, not to every failure.
func TestListingDoesNotRetryOtherFailures(t *testing.T) {
	resetListingCache()
	c, err := readCredential(testCredential(siteQoder))
	if err != nil {
		t.Fatal(err)
	}
	spec, _ := siteFor(c.Site)
	calls := 0
	sender := &fakeSender{reply: func(pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		calls++
		return pluginapi.HTTPResponse{StatusCode: 500, Body: []byte(`{"message":"upstream is unwell"}`)}, nil
	}}
	if _, err := readListing(sender, c, spec); err == nil {
		t.Fatal("a failed catalogue read reported success")
	}
	if calls != 1 {
		t.Fatalf("catalogue calls = %d", calls)
	}
}
