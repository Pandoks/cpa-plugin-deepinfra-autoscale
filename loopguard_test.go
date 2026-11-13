package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func feed(d *loopDetector, s string) (int, string) {
	for len(s) > 0 {
		n := min(7, len(s))
		if keep, rule := d.add(s[:n]); rule != "" {
			return keep, rule
		}
		s = s[n:]
	}
	return 0, ""
}

// wordList is a word list that never repeats a phrase, like the synonym chains RED-SNOW emits under
// a presence penalty.
func wordList(n int) string {
	var list strings.Builder
	for x := uint32(1); list.Len() < n; {
		x = x*1664525 + 1013904223
		list.WriteString(string(rune('a'+x>>27%26)) + string(rune('a'+x>>22%26)) + string(rune('a'+x>>17%26)) + "ing ")
	}
	return list.String()
}

func TestLoopDetectorRunOnWordList(t *testing.T) {
	head := "The answer is the function above. Let me double check the edge cases.\n"
	d := loopDetector{runOn: true}
	keep, rule := feed(&d, head+wordList(3000))
	if rule != ruleRunOn || keep != len(head) {
		t.Fatalf("got keep=%d rule=%q, want keep=%d rule=%q", keep, rule, len(head), ruleRunOn)
	}
}

func TestLoopDetectorRunOnAllowsProse(t *testing.T) {
	// Non-repeating sentences of ten words each, with the punctuation reasoning normally has.
	var text, cjk strings.Builder
	x := uint32(7)
	for i := 0; text.Len() < 20000; i++ {
		for w := 0; w < 10; w++ {
			x = x*1664525 + 1013904223
			text.WriteString(string(rune('a'+x>>27%26)) + string(rune('a'+x>>22%26)) + "ed ")
			cjk.WriteRune(rune(0x4e00 + x>>20%2000))
		}
		text.WriteString([]string{"done. ", "right? ", "so: ", "yes!\n", "then; "}[i%5])
		cjk.WriteString([]string{"。", "？", "！"}[i%3])
	}
	for name, s := range map[string]string{"latin": text.String(), "cjk": cjk.String()} {
		d := loopDetector{runOn: true}
		if keep, rule := feed(&d, s); rule != "" {
			t.Fatalf("%s prose flagged: keep=%d rule=%q", name, keep, rule)
		}
	}
}

func TestLoopDetectorAnswerExemptFromRunOn(t *testing.T) {
	var j strings.Builder
	for i := 0; j.Len() < 5000; i++ {
		j.WriteString(`{"id":` + strings.Repeat("1", i%9+1) + `,"name":"item` + strings.Repeat("z", i%13) + `"},`)
	}
	d := loopDetector{}
	if keep, rule := feed(&d, j.String()); rule == ruleRunOn {
		t.Fatalf("answer JSON flagged as run-on at keep=%d", keep)
	}
}

func TestLoopDetectorRepeat(t *testing.T) {
	unit := "```python\ndef median(l: list):\n    s = sorted(l)\n    n = len(s)\n    return s[n // 2] if n % 2 else (s[n // 2 - 1] + s[n // 2]) / 2\n```\nFinal.\n"
	d := loopDetector{runOn: true}
	text := "Here it is.\n" + strings.Repeat(unit, 10)
	keep, rule := feed(&d, text)
	if rule != ruleRepeat || strings.Count(text[:keep], "def median") != 1 {
		t.Fatalf("got keep=%d rule=%q, want one copy kept and rule %q", keep, rule, ruleRepeat)
	}
}

// sse frames one Anthropic stream event as upstream sends it.
func sse(name string, data map[string]any) sseEvent {
	raw, _ := json.Marshal(data)
	return sseEvent{name: name, data: raw}
}

func textDeltas(index int, kind, key, text string) (evs []sseEvent) {
	for len(text) > 0 {
		n := min(23, len(text))
		evs = append(evs, sse("content_block_delta", map[string]any{"type": "content_block_delta", "index": index, "delta": map[string]any{"type": kind, key: text[:n]}}))
		text = text[n:]
	}
	return evs
}

