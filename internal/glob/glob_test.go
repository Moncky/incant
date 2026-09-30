package glob

import "testing"

func TestCompile(t *testing.T) {
	cases := []struct {
		pattern string
		opt     Options
		name    string
		want    bool
	}{
		{"*.log", Options{}, "app.log", true},
		{"*.log", Options{}, "app.log.1", false},
		{"*.log", Options{}, "dir/app.log", false},
		{"*.log", Options{CrossSlash: true}, "dir/app.log", true},
		{"file?.txt", Options{}, "file1.txt", true},
		{"file?.txt", Options{}, "file10.txt", false},
		{"[abc]*", Options{}, "beta", true},
		{"[!abc]*", Options{}, "beta", false},
		{"[]x]", Options{}, "]", true},
		{"[[:digit:]]*", Options{}, "9lives", true},
		{`\*.txt`, Options{}, "*.txt", true},
		{`\*.txt`, Options{}, "a.txt", false},
		{"*.JPG", Options{Fold: true}, "photo.jpg", true},
		{"a[b", Options{}, "a[b", true},
		{"a.b", Options{}, "axb", false},
	}
	for _, c := range cases {
		re, err := Compile(c.pattern, c.opt)
		if err != nil {
			t.Fatalf("Compile(%q): %v", c.pattern, err)
		}
		if got := re.MatchString(c.name); got != c.want {
			t.Errorf("%q (%+v) vs %q = %v, want %v", c.pattern, c.opt, c.name, got, c.want)
		}
	}
}

func TestHasMetaAndUnescape(t *testing.T) {
	if !HasMeta("*.go") || HasMeta(`\*.go`) || HasMeta("plain") {
		t.Error("HasMeta misclassified a pattern")
	}
	if got := Unescape(`a\*b\ c`); got != "a*b c" {
		t.Errorf("Unescape = %q", got)
	}
}
