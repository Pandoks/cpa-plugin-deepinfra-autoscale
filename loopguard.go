package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"hash"
	"hash/fnv"
	"maps"
	"regexp"
	"strings"
	"sync/atomic"
	"unicode/utf8"
)

// Loop guard: a model's loop_guard setting watches its streamed output for degeneration. "observe"
// only logs a loop. "on" holds streamed text back while a loop could be forming, so when the output
// loops it is cut before the repeated copies reach the client; the request is then sent again with
// the output so far and a note asking the model to finish.
const (
	loopGuardOff     = "off"
	loopGuardObserve = "observe"
	loopGuardOn      = "on"
)

// Output is looping once some loopChunk bytes of it, with whitespace runs collapsed and each digit run
// read as one 0 (so a counter like "pass 67" and "pass 68" doesn't hide a copy), have recurred
// loopRepeats times without overlapping. Reasoning also counts as looping once loopRunOn bytes pass
// with no sentence break or newline: under a presence penalty the model trails off into an endless
// word list instead of repeating itself. Answers are exempt because they can be one long line of JSON.
const (
	loopChunk      = 128
	loopRepeats    = 6
	loopRunOn      = 1500
	loopMaxRetries = 2
)

// A copy only counts toward a loop if it starts at most loopStale bytes (whitespace collapsed) after
// the chunk's previous copy; a later one starts the count over. A loop is cut at its chunk's second
// copy but found only at the loopRepeats-th, so "on" holds output from the second copy of any chunk
// that can still reach loopRepeats, until it loops or goes stale. In the 1757 repeat loops logged by
// the guard so far, copies were at most 21427 bytes apart, so all are still found and none leak.
// Normal output is held 132 bytes at the median and 4.7 KB at the 99th percentile.
const loopStale = 24576

const (
	ruleRepeat = "repeat"
	ruleRunOn  = "run-on"
)

type chunkSeen struct{ last, count, second int }

type loopDetector struct {
	runOn    bool   // apply the run-on rule
	runStart int    // raw offset just past the last sentence break
	seedLen  int    // raw bytes of text the client already had when this detector started
	text     []byte // raw output
	norm     []byte // output with whitespace runs collapsed to one space and digit runs to one 0
	raw      []int  // raw offset of each norm byte
	seen     map[uint64]*chunkSeen
	live     []*chunkSeen // chunks seen at least twice, maybe still recurring
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
		} else if '0' <= c && c <= '9' {
			if p := len(d.text) - 2; p >= 0 && '0' <= d.text[p] && d.text[p] <= '9' {
				continue
			}
			c = '0'
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
			// The chunk recurs overlapping itself: the text repeats with a period under loopChunk, so
			// the cut goes here and keeps one period, if the run goes on to a counted copy.
			if e.count == 1 && e.second == 0 {
				e.second = d.raw[at]
				d.live = append(d.live, e)
			}
			continue
		}
		if at-e.last > loopStale {
			e.last, e.count, e.second = at, 1, 0
			continue
		}
		if e.second == 0 || e.count == 1 && at-e.last > 2*loopChunk {
			e.second = d.raw[at]
			d.live = append(d.live, e)
		}
		e.last, e.count = at, e.count+1
		if e.count >= loopRepeats {
			return e.second, ruleRepeat
		}
	}
	return 0, ""
}

// seed starts the detector on text the client already has from an earlier attempt, so copies in a
// retry count against copies already sent. A cut can't reach into that text, so the next copy of a
// seeded chunk becomes its cut point, and a chunk of the loop just cut ends the retry at its next copy.
func (d *loopDetector) seed(text string, cut map[uint64]bool) {
	runOn := d.runOn
	d.runOn = false
	d.add(text)
	d.runOn, d.runStart, d.seedLen, d.live = runOn, len(d.text), len(d.text), nil
	for k, e := range d.seen {
		e.second = 0
		if cut[k] {
			e.count, e.last = loopRepeats-1, len(d.norm)-loopChunk
		}
	}
}

// loopChunks returns the chunks repeated from offset keep on: the loop being cut there.
func (d *loopDetector) loopChunks(keep int, into map[uint64]bool) {
	for k, e := range d.seen {
		if e.count >= 2 && e.second >= keep {
			into[k] = true
		}
	}
}

