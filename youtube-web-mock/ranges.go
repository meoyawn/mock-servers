package youtubewebmock

import (
	"encoding/json"
	"io"
	"net/http"
)

const rangeControlBodyLimit = 1 << 10

func (server *Server) configureRangeFailure(writer http.ResponseWriter, request *http.Request) {
	var failure struct {
		Context string `json:"context"`
		Start   int64  `json:"start"`
	}
	if err := json.NewDecoder(io.LimitReader(request.Body, rangeControlBodyLimit)).
		Decode(&failure); err != nil || failure.Context == "" ||
		failure.Start < 1 {
		http.Error(writer, "invalid range failure", http.StatusBadRequest)
		return
	}
	server.mu.Lock()
	if server.failedRanges == nil {
		server.failedRanges = make(map[string]int64)
	}
	server.failedRanges[failure.Context] = failure.Start
	server.mu.Unlock()
	writer.WriteHeader(http.StatusNoContent)
}

// Media windows release finite real response bytes. Tests can advance their
// client clock after receipt, without sleeping through network-rate windows.
func (server *Server) configureMediaWindow(writer http.ResponseWriter, request *http.Request) {
	var input struct {
		Context string `json:"context"`
		Enabled *bool  `json:"enabled"`
		Bytes   int64  `json:"bytes"`
	}
	if err := json.NewDecoder(io.LimitReader(request.Body, rangeControlBodyLimit)).
		Decode(&input); err != nil || input.Context == "" || input.Bytes < 0 ||
		input.Bytes > 1<<20 {
		http.Error(writer, "invalid media window", http.StatusBadRequest)
		return
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if server.mediaWindows == nil {
		server.mediaWindows = make(map[string]chan int64)
	}
	if input.Enabled != nil {
		server.setMediaWindow(input.Context, *input.Enabled)
		writer.WriteHeader(http.StatusNoContent)
		return
	}
	window := server.mediaWindows[input.Context]
	if window == nil || input.Bytes <= 0 {
		http.Error(writer, "media window is not enabled", http.StatusBadRequest)
		return
	}
	select {
	case window <- input.Bytes:
	default:
		http.Error(writer, "previous media window has not been consumed", http.StatusConflict)
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}

func (server *Server) copyMediaWindows(
	writer io.Writer,
	file io.Reader,
	remaining int64,
	context string,
) {
	server.mu.Lock()
	window := server.mediaWindows[context]
	server.mu.Unlock()
	for window != nil && remaining > 0 {
		credit, open := <-window
		if !open {
			break
		}
		written, err := io.CopyN(writer, file, min(credit, remaining))
		remaining -= written
		if err != nil {
			return
		}
	}
	_, _ = io.CopyN(writer, file, remaining)
}

// setMediaWindow requires server.mu so closing and publishing a window are atomic.
func (server *Server) setMediaWindow(context string, enabled bool) {
	if window := server.mediaWindows[context]; window != nil {
		close(window)
		delete(server.mediaWindows, context)
	}
	if enabled {
		server.mediaWindows[context] = make(chan int64, 1)
	}
}
