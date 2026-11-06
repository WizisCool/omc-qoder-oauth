package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// eventsTestTools is the request's declarations as a client sends them: what a
// tool call written as text is checked against, and where a parameter's type
// comes from.
const eventsTestTools = `[
	{"type":"function","function":{"name":"fs_read","parameters":{"type":"object","properties":{"path":{"type":"string"},"limit":{"type":"integer"}}}}},
	{"type":"function","function":{"name":"web_search","parameters":{"type":"object","properties":{"queries":{"type":"array"},"note":{"type":"string"}}}}},
	{"type":"function","function":{"name":"Bash","parameters":{"type":"object","properties":{"command":{"type":"string"}}}}}
]`

func eventsTools(t *testing.T) []chatTool {
	t.Helper()
	var tools []chatTool
	if err := json.Unmarshal([]byte(eventsTestTools), &tools); err != nil {
		t.Fatalf("tools: %v", err)
	}
	return tools
}

// chunkFrame is one Qoder SSE frame carrying this OpenAI chunk as its body.
func chunkFrame(chunk map[string]any) string { return sseOf(mustJSON(chunk)) }

func deltaFrame(delta map[string]any) string {
	return chunkFrame(map[string]any{"choices": []any{map[string]any{"delta": delta}}})
}

func textFrame(text string) string {
	return deltaFrame(map[string]any{"content": text})
}

func reasoningFrame(text string) string {
	return deltaFrame(map[string]any{"reasoning_content": text})
}

func finishFrame(reason string) string {
	return chunkFrame(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{}, "finish_reason": reason}}})
}

// usageFrame is the chunk Qoder sends after the finish, with no choices.
func usageFrame(prompt, completion, cached, reasoning int) string {
	usage := map[string]any{"prompt_tokens": prompt, "completion_tokens": completion}
	if cached > 0 {
		usage["prompt_tokens_details"] = map[string]any{"cached_tokens": cached}
	}
	if reasoning > 0 {
		usage["completion_tokens_details"] = map[string]any{"reasoning_tokens": reasoning}
	}
	return chunkFrame(map[string]any{"choices": []any{}, "usage": usage})
}

func nativeFrame(calls ...map[string]any) string {
	return deltaFrame(map[string]any{"tool_calls": calls})
}

// upstreamCall is one delta.tool_calls entry; an empty field is left out, as a
// stream that names a call after its id and arguments sends it.
func upstreamCall(index int, id, name, args string) map[string]any {
	function := map[string]any{}
	if name != "" {
		function["name"] = name
	}
	if args != "" {
		function["arguments"] = args
	}
	call := map[string]any{"index": index, "function": function}
	if id != "" {
		call["id"] = id
	}
	return call
}

// runEvents reads frames the way an executor does: feed, then flush.
func runEvents(t *testing.T, tools []chatTool, frames ...string) []qoderEvent {
	t.Helper()
	q := newQoderEvents(tools)
	var out []qoderEvent
	for _, frame := range frames {
		out = append(out, q.feed([]byte(frame))...)
	}
	return append(out, q.flush()...)
}

// eventSummary is what an answer came to, for comparing against a want.
type eventSummary struct {
	text      string
	reasoning string
	calls     []string
	args      []string
	stop      string
	usage     *openAIUsage
	err       *qoderFailure
}

func summarize(events []qoderEvent) eventSummary {
	var s eventSummary
	for _, e := range events {
		switch {
		case e.Err != nil:
			s.err = e.Err
		case e.Tool != nil:
			s.calls = append(s.calls, fmt.Sprintf("%d %s %s", e.Tool.Index, e.Tool.Name, e.Tool.Args))
			if e.Tool.ID == "" {
				s.calls[len(s.calls)-1] += " (no id)"
			}
		case e.Args != nil:
			s.args = append(s.args, fmt.Sprintf("%d %s", e.Args.Index, e.Args.Text))
		case e.Reasoning != "":
			s.reasoning += e.Reasoning
		case e.Text != "":
			s.text += e.Text
		case e.Stop != "":
			s.stop, s.usage = e.Stop, e.Usage
		}
	}
	return s
}

