package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type fakeSender struct {
	mu    sync.Mutex
	calls []pluginapi.HTTPRequest
	reply func(pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error)
}

func (f *fakeSender) Do(req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	f.mu.Lock()
	f.calls = append(f.calls, req)
	f.mu.Unlock()
	return f.reply(req)
}

func (f *fakeSender) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func testCredential(site string) []byte {
	return storageOf(credential{
		Site:        site,
		Access:      "jt-test-token",
		Refresh:     "rt-old",
		Expires:     time.Now().Add(2 * time.Hour).UnixMilli(),
		DeviceToken: "dt-test-token",
		UID:         "user-1",
		Email:       "dev@example.test",
		MachineID:   "11111111-2222-3333-4444-555555555555",
	})
}

func storageOf(c credential) []byte { b, _ := json.Marshal(c); return b }

func TestCosyBodyCodecRoundTrip(t *testing.T) {
	if len(cosyAlphabet) != 64 {
		t.Fatalf("alphabet has %d symbols", len(cosyAlphabet))
	}
	for _, plain := range []string{`{"a":1}`, "", strings.Repeat("qoder-", 200), "汉字测试"} {
		wire := encodeBody([]byte(plain))
		got, err := decodeBody(wire)
		if err != nil || string(got) != plain {
			t.Fatalf("round trip failed for %d bytes: %v", len(plain), err)
		}
		if strings.Contains(wire, "=") {
			t.Fatal("wire form must use the client's padding, not base64 '='")
		}
	}
}

func TestCosyHeadersSignOneRequest(t *testing.T) {
	spec, _ := siteFor(siteQoder)
	h, err := cosyHeaders("https://api3.qoder.sh/algo/api/v2/model/list?Encode=1", cosyUserFrom(credential{UID: "u1", MachineID: "11111111-2222-3333-4444-555555555555"}), "", spec, "1.1.69", 1700000000)
	if err != nil {
		t.Fatal(err)
	}
	auth := h.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer COSY.") || strings.Count(auth, ".") != 2 {
		t.Fatalf("authorization = %q", auth)
	}
	if h["Cosy-MachineOS"][0] != cosyMachineOS() || h["Cosy-User"][0] != "u1" || h["Cosy-Date"][0] != "1700000000" {
		t.Fatalf("envelope headers = %v", h)
	}
	if h["Cosy-Version"][0] != "1.1.69" || h.Get("Login-Version") != "v2" {
		t.Fatal("envelope version headers are wrong")
	}
}

func TestChatHeadersDropClientIdentityAndAddModel(t *testing.T) {
	spec, _ := siteFor(siteQoder)
	m := qoderModel{Key: "ultimate", Source: "system"}
	h, err := chatHeaders("https://api3.qoder.sh/algo/api/v2/service/pro/sse/agent_chat_generation?Encode=1", cosyUserFrom(credential{UID: "u1", MachineID: "11111111-2222-3333-4444-555555555555"}), "wire", spec, "1.1.69", m, 1700000000)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := h["Cosy-ClientIp"]; ok {
		t.Fatal("chat headers kept the client IP")
	}
	if _, ok := h["Cosy-MachineToken"]; ok {
		t.Fatal("chat headers kept the machine token")
	}
	if _, ok := h["Cosy-MachineType"]; ok {
		t.Fatal("chat headers kept the machine type")
	}
	if h.Get("Accept") != "text/event-stream" || h.Get("X-Model-Key") != "ultimate" || h.Get("X-Model-Source") != "system" {
		t.Fatal("chat headers are missing the stream accept or model headers")
	}
}

