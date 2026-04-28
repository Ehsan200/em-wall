package rules

import "testing"

func TestMatch(t *testing.T) {
	cases := []struct {
		pattern string
		name    string
		want    bool
	}{
		{"y.com", "y.com", true},
		{"y.com", "Y.COM", true},
		{"y.com", "y.com.", true},
		{"y.com", "a.y.com", false},
		{"*.y.com", "y.com", true},
		{"*.y.com", "a.y.com", true},
		{"*.y.com", "a.b.y.com", true},
		{"*.y.com", "x.com", false},
		{"*.y.com", "yy.com", false},
		{"*.y.com", "ay.com", false},
		{"*.y.com", "", false},
		{"", "y.com", false},
		{"*.foo.bar.com", "x.foo.bar.com", true},
		{"*.foo.bar.com", "foo.bar.com", true},
		{"*.foo.bar.com", "bar.com", false},
	}
	for _, tc := range cases {
		got := Match(tc.pattern, tc.name)
		if got != tc.want {
			t.Errorf("Match(%q, %q) = %v, want %v", tc.pattern, tc.name, got, tc.want)
		}
	}
}

func TestSpecificity(t *testing.T) {
	if Specificity("y.com") <= Specificity("*.y.com") {
		t.Errorf("exact y.com should outrank *.y.com")
	}
	if Specificity("*.foo.y.com") <= Specificity("*.y.com") {
		t.Errorf("longer wildcard should outrank shorter")
	}
	if Specificity("a.b.c.com") <= Specificity("*.b.c.com") {
		t.Errorf("exact deeper should outrank wildcard same depth")
	}
}

func TestValidate(t *testing.T) {
	good := []string{"y.com", "*.y.com", "a-b.example.co", "*.a-b.example.co"}
	for _, p := range good {
		if err := Validate(p); err != nil {
			t.Errorf("Validate(%q) unexpected err: %v", p, err)
		}
	}
	bad := []string{"", "*", "*.", "x.*.com", "foo bar.com", "a..b.com", "*.*.com"}
	for _, p := range bad {
		if err := Validate(p); err == nil {
			t.Errorf("Validate(%q) expected error, got nil", p)
		}
	}
}

func TestMostSpecific(t *testing.T) {
	rules := []Rule{
		{ID: 1, Pattern: "*.y.com", Action: ActionBlock, Enabled: true},
		{ID: 2, Pattern: "*.public.y.com", Action: ActionAllow, Interface: "utun3", Enabled: true},
		{ID: 3, Pattern: "z.com", Action: ActionBlock, Enabled: true},
		{ID: 4, Pattern: "*.disabled.com", Action: ActionBlock, Enabled: false},
	}
	cases := []struct {
		name   string
		wantID int64
	}{
		{"foo.public.y.com", 2},
		{"a.y.com", 1},
		{"y.com", 1},
		{"z.com", 3},
		{"x.disabled.com", 0},
		{"unrelated.com", 0},
	}
	for _, tc := range cases {
		got := MostSpecific(rules, tc.name)
		var gotID int64
		if got != nil {
			gotID = got.ID
		}
		if gotID != tc.wantID {
			t.Errorf("MostSpecific(%q) id=%d, want %d", tc.name, gotID, tc.wantID)
		}
	}
}
