// Package web serves the operator UI. Week 1 ships a single page (/hosts);
// subsequent weeks add Investigation, Runs, Audit, Settings.
package web

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"time"

	"github.com/vasyakrg/recon/internal/common/version"
	"github.com/vasyakrg/recon/internal/hub/store"
)

//go:embed templates/*.html
var tplFS embed.FS

type Server struct {
	store *store.Store
	tpl   *template.Template
	log   *slog.Logger
}

func NewServer(st *store.Store, log *slog.Logger) (*Server, error) {
	tpl, err := template.ParseFS(tplFS, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}
	return &Server{store: st, tpl: tpl, log: log}, nil
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleRoot)
	mux.HandleFunc("/hosts", s.handleHosts)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	return mux
}

func (s *Server) Serve(ctx context.Context, addr string) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           s.Routes(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	//nolint:gosec // G118: parent ctx is already done before Shutdown; need fresh ctx for graceful drain.
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	s.log.Info("web listening", "addr", addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, "/hosts", http.StatusFound)
}

func (s *Server) handleHosts(w http.ResponseWriter, r *http.Request) {
	hosts, err := s.store.ListHosts(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.render(w, "hosts.html", map[string]any{
		"Title":   "Hosts",
		"Version": version.Version,
		"Hosts":   hosts,
	})
}

// templates were parsed as a single set; we look up "layout" but the content
// block is provided by the per-page template (hosts.html → defines content).
// Render by executing the page template, which extends layout via {{template
// "layout" .}} indirection — here we instead parse separately per-request
// for simplicity by cloning and adding the page on top.
func (s *Server) render(w http.ResponseWriter, page string, data any) {
	t, err := s.tpl.Clone()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.ExecuteTemplate(w, "layout", data); err != nil {
		s.log.Error("render", "page", page, "err", err)
	}
}