func TestLoginStateFitsHostLimitAndRoundTrips(t *testing.T) {
	dir := t.TempDir()
	key, err := loadStateKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	nonce, _ := newNonce()
	machine, _ := newNonce()
	s := loginState{Site: siteQoderCN, Expires: time.Now().Add(time.Minute).Unix(), Nonce: nonce, MachineID: machine}
	encoded, err := sealState(key, s)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) > maxStateLength {
		t.Fatalf("state length %d exceeds %d", len(encoded), maxStateLength)
	}
	round, err := openState(key, encoded)
	if err != nil {
		t.Fatal(err)
	}
	if round.Site != s.Site || round.Expires != s.Expires || !bytes.Equal(round.Nonce, nonce) || !bytes.Equal(round.MachineID, machine) {
		t.Fatal("state did not round-trip")
	}
	modified := []byte(encoded)
	modified[10] ^= 1
	if _, err := openState(key, string(modified)); err == nil {
		t.Fatal("tampered state accepted")
	}
}

func TestStartLoginBuildsBothDeviceFlows(t *testing.T) {
	t.Cleanup(func() { _ = configure(nil) })
	for _, entry := range []string{siteQoder, siteQoderCN} {
		dir := filepathJoin(t)
		spec, _ := siteByEntry(entry)
		out, err := startLogin(pluginapi.AuthLoginStartRequest{Provider: entry, Host: pluginapi.HostConfigSummary{AuthDir: dir}}, &fakeSender{})
		if err != nil {
			t.Fatalf("%s: %v", entry, err)
		}
		u := mustURL(t, out.URL)
		if u.Scheme != "https" || !strings.HasPrefix(u.Host, strings.TrimPrefix(spec.Web, "https://")) || u.Path != "/device/selectAccounts" {
			t.Fatalf("%s: url = %s", entry, out.URL)
		}
		q := u.Query()
		if q.Get("client_id") != spec.ClientID || q.Get("challenge_method") != "S256" || q.Get("challenge") == "" {
			t.Fatalf("%s: query = %v", entry, q)
		}
		if q.Get("redirect_uri") != "" || q.Get("nonce") == "" || q.Get("machine_id") == "" {
			t.Fatalf("%s: unexpected query = %v", entry, q)
		}
		if out.State == "" || len(out.State) > maxStateLength || out.ExpiresAt.IsZero() {
			t.Fatalf("%s: state/expiry = %q %v", entry, out.State, out.ExpiresAt)
		}
	}
}

func TestStartLoginRejectsUnknownEntry(t *testing.T) {
	_, err := startLogin(pluginapi.AuthLoginStartRequest{Provider: "qoder-elsewhere", Host: pluginapi.HostConfigSummary{AuthDir: t.TempDir()}}, &fakeSender{})
	if err == nil || !strings.Contains(err.Error(), "qoder-cn") {
		t.Fatalf("err = %v", err)
	}
}