// hold returns the earliest raw offset that could still become a keep point: the newest chunk, the
// second copy of a chunk still recurring (or its overlapping recurrence, until the run ends), or the start of the current run-on stretch, which is the
// text since the last sentence break. Output before it is safe to send.
func (d *loopDetector) hold() int {
	h := len(d.text)
	if len(d.norm) > 0 {
		h = d.raw[max(0, len(d.norm)-loopChunk)]
	}
	live := d.live[:0]
	for _, e := range d.live {
		if e.second > 0 && (e.count >= 2 && len(d.norm)-e.last <= loopStale+loopChunk || len(d.norm)-e.last <= 3*loopChunk) {
			live = append(live, e)
			h = min(h, e.second)
		}
	}
	d.live = live
	if d.runOn {
		h = min(h, d.runStart)
	}
	for h > 0 && h < len(d.text) && !utf8.RuneStart(d.text[h]) {
		h--
	}
	return h
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

// loopNudges are sent as the user turn after the output so far. A plain user turn gets answered and
// narrated ("The user says my response was cut off…"), so they are worded as the system reminders
// Claude Code puts in user turns, which the model treats as context rather than something to reply to.
var loopNudges = map[string]string{
	ruleRepeat: "<system-reminder>Your previous message stopped where it began repeating itself. Continue the task from exactly that point, without writing any of that text again. This is an automatic note, not a message from the user: do not mention it, the stop or the repetition.</system-reminder>",
	ruleRunOn:  "<system-reminder>Your previous message stopped where your reasoning turned into a list of words with no sentences. Continue the task from exactly that point, in sentences. This is an automatic note, not a message from the user: do not mention it, the stop or the list.</system-reminder>",
}

// echoesNudge reports whether a retry's answer opens by talking about the cut, as models do when
// they narrate the note above.
var echoesNudge = regexp.MustCompile(`(?i)^[^\n]{0,200}?\b(cut off|cut short|truncated|repeat(ed|ing)?|repetition|degenerat\w*|loop(ed|ing)?)\b`).MatchString

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
	// compaction is Claude Code asking for a summary; end holds its upstream message_delta and
	// message_stop until the reply is known to have text.
	compaction, thinkingOff bool
	end                     [][]byte

	// The current attempt's detectors, and its output held back from the client.
	det, detReasoning  loopDetector
	observed, echoSeen bool
	held               []outEvent
	// cutChunks are the chunks of loops cut so far, and attemptStart the first block of this attempt.
	cutChunks    map[uint64]bool
	attemptStart int

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

func (g *guard) emit(payloads ...[]byte) bool {
	for _, p := range payloads {
		if streamEmit(g.downstream, p) != nil {
			return false
		}
	}
	return true
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
	g.startAttempt(attempt)
	for {
		chunk, err := httpStreamRead(hostStream)
		if err != nil {
			httpStreamClose(hostStream)
			g.emit(append(g.flush(), g.end...)...)
			streamClose(g.downstream, err.Error())
			return
		}
		for _, ev := range rd.feed(chunk.Payload) {
			out, rule := g.step(ev, attempt)
			if !g.emit(out...) {
				httpStreamClose(hostStream)
				streamClose(g.downstream, "")
				return
			}
			if rule != "" {
				httpStreamClose(hostStream)
				g.recoverLoop(attempt, rule)
				return
			}
		}
		if chunk.Error != "" || chunk.Done {
			httpStreamClose(hostStream)
			g.emit(g.flush()...)
			g.logf("END attempt=%d loops=%d chars=%d", attempt, g.loops, g.chars)
			if chunk.Error == "" && g.emptySummary() && g.retryThinkingOff(attempt) {
				return
			}
			g.emit(g.end...)
			streamClose(g.downstream, chunk.Error)
			return
		}
	}
}

// startAttempt resets the detectors for an upstream attempt. An OpenAI retry continues the same
// message, so its detectors start on the message so far.
func (g *guard) startAttempt(attempt int) {
	g.det, g.detReasoning, g.observed, g.echoSeen, g.attemptStart = loopDetector{}, loopDetector{runOn: true}, false, false, len(g.blocks)
	if attempt > 0 && !g.anthropic {
		g.det.seed(g.content, g.cutChunks)
		g.detReasoning.seed(g.reasoning, g.cutChunks)
	}
}

// outEvent is one translated event on its way to the client.
type outEvent struct {
	raw       []byte         // sent as is when msg is nil
	name      string         // SSE event name
	msg       map[string]any // the event, encoded when sent
	delta     map[string]any // holds the event's output text under key; nil if it has none
	key       string
	reasoning bool
	start, n  int // offset of the text in its detector's output, and the output bytes it carries
}

func (e *outEvent) encode() []byte {
	if e.msg == nil {
		return e.raw
	}
	return encodeSSE(e.name, e.msg)
}

// step tracks one upstream event and returns the payloads now due to the client. With "on", output
// text is held while it could be cut, and any other event first flushes what is held. A non-empty
// rule means the output looped and was cut there.
func (g *guard) step(ev sseEvent, attempt int) (out [][]byte, rule string) {
	e, piece, reasoningPiece, ok := g.translate(ev, attempt)
	if !ok {
		return nil, ""
	}
	g.chars += len(piece) + len(reasoningPiece)
	if g.compaction && (e.msg["type"] == "message_delta" || e.msg["type"] == "message_stop") {
		g.end = append(g.end, e.encode())
		return g.flush(), ""
	}
	if g.mode != loopGuardOn || e.delta == nil {
		if ev.name != "ping" {
			out = g.flush()
		}
		out = append(out, e.encode())
	} else {
		e.start, e.n = len(g.det.text), len(piece)+len(reasoningPiece)
		if e.reasoning {
			e.start = len(g.detReasoning.text)
		}
		g.held = append(g.held, e)
	}
	for _, d := range []struct {
		det  *loopDetector
		text string
	}{{&g.det, piece}, {&g.detReasoning, reasoningPiece}} {
		if d.text == "" || g.observed {
			continue
		}
		keep, rule := d.det.add(d.text)
		if rule == "" {
			continue
		}
		reasoning := d.det == &g.detReasoning
		g.loops++
		g.logf("LOOP attempt=%d reasoning=%v rule=%s at=%d keep=%d", attempt, reasoning, rule, len(d.det.text), keep)
		if g.mode == loopGuardObserve {
			g.observed = true
			continue
		}
		for keep > 0 && !utf8.RuneStart(d.det.text[keep]) {
			keep--
		}
		keep = max(keep, d.det.seedLen)
		if g.cutChunks == nil {
			g.cutChunks = map[uint64]bool{}
		}
		d.det.loopChunks(keep, g.cutChunks)
		if leaked := g.cut(reasoning, keep); leaked > 0 {
			g.logf("LEAK attempt=%d bytes=%d", attempt, leaked)
		}
		kept := d.det.text[:keep]
		if g.anthropic {
			kept = kept[d.det.seedLen:]
		}
		g.truncateCurrent(reasoning, string(kept))
		return append(out, g.flush()...), rule
	}
	// Log a retry's answer that opens by narrating the note it was sent.
	if t := g.det.text[g.det.seedLen:]; attempt > 0 && piece != "" && !g.echoSeen && (len(t) >= 200 || bytes.IndexByte(t, '\n') >= 0) {
		g.echoSeen = true
		if echoesNudge(string(t)) {
			g.logf("ECHO attempt=%d: %q", attempt, truncate(string(t), 120))
		}
	}
	return append(out, g.release()...), ""
}

// release returns the held events that can no longer be cut.
func (g *guard) release() (out [][]byte) {
	holds := [2]int{g.det.hold(), g.detReasoning.hold()}
	for len(g.held) > 0 {
		e, h := &g.held[0], holds[0]
		if e.reasoning {
			h = holds[1]
		}
		if e.start+e.n > h {
			break
		}
		out = append(out, e.encode())
		g.held = g.held[1:]
	}
	return out
}

func (g *guard) flush() (out [][]byte) {
	for i := range g.held {
		out = append(out, g.held[i].encode())
	}
	g.held = nil
	return out
}

// cut drops the held looping text from offset keep on, and returns how many bytes past keep were
// already released.
func (g *guard) cut(reasoning bool, keep int) (leaked int) {
	kept, first := g.held[:0], true
	for _, e := range g.held {
		if e.reasoning == reasoning {
			if first {
				leaked, first = max(0, e.start-keep), false
			}
			if e.start >= keep {
				continue
			}
			if text := str(e.delta[e.key]); e.start+len(text) > keep {
				e.delta[e.key] = text[:keep-e.start]
			}
		}
		kept = append(kept, e)
	}
	g.held = kept
	return leaked
}

// translate tracks one upstream event and returns what to send downstream plus any new output text.
func (g *guard) translate(ev sseEvent, attempt int) (e outEvent, piece, reasoningPiece string, ok bool) {
	if !g.anthropic {
		if string(ev.data) == "[DONE]" {
			return outEvent{raw: []byte("data: [DONE]\n\n")}, "", "", true
		}
		var chunk map[string]any
		if json.Unmarshal(ev.data, &chunk) != nil {
			return e, "", "", false
		}
		g.lastChunk = chunk
		e.msg = chunk
		choices, _ := chunk["choices"].([]any)
		if len(choices) > 0 {
			choice, _ := choices[0].(map[string]any)
			delta, _ := choice["delta"].(map[string]any)
			if delta != nil {
				if attempt > 0 {
					delete(delta, "role")
				}
				piece, _ = delta["content"].(string)
				reasoningKey := "reasoning_content"
				reasoningPiece, _ = delta[reasoningKey].(string)
				if reasoningPiece == "" {
					reasoningKey = "reasoning"
					reasoningPiece, _ = delta[reasoningKey].(string)
				}
				if delta["tool_calls"] != nil {
					g.toolCalls = true
				}
				g.content += piece
				g.reasoning += reasoningPiece
				// Held chunks carry one kind of text and nothing else, so a cut can trim them.
				if (piece == "") != (reasoningPiece == "") && delta["tool_calls"] == nil && choice["finish_reason"] == nil {
					e.delta, e.key, e.reasoning = delta, "content", reasoningPiece != ""
					if e.reasoning {
						e.key = reasoningKey
					}
				}
			}
		}
		return e, piece, reasoningPiece, true
	}

	var msg map[string]any
	if json.Unmarshal(ev.data, &msg) != nil {
		return e, "", "", false
	}
	e.name, e.msg = ev.name, msg
	switch msg["type"] {
	case "message_start":
		if attempt > 0 {
			return e, "", "", false
		}
	case "content_block_start":
		block, _ := msg["content_block"].(map[string]any)
		g.blocks = append(g.blocks, map[string]any{})
		for k, v := range block {
			g.blocks[len(g.blocks)-1][k] = v
		}
		g.det, g.detReasoning, g.echoSeen = loopDetector{}, loopDetector{runOn: true}, false
		// A retry's block continues the same kind of block from earlier attempts.
		for i := g.attemptStart - 1; attempt > 0 && i >= 0; i-- {
			if b := g.blocks[i]; b["type"] == block["type"] {
				if b["type"] == "thinking" {
					g.detReasoning.seed(str(b["thinking"]), g.cutChunks)
				} else {
					g.det.seed(str(b["text"]), g.cutChunks)
				}
				break
			}
		}
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
			e.delta, e.key, e.reasoning = delta, "thinking", true
		case "text_delta":
			piece, _ = delta["text"].(string)
			block["text"] = str(block["text"]) + piece
			e.delta, e.key = delta, "text"
		case "signature_delta":
			block["signature"], _ = delta["signature"].(string)
		}
	}
	return e, piece, reasoningPiece, true
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

// recoverLoop asks the model to finish after a loop, at most loopMaxRetries times and never after a
// tool call; otherwise it ends the response where the loop was cut.
func (g *guard) recoverLoop(attempt int, rule string) {
	if g.anthropic {
		g.emit(encodeSSE("content_block_stop", map[string]any{"type": "content_block_stop", "index": len(g.blocks) - 1}))
	}
	if attempt >= loopMaxRetries || g.hasToolUse() {
		g.finish()
		return
	}
	payload, _ := json.Marshal(loopRetry(g.body, g.assistantMessage(), rule))
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

// loopRetry is the recovery recipe: the request a retry sends, built from the original request, the
// output so far and the rule that fired. The default appends the output and a note asking the model
// to continue without repeating itself.
var loopRetry = func(original, assistant map[string]any, rule string) map[string]any {
	body := map[string]any{}
	for k, v := range original {
		body[k] = v
	}
	messages, _ := original["messages"].([]any)
	body["messages"] = append(append([]any{}, messages...), assistant, map[string]any{"role": "user", "content": loopNudges[rule]})
	return body
}

// emptySummary reports whether a compaction reply ended with no text. Claude Code reads the summary
// from the text and fails the compaction without it, even when the summary is in thinking.
func (g *guard) emptySummary() bool {
	if !g.compaction || g.thinkingOff || g.hasToolUse() {
		return false
	}
	for _, b := range g.blocks {
		if strings.TrimSpace(str(b["text"])) != "" {
			return false
		}
	}
	return true
}

// retryThinkingOff sends the request again with thinking off, so the model can only answer in text.
func (g *guard) retryThinkingOff(attempt int) bool {
	g.thinkingOff = true
	body := maps.Clone(g.body)
	body["thinking"] = map[string]any{"type": "disabled"}
	payload, _ := json.Marshal(body)
	stream, err := httpDoStream(g.r.HostCallbackID, "POST", g.url, g.headers, payload)
	if err != nil {
		g.logf("thinking-off retry failed: %v", err)
		return false
	}
	if stream.StatusCode >= 400 {
		g.logf("thinking-off retry failed: %d %s", stream.StatusCode, truncate(drainStreamError(stream.StreamID), 300))
		return false
	}
	g.logf("EMPTY attempt=%d: compaction reply had no text, retrying with thinking off", attempt)
	g.end = nil
	g.pump(stream.StreamID, attempt+1)
	return true
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
