package main

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"testing/fstest"

	"github.com/bittopaz/go-propshaft"
)

func runCLI(t *testing.T, args ...string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if err := run(args, &stdout, &stderr); err != nil {
		t.Fatalf("%v: %v\n%s", args, err, stderr.String())
	}
}

func readFile(t *testing.T, root, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func nativeEntries(t *testing.T, root string) map[string]string {
	t.Helper()
	manifest, err := propshaft.LoadManifest(bytes.NewReader(readFile(t, root, propshaft.ManifestName)), "")
	if err != nil {
		t.Fatal(err)
	}
	return manifest.Entries()
}

func TestMergeRetentionAndRollback(t *testing.T) {
	parent := t.TempDir()
	source := fixture(t, parent, "source", map[string]string{
		"app.js": `import "controllers/example";`, "controllers/example.js": "export const value = 1;",
		"app.css": `@import "base.css";`, "base.css": `body{background:url(logo.svg)}`, "logo.svg": "<svg/>",
	})
	first, candidate := filepath.Join(parent, "first"), filepath.Join(parent, "candidate")
	runCLI(t, "precompile", "-root", source, "-out", first)
	old := nativeEntries(t, first)
	if err := os.WriteFile(filepath.Join(source, "logo.svg"), []byte("<svg>changed</svg>"), 0o600); err != nil {
		t.Fatal(err)
	}
	runCLI(t, "precompile", "-root", source, "-out", candidate)
	current := nativeEntries(t, candidate)
	for _, name := range []string{"app.css", "base.css", "logo.svg"} {
		if old[name] == current[name] {
			t.Fatal("dependency not invalidated", name)
		}
	}
	if old["app.js"] != current["app.js"] {
		t.Fatal("native JavaScript changed")
	}
	for _, selected := range []string{first, candidate} {
		out := filepath.Join(parent, "merged_"+filepath.Base(selected))
		runCLI(t, "merge", "-manifest-from", selected, "-root", first, "-root", candidate, "-out", out)
		if !bytes.Equal(readFile(t, selected, propshaft.ManifestName), readFile(t, out, propshaft.ManifestName)) {
			t.Fatal("selected manifest changed")
		}
		for root, entries := range map[string]map[string]string{first: old, candidate: current} {
			for _, name := range entries {
				if !bytes.Equal(readFile(t, root, name), readFile(t, out, name)) {
					t.Fatalf("historical bytes changed: %s", name)
				}
			}
		}
	}
	// A subsequent release keeps the cumulative archive, not just its current manifest.
	next := filepath.Join(parent, "next")
	runCLI(t, "merge", "-manifest-from", candidate, "-root", filepath.Join(parent, "merged_candidate"), "-out", next)
	for _, name := range old {
		readFile(t, next, name)
	}
}

func TestMergeHistoricalFormats(t *testing.T) {
	for _, vite := range []bool{false, true} {
		t.Run(map[bool]string{false: "ruby", true: "vite"}[vite], func(t *testing.T) {
			parent := t.TempDir()
			source := fixture(t, parent, "source", map[string]string{"app.js": "// new"})
			candidate := filepath.Join(parent, "candidate")
			runCLI(t, "precompile", "-root", source, "-out", candidate)
			files := map[string]string{"app-12345678.js": "// old", rubyManifestName: `{"app.js":{"digested_path":"app-12345678.js","integrity":null}}`}
			metadata := rubyManifestName
			if vite {
				files = map[string]string{"assets/app-12345678.js": "// old", "index.html": `<script src="/assets/app-12345678.js"></script>`}
				metadata = "index.html"
			}
			previous := fixture(t, parent, "previous", files)
			for _, selected := range []string{previous, candidate} {
				output := filepath.Join(parent, "merged_"+filepath.Base(selected))
				args := []string{"merge", "-manifest-from", selected, "-root", previous, "-root", candidate, "-out", output}
				if vite {
					args = append(args, "-legacy-vite")
				}
				runCLI(t, args...)
				readFile(t, output, "app-12345678.js")
				if vite {
					readFile(t, output, "assets/app-12345678.js")
				}
				for _, name := range nativeEntries(t, candidate) {
					readFile(t, output, name)
					if vite {
						readFile(t, output, "assets/"+name)
					}
				}
				if selected == previous {
					if !bytes.Equal(readFile(t, previous, metadata), readFile(t, output, metadata)) {
						t.Fatal("rollback metadata changed")
					}
					if _, err := os.Stat(filepath.Join(output, propshaft.ManifestName)); !errors.Is(err, fs.ErrNotExist) {
						t.Fatal("candidate manifest leaked into bridge")
					}
				} else if _, err := os.Stat(filepath.Join(output, metadata)); !errors.Is(err, fs.ErrNotExist) {
					t.Fatal("historical metadata leaked into candidate")
				}
			}
		})
	}
}

func TestMergeRefusesUnsafeInputs(t *testing.T) {
	for _, kind := range []string{"corrupt", "missing", "symlink", "traversal", "existing", "overlap", "alias_overlap", "vite_without_flag", "collision", "directory_collision"} {
		t.Run(kind, func(t *testing.T) {
			parent := t.TempDir()
			source := fixture(t, parent, "source", map[string]string{"app.js": "// app"})
			input := filepath.Join(parent, "input")
			output := filepath.Join(parent, "output")
			runCLI(t, "precompile", "-root", source, "-out", input)
			entries := nativeEntries(t, input)
			args := []string{"merge", "-manifest-from", input}
			switch kind {
			case "corrupt":
				if err := os.WriteFile(filepath.Join(input, entries["app.js"]), []byte("corrupt"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "missing":
				if err := os.Remove(filepath.Join(input, entries["app.js"])); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Symlink(entries["app.js"], filepath.Join(input, "link.js")); err != nil {
					t.Fatal(err)
				}
			case "traversal":
				if err := os.Remove(filepath.Join(input, propshaft.ManifestName)); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(input, rubyManifestName), []byte(`{"app.js":{"digested_path":"../secret.js"}}`), 0o600); err != nil {
					t.Fatal(err)
				}
			case "existing":
				fixture(t, parent, "output", map[string]string{"keep": "untouched"})
			case "overlap":
				output = filepath.Join(input, "generated")
			case "alias_overlap":
				alias := filepath.Join(parent, "alias")
				if err := os.Symlink(input, alias); err != nil {
					t.Fatal(err)
				}
				output = filepath.Join(alias, "generated")
			case "vite_without_flag":
				vite := fixture(t, parent, "vite", map[string]string{"index.html": "<html/>", "assets/a.js": "//old"})
				args = append(args, "-root", vite)
			case "collision", "directory_collision":
				otherFiles := map[string]string{rubyManifestName: `{"old.js":{"digested_path":"old-123.js"}}`, "old-123.js": "//old"}
				if kind == "collision" {
					otherFiles[entries["app.js"]] = "different"
				} else {
					otherFiles[entries["app.js"]+"/file.js"] = "directory conflicts with asset"
				}
				other := fixture(t, parent, "other", otherFiles)
				args = append(args, "-root", other)
			}
			var stdout, stderr bytes.Buffer
			if err := run(append(args, "-out", output), &stdout, &stderr); err == nil {
				t.Fatal("unsafe merge succeeded")
			}
			if kind == "existing" {
				if string(readFile(t, output, "keep")) != "untouched" {
					t.Fatal("output damaged")
				}
			} else if _, err := os.Stat(output); !errors.Is(err, fs.ErrNotExist) {
				t.Fatal("partial output published")
			}
			stages, err := filepath.Glob(filepath.Join(parent, ".propshaft-merge-*"))
			if err != nil || len(stages) != 0 {
				t.Fatalf("staging leaked: %v %v", stages, err)
			}
		})
	}
}

func TestMergeEmptyHiddenAndArguments(t *testing.T) {
	parent := t.TempDir()
	input := fixture(t, parent, "input", map[string]string{propshaft.ManifestName: `{"version":1,"assets":{}}`, ".env": "secret", ".git/config": "private"})
	output := filepath.Join(parent, "output")
	runCLI(t, "merge", "-manifest-from", input, "-out", output)
	if !reflect.DeepEqual(nativeEntries(t, input), nativeEntries(t, output)) {
		t.Fatal("empty manifest changed")
	}
	entries, err := os.ReadDir(output)
	if err != nil || len(entries) != 1 {
		t.Fatal("hidden files leaked", entries, err)
	}
	for _, args := range [][]string{{"merge"}, {"merge", "-manifest-from", input}, {"merge", "-bad"}, {"merge", "-manifest-from", input, "-out", output, "extra"}} {
		var stdout, stderr bytes.Buffer
		if err := run(args, &stdout, &stderr); err == nil {
			t.Fatal("invalid arguments accepted", args)
		}
	}
	runCLI(t, "merge", "-h")
}

func TestNativeManifestEnumerationIsACopy(t *testing.T) {
	parent := t.TempDir()
	source := fixture(t, parent, "source", map[string]string{"app.js": "// app"})
	output := filepath.Join(parent, "output")
	runCLI(t, "precompile", "-root", source, "-out", output)
	manifest, err := propshaft.LoadManifest(bytes.NewReader(readFile(t, output, propshaft.ManifestName)), "")
	if err != nil {
		t.Fatal(err)
	}
	entries := manifest.Entries()
	delete(entries, "app.js")
	if _, err := manifest.URL("app.js"); err != nil {
		t.Fatal("mutable entries changed manifest")
	}
}

func FuzzRubyArchiveManifest(f *testing.F) {
	for _, seed := range []string{`{}`, `null`, `{"app.js":{"digested_path":"app-123.js"}}`, `{"app.js":{"digested_path":"../app.js"}}`} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, document string) {
		root := fstest.MapFS{rubyManifestName: {Data: []byte(document)}, "app-123.js": {Data: []byte("//app")}}
		input, err := readArchive(root, false)
		if err == nil {
			if input.metadata != rubyManifestName || string(input.data) != document {
				t.Fatal("selected metadata changed")
			}
			for name := range input.files {
				if !safeArchivePath(name) {
					t.Fatalf("unsafe output path: %q", name)
				}
			}
		}
	})
}
