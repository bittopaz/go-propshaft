package propshaft

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
)

// Export writes fingerprinted files and manifest.json to a new directory.
// Existing destinations are refused, including empty directories and symlinks.
// Output is staged in the destination's parent directory and renamed only after
// every write succeeds. It does not replace live deployments or prune old files.
// The caller must keep the destination outside all source roots (the CLI checks
// this; opaque fs.FS roots cannot be inspected by the library).
func (p *Pipeline) Export(directory string) error {
	if directory == "" {
		return fmt.Errorf("propshaft: output directory is required")
	}
	snap, err := p.current()
	if err != nil {
		return err
	}
	absolute, err := filepath.Abs(directory)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(absolute); err == nil {
		return fmt.Errorf("propshaft: output %q already exists: %w", directory, fs.ErrExist)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	parent := filepath.Dir(absolute)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(parent, ".propshaft-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	for _, name := range slices.Sorted(maps.Keys(snap.output)) {
		asset := snap.output[name]
		local, err := filepath.Localize(name)
		if err != nil {
			return fmt.Errorf("propshaft: output path %q: %w", name, err)
		}
		destination := filepath.Join(stage, local)
		if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(destination, asset.data, 0o644); err != nil {
			return err
		}
	}
	manifest, err := snap.manifest()
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(stage, ManifestName), manifest, 0o644); err != nil {
		return err
	}
	if err := os.Chmod(stage, 0o755); err != nil {
		return err
	}
	// Recheck before publication; never intentionally replace a destination.
	// As with other path-based filesystem APIs, callers must control its parent.
	if _, err := os.Lstat(absolute); err == nil {
		return fmt.Errorf("propshaft: output %q appeared during export: %w", directory, fs.ErrExist)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.Rename(stage, absolute); err != nil {
		return fmt.Errorf("propshaft: publish output: %w", err)
	}
	return nil
}
