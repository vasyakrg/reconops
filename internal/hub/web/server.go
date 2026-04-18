// Package web serves the operator UI. Week 2 ships pages for hosts inventory,
// collector catalog, run launching and run inspection (incl. artifacts).
package web

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/vasyakrg/recon/internal/common/version"
	hubrunner "github.com/vasyakrg/recon/internal/hub/runner"
	"github.com/vasyakrg/recon/internal/hub/store"
)

//go:embed templates/*.html
var tplFS embed.FS

type Server struct {
	store  *store.Store
	runner *hubrunner.Runner
	tpl    *template.Template
	log    *slog.Logger
}

func NewServer(st *store.Store, runner *hubrunner.Runner, log *slog.Logger) (*Server, error) {
	tpl, err := template.New("").Funcs(template.FuncMap{
		"prettyJSON": prettyJSON,
		"truncate":   truncate,
	}).ParseFS(tplFS, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}
	return &Server{store: st, runner: runner, tpl: tpl, log: log}, nil
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleRoot)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/hosts", s.handleHosts)
	mux.HandleFunc("/hosts/", s.handleHostDetail) // /hosts/{id}
	mux.HandleFunc("/collectors", s.handleCollectorsCatalog)
	mux.HandleFunc("/runs", s.handleRunsList)
	mux.HandleFunc("/runs/", s.handleRunsDetail) // /runs/{id} | /runs/{id}/artifact?...
	mux.HandleFunc("/runs/new", s.handleRunsNew) // POST: launch a run
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
	s.render(w, "hosts", map[string]any{
		"Title":        "Hosts",
		"Version":      version.Version,
		"ContentBlock": "hosts",
		"Hosts":        hosts,
	})
}

func (s *Server) handleHostDetail(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/hosts/")
	if id == "" || strings.Contains(id, "/") {
		http.NotFound(w, r)
		return
	}
	host, err := s.store.GetHost(r.Context(), id)
	if err != nil {
		http.Error(w, "host not found", http.StatusNotFound)
		return
	}
	mans, _ := s.store.ListCollectorManifests(r.Context(), id)
	s.render(w, "host_detail", map[string]any{
		"Title":        "Host " + id,
		"Version":      version.Version,
		"ContentBlock": "host_detail",
		"Host":         host,
		"Collectors":   mans,
	})
}

func (s *Server) handleCollectorsCatalog(w http.ResponseWriter, r *http.Request) {
	hosts, _ := s.store.ListHosts(r.Context())
	type entry struct {
		Name   string
		Hosts  []string
		Latest string
	}
	byName := map[string]*entry{}
	for _, h := range hosts {
		mans, _ := s.store.ListCollectorManifests(r.Context(), h.ID)
		for _, m := range mans {
			e, ok := byName[m.Name]
			if !ok {
				e = &entry{Name: m.Name}
				byName[m.Name] = e
			}
			e.Hosts = append(e.Hosts, h.ID)
			e.Latest = m.Version
		}
	}
	var entries []*entry
	for _, e := range byName {
		entries = append(entries, e)
	}
	s.render(w, "collectors", map[string]any{
		"Title":        "Collectors",
		"Version":      version.Version,
		"ContentBlock": "collectors",
		"Entries":      entries,
	})
}

func (s *Server) handleRunsList(w http.ResponseWriter, r *http.Request) {
	runs, err := s.store.ListRuns(r.Context(), 100)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.render(w, "runs_list", map[string]any{
		"Title":        "Runs",
		"Version":      version.Version,
		"ContentBlock": "runs_list",
		"Runs":         runs,
	})
}

func (s *Server) handleRunsDetail(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/runs/")
	if rest == "" || rest == "new" {
		http.NotFound(w, r)
		return
	}
	parts := strings.SplitN(rest, "/", 2)
	runID := parts[0]
	if len(parts) == 2 && strings.HasPrefix(parts[1], "artifact/") {
		s.serveArtifact(w, r, runID, strings.TrimPrefix(parts[1], "artifact/"))
		return
	}
	run, err := s.store.GetRun(r.Context(), runID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	tasks, _ := s.store.ListTasks(r.Context(), runID)
	type tview struct {
		store.Task
		Result *store.Result
	}
	views := make([]tview, 0, len(tasks))
	for _, t := range tasks {
		v := tview{Task: t}
		if res, err := s.store.GetResult(r.Context(), t.ID); err == nil {
			v.Result = &res
		}
		views = append(views, v)
	}
	s.render(w, "run_detail", map[string]any{
		"Title":        "Run " + runID,
		"Version":      version.Version,
		"ContentBlock": "run_detail",
		"Run":          run,
		"Tasks":        views,
	})
}

func (s *Server) serveArtifact(w http.ResponseWriter, r *http.Request, taskID, name string) {
	res, err := s.store.GetResult(r.Context(), taskID)
	if err != nil || res.ArtifactDir == "" {
		http.NotFound(w, r)
		return
	}
	clean := filepath.Clean(filepath.Join(res.ArtifactDir, name))
	if !strings.HasPrefix(clean, filepath.Clean(res.ArtifactDir)+string(os.PathSeparator)) {
		http.Error(w, "path traversal", http.StatusBadRequest)
		return
	}
	http.ServeFile(w, r, clean)
}

func (s *Server) handleRunsNew(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64*1024) // hard cap on form size
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	hosts := r.Form["host_id"]
	collector := r.FormValue("collector")
	if len(hosts) == 0 || collector == "" {
		http.Error(w, "host_id and collector required", http.StatusBadRequest)
		return
	}
	params := map[string]string{}
	for k, v := range r.Form {
		if strings.HasPrefix(k, "param_") && len(v) > 0 && v[0] != "" {
			params[strings.TrimPrefix(k, "param_")] = v[0]
		}
	}
	runID, err := s.runner.CreateRun(r.Context(), hubrunner.RunRequest{
		Name:      r.FormValue("name"),
		HostIDs:   hosts,
		Collector: collector,
		Params:    params,
		CreatedBy: "operator", // week 2: no auth, see plan
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/runs/"+runID, http.StatusSeeOther)
}

// render executes layout.html, dynamically aliasing the "content" block to
// the per-page template. Each page template defines a uniquely-named block
// (e.g. "hosts", "run_detail") so they don't clash; the alias is set per
// request on a clone of the parsed set.
func (s *Server) render(w http.ResponseWriter, page string, data any) {
	t, err := s.tpl.Clone()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if _, err := t.New("content").Parse(fmt.Sprintf(`{{template %q .}}`, page)); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.ExecuteTemplate(w, "layout", data); err != nil {
		s.log.Error("render", "page", page, "err", err)
	}
}

// prettyJSON formats raw JSON bytes for display. Best-effort — returns the
// input as a string on parse error.
func prettyJSON(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return string(raw)
	}
	return string(out)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
