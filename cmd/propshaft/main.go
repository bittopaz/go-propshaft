// Command propshaft precompiles assets and merges immutable release archives.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/bittopaz/go-propshaft"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "propshaft:", err)
		os.Exit(1)
	}
}

type rootsFlag []string

func (r *rootsFlag) String() string { return strings.Join(*r, ", ") }
func (r *rootsFlag) Set(value string) error {
	if value == "" {
		return errors.New("asset root must not be empty")
	}
	*r = append(*r, value)
	return nil
}

func run(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Fprintln(stdout, "Usage: propshaft precompile -root DIR [-root DIR ...] -out NEW_DIR [-prefix /assets/]")
		fmt.Fprintln(stdout, "       propshaft merge -manifest-from DIR [-root DIR ...] -out NEW_DIR [-legacy-vite]")
		return nil
	}
	if args[0] == "merge" {
		return runMerge(args[1:], stdout, stderr)
	}
	if args[0] != "precompile" {
		return fmt.Errorf("unknown command %q (expected precompile or merge)", args[0])
	}
	flags := flag.NewFlagSet("precompile", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var roots rootsFlag
	flags.Var(&roots, "root", "asset directory, repeatable; first root wins")
	output := flags.String("out", "", "new output directory (must not exist or overlap any root)")
	prefix := flags.String("prefix", "/assets/", "asset URL prefix, also used to recognize root-absolute CSS references")
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 || len(roots) == 0 || *output == "" {
		return errors.New("precompile requires -root and -out, with no positional arguments")
	}
	canonicalOutput, err := canonicalPath(*output)
	if err != nil {
		return fmt.Errorf("output directory: %w", err)
	}
	var sources []fs.FS
	for _, directory := range roots {
		canonicalRoot, err := filepath.EvalSymlinks(directory)
		if err != nil {
			return fmt.Errorf("root %q: %w", directory, err)
		}
		canonicalRoot, err = filepath.Abs(canonicalRoot)
		if err != nil {
			return err
		}
		if within(canonicalRoot, canonicalOutput) || within(canonicalOutput, canonicalRoot) {
			return fmt.Errorf("output %q overlaps asset root %q", *output, directory)
		}
		// os.Root prevents reads escaping the root, even if a symlink is swapped
		// in concurrently. The compiler also rejects visible symlink entries.
		root, err := os.OpenRoot(canonicalRoot)
		if err != nil {
			return fmt.Errorf("root %q: %w", directory, err)
		}
		defer root.Close()
		sources = append(sources, root.FS())
	}
	pipeline, err := propshaft.New(propshaft.Config{Roots: sources, Prefix: *prefix})
	if err != nil {
		return err
	}
	if err := pipeline.Export(canonicalOutput); err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "Precompiled assets: %s\nManifest: %s\n", canonicalOutput, filepath.Join(canonicalOutput, propshaft.ManifestName))
	return err
}

func within(parent, child string) bool {
	relative, err := filepath.Rel(parent, child)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

// canonicalPath resolves symlinks in the existing ancestor of a possibly new
// output path, so an alias into an input root cannot bypass the overlap check.
func canonicalPath(name string) (string, error) {
	absolute, err := filepath.Abs(name)
	if err != nil {
		return "", err
	}
	current := absolute
	var missing []string
	for {
		_, err := os.Lstat(current)
		if err == nil {
			resolved, err := filepath.EvalSymlinks(current)
			if err != nil {
				return "", err
			}
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return resolved, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", err
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}