type wantEvents struct {
	text      string
	reasoning string
	calls     []string
	args      []string
	stop      string
	usage     *openAIUsage
	errStatus int
	errCode   string
	errText   string
	nameless  bool
}

func TestQoderEvents(t *testing.T) {
	fenced := "```\nThe model writes <function_calls><invoke name=\"fs_read\"><parameter name=\"path\">x</parameter></invoke></function_calls> like this\n```"
	invokeCall := `<function_calls><invoke name="fs_read"><parameter name="path">x.txt</parameter></invoke></function_calls>`
	undeclared := `<function_calls><invoke name="not_declared"><parameter name="path">x</parameter></invoke></function_calls>`

	cases := []struct {
		name   string
		frames []string
		want   wantEvents
	}{
		{
			name: "text and reasoning before a native call named late",
			frames: []string{
				textFrame("Let me look at that."),
				reasoningFrame("thinking it over"),
				nativeFrame(upstreamCall(0, "call_up_1", "", `{"path":`)),
				nativeFrame(upstreamCall(0, "", "fs_read", `"a.txt"}`)),
				nativeFrame(upstreamCall(0, "", "", `,"limit":5}`)),
				nativeFrame(upstreamCall(1, "call_up_2", "web_search", `{"queries":["x"]}`)),
				finishFrame("tool_calls"),
				usageFrame(7, 3, 2, 1),
			},
			want: wantEvents{
				text:      "Let me look at that.",
				reasoning: "thinking it over",
				calls:     []string{`0 fs_read {"path":"a.txt"}`, `1 web_search {"queries":["x"]}`},
				args:      []string{`0 ,"limit":5}`},
				stop:      "tool_calls",
				usage:     &openAIUsage{PromptTokens: 7, CompletionTokens: 3, TotalTokens: 10, CachedTokens: 2, ReasoningTokens: 1},
			},
		},
		{
			name:   "tool_call wrapping a JSON call",
			frames: []string{textFrame("<tool_call>\n" + `{"name":"fs_read","arguments":{"path":"README.md"}}` + "\n</tool_call>")},
			want: wantEvents{
				calls: []string{`0 fs_read {"path":"README.md"}`},
				stop:  "tool_calls",
			},
		},
		{
			name:   "tool_call wrapping an XML function body",
			frames: []string{textFrame(`<tool_call><function=web_search><parameter=queries>["a","b"]</parameter><parameter=note>hi</parameter></function></tool_call>`)},
			want: wantEvents{
				calls: []string{`0 web_search {"note":"hi","queries":["a","b"]}`},
				stop:  "tool_calls",
			},
		},
		{
			name:   "bare XML function keeps a string parameter as text",
			frames: []string{textFrame(`<function=Bash><parameter=command>[1, 2]</parameter></function>`)},
			want: wantEvents{
				calls: []string{`0 Bash {"command":"[1, 2]"}`},
				stop:  "tool_calls",
			},
		},
		{
			name:   "bare invoke call drops the close tags after it",
			frames: []string{textFrame(invokeCall + " then this")},
			want: wantEvents{
				// The close tag and the blank after it are dropped, not shown.
				text:  "then this",
				calls: []string{`0 fs_read {"path":"x.txt"}`},
				stop:  "tool_calls",
			},
		},
		{
			name:   "a call in a code fence stays text",
			frames: []string{textFrame(fenced)},
			want:   wantEvents{text: fenced, stop: "stop"},
		},
		{
			name:   "an unreadable block in a code fence stays text",
			frames: []string{textFrame("```\n<tool_call>{not json}</tool_call>\n```")},
			want:   wantEvents{text: "```\n<tool_call>{not json}</tool_call>\n```", stop: "stop"},
		},
		{
			name:   "a call to a tool the request never declared stays text",
			frames: []string{textFrame(undeclared)},
			want:   wantEvents{text: undeclared, stop: "stop"},
		},
		{
			name:   "a call that cannot be read is an error, not text",
			frames: []string{textFrame(`<tool_call>{not json}</tool_call>`)},
			want:   wantEvents{errStatus: 502, errText: unreadableCallText},
		},
		{
			name:   "a call cut off at stream end is an error",
			frames: []string{textFrame(`<tool_call>{"name":"fs_read","arguments":{"path":"a`)},
			want:   wantEvents{errStatus: 502, errText: cutShortCallText},
		},
		{
			name:   "a whole JSON block with no close still counts at the end",
			frames: []string{textFrame(`<tool_call>{"name":"fs_read","arguments":{"path":"a"}}`)},
			want: wantEvents{
				calls: []string{`0 fs_read {"path":"a"}`},
				stop:  "tool_calls",
			},
		},
		{
			name:   "a bare call cut off at stream end is an error",
			frames: []string{textFrame(`<function_calls><invoke name="fs_read"><parameter name="path">a`)},
			want:   wantEvents{errStatus: 502, errText: cutShortCallText},
		},
		{
			name:   "a nameless native call with nothing said is an error",
			frames: []string{nativeFrame(upstreamCall(0, "call_up_9", "", `{"path":"a"}`)), finishFrame("tool_calls")},
			want:   wantEvents{errStatus: 502, errText: namelessCallText, nameless: true},
		},
		{
			name:   "a nameless native call after an answer began is dropped",
			frames: []string{textFrame("Sure. "), nativeFrame(upstreamCall(0, "call_up_9", "", `{"path":"a"}`)), finishFrame("tool_calls")},
			want:   wantEvents{text: "Sure. ", stop: "stop"},
		},
		{
			name:   "the usage chunk after the finish is kept",
			frames: []string{textFrame("Hi"), finishFrame("length"), usageFrame(5, 2, 0, 0)},
			want:   wantEvents{text: "Hi", stop: "length", usage: &openAIUsage{PromptTokens: 5, CompletionTokens: 2, TotalTokens: 7}},
		},
		{
			name:   "a queue notice said in an OK frame keeps its status",
			frames: []string{sseOf(`{"code":10605,"message":"busy","queue":{"isQueued":true,"retryAfterSeconds":12}}`)},
			want:   wantEvents{errStatus: 503, errCode: queuedCode, errText: "try again in 12s"},
		},
		{
			name:   "a frame that is not a 200 is the turn's failure",
			frames: []string{"data: " + mustJSON(map[string]any{"statusCodeValue": 403, "body": `{"code":105,"message":"Login expired"}`}) + "\n\n"},
			want:   wantEvents{errStatus: 401, errCode: "105", errText: "sign in again"},
		},
		{
			name:   "DONE ends the reading, not the answer",
			frames: []string{textFrame("Hi"), sseOf("[DONE]"), textFrame("read past the end")},
			want:   wantEvents{text: "Hi", stop: "stop"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := summarize(runEvents(t, eventsTools(t), c.frames...))
			if got.text != c.want.text {
				t.Errorf("text = %q, want %q", got.text, c.want.text)
			}
			if got.reasoning != c.want.reasoning {
				t.Errorf("reasoning = %q, want %q", got.reasoning, c.want.reasoning)
			}
			if !reflect.DeepEqual(got.calls, c.want.calls) {
				t.Errorf("calls = %v, want %v", got.calls, c.want.calls)
			}
			if !reflect.DeepEqual(got.args, c.want.args) {
				t.Errorf("args = %v, want %v", got.args, c.want.args)
			}
			if got.stop != c.want.stop {
				t.Errorf("stop = %q, want %q", got.stop, c.want.stop)
			}
			if !reflect.DeepEqual(got.usage, c.want.usage) {
				t.Errorf("usage = %+v, want %+v", got.usage, c.want.usage)
			}
			switch {
			case c.want.errStatus == 0:
				if got.err != nil {
					t.Fatalf("err = %+v, want none", got.err)
				}
				return
			case got.err == nil:
				t.Fatalf("err = none, want %d", c.want.errStatus)
			}
			if got.err.Status != c.want.errStatus {
				t.Errorf("err status = %d, want %d", got.err.Status, c.want.errStatus)
			}
			if got.err.Code != c.want.errCode {
				t.Errorf("err code = %q, want %q", got.err.Code, c.want.errCode)
			}
			if !strings.Contains(got.err.Message, c.want.errText) {
				t.Errorf("err message = %q, want %q in it", got.err.Message, c.want.errText)
			}
			if got.err.Nameless != c.want.nameless {
				t.Errorf("err nameless = %v, want %v", got.err.Nameless, c.want.nameless)
			}
			if got.stop != "" {
				t.Errorf("stop = %q after an error, want none", got.stop)
			}
		})
	}
}

