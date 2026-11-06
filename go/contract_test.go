package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestRegistrationContract(t *testing.T) {
	out, err := handleMethod("plugin.register", nil)
	if err != nil {
		t.Fatal(err)
	}
	reg, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("registration = %T", out)
	}
	if reg["schema_version"] != pluginabi.SchemaVersion || pluginabi.SchemaVersion != 10 {
		t.Fatalf("schema = %v", reg["schema_version"])
	}
	meta := reg["metadata"].(pluginapi.Metadata)
	if meta.Name != "OMC Qoder OAuth" || meta.Version != pluginVersion {
		t.Fatalf("metadata = %+v", meta)
	}
	if meta.Logo != publicAssetBaseURL+pluginLogo {
		t.Fatalf("logo = %q", meta.Logo)
	}
	if len(meta.ConfigFields) != 4 {
		t.Fatalf("config fields = %+v", meta.ConfigFields)
	}
	for _, field := range meta.ConfigFields {
		if field.Default == nil || field.Description == "" {
			t.Fatalf("field %q has no default or description", field.Name)
		}
	}
	caps := reg["capabilities"].(map[string]any)
	if caps["executor_input_formats"].([]string)[0] != "openai" || caps["executor_output_formats"].([]string)[0] != "openai" {
		t.Fatalf("formats = %v", caps)
	}
	if caps["executor_model_scope"] != "oauth" {
		t.Fatalf("scope = %v", caps["executor_model_scope"])
	}
}

func TestAuthIdentifierPublishesTwoEntries(t *testing.T) {
	out, err := handleMethod("auth.identifier", nil)
	if err != nil {
		t.Fatal(err)
	}
	got := out.(map[string]any)
	if got["identifier"] != siteQoder || got["credential_providers"].([]string)[0] != credentialProvider {
		t.Fatalf("identifier = %v", got)
	}
	ids := got["identifiers"].([]string)
	if len(ids) != 2 || ids[0] != siteQoder || ids[1] != siteQoderCN {
		t.Fatalf("identifiers = %v", ids)
	}
	entries := got["entries"].([]pluginapi.AuthProviderEntry)
	if len(entries) != 2 {
		t.Fatalf("entries = %v", entries)
	}
	if entries[0].Identifier != siteQoder || entries[0].Name != "Qoder" || entries[1].Name != "Qoder CN" {
		t.Fatalf("entries = %+v", entries)
	}
	if entries[0].Logo == entries[1].Logo {
		t.Fatal("the two entries share one logo")
	}
	if entries[0].Logo != publicAssetBaseURL+"qoder-color.svg" || entries[1].Logo != publicAssetBaseURL+"qoder.svg" {
		t.Fatalf("logos = %q %q", entries[0].Logo, entries[1].Logo)
	}
	for _, method := range []string{"executor.identifier", "quota.identifier"} {
		out, err := handleMethod(method, nil)
		if err != nil {
			t.Fatal(err)
		}
		if out.(map[string]string)["identifier"] != credentialProvider {
			t.Fatalf("%s = %v", method, out)
		}
	}
}

func TestParseAuthReadsPersistedFile(t *testing.T) {
	raw := map[string]any{
		"type":           "qoder",
		"site":           siteQoderCN,
		"access":         "jt",
		"refresh":        "rt",
		"device_token":   "dt",
		"uid":            "u-1",
		"email":          "dev@example.test",
		"machine_id":     "11111111-2222-3333-4444-555555555555",
		"oauth_provider": siteQoderCN,
	}
	body, _ := json.Marshal(raw)
	req, _ := json.Marshal(pluginapi.AuthParseRequest{Provider: siteQoderCN, FileName: "qoder-x.json", RawJSON: body})
	out, err := handleMethod("auth.parse", req)
	if err != nil {
		t.Fatal(err)
	}
	resp := out.(pluginapi.AuthParseResponse)
	if !resp.Handled || resp.Auth.Provider != credentialProvider {
		t.Fatalf("parse = %+v", resp)
	}
	if resp.Auth.Metadata["oauth_provider"] != siteQoderCN || resp.Auth.Label != "dev@example.test" {
		t.Fatalf("auth = %+v", resp.Auth)
	}
	if resp.Auth.ID != "qoder-x.json" || resp.Auth.FileName != "qoder-x.json" {
		t.Fatalf("identity = %q %q", resp.Auth.ID, resp.Auth.FileName)
	}
}

