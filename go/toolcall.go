package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"strings"
)

// What a tool call written as text can go wrong with. Each one ends the turn
// as a 502: the call is the answer's whole point, so passing it on as prose
// answers the wrong question.
const (
	cutShortCallText   = "the model's tool call, written as text, was cut short; it is not sent on as the answer"
	unreadableCallText = "the model's tool call, written as text, can't be read; it is not sent on as the answer"
	namelessCallText   = "the model's native tool call has no name; it is not sent on as a tool"
)

// The tags a model uses for a tool call in its text: Qoder's own wrapper, and
// Anthropic's and Qwen's bare ones.
const (
	callOpen  = "<tool_call>"
	callClose = "</tool_call>"
)

var (
	// xmlAt is where a bare call may start: an opener still being written, the
	// wrapper a call is followed by, the next call's opener.
	xmlAtRe = regexp.MustCompile(`<(?:antml:)?(?:function_calls>|invoke\s+name\s*=|function=)`)
	// xmlHold is an opener whose tag has not finished arriving.
	xmlHoldRe = regexp.MustCompile(`<(?:antml:)?(?:invoke\s[^<>]{0,300}|function=[^<>]{0,300})$`)
	// xmlTagOpener is the tag a bare-read emits as text when it is no call.
	xmlTagOpenerRe = regexp.MustCompile(`^<[^<>]*>?`)
	// xmlHoldStart is what a wrapper's body may still be: the start of a call.
	xmlHoldStartRe = regexp.MustCompile(`^<(?:antml:)?(?:invoke\s|function=)[^<>]{0,300}$`)
	xmlWrapperRe   = regexp.MustCompile(`^<(?:antml:)?function_calls>`)
	xmlInvokeRe    = regexp.MustCompile(`^<(?:antml:)?invoke\s+name\s*=\s*"([^"<>]+)"\s*>`)
	xmlFunctionRe  = regexp.MustCompile(`^<function=([^\s<>]+?)\s*>`)
	xmlInvokeEnd   = regexp.MustCompile(`</(?:antml:)?invoke\s*>`)
	xmlFunctionEnd = regexp.MustCompile(`</function\s*>`)
	xmlNextRe      = regexp.MustCompile(`<(?:antml:)?invoke\s+name\s*=|<function=`)
	xmlParamRe     = regexp.MustCompile(`<(?:antml:)?parameter(?:\s+name\s*=\s*"([^"]+)"|=([^\s>]+))\s*>`)
	xmlParamEndRe  = regexp.MustCompile(`</(?:antml:)?parameter>`)
	xmlParamTestRe = regexp.MustCompile(`<(?:antml:)?parameter\b`)
	xmlLeftRe      = regexp.MustCompile(`^(?:\s+|</(?:antml:)?(?:function_calls|invoke|function)>)+`)
	// xmlStarts are the openers a wrapper may be followed by, and xmlMarks the
	// openers to hold when only a prefix of one has arrived.
	xmlStarts = []string{"<invoke ", "<antml:invoke ", "<function="}
	xmlMarks  = []string{"<function_calls>", "<antml:function_calls>", "<invoke ", "<antml:invoke ", "<function="}
	// xmlCloses are what may follow a bare call.
	xmlCloses = []string{"</function_calls>", "</antml:function_calls>", "</invoke>", "</antml:invoke>", "</function>"}
	// jsonCallRe is the XML body of a <tool_call>: <function=NAME><parameter=KEY>V</parameter></function>.
	jsonFuncRe  = regexp.MustCompile(`(?s)<function=([^>]+)>(.*?)</function>`)
	jsonParamRe = regexp.MustCompile(`(?s)<parameter=([^>]+)>(.*?)</parameter>`)
)

// toolCallText is a tool call the model wrote as text, lifted out of the answer.
type toolCallText struct {
	Name string
	Args string
}

// callPiece is one finding of the splitter: text to pass on, a call, or the
// error of a call that is not readable.
type callPiece struct {
	Text  string
	Call  *toolCallText
	Error string
}

// splitter lifts tool calls the model wrote as text out of the answer. It holds
// text until a <tool_call> block is whole, and until a tag that may still become
// a bare Anthropic (<invoke name="…">) or Qwen (<function=…>) call is settled.
// A block on its way to being a call that can't be read, or that the stream
// ended inside, is an error and never the answer: the turn fails so it can be
// tried again. A block whose JSON is whole when the stream ends without its
// close is still the call, and text in a code fence or an inline code span stays
// text — a <tool_call> there is prose about one.
type splitter struct {
	tools   []chatTool
	textBuf string
	callBuf string
	inCall  bool
	// after is set once a bare call ended: the close tags and blanks after it
	// are dropped, not shown as text.
	after bool
	// sawTool is whether the text held a call, which makes the stop reason
	// tool_calls.
	sawTool bool
	// sent is the text given out so far, to tell a block in code from a call.
	sent string
}