func TestPollLoginExchangesJobToken(t *testing.T) {
	sender := &fakeSender{reply: func(r pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		switch {
		case strings.Contains(r.URL, "/deviceToken/poll"):
			if !strings.Contains(r.URL, "challenge_method=S256") || !strings.Contains(r.URL, "verifier=") {
				t.Errorf("poll query = %s", r.URL)
			}
			return jsonResponse(`{"token":"dt-1","refresh_token":"dr-1","user_id":"user-1"}`), nil
		case strings.Contains(r.URL, "/me/jobToken"):
			if r.Headers.Get("Authorization") != "Bearer dt-1" {
				t.Errorf("job token auth = %q", r.Headers.Get("Authorization"))
			}
			var body map[string]string
			_ = json.Unmarshal(r.Body, &body)
			if body["clientId"] == "" {
				t.Error("job token request has no client id")
			}
			return jsonResponse(fmt.Sprintf(`{"token":"jt-1","refresh_token":"rt-1","expires_in":%d}`, int64(2*time.Hour/time.Millisecond))), nil
		case strings.Contains(r.URL, "/userinfo"):
			return jsonResponse(`{"email":"dev@example.test","name":"Dev","id":"user-1","security_mobile":"13800138000"}`), nil
		default:
			t.Errorf("unexpected url %s", r.URL)
			return jsonResponse(`{}`), nil
		}
	}}
	dir := t.TempDir()
	start, err := startLogin(pluginapi.AuthLoginStartRequest{Provider: siteQoder, Host: pluginapi.HostConfigSummary{AuthDir: dir}}, sender)
	if err != nil {
		t.Fatal(err)
	}
	poll, err := pollLogin(pluginapi.AuthLoginPollRequest{Provider: siteQoder, State: start.State, Host: pluginapi.HostConfigSummary{AuthDir: dir}}, sender)
	if err != nil {
		t.Fatal(err)
	}
	if poll.Status != pluginapi.AuthLoginStatusSuccess {
		t.Fatalf("status = %v (%s)", poll.Status, poll.Message)
	}
	c, err := readCredential(poll.Auth.StorageJSON)
	if err != nil {
		t.Fatal(err)
	}
	if c.Site != siteQoder || c.Access != "jt-1" || c.Refresh != "rt-1" || c.DeviceToken != "dt-1" || c.UID != "user-1" || c.DeviceChat {
		t.Fatalf("credential = %+v", c)
	}
	if poll.Auth.Metadata["oauth_provider"] != siteQoder || poll.Auth.Metadata["type"] != credentialProvider {
		t.Fatalf("metadata = %v", poll.Auth.Metadata)
	}
	if poll.Auth.Label != "dev@example.test" || !strings.HasPrefix(poll.Auth.ID, "qoder-") {
		t.Fatalf("label/id = %q %q", poll.Auth.Label, poll.Auth.ID)
	}
	if poll.Auth.NextRefreshAfter.Before(time.Now().Add(90 * time.Minute)) {
		t.Fatalf("next refresh = %v", poll.Auth.NextRefreshAfter)
	}
}

func TestPollLoginCNFallsBackToDeviceToken(t *testing.T) {
	sender := &fakeSender{reply: func(r pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		switch {
		case strings.Contains(r.URL, "/deviceToken/poll"):
			return jsonResponse(`{"token":"dt-cn","refresh_token":"dr-cn","expires_at":"2099-01-02T03:04:05Z","user_id":"cn-user"}`), nil
		case strings.Contains(r.URL, "/me/jobToken"):
			return pluginapi.HTTPResponse{StatusCode: 403, Body: []byte(`{"message":"no job token"}`)}, nil
		default:
			return jsonResponse(`{"email":"phone_1234abcd@phone.local","name":"13800138000","security_mobile":"13800138000"}`), nil
		}
	}}
	dir := t.TempDir()
	start, _ := startLogin(pluginapi.AuthLoginStartRequest{Provider: siteQoderCN, Host: pluginapi.HostConfigSummary{AuthDir: dir}}, sender)
	poll, err := pollLogin(pluginapi.AuthLoginPollRequest{Provider: siteQoderCN, State: start.State, Host: pluginapi.HostConfigSummary{AuthDir: dir}}, sender)
	if err != nil {
		t.Fatal(err)
	}
	if poll.Status != pluginapi.AuthLoginStatusSuccess {
		t.Fatalf("status = %v (%s)", poll.Status, poll.Message)
	}
	c, _ := readCredential(poll.Auth.StorageJSON)
	if !c.DeviceChat || c.chatToken() != "dt-cn" {
		t.Fatalf("credential = %+v", c)
	}
	if poll.Auth.Label != "138****8000" {
		t.Fatalf("label = %q", poll.Auth.Label)
	}
}

func TestPollLoginStaysPendingBeforeAuthorization(t *testing.T) {
	sender := &fakeSender{reply: func(pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		return pluginapi.HTTPResponse{StatusCode: 404, Body: []byte(`{}`)}, nil
	}}
	dir := t.TempDir()
	start, _ := startLogin(pluginapi.AuthLoginStartRequest{Provider: siteQoder, Host: pluginapi.HostConfigSummary{AuthDir: dir}}, sender)
	poll, err := pollLogin(pluginapi.AuthLoginPollRequest{Provider: siteQoder, State: start.State, Host: pluginapi.HostConfigSummary{AuthDir: dir}}, sender)
	if err != nil || poll.Status != pluginapi.AuthLoginStatusPending {
		t.Fatalf("poll = %v, %v", poll, err)
	}
}

