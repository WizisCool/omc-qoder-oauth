package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
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

// A refresh replaces the auth record CPA persists, and its merge treats a field
// the refresh omits as deleted. The host-managed metadata must therefore
// survive a token rotation, or the note (and every other management field)
// disappears from the credential.
func TestRefreshPreservesManagementFields(t *testing.T) {
	sender := &fakeSender{reply: func(pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		return jsonResponse(`{"token":"jt-new","refresh_token":"rt-new","expires_at":"2099-01-01T00:00:00Z"}`), nil
	}}
	metadata := map[string]any{
		"note":            "work account",
		"prefix":          "q",
		"priority":        7,
		"weight":          3,
		"proxy_url":       "http://proxy.test:8080",
		"disable_cooling": true,
		"websockets":      false,
		"using_api":       true,
		"expired":         "2030-01-01T00:00:00Z",
		"request_retry":   2,
		"headers":         map[string]any{"X-Test": "1"},
		"excluded_models": []any{"m1"},
		"model_aliases":   []any{map[string]any{"name": "m", "alias": "a"}},
	}
	resp, err := refreshCredential(pluginapi.AuthRefreshRequest{AuthID: "qoder-x.json", StorageJSON: testCredential(siteQoderCN), Metadata: metadata}, sender)
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range metadata {
		if got := resp.Auth.Metadata[key]; !reflect.DeepEqual(got, want) {
			t.Fatalf("metadata[%q] = %#v, want %#v", key, got, want)
		}
	}
	if got := resp.Auth.Metadata["type"]; got != credentialProvider {
		t.Fatalf("type = %#v", got)
	}
	if _, ok := resp.Auth.Metadata["access"]; ok {
		t.Fatal("refresh echoed the credential into metadata")
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
		got := classifyUpstream(c.status, nil, []byte(c.body))
		if got.Status != c.want || (c.code != "" && got.Code != c.code) {
			t.Errorf("classify(%d, %s) = %+v", c.status, c.body, got)
		}
	}
	queue := classifyUpstream(403, nil, []byte(`{"code":10605,"message":"queued","queue":{"isQueued":true,"retryAfterSeconds":7,"modelKey":"fast"}}`))
	if queue.RetryAfter != 7 || !queue.Queue {
		t.Fatalf("queue = %+v", queue)
	}
}

