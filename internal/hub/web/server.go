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
	"github.com/vasyakrg/recon/internal/hub/investigator"
	hubrunner "github.com/vasyakrg/recon/internal/hub/runner"
	"github.com/vasyakrg/recon/internal/hub/store"
)

//go:embed templates/*.html
var tplFS embed.FS

type Server struct {
	store  *store.Store
	runner *hubrunner.Runner
	loop   *investigator.Loop // optional — nil when LLM is not configured
	tpl    *template.Template
	log    *slog.Logger
}

func NewServer(st *store.Store, runner *hubrunner.Runner, loop *investigator.Loop, log *slog.Logger) (*Server, error) {
	tpl, err := template.New("").Funcs(template.FuncMap{
		"prettyJSON": prettyJSON,
		"truncate":   truncate,
		"bytesOf":    func(s string) []byte { return []byte(s) },
		"mapJSON": func(m map[string]any) string {
			b, _ := json.MarshalIndent(m, "", "  ")
			return string(b)
		},
	}).ParseFS(tplFS, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}
	return &Server{store: st, runner: runner, loop: loop, tpl: tpl, log: log}, nil
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
	mux.HandleFunc("/investigations", s.handleInvestigationsList)
	mux.HandleFunc("/investigations/", s.handleInvestigationsDetail)      // /investigations/{id}
	mux.HandleFunc("/investigations/new", s.handleInvestigationsNew)      // POST: start
	mux.HandleFunc("/investigations/decide", s.handleInvestigationDecide) // POST: approve/skip/end
	mux.HandleFunc("/audit", s.handleAudit)
	mux.HandleFunc("/investigations/hypothesis", s.handleHypothesis)       // POST
	mux.HandleFunc("/findings/", s.handleFindingAction)                    // POST /findings/{id}/{pin|unpin|ignore|unignore}
	mux.HandleFunc("/investigations/export/", s.handleInvestigationExport) // GET /investigations/export/{id}
	mux.HandleFunc("/investigations/events/", s.handleInvestigationSSE)    // GET SSE
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
	s.audit(r.Context(), "operator", "run.create",
		map[string]any{"run_id": runID, "collector": collector, "host_count": len(hosts)})
	http.Redirect(w, r, "/runs/"+runID, http.StatusSeeOther)
}