// streamGuard feeds one attempt's events through a guard until a loop is cut, calling sent (if set) with the
// output text the client has after each event.
func streamGuard(g *guard, attempt int, evs []sseEvent, sent func(text string)) (got []sseEvent, rule string) {
	var rd sseReader
	text := ""
	for _, ev := range evs {
		var out [][]byte
		out, rule = g.step(ev, attempt)
		for _, p := range out {
			for _, e := range rd.feed(p) {
				got = append(got, e)
				var msg map[string]any
				_ = json.Unmarshal(e.data, &msg)
				if delta, ok := msg["delta"].(map[string]any); ok {
					text += str(delta["text"]) + str(delta["thinking"])
				}
			}
		}
		if rule != "" {
			break
		}
		if sent != nil {
			sent(text)
		}
	}
	return got, rule
}

func blockEvents(kind, text string) []sseEvent {
	key := map[string]string{"text": "text", "thinking": "thinking"}[kind]
	evs := []sseEvent{sse("content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": kind, key: ""}})}
	return append(evs, textDeltas(0, kind+"_delta", key, text)...)
}

func TestGuardSendsNonRepeatingTextAtOnce(t *testing.T) {
	words := strings.Fields(wordList(40000))
	var prose strings.Builder
	for i := 0; prose.Len() < 20000; i += 3 {
		fmt.Fprintf(&prose, "The %s check covers the %s path and the %s case. ", words[i%len(words)], words[(i+1)%len(words)], words[(i+2)%len(words)])
	}
	for _, kind := range []string{"text", "thinking"} {
		g := &guard{r: &executorRequest{}, mode: loopGuardOn, anthropic: true}
		evs := blockEvents(kind, prose.String())
		_, rule := streamGuard(g, 0, evs, func(text string) {
			if fed := len(str(g.blocks[0][kind])); fed-len(text) > loopChunk+23 {
				t.Fatalf("%s: holding %d bytes of non-repeating output", kind, fed-len(text))
			}
		})
		if rule != "" {
			t.Fatalf("%s: prose flagged as %s", kind, rule)
		}
	}
}

func TestGuardCutsThinkingRunOnBeforeClient(t *testing.T) {
	head := "The answer is the function above. Let me double check the edge cases.\n"
	g := &guard{r: &executorRequest{}, mode: loopGuardOn, anthropic: true}
	got, rule := streamGuard(g, 0, blockEvents("thinking", head+wordList(3000)), nil)
	sent := ""
	for _, ev := range got {
		var msg map[string]any
		_ = json.Unmarshal(ev.data, &msg)
		if delta, ok := msg["delta"].(map[string]any); ok {
			sent += str(delta["thinking"])
		}
	}
	if rule != ruleRunOn || sent != head {
		t.Fatalf("rule=%q, client got %d bytes, want the %d before the word list", rule, len(sent), len(head))
	}
}

