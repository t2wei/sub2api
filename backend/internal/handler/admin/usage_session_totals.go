package admin

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/adminaccess"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"

	"github.com/gin-gonic/gin"
)

// SessionTotals returns usage aggregated by (day, user, API key, group, client
// session id), one keyset page at a time. Read-only.
//
// GET /api/v1/admin/usage/session-totals
//
//	start_date, end_date  YYYY-MM-DD, inclusive, cut in `timezone` (required)
//	timezone              IANA name, e.g. Asia/Shanghai (required; no server default)
//	user_id               only this user (optional)
//	page_size             1..5000, default 5000
//	cursor                next_cursor of the previous page
//
// Each item: day, user_id, user_email, api_key_id, group_id (null = none),
// session_id (null = the client sent none), calls, input_tokens (uncached),
// cache_creation_tokens, cache_read_tokens, output_tokens, total_cost,
// actual_cost, first_at, last_at. next_cursor is null on the last page.
//
// A scoped admin key needs usage.read; user_email is only filled for a key that
// also holds users.read (contact data stays behind that permission).
func (h *UsageHandler) SessionTotals(c *gin.Context) {
	tzName := strings.TrimSpace(c.Query("timezone"))
	if tzName == "" || tzName == "Local" {
		response.BadRequest(c, "timezone is required (IANA name, e.g. Asia/Shanghai)")
		return
	}
	loc, err := time.LoadLocation(tzName)
	if err != nil {
		response.BadRequest(c, "Invalid timezone: "+tzName)
		return
	}

	startDateStr := strings.TrimSpace(c.Query("start_date"))
	endDateStr := strings.TrimSpace(c.Query("end_date"))
	if startDateStr == "" || endDateStr == "" {
		response.BadRequest(c, "start_date and end_date are required (YYYY-MM-DD)")
		return
	}
	startTime, err := time.ParseInLocation("2006-01-02", startDateStr, loc)
	if err != nil {
		response.BadRequest(c, "Invalid start_date format, use YYYY-MM-DD")
		return
	}
	endDay, err := time.ParseInLocation("2006-01-02", endDateStr, loc)
	if err != nil {
		response.BadRequest(c, "Invalid end_date format, use YYYY-MM-DD")
		return
	}
	if endDay.Before(startTime) {
		response.BadRequest(c, "end_date is before start_date")
		return
	}
	// Half-open [start, next day of end), DST-safe.
	endTime := endDay.AddDate(0, 0, 1)
	if days := int(endTime.Sub(startTime).Hours()/24 + 0.5); days > usagestats.SessionTotalsMaxDays {
		response.BadRequest(c, "date range exceeds "+strconv.Itoa(usagestats.SessionTotalsMaxDays)+" days")
		return
	}

	var userID int64
	if raw := strings.TrimSpace(c.Query("user_id")); raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || id <= 0 {
			response.BadRequest(c, "Invalid user_id")
			return
		}
		userID = id
	}

	limit := usagestats.SessionTotalsMaxPageSize
	if raw := strings.TrimSpace(c.Query("page_size")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > usagestats.SessionTotalsMaxPageSize {
			response.BadRequest(c, "page_size must be 1.."+strconv.Itoa(usagestats.SessionTotalsMaxPageSize))
			return
		}
		limit = n
	}

	var after *usagestats.SessionTotalsKey
	if raw := strings.TrimSpace(c.Query("cursor")); raw != "" {
		key, err := decodeSessionTotalsCursor(raw)
		if err != nil {
			response.BadRequest(c, "Invalid cursor")
			return
		}
		after = key
	}

	page, err := h.usageService.GetSessionTotals(c.Request.Context(), usagestats.SessionTotalsQuery{
		StartTime: startTime,
		EndTime:   endTime,
		Timezone:  tzName,
		UserID:    userID,
		After:     after,
		Limit:     limit,
	})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}

	if principal := adminaccess.RestrictedFromContext(c.Request.Context()); principal != nil &&
		!slices.Contains(principal.Policy.GlobalPermissions, "users.read") {
		for i := range page.Items {
			page.Items[i].UserEmail = ""
		}
	}

	var next any
	if page.Next != nil {
		cursor, err := encodeSessionTotalsCursor(*page.Next)
		if err != nil {
			response.ErrorFrom(c, err)
			return
		}
		next = cursor
	}
	response.Success(c, gin.H{
		"items":       page.Items,
		"next_cursor": next,
		"timezone":    tzName,
		"start":       startTime.UTC(),
		"end":         endTime.UTC(),
	})
}

func encodeSessionTotalsCursor(key usagestats.SessionTotalsKey) (string, error) {
	raw, err := json.Marshal(key)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func decodeSessionTotalsCursor(cursor string) (*usagestats.SessionTotalsKey, error) {
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return nil, err
	}
	var key usagestats.SessionTotalsKey
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&key); err != nil {
		return nil, err
	}
	if _, err := time.Parse("2006-01-02", key.Day); err != nil || key.UserID <= 0 || key.APIKeyID <= 0 || key.GroupID < 0 {
		return nil, errInvalidSessionTotalsCursor
	}
	return &key, nil
}

var errInvalidSessionTotalsCursor = errors.New("invalid session totals cursor")
