package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// qoderSystemPrompt is Qoder's own system line, which comes before the
// client's system prompt as Qoder's client sends it.
const qoderSystemPrompt = "You are a Qoder agent. Use the instructions below and the tools available to you to assist the user."

type chatFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Arguments   json.RawMessage `json:"arguments,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type chatToolCall struct {
	ID       string       `json:"id,omitempty"`
	Type     string       `json:"type,omitempty"`
	Function chatFunction `json:"function"`
}

type chatTool struct {
	Type     string       `json:"type,omitempty"`
	Function chatFunction `json:"function"`
}

type chatMessage struct {
	Role             string          `json:"role"`
	Content          json.RawMessage `json:"content,omitempty"`
	Name             string          `json:"name,omitempty"`
	ToolCallID       string          `json:"tool_call_id,omitempty"`
	ToolCalls        []chatToolCall  `json:"tool_calls,omitempty"`
	ReasoningContent string          `json:"reasoning_content,omitempty"`
	Reasoning        string          `json:"reasoning,omitempty"`
}

// chatRequest is the OpenAI chat completion the host hands the executor.
type chatRequest struct {
	Model               string          `json:"model"`
	Stream              bool            `json:"stream,omitempty"`
	Messages            []chatMessage   `json:"messages"`
	Tools               []chatTool      `json:"tools,omitempty"`
	ToolChoice          json.RawMessage `json:"tool_choice,omitempty"`
	MaxTokens           int             `json:"max_tokens,omitempty"`
	MaxCompletionTokens int             `json:"max_completion_tokens,omitempty"`
	ReasoningEffort     string          `json:"reasoning_effort,omitempty"`
	PromptCacheKey      string          `json:"prompt_cache_key,omitempty"`
	User                string          `json:"user,omitempty"`
}

func parseChatRequest(raw []byte) (chatRequest, error) {
	var chat chatRequest
	if err := json.Unmarshal(raw, &chat); err != nil {
		return chat, errors.New("invalid chat completion request")
	}
	if len(chat.Messages) == 0 {
		return chat, errors.New("messages array required")
	}
	return chat, nil
}

func toolChoiceName(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s)
	}
	return ""
}

// sessionID is the stable session a conversation goes out under: the session
// the host names, else the client's prompt_cache_key, else its user, else a
// hash of the conversation's opening.
func sessionID(chat chatRequest, named string) string {
	seed := ""
	switch {
	case strings.TrimSpace(named) != "":
		seed = "session\x00" + strings.TrimSpace(named)
	case strings.TrimSpace(chat.PromptCacheKey) != "":
		seed = "key\x00" + strings.TrimSpace(chat.PromptCacheKey)
	case strings.TrimSpace(chat.User) != "":
		seed = "user\x00" + strings.TrimSpace(chat.User)
	default:
		var systems []string
		var first any
		for _, m := range chat.Messages {
			if m.Role == "system" || m.Role == "developer" {
				systems = append(systems, textOf(m.Content))
				continue
			}
			if first == nil && m.Role == "user" {
				first = m.Content
			}
		}
		raw, _ := json.Marshal([]any{systems, first})
		seed = "chat\x00" + string(raw)
	}
	sum := sha256.Sum256([]byte(seed))
	b := sum[:16]
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return uuidString(b)
}

// qoderTask is what Qoder's client sends under one request_set_id and one
// business record: every model call answering one user prompt, the tool loop's
// turns included.
type qoderTask struct {
	ID       string
	Begin    int64
	Answered bool
	Name     string
}

// taskStore keeps at most taskMax tasks for taskIdle after their last call.
type taskStore struct {
	mu    sync.Mutex
	tasks map[string]*qoderTask
	used  map[string]int64
}

const (
	taskMax  = 512
	taskIdle = 2 * time.Hour
)

var qoderTasks = &taskStore{tasks: map[string]*qoderTask{}, used: map[string]int64{}}

// begin is the task of chat's latest user turn in session.
func (s *taskStore) begin(session string, chat chatRequest, now int64) *qoderTask {
	at := -1
	for i := len(chat.Messages) - 1; i >= 0; i-- {
		if chat.Messages[i].Role == "user" {
			at = i
			break
		}
	}
	var content any
	var contentRaw json.RawMessage
	if at >= 0 {
		contentRaw = chat.Messages[at].Content
		content = contentRaw
	}
	raw, _ := json.Marshal([]any{session, at, content})
	sum := sha256.Sum256(raw)
	key := hex.EncodeToString(sum[:12])

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tasks == nil {
		s.tasks = map[string]*qoderTask{}
		s.used = map[string]int64{}
	}
	for k, t := range s.used {
		if now-t > int64(taskIdle/time.Millisecond) {
			delete(s.used, k)
			delete(s.tasks, k)
		}
	}
	t := s.tasks[key]
	if t == nil {
		id, err := uuidText()
		if err != nil || id == "" {
			id = fmt.Sprintf("%s-%d", key[:8], now)
		}
		t = &qoderTask{ID: id, Begin: now, Name: taskName(textOf(contentRaw))}
		s.tasks[key] = t
	}
	s.used[key] = now
	for k := range s.tasks {
		if len(s.tasks) <= taskMax {
			break
		}
		if k != key {
			delete(s.tasks, k)
			delete(s.used, k)
		}
	}
	return t
}

// markAnswered moves a task past its first answer, as Qoder's client does.
func (t *qoderTask) markAnswered(int64) {
	if t != nil {
		t.Answered = true
	}
}

func taskName(prompt string) string {
	r := []rune(strings.TrimSpace(prompt))
	if len(r) > 10 {
		r = r[:10]
	}
	return string(r)
}

// textOf is a message's text content.
func textOf(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []map[string]any
	if json.Unmarshal(raw, &blocks) == nil {
		var b strings.Builder
		for _, block := range blocks {
			if block["type"] == "text" {
				if t, ok := block["text"].(string); ok {
					b.WriteString(t)
				}
			}
		}
		return b.String()
	}
	return ""
}

// contentBlocks is a message's content as Qoder takes it: text and image blocks.
func contentBlocks(raw json.RawMessage) []map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if s == "" {
			return nil
		}
		return []map[string]any{{"type": "text", "text": s}}
	}
	var in []map[string]any
	if json.Unmarshal(raw, &in) != nil {
		return nil
	}
	var out []map[string]any
	for _, p := range in {
		switch p["type"] {
		case "text":
			if t, ok := p["text"].(string); ok {
				out = append(out, map[string]any{"type": "text", "text": t})
			}
		case "image_url":
			if url := imageURL(p); url != "" {
				out = append(out, map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}})
			}
		}
	}
	return out
}

func imageURL(p map[string]any) string {
	switch v := p["image_url"].(type) {
	case string:
		return v
	case map[string]any:
		if s, ok := v["url"].(string); ok {
			return s
		}
	}
	return ""
}

// reasoningOf is the thinking an assistant turn carries back.
func reasoningOf(m chatMessage) string {
	for _, s := range []string{m.ReasoningContent, m.Reasoning} {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

// qoderMessages is the chat's turns as Qoder takes them. A tool result whose
// id the client lost answers the earliest call of its turn still open, so the
// history Qoder sees stays valid.
func qoderMessages(msgs []chatMessage) []map[string]any {
	var out []map[string]any
	var seen []map[string]any
	flushSeen := func() {
		if len(seen) > 0 {
			out = append(out, map[string]any{"role": "user", "content": seen})
			seen = nil
		}
	}
	var open []string
	for i, m := range msgs {
		switch m.Role {
		case "system", "developer":
			continue
		case "user":
			open = nil
		case "assistant":
			open = nil
		case "tool":
			// handled below
		default:
			flushSeen()
		}
		if m.Role == "tool" {
			text := textOf(m.Content)
			named := strings.TrimSpace(m.ToolCallID)
			pick := -1
			for k, id := range open {
				if named != "" && id == named {
					pick = k
					break
				}
			}
			if pick < 0 && named == "" && len(open) > 0 {
				pick = 0
			}
			if pick < 0 {
				if text != "" {
					if len(seen) > 0 {
						seen = append(seen, map[string]any{"type": "text", "text": "\n\n"})
					}
					seen = append(seen, map[string]any{"type": "text", "text": text})
				}
				continue
			}
			id := open[pick]
			open = append(open[:pick], open[pick+1:]...)
			out = append(out, map[string]any{"role": "tool", "tool_call_id": id, "content": text})
			continue
		}
		if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			flushSeen()
			calls := make([]map[string]any, 0, len(m.ToolCalls))
			open = open[:0]
			for k, c := range m.ToolCalls {
				id := strings.TrimSpace(c.ID)
				if id == "" {
					id = callID(i, k, c)
				}
				open = append(open, id)
				args := "{}"
				if len(c.Function.Arguments) > 0 {
					args = string(c.Function.Arguments)
					if !strings.HasPrefix(strings.TrimSpace(args), "{") {
						args = `"` + strings.ReplaceAll(strings.TrimSpace(args), `"`, `\"`) + `"`
					}
				}
				calls = append(calls, map[string]any{"id": id, "type": "function", "function": map[string]any{"name": c.Function.Name, "arguments": args}})
			}
			turn := map[string]any{"role": "assistant", "content": textOf(m.Content), "tool_calls": calls}
			if think := reasoningOf(m); think != "" {
				turn["reasoning_content"] = think
			}
			out = append(out, turn)
			continue
		}
		if m.Role == "assistant" {
			flushSeen()
		}
		blocks := contentBlocks(m.Content)
		if m.Role == "assistant" {
			var texts []map[string]any
			for _, b := range blocks {
				if b["type"] == "text" {
					texts = append(texts, b)
				}
			}
			blocks = texts
		}
		if len(blocks) == 0 {
			continue
		}
		if m.Role == "user" && len(seen) > 0 {
			blocks = append(seen, blocks...)
			seen = nil
		}
		turn := map[string]any{"role": m.Role, "content": blocks}
		if m.Role == "assistant" {
			if think := reasoningOf(m); think != "" {
				turn["reasoning_content"] = think
			}
		}
		out = append(out, turn)
	}
	flushSeen()
	return out
}

// callID is the id a call the client sent without one goes out with: the same
// on every turn, made from where the call is and what it says.
func callID(at, k int, c chatToolCall) string {
	args := string(c.Function.Arguments)
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d\x00%d\x00%s\x00%s", at, k, c.Function.Name, args)))
	return "call_" + hex.EncodeToString(sum[:])[:32]
}

// cacheMarked adds the prompt-cache markers Qoder's own client sends: every
// user turn's blocks also as contents, and the last cacheable block of the last
// turn marked ephemeral.
func cacheMarked(msgs []map[string]any) []map[string]any {
	for i, m := range msgs {
		if m["role"] != "user" {
			continue
		}
		if blocks, ok := m["content"].([]map[string]any); ok {
			copied := append([]map[string]any(nil), blocks...)
			out := map[string]any{}
			for k, v := range m {
				out[k] = v
			}
			out["content"] = copied
			out["contents"] = copied
			msgs[i] = out
		}
	}
	if len(msgs) == 0 {
		return msgs
	}
	last := msgs[len(msgs)-1]
	if last["role"] != "user" {
		return msgs
	}
	blocks, ok := last["content"].([]map[string]any)
	if !ok {
		return msgs
	}
	at := -1
	for i := len(blocks) - 1; i >= 0; i-- {
		switch blocks[i]["type"] {
		case "thinking", "redacted_thinking", "tool_use", "tool_result":
			continue
		}
		at = i
		break
	}
	if at < 0 {
		return msgs
	}
	copied := append([]map[string]any(nil), blocks...)
	marked := map[string]any{}
	for k, v := range copied[at] {
		marked[k] = v
	}
	marked["cache_control"] = map[string]any{"type": "ephemeral"}
	copied[at] = marked
	out := map[string]any{}
	for k, v := range last {
		out[k] = v
	}
	out["content"] = copied
	out["contents"] = copied
	msgs[len(msgs)-1] = out
	return msgs
}

// effortFor is whether to think and at which of the model's efforts. A model
// that always thinks thinks at its lowest when asked for none, and a request
// whose own budget is tiny is not one the client meant to think in.
func effortFor(asked string, m qoderModel, budget int) (bool, string) {
	want := strings.ToLower(strings.TrimSpace(asked))
	rank := func(name string) int {
		for i, v := range effortOrder {
			if v == name {
				return i
			}
		}
		return -1
	}
	if rank(want) < 0 {
		want = ""
	}
	if want == "" && budget > 0 && budget <= 1024 {
		want = "none"
	}
	if !m.Thinks {
		return false, ""
	}
	if len(m.Efforts) == 0 {
		return want != "none", ""
	}
	if want == "none" {
		if m.AlwaysThinks {
			return true, m.Efforts[0]
		}
		return false, ""
	}
	if want == "" {
		return true, m.DefaultEffort
	}
	best, bestDistance := want, len(effortOrder)+1
	for _, level := range m.Efforts {
		at := rank(want)
		to := rank(level)
		if at < 0 || to < 0 {
			continue
		}
		d := at - to
		if d < 0 {
			d = -d
		}
		if d < bestDistance {
			best, bestDistance = level, d
		}
	}
	return true, best
}

// windowFor is the context window a request is sent on a model that offers
// several: the default while the request fits it, else the smallest that holds
// it, else the largest.
func windowFor(chat chatRequest, m qoderModel, reply int) int64 {
	base := m.DefaultWindow
	if base <= 0 {
		base = m.MaxInput
	}
	if len(m.Windows) == 0 {
		return m.Context
	}
	raw, _ := json.Marshal([]any{chat.Messages, chat.Tools})
	need := int64(len(raw)/3 + reply)
	if base > 0 && need <= base {
		return base
	}
	for _, w := range m.Windows {
		if w >= need {
			return w
		}
	}
	return m.Windows[len(m.Windows)-1]
}

// modelConfig is the model_config Qoder's client sends: never the listing entry
// itself, whose extra fields change how calls are billed.
func modelConfig(m qoderModel, thinking bool) map[string]any {
	maxInput := m.MaxInput
	if maxInput <= 0 {
		maxInput = m.Context
	}
	if maxInput <= 0 {
		maxInput = 200_000
	}
	cfg := map[string]any{
		"key":              m.Key,
		"display_name":     firstNonEmpty(m.Name, m.Key),
		"format":           firstNonEmpty(m.Format, "openai"),
		"is_vl":            m.Images,
		"is_reasoning":     m.Thinks && thinking,
		"api_key":          "",
		"url":              "",
		"source":           firstNonEmpty(m.Source, "system"),
		"max_input_tokens": maxInput,
	}
	if m.OuterProvider != "" {
		cfg["outer_provider"] = m.OuterProvider
	} else {
		cfg["model"] = ""
	}
	return cfg
}

// buildQoderBody is the plaintext agent_chat_generation takes for a chat
// completion request.
func buildQoderBody(chat chatRequest, m qoderModel, session string, task *qoderTask, cosyVersion string, defaultMaxTokens int) ([]byte, error) {
	if strings.TrimSpace(m.Key) == "" || strings.EqualFold(m.Key, "auto") || strings.EqualFold(m.Key, "default") {
		return nil, fmt.Errorf("no Qoder model key for %q", m.Key)
	}
	var systemParts []string
	for _, msg := range chat.Messages {
		if msg.Role == "system" || msg.Role == "developer" {
			if text := strings.TrimSpace(textOf(msg.Content)); text != "" {
				systemParts = append(systemParts, text)
			}
		}
	}
	sysText := qoderSystemPrompt
	if len(systemParts) > 0 {
		sysText += "\n\n" + strings.Join(systemParts, "\n\n")
	}
	choice := toolChoiceName(chat.ToolChoice)
	tools := chat.Tools
	if choice == "none" {
		tools = nil
	}
	if choice == "required" && len(tools) > 0 {
		sysText += "\nYou must call an available function in this response."
	}
	budget := chat.MaxCompletionTokens
	if budget <= 0 {
		budget = chat.MaxTokens
	}
	if budget <= 0 {
		budget = defaultMaxTokens
	}
	if budget <= 0 {
		budget = 32000
	}
	thinking, effort := effortFor(chat.ReasoningEffort, m, budget)
	params := map[string]any{"enable_thinking": thinking, "max_tokens": budget}
	if effort != "" {
		params["reasoning_effort"] = effort
	}
	if window := windowFor(chat, m, budget); window > 0 {
		params["context_length"] = window
	}
	requestID, err := uuidText()
	if err != nil {
		return nil, errors.New("cannot create Qoder request id")
	}
	if task == nil {
		task = qoderTasks.begin(session, chat, time.Now().UnixMilli())
	}
	stage := "start"
	if task.Answered {
		stage = "processing"
	}
	sys := map[string]any{"type": "text", "text": sysText}
	body := map[string]any{
		"request_id":     requestID,
		"request_set_id": task.ID,
		"chat_record_id": requestID,
		"session_id":     session,
		"parameters":     params,
		"business": map[string]any{
			"product":   "app",
			"version":   cosyVersion,
			"type":      "agent",
			"id":        task.ID,
			"name":      task.Name,
			"begin_at":  task.Begin,
			"stage":     stage,
			"model_key": m.Key,
		},
		"agent_id":     "agent_common",
		"task_id":      "common",
		"session_type": "app",
		"model_config": modelConfig(m, thinking),
		"system":       []any{sys},
		"messages":     append([]map[string]any{{"role": "system", "content": []any{sys}}}, cacheMarked(qoderMessages(chat.Messages))...),
	}
	if len(tools) > 0 {
		out := make([]map[string]any, 0, len(tools))
		for _, t := range tools {
			if strings.TrimSpace(t.Function.Name) == "" {
				continue
			}
			params := t.Function.Parameters
			if len(params) == 0 || string(params) == "null" {
				params = json.RawMessage(`{"type":"object","properties":{}}`)
			}
			out = append(out, map[string]any{"type": "function", "function": map[string]any{"name": t.Function.Name, "description": t.Function.Description, "parameters": params}})
		}
		if len(out) > 0 {
			body["tools"] = out
		}
	}
	return json.Marshal(body)
}