func TestGuardHoldbackCutsAnswerLoopBeforeClient(t *testing.T) {
	unit := "```python\ndef median(l: list):\n    s = sorted(l)\n    n = len(s)\n    return s[n // 2] if n % 2 else (s[n // 2 - 1] + s[n // 2]) / 2\n```\nFinal.\n"
	thinking := "The user wants a median function. Sorting first is simplest.\n"
	evs := []sseEvent{
		sse("message_start", map[string]any{"type": "message_start", "message": map[string]any{"id": "msg_1", "role": "assistant", "content": []any{}}}),
		sse("content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "thinking", "thinking": ""}}),
	}
	evs = append(evs, textDeltas(0, "thinking_delta", "thinking", thinking)...)
	evs = append(evs,
		sse("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0}),
		sse("content_block_start", map[string]any{"type": "content_block_start", "index": 1, "content_block": map[string]any{"type": "text", "text": ""}}))
	evs = append(evs, textDeltas(1, "text_delta", "text", "Here it is.\n"+strings.Repeat(unit, 10))...)

	g := &guard{r: &executorRequest{}, mode: loopGuardOn, anthropic: true,
		body: map[string]any{"messages": []any{map[string]any{"role": "user", "content": "median?"}}}}
	got, rule := streamGuard(g, 0, evs, nil)
	if rule != ruleRepeat {
		t.Fatalf("loop not detected: rule=%q", rule)
	}

	// What reached the client is a valid event sequence whose answer holds one copy of the unit.
	text, think, open, next := "", "", -1, 0
	for i, ev := range got {
		var msg map[string]any
		if err := json.Unmarshal(ev.data, &msg); err != nil || msg["type"] != ev.name {
			t.Fatalf("event %d: bad frame %q %s", i, ev.name, ev.data)
		}
		index, _ := msg["index"].(float64)
		switch ev.name {
		case "message_start":
			if i != 0 {
				t.Fatalf("message_start at event %d", i)
			}
		case "content_block_start":
			if open != -1 || int(index) != next {
				t.Fatalf("event %d: block %d started with %d open, want %d", i, int(index), open, next)
			}
			open, next = next, next+1
		case "content_block_delta":
			if int(index) != open {
				t.Fatalf("event %d: delta for block %d while %d is open", i, int(index), open)
			}
			delta := msg["delta"].(map[string]any)
			text += str(delta["text"])
			think += str(delta["thinking"])
		case "content_block_stop":
			if int(index) != open {
				t.Fatalf("event %d: stop for block %d while %d is open", i, int(index), open)
			}
			open = -1
		}
	}
	if think != thinking || open != 1 {
		t.Fatalf("thinking=%q open=%d, want all thinking sent and the answer block open", think, open)
	}
	if n := strings.Count(text, "def median"); n != 1 {
		t.Fatalf("client got %d copies of the repeated unit:\n%s", n, text)
	}

	// The retry carries exactly what the client has, then asks the model to continue.
	if kept := str(g.blocks[1]["text"]); kept != text {
		t.Fatalf("retry would continue from %q, client has %q", kept, text)
	}
	messages := loopRetry(g.body, g.assistantMessage(), rule)["messages"].([]any)
	last := messages[len(messages)-1].(map[string]any)
	if len(messages) != 3 || !strings.HasSuffix(str(messages[1].(map[string]any)["content"]), strings.TrimSpace(text)) || last["content"] != loopNudges[ruleRepeat] {
		t.Fatalf("retry messages = %v", messages)
	}
}

func TestGuardHoldsSparseLoop(t *testing.T) {
	// Each copy of the unit is 5 KB of distinct text after the last: further apart than the first
	// holdback kept, so it sent the second copy before the loop was found.
	unit := "(Nothing further to add.) (End of report.) (The end.) (Really done.) (No more text.) (Stopping now.) (This is it.) (Goodbye.) (Silence.) (Final line.)\n"
	var text strings.Builder
	text.WriteString("Pass complete. ")
	for i := 0; i < 8; i++ {
		text.WriteString(unit)
		for j := 0; text.Len() < (i+1)*5200; j++ {
			fmt.Fprintf(&text, "Check %d.%d confirms that value %d sorts before %d. ", i, j, j*7, j*7+3)
		}
	}
	g := &guard{r: &executorRequest{}, mode: loopGuardOn, anthropic: true}
	got, rule := streamGuard(g, 0, blockEvents("text", text.String()), nil)
	sent := ""
	for _, ev := range got {
		var msg map[string]any
		_ = json.Unmarshal(ev.data, &msg)
		if delta, ok := msg["delta"].(map[string]any); ok {
			sent += str(delta["text"])
		}
	}
	if rule != ruleRepeat || strings.Count(sent, "(Final line.)") != 1 {
		t.Fatalf("rule=%q, client got %d copies of the unit", rule, strings.Count(sent, "(Final line.)"))
	}
}

func TestGuardRetriesCompactionWithoutText(t *testing.T) {
	end := []sseEvent{
		sse("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0}),
		sse("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn"}}),
		sse("message_stop", map[string]any{"type": "message_stop"}),
	}
	for _, c := range []struct {
		name        string
		compaction  bool
		blocks      []sseEvent
		retry, held bool
	}{
		{"compaction with the summary only in thinking", true, blockEvents("thinking", "<summary>All the work so far.</summary>"), true, true},
		{"compaction with text", true, append(blockEvents("thinking", "Summarizing."), blockEvents("text", "<summary>All the work so far.</summary>")[1:]...), false, true},
		{"other request with thinking only", false, blockEvents("thinking", "Nothing to say."), false, false},
	} {
		g := &guard{r: &executorRequest{}, mode: loopGuardOn, anthropic: true, compaction: c.compaction}
		got, _ := streamGuard(g, 0, append(c.blocks, end...), nil)
		sentEnd := got[len(got)-1].name == "message_stop"
		if g.emptySummary() != c.retry || sentEnd == c.held {
			t.Errorf("%s: retry=%v message_stop sent=%v, want retry=%v sent=%v", c.name, g.emptySummary(), sentEnd, c.retry, !c.held)
		}
	}
}

func TestLoopDetectorShortAndCountingRepeats(t *testing.T) {
	// From real sessions: a Q/A line whose pass number counts up, and two short units.
	var qa strings.Builder
	for i := 1; i <= 20; i++ {
		fmt.Fprintf(&qa, "Q%d: Was pass %d a converged pass? A%d: Yes. The diff is unchanged and every gate passed again.\n", i, 66+i, i)
	}
	for _, c := range []struct{ text, unit string }{
		{qa.String(), "converged"},
		{strings.Repeat("Done. No more. Executing. ", 60), "Executing"},
		{strings.Repeat("None\t", 300), "None"},
	} {
		d := loopDetector{}
		keep, rule := feed(&d, "Review complete.\n"+c.text)
		if kept := ("Review complete.\n" + c.text)[:keep]; rule != ruleRepeat || strings.Count(kept, c.unit) != 1 {
			t.Errorf("%q: rule=%q, kept %d copies, want the repeat cut after one", c.unit, rule, strings.Count(kept, c.unit))
		}
	}
}

func TestGuardRetryCutsLoopOnFirstReappearance(t *testing.T) {
	unit := "```python\ndef median(l: list):\n    s = sorted(l)\n    n = len(s)\n    return s[n // 2] if n % 2 else (s[n // 2 - 1] + s[n // 2]) / 2\n```\nFinal.\n"
	g := &guard{r: &executorRequest{}, mode: loopGuardOn, anthropic: true}
	g.startAttempt(0)
	if _, rule := streamGuard(g, 0, blockEvents("text", "Here it is.\n"+strings.Repeat(unit, 10)), nil); rule != ruleRepeat {
		t.Fatalf("first attempt: rule=%q", rule)
	}
	// The retry writes a sentence, then the unit the client already has once more.
	g.startAttempt(1)
	got, rule := streamGuard(g, 1, blockEvents("text", "Continuing with the remaining checks.\n"+unit+"All other functions are covered."), nil)
	sent := ""
	for _, ev := range got {
		var msg map[string]any
		_ = json.Unmarshal(ev.data, &msg)
		if delta, ok := msg["delta"].(map[string]any); ok {
			sent += str(delta["text"])
		}
	}
	if rule != ruleRepeat || strings.Contains(sent, "def median") || !strings.HasPrefix(sent, "Continuing") {
		t.Fatalf("retry: rule=%q, client got %q", rule, sent)
	}
}

func TestLoopRetryNoteIsNotNarrated(t *testing.T) {
	for rule := range loopNudges {
		messages := loopRetry(map[string]any{"messages": []any{}}, map[string]any{"role": "assistant", "content": "so far"}, rule)["messages"].([]any)
		note := str(messages[len(messages)-1].(map[string]any)["content"])
		if !strings.HasPrefix(note, "<system-reminder>") || !strings.Contains(note, "not a message from the user") {
			t.Errorf("%s: retry note %q is not a system reminder", rule, note)
		}
	}
	// From a real session: the retry's answer narrating the old note.
	if !echoesNudge("The user says my responses were cut off due to repetition once more. I'll give the final report once.\n") {
		t.Error("narrated note not detected")
	}
	if echoesNudge("Twenty-sixth pass complete. Found and closed the last untested line in the routing logic.\n") {
		t.Error("normal answer flagged as narrating the note")
	}
}
