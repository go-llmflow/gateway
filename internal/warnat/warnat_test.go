package warnat

import (
	"reflect"
	"strings"
	"testing"
)

func TestValidate(t *testing.T) {
	ok := [][]int{nil, {}, {1}, {75, 95}, {1, 2, 3, 4, 5}, {50, 75, 95}, {98, 99}}
	for _, a := range ok {
		if err := Validate(a); err != nil {
			t.Errorf("%v: %v", a, err)
		}
	}
	bad := [][]int{{0}, {100}, {-5}, {75, 75}, {95, 75}, {1, 2, 3, 4, 5, 6}, {75, 100}}
	for _, a := range bad {
		if Validate(a) == nil {
			t.Errorf("%v accepted", a)
		}
	}
}

func TestParse(t *testing.T) {
	type c struct {
		in  string
		out []int
		set bool
		err bool
	}
	for _, x := range []c{
		{"", nil, false, false},
		{"  ", nil, false, false},
		{"off", []int{}, true, false},
		{"NONE", []int{}, true, false},
		{"[]", []int{}, true, false},
		{"75,95", []int{75, 95}, true, false},
		{"[50 75 95]", []int{50, 75, 95}, true, false},
		{"80, 95", []int{80, 95}, true, false},
		{"95,75", nil, false, true},
		{"0", nil, false, true},
		{"100", nil, false, true},
		{"7.5", nil, false, true},
		{"a", nil, false, true},
		{",", nil, false, true},
		{"1,2,3,4,5,6", nil, false, true},
		{strings.Repeat("1", MaxInput+1), nil, false, true},
	} {
		got, set, err := Parse(x.in)
		if (err != nil) != x.err || set != x.set || !reflect.DeepEqual(got, x.out) {
			t.Errorf("Parse(%q) = %v %v %v; want %v %v err=%v", x.in, got, set, err, x.out, x.set, x.err)
		}
	}
}

func TestFormatRoundTrip(t *testing.T) {
	for _, a := range [][]int{{}, {75, 95}, {1}, {10, 20, 30, 40, 50}} {
		got, set, err := Parse(Format(a))
		if err != nil || !set || !reflect.DeepEqual(got, a) {
			t.Errorf("%v -> %q -> %v %v %v", a, Format(a), got, set, err)
		}
	}
	if Format(nil) != "" {
		t.Error("nil is spelled empty")
	}
}

// The chain gateway -> org -> profile -> budget: the most specific set level
// wins; "off" (empty, not nil) at a level beats a default above it.
func TestResolve(t *testing.T) {
	gw, org, prof, bud := []int{70}, []int{80, 95}, []int{50, 75, 95}, []int{60}
	cases := []struct {
		name string
		got  []int
		want []int
	}{
		{"nothing set: Anthropic's pair", Resolve(nil, nil, nil, nil), []int{75, 95}},
		{"gateway only", Resolve(nil, nil, nil, gw), []int{70}},
		{"org beats gateway", Resolve(nil, nil, org, gw), []int{80, 95}},
		{"profile beats org", Resolve(nil, prof, org, gw), []int{50, 75, 95}},
		{"budget beats profile", Resolve(bud, prof, org, gw), []int{60}},
		{"off at the budget", Resolve([]int{}, prof, org, gw), []int{}},
		{"off at the org, profile sets again", Resolve(nil, prof, []int{}, gw), []int{50, 75, 95}},
		{"off at the gateway", Resolve(nil, nil, nil, []int{}), []int{}},
	}
	for _, c := range cases {
		if !reflect.DeepEqual(c.got, c.want) {
			t.Errorf("%s: %v, want %v", c.name, c.got, c.want)
		}
	}
	d := Default()
	d[0] = 1
	if Default()[0] != 75 {
		t.Error("Default hands out its own storage")
	}
}

func TestRearm(t *testing.T) {
	for th, want := range map[int]int{1: 1, 2: 1, 10: 5, 50: 45, 75: 70, 95: 90, 99: 94} {
		if got := RearmBelow(th); got != want {
			t.Errorf("RearmBelow(%d) = %d, want %d", th, got, want)
		}
	}
}

func TestCrossed(t *testing.T) {
	l := []int{50, 75, 95}
	for pct, want := range map[float64][]int{0: nil, 49.99: nil, 50: {50}, 80: {50, 75}, 100: {50, 75, 95}} {
		if got := Crossed(l, pct); !reflect.DeepEqual(got, want) {
			t.Errorf("Crossed(%v) = %v, want %v", pct, got, want)
		}
	}
}

// FuzzParse: whatever the text, Parse either fails or yields a valid list
// that formats and parses back to itself.
func FuzzParse(f *testing.F) {
	for _, s := range []string{"", "off", "75,95", "[1 2 3]", "0", "99,100", "1,1", "\x00", "9999999999999999999999", "-1", "1,,2", "[[1]]"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		got, set, err := Parse(s)
		if err != nil {
			if set || got != nil {
				t.Fatalf("error with a result: %v %v", got, set)
			}
			return
		}
		if !set {
			if got != nil {
				t.Fatalf("inherit with levels %v", got)
			}
			return
		}
		if got == nil || Validate(got) != nil {
			t.Fatalf("Parse(%q) = %v, invalid", s, got)
		}
		back, set2, err := Parse(Format(got))
		if err != nil || !set2 || !reflect.DeepEqual(back, got) {
			t.Fatalf("round trip %v -> %q -> %v %v", got, Format(got), back, err)
		}
	})
}

// FuzzValidate: no list panics, and a valid one is strictly increasing in range.
func FuzzValidate(f *testing.F) {
	f.Add(75, 95, 0, 0, 0, 0)
	f.Fuzz(func(t *testing.T, a, b, c, d, e, g int) {
		l := []int{a, b, c, d, e, g}
		if Validate(l) == nil {
			t.Fatalf("six thresholds accepted: %v", l)
		}
		l = l[:5]
		if Validate(l) == nil {
			prev := 0
			for _, v := range l {
				if v < Min || v > Max || v <= prev {
					t.Fatalf("accepted %v", l)
				}
				prev = v
			}
		}
	})
}
