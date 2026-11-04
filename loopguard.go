package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"hash"
	"hash/fnv"
	"strings"
	"sync/atomic"
)

// Loop guard: a model's loop_guard setting watches its streamed output for degeneration. "observe"
// only logs a loop. "on" cuts it: a loop in the answer ends the response there, since the answer is
// already shown; a loop in thinking would leave the client with no answer, so the request is sent
// again with the output so far and a note asking the model to finish.
const (
	loopGuardOff     = "off"
	loopGuardObserve = "observe"
	loopGuardOn      = "on"
)

// Output is looping once some loopChunk bytes of it, whitespace collapsed, have recurred
// loopRepeats times without overlapping. Reasoning also counts as looping once loopRunOn bytes pass
// with no sentence break or newline: under a presence penalty the model trails off into an endless
// word list instead of repeating itself. Answers are exempt because they can be one long line of JSON.
const (
	loopChunk      = 128
	loopRepeats    = 6
	loopRunOn      = 1500
	loopMaxRetries = 2
)

const (
	ruleRepeat = "repeat"
	ruleRunOn  = "run-on"
)

type chunkSeen struct{ last, count, second int }

type loopDetector struct {
	runOn    bool   // apply the run-on rule
	runStart int    // raw offset just past the last sentence break
	text     []byte // raw output
	norm     []byte // output with whitespace runs collapsed to one space
	raw      []int  // raw offset of each norm byte
	seen     map[uint64]*chunkSeen
	hash     hash.Hash64
}

// add appends output and, once it is looping, returns the rule that fired and how much raw output
// to keep: everything before the repeated chunk's second copy, or before the run-on stretch.
func (d *loopDetector) add(s string) (keep int, rule string) {
	if d.seen == nil {
		d.seen, d.hash = map[uint64]*chunkSeen{}, fnv.New64a()
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		d.text = append(d.text, c)
		if d.runOn {
			if sentenceBreak(d.text) {
				d.runStart = len(d.text)
			} else if len(d.text)-d.runStart > loopRunOn {
				return d.runStart, ruleRunOn
			}
		}
		if c == ' ' || c == '\n' || c == '\t' || c == '\r' {
			if len(d.norm) > 0 && d.norm[len(d.norm)-1] == ' ' {
				continue
			}
			c = ' '
		}
		d.norm = append(d.norm, c)
		d.raw = append(d.raw, len(d.text)-1)
		if len(d.norm) < loopChunk {
			continue
		}
		at := len(d.norm) - loopChunk
		d.hash.Reset()
		d.hash.Write(d.norm[at:])
		key := d.hash.Sum64()
		e := d.seen[key]
		if e == nil {
			d.seen[key] = &chunkSeen{last: at, count: 1}
			continue
		}
		if at-e.last < loopChunk {
			continue
		}
		e.last, e.count = at, e.count+1
		if e.count == 2 {
			e.second = d.raw[at]
		}
		if e.count >= loopRepeats {
			return e.second, ruleRepeat
		}
	}
	return 0, ""
}

// sentenceBreak reports whether text ends a sentence or line: a newline, sentence punctuation
// followed by whitespace, or CJK sentence punctuation.
func sentenceBreak(text []byte) bool {
	n := len(text)
	switch text[n-1] {
	case '\n':
		return true
	case ' ', '\t', '\r':
		return n > 1 && strings.IndexByte(".!?:;", text[n-2]) >= 0
	}
	if text[n-1] < 0x80 {
		return false
	}
	for _, p := range []string{"。", "！", "？", "：", "；"} {
		if bytes.HasSuffix(text, []byte(p)) {
			return true
		}
	}
	return false
}

// sseReader splits a byte stream into SSE events.
type sseReader struct{ buf []byte }

type sseEvent struct {
	name string
	data []byte
}

