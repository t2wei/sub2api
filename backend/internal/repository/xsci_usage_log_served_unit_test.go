//go:build unit

package repository

// [OXSCI] P160 T9: the served-in-window lookup of the model_unsupported verdict.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	dbmigrations "github.com/Wei-Shaw/sub2api/migrations"
	"github.com/stretchr/testify/require"
)

func TestXSciGroupServedModelSince(t *testing.T) {
	since := time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)

	t.Run("query shape and args", func(t *testing.T) {
		var captured []string
		db, mock := newSQLCapturingMock(t, &captured)
		repo := newUsageLogRepositoryWithSQL(nil, db)
		mock.ExpectQuery("served").WithArgs("claude-opus-9-9", since, int64(7)).
			WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))

		served, err := repo.XSciGroupServedModelSince(context.Background(), 7, "claude-opus-9-9", since)
		require.NoError(t, err)
		require.True(t, served)
		require.NoError(t, mock.ExpectationsWereMet())

		require.Len(t, captured, 1)
		q := strings.Join(strings.Fields(captured[0]), " ")
		// Same expression as idx_usage_logs_effective_requested_model_created
		// (migration 226), so the planner can use that index.
		require.Contains(t, q, "COALESCE(NULLIF(BTRIM(requested_model), ''), model) = $1")
		require.Contains(t, q, "created_at >= $2")
		require.Contains(t, q, "group_id = $3")
		require.True(t, strings.HasPrefix(q, "SELECT EXISTS ("), q)
	})

	t.Run("not served", func(t *testing.T) {
		var captured []string
		db, mock := newSQLCapturingMock(t, &captured)
		repo := newUsageLogRepositoryWithSQL(nil, db)
		mock.ExpectQuery("served").WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
		served, err := repo.XSciGroupServedModelSince(context.Background(), 7, "claude-opus-9-9", since)
		require.NoError(t, err)
		require.False(t, served)
	})

	t.Run("error", func(t *testing.T) {
		var captured []string
		db, mock := newSQLCapturingMock(t, &captured)
		repo := newUsageLogRepositoryWithSQL(nil, db)
		mock.ExpectQuery("served").WillReturnError(errors.New("db down"))
		_, err := repo.XSciGroupServedModelSince(context.Background(), 7, "claude-opus-9-9", since)
		require.Error(t, err)
	})
}

func TestXSciGroupServedModelSQLMatchesIndexMigration(t *testing.T) {
	// The index the lookup relies on is created by this migration with the
	// same expression.
	raw, err := dbmigrations.FS.ReadFile(usageLogsEffectiveModelIndexesMigration)
	require.NoError(t, err)
	require.Contains(t, strings.Join(strings.Fields(string(raw)), " "), "(COALESCE(NULLIF(BTRIM(requested_model), ''), model))")
}