func TestRefreshSpendsOneRefreshTokenOnce(t *testing.T) {
	var mu sync.Mutex
	upstream := 0
	sender := &fakeSender{reply: func(r pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		mu.Lock()
		upstream++
		mu.Unlock()
		time.Sleep(20 * time.Millisecond)
		return jsonResponse(`{"token":"jt-new","refresh_token":"rt-new","expires_at":"2099-01-01T00:00:00Z"}`), nil
	}}
	req := pluginapi.AuthRefreshRequest{AuthID: "qoder-test.json", StorageJSON: testCredential(siteQoder)}
	var wg sync.WaitGroup
	results := make([]pluginapi.AuthRefreshResponse, 2)
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = refreshCredential(req, sender)
		}(i)
	}
	wg.Wait()
	if errs[0] != nil || errs[1] != nil {
		t.Fatalf("errors = %v %v", errs[0], errs[1])
	}
	if upstream != 1 {
		t.Fatalf("upstream refreshes = %d, want 1", upstream)
	}
	c, _ := readCredential(results[0].Auth.StorageJSON)
	if c.Access != "jt-new" || c.Refresh != "rt-new" || c.DeviceToken != "dt-test-token" {
		t.Fatalf("credential = %+v", c)
	}
}

func TestRefreshRejectedNeedsSignIn(t *testing.T) {
	sender := &fakeSender{reply: func(pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		return pluginapi.HTTPResponse{StatusCode: 401, Body: []byte(`{"message":"expired"}`)}, nil
	}}
	_, err := refreshCredential(pluginapi.AuthRefreshRequest{AuthID: "a", StorageJSON: testCredential(siteQoder)}, sender)
	if err == nil || !strings.Contains(err.Error(), "sign in again") {
		t.Fatalf("err = %v", err)
	}
}

func TestClassifyUpstreamCodes(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   int
		code   string
	}{
		{403, `{"code":10605,"message":"queued"}`, 503, "10605"},
		{200, `{"code":113,"message":"no credits"}`, 429, "113"},
		{400, `{"code":406,"message":"sensitive"}`, 400, "406"},
		{403, `{"code":105,"message":"Login expired"}`, 401, "105"},
		{401, `{"message":"unauthorized"}`, 401, ""},
		{400, `{"message":"Error in upstream response"}`, 502, ""},
		{500, `{"message":"boom"}`, 500, ""},
	}
	for _, c := range cases {
		got := classifyUpstream(c.status, []byte(c.body))
		if got.Status != c.want || (c.code != "" && got.Code != c.code) {
			t.Errorf("classify(%d, %s) = %+v", c.status, c.body, got)
		}
	}
	queue := classifyUpstream(403, []byte(`{"code":10605,"message":"queued","queue":{"isQueued":true,"retryAfterSeconds":7,"modelKey":"fast"}}`))
	if queue.RetryAfter != 7 || !queue.Queue {
		t.Fatalf("queue = %+v", queue)
	}
}