// audit writes an audit row, escalating any failure to ERROR-level slog —
// audit is the one table where silent loss is unacceptable (review H2).
func (s *Server) audit(ctx context.Context, actor, action string, details map[string]any) {
	if err := s.store.AuditLog(ctx, actor, action, details); err != nil {
		s.log.Error("audit write failed", "actor", actor, "action", action, "err", err)
	}
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

// ---- Investigations -----------------------------------------------------

func (s *Server) handleInvestigationsList(w http.ResponseWriter, r *http.Request) {
	invs, err := s.store.ListInvestigations(r.Context(), 100)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.render(w, "investigations_list", map[string]any{
		"Title":      "Investigations",
		"Version":    version.Version,
		"Items":      invs,
		"LLMEnabled": s.loop != nil,
	})
}

func (s *Server) handleInvestigationsNew(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if s.loop == nil {
		http.Error(w, "investigator disabled — set RECON_LLM_API_KEY and restart hub", http.StatusServiceUnavailable)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 32*1024)
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	goal := strings.TrimSpace(r.FormValue("goal"))
	if goal == "" {
		http.Error(w, "goal required", http.StatusBadRequest)
		return
	}
	id, err := s.loop.Start(r.Context(), goal, "operator")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.audit(r.Context(), "operator", "investigation.start",
		map[string]any{"investigation_id": id, "goal_chars": len(goal)})
	http.Redirect(w, r, "/investigations/"+id, http.StatusSeeOther)
}

func (s *Server) handleInvestigationsDetail(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/investigations/")
	if rest == "" || rest == "new" || rest == "decide" {
		http.NotFound(w, r)
		return
	}
	id := rest
	inv, err := s.store.GetInvestigation(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	tcs, _ := s.store.ListToolCalls(r.Context(), id)
	findings, _ := s.store.ListFindings(r.Context(), id)
	pending, _ := s.store.PendingToolCall(r.Context(), id)

	s.render(w, "investigation_detail", map[string]any{
		"Title":      "Investigation " + id,
		"Version":    version.Version,
		"Inv":        inv,
		"ToolCalls":  tcs,
		"Findings":   findings,
		"Pending":    pending,
		"LLMEnabled": s.loop != nil,
	})
}

func (s *Server) handleInvestigationDecide(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if s.loop == nil {
		http.Error(w, "investigator disabled", http.StatusServiceUnavailable)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 32*1024)
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	id := r.FormValue("investigation_id")
	decision := r.FormValue("decision")
	if id == "" || decision == "" {
		http.Error(w, "investigation_id and decision required", http.StatusBadRequest)
		return
	}
	newInput := r.FormValue("new_input_json")
	if err := s.loop.DecideWithEdit(r.Context(), id, decision, newInput, "operator"); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.audit(r.Context(), "operator", "investigation.decide",
		map[string]any{"investigation_id": id, "decision": decision, "edited": newInput != ""})
	http.Redirect(w, r, "/investigations/"+id, http.StatusSeeOther)
}

func (s *Server) handleHypothesis(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if s.loop == nil {
		http.Error(w, "investigator disabled", http.StatusServiceUnavailable)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 32*1024)
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	id := r.FormValue("investigation_id")
	claim := r.FormValue("claim")
	expected := r.FormValue("expected")
	instruction := r.FormValue("instruction")
	if id == "" || strings.TrimSpace(claim) == "" {
		http.Error(w, "investigation_id and claim required", http.StatusBadRequest)
		return
	}
	if err := s.loop.InjectHypothesis(r.Context(), id, claim, expected, instruction, "operator"); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.audit(r.Context(), "operator", "investigation.hypothesis",
		map[string]any{"investigation_id": id, "claim_chars": len(claim)})
	http.Redirect(w, r, "/investigations/"+id, http.StatusSeeOther)
}

func (s *Server) handleFindingAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/findings/")
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) != 2 {
		http.Error(w, "expected /findings/{id}/{action}", http.StatusBadRequest)
		return
	}
	id, action := parts[0], parts[1]
	f, err := s.store.GetFinding(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	switch action {
	case "pin":
		err = s.store.SetFindingPinned(r.Context(), id, true)
	case "unpin":
		err = s.store.SetFindingPinned(r.Context(), id, false)
	case "ignore":
		// (review M3) Idempotent — re-ignoring an already-ignored finding
		// must not stack duplicate system_notes in the message stream.
		if f.Ignored {
			http.Redirect(w, r, "/investigations/"+f.InvestigationID, http.StatusSeeOther)
			return
		}
		err = s.store.SetFindingIgnored(r.Context(), id, true)
		if err == nil && s.loop != nil {
			_ = s.loop.InjectIgnoreNote(r.Context(), f.InvestigationID, f.Code, f.Message)
		}
	case "unignore":
		// (review M4) Idempotent + emit a restore note so the model sees
		// the IGNORED directive being lifted; otherwise the older "do not
		// investigate" note hangs in context unrebutted.
		if !f.Ignored {
			http.Redirect(w, r, "/investigations/"+f.InvestigationID, http.StatusSeeOther)
			return
		}
		err = s.store.SetFindingIgnored(r.Context(), id, false)
		if err == nil && s.loop != nil {
			_ = s.loop.InjectRestoreNote(r.Context(), f.InvestigationID, f.Code, f.Message)
		}
	default:
		http.Error(w, "unknown action", http.StatusBadRequest)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.audit(r.Context(), "operator", "finding."+action,
		map[string]any{"finding_id": id, "investigation_id": f.InvestigationID})
	http.Redirect(w, r, "/investigations/"+f.InvestigationID, http.StatusSeeOther)
}

func (s *Server) handleInvestigationExport(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/investigations/export/")
	if id == "" {
		http.NotFound(w, r)
		return
	}
	inv, err := s.store.GetInvestigation(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	tcs, _ := s.store.ListToolCalls(r.Context(), id)
	findings, _ := s.store.ListFindings(r.Context(), id)

	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+id+`.md"`)
	_, _ = fmt.Fprintf(w, "# Investigation %s\n\n", inv.ID)
	_, _ = fmt.Fprintf(w, "- **Status:** %s\n- **Model:** %s\n- **Created:** %s\n- **Steps:** %d\n- **Tokens:** %d prompt + %d completion\n\n",
		inv.Status, inv.Model, inv.CreatedAt.UTC().Format(time.RFC3339),
		inv.TotalToolCalls, inv.TotalPromptTokens, inv.TotalCompletionTokens)
	_, _ = fmt.Fprintf(w, "## Goal\n\n> %s\n\n", inv.Goal)

	_, _ = fmt.Fprintf(w, "## Findings\n\n")
	if len(findings) == 0 {
		_, _ = fmt.Fprintln(w, "_(none)_")
	}
	for _, f := range findings {
		mark := ""
		if f.Pinned {
			mark = " 📌"
		}
		if f.Ignored {
			mark = " 🚫"
		}
		_, _ = fmt.Fprintf(w, "- **[%s]** `%s`%s — %s\n", strings.ToUpper(f.Severity), f.Code, mark, f.Message)
	}

	_, _ = fmt.Fprintf(w, "\n## Tool-call timeline\n\n")
	for _, tc := range tcs {
		_, _ = fmt.Fprintf(w, "### %d. `%s` — _%s_\n", tc.Seq, tc.Tool, tc.Status)
		if tc.Rationale != "" {
			_, _ = fmt.Fprintf(w, "> %s\n\n", tc.Rationale)
		}
		_, _ = fmt.Fprintf(w, "**Input:**\n```json\n%s\n```\n", prettyJSON([]byte(tc.InputJSON)))
		if tc.ResultJSON.Valid && tc.ResultJSON.String != "" {
			_, _ = fmt.Fprintf(w, "**Result:**\n```json\n%s\n```\n", prettyJSON([]byte(tc.ResultJSON.String)))
		}
		_, _ = fmt.Fprintln(w)
	}

	if inv.SummaryJSON.Valid {
		_, _ = fmt.Fprintf(w, "## Summary\n\n```json\n%s\n```\n", prettyJSON([]byte(inv.SummaryJSON.String)))
	}
}

// handleInvestigationSSE streams a minimal status pulse to the browser so the
// page reloads itself when something changes. We do NOT stream LLM chunks
// here — the loop is poll-based, so the SSE just announces server-side
// state transitions and the page does its own refresh on `state-change`.
func (s *Server) handleInvestigationSSE(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/investigations/events/")
	if id == "" {
		http.NotFound(w, r)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // nginx friendly

	last := ""
	tick := time.NewTicker(1 * time.Second)
	defer tick.Stop()
	deadline := time.After(5 * time.Minute) // cap connection life
	for {
		select {
		case <-r.Context().Done():
			return
		case <-deadline:
			_, _ = fmt.Fprint(w, "event: bye\ndata: timeout\n\n")
			flusher.Flush()
			return
		case <-tick.C:
			snap, err := s.snapshotForSSE(r.Context(), id)
			if err != nil {
				return
			}
			if snap == last {
				// Heartbeat comment so the connection does not idle-close.
				_, _ = fmt.Fprint(w, ": ping\n\n")
				flusher.Flush()
				continue
			}
			last = snap
			//nolint:gosec // G705: SSE response is text/event-stream, not HTML — no XSS surface; snap is %q-quoted JSON.
			_, _ = fmt.Fprintf(w, "event: state-change\ndata: %s\n\n", snap)
			flusher.Flush()
		}
	}
}

// snapshotForSSE returns a small JSON-ish digest used to detect whether the
// page should self-refresh: status, total_tool_calls, latest tool_call status,
// findings count.
func (s *Server) snapshotForSSE(ctx context.Context, id string) (string, error) {
	inv, err := s.store.GetInvestigation(ctx, id)
	if err != nil {
		return "", err
	}
	tcs, _ := s.store.ListToolCalls(ctx, id)
	findings, _ := s.store.ListFindings(ctx, id)
	lastStatus := ""
	if len(tcs) > 0 {
		lastStatus = tcs[len(tcs)-1].Status
	}
	return fmt.Sprintf(`{"status":%q,"steps":%d,"last":%q,"findings":%d}`,
		inv.Status, inv.TotalToolCalls, lastStatus, len(findings)), nil
}

func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	entries, err := s.store.ListAudit(r.Context(), 200)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.render(w, "audit", map[string]any{
		"Title":   "Audit",
		"Version": version.Version,
		"Entries": entries,
	})
}
