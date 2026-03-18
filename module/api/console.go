package api

import (
	_ "embed"
	"net/http"
)

//go:embed console.html
var consoleHTML []byte

func (m *Module) handleConsole(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(consoleHTML)
}
