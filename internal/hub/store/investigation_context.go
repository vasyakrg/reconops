package store

import (
	"context"
	"time"
)

type InvestigationContextTurn struct {
	ID                       int64
	InvestigationID          string
	MessageSeq               int
	Operation                string
	ModelProfile             string
	EstimatedPromptTokens    int
	ProviderPromptTokens     int
	ProviderCompletionTokens int
	ContextWindowTokens      int
	ThresholdTokens          int
	ReservedOutputTokens     int
	SafetyHeadroomTokens     int
	ShouldCompact            bool
	CompactionReason         string
	CreatedAt                time.Time
}

func (s *Store) InsertContextTurn(ctx context.Context, turn InvestigationContextTurn) (int64, error) {
	if turn.CreatedAt.IsZero() {
		turn.CreatedAt = time.Now().UTC()
	}
	res, err := s.db.ExecContext(ctx, `
        INSERT INTO investigation_context_turns (
            investigation_id, message_seq, operation, model_profile,
            estimated_prompt_tokens, provider_prompt_tokens, provider_completion_tokens,
            context_window_tokens, threshold_tokens, reserved_output_tokens,
            safety_headroom_tokens, should_compact, compaction_reason, created_at
        ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		turn.InvestigationID, turn.MessageSeq, turn.Operation, turn.ModelProfile,
		turn.EstimatedPromptTokens, turn.ProviderPromptTokens, turn.ProviderCompletionTokens,
		turn.ContextWindowTokens, turn.ThresholdTokens, turn.ReservedOutputTokens,
		turn.SafetyHeadroomTokens, boolToInt(turn.ShouldCompact), nullable(turn.CompactionReason), turn.CreatedAt.UTC())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) UpdateContextTurnUsage(ctx context.Context, id int64, promptTokens, completionTokens int) error {
	_, err := s.db.ExecContext(ctx, `
        UPDATE investigation_context_turns
           SET provider_prompt_tokens = ?,
               provider_completion_tokens = ?
         WHERE id = ?`,
		promptTokens, completionTokens, id)
	return err
}

func (s *Store) ListContextTurns(ctx context.Context, investigationID string, limit int) ([]InvestigationContextTurn, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `
        SELECT id, investigation_id, message_seq, operation, model_profile,
               estimated_prompt_tokens, provider_prompt_tokens, provider_completion_tokens,
               context_window_tokens, threshold_tokens, reserved_output_tokens,
               safety_headroom_tokens, should_compact, COALESCE(compaction_reason, ''), created_at
          FROM investigation_context_turns
         WHERE investigation_id = ?
         ORDER BY created_at DESC, id DESC
         LIMIT ?`, investigationID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []InvestigationContextTurn{}
	for rows.Next() {
		var turn InvestigationContextTurn
		var shouldCompact int
		if err := rows.Scan(
			&turn.ID, &turn.InvestigationID, &turn.MessageSeq, &turn.Operation, &turn.ModelProfile,
			&turn.EstimatedPromptTokens, &turn.ProviderPromptTokens, &turn.ProviderCompletionTokens,
			&turn.ContextWindowTokens, &turn.ThresholdTokens, &turn.ReservedOutputTokens,
			&turn.SafetyHeadroomTokens, &shouldCompact, &turn.CompactionReason, &turn.CreatedAt,
		); err != nil {
			return nil, err
		}
		turn.ShouldCompact = shouldCompact == 1
		out = append(out, turn)
	}
	return out, rows.Err()
}
