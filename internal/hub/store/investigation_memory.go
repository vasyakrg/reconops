package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const (
	MemoryKindEvidence          = "evidence"
	MemoryKindHypothesis        = "hypothesis"
	MemoryKindRuledOut          = "ruled_out"
	MemoryKindOperatorDirective = "operator_directive"
	MemoryKindContextSummary    = "context_summary"
	MemoryKindFinding           = "finding"
)

type InvestigationMemory struct {
	ID               string
	InvestigationID  string
	Kind             string
	Content          string
	EvidenceRefsJSON string
	MessageSeqStart  int
	MessageSeqEnd    int
	TokenEstimate    int
	CreatedAt        time.Time
}

func (s *Store) AddMemory(ctx context.Context, memory InvestigationMemory) error {
	memory.Kind = strings.TrimSpace(memory.Kind)
	if memory.Kind == "" {
		return fmt.Errorf("memory kind required")
	}
	if memory.ID == "" {
		return fmt.Errorf("memory id required")
	}
	if memory.InvestigationID == "" {
		return fmt.Errorf("investigation_id required")
	}
	memory.Content = strings.TrimSpace(memory.Content)
	if memory.Content == "" {
		return fmt.Errorf("memory content required")
	}
	if strings.TrimSpace(memory.EvidenceRefsJSON) == "" {
		memory.EvidenceRefsJSON = "[]"
	}
	var refs []json.RawMessage
	if err := json.Unmarshal([]byte(memory.EvidenceRefsJSON), &refs); err != nil {
		return fmt.Errorf("evidence_refs_json must be a JSON array: %w", err)
	}
	if memory.CreatedAt.IsZero() {
		memory.CreatedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `
        INSERT INTO investigation_memory (
            id, investigation_id, kind, content, evidence_refs_json,
            message_seq_start, message_seq_end, token_estimate, created_at
        ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		memory.ID, memory.InvestigationID, memory.Kind, memory.Content, memory.EvidenceRefsJSON,
		memory.MessageSeqStart, memory.MessageSeqEnd, memory.TokenEstimate, memory.CreatedAt.UTC())
	return err
}

func (s *Store) ListMemory(ctx context.Context, investigationID string, limit int) ([]InvestigationMemory, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `
        SELECT id, investigation_id, kind, content, evidence_refs_json,
               message_seq_start, message_seq_end, token_estimate, created_at
          FROM investigation_memory
         WHERE investigation_id = ?
         ORDER BY created_at DESC, id DESC
         LIMIT ?`, investigationID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []InvestigationMemory{}
	for rows.Next() {
		var memory InvestigationMemory
		if err := rows.Scan(
			&memory.ID, &memory.InvestigationID, &memory.Kind, &memory.Content, &memory.EvidenceRefsJSON,
			&memory.MessageSeqStart, &memory.MessageSeqEnd, &memory.TokenEstimate, &memory.CreatedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, memory)
	}
	return out, rows.Err()
}