func TestParseAuthRejectsForeignFiles(t *testing.T) {
	cases := []string{
		`{"type":"zcode","site":"qoder","uid":"u","machine_id":"m","access":"a"}`,
		`{"type":"qoder","site":"elsewhere","uid":"u","machine_id":"m","access":"a"}`,
		`{"type":"qoder","site":"qoder","uid":"u","machine_id":"m"}`,
		`not json`,
	}
	for _, body := range cases {
		req, _ := json.Marshal(pluginapi.AuthParseRequest{Provider: siteQoder, RawJSON: []byte(body)})
		out, err := handleMethod("auth.parse", req)
		if err != nil {
			t.Fatal(err)
		}
		if resp := out.(pluginapi.AuthParseResponse); resp.Handled {
			t.Fatalf("accepted %s", body)
		}
	}
	req, _ := json.Marshal(pluginapi.AuthParseRequest{Provider: "someone-else"})
	out, _ := handleMethod("auth.parse", req)
	if out.(pluginapi.AuthParseResponse).Handled {
		t.Fatal("accepted a foreign provider")
	}
}

func TestManagementRegisterListsPassiveResources(t *testing.T) {
	out, err := handleMethod("management.register", nil)
	if err != nil {
		t.Fatal(err)
	}
	resources := out.(map[string]any)["resources"].([]map[string]any)
	want := []string{"/qoder-color.svg", "/qoder.svg", "/omc.json"}
	if len(resources) != len(want) {
		t.Fatalf("resources = %v", resources)
	}
	for i, path := range want {
		if resources[i]["path"] != path || resources[i]["menu"] != "" {
			t.Fatalf("resource[%d] = %v", i, resources[i])
		}
	}
}

func TestLogosMatchPublishedCopies(t *testing.T) {
	if testing.Short() {
		t.Skip("reads the published asset copies")
	}
	for name, embedded := range map[string][]byte{"qoder-color.svg": qoderLogoColorSVG, "qoder.svg": qoderLogoMonoSVG} {
		published, err := os.ReadFile(filepath.Join("..", "assets", name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !bytes.Equal(published, embedded) {
			t.Fatalf("%s differs between assets/ and go/assets/", name)
		}
		if !bytes.Contains(embedded, []byte("<svg")) || bytes.Contains(embedded, []byte("<script")) {
			t.Fatalf("%s is not a passive SVG", name)
		}
	}
}

func TestModelForAuthUsesTheCatalogue(t *testing.T) {
	sender := &fakeSender{reply: func(r pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		if _, ok := r.Headers["Authorization"]; !ok {
			t.Error("catalogue request is unsigned")
		}
		return jsonResponse(`{"app":[{"key":"ultimate","display_name":"Ultimate","enable":true,"max_input_tokens":1000000},{"key":"fast","display_name":"Fast","enable":false}]}`), nil
	}}
	out, err := modelsForAuth(pluginapi.AuthModelRequest{StorageJSON: testCredential(siteQoder)}, sender)
	if err != nil {
		t.Fatal(err)
	}
	if out.Provider != credentialProvider || len(out.Models) != 1 {
		t.Fatalf("models = %+v", out)
	}
	if out.Models[0].ID != "ultimate" || out.Models[0].OwnedBy != credentialProvider {
		t.Fatalf("model = %+v", out.Models[0])
	}
}

func TestModelForAuthFallsBackWhenCatalogueFails(t *testing.T) {
	listingCache.Lock()
	listingCache.entries = nil
	listingCache.Unlock()
	sender := &fakeSender{reply: func(pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		return pluginapi.HTTPResponse{StatusCode: 503}, nil
	}}
	out, err := modelsForAuth(pluginapi.AuthModelRequest{StorageJSON: testCredential(siteQoderCN)}, sender)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Models) == 0 {
		t.Fatal("no fallback models")
	}
	for _, m := range out.Models {
		if m.ID != "lite" && m.ID != "ultimate" && m.ID != "performance" && m.ID != "efficient" {
			t.Fatalf("CN fallback leaked %q", m.ID)
		}
	}
}
