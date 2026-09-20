// Copyright (c) 2026 Aaron LI
//
// Home page: the program name and version, plus a link to the API index.
//
// Co-authored-by: DeepSeek-v4.1-flash (with Pi Coding Agent)

package main

import (
	"fmt"
	"net/http"
)

const homePageHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>%[1]s</title>
</head>
<body>
<h1>%[1]s %[2]s</h1>
<p><a href="%[3]s">API</a></p>
</body>
</html>
`

func (s *Server) handleHome(w http.ResponseWriter, r *http.Request) {
	if !requireGetHead(w, r) {
		return
	}
	writeSecurityHeaders(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == http.MethodHead {
		return
	}
	fmt.Fprintf(w, homePageHTML, programName, version, apiBase)
}