func newSplitter(tools []chatTool) *splitter {
	declared := make([]chatTool, 0, len(tools))
	for _, tool := range tools {
		if tool.Function.Name != "" {
			declared = append(declared, tool)
		}
	}
	return &splitter{tools: declared}
}

func (s *splitter) text(out *[]callPiece, t string) {
	s.sent += t
	*out = append(*out, callPiece{Text: t})
}

// feed reads a piece of the answer's text and returns what it settled.
func (s *splitter) feed(input string) []callPiece {
	var out []callPiece
	for input != "" {
		if !s.inCall {
			comb := s.textBuf + input
			s.textBuf = ""
			if s.after {
				rest, held := s.left(comb, false)
				if held {
					s.textBuf = comb
					break
				}
				comb = rest
				if comb == "" {
					break
				}
			}
			i := strings.Index(comb, callOpen)
			x := -1
			if len(s.tools) > 0 {
				if at := xmlAtRe.FindStringIndex(comb); at != nil {
					x = at[0]
				}
			}
			if x >= 0 && (i < 0 || x < i) {
				next, outcome := s.bare(&out, comb, x, false)
				if outcome == bareStop {
					return out
				}
				if outcome == bareHeld {
					break
				}
				input = next
				continue
			}
			if i < 0 {
				keep := 0
				marks := []string{callOpen}
				if len(s.tools) > 0 {
					marks = append([]string{callOpen}, xmlMarks...)
				}
				for _, mark := range marks {
					longest := len(mark) - 1
					if len(comb) < longest {
						longest = len(comb)
					}
					for n := longest; n > keep; n-- {
						if strings.HasSuffix(comb, mark[:n]) {
							keep = n
							break
						}
					}
				}
				if len(s.tools) > 0 {
					if h := xmlHoldRe.FindStringIndex(comb); h != nil && len(comb)-h[0] > keep {
						keep = len(comb) - h[0]
					}
				}
				if keep < len(comb) {
					s.text(&out, comb[:len(comb)-keep])
				}
				s.textBuf = comb[len(comb)-keep:]
				break
			}
			if i > 0 {
				s.text(&out, comb[:i])
			}
			input = comb[i+len(callOpen):]
			s.inCall = true
			continue
		}
		comb := s.callBuf + input
		s.callBuf = ""
		j := strings.Index(comb, callClose)
		if j < 0 {
			s.callBuf = comb
			break
		}
		body := comb[:j]
		if call := s.parseCall(body); call != nil {
			s.sawTool = true
			out = append(out, callPiece{Call: call})
		} else if callish(body) && !quoted(s.sent) {
			s.inCall = false
			out = append(out, callPiece{Error: unreadableCallText})
			return out
		} else {
			s.text(&out, callOpen+body+callClose)
		}
		input = comb[j+len(callClose):]
		s.inCall = false
	}
	return out
}

// flush ends the text: a block the stream ended inside becomes its call when it
// began as one and the JSON is whole, and the error of a call cut short
// otherwise.
func (s *splitter) flush() []callPiece {
	if s.inCall {
		body := s.callBuf
		s.inCall, s.callBuf = false, ""
		if callish(body) && !quoted(s.sent) {
			call := s.parseCall(body)
			if call == nil {
				return []callPiece{{Error: cutShortCallText}}
			}
			s.sawTool = true
			return []callPiece{{Call: call}}
		}
		t := callOpen + body
		s.sent += t
		return []callPiece{{Text: t}}
	}
	if s.textBuf == "" {
		return nil
	}
	t := s.textBuf
	s.textBuf = ""
	if s.after {
		t, _ = s.left(t, true)
	}
	var out []callPiece
	for t != "" {
		x := -1
		if len(s.tools) > 0 {
			if at := xmlAtRe.FindStringIndex(t); at != nil {
				x = at[0]
			}
		}
		if x < 0 {
			s.text(&out, t)
			break
		}
		next, rest := s.bare(&out, t, x, true)
		if rest != bareRest {
			break
		}
		t = next
		if s.after {
			t, _ = s.left(t, true)
		}
	}
	return out
}

// bareOutcome says how a bare read ended.
type bareOutcome int

const (
	// bareRest is a settled read: the leftover input is what to read next.
	bareRest bareOutcome = iota
	// bareHeld is a read held for more input.
	bareHeld
	// bareStop is a read that ended the turn in an error.
	bareStop
)

