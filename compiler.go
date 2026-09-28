package propshaft

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io/fs"
	"maps"
	"net/url"
	"path"
	"slices"
	"strings"
)

type compiledAsset struct {
	output string
	digest string
	data   []byte
}

type snapshot struct {
	logical map[string]*compiledAsset
	output  map[string]*compiledAsset
}

func readSources(roots []fs.FS) (map[string][]byte, error) {
	sources := make(map[string][]byte)
	for index, root := range roots {
		if root == nil {
			return nil, fmt.Errorf("propshaft: root %d is nil", index)
		}
		err := fs.WalkDir(root, ".", func(name string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if name != "." && strings.HasPrefix(entry.Name(), ".") {
				if entry.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			if entry.IsDir() {
				return nil
			}
			if !validName(name) {
				return fmt.Errorf("invalid asset path %q: %w", name, fs.ErrInvalid)
			}
			if _, exists := sources[name]; exists {
				return nil
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("asset %q is not a regular file (symlinks are not supported)", name)
			}
			data, err := fs.ReadFile(root, name)
			if err != nil {
				return err
			}
			sources[name] = bytes.Clone(data)
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("propshaft: root %d: %w", index, err)
		}
	}
	return sources, nil
}

func compile(sources map[string][]byte, mount string) (*snapshot, error) {
	snap := &snapshot{logical: make(map[string]*compiledAsset), output: make(map[string]*compiledAsset)}
	visiting := make(map[string]bool)
	var stack []string
	var build func(string) (*compiledAsset, error)
	build = func(name string) (*compiledAsset, error) {
		if asset, ok := snap.logical[name]; ok {
			return asset, nil
		}
		if visiting[name] {
			return nil, fmt.Errorf("propshaft: dependency cycle: %s", strings.Join(append(slices.Clone(stack), name), " -> "))
		}
		data, ok := sources[name]
		if !ok {
			return nil, fmt.Errorf("propshaft: asset %q: %w", name, fs.ErrNotExist)
		}
		visiting[name] = true
		stack = append(stack, name)
		defer func() {
			delete(visiting, name)
			stack = stack[:len(stack)-1]
		}()
		if strings.EqualFold(path.Ext(name), ".css") {
			var err error
			data, err = rewriteCSS(data, func(reference string) (string, error) {
				logical, suffix, managed, err := resolveReference(name, reference, mount)
				if err != nil || !managed {
					return reference, err
				}
				dependency, err := build(logical)
				if err != nil {
					return "", fmt.Errorf("reference %q: %w", reference, err)
				}
				return escapePath(relativePath(path.Dir(name), dependency.output)) + suffix, nil
			})
			if err != nil {
				return nil, fmt.Errorf("propshaft: CSS %q: %w", name, err)
			}
		}
		digest := fmt.Sprintf("%x", sha256.Sum256(data))
		asset := &compiledAsset{output: fingerprintedPath(name, digest), digest: digest, data: data}
		if _, exists := snap.output[asset.output]; exists {
			return nil, fmt.Errorf("propshaft: output path collision: %q", asset.output)
		}
		snap.logical[name], snap.output[asset.output] = asset, asset
		return asset, nil
	}
	for _, name := range slices.Sorted(maps.Keys(sources)) {
		if _, err := build(name); err != nil {
			return nil, err
		}
	}
	return snap, nil
}

func fingerprintedPath(name, digest string) string {
	ext := path.Ext(name)
	return strings.TrimSuffix(name, ext) + "-" + digest + ext
}

func resolveReference(from, reference, mount string) (logical, suffix string, managed bool, err error) {
	if reference == "" || strings.HasPrefix(reference, "#") || strings.HasPrefix(strings.ToLower(reference), "%23") {
		return "", "", false, nil
	}
	u, err := url.Parse(reference)
	if err != nil {
		return "", "", false, fmt.Errorf("invalid URL %q: %w", reference, err)
	}
	if u.Scheme != "" || u.Host != "" || strings.HasPrefix(reference, "//") || u.Path == "" {
		return "", "", false, nil
	}
	if strings.HasPrefix(u.Path, "/") {
		var ok bool
		logical, ok = strings.CutPrefix(u.Path, mount)
		if !ok {
			return "", "", false, nil
		}
		logical = path.Clean(logical)
	} else {
		logical = path.Join(path.Dir(from), u.Path)
	}
	if !validName(logical) {
		return "", "", false, fmt.Errorf("reference %q escapes the asset root or has an invalid path: %w", reference, fs.ErrInvalid)
	}
	if index := strings.IndexAny(reference, "?#"); index >= 0 {
		suffix = reference[index:]
	}
	return logical, suffix, true, nil
}

// relativePath uses slash-separated logical paths on every operating system.
// A leading ./ also keeps colons in a first path segment from becoming a scheme.
func relativePath(directory, target string) string {
	var from []string
	if directory != "." {
		from = strings.Split(directory, "/")
	}
	to := strings.Split(target, "/")
	common := 0
	for common < len(from) && common < len(to)-1 && from[common] == to[common] {
		common++
	}
	result := strings.Repeat("../", len(from)-common) + strings.Join(to[common:], "/")
	if !strings.HasPrefix(result, "../") {
		result = "./" + result
	}
	return result
}
