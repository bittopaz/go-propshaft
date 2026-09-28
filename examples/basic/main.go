// Run from the repository root:
//
//	go run ./examples/basic                 # embedded, immutable assets
//	go run ./examples/basic -dev            # live disk assets
//	go run ./examples/basic -manifest /tmp/build/assets/manifest.json
package main

import (
	"bytes"
	"embed"
	"flag"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/bittopaz/go-propshaft"
)

//go:embed assets
var embedded embed.FS

const page = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>go-propshaft example</title>
  <link rel="stylesheet" href="{{asset "styles/app.css"}}">
  <script src="{{asset "app.js"}}" defer></script>
</head>
<body>
  <main>
    <p class="eyebrow">GO · CSS · JAVASCRIPT</p>
    <h1>Assets. No frontend toolchain.</h1>
    <p>This page uses fingerprinted assets. Its stylesheet imports another stylesheet
       and references an SVG; all three participate in dependency invalidation.</p>
    <p><img src="{{asset "images/mark.svg"}}" alt="Propshaft example mark" width="48" height="48"></p>
    <button type="button" id="demo">Test the JavaScript</button>
    <p id="status" role="status">Ready.</p>
  </main>
</body>
</html>`

func main() {
	if err := serve(); err != nil {
		log.Fatal(err)
	}
}

func serve() error {
	dev := flag.Bool("dev", false, "read and refresh assets from disk")
	rootPath := flag.String("root", "examples/basic/assets", "development asset directory")
	manifestPath := flag.String("manifest", "", "serve a precompiled directory using this manifest")
	addr := flag.String("addr", "127.0.0.1:8080", "HTTP listen address")
	flag.Parse()
	if *dev && *manifestPath != "" {
		return fmt.Errorf("-dev and -manifest are mutually exclusive")
	}
	var assetURL func(string) (string, error)
	var handler http.Handler
	if *manifestPath != "" {
		file, err := os.Open(*manifestPath)
		if err != nil {
			return err
		}
		manifest, loadErr := propshaft.LoadManifest(file, "/assets/")
		closeErr := file.Close()
		if loadErr != nil {
			return loadErr
		}
		if closeErr != nil {
			return closeErr
		}
		assetURL = manifest.URL
		handler = http.StripPrefix("/assets/", http.FileServerFS(os.DirFS(filepath.Dir(*manifestPath))))
	} else {
		var root fs.FS
		if *dev {
			directory, err := os.OpenRoot(*rootPath)
			if err != nil {
				return err
			}
			defer directory.Close()
			root = directory.FS()
		} else {
			var err error
			root, err = fs.Sub(embedded, "assets")
			if err != nil {
				return err
			}
		}
		pipeline, err := propshaft.New(propshaft.Config{Roots: []fs.FS{root}, Development: *dev})
		if err != nil {
			return err
		}
		assetURL, handler = pipeline.URL, pipeline.Handler()
	}
	tmpl, err := template.New("page").Funcs(template.FuncMap{"asset": assetURL}).Parse(page)
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.Handle("/assets/", handler)
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		var body bytes.Buffer
		if err := tmpl.Execute(&body, nil); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(body.Bytes())
	})
	listener, err := net.Listen("tcp", *addr)
	if err != nil {
		return err
	}
	log.Printf("Example listening at http://%s", listener.Addr())
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	return server.Serve(listener)
}