// bare reads text that may hold a bare call, comb starting at its tag x.
func (s *splitter) bare(out *[]callPiece, comb string, x int, end bool) (string, bareOutcome) {
	head, at := comb[:x], comb[x:]
	opener := xmlTagOpenerRe.FindString(at)
	if quoted(s.sent + head) {
		s.text(out, head+opener)
		return at[len(opener):], bareRest
	}
	found := s.xmlCall(at, end)
	switch {
	case found.held:
		if head != "" {
			s.text(out, head)
		}
		s.textBuf = at
		return "", bareHeld
	case found.text:
		s.text(out, head+opener)
		return at[len(opener):], bareRest
	}
	if head != "" {
		s.text(out, head)
	}
	if found.err != "" {
		*out = append(*out, callPiece{Error: found.err})
		return "", bareStop
	}
	s.sawTool = true
	*out = append(*out, callPiece{Call: &toolCallText{Name: found.name, Args: found.args}})
	s.after = true
	return found.rest, bareRest
}

// left drops what is left after a bare call, held while it may still be one of
// its close tags.
func (s *splitter) left(comb string, end bool) (string, bool) {
	rest := xmlLeftRe.ReplaceAllString(comb, "")
	if !end && (rest == "" || hasLonger(rest, xmlCloses)) {
		return comb, true
	}
	s.after = false
	return rest, false
}

// xmlRead is what reading a bare call found.
type xmlRead struct {
	held bool // may still become a call
	text bool // no call at all: a tool the request didn't declare, a tag in prose
	err  string
	name string
	args string
	rest string
}

// xmlCall reads a bare call in Anthropic's tags (<function_calls> around one or
// more <invoke name="…"><parameter name="k">v</parameter></invoke>) or Qwen's
// (<function=…><parameter=k>v</parameter></function>), at starting at its first
// tag. Only a declared tool's call counts: these tags are common in prose.
func (s *splitter) xmlCall(at string, end bool) xmlRead {
	if w := xmlWrapperRe.FindString(at); w != "" {
		rest := strings.TrimLeft(at[len(w):], " \t\r\n")
		if !end && (rest == "" || hasPrefixIn(rest, xmlStarts)) {
			return xmlRead{held: true}
		}
		at = rest
	}
	var name, body string
	closeRe := xmlInvokeEnd
	if m := xmlInvokeRe.FindStringSubmatch(at); m != nil {
		name, body = m[1], at[len(m[0]):]
	} else if m := xmlFunctionRe.FindStringSubmatch(at); m != nil {
		name, body = m[1], at[len(m[0]):]
		closeRe = xmlFunctionEnd
	}
	if name == "" {
		if !end && (hasPrefixIn(at, xmlStarts) || xmlHoldStartRe.MatchString(at)) {
			return xmlRead{held: true}
		}
		return xmlRead{text: true}
	}
	name = strings.TrimSpace(name)
	if !s.declares(name) {
		return xmlRead{text: true}
	}
	closeAt := closeRe.FindStringIndex(body)
	nextAt := xmlNextRe.FindStringIndex(body)
	stop := -1
	if closeAt != nil {
		stop = closeAt[0]
	}
	if nextAt != nil && (stop < 0 || nextAt[0] < stop) {
		stop = nextAt[0]
	}
	if stop < 0 {
		if !end {
			return xmlRead{held: true}
		}
		stop = len(body)
	}
	whole := closeAt != nil && closeAt[0] == stop
	args, closed := s.xmlArgs(body[:stop], name)
	if !whole && !closed {
		// No parameter and no close: a tag named in passing. A parameter left
		// open is the call cut short.
		if !xmlParamTestRe.MatchString(body[:stop]) {
			return xmlRead{text: true}
		}
		return xmlRead{err: cutShortCallText}
	}
	rest := body[stop:]
	if whole {
		rest = body[stop+closeAt[1]-closeAt[0]:]
	}
	return xmlRead{name: name, args: argsJSON(args), rest: rest}
}

// xmlArgs reads the parameters of an invoke or function body, their values
// typed as the tool's schema says; closed is whether every one was closed.
func (s *splitter) xmlArgs(body, tool string) (map[string]any, bool) {
	opens := xmlParamRe.FindAllStringSubmatchIndex(body, -1)
	props := toolTextProps(s.tools, tool)
	args := make(map[string]any, len(opens))
	closed := len(opens) > 0
	for k, m := range opens {
		key := ""
		switch {
		case m[2] >= 0:
			key = body[m[2]:m[3]]
		case m[4] >= 0:
			key = body[m[4]:m[5]]
		}
		start := m[1]
		end := len(body)
		if k+1 < len(opens) {
			end = opens[k+1][0]
		}
		value := body[start:end]
		if c := xmlParamEndRe.FindStringIndex(value); c != nil {
			value = value[:c[0]]
		} else {
			closed = false
		}
		args[key] = typedValue(trimOneNewline(value), props[key])
	}
	return args, closed
}

