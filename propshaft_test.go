package propshaft_test

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/bittopaz/go-propshaft"
)

func newPipeline(t *testing.T, files fstest.MapFS, dev bool) *propshaft.Pipeline {
	t.Helper()
	p, err := propshaft.New(propshaft.Config{Roots: []fs.FS{files}, Development: dev})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func assetURL(t *testing.T, p interface{ URL(string) (string, error) }, name string) string {
	t.Helper()
	u, err := p.URL(name)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func request(handler http.Handler, method, target string, headers map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, nil)
	for name, value := range headers {
		r.Header.Set(name, value)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

func served(t *testing.T, p *propshaft.Pipeline, name string) []byte {
	t.Helper()
	w := request(p.Handler(), "GET", assetURL(t, p, name), nil)
	if w.Code != 200 {
		t.Fatalf("serve %s: %d %s", name, w.Code, w.Body.String())
	}
	return w.Body.Bytes()
}

func TestTransitiveFingerprints(t *testing.T) {
	files := fstest.MapFS{
		"images/mark.svg": {Data: []byte("<svg>first</svg>")},
		"css/base.css":    {Data: []byte(`.mark { background: url(../images/mark.svg?v=2#icon) }`)},
		"css/app.css":     {Data: []byte(`@import "base.css" layer(base) screen;`)},
		"app.js":          {Data: []byte(`console.log("unchanged")`)},
	}
	first := newPipeline(t, files, false)
	before := make(map[string]string)
	for name := range files {
		before[name] = assetURL(t, first, name)
		data := served(t, first, name)
		digest := fmt.Sprintf("%x", sha256.Sum256(data))
		if !strings.Contains(before[name], "-"+digest+path.Ext(name)) {
			t.Fatalf("%s is not fingerprinted from served bytes", name)
		}
	}
	css := string(served(t, first, "css/base.css"))
	if !strings.Contains(css, "../images/"+path.Base(before["images/mark.svg"])+"?v=2#icon") {
		t.Fatalf("dependency not rewritten: %s", css)
	}
	app := string(served(t, first, "css/app.css"))
	if !strings.Contains(app, "./"+path.Base(before["css/base.css"])) || !strings.Contains(app, "layer(base) screen") {
		t.Fatalf("import not rewritten or modifiers lost: %s", app)
	}
	files["images/mark.svg"].Data = []byte("<svg>second</svg>")
	second := newPipeline(t, files, false)
	for _, name := range []string{"images/mark.svg", "css/base.css", "css/app.css"} {
		if before[name] == assetURL(t, second, name) {
			t.Fatalf("%s did not invalidate", name)
		}
	}
	if before["app.js"] != assetURL(t, second, "app.js") {
		t.Fatal("unrelated asset invalidated")
	}
	if got := string(served(t, first, "images/mark.svg")); got != "<svg>first</svg>" {
		t.Fatalf("production snapshot changed: %s", got)
	}
}

func TestRootsAndPaths(t *testing.T) {
	p, err := propshaft.New(propshaft.Config{Roots: []fs.FS{
		fstest.MapFS{"a.txt": {Data: []byte("first")}, ".secret": {}, ".git/config": {}},
		fstest.MapFS{"a.txt": {Data: []byte("second")}, "sub/space #é.txt": {Data: []byte("extra")}, "README": {}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(served(t, p, "a.txt")); got != "first" {
		t.Fatal(got)
	}
	if got := string(served(t, p, "sub/space #é.txt")); got != "extra" {
		t.Fatal(got)
	}
	for _, name := range []string{".secret", ".git/config", "missing"} {
		if _, err := p.URL(name); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("%q: %v", name, err)
		}
	}
	for _, name := range []string{"", ".", "..", "../a.txt", "/a.txt", "a//b", "a/./b", "a\\b", "a\x00b"} {
		if _, err := p.URL(name); !errors.Is(err, fs.ErrInvalid) {
			t.Fatalf("unsafe path %q: %v", name, err)
		}
	}
	if !strings.Contains(assetURL(t, p, "README"), "README-") {
		t.Fatal("extensionless file not fingerprinted")
	}
}

func TestCompileErrors(t *testing.T) {
	for _, tc := range []struct {
		name    string
		files   fstest.MapFS
		message string
	}{
		{"missing", fstest.MapFS{"a.css": {Data: []byte(`a { background: url(missing.png) }`)}}, "missing.png"},
		{"cycle", fstest.MapFS{"a.css": {Data: []byte(`@import "b.css";`)}, "b.css": {Data: []byte(`@import "a.css";`)}}, "a.css -> b.css -> a.css"},
		{"self", fstest.MapFS{"a.css": {Data: []byte(`@import url(a.css);`)}}, "a.css -> a.css"},
		{"escape", fstest.MapFS{"a.css": {Data: []byte(`a { background: url(../secret) }`)}}, "escapes the asset root"},
		{"absolute escape", fstest.MapFS{"a.css": {Data: []byte(`a { background: url(/assets/../secret) }`)}}, "escapes the asset root"},
		{"symlink", fstest.MapFS{"a": {Mode: fs.ModeSymlink, Data: []byte("outside")}}, "symlinks"},
		{"special", fstest.MapFS{"a": {Mode: fs.ModeNamedPipe}}, "regular file"},
		{"invalid CSS", fstest.MapFS{"a.css": {Data: []byte("/* never closed")}}, "unterminated"},
		{"encoding", fstest.MapFS{"a.css": {Data: []byte{0xff}}}, "UTF-8"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := propshaft.New(propshaft.Config{Roots: []fs.FS{tc.files}})
			if err == nil || !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("want %q, got %v", tc.message, err)
			}
		})
	}
	if _, err := propshaft.New(propshaft.Config{}); err == nil {
		t.Fatal("missing roots accepted")
	}
	if _, err := propshaft.New(propshaft.Config{Roots: []fs.FS{nil}}); err == nil {
		t.Fatal("nil root accepted")
	}
}

func TestPrefixes(t *testing.T) {
	for _, prefix := range []string{"/assets", "/", "/nested/static/", "https://cdn.example.com/assets/", "/space here/"} {
		t.Run(prefix, func(t *testing.T) {
			p, err := propshaft.New(propshaft.Config{Roots: []fs.FS{fstest.MapFS{"x.txt": {Data: []byte("x")}}}, Prefix: prefix})
			if err != nil {
				t.Fatal(err)
			}
			if string(served(t, p, "x.txt")) != "x" {
				t.Fatal("bad response")
			}
		})
	}
	for _, prefix := range []string{"assets", "//evil.example/assets", "ftp://host/assets", "https:/assets", "https://user:pass@host/assets", "/a/../assets", "/a//b", "/assets?q=1", "/assets#", "/assets?", "/a\\b"} {
		_, err := propshaft.New(propshaft.Config{Roots: []fs.FS{fstest.MapFS{}}, Prefix: prefix})
		if err == nil {
			t.Errorf("accepted prefix %q", prefix)
		}
	}
}

func TestHTTP(t *testing.T) {
	p := newPipeline(t, fstest.MapFS{"x.css": {Data: []byte("body { color: red }")}}, false)
	u := assetURL(t, p, "x.css")
	response := request(p.Handler(), "GET", u, nil)
	if response.Code != 200 || !strings.HasPrefix(response.Header().Get("Content-Type"), "text/css") || !strings.Contains(response.Header().Get("Cache-Control"), "immutable") {
		t.Fatalf("unexpected response: %+v", response.Result())
	}
	etag := response.Header().Get("ETag")
	if etag == "" || response.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("missing safety/cache headers")
	}
	for _, tc := range []struct {
		method  string
		target  string
		headers map[string]string
		status  int
	}{
		{"HEAD", u, nil, 200},
		{"GET", u, map[string]string{"If-None-Match": etag}, 304},
		{"GET", u, map[string]string{"If-None-Match": "W/" + etag}, 304},
		{"GET", u, map[string]string{"Range": "bytes=0-3"}, 206},
		{"GET", u, map[string]string{"Range": "bytes=99999-"}, 416},
		{"GET", u, map[string]string{"If-Match": `"not-current"`}, 412},
		{"POST", u, nil, 405},
		{"GET", "/assets/x.css", nil, 404},
		{"GET", "/assets/", nil, 404},
		{"GET", "/wrong/" + path.Base(u), nil, 404},
		{"GET", strings.Replace(u, "x-", "x-0", 1), nil, 404},
		{"GET", "/assets/../x.css", nil, 404},
		{"GET", "/assets/%2e%2e/x.css", nil, 404},
		{"GET", "/assets/a%5cb", nil, 404},
	} {
		w := request(p.Handler(), tc.method, tc.target, tc.headers)
		if w.Code != tc.status {
			t.Errorf("%s %s: got %d, want %d", tc.method, tc.target, w.Code, tc.status)
		}
		if (tc.method == "HEAD" || tc.status == 304) && w.Body.Len() != 0 {
			t.Errorf("unexpected body for %s/%d", tc.method, tc.status)
		}
		if tc.status >= 400 && w.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("cacheable error: %v", w.Header())
		}
		if tc.status == 206 && w.Body.String() != "body" {
			t.Fatalf("range: %s", w.Body.String())
		}
	}
}

func TestDevelopmentRefreshAndRecovery(t *testing.T) {
	files := fstest.MapFS{"image.txt": {Data: []byte("one")}, "app.css": {Data: []byte(`a { background: url(image.txt) }`)}}
	p := newPipeline(t, files, true)
	old := assetURL(t, p, "app.css")
	files["image.txt"].Data = []byte("two") // Same length and zero modtime.
	updated := assetURL(t, p, "app.css")
	if updated == old {
		t.Fatal("content change missed")
	}
	if w := request(p.Handler(), "GET", old, nil); w.Code != 404 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("obsolete fingerprint served: %d", w.Code)
	}
	delete(files, "image.txt")
	if _, err := p.URL("app.css"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing dependency was hidden: %v", err)
	}
	if w := request(p.Handler(), "GET", updated, nil); w.Code != 500 {
		t.Fatalf("failed rebuild: %d", w.Code)
	}
	files["image.txt"] = &fstest.MapFile{Data: []byte("two")}
	if got := assetURL(t, p, "app.css"); got != updated {
		t.Fatal("recovery was not deterministic")
	}
	files["new.txt"] = &fstest.MapFile{Data: []byte("new")}
	if got := string(served(t, p, "new.txt")); got != "new" {
		t.Fatal(got)
	}
	delete(files, "new.txt")
	if _, err := p.URL("new.txt"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
}

func TestDiskRefreshSameMetadata(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "x.txt")
	stamp := time.Unix(100, 0)
	write := func(value string) {
		t.Helper()
		if err := os.WriteFile(file, []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(file, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	write("one")
	p, err := propshaft.New(propshaft.Config{Roots: []fs.FS{os.DirFS(dir)}, Development: true})
	if err != nil {
		t.Fatal(err)
	}
	before := assetURL(t, p, "x.txt")
	write("two")
	if assetURL(t, p, "x.txt") == before {
		t.Fatal("disk content change missed")
	}
}

func TestConcurrentAccess(t *testing.T) {
	for _, dev := range []bool{false, true} {
		p := newPipeline(t, fstest.MapFS{"x.txt": {Data: []byte("ok")}}, dev)
		var wg sync.WaitGroup
		for range 16 {
			wg.Go(func() {
				for range 20 {
					u, err := p.URL("x.txt")
					if err != nil {
						t.Error(err)
						return
					}
					w := request(p.Handler(), "GET", u, nil)
					if w.Code != 200 || w.Body.String() != "ok" {
						t.Errorf("bad response: %d", w.Code)
					}
				}
			})
		}
		wg.Wait()
	}
}

func TestConcurrentDiskChanges(t *testing.T) {
	directory := t.TempDir()
	file := filepath.Join(directory, "x.txt")
	if err := os.WriteFile(file, []byte("initial"), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := propshaft.New(propshaft.Config{Roots: []fs.FS{os.DirFS(directory)}, Development: true})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Go(func() {
		for i := range 40 {
			// Stage outside the asset tree, then atomically replace the source.
			staged, err := os.CreateTemp(filepath.Dir(directory), "propshaft-update-*")
			if err != nil {
				t.Error(err)
				return
			}
			_, writeErr := fmt.Fprintf(staged, "generation %d", i)
			closeErr := staged.Close()
			if writeErr != nil || closeErr != nil {
				os.Remove(staged.Name())
				t.Errorf("stage: %v %v", writeErr, closeErr)
				return
			}
			if err := os.Rename(staged.Name(), file); err != nil {
				os.Remove(staged.Name())
				t.Error(err)
				return
			}
		}
	})
	for range 8 {
		wg.Go(func() {
			for range 40 {
				u, err := p.URL("x.txt")
				if err != nil {
					t.Error(err)
					return
				}
				response := request(p.Handler(), "GET", u, nil)
				switch response.Code {
				case 200:
					digest := fmt.Sprintf("%x", sha256.Sum256(response.Body.Bytes()))
					if !strings.Contains(u, "-"+digest+".txt") {
						t.Error("served new bytes under an old fingerprint")
					}
				case 404:
					// A newer generation was published between URL and GET.
				default:
					t.Errorf("unexpected response: %d %s", response.Code, response.Body.String())
				}
			}
		})
	}
	wg.Wait()
}

func TestTemplateHelper(t *testing.T) {
	p := newPipeline(t, fstest.MapFS{"app.css": {}}, false)
	tmpl := template.Must(template.New("page").Funcs(template.FuncMap{"asset": p.URL}).Parse(`<link href="{{asset .}}">`))
	var output bytes.Buffer
	if err := tmpl.Execute(&output, "app.css"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), assetURL(t, p, "app.css")) {
		t.Fatal(output.String())
	}
	if err := tmpl.Execute(&output, "missing.css"); err == nil {
		t.Fatal("template swallowed missing asset")
	}
}

func TestCSSURLSpaceEncoding(t *testing.T) {
	p := newPipeline(t, fstest.MapFS{"css/a.css": {Data: []byte(`a { background: url("../images/a%20b.svg#icon") }`)}, "images/a b.svg": {Data: []byte("svg")}}, false)
	css := string(served(t, p, "css/a.css"))
	image, err := url.Parse(assetURL(t, p, "images/a b.svg"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(css, "../images/"+path.Base(image.EscapedPath())+"#icon") {
		t.Fatal(css)
	}
}
