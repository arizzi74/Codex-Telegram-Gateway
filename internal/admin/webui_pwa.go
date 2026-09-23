package admin

import "net/http"

// Only the public application shell and notification worker are installable.
// The worker does not intercept requests or cache authenticated content.
func (s *Server) webuiPWARoutes() {
	s.mux.HandleFunc("/tgw/webui/manifest.webmanifest", func(w http.ResponseWriter, r *http.Request) {
		serveAsset(w, r, "static/webui-manifest.webmanifest", "application/manifest+json")
	})
	s.mux.HandleFunc("/tgw/webui/sw.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Service-Worker-Allowed", "/tgw/webui/")
		serveAsset(w, r, "static/webui-sw.js", "application/javascript; charset=utf-8")
	})
	for _, asset := range []struct{ name, contentType string }{
		{"webui-notifications.js", "application/javascript; charset=utf-8"},
		{"webui-icon.svg", "image/svg+xml"},
		{"webui-icon-192.png", "image/png"},
		{"webui-icon-512.png", "image/png"},
		{"webui-icon-180.png", "image/png"},
	} {
		s.mux.HandleFunc("/tgw/webui/static/"+asset.name, func(w http.ResponseWriter, r *http.Request) {
			serveAsset(w, r, "static/"+asset.name, asset.contentType)
		})
	}
}
