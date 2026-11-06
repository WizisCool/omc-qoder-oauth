package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func msg(t *testing.T, raw string) chatMessage {
	t.Helper()
	var m chatMessage
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("fixture %s: %v", raw, err)
	}
	return m
}

func TestSessionIDIsStablePerConversation(t *testing.T) {
	chat, _ := parseChatRequest([]byte(`{"messages":[{"role":"system","content":"sys"},{"role":"user","content":"hello"}]}`))
	first := sessionID(chat, "")
	second := sessionID(chat, "")
	if first != second || len(first) != 36 || strings.Count(first, "-") != 4 {
		t.Fatalf("session = %q / %q", first, second)
	}
	other, _ := parseChatRequest([]byte(`{"messages":[{"role":"user","content":"goodbye"}]}`))
	if sessionID(other, "") == first {
		t.Fatal("different conversations share a session")
	}
	if got := sessionID(chat, "agent-session"); got == first {
		t.Fatal("a named session was ignored")
	}
	keyed, _ := parseChatRequest([]byte(`{"prompt_cache_key":"thread-1","messages":[{"role":"user","content":"hello"}]}`))
	if sessionID(keyed, "") == first {
		t.Fatal("prompt_cache_key was ignored")
	}
}

func TestQoderMessagesPairToolResults(t *testing.T) {
	chat, err := parseChatRequest([]byte(`{"messages":[
		{"role":"system","content":"be brief"},
		{"role":"user","content":"list files"},
		{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"ls","arguments":"{\"path\":\".\"}"}}]},
		{"role":"tool","tool_call_id":"call_1","content":"a.go b.go"},
		{"role":"assistant","content":"there are two files"}
	]}`))
	if err != nil {
		t.Fatal(err)
	}
	out := qoderMessages(chat.Messages)
	if len(out) != 4 {
		t.Fatalf("turns = %d: %v", len(out), out)
	}
	if out[0]["role"] != "user" || out[1]["role"] != "assistant" || out[2]["role"] != "tool" || out[3]["role"] != "assistant" {
		t.Fatalf("roles = %v", out)
	}
	calls, _ := out[1]["tool_calls"].([]map[string]any)
	if len(calls) != 1 || calls[0]["id"] != "call_1" {
		t.Fatalf("calls = %v", calls)
	}
	fn, _ := calls[0]["function"].(map[string]any)
	if fn["arguments"] != `{"path":"."}` {
		t.Fatalf("arguments = %v", fn["arguments"])
	}
	if out[2]["tool_call_id"] != "call_1" || out[2]["content"] != "a.go b.go" {
		t.Fatalf("tool turn = %v", out[2])
	}
}

func TestQoderMessagesPairLostToolIDs(t *testing.T) {
	chat, _ := parseChatRequest([]byte(`{"messages":[
		{"role":"user","content":"run"},
		{"role":"assistant","content":"","tool_calls":[{"function":{"name":"run","arguments":"{}"}},{"function":{"name":"read","arguments":"{}"}}]},
		{"role":"tool","content":"one"},
		{"role":"tool","content":"two"}
	]}`))
	out := qoderMessages(chat.Messages)
	assistant, _ := out[1]["tool_calls"].([]map[string]any)
	if len(assistant) != 2 {
		t.Fatalf("calls = %v", assistant)
	}
	firstID := assistant[0]["id"].(string)
	secondID := assistant[1]["id"].(string)
	if firstID == "" || firstID == secondID {
		t.Fatalf("generated ids = %q %q", firstID, secondID)
	}
	if out[2]["tool_call_id"] != firstID || out[3]["tool_call_id"] != secondID {
		t.Fatalf("results = %v %v", out[2], out[3])
	}
}

func TestQoderMessagesMoveOrphanResultToUserText(t *testing.T) {
	chat, _ := parseChatRequest([]byte(`{"messages":[
		{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"ls","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"call_1","content":"ok"},
		{"role":"tool","tool_call_id":"call_missing","content":"orphan text"},
		{"role":"user","content":"next"}
	]}`))
	out := qoderMessages(chat.Messages)
	if out[len(out)-1]["role"] != "user" {
		t.Fatalf("turns = %v", out)
	}
	blocks, _ := out[len(out)-1]["content"].([]map[string]any)
	if len(blocks) < 2 || blocks[0]["text"] != "orphan text" {
		t.Fatalf("orphan text = %v", blocks)
	}
}

