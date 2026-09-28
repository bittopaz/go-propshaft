//go:build integration

package propshaft_test

import (
	"bufio"
	"cmp"
	"crypto/sha256"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// TestBinaries builds and runs the actual CLI and example binaries. No Node,
// browser, shell, source-directory access in production, or external service is
// needed. Run explicitly with go test -tags=integration -run TestBinaries -v .
func TestBinaries(t *testing.T) {
	workspace := t.TempDir()
	cli := filepath.Join(workspace, "propshaft")
	example := filepath.Join(workspace, "example")
	for binary, target := range map[string]string{cli: "./cmd/propshaft", example: "./examples/basic"} {
		command := exec.CommandContext(t.Context(), "go", "build", "-o", binary, target)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("build: %v\n%s", err, output)
		}
	}
	sources := filepath.Join(workspace, "source-assets")
	err := fs.WalkDir(os.DirFS("examples/basic/assets"), ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		destination := filepath.Join(sources, filepath.FromSlash(name))
		if entry.IsDir() {
			return os.MkdirAll(destination, 0o755)
		}
		data, err := os.ReadFile(filepath.Join("examples/basic/assets", filepath.FromSlash(name)))
		if err != nil {
			return err
		}
		return os.WriteFile(destination, data, 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Run("embedded-without-source-directory", func(t *testing.T) {
		base := startExample(t, example)
		verifyPage(t, base)
	})
	t.Run("development-refresh", func(t *testing.T) {
		base := startExample(t, example, "-dev", "-root", sources)
		before := verifyPage(t, base)
		if err := os.WriteFile(filepath.Join(sources, "images", "mark.svg"), []byte(`<svg xmlns="http://www.w3.org/2000/svg"><text>changed</text></svg>`), 0o600); err != nil {
			t.Fatal(err)
		}
		after := verifyPage(t, base)
		if before == after {
			t.Fatal("page URLs did not refresh")
		}
		oldCSS := regexp.MustCompile(`href="([^"]+)"`).FindStringSubmatch(before)[1]
		response := fetchResponse(t, base+oldCSS)
		defer response.Body.Close()
		if response.StatusCode != 404 || response.Header.Get("Cache-Control") != "no-store" {
			t.Fatalf("stale CSS: %d %v", response.StatusCode, response.Header)
		}
	})
	output := filepath.Join(workspace, "precompiled")
	command := exec.CommandContext(t.Context(), cli, "precompile", "-root", sources, "-out", output)
	if data, err := command.CombinedOutput(); err != nil {
		t.Fatalf("CLI: %v\n%s", err, data)
	}
	command = exec.CommandContext(t.Context(), cli, "precompile", "-root", sources, "-out", output)
	if data, err := command.CombinedOutput(); err == nil || !strings.Contains(string(data), "already exists") {
		t.Fatalf("CLI overwrite guard: %v\n%s", err, data)
	}
	if err := os.Rename(sources, sources+"-unavailable"); err != nil {
		t.Fatal(err)
	}
	t.Run("precompiled-without-sources", func(t *testing.T) {
		base := startExample(t, example, "-manifest", filepath.Join(output, "manifest.json"))
		verifyPage(t, base)
	})
}

func startExample(t *testing.T, binary string, args ...string) string {
	t.Helper()
	args = append([]string{"-addr", "127.0.0.1:0"}, args...)
	command := exec.CommandContext(t.Context(), binary, args...)
	command.Dir = t.TempDir() // Deliberately has no source assets.
	stderr, err := command.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	ready := make(chan string, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			if _, address, ok := strings.Cut(scanner.Text(), "Example listening at "); ok {
				select {
				case ready <- address:
				default:
				}
			}
		}
	}()
	t.Cleanup(func() { _ = command.Process.Kill(); <-done; _ = command.Wait() })
	select {
	case address := <-ready:
		return address
	case <-done:
		t.Fatal("example exited before becoming ready")
	case <-time.After(15 * time.Second):
		t.Fatal("example did not become ready")
	case <-t.Context().Done():
		t.Fatal(t.Context().Err())
	}
	return ""
}

func fetchResponse(t *testing.T, target string) *http.Response {
	t.Helper()
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Get(target)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func verifyPage(t *testing.T, base string) string {
	t.Helper()
	response := fetchResponse(t, base+"/")
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || response.StatusCode != 200 {
		t.Fatalf("page: %d %v", response.StatusCode, err)
	}
	if !strings.Contains(string(body), "Assets. No frontend toolchain.") {
		t.Fatal("example page missing")
	}
	references := regexp.MustCompile(`(?:href|src)="([^"]+)"`).FindAllStringSubmatch(string(body), -1)
	if len(references) != 3 {
		t.Fatalf("expected CSS, JS and image: %s", body)
	}
	visited := make(map[string]bool)
	var visit func(string)
	visit = func(target string) {
		if visited[target] {
			return
		}
		visited[target] = true
		response := fetchResponse(t, target)
		data, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil || response.StatusCode != 200 {
			t.Fatalf("asset %s: %d %v", target, response.StatusCode, err)
		}
		u, err := url.Parse(target)
		if err != nil {
			t.Fatal(err)
		}
		digest := fmt.Sprintf("%x", sha256.Sum256(data))
		if !strings.Contains(u.Path, "-"+digest+path.Ext(u.Path)) {
			t.Fatalf("fingerprint does not match bytes: %s", target)
		}
		if path.Ext(u.Path) == ".css" {
			for _, match := range regexp.MustCompile(`(?:url\("([^"]+)"\)|@import "([^"]+)")`).FindAllStringSubmatch(string(data), -1) {
				reference := cmp.Or(match[1], match[2])
				relative, err := url.Parse(reference)
				if err != nil {
					t.Fatal(err)
				}
				visit(u.ResolveReference(relative).String())
			}
		}
	}
	for _, reference := range references {
		visit(base + reference[1])
	}
	if len(visited) < 4 {
		t.Fatal("did not follow CSS dependencies")
	}
	return string(body)
}
