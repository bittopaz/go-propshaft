package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"unicode"

	"github.com/bittopaz/go-propshaft"
)

const rubyManifestName = ".manifest.json"

type archive struct {
	metadata string
	data     []byte
	files    map[string][]byte
}

func runMerge(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("merge", flag.ContinueOnError)
	flags.SetOutput(stderr)
	selected := flags.String("manifest-from", "", "archive whose manifest (or legacy index.html) to preserve; its assets are included")
	output := flags.String("out", "", "new output directory, never an existing directory")
	legacy := flags.Bool("legacy-vite", false, "accept a legacy Vite archive and retain both root and assets/ lookup layouts")
	var roots rootsFlag
	flags.Var(&roots, "root", "additional published archive to retain, repeatable")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 || *selected == "" || *output == "" {
		return errors.New("merge requires -manifest-from and -out, with no positional arguments")
	}
	canonicalOutput, err := canonicalPath(*output)
	if err != nil {
		return err
	}
	if err := requireNewDirectory(canonicalOutput); err != nil {
		return err
	}
	var archives []archive
	for _, directory := range append([]string{*selected}, roots...) {
		canonicalRoot, err := canonicalPath(directory)
		if err != nil {
			return err
		}
		if within(canonicalRoot, canonicalOutput) || within(canonicalOutput, canonicalRoot) {
			return fmt.Errorf("output %q overlaps archive %q", *output, directory)
		}
		root, err := os.OpenRoot(canonicalRoot)
		if err != nil {
			return err
		}
		input, readErr := readArchive(root.FS(), *legacy)
		closeErr := root.Close()
		if readErr != nil {
			return fmt.Errorf("archive %q: %w", directory, readErr)
		}
		if closeErr != nil {
			return closeErr
		}
		archives = append(archives, input)
	}
	files := make(map[string][]byte)
	add := func(name string, data []byte) error {
		if previous, exists := files[name]; exists && !bytes.Equal(previous, data) {
			return fmt.Errorf("immutable asset collision: %s", name)
		}
		files[name] = data
		return nil
	}
	for _, input := range archives {
		for _, name := range slices.Sorted(maps.Keys(input.files)) {
			data := input.files[name]
			if err := add(name, data); err != nil {
				return err
			}
			if *legacy {
				alias, ok := strings.CutPrefix(name, "assets/")
				if !ok {
					alias = "assets/" + name
				}
				if err := add(alias, data); err != nil {
					return err
				}
			}
		}
	}
	chosen := archives[0]
	if err := add(chosen.metadata, chosen.data); err != nil {
		return err
	}
	if err := writeArchive(canonicalOutput, files); err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "Merged assets: %s\nPreserved metadata: %s\n", canonicalOutput, chosen.metadata)
	return err
}

func readArchive(root fs.FS, allowVite bool) (archive, error) {
	files := make(map[string][]byte)
	err := fs.WalkDir(root, ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if name != "." && name != rubyManifestName && strings.HasPrefix(entry.Name(), ".") {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if name != "." && !safeArchivePath(name) {
			return fmt.Errorf("invalid archive path %q", name)
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("non-regular file in archive: %s", name)
		}
		data, err := fs.ReadFile(root, name)
		if err != nil {
			return err
		}
		files[name] = data
		return nil
	})
	if err != nil {
		return archive{}, err
	}
	var metadata string
	var entries map[string]string
	if data, exists := files[propshaft.ManifestName]; exists {
		manifest, err := propshaft.LoadManifest(bytes.NewReader(data), "/")
		if err != nil {
			return archive{}, err
		}
		metadata, entries = propshaft.ManifestName, manifest.Entries()
		for logical, output := range entries {
			data, exists := files[output]
			if !exists {
				return archive{}, fmt.Errorf("incomplete archive: %s", output)
			}
			ext := path.Ext(logical)
			expected := strings.TrimSuffix(logical, ext) + fmt.Sprintf("-%x", sha256.Sum256(data)) + ext
			if output != expected {
				return archive{}, fmt.Errorf("fingerprint does not match bytes: %s", output)
			}
		}
	} else if data, exists := files[rubyManifestName]; exists {
		var document map[string]struct {
			Path string `json:"digested_path"`
		}
		if err := json.Unmarshal(data, &document); err != nil {
			return archive{}, err
		}
		if document == nil {
			return archive{}, errors.New("invalid Ruby Propshaft manifest")
		}
		entries = make(map[string]string, len(document))
		for logical, entry := range document {
			entries[logical] = entry.Path
		}
		metadata = rubyManifestName
	} else if _, exists := files["index.html"]; exists && allowVite {
		info, err := fs.Stat(root, "assets")
		if err != nil || !info.IsDir() {
			return archive{}, errors.New("legacy Vite archive requires an assets directory")
		}
		metadata = "index.html"
	} else {
		return archive{}, errors.New("missing asset manifest (legacy Vite requires -legacy-vite)")
	}
	for logical, output := range entries {
		if !safeArchivePath(logical) || !safeArchivePath(output) || output == logical || output == propshaft.ManifestName || output == rubyManifestName || output == "index.html" {
			return archive{}, fmt.Errorf("invalid manifest entry %q -> %q", logical, output)
		}
		if _, exists := files[output]; !exists {
			return archive{}, fmt.Errorf("incomplete archive: %s", output)
		}
	}
	result := archive{metadata: metadata, data: files[metadata], files: files}
	// Historical metadata is not immutable asset data. Preserve only the
	// selected application's own document, never another release's manifest.
	delete(files, propshaft.ManifestName)
	delete(files, rubyManifestName)
	delete(files, "index.html")
	return result, nil
}

func safeArchivePath(name string) bool {
	return name != "." && fs.ValidPath(name) && !strings.ContainsRune(name, '\\') && !strings.ContainsFunc(name, unicode.IsControl)
}

func requireNewDirectory(directory string) error {
	if _, err := os.Lstat(directory); err == nil {
		return fmt.Errorf("output %q already exists: %w", directory, fs.ErrExist)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

func writeArchive(destination string, files map[string][]byte) error {
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(filepath.Dir(destination), ".propshaft-merge-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	for _, name := range slices.Sorted(maps.Keys(files)) {
		local, err := filepath.Localize(name)
		if err != nil {
			return err
		}
		target := filepath.Join(stage, local)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(target, files[name], 0o644); err != nil {
			return err
		}
	}
	if err := os.Chmod(stage, 0o755); err != nil {
		return err
	}
	if err := requireNewDirectory(destination); err != nil {
		return err
	}
	return os.Rename(stage, destination)
}
