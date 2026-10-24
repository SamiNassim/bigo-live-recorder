package recording

import (
	"strings"
	"testing"
)

func TestSafeName(t *testing.T) {
	for _, input := range []string{"../../escape", `a:b/c\d?*`, "CON", "LPT1", "NUL.txt", "\n", "...", strings.Repeat("界", 100)} {
		got := safeName(input)
		if got == "" || got == "CON" || got == "LPT1" || strings.ContainsAny(got, `<>:"/\|?*`) || strings.HasSuffix(got, ".") || len(got) > 94 {
			t.Errorf("unsafe %q -> %q", input, got)
		}
	}
}

func TestTailBounded(t *testing.T) {
	var tail tailBuffer
	_, _ = tail.Write([]byte(strings.Repeat("x", 100000)))
	_, _ = tail.Write([]byte("end"))
	if len(tail.String()) != 8192 || !strings.HasSuffix(tail.String(), "end") {
		t.Fatal("tail lost or not bounded")
	}
}