func TestQoderEventsHoldsAPartialTag(t *testing.T) {
	q := newQoderEvents(eventsTools(t))
	if got := q.feed([]byte(textFrame("<tool_"))); len(got) != 0 {
		t.Fatalf("a partial tag went out early: %+v", got)
	}
	got := q.feed([]byte(textFrame(`call>{"name":"fs_read","arguments":{"path":"a"}}</tool_call>`)))
	if len(got) != 1 || got[0].Tool == nil || got[0].Tool.Name != "fs_read" || got[0].Tool.Index != 0 || got[0].Tool.ID == "" {
		t.Fatalf("events = %+v", got)
	}
}

func TestQoderEventsHoldsABareOpener(t *testing.T) {
	q := newQoderEvents(eventsTools(t))
	if got := q.feed([]byte(textFrame("before <invoke name="))); len(got) != 1 || got[0].Text != "before " {
		t.Fatalf("events = %+v", got)
	}
	got := q.feed([]byte(textFrame(`"fs_read"><parameter name="path">a</parameter></invoke></function_calls>`)))
	if len(got) != 1 || got[0].Tool == nil || got[0].Tool.Args != `{"path":"a"}` {
		t.Fatalf("events = %+v", got)
	}
}

func TestQoderEventsNeverEmitsAnUnnamedNativeCall(t *testing.T) {
	q := newQoderEvents(eventsTools(t))
	if got := q.feed([]byte(nativeFrame(upstreamCall(0, "call_up_1", "", `{"path":`)))); len(got) != 0 {
		t.Fatalf("events before the name = %+v", got)
	}
	got := q.feed([]byte(nativeFrame(upstreamCall(0, "", "fs_read", `"a.txt"}`))))
	if len(got) != 1 || got[0].Tool == nil {
		t.Fatalf("events = %+v", got)
	}
	if got[0].Tool.ID != "call_up_1" || got[0].Tool.Name != "fs_read" || got[0].Tool.Args != `{"path":"a.txt"}` {
		t.Fatalf("tool = %+v", got[0].Tool)
	}
}

