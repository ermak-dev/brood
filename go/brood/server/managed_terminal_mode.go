package server

import (
	"fmt"
	"html"
	"net/http"
)

// serveTerminalModeChoice is only navigation. A page visit does not claim a
// writable attachment or interrupt work. The selected rw ticket is immutable;
// its eventual websocket boundary records the intervention before any bytes.
func serveTerminalModeChoice(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; frame-ancestors 'self'")
	query := r.URL.Query()
	query.Set("mode", "ro")
	readonly := html.EscapeString("?" + query.Encode())
	query.Set("mode", "rw")
	writable := html.EscapeString("?" + query.Encode())
	fmt.Fprintf(w, `<!doctype html><html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Terminal mode</title><body style="font:16px system-ui;margin:0;padding:24px 16px;background:#f6f6f4;color:#202320"><main style="max-width:38rem;margin:auto"><h1>Choose terminal mode</h1><p>Opening this page has not changed the actor or its execution.</p><p><a href="%s">Read-only snapshot</a>: inspect the screen without input, resize, or interruption.</p><p><a href="%s">Open writable terminal</a>: explicitly take control. Opening its live connection may interrupt automatic execution. Child records and uncertain effects remain for reconciliation; disconnect does not prove completion.</p></main></body></html>`, readonly, writable)
}
