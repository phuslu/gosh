package runes

import (
	"reflect"
	"testing"
)

type twidth struct {
	r      []rune
	length int
}

func TestSingleRuneWidth(t *testing.T) {
	type test struct {
		r rune
		w int
	}

	tests := []test{
		{0, 0}, // default rune is 0 - default mask
		{'a', 1},
		{'☭', 1},
		{'你', 2},
		{'日', 2}, // kanji
		{'ｶ', 1}, // half-width katakana
		{'カ', 2}, // full-width katakana
		{'ひ', 2}, // full-width hiragana
		{'Ｗ', 2}, // full-width romanji
		{'）', 2}, // full-width symbols
		{'😅', 2}, // emoji
	}

	for _, test := range tests {
		if w := Width(test.r); w != test.w {
			t.Error("result is not expected", string(test.r), test.w, w)
		}
	}
}

func TestRuneWidth(t *testing.T) {
	rs := []twidth{
		{[]rune(""), 0},
		{[]rune("☭"), 1},
		{[]rune("a"), 1},
		{[]rune("你"), 2},
		{ColorFilter([]rune("☭\033[13;1m你")), 3},
		{[]rune("漢字"), 4},     // kanji
		{[]rune("ｶﾀｶﾅ"), 4},   // half-width katakana
		{[]rune("カタカナ"), 8},   // full-width katakana
		{[]rune("ひらがな"), 8},   // full-width hiragana
		{[]rune("ＷＩＤＥ"), 8},   // full-width romanji
		{[]rune("ー。"), 4},     // full-width symbols
		{[]rune("안녕하세요"), 10}, // full-width Hangul
		{[]rune("😅"), 2},      // emoji
	}
	for _, r := range rs {
		if w := WidthAll(r.r); w != r.length {
			t.Error("result is not expected", string(r.r), r.length, w)
		}
	}
}

func TestColorFilter(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"abc", "abc"},
		{"\033[31mred\033[0m", "red"},
		// A lone escape at the very end must not read past the slice.
		{"\033", "\033"},
		{"prompt\033", "prompt\033"},
		// An unterminated CSI keeps its existing (escape-dropping) behavior.
		{"\033[", "["},
		{"a\033[31", "a[31"},
		{"\033[31mred\033", "red\033"},
	}
	for _, test := range tests {
		got := string(ColorFilter([]rune(test.in)))
		if got != test.want {
			t.Errorf("ColorFilter(%q) = %q, want %q", test.in, got, test.want)
		}
	}
}

type tagg struct {
	r      [][]rune
	e      [][]rune
	length int
}

func TestAggRunes(t *testing.T) {
	rs := []tagg{
		{
			[][]rune{[]rune("ab"), []rune("a"), []rune("abc")},
			[][]rune{[]rune("b"), []rune(""), []rune("bc")},
			1,
		},
		{
			[][]rune{[]rune("addb"), []rune("ajkajsdf"), []rune("aasdfkc")},
			[][]rune{[]rune("ddb"), []rune("jkajsdf"), []rune("asdfkc")},
			1,
		},
		{
			[][]rune{[]rune("ddb"), []rune("ajksdf"), []rune("aasdfkc")},
			[][]rune{[]rune("ddb"), []rune("ajksdf"), []rune("aasdfkc")},
			0,
		},
		{
			[][]rune{[]rune("ddb"), []rune("ddajksdf"), []rune("ddaasdfkc")},
			[][]rune{[]rune("b"), []rune("ajksdf"), []rune("aasdfkc")},
			2,
		},
	}
	for _, r := range rs {
		same, off := Aggregate(r.r)
		if off != r.length {
			t.Fatal("result not expect", off)
		}
		if len(same) != off {
			t.Fatal("result not expect", same)
		}
		if !reflect.DeepEqual(r.r, r.e) {
			t.Fatal("result not expect")
		}
	}
}
