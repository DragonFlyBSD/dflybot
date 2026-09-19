// Copyright (c) 2026 Aaron LI
//
// Short-link redirect handler. The redirect path is read-only: one db.View per
// request and no writes (C11).
//
// Co-authored-by: DeepSeek-v4.1-flash (with Pi Coding Agent)

package main

import (
	"errors"
	"net/http"
)

func (s *Server) handleRedirect(w http.ResponseWriter, r *http.Request) {
	writeSecurityHeaders(w)
	w.Header().Set("Cache-Control", "no-store")

	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	key := r.URL.Path
	link, err := s.store.Get(key)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			http.Error(w, "404 page not found", http.StatusNotFound)
			return
		}
		s.logger.Error("redirect lookup failed", "key", key, "error", err)
		s.status.SetError(err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	ai := accessInfoFrom(r)
	ai.Type = AccessTypeRedirect
	ai.Key = key
	ai.Target = link.Target
	ai.Rule = link.Rule

	w.Header().Set("Location", link.Target)
	w.WriteHeader(http.StatusFound)
	if r.Method == http.MethodGet {
		w.Write([]byte("Redirecting to " + link.Target + "\n"))
	}
}
