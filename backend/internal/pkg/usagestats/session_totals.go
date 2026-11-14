package usagestats

import "time"

// SessionTotalsMaxPageSize bounds one page of the session-totals read.
const SessionTotalsMaxPageSize = 5000

// SessionTotalsMaxDays bounds the requested window (usage_logs keeps ~90 days).
const SessionTotalsMaxDays = 93

// SessionTotalsKey identifies one aggregated row and doubles as the keyset
// cursor. A row without a group carries GroupID 0; a row without a client
// session id carries SessionID "".
type SessionTotalsKey struct {
	Day       string `json:"d"`
	UserID    int64  `json:"u"`
	APIKeyID  int64  `json:"k"`
	GroupID   int64  `json:"g"`
	SessionID string `json:"s"`
}

// SessionTotalsQuery selects usage_logs rows with StartTime <= created_at <
// EndTime and cuts days in Timezone (an IANA name).
type SessionTotalsQuery struct {
	StartTime time.Time
	EndTime   time.Time
	Timezone  string
	UserID    int64 // 0 = every user
	After     *SessionTotalsKey
	Limit     int
}

// SessionTotalsRow is usage aggregated by (day, user, API key, group, client
// session id). Input tokens are the uncached input; the four token kinds are
// disjoint.
type SessionTotalsRow struct {
	Day                 string    `json:"day"`
	UserID              int64     `json:"user_id"`
	UserEmail           string    `json:"user_email"`
	APIKeyID            int64     `json:"api_key_id"`
	GroupID             *int64    `json:"group_id"`
	SessionID           *string   `json:"session_id"`
	Calls               int64     `json:"calls"`
	InputTokens         int64     `json:"input_tokens"`
	CacheCreationTokens int64     `json:"cache_creation_tokens"`
	CacheReadTokens     int64     `json:"cache_read_tokens"`
	OutputTokens        int64     `json:"output_tokens"`
	TotalCost           float64   `json:"total_cost"`
	ActualCost          float64   `json:"actual_cost"`
	FirstAt             time.Time `json:"first_at"`
	LastAt              time.Time `json:"last_at"`
}

// Key returns the keyset position of the row.
func (r SessionTotalsRow) Key() SessionTotalsKey {
	key := SessionTotalsKey{Day: r.Day, UserID: r.UserID, APIKeyID: r.APIKeyID}
	if r.GroupID != nil {
		key.GroupID = *r.GroupID
	}
	if r.SessionID != nil {
		key.SessionID = *r.SessionID
	}
	return key
}

// SessionTotalsPage is one page of rows in key order; Next is nil on the last page.
type SessionTotalsPage struct {
	Items []SessionTotalsRow
	Next  *SessionTotalsKey
}
