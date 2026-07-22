package diff

import (
	"strings"
	"testing"
)

func apply(ops []Op) (a, b string) {
	var as, bs []string
	for _, o := range ops {
		switch o.Kind {
		case ' ':
			as = append(as, o.Line)
			bs = append(bs, o.Line)
		case '-':
			as = append(as, o.Line)
		case '+':
			bs = append(bs, o.Line)
		}
	}
	return strings.Join(as, "\n"), strings.Join(bs, "\n")
}

func roundtrip(t *testing.T, a, b string) []Op {
	t.Helper()
	ops := Lines(a, b)
	ga, gb := apply(ops)
	if ga != strings.TrimSuffix(a, "\n") || gb != strings.TrimSuffix(b, "\n") {
		t.Fatalf("diff does not reconstruct inputs\n a=%q got %q\n b=%q got %q", a, ga, b, gb)
	}
	return ops
}

func TestIdentical(t *testing.T) {
	ops := roundtrip(t, "x\ny\n", "x\ny\n")
	if Unified(ops, "a", "b", 3, false) != "" {
		t.Fatal("expected empty unified diff for identical inputs")
	}
}

func TestSimpleChange(t *testing.T) {
	ops := roundtrip(t, "one\ntwo\nthree\n", "one\nTWO\nthree\n")
	u := Unified(ops, "a", "b", 1, false)
	for _, want := range []string{"-two", "+TWO", "@@ -1,3 +1,3 @@", "--- a", "+++ b"} {
		if !strings.Contains(u, want) {
			t.Errorf("unified diff missing %q:\n%s", want, u)
		}
	}
}

func TestEmptySides(t *testing.T) {
	roundtrip(t, "", "a\nb\n")
	roundtrip(t, "a\nb\n", "")
	roundtrip(t, "", "")
}

func TestInsertDelete(t *testing.T) {
	a := "a\nb\nc\nd\ne\n"
	b := "a\nc\nd\nX\ne\n"
	ops := roundtrip(t, a, b)
	u := Unified(ops, "a", "b", 3, false)
	if !strings.Contains(u, "-b") || !strings.Contains(u, "+X") {
		t.Fatalf("bad diff:\n%s", u)
	}
}

func TestTwoHunks(t *testing.T) {
	var as, bs []string
	for i := 0; i < 30; i++ {
		l := strings.Repeat("l", 1) + string(rune('a'+i%26))
		as = append(as, l)
		bs = append(bs, l)
	}
	bs[2] = "CHANGED-EARLY"
	bs[27] = "CHANGED-LATE"
	ops := roundtrip(t, strings.Join(as, "\n"), strings.Join(bs, "\n"))
	u := Unified(ops, "a", "b", 2, false)
	if strings.Count(u, "@@") != 4 { // 2 hunks x 2 @@ markers each
		t.Fatalf("expected 2 hunks, got:\n%s", u)
	}
}

func TestColor(t *testing.T) {
	ops := Lines("a\n", "b\n")
	u := Unified(ops, "x", "y", 3, true)
	if !strings.Contains(u, "\x1b[31m-a") || !strings.Contains(u, "\x1b[32m+b") {
		t.Fatalf("expected colored output:\n%s", u)
	}
}

func TestRandomRoundtrip(t *testing.T) {
	rnd := uint64(12345)
	next := func(n int) int {
		rnd = rnd*6364136223846793005 + 1442695040888963407
		return int(rnd>>33) % n
	}
	words := []string{"alpha", "beta", "gamma", "delta", "", "x", "long line here"}
	for iter := 0; iter < 200; iter++ {
		var as, bs []string
		for i := 0; i < next(40); i++ {
			as = append(as, words[next(len(words))])
		}
		for i := 0; i < next(40); i++ {
			bs = append(bs, words[next(len(words))])
		}
		roundtrip(t, strings.Join(as, "\n"), strings.Join(bs, "\n"))
	}
}