// TestQuotaRefusalsAreCredentialScoped pins which refusal CPA may cool for the
// whole credential: Qoder's credit pools are account-level, so a quota code or
// quota wording stops every model until the window ends. A bare 429 and the queue
// notice stay model-scoped.
func TestQuotaRefusalsAreCredentialScoped(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"quota code", 200, `{"code":113,"message":"no credits"}`, true},
		{"quota wording", 429, `{"message":"quota exceeded"}`, true},
		{"bare rate limit", 429, `{"message":"too many requests"}`, false},
		{"queue notice", 403, `{"code":10605,"message":"queued"}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := classifyUpstream(tc.status, nil, []byte(tc.body))
			if f.CredentialScoped != tc.want {
				t.Fatalf("failure = %+v", f)
			}
			var perr *pluginabi.Error
			if err := decisionError(f); !errors.As(err, &perr) || perr.CredentialScoped != tc.want {
				t.Fatalf("decisionError = %#v", err)
			}
		})
	}
}

// TestAMidStreamRefusalClosesWithItsScope pins the host bridge contract for a
// refusal that ends an answered stream: it must carry its status and credential
// scope, or CPA answers it as a statusless 500 and cools only the model.
func TestAMidStreamRefusalClosesWithItsScope(t *testing.T) {
	previous := closeHostStreamWith
	t.Cleanup(func() { closeHostStreamWith = previous })
	closed := make(chan *pluginabi.Error, 1)
	closeHostStreamWith = func(id, message string, err error) {
		var detail *pluginabi.Error
		errors.As(err, &detail)
		closed <- detail
	}
	p := &streamPump{target: "stream-out"}
	if p.deliver([]qoderEvent{{Err: &qoderFailure{Status: 429, Code: "113", Message: "no credits", CredentialScoped: true}}}) {
		t.Fatal("deliver returned true for a refusal")
	}
	select {
	case detail := <-closed:
		if detail == nil {
			t.Fatal("the stream was closed without an error_detail")
		}
		if detail.HTTPStatus != http.StatusTooManyRequests || !detail.CredentialScoped {
			t.Fatalf("detail = %d %s scoped=%v", detail.HTTPStatus, detail.Code, detail.CredentialScoped)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the stream was never closed")
	}
}

// TestQuotaRefusalCarriesTheRetryWindow pins what a quota refusal hands CPA: the
// Retry-After Qoder sends is the only statement of when the window reopens, and
// without it the credential is retried long before the allowance is back.
func TestQuotaRefusalCarriesTheRetryWindow(t *testing.T) {
	at := time.Now().Add(90 * time.Second).UTC().Format(http.TimeFormat)
	for _, tc := range []struct {
		name     string
		headers  http.Header
		body     string
		wantWait int
	}{
		{"seconds", http.Header{"Retry-After": {"120"}}, `{"code":113,"message":"no credits"}`, 120},
		{"http date", http.Header{"Retry-After": {at}}, `{"message":"quota exceeded"}`, 90},
		{"header outranks nothing else", nil, `{"code":113,"message":"no credits"}`, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyUpstream(429, tc.headers, []byte(tc.body))
			if got.Status != 429 {
				t.Fatalf("status = %d", got.Status)
			}
			if tc.wantWait == 0 {
				if got.RetryAfter != 0 {
					t.Fatalf("RetryAfter = %d, want none", got.RetryAfter)
				}
				return
			}
			if got.RetryAfter < tc.wantWait-2 || got.RetryAfter > tc.wantWait {
				t.Fatalf("RetryAfter = %d, want about %d", got.RetryAfter, tc.wantWait)
			}
		})
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
	spec, _ := siteFor(siteQoder)
	models := usableModels(entries, spec)
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
	var framesMu sync.Mutex
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
		framesMu.Lock()
		frames = append(frames, append([]byte(nil), payload...))
		framesMu.Unlock()
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
	count := func() int { framesMu.Lock(); defer framesMu.Unlock(); return len(frames) }
	for count() < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if count() < 3 {
		t.Fatalf("frames = %d", count())
	}
	framesMu.Lock()
	joined := string(bytes.Join(frames, nil))
	framesMu.Unlock()
	if !strings.Contains(joined, `"content":"Hi"`) || !strings.Contains(joined, "data: [DONE]") {
		t.Fatalf("frames = %s", joined)
	}
}

func TestResourcesArePassive(t *testing.T) {
	for _, path := range []string{resourcePrefix + "/" + pluginLogo, resourcePrefix + "/qoder-color.svg", resourcePrefix + "/qoder.svg", omcResourcePath} {
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
	if len(pluginLogoPNG) == 0 || len(qoderLogoColorSVG) == 0 || len(qoderLogoMonoSVG) == 0 {
		t.Fatal("embedded logos are empty")
	}
	if bytes.Equal(qoderLogoColorSVG, qoderLogoMonoSVG) {
		t.Fatal("the two login entries must not share one logo")
	}
	if bytes.HasPrefix(pluginLogoPNG, []byte("<svg")) {
		t.Fatal("the plugin logo must stay a raster sticker, not a provider mark")
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

func TestRefreshDeviceChatRotatesTheChatPair(t *testing.T) {
	sender := &fakeSender{reply: func(r pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		if !strings.Contains(r.URL, "/deviceToken/refresh") {
			t.Errorf("device-chat account refreshed via %s", r.URL)
		}
		var body map[string]string
		_ = json.Unmarshal(r.Body, &body)
		if body["refresh_token"] != "dr-1" {
			t.Errorf("refresh body = %v", body)
		}
		return jsonResponse(`{"token":"dt-2","refresh_token":"dr-2","expires_in":3600}`), nil
	}}
	c := credential{Site: siteQoderCN, Refresh: "dr-1", DeviceToken: "dt-1", DeviceRefresh: "dr-1", DeviceChat: true, UID: "u", MachineID: "m"}
	out, err := refreshCredential(pluginapi.AuthRefreshRequest{AuthID: "a", StorageJSON: storageOf(c)}, sender)
	if err != nil {
		t.Fatal(err)
	}
	next, err := readCredential(out.Auth.StorageJSON)
	if err != nil {
		t.Fatal(err)
	}
	if next.DeviceToken != "dt-2" || next.DeviceRefresh != "dr-2" || next.Access != "dt-2" || next.Refresh != "dr-2" || !next.DeviceChat {
		t.Fatalf("credential = %+v", next)
	}
	if next.chatToken() != "dt-2" {
		t.Fatalf("chat token = %q", next.chatToken())
	}
	if time.UnixMilli(next.Expires).Before(time.Now().Add(50 * time.Minute)) {
		t.Fatalf("expiry = %v", next.expiry())
	}
}

// A refused job pair is not a refused account: Qoder hands the same account a
// device pair, and that token signs the same calls. So a dead job pair spends the
// device pair before it asks the user to sign in again.
func TestRefreshFallsBackToDeviceTokenWhenJobPairIsRefused(t *testing.T) {
	c, err := readCredential(testCredential(siteQoder))
	if err != nil {
		t.Fatal(err)
	}
	c.DeviceRefresh = "drt-live"
	spec, _ := siteFor(c.Site)
	sender := &fakeSender{reply: func(r pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		switch {
		case strings.Contains(r.URL, "/api/v1/jobToken/refresh"):
			return pluginapi.HTTPResponse{StatusCode: 401, Body: []byte(`{"code":"105","message":"Login expired"}`)}, nil
		case strings.Contains(r.URL, "/api/v1/deviceToken/refresh"):
			return jsonResponse(`{"token":"dt-new","refresh_token":"drt-new","expires_in":3600}`), nil
		}
		t.Fatalf("unexpected url %s", r.URL)
		return jsonResponse(`{}`), nil
	}}
	next, err := renewOnce(sender, spec, c)
	if err != nil {
		t.Fatal(err)
	}
	if !next.DeviceChat || next.DeviceToken != "dt-new" || next.DeviceRefresh != "drt-new" {
		t.Fatalf("renewed = %+v", next)
	}
	if next.chatToken() != "dt-new" {
		t.Fatalf("chat token = %q", next.chatToken())
	}
	if next.Access != c.Access {
		t.Fatalf("job token rewritten: %q", next.Access)
	}
	if next.Expires <= time.Now().UnixMilli() {
		t.Fatalf("expiry = %d", next.Expires)
	}
}

// A refused job token costs one retry, not the answer.
func TestExecuteRetriesWithDeviceTokenWhenSignInLapsed(t *testing.T) {
	resetListingCache()
	chats := 0
	sender := &fakeSender{reply: func(r pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		switch {
		case strings.Contains(r.URL, "/model/list"):
			return jsonResponse(`{"app":[{"key":"lmodel","display_name":"QoDuck","enable":true}]}`), nil
		case strings.Contains(r.URL, "agent_chat_generation"):
			chats++
			if chats == 1 {
				return pluginapi.HTTPResponse{StatusCode: 403, Body: []byte(`{"code":"105","message":"Login expired"}`)}, nil
			}
			return pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(sseOf(`{"choices":[{"delta":{"content":"Hi"},"finish_reason":"stop"}]}`))}, nil
		}
		t.Fatalf("unexpected url %s", r.URL)
		return jsonResponse(`{}`), nil
	}}
	payload := []byte(`{"model":"qoduck","messages":[{"role":"user","content":"hi"}]}`)
	resp, err := execute(pluginapi.ExecutorRequest{Model: "qoduck", Payload: payload, StorageJSON: testCredential(siteQoder), AuthID: "a"}, sender)
	if err != nil {
		t.Fatal(err)
	}
	if chats != 2 {
		t.Fatalf("chat calls = %d", chats)
	}
	if !strings.Contains(string(resp.Payload), `"content":"Hi"`) {
		t.Fatalf("answer = %s", resp.Payload)
	}
}

// Only a lapsed token earns the extra call; another refusal is the answer.
func TestExecuteDoesNotRetryOtherRefusals(t *testing.T) {
	resetListingCache()
	chats := 0
	sender := &fakeSender{reply: func(r pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		if strings.Contains(r.URL, "/model/list") {
			return jsonResponse(`{"app":[{"key":"ultimate","enable":true}]}`), nil
		}
		chats++
		return pluginapi.HTTPResponse{StatusCode: 403, Body: []byte(`{"code":10605,"message":"queued","queue":{"isQueued":true,"retryAfterSeconds":9}}`)}, nil
	}}
	payload := []byte(`{"model":"ultimate","messages":[{"role":"user","content":"hi"}]}`)
	_, err := execute(pluginapi.ExecutorRequest{Model: "ultimate", Payload: payload, StorageJSON: testCredential(siteQoder), AuthID: "a"}, sender)
	if err == nil || !strings.Contains(err.Error(), "try again") {
		t.Fatalf("err = %v", err)
	}
	if chats != 1 {
		t.Fatalf("chat calls = %d", chats)
	}
}

// A lapsed token can arrive inside an OK stream instead of as a status; the
// stream is opened again with the device token rather than answered with 401.
func TestStreamRetriesWithDeviceTokenWhenSignInLapsed(t *testing.T) {
	resetListingCache()
	var frames []string
	var framesMu sync.Mutex
	origRead, origEmit, origOpen := readStreamChunk, emitStreamFrame, openHTTPStream
	t.Cleanup(func() { readStreamChunk, emitStreamFrame, openHTTPStream = origRead, origEmit, origOpen })
	sent := map[string]bool{}
	readStreamChunk = func(id string) (hostReadResponse, error) {
		if sent[id] {
			return hostReadResponse{Done: true}, nil
		}
		sent[id] = true
		if id == "refused" {
			return hostReadResponse{Payload: []byte(sseOf(`{"code":"105","message":"Login expired"}`))}, nil
		}
		return hostReadResponse{Payload: []byte(sseOf(`{"choices":[{"delta":{"content":"Hi"}}]}`))}, nil
	}
	emitStreamFrame = func(id string, payload []byte) error {
		framesMu.Lock()
		frames = append(frames, string(payload))
		framesMu.Unlock()
		return nil
	}
	opened := 0
	openHTTPStream = func(plan requestPlan, callbackID string) (hostStreamResponse, error) {
		opened++
		if opened == 1 {
			return hostStreamResponse{StatusCode: 200, StreamID: "refused"}, nil
		}
		return hostStreamResponse{StatusCode: 200, StreamID: "upstream-2"}, nil
	}
	sender := &fakeSender{reply: func(r pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		return jsonResponse(`{"app":[{"key":"ultimate","enable":true}]}`), nil
	}}
	payload := []byte(`{"model":"ultimate","messages":[{"role":"user","content":"hi"}]}`)
	if _, err := executeStream(pluginapi.ExecutorRequest{Model: "ultimate", Payload: payload, StorageJSON: testCredential(siteQoder), AuthID: "a"}, "", "target", sender); err != nil {
		t.Fatal(err)
	}
	if opened != 2 {
		t.Fatalf("stream opens = %d", opened)
	}
	deadline := time.Now().Add(2 * time.Second)
	joined := func() string {
		framesMu.Lock()
		defer framesMu.Unlock()
		return strings.Join(frames, "")
	}
	for !strings.Contains(joined(), `"content":"Hi"`) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !strings.Contains(joined(), `"content":"Hi"`) {
		t.Fatalf("frames = %s", joined())
	}
}
