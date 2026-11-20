package repository

// [OXSCI] P160 T9: "has this group served this model lately?" for the
// model_unsupported verdict (service.XSciModelUnsupported). Asked only after
// a request already failed with "model not found" answers; never on the
// success path.

import (
	"context"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

var _ service.XSciModelServedLookup = (*usageLogRepository)(nil)

// xsciGroupServedModelSQL matches the requested model with the expression of
// idx_usage_logs_effective_requested_model_created (migration 226), so a model
// nobody asked for in the window is an empty index range, and EXISTS stops at
// the first row of the group otherwise.
const xsciGroupServedModelSQL = `SELECT EXISTS (
	SELECT 1 FROM usage_logs
	WHERE COALESCE(NULLIF(BTRIM(requested_model), ''), model) = $1
	  AND created_at >= $2
	  AND group_id = $3
)`

// XSciGroupServedModelSince reports whether a usage log row (a request that
// was served and billed) exists for the group and the client-requested model
// since the given time.
func (r *usageLogRepository) XSciGroupServedModelSince(ctx context.Context, groupID int64, model string, since time.Time) (bool, error) {
	var served bool
	if err := scanSingleRow(ctx, r.sql, xsciGroupServedModelSQL, []any{model, since, groupID}, &served); err != nil {
		return false, err
	}
	return served, nil
}