func TestBuildQoderBodyCarriesClientAndProtocol(t *testing.T) {
	raw := `{
		"model":"ultimate",
		"messages":[{"role":"system","content":"be brief"},{"role":"user","content":"hi"}],
		"tools":[{"type":"function","function":{"name":"shell","description":"run","parameters":{"type":"object","properties":{"cmd":{"type":"string"}}}}}],
		"max_tokens":2048,
		"reasoning_effort":"high"
	}`
	chat, err := parseChatRequest([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	m := qoderModel{Key: "ultimate", Name: "Ultimate", Format: "openai", Source: "system", Context: 1_000_000, MaxInput: 1_000_000, Thinks: true, Efforts: []string{"low", "medium", "high", "xhigh", "max"}, DefaultEffort: "medium"}
	session := sessionID(chat, "")
	task := qoderTasks.begin(session, chat, 1700000000000)
	body, err := buildQoderBody(chat, m, session, task, "1.1.69", 32000)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["session_id"] != session || doc["request_id"] == "" || doc["request_set_id"] != task.ID {
		t.Fatalf("identity = %v", doc)
	}
	params, _ := doc["parameters"].(map[string]any)
	if params["max_tokens"] != float64(2048) || params["enable_thinking"] != true || params["reasoning_effort"] != "high" {
		t.Fatalf("parameters = %v", params)
	}
	if params["context_length"] != float64(1_000_000) {
		t.Fatalf("context_length = %v", params)
	}
	sys, _ := doc["system"].([]any)
	if len(sys) != 1 {
		t.Fatalf("system = %v", sys)
	}
	text := sys[0].(map[string]any)["text"].(string)
	if !strings.HasPrefix(text, qoderSystemPrompt) || !strings.Contains(text, "be brief") {
		t.Fatalf("system text = %q", text)
	}
	business, _ := doc["business"].(map[string]any)
	if business["product"] != "app" || business["version"] != "1.1.69" || business["model_key"] != "ultimate" || business["stage"] != "start" {
		t.Fatalf("business = %v", business)
	}
	cfg, _ := doc["model_config"].(map[string]any)
	if cfg["key"] != "ultimate" || cfg["format"] != "openai" || cfg["is_reasoning"] != true {
		t.Fatalf("model_config = %v", cfg)
	}
	tools, _ := doc["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %v", tools)
	}
	messages, _ := doc["messages"].([]any)
	if len(messages) != 2 || messages[0].(map[string]any)["role"] != "system" {
		t.Fatalf("messages = %v", messages)
	}
	last := messages[len(messages)-1].(map[string]any)
	blocks, _ := last["content"].([]any)
	lastBlock := blocks[len(blocks)-1].(map[string]any)
	if lastBlock["cache_control"] == nil {
		t.Fatalf("last user block is not cache marked: %v", lastBlock)
	}
	if last["contents"] == nil {
		t.Fatalf("user turn has no contents: %v", last)
	}
}

func TestBuildQoderBodyToolChoiceNoneDropsTools(t *testing.T) {
	chat, _ := parseChatRequest([]byte(`{"model":"fast","tool_choice":"none","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"shell"}}]}`))
	body, err := buildQoderBody(chat, qoderModel{Key: "fast", Name: "Fast", Format: "openai"}, sessionID(chat, ""), nil, "1.1.69", 32000)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	_ = json.Unmarshal(body, &doc)
	if _, ok := doc["tools"]; ok {
		t.Fatal("tool_choice none still sent tools")
	}
}

func TestBuildQoderBodyRejectsAuto(t *testing.T) {
	chat, _ := parseChatRequest([]byte(`{"messages":[{"role":"user","content":"hi"}]}`))
	if _, err := buildQoderBody(chat, qoderModel{Key: "auto"}, "", nil, "1.1.69", 32000); err == nil {
		t.Fatal("auto was accepted as a model")
	}
}

func TestEffortFor(t *testing.T) {
	thinking := qoderModel{Thinks: true, Efforts: []string{"low", "medium", "high"}, DefaultEffort: "medium"}
	always := qoderModel{Thinks: true, AlwaysThinks: true, Efforts: []string{"low", "high"}}
	cases := []struct {
		name   string
		asked  string
		m      qoderModel
		budget int
		wantOn bool
		want   string
	}{
		{"model default", "", thinking, 4096, true, "medium"},
		{"small budget disables", "", thinking, 512, false, ""},
		{"explicit low", "low", thinking, 4096, true, "low"},
		{"unknown falls back", "ultra", thinking, 4096, true, "medium"},
		{"always thinks at lowest", "none", always, 4096, true, "low"},
		{"no thinking support", "high", qoderModel{}, 4096, false, ""},
	}
	for _, c := range cases {
		on, effort := effortFor(c.asked, c.m, c.budget)
		if on != c.wantOn || effort != c.want {
			t.Errorf("%s: got %v %q", c.name, on, effort)
		}
	}
}

func TestTaskStoreReusesTheToolLoopTask(t *testing.T) {
	store := &taskStore{tasks: map[string]*qoderTask{}, used: map[string]int64{}}
	chat, _ := parseChatRequest([]byte(`{"messages":[{"role":"user","content":"long task"}]}`))
	first := store.begin("session-1", chat, 1000)
	toolLoop, _ := parseChatRequest([]byte(`{"messages":[{"role":"user","content":"long task"},{"role":"assistant","content":"","tool_calls":[{"id":"c1","function":{"name":"ls","arguments":"{}"}}]},{"role":"tool","tool_call_id":"c1","content":"ok"}]}`))
	second := store.begin("session-1", toolLoop, 2000)
	if first.ID != second.ID {
		t.Fatalf("tool loop started a new task: %s != %s", first.ID, second.ID)
	}
	other, _ := parseChatRequest([]byte(`{"messages":[{"role":"user","content":"another task"}]}`))
	if store.begin("session-1", other, 3000).ID == first.ID {
		t.Fatal("a new prompt reused the task")
	}
}
