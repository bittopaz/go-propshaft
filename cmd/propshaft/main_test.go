package main

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bittopaz/go-propshaft"
)

func fixture(t *testing.T, parent, name string, files map[string]string) string {
	t.Helper()
	root := filepath.Join(parent, name)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	for file, data := range files {
		destination := filepath.Join(root, file)
		if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(destination, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestPrecompile(t *testing.T) {
	parent := t.TempDir()
	first := fixture(t, parent, "first", map[string]string{"app.css": `a{background:url(icons/mark.svg)}`, "icons/mark.svg": "first"})
	second := fixture(t, parent, "second", map[string]string{"icons/mark.svg": "second", "extra.txt": "extra"})
	output := filepath.Join(parent, "build", "assets")
	var stdout, stderr bytes.Buffer
	args := []string{"precompile", "-root", first, "-root", second, "-out", output}
	if err := run(args, &stdout, &stderr); err != nil {
		t.Fatalf("%v: %s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Manifest:") {
		t.Fatal(stdout.String())
	}
	data, err := os.ReadFile(filepath.Join(output, propshaft.ManifestName))
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := propshaft.LoadManifest(bytes.NewReader(data), "/assets/")
	if err != nil {
		t.Fatal(err)
	}
	image, err := manifest.URL("icons/mark.svg")
	if err != nil {
		t.Fatal(err)
	}
	imageData, err := os.ReadFile(filepath.Join(output, strings.TrimPrefix(image, "/assets/")))
	if err != nil || string(imageData) != "first" {
		t.Fatalf("root precedence: %s %v", imageData, err)
	}
	if err := run(args, &stdout, &stderr); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("existing output: %v", err)
	}
}

func TestPrecompileSafety(t *testing.T) {
	parent := t.TempDir()
	root := fixture(t, parent, "input", map[string]string{"app.txt": "safe"})
	alias := filepath.Join(parent, "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	for _, output := range []string{root, filepath.Join(root, "generated"), parent, filepath.Join(alias, "nested", "assets")} {
		var stdout, stderr bytes.Buffer
		err := run([]string{"precompile", "-root", root, "-out", output}, &stdout, &stderr)
		if err == nil || !strings.Contains(err.Error(), "overlaps") {
			t.Errorf("output %s: %v", output, err)
		}
	}
	outside := fixture(t, parent, "outside", map[string]string{"secret": "do not export"})
	if err := os.Symlink(filepath.Join(outside, "secret"), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	output := filepath.Join(parent, "output")
	if err := run([]string{"precompile", "-root", root, "-out", output}, &stdout, &stderr); err == nil {
		t.Fatal("symlink accepted")
	}
	if _, err := os.Stat(output); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("failed build published output")
	}
	if got, err := os.ReadFile(filepath.Join(root, "app.txt")); err != nil || string(got) != "safe" {
		t.Fatal("source damaged")
	}
}

func TestCommandsAndErrors(t *testing.T) {
	for _, args := range [][]string{nil, {"help"}, {"--help"}, {"precompile", "-h"}} {
		var stdout, stderr bytes.Buffer
		if err := run(args, &stdout, &stderr); err != nil {
			t.Fatal(err)
		}
		if stdout.Len()+stderr.Len() == 0 {
			t.Fatal("missing help")
		}
	}
	for _, args := range [][]string{{"unknown"}, {"precompile"}, {"precompile", "-root", ""}, {"precompile", "-unknown"}, {"precompile", "-root", "absent", "-out", "unused", "extra"}} {
		var stdout, stderr bytes.Buffer
		if err := run(args, &stdout, &stderr); err == nil {
			t.Errorf("accepted %v", args)
		}
	}
	parent := t.TempDir()
	root := fixture(t, parent, "input", map[string]string{"app.css": `a{background:url(missing)}`})
	var stdout, stderr bytes.Buffer
	output := filepath.Join(parent, "output")
	if err := run([]string{"precompile", "-root", root, "-out", output}, &stdout, &stderr); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	if _, err := os.Stat(output); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("failed compilation published output")
	}
}
