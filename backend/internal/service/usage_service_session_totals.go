package service

import (
	"context"
	"fmt"

	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
)

// GetSessionTotals returns one keyset page of usage aggregated by (day, user,
// API key, group, client session id). Read-only; nothing is cached or stored.
func (s *UsageService) GetSessionTotals(ctx context.Context, q usagestats.SessionTotalsQuery) (*usagestats.SessionTotalsPage, error) {
	page, err := s.usageRepo.GetSessionTotals(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("get usage session totals: %w", err)
	}
	return page, nil
}
