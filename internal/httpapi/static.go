package httpapi

import (
	"net/http"
	"os"
	"path/filepath"
)

// handleConsoleIndex serves the Console single-page entry point.
func (s *Server) handleConsoleIndex(w http.ResponseWriter, r *http.Request) {
	index := filepath.Join(s.webRoot, "index.html")
	if info, err := os.Stat(index); err != nil || info.IsDir() {
		writeDetail(w, http.StatusNotFound, "Not Found")
		return
	}
	http.ServeFile(w, r, index)
}

// handleConsoleAsset serves built Console assets under /console/assets/.
func (s *Server) handleConsoleAsset(w http.ResponseWriter, r *http.Request) {
	http.StripPrefix("/console/assets", http.FileServer(http.Dir(s.webRoot))).ServeHTTP(w, r)
}