func (r *sseReader) feed(b []byte) []sseEvent {
	r.buf = append(r.buf, bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n"))...)
	var out []sseEvent
	for {
		i := bytes.Index(r.buf, []byte("\n\n"))
		if i < 0 {
			return out
		}
		var ev sseEvent
		for _, line := range bytes.Split(r.buf[:i], []byte("\n")) {
			if v, ok := bytes.CutPrefix(line, []byte("event:")); ok {
				ev.name = string(bytes.TrimSpace(v))
			} else if v, ok := bytes.CutPrefix(line, []byte("data:")); ok {
				ev.data = append(ev.data, bytes.TrimSpace(v)...)
			}
		}
		r.buf = r.buf[i+2:]
		if ev.name != "" || len(ev.data) > 0 {
			out = append(out, ev)
		}
	}
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func encodeSSE(name string, data any) []byte {
	raw, _ := json.Marshal(data)
	if name == "" {
		return []byte("data: " + string(raw) + "\n\n")
	}
	return []byte("event: " + name + "\ndata: " + string(raw) + "\n\n")
}

var loopNudges = map[string]string{
	ruleRepeat: "You started repeating the same text over and over, so your response was cut off there. Do not repeat it again. Continue from where you left off and finish the task.",
	ruleRunOn:  "Your reasoning turned into a long list of words with no sentences, so your response was cut off there. Do not continue that list. Continue from where you left off and finish the task.",
}

var guardSeq atomic.Int64

// guard carries one guarded client stream across its upstream attempts.
type guard struct {
	id         int64
	r          *executorRequest
	url        string
	headers    map[string][]string
	body       map[string]any
	downstream string
	mode       string
	anthropic  bool
	loops      int
	chars      int

	// Anthropic: the assistant content so far, one entry per downstream block index.
	blocks []map[string]any
	// OpenAI: the assistant message so far.
	content, reasoning string
	toolCalls          bool
	lastChunk          map[string]any
}

func (g *guard) logf(format string, args ...any) {
	hostLog("info", fmt.Sprintf("loopguard[%d] model=%s format=%s mode=%s: ", g.id, g.r.Model, g.r.SourceFormat, g.mode)+fmt.Sprintf(format, args...))
}

func (g *guard) emit(payload []byte) bool {
	return streamEmit(g.downstream, payload) == nil
}

// pump reads one upstream attempt to the end, or until a loop hands off to recover.
func (g *guard) pump(hostStream string, attempt int) {
	defer func() {
		if v := recover(); v != nil {
			hostLog("warn", fmt.Sprintf("loopguard: pump panic: %v", v))
			httpStreamClose(hostStream)
			streamClose(g.downstream, "stream pump panic")
		}
	}()
	var rd sseReader
	det, detReasoning := loopDetector{}, loopDetector{runOn: true}
	observed := false
	for {
		chunk, err := httpStreamRead(hostStream)
		if err != nil {
			httpStreamClose(hostStream)
			streamClose(g.downstream, err.Error())
			return
		}
		for _, ev := range rd.feed(chunk.Payload) {
			out, piece, reasoningPiece, ok := g.translate(ev, attempt, &det, &detReasoning)
			if !ok {
				continue
			}
			g.chars += len(piece) + len(reasoningPiece)
			for _, d := range []struct {
				det  *loopDetector
				text string
			}{{&det, piece}, {&detReasoning, reasoningPiece}} {
				if d.text == "" || observed {
					continue
				}
				keep, rule := d.det.add(d.text)
				if rule == "" {
					continue
				}
				g.loops++
				g.logf("LOOP attempt=%d reasoning=%v rule=%s at=%d keep=%d", attempt, d.det == &detReasoning, rule, len(d.det.text), keep)
				if g.mode == loopGuardObserve {
					observed = true
					continue
				}
				httpStreamClose(hostStream)
				g.truncateCurrent(d.det == &detReasoning, string(d.det.text[:keep]))
				g.recoverLoop(attempt, d.det == &detReasoning, rule)
				return
			}
			if out != nil && !g.emit(out) {
				httpStreamClose(hostStream)
				streamClose(g.downstream, "")
				return
			}
		}
		if chunk.Error != "" || chunk.Done {
			httpStreamClose(hostStream)
			g.logf("END attempt=%d loops=%d chars=%d", attempt, g.loops, g.chars)
			streamClose(g.downstream, chunk.Error)
			return
		}
	}
}

// translate tracks one upstream event and returns what to send downstream plus any new output text.
func (g *guard) translate(ev sseEvent, attempt int, det, detReasoning *loopDetector) (out []byte, piece, reasoningPiece string, ok bool) {
	if !g.anthropic {
		if string(ev.data) == "[DONE]" {
			return []byte("data: [DONE]\n\n"), "", "", true
		}
		var chunk map[string]any
		if json.Unmarshal(ev.data, &chunk) != nil {
			return nil, "", "", false
		}
		g.lastChunk = chunk
		choices, _ := chunk["choices"].([]any)
		if len(choices) > 0 {
			choice, _ := choices[0].(map[string]any)
			delta, _ := choice["delta"].(map[string]any)
			if delta != nil {
				if attempt > 0 {
					delete(delta, "role")
				}
				piece, _ = delta["content"].(string)
				reasoningPiece, _ = delta["reasoning_content"].(string)
				if reasoningPiece == "" {
					reasoningPiece, _ = delta["reasoning"].(string)
				}
				if delta["tool_calls"] != nil {
					g.toolCalls = true
				}
				g.content += piece
				g.reasoning += reasoningPiece
			}
		}
		return encodeSSE("", chunk), piece, reasoningPiece, true
	}

	var msg map[string]any
	if json.Unmarshal(ev.data, &msg) != nil {
		return nil, "", "", false
	}
	switch msg["type"] {
	case "message_start":
		if attempt > 0 {
			return nil, "", "", false
		}
	case "content_block_start":
		block, _ := msg["content_block"].(map[string]any)
		g.blocks = append(g.blocks, map[string]any{})
		for k, v := range block {
			g.blocks[len(g.blocks)-1][k] = v
		}
		*det, *detReasoning = loopDetector{}, loopDetector{runOn: true}
		msg["index"] = len(g.blocks) - 1
	case "content_block_delta", "content_block_stop":
		if len(g.blocks) == 0 {
			break
		}
		msg["index"] = len(g.blocks) - 1
		block := g.blocks[len(g.blocks)-1]
		delta, _ := msg["delta"].(map[string]any)
		switch delta["type"] {
		case "thinking_delta":
			reasoningPiece, _ = delta["thinking"].(string)
			block["thinking"] = str(block["thinking"]) + reasoningPiece
		case "text_delta":
			piece, _ = delta["text"].(string)
			block["text"] = str(block["text"]) + piece
		case "signature_delta":
			block["signature"], _ = delta["signature"].(string)
		}
	}
	return encodeSSE(ev.name, msg), piece, reasoningPiece, true
}

// truncateCurrent cuts the looping text back to its first copy of the repeated unit.
func (g *guard) truncateCurrent(reasoning bool, keep string) {
	if !g.anthropic {
		if reasoning {
			g.reasoning = keep
		} else {
			g.content = keep
		}
		return
	}
	block := g.blocks[len(g.blocks)-1]
	if reasoning {
		block["thinking"] = keep
	} else {
		block["text"] = keep
	}
}

// recoverLoop ends the response after a loop in the answer, or asks the model to finish after a
// loop in thinking.
func (g *guard) recoverLoop(attempt int, reasoning bool, rule string) {
	if g.anthropic {
		g.emit(encodeSSE("content_block_stop", map[string]any{"type": "content_block_stop", "index": len(g.blocks) - 1}))
	}
	if !reasoning || attempt >= loopMaxRetries || g.hasToolUse() {
		g.finish()
		return
	}

	body := map[string]any{}
	for k, v := range g.body {
		body[k] = v
	}
	messages, _ := g.body["messages"].([]any)
	body["messages"] = append(append([]any{}, messages...), g.assistantMessage(), map[string]any{"role": "user", "content": loopNudges[rule]})
	payload, _ := json.Marshal(body)

	stream, err := httpDoStream(g.r.HostCallbackID, "POST", g.url, g.headers, payload)
	if err != nil {
		g.logf("continue failed: %v", err)
		g.finish()
		return
	}
	if stream.StatusCode >= 400 {
		g.logf("continue failed: %d %s", stream.StatusCode, truncate(drainStreamError(stream.StreamID), 300))
		g.finish()
		return
	}
	g.pump(stream.StreamID, attempt+1)
}

func (g *guard) hasToolUse() bool {
	if !g.anthropic {
		return g.toolCalls
	}
	for _, b := range g.blocks {
		if b["type"] == "tool_use" {
			return true
		}
	}
	return false
}

// assistantMessage is the output so far as plain text: chat templates drop an earlier turn's thinking.
func (g *guard) assistantMessage() map[string]any {
	var parts []string
	if !g.anthropic {
		parts = []string{g.reasoning, g.content}
	}
	for _, b := range g.blocks {
		parts = append(parts, str(b["thinking"]), str(b["text"]))
	}
	return map[string]any{"role": "assistant", "content": strings.TrimSpace(strings.Join(parts, "\n\n"))}
}

// finish closes the client stream as a normal end of turn.
func (g *guard) finish() {
	g.logf("END stopped loops=%d chars=%d", g.loops, g.chars)
	if g.anthropic {
		g.emit(encodeSSE("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil}, "usage": map[string]any{"output_tokens": g.chars / 4}}))
		g.emit(encodeSSE("message_stop", map[string]any{"type": "message_stop"}))
	} else {
		chunk := map[string]any{"object": "chat.completion.chunk", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}}
		for _, k := range []string{"id", "created", "model"} {
			if v, ok := g.lastChunk[k]; ok {
				chunk[k] = v
			}
		}
		g.emit(encodeSSE("", chunk))
		g.emit([]byte("data: [DONE]\n\n"))
	}
	streamClose(g.downstream, "")
}