func TestModelListingNormalizesEnabledModels(t *testing.T) {
	listing := `{"app":[
		{"key":"auto","enable":true},
		{"key":"Ultimate","display_name":"Ultimate","enable":true,"is_vl":true,"max_input_tokens":1000000,"thinking_config":{"disabled":{},"enabled":{"efforts":{"high":{"is_default":true},"low":{}}}},"context_config":{"1M":{"token_count":1000000},"200K":{"token_count":200000,"is_default":true}},"price_factor":2},
		{"key":"off","enable":false},
		{"key":"lite","enable":true,"display_name":"Lite"}
	],"chat":[{"key":"legacy","enable":true,"display_name":"Legacy"}]}`
	entries, err := parseListing([]byte(listing))
	if err != nil {
		t.Fatal(err)
	}
	models := usableModels(entries)
	if len(models) != 3 {
		t.Fatalf("models = %d: %+v", len(models), models)
	}
	var ultimate qoderModel
	for _, m := range models {
		if m.Key == "Ultimate" {
			ultimate = m
		}
	}
	if ultimate.Key != "Ultimate" || ultimate.Context != 1_000_000 || ultimate.DefaultWindow != 200_000 {
		t.Fatalf("ultimate = %+v", ultimate)
	}
	if ultimate.DefaultEffort != "high" || len(ultimate.Efforts) != 2 {
		t.Fatalf("efforts = %+v", ultimate)
	}
	info := modelInfo(ultimate)
	if info.ID != "ultimate" || info.ContextLength != 1_000_000 || info.Thinking == nil {
		t.Fatalf("info = %+v", info)
	}
	if info.Thinking.Levels[0] != "none" {
		t.Fatalf("levels = %v", info.Thinking.Levels)
	}
}

