package main

import (
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

func TestLoopDetectorRunOnWordList(t *testing.T) {
	head := "The answer is the function above. Let me double check the edge cases.\n"
	// A word list that never repeats a phrase, like the synonym chains RED-SNOW emits under a presence penalty.
	var list strings.Builder
	for x := uint32(1); list.Len() < 3000; {
		x = x*1664525 + 1013904223
		list.WriteString(string(rune('a'+x>>27%26)) + string(rune('a'+x>>22%26)) + string(rune('a'+x>>17%26)) + "ing ")
	}
	d := loopDetector{runOn: true}
	keep, rule := feed(&d, head+list.String())
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
