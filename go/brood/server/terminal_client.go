package server

import (
	"bytes"
	_ "embed"
	"io"
	"net/http"
	"strconv"
	"strings"
)

//go:embed static/driftty.html
var drifttyClient []byte

func useDrifttyClient(r *http.Request, response *http.Response, view agentView) {
	if r.Method != http.MethodGet || response.StatusCode != http.StatusOK || r.URL.Path != view.path() ||
		!strings.EqualFold(strings.TrimSpace(strings.SplitN(response.Header.Get("Content-Type"), ";", 2)[0]), "text/html") {
		return
	}
	response.Body.Close()
	response.Body = io.NopCloser(bytes.NewReader(drifttyClient))
	response.ContentLength = int64(len(drifttyClient))
	response.Header.Set("Content-Length", strconv.Itoa(len(drifttyClient)))
	response.Header.Set("Content-Type", "text/html; charset=utf-8")
	for _, name := range []string{"Content-Encoding", "ETag", "Last-Modified", "Accept-Ranges"} {
		response.Header.Del(name)
	}
}
