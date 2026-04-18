package store

import "context"

// NavCounts is the small set of badges shown in the sidebar navigation.
// One round-trip per render keeps the layout simple; if it ever becomes
// hot we can cache it with a short TTL.
type NavCounts struct {
	Hosts                int
	Collectors           int
	InvestigationsActive int
}

func (s *Store) NavCounts(ctx context.Context) (NavCounts, error) {
	var c NavCounts
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM hosts`).Scan(&c.Hosts); err != nil {
		return c, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM collector_manifests`).Scan(&c.Collectors); err != nil {
		// collector_manifests may not exist on a fresh DB until at least one
		// agent reports — treat as zero rather than fail the whole render.
		c.Collectors = 0
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM investigations WHERE status IN ('active','waiting')`).Scan(&c.InvestigationsActive); err != nil {
		c.InvestigationsActive = 0
	}
	return c, nil
}
