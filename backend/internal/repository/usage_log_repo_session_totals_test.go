//go:build unit

package repository

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
)

var sessionTotalsColumns = []string{
	"day", "user_id", "email", "api_key_id", "group_id", "session_id",
	"calls", "input_tokens", "cache_creation_tokens", "cache_read_tokens", "output_tokens",
	"total_cost", "actual_cost", "first_at", "last_at",
}

func TestUsageLogRepositoryGetSessionTotalsFirstPage(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := newUsageLogRepositoryWithSQL(nil, db)
	start := time.Date(2026, 3, 8, 16, 0, 0, 0, time.UTC)
	end := time.Date(2026, 3, 10, 16, 0, 0, 0, time.UTC)
	at := time.Date(2026, 3, 9, 1, 0, 0, 0, time.UTC)

	// No user filter and no cursor clause on the first page; limit asks for one extra row.
	mock.ExpectQuery(`(?s)AT TIME ZONE \$3.*WHERE ul\.created_at >= \$1 AND ul\.created_at < \$2\s+GROUP BY 1, 2, 3, 4, 5.*LEFT JOIN users u ON u\.id = agg\.user_id\s+ORDER BY .*LIMIT \$4$`).
		WithArgs(start, end, "Asia/Shanghai", 3).
		WillReturnRows(sqlmock.NewRows(sessionTotalsColumns).
			AddRow("2026-03-09", int64(1), "a@x", int64(10), int64(0), "", int64(2), int64(5), int64(0), int64(0), int64(5), 0.1, 0.0, at, at).
			AddRow("2026-03-09", int64(1), "a@x", int64(10), int64(7), "s1", int64(3), int64(1), int64(2), int64(3), int64(4), 0.5, 0.25, at, at).
			AddRow("2026-03-09", int64(2), "b@x", int64(11), int64(7), "s2", int64(1), int64(1), int64(1), int64(1), int64(1), 0.2, 0.2, at, at))

	page, err := repo.GetSessionTotals(context.Background(), usagestats.SessionTotalsQuery{
		StartTime: start, EndTime: end, Timezone: "Asia/Shanghai", Limit: 2,
	})
	require.NoError(t, err)
	require.Len(t, page.Items, 2)
	require.Nil(t, page.Items[0].GroupID, "group 0 is no group")
	require.Nil(t, page.Items[0].SessionID, "empty session is no session id")
	require.Equal(t, int64(7), *page.Items[1].GroupID)
	require.Equal(t, "s1", *page.Items[1].SessionID)
	require.Equal(t, &usagestats.SessionTotalsKey{Day: "2026-03-09", UserID: 1, APIKeyID: 10, GroupID: 7, SessionID: "s1"}, page.Next)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestUsageLogRepositoryGetSessionTotalsCursorAndUser(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := newUsageLogRepositoryWithSQL(nil, db)
	start := time.Date(2026, 3, 8, 16, 0, 0, 0, time.UTC)
	end := time.Date(2026, 3, 10, 16, 0, 0, 0, time.UTC)
	after := &usagestats.SessionTotalsKey{Day: "2026-03-09", UserID: 1, APIKeyID: 10, GroupID: 0, SessionID: ""}

	mock.ExpectQuery(`(?s)WHERE ul\.created_at >= \$1 AND ul\.created_at < \$2 AND ul\.user_id = \$4\s+GROUP BY.*WHERE \(agg\.day COLLATE "C", agg\.user_id, agg\.api_key_id, agg\.group_id, agg\.session_id COLLATE "C"\) > \(\$5::text COLLATE "C", \$6::bigint, \$7::bigint, \$8::bigint, \$9::text COLLATE "C"\).*LIMIT \$10$`).
		WithArgs(start, end, "UTC", int64(1), "2026-03-09", int64(1), int64(10), int64(0), "", 6).
		WillReturnRows(sqlmock.NewRows(sessionTotalsColumns))

	page, err := repo.GetSessionTotals(context.Background(), usagestats.SessionTotalsQuery{
		StartTime: start, EndTime: end, Timezone: "UTC", UserID: 1, After: after, Limit: 5,
	})
	require.NoError(t, err)
	require.Empty(t, page.Items)
	require.Nil(t, page.Next)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestUsageLogRepositoryGetSessionTotalsRejectsBadQuery(t *testing.T) {
	db, _ := newSQLMock(t)
	repo := newUsageLogRepositoryWithSQL(nil, db)
	start := time.Date(2026, 3, 8, 16, 0, 0, 0, time.UTC)
	for name, q := range map[string]usagestats.SessionTotalsQuery{
		"zero limit":  {StartTime: start, EndTime: start.Add(time.Hour), Timezone: "UTC", Limit: 0},
		"over limit":  {StartTime: start, EndTime: start.Add(time.Hour), Timezone: "UTC", Limit: usagestats.SessionTotalsMaxPageSize + 1},
		"no timezone": {StartTime: start, EndTime: start.Add(time.Hour), Limit: 1},
		"empty range": {StartTime: start, EndTime: start, Timezone: "UTC", Limit: 1},
	} {
		_, err := repo.GetSessionTotals(context.Background(), q)
		require.Error(t, err, name)
	}
}