// parseCall reads one <tool_call> block: JSON {name, arguments}, or the XML
// function/parameter form, its values typed as the tool's schema says.
func (s *splitter) parseCall(v string) *toolCallText {
	var body map[string]json.RawMessage
	if json.Unmarshal([]byte(strings.TrimSpace(v)), &body) == nil {
		var name string
		if raw, ok := body["name"]; ok && json.Unmarshal(raw, &name) == nil {
			name = strings.TrimSpace(name)
			if name != "" {
				var args map[string]json.RawMessage
				if raw, ok := body["arguments"]; ok && json.Unmarshal(raw, &args) == nil && args != nil {
					encoded, _ := json.Marshal(args)
					return &toolCallText{Name: name, Args: string(encoded)}
				}
			}
		}
	}
	m := jsonFuncRe.FindStringSubmatch(v)
	if m == nil || strings.TrimSpace(m[1]) == "" {
		return nil
	}
	name := strings.TrimSpace(m[1])
	props := toolTextProps(s.tools, name)
	args := map[string]any{}
	for _, pm := range jsonParamRe.FindAllStringSubmatch(m[2], -1) {
		key := strings.TrimSpace(pm[1])
		if key == "" {
			continue
		}
		args[key] = typedValue(strings.TrimSpace(pm[2]), props[key])
	}
	return &toolCallText{Name: name, Args: argsJSON(args)}
}

func (s *splitter) declares(name string) bool {
	for _, tool := range s.tools {
		if tool.Function.Name == name {
			return true
		}
	}
	return false
}

// toolTextProps is the set of a tool's parameters whose schema type is string:
// a call written as XML keeps their value as text, and reads every other value
// as JSON, with its text where it isn't JSON (a tool that takes an array or a
// number).
func toolTextProps(tools []chatTool, name string) map[string]bool {
	out := map[string]bool{}
	for _, tool := range tools {
		if tool.Function.Name != name || len(tool.Function.Parameters) == 0 {
			continue
		}
		var schema struct {
			Properties map[string]struct {
				Type json.RawMessage `json:"type"`
			} `json:"properties"`
		}
		if json.Unmarshal(tool.Function.Parameters, &schema) != nil {
			return out
		}
		for key, prop := range schema.Properties {
			var kind string
			if json.Unmarshal(prop.Type, &kind) == nil && strings.TrimSpace(kind) == "string" {
				out[key] = true
			}
		}
	}
	return out
}

// typedValue is a parameter's value: its text where the tool's schema says
// string, else its JSON, else its text.
func typedValue(v string, text bool) any {
	if text {
		return v
	}
	var out any
	if json.Unmarshal([]byte(strings.TrimSpace(v)), &out) == nil {
		return out
	}
	return v
}

func argsJSON(args map[string]any) string {
	encoded, err := json.Marshal(args)
	if err != nil {
		return "{}"
	}
	return string(encoded)
}

// quoted says text ends inside a code fence or an inline code span: a
// <tool_call> there is prose about one, not a call.
func quoted(text string) bool {
	if strings.Count(text, "```")%2 == 1 {
		return true
	}
	line := text
	if i := strings.LastIndexByte(text, '\n'); i >= 0 {
		line = text[i+1:]
	}
	return strings.Count(strings.ReplaceAll(line, "```", ""), "`")%2 == 1
}

// callish says a block was on its way to being a call: its JSON began, or the
// XML function form.
func callish(v string) bool {
	return strings.HasPrefix(strings.TrimLeft(v, " \t\r\n"), "{") || strings.Contains(v, "<function=")
}

func hasPrefixIn(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(p, s) {
			return true
		}
	}
	return false
}

func hasLonger(s string, longer []string) bool {
	for _, l := range longer {
		if len(l) > len(s) && strings.HasPrefix(l, s) {
			return true
		}
	}
	return false
}

// trimOneNewline drops one line break from each end, as Qoder's own XML reading
// does, so a value written on its own line keeps its text.
func trimOneNewline(v string) string {
	if s, ok := strings.CutPrefix(v, "\r\n"); ok {
		v = s
	} else if s, ok := strings.CutPrefix(v, "\n"); ok {
		v = s
	}
	if s, ok := strings.CutSuffix(v, "\r\n"); ok {
		v = s
	} else if s, ok := strings.CutSuffix(v, "\n"); ok {
		v = s
	}
	return v
}

// randomCallID is the id a tool call lifted out of the text carries: Qoder
// names none, and a client keys its result on one.
func randomCallID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "call_00000000000000000000000000000000"
	}
	return "call_" + hex.EncodeToString(b)
}
