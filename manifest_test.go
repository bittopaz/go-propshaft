package propshaft_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/bittopaz/go-propshaft"
)

func exportedFiles(t *testing.T, directory string) map[string]string {
	t.Helper()
	files := make(map[string]string)
	root := os.DirFS(directory)
	err := fs.WalkDir(root, ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		data, err := fs.ReadFile(root, name)
		files[name] = string(data)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestExportManifestAndRelocation(t *testing.T) {
	files := fstest.MapFS{
		"css/app.css":     {Data: []byte(`@import "base.css"; a { background: url(/assets/images/logo.svg#logo) }`)},
		"css/base.css":    {Data: []byte(`body { color: navy }`)},
		"images/logo.svg": {Data: []byte(`<svg id="logo"/>`)},
		"app.js":          {Data: []byte(`console.log("hello")`)},
	}
	p := newPipeline(t, files, false)
	parent := t.TempDir()
	first, second := filepath.Join(parent, "first"), filepath.Join(parent, "second")
	if err := p.Export(first); err != nil {
		t.Fatal(err)
	}
	if err := newPipeline(t, files, false).Export(second); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(exportedFiles(t, first), exportedFiles(t, second)) {
		t.Fatal("exports are not deterministic")
	}
	manifestBytes, err := os.ReadFile(filepath.Join(first, propshaft.ManifestName))
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := propshaft.LoadManifest(bytes.NewReader(manifestBytes), "/relocated/static/")
	if err != nil {
		t.Fatal(err)
	}
	clear(files) // Nothing below relies on sources or the original pipeline.
	mux := http.NewServeMux()
	mux.Handle("/relocated/static/", http.StripPrefix("/relocated/static/", http.FileServerFS(os.DirFS(first))))
	server := httptest.NewServer(mux)
	defer server.Close()
	fetch := func(target string) []byte {
		t.Helper()
		response, err := server.Client().Get(server.URL + target)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != 200 {
			t.Fatalf("%s: %d", target, response.StatusCode)
		}
		return body
	}
	appURL := assetURL(t, manifest, "css/app.css")
	css := string(fetch(appURL))
	base, err := url.Parse(appURL)
	if err != nil {
		t.Fatal(err)
	}
	// The scanner canonicalizes rewritten references to double-quoted URLs.
	// Follow every quoted reference in this fixture as a browser would.
	for i, field := range strings.Split(css, `"`) {
		if i%2 == 0 {
			continue
		}
		reference, err := url.Parse(field)
		if err != nil {
			t.Fatal(err)
		}
		target := base.ResolveReference(reference)
		body := fetch(target.RequestURI())
		digest := fmt.Sprintf("%x", sha256.Sum256(body))
		if !strings.Contains(target.Path, "-"+digest+path.Ext(target.Path)) {
			t.Fatalf("exported hash mismatch: %s", target)
		}
	}
	for _, name := range []string{"images/logo.svg", "css/base.css", "app.js"} {
		fetch(assetURL(t, manifest, name))
	}
	cdn, err := propshaft.LoadManifest(bytes.NewReader(manifestBytes), "https://cdn.example.com/version/assets/")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(assetURL(t, cdn, "app.js"), "https://cdn.example.com/version/assets/") {
		t.Fatal("CDN prefix lost")
	}
	if _, err := manifest.URL("missing"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	if _, err := manifest.URL("../secret"); !errors.Is(err, fs.ErrInvalid) {
		t.Fatal(err)
	}
}

func TestManifestValidation(t *testing.T) {
	digest := strings.Repeat("0", 64)
	for _, document := range []string{
		`{}`, `null`, `{"version":2,"assets":{}}`, `{"version":1,"assets":null}`,
		`{"version":1,"assets":{},"surprise":true}`, `{"version":1,"assets":{}} {}`,
		`{"version":1,"assets":{}} trailing`,
		`{"version":1,"assets":{"../secret":"secret-` + digest + `"}}`,
		`{"version":1,"assets":{"a.txt":"../a-` + digest + `.txt"}}`,
		`{"version":1,"assets":{"a.txt":"a-short.txt"}}`,
		`{"version":1,"assets":{"a.txt":"b-` + digest + `.txt"}}`,
	} {
		if _, err := propshaft.LoadManifest(strings.NewReader(document), ""); err == nil {
			t.Errorf("accepted %s", document)
		}
	}
	if _, err := propshaft.LoadManifest(strings.NewReader(`{"version":1,"assets":{}}`), ""); err != nil {
		t.Fatal(err)
	}
}

func TestExportRefusesExistingAndCleansFailure(t *testing.T) {
	p := newPipeline(t, fstest.MapFS{"x.txt": {Data: []byte("x")}}, false)
	parent := t.TempDir()
	existing := filepath.Join(parent, "existing")
	if err := os.Mkdir(existing, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(existing, "keep"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := p.Export(existing); !errors.Is(err, fs.ErrExist) {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(existing, "keep")); err != nil || string(data) != "keep" {
		t.Fatal("existing output damaged")
	}
	if err := p.Export(""); err == nil {
		t.Fatal("empty output accepted")
	}
	empty := filepath.Join(parent, "empty")
	if err := os.Mkdir(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := p.Export(empty); !errors.Is(err, fs.ErrExist) {
		t.Fatal("empty directory overwritten")
	}
	alias := filepath.Join(parent, "alias")
	if err := os.Symlink(existing, alias); err != nil {
		t.Fatal(err)
	}
	if err := p.Export(alias); !errors.Is(err, fs.ErrExist) {
		t.Fatal("symlink overwritten")
	}
	// A generated filename can conflict with a source directory name. The
	// export must fail without publishing or leaving its staging directory.
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte("x")))
	conflict := newPipeline(t, fstest.MapFS{"x": {Data: []byte("x")}, "x-" + digest + "/child": {Data: []byte("y")}}, false)
	failed := filepath.Join(parent, "failed")
	if err := conflict.Export(failed); err == nil {
		t.Fatal("conflicting output unexpectedly succeeded")
	}
	if _, err := os.Stat(failed); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("partial output published")
	}
	stages, err := filepath.Glob(filepath.Join(parent, ".propshaft-*"))
	if err != nil || len(stages) != 0 {
		t.Fatalf("staging directories leaked: %v %v", stages, err)
	}
}

func TestEmptyExport(t *testing.T) {
	p := newPipeline(t, fstest.MapFS{}, false)
	output := filepath.Join(t.TempDir(), "assets")
	if err := p.Export(output); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(output, propshaft.ManifestName))
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Version int
		Assets  map[string]string
	}
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	if document.Version != 1 || document.Assets == nil || len(document.Assets) != 0 {
		t.Fatalf("bad empty manifest: %s", data)
	}
}
