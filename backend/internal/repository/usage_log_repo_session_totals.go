package repository

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
)

// GetSessionTotals aggregates usage_logs by (day in q.Timezone, user, API key,
// group, client session id) and returns one keyset page in key order.
//
// Every row in the window counts (no success filter): a failed upstream call
// never writes a usage_logs row, and zero-cost rows are still calls. NULL group
// and NULL session id aggregate as their own bucket (key 0 / "").
func (r *usageLogRepository) GetSessionTotals(ctx context.Context, q usagestats.SessionTotalsQuery) (page *usagestats.SessionTotalsPage, err error) {
	if q.Limit <= 0 || q.Limit > usagestats.SessionTotalsMaxPageSize {
		return nil, fmt.Errorf("session totals: limit %d out of range 1..%d", q.Limit, usagestats.SessionTotalsMaxPageSize)
	}
	if strings.TrimSpace(q.Timezone) == "" {
		return nil, fmt.Errorf("session totals: timezone is required")
	}
	if !q.EndTime.After(q.StartTime) {
		return nil, fmt.Errorf("session totals: end %s is not after start %s", q.EndTime, q.StartTime)
	}

	args := []any{q.StartTime, q.EndTime, q.Timezone}
	userFilter := ""
	if q.UserID > 0 {
		args = append(args, q.UserID)
		userFilter = fmt.Sprintf(" AND ul.user_id = $%d", len(args))
	}
	cursorFilter := ""
	if q.After != nil {
		base := len(args)
		args = append(args, q.After.Day, q.After.UserID, q.After.APIKeyID, q.After.GroupID, q.After.SessionID)
		cursorFilter = fmt.Sprintf(
			`WHERE (agg.day COLLATE "C", agg.user_id, agg.api_key_id, agg.group_id, agg.session_id COLLATE "C")`+
				` > ($%d::text COLLATE "C", $%d::bigint, $%d::bigint, $%d::bigint, $%d::text COLLATE "C")`,
			base+1, base+2, base+3, base+4, base+5)
	}
	args = append(args, q.Limit+1)
	limitArg := len(args)

	query := `
		WITH agg AS (
			SELECT
				TO_CHAR(ul.created_at AT TIME ZONE $3, 'YYYY-MM-DD') AS day,
				ul.user_id,
				ul.api_key_id,
				COALESCE(ul.group_id, 0) AS group_id,
				COALESCE(ul.session_id, '') AS session_id,
				COUNT(*) AS calls,
				COALESCE(SUM(ul.input_tokens), 0) AS input_tokens,
				COALESCE(SUM(ul.cache_creation_tokens), 0) AS cache_creation_tokens,
				COALESCE(SUM(ul.cache_read_tokens), 0) AS cache_read_tokens,
				COALESCE(SUM(ul.output_tokens), 0) AS output_tokens,
				COALESCE(SUM(ul.total_cost), 0) AS total_cost,
				COALESCE(SUM(ul.actual_cost), 0) AS actual_cost,
				MIN(ul.created_at) AS first_at,
				MAX(ul.created_at) AS last_at
			FROM usage_logs ul
			WHERE ul.created_at >= $1 AND ul.created_at < $2` + userFilter + `
			GROUP BY 1, 2, 3, 4, 5
		)
		SELECT
			agg.day, agg.user_id, COALESCE(u.email, ''), agg.api_key_id, agg.group_id, agg.session_id,
			agg.calls, agg.input_tokens, agg.cache_creation_tokens, agg.cache_read_tokens, agg.output_tokens,
			agg.total_cost, agg.actual_cost, agg.first_at, agg.last_at
		FROM agg
		LEFT JOIN users u ON u.id = agg.user_id
		` + cursorFilter + `
		ORDER BY agg.day COLLATE "C", agg.user_id, agg.api_key_id, agg.group_id, agg.session_id COLLATE "C"
		LIMIT $` + fmt.Sprint(limitArg)

	rows, err := r.sql.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil && err == nil {
			err = closeErr
			page = nil
		}
	}()

	items := make([]usagestats.SessionTotalsRow, 0)
	for rows.Next() {
		var (
			row       usagestats.SessionTotalsRow
			groupID   int64
			sessionID string
			firstAt   time.Time
			lastAt    time.Time
		)
		if err = rows.Scan(
			&row.Day, &row.UserID, &row.UserEmail, &row.APIKeyID, &groupID, &sessionID,
			&row.Calls, &row.InputTokens, &row.CacheCreationTokens, &row.CacheReadTokens, &row.OutputTokens,
			&row.TotalCost, &row.ActualCost, &firstAt, &lastAt,
		); err != nil {
			return nil, err
		}
		if groupID != 0 {
			gid := groupID
			row.GroupID = &gid
		}
		if sessionID != "" {
			sid := sessionID
			row.SessionID = &sid
		}
		row.FirstAt = firstAt.UTC()
		row.LastAt = lastAt.UTC()
		items = append(items, row)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}

	page = &usagestats.SessionTotalsPage{Items: items}
	if len(items) > q.Limit {
		page.Items = items[:q.Limit]
		next := page.Items[q.Limit-1].Key()
		page.Next = &next
	}
	return page, nil
}