func TestQoderEventsEmittedToolSetsTheStopReason(t *testing.T) {
	q := newQoderEvents(eventsTools(t))
	q.feed([]byte(textFrame(`<tool_call>{"name":"fs_read","arguments":{"path":"a"}}</tool_call>`)))
	if !q.toolEmitted() {
		t.Fatal("toolEmitted = false after a text call")
	}
	stop := summarize(q.flush())
	if stop.stop != "tool_calls" {
		t.Fatalf("stop = %q", stop.stop)
	}
}

func TestTextCallBecomesOpenAIFrames(t *testing.T) {
	events := runEvents(t, eventsTools(t), textFrame(`<tool_call>{"name":"fs_read","arguments":{"path":"a"}}</tool_call>`))
	answer := newStreamAnswer("ultimate")
	var frames [][]byte
	for _, e := range events {
		frames = append(frames, answer.frames(e)...)
	}
	joined := string(bytes.Join(frames, nil))
	for _, want := range []string{`"tool_calls"`, `"name":"fs_read"`, `"arguments":"{\"path\":\"a\"}"`, `"finish_reason":"tool_calls"`} {
		if !strings.Contains(joined, want) {
			t.Fatalf("frames = %s, want %s", joined, want)
		}
	}
	if len(answer.done()) == 0 {
		t.Fatal("no [DONE] after the answer")
	}
	payload, failure := completionFromEvents("ultimate", events)
	if failure != nil {
		t.Fatalf("completion failure = %+v", failure)
	}
	var answer2 struct {
		Choices []struct {
			Message struct {
				ToolCalls []completionCall `json:"tool_calls"`
			} `json:"message"`
			Finish string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(payload, &answer2); err != nil {
		t.Fatal(err)
	}
	if len(answer2.Choices[0].Message.ToolCalls) != 1 || answer2.Choices[0].Finish != "tool_calls" {
		t.Fatalf("completion = %s", payload)
	}
}
