package propshaft

import (
	"fmt"
	"strings"
	"testing"
)

func TestCSSScanner(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  string
		count int
	}{
		{`a{background:url(icon.svg)}`, `a{background:url("built/icon.svg")}`, 1},
		{`a{background:URL( 'icon.svg' )}`, `a{background:url("built/icon.svg")}`, 1},
		{`a{background:u\72l(icon.svg)}`, `a{background:url("built/icon.svg")}`, 1},
		{`@import 'theme.css' layer(theme) screen;`, `@import "built/theme.css" layer(theme) screen;`, 1},
		{`@IMPORT /* comment */ "theme.css";`, `@IMPORT /* comment */ "built/theme.css";`, 1},
		{`@import url(theme.css) supports(display: grid);`, `@import url("built/theme.css") supports(display: grid);`, 1},
		{`a{src:url(a.woff2) format("woff2"), url(b.woff)}`, `a{src:url("built/a.woff2") format("woff2"), url("built/b.woff")}`, 2},
		{`/* url(missing) */ a{content:"url(missing)";x:myurl(no)}`, `/* url(missing) */ a{content:"url(missing)";x:myurl(no)}`, 0},
		{`a{background:url(icon\20 name.svg)}`, `a{background:url("built/icon name.svg")}`, 1},
		{`a{background:url(icon\)name.svg)}`, `a{background:url("built/icon)name.svg")}`, 1},
		{`a{background:url("icon\"name.svg")}`, `a{background:url("built/icon\"name.svg")}`, 1},
		{"@import \"the\\\nme.css\";", `@import "built/theme.css";`, 1},
		{`a{background:url("icon.svg" /* c */)}`, `a{background:url("built/icon.svg")}`, 1},
	} {
		t.Run(tc.input, func(t *testing.T) {
			count := 0
			got, err := rewriteCSS([]byte(tc.input), func(ref string) (string, error) {
				count++
				return "built/" + ref, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want || count != tc.count {
				t.Fatalf("got %q (%d references), want %q (%d)", got, count, tc.want, tc.count)
			}
		})
	}
}

func TestCSSPreservesUnmanagedReferences(t *testing.T) {
	input := `/* url(missing.png) */
@import "https://example.com/theme.css";
a { content: "url(missing.png)";
a: url(https://example.com/image.svg); b:url(//example.com/x);
c: url(data:image/svg+xml;base64,AAAA); d:url(#mask); e:url(%23mask);
f: url(/uploads/photo.jpg); g:url(blob:https://example.com/id);
h: url("data:image/svg+xml,%3Csvg%20mask='url(%23mask)'%3E");
i: url(?version=2); j:url(); }`
	got, err := rewriteCSS([]byte(input), func(ref string) (string, error) {
		_, _, managed, err := resolveReference("css/app.css", ref, "/assets/")
		if managed {
			t.Errorf("managed unexpectedly: %s", ref)
		}
		return ref, err
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != input {
		t.Fatal("unmanaged content changed")
	}
}

func TestCSSMalformed(t *testing.T) {
	for _, input := range []string{"/*", `a{content:"`, `url(x`, `url("x"`, `url(a b)`, `url(a(b))`, "url(a\\\n)", `url("x") /*`, "a{content:'a\nb'}", `url("x" other)`, "a\\"} {
		t.Run(input, func(t *testing.T) {
			_, err := rewriteCSS([]byte(input), func(ref string) (string, error) { return ref, nil })
			if err == nil {
				t.Fatal("accepted malformed CSS")
			}
		})
	}
}

func TestResolveReference(t *testing.T) {
	for _, tc := range []struct{ ref, logical, suffix string }{
		{"../img/icon.svg?q=1#part", "img/icon.svg", "?q=1#part"},
		{"./base.css", "css/base.css", ""},
		{"/assets/img/icon.svg", "img/icon.svg", ""},
		{"nested/../base.css?", "css/base.css", "?"},
		{"icon%20name.svg#", "css/icon name.svg", "#"},
		{"icon%23name.svg", "css/icon#name.svg", ""},
	} {
		logical, suffix, managed, err := resolveReference("css/app.css", tc.ref, "/assets/")
		if err != nil || !managed || logical != tc.logical || suffix != tc.suffix {
			t.Errorf("%q: %q %q %v %v", tc.ref, logical, suffix, managed, err)
		}
	}
	for _, ref := range []string{"../../secret", "../%2e%2e/secret", "/assets/../../secret", `dir\file`, "%zz", "%00"} {
		if _, _, _, err := resolveReference("css/app.css", ref, "/assets/"); err == nil {
			t.Errorf("accepted %q", ref)
		}
	}
}

func FuzzCSS(f *testing.F) {
	for _, seed := range []string{`a{background:url(icon.svg)}`, `@import "theme.css";`, `/* test */`, `url(data:abc)`, `u\72l(icon.svg)`, "url(\\)", "\xff", `url("\0")`} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input string) {
		data, err := rewriteCSS([]byte(input), func(ref string) (string, error) { return ref, nil })
		if err == nil && string(data) != input {
			t.Fatal("identity rewrite changed bytes")
		}
		_, _ = rewriteCSS([]byte(input), func(ref string) (string, error) {
			logical, suffix, managed, err := resolveReference("css/app.css", ref, "/assets/")
			if err != nil || !managed {
				return ref, err
			}
			return escapePath(relativePath("css", logical)) + suffix, nil
		})
	})
}

func FuzzLogicalPaths(f *testing.F) {
	for _, seed := range []string{"a.css", "../secret", "a b.svg", "a\\b", "\x00", "x:y", "日本語.txt"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, name string) {
		if !validName(name) {
			return
		}
		digest := fmt.Sprintf("%064x", 1)
		output := fingerprintedPath(name, digest)
		if !validName(output) || !validFingerprint(name, output) {
			t.Fatalf("invalid generated path %q", output)
		}
		if strings.HasPrefix(relativePath("css", output), "/") {
			t.Fatal("absolute relative path")
		}
	})
}