func TestExecuteNonStreamAnswer(t *testing.T) {
	sender := &fakeSender{reply: func(r pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		switch {
		case strings.Contains(r.URL, "/model/list"):
			return jsonResponse(`{"app":[{"key":"ultimate","display_name":"Ultimate","enable":true,"max_input_tokens":1000000}]}`), nil
		case strings.Contains(r.URL, "agent_chat_generation"):
			var body map[string]any
			if err := json.Unmarshal([]byte(decodeMust(t, string(r.Body))), &body); err != nil {
				t.Fatalf("wire body: %v", err)
			}
			if body["session_id"] == "" || body["model_config"] == nil {
				t.Fatalf("body = %v", body)
			}
			inner := `{"choices":[{"delta":{"content":"Hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`
			return pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(sseOf(inner))}, nil
		default:
			t.Errorf("unexpected url %s", r.URL)
			return jsonResponse(`{}`), nil
		}
	}}
	payload := []byte(`{"model":"ultimate","messages":[{"role":"user","content":"hi"}]}`)
	resp, err := execute(pluginapi.ExecutorRequest{Model: "ultimate", Payload: payload, StorageJSON: testCredential(siteQoder), AuthID: "a"}, sender)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Object  string `json:"object"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			Finish string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			Total int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(resp.Payload, &out); err != nil {
		t.Fatal(err)
	}
	if out.Object != "chat.completion" || out.Choices[0].Message.Content != "Hello" || out.Choices[0].Finish != "stop" || out.Usage.Total != 5 {
		t.Fatalf("answer = %s", resp.Payload)
	}
}

func TestExecuteMapsUpstreamRefusal(t *testing.T) {
	sender := &fakeSender{reply: func(r pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		if strings.Contains(r.URL, "/model/list") {
			return jsonResponse(`{"app":[{"key":"ultimate","enable":true}]}`), nil
		}
		return pluginapi.HTTPResponse{StatusCode: 403, Body: []byte(`{"code":10605,"message":"queued","queue":{"isQueued":true,"retryAfterSeconds":9}}`)}, nil
	}}
	_, err := execute(pluginapi.ExecutorRequest{Model: "ultimate", Payload: []byte(`{"model":"ultimate","messages":[{"role":"user","content":"hi"}]}`), StorageJSON: testCredential(siteQoder), AuthID: "a"}, sender)
	if err == nil || !strings.Contains(err.Error(), "try again") {
		t.Fatalf("err = %v", err)
	}
}

func TestStreamPumpEmitsOpenAIFrames(t *testing.T) {
	var frames [][]byte
	origRead, origEmit, origOpen := readStreamChunk, emitStreamFrame, openHTTPStream
	sent := false
	readStreamChunk = func(id string) (hostReadResponse, error) {
		if id == "upstream-1" && !sent {
			sent = true
			return hostReadResponse{Payload: []byte(sseOf(`{"choices":[{"delta":{"content":"Hi"}}]}`)), Done: false}, nil
		}
		return hostReadResponse{Done: true}, nil
	}
	emitStreamFrame = func(id string, payload []byte) error {
		frames = append(frames, append([]byte(nil), payload...))
		return nil
	}
	openHTTPStream = func(plan requestPlan, callbackID string) (hostStreamResponse, error) {
		return hostStreamResponse{StatusCode: 200, StreamID: "upstream-1"}, nil
	}
	t.Cleanup(func() { readStreamChunk, emitStreamFrame, openHTTPStream = origRead, origEmit, origOpen })

	sender := &fakeSender{reply: func(r pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		return jsonResponse(`{"app":[{"key":"ultimate","enable":true}]}`), nil
	}}
	_, err := executeStream(pluginapi.ExecutorRequest{Model: "ultimate", Payload: []byte(`{"model":"ultimate","messages":[{"role":"user","content":"hi"}]}`), StorageJSON: testCredential(siteQoder), AuthID: "a"}, "", "target", sender)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for len(frames) < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if len(frames) < 3 {
		t.Fatalf("frames = %d", len(frames))
	}
	joined := string(bytes.Join(frames, nil))
	if !strings.Contains(joined, `"content":"Hi"`) || !strings.Contains(joined, "data: [DONE]") {
		t.Fatalf("frames = %s", joined)
	}
}

func TestResourcesArePassive(t *testing.T) {
	for _, path := range []string{resourcePrefix + "/qoder-color.svg", resourcePrefix + "/qoder.svg", omcResourcePath} {
		resp, _ := handleManagementResource(pluginapi.ManagementRequest{Method: "GET", Path: path})
		if resp.StatusCode != 200 || len(resp.Body) == 0 {
			t.Fatalf("%s: %+v", path, resp)
		}
		bad, _ := handleManagementResource(pluginapi.ManagementRequest{Method: "POST", Path: path})
		if bad.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("%s accepted POST", path)
		}
	}
	for _, path := range []string{resourcePrefix + "/other.svg", resourcePrefix + "/../omc.json", resourcePrefix + "/qoder.svg/", omcResourcePath + "?x=1"} {
		resp, _ := handleManagementResource(pluginapi.ManagementRequest{Method: "GET", Path: path})
		if resp.StatusCode != 404 {
			t.Fatalf("%s = %d", path, resp.StatusCode)
		}
	}
}

func TestConfigValidation(t *testing.T) {
	t.Cleanup(func() { _ = configure(nil) })
	for _, bad := range []string{"site: nowhere", "max_concurrency: 0", "max_concurrency: 101", "cosy_version: ''", "max_tokens: 0"} {
		if err := configure([]byte(bad)); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
	if err := configure([]byte("site: qoder-cn\nmax_concurrency: 2")); err != nil {
		t.Fatal(err)
	}
	if cfg := getConfig(); cfg.Site != siteQoderCN || cfg.MaxConcurrency != 2 || cfg.CosyVersion != defaultCosyVersion || cfg.MaxTokens != 32000 {
		t.Fatalf("config = %+v", cfg)
	}
}

func TestEmbeddedLogosMatchPublishedAssets(t *testing.T) {
	if len(qoderLogoColorSVG) == 0 || len(qoderLogoMonoSVG) == 0 {
		t.Fatal("embedded logos are empty")
	}
	if bytes.Equal(qoderLogoColorSVG, qoderLogoMonoSVG) {
		t.Fatal("the two login entries must not share one logo")
	}
}

func jsonResponse(body string) pluginapi.HTTPResponse {
	return pluginapi.HTTPResponse{StatusCode: 200, Headers: http.Header{"Content-Type": {"application/json"}}, Body: []byte(body)}
}

func sseOf(inner string) string {
	return "data: " + mustJSON(map[string]any{"statusCodeValue": 200, "body": inner}) + "\n\n"
}

func mustJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

func filepathJoin(t *testing.T) string { return t.TempDir() }

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func decodeMust(t *testing.T, wire string) []byte {
	t.Helper()
	raw, err := decodeBody(wire)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
