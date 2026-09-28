package propshaft

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"path"
	"strings"
)

// ManifestName is the versioned JSON index written into precompile output.
const ManifestName = "manifest.json"

type manifestDocument struct {
	Version int               `json:"version"`
	Assets  map[string]string `json:"assets"`
}

// Manifest resolves precompiled assets without their original source files.
// It is immutable and safe for concurrent use. Serve the output directory with
// a static server or CDN; the manifest itself need not be publicly accessible.
type Manifest struct {
	prefix assetPrefix
	assets map[string]string
}

// LoadManifest reads a version-1 manifest. Prefix follows Config.Prefix's rules
// and may differ from the build prefix because compiled CSS uses relative URLs.
// Loading validates paths and fingerprint shape, not the exported file bytes.
func LoadManifest(r io.Reader, prefix string) (*Manifest, error) {
	parsed, err := parsePrefix(prefix)
	if err != nil {
		return nil, err
	}
	var document manifestDocument
	decoder := json.NewDecoder(r)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("propshaft: manifest: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("propshaft: manifest must contain exactly one JSON document")
	}
	if document.Version != 1 || document.Assets == nil {
		return nil, fmt.Errorf("propshaft: unsupported or incomplete manifest (version %d)", document.Version)
	}
	for logical, output := range document.Assets {
		if !validName(logical) || !validName(output) || !validFingerprint(logical, output) {
			return nil, fmt.Errorf("propshaft: invalid manifest asset %q -> %q", logical, output)
		}
	}
	return &Manifest{prefix: parsed, assets: document.Assets}, nil
}

// URL resolves a logical path to its precompiled URL. Like Pipeline.URL, this
// method can be registered directly as an html/template function.
func (m *Manifest) URL(logical string) (string, error) {
	if !validName(logical) {
		return "", fmt.Errorf("propshaft: invalid logical path %q: %w", logical, fs.ErrInvalid)
	}
	output, ok := m.assets[logical]
	if !ok {
		return "", fmt.Errorf("propshaft: asset %q: %w", logical, fs.ErrNotExist)
	}
	return m.prefix.url + escapePath(output), nil
}

// Entries returns a copy of the logical-to-fingerprinted relative path mappings.
// Modifying the returned map cannot change the manifest or concurrent lookups.
func (m *Manifest) Entries() map[string]string {
	return maps.Clone(m.assets)
}

func validFingerprint(logical, output string) bool {
	ext := path.Ext(logical)
	stem := strings.TrimSuffix(logical, ext) + "-"
	digest, ok := strings.CutPrefix(output, stem)
	if !ok {
		return false
	}
	if ext != "" {
		digest, ok = strings.CutSuffix(digest, ext)
		if !ok {
			return false
		}
	}
	if len(digest) != 64 || digest != strings.ToLower(digest) {
		return false
	}
	_, err := hex.DecodeString(digest)
	return err == nil
}

func (s *snapshot) manifest() ([]byte, error) {
	assets := make(map[string]string, len(s.logical))
	for name, asset := range s.logical {
		assets[name] = asset.output
	}
	data, err := json.MarshalIndent(manifestDocument{Version: 1, Assets: assets}, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}
