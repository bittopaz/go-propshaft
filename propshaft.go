// Package propshaft fingerprints assets, rewrites CSS dependencies, and serves
// or exports the results without requiring a frontend build toolchain.
package propshaft

import (
	"bytes"
	"cmp"
	"fmt"
	"io/fs"
	"net/url"
	"path"
	"slices"
	"strings"
	"sync"
	"unicode"
)

// Config defines an asset collection. Roots are searched in order: the first
// file with a given logical path wins. Dotfiles and dot-directories are ignored.
// Roots must expose regular files and directories, not symlinks or special files.
// The caller owns the roots and must keep them usable throughout development.
type Config struct {
	Roots []fs.FS
	// Prefix is an absolute URL path or an HTTP(S) URL, defaulting to /assets/.
	// It also identifies managed root-absolute references in source CSS.
	Prefix string
	// Development rescans sources on every URL lookup and asset request. Changes
	// are detected by content, including additions and deletions. Rebuild errors
	// are returned to the caller; incomplete snapshots are never published.
	Development bool
}

// Pipeline owns immutable compiled assets. Its methods are safe for concurrent
// use, provided callers do not mutate its source filesystems unsafely.
type Pipeline struct {
	roots       []fs.FS
	prefix      assetPrefix
	development bool
	mu          sync.Mutex
	sources     map[string][]byte
	snapshot    *snapshot
}

type assetPrefix struct {
	url   string
	mount string
}

// New reads and compiles every visible asset. Production pipelines do not read
// their roots again; development pipelines rescan on demand.
func New(config Config) (*Pipeline, error) {
	if len(config.Roots) == 0 {
		return nil, fmt.Errorf("propshaft: at least one asset root is required")
	}
	prefix, err := parsePrefix(config.Prefix)
	if err != nil {
		return nil, err
	}
	roots := slices.Clone(config.Roots)
	sources, err := readSources(roots)
	if err != nil {
		return nil, err
	}
	snap, err := compile(sources, prefix.mount)
	if err != nil {
		return nil, err
	}
	return &Pipeline{roots: roots, prefix: prefix, development: config.Development, sources: sources, snapshot: snap}, nil
}

// URL resolves a logical path such as "styles/app.css" to its fingerprinted URL.
// It is suitable for html/template.FuncMap: {"asset": pipeline.URL}. Missing
// assets return an error wrapping fs.ErrNotExist. Queries and fragments belong
// to the returned URL, not to the logical path argument.
func (p *Pipeline) URL(logical string) (string, error) {
	if !validName(logical) {
		return "", fmt.Errorf("propshaft: invalid logical path %q: %w", logical, fs.ErrInvalid)
	}
	snap, err := p.current()
	if err != nil {
		return "", err
	}
	asset, ok := snap.logical[logical]
	if !ok {
		return "", fmt.Errorf("propshaft: asset %q: %w", logical, fs.ErrNotExist)
	}
	return p.prefix.url + escapePath(asset.output), nil
}

func (p *Pipeline) current() (*snapshot, error) {
	if !p.development {
		return p.snapshot, nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	sources, err := readSources(p.roots)
	if err != nil {
		return nil, err
	}
	if sameSources(sources, p.sources) {
		return p.snapshot, nil
	}
	snap, err := compile(sources, p.prefix.mount)
	if err != nil {
		return nil, err
	}
	p.sources, p.snapshot = sources, snap
	return snap, nil
}

func sameSources(a, b map[string][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for name, data := range a {
		other, ok := b[name]
		if !ok || !bytes.Equal(data, other) {
			return false
		}
	}
	return true
}

func validName(name string) bool {
	return name != "." && fs.ValidPath(name) && !strings.ContainsRune(name, '\\') && !strings.ContainsFunc(name, unicode.IsControl)
}

func escapePath(name string) string {
	return (&url.URL{Path: name}).EscapedPath()
}

func parsePrefix(raw string) (assetPrefix, error) {
	raw = cmp.Or(raw, "/assets/")
	u, err := url.Parse(raw)
	if err != nil {
		return assetPrefix{}, fmt.Errorf("propshaft: prefix: %w", err)
	}
	if (u.Scheme != "" && u.Scheme != "http" && u.Scheme != "https") ||
		(u.Scheme != "" && u.Host == "") || (u.Scheme == "" && u.Host != "") ||
		u.User != nil || u.Opaque != "" || strings.ContainsAny(raw, "?#") ||
		!strings.HasPrefix(u.Path, "/") || strings.ContainsRune(u.Path, '\\') ||
		strings.ContainsFunc(u.Path, unicode.IsControl) {
		return assetPrefix{}, fmt.Errorf("propshaft: invalid prefix %q", raw)
	}
	mount := strings.TrimSuffix(u.Path, "/")
	if mount == "" {
		mount = "/"
	}
	if path.Clean(mount) != mount {
		return assetPrefix{}, fmt.Errorf("propshaft: prefix must have a clean path: %q", raw)
	}
	u.Path = strings.TrimSuffix(mount, "/") + "/"
	u.RawPath = ""
	return assetPrefix{url: u.String(), mount: u.Path}, nil
}
