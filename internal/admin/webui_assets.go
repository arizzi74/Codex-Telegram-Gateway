package admin

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

type webUIAssetBundle struct {
	pages       map[string][]byte
	fingerprint string
}

// The shell and every embedded asset share one content fingerprint. An open
// tab keeps its loaded release; its next full navigation requests matching new
// URLs even if a browser/proxy incorrectly retained an old unversioned asset.
var versionedWebUIAssets = sync.OnceValues(func() (webUIAssetBundle, error) {
	page, err := assets.ReadFile("static/webui.html")
	if err != nil {
		return webUIAssetBundle{}, err
	}
	frame, err := assets.ReadFile("static/webui-mermaid-frame.html")
	if err != nil {
		return webUIAssetBundle{}, err
	}
	names := []string{"webui.css", "webui-format.js", "webui-diagrams.js", "webui-mermaid-runtime.js", "webui-commands.js", "webui-command-ui.js", "webui-notifications.js", "webui-drafts.js", "webui.js", "session-auth.js", "session-auth.css"}
	hash := sha256.New()
	_, _ = hash.Write(page)
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write(frame)
	for _, name := range names {
		data, err := assets.ReadFile("static/" + name)
		if err != nil {
			return webUIAssetBundle{}, err
		}
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write([]byte(name))
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write(data)
	}
	version := hex.EncodeToString(hash.Sum(nil))[:20]
	pages := map[string][]byte{"webui": page, "diagram": frame}
	for key, content := range pages {
		for _, name := range names {
			for _, prefix := range []string{"/tgw/webui/static/", "/tgw/admin/static/"} {
				path := prefix + name
				content = bytes.ReplaceAll(content, []byte(path+`"`), []byte(path+"?v="+version+`"`))
			}
		}
		pages[key] = content
	}
	return webUIAssetBundle{pages: pages, fingerprint: version}, nil
})

func versionedWebUIPage() ([]byte, error) {
	bundle, err := versionedWebUIAssets()
	return bundle.pages["webui"], err
}

func versionedWebUIDiagramPage() ([]byte, error) {
	bundle, err := versionedWebUIAssets()
	return bundle.pages["diagram"], err
}

type diagramRuntimeAsset struct {
	identity []byte
	gzip     []byte
}

// Compression is shared across requests. The renderer iframe is replaced when
// sessions change, but this large public library has no conversation content.
var bundledDiagramRuntime = sync.OnceValues(func() (diagramRuntimeAsset, error) {
	data, err := assets.ReadFile("static/webui-mermaid-runtime.js")
	if err != nil {
		return diagramRuntimeAsset{}, err
	}
	var buffer bytes.Buffer
	writer := gzip.NewWriter(&buffer)
	if _, err := writer.Write(data); err != nil {
		return diagramRuntimeAsset{}, err
	}
	if err := writer.Close(); err != nil {
		return diagramRuntimeAsset{}, err
	}
	return diagramRuntimeAsset{identity: data, gzip: buffer.Bytes()}, nil
})

func (s *Server) webuiDiagramRuntime(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		method(w)
		return
	}
	asset, err := bundledDiagramRuntime()
	if err != nil {
		fail(w, err)
		return
	}
	bundle, err := versionedWebUIAssets()
	if err != nil {
		fail(w, err)
		return
	}
	versions := r.URL.Query()["v"]
	if len(versions) == 1 && versions[0] == bundle.fingerprint {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	}
	w.Header().Add("Vary", "Accept-Encoding")
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	data := asset.identity
	if acceptsDiagramGzip(r.Header.Values("Accept-Encoding")) {
		w.Header().Set("Content-Encoding", "gzip")
		data = asset.gzip
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	_, _ = w.Write(data)
}

func acceptsDiagramGzip(headers []string) bool {
	var explicit, wildcard bool
	var foundGzip, foundWildcard bool
	for _, header := range headers {
		for _, value := range strings.Split(header, ",") {
			parts := strings.Split(value, ";")
			coding := strings.TrimSpace(parts[0])
			if !strings.EqualFold(coding, "gzip") && coding != "*" {
				continue
			}
			quality := 1.0
			for _, parameter := range parts[1:] {
				key, value, ok := strings.Cut(strings.TrimSpace(parameter), "=")
				if ok && strings.EqualFold(strings.TrimSpace(key), "q") {
					parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
					if err != nil || !(parsed > 0 && parsed <= 1) {
						quality = 0
						break
					}
					quality = parsed
				}
			}
			if strings.EqualFold(coding, "gzip") {
				if !foundGzip {
					explicit = quality > 0
				} else {
					explicit = explicit && quality > 0
				}
				foundGzip = true
			} else {
				if !foundWildcard {
					wildcard = quality > 0
				} else {
					wildcard = wildcard && quality > 0
				}
				foundWildcard = true
			}
		}
	}
	if foundGzip {
		return explicit
	}
	return wildcard
}
