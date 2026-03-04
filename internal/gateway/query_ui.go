package gateway

import (
	"embed"
	"net/http"
)

//go:embed ui/index.html
var queryUIFS embed.FS

func (h *Handler) QueryUI(w http.ResponseWriter, _ *http.Request) {
	body, err := queryUIFS.ReadFile("ui/index.html")
	if err != nil {
		h.logger.Error("failed to load query UI")
		http.Error(w, "query ui unavailable", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}
