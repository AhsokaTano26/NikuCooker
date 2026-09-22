package qc

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/AhsokaTano26/NikuCooker/internal/database"
)

// ErrNotFound reports a finding that does not exist.
var ErrNotFound = errors.New("qc: finding not found")

// StoredFinding is a finding as the database holds it.
//
// Distinct from Finding, which is what a rule produces. A stored finding has an
// identity, a resolution and a timestamp; a produced one has a code and a
// message. Conflating them would mean a rule had to invent a primary key.
type StoredFinding struct {
	ID string `json:"id"`

	// SegmentID is empty for a project-level finding.
	SegmentID string `json:"segment_id"`

	SegmentOrdinal int `json:"segment_ordinal,omitempty"`

	Stage    string   `json:"stage"`
	Severity Severity `json:"severity"`
	Code     string   `json:"code"`
	Message  string   `json:"message"`

	Suggestion *string `json:"suggestion"`

	Resolved   bool       `json:"resolved"`
	ResolvedAt *time.Time `json:"resolved_at,omitempty"`

	CreatedAt time.Time `json:"created_at"`
}

// Repository reads and writes findings.
type Repository struct {
	db *database.DB
}

// NewRepository binds a repository to a database.
func NewRepository(db *database.DB) *Repository { return &Repository{db: db} }

// ListQuery filters findings.
type ListQuery struct {
	Severity Severity
	Code     string
	Resolved bool
	Limit    int
	Offset   int
}

// List returns a page of findings and the total matching count.
func (r *Repository) List(ctx context.Context, projectID string, query ListQuery) ([]StoredFinding, int, error) {
	if query.Limit <= 0 || query.Limit > 500 {
		query.Limit = 200
	}

	where := []string{"q.project_id = ?"}
	args := []any{projectID}

	if query.Severity != "" {
		where = append(where, "q.severity = ?")
		args = append(args, string(query.Severity))
	}
	if query.Code != "" {
		where = append(where, "q.code = ?")
		args = append(args, query.Code)
	}
	// Resolved findings are hidden unless explicitly asked for. The queue is
	// what is left to do, and a list that included finished work would make it
	// impossible to tell how much remains.
	where = append(where, "q.resolved = ?")
	args = append(args, boolToInt(query.Resolved))

	condition := strings.Join(where, " AND ")

	var total int
	if err := r.db.Read.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM qc_results q WHERE `+condition, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("qc: count findings: %w", err)
	}

	// Ordered by severity then position, so the worst problems on the earliest
	// lines come first — which is the order a reviewer works in.
	rows, err := r.db.Read.QueryContext(ctx, `
		SELECT q.id, COALESCE(q.segment_id, ''), COALESCE(s.ordinal, 0),
		       q.stage, q.severity, q.code, q.message, q.suggestion,
		       q.resolved, q.resolved_at, q.created_at
		FROM qc_results q
		LEFT JOIN segments s ON s.id = q.segment_id
		WHERE `+condition+`
		ORDER BY CASE q.severity WHEN 'error' THEN 0 WHEN 'warning' THEN 1 ELSE 2 END,
		         COALESCE(s.ordinal, 0)
		LIMIT ? OFFSET ?`, append(args, query.Limit, query.Offset)...)
	if err != nil {
		return nil, 0, fmt.Errorf("qc: list findings: %w", err)
	}
	defer rows.Close()

	findings, err := scanFindings(rows)
	if err != nil {
		return nil, 0, err
	}
	return findings, total, nil
}

// ForSegments returns the unresolved findings for a set of lines, keyed by line.
func (r *Repository) ForSegments(ctx context.Context, projectID string, segmentIDs []string) (map[string][]StoredFinding, error) {
	out := map[string][]StoredFinding{}
	if len(segmentIDs) == 0 {
		return out, nil
	}

	// Chunked, because SQLite's bound-variable limit is 999 and a page of lines
	// can exceed it.
	const chunk = 200

	for start := 0; start < len(segmentIDs); start += chunk {
		end := min(start+chunk, len(segmentIDs))
		ids := segmentIDs[start:end]

		query := `SELECT q.id, q.segment_id, 0, q.stage, q.severity, q.code, q.message,
		                 q.suggestion, q.resolved, q.resolved_at, q.created_at
		          FROM qc_results q
		          WHERE q.project_id = ? AND q.resolved = 0
		            AND q.segment_id IN (` +
			strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",") + `)`

		args := make([]any, 0, len(ids)+1)
		args = append(args, projectID)
		for _, id := range ids {
			args = append(args, id)
		}

		rows, err := r.db.Read.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, fmt.Errorf("qc: read findings for lines: %w", err)
		}

		found, err := scanFindings(rows)
		_ = rows.Close()
		if err != nil {
			return nil, err
		}
		for _, finding := range found {
			out[finding.SegmentID] = append(out[finding.SegmentID], finding)
		}
	}

	return out, nil
}

// Summary counts the unresolved findings by severity.
func (r *Repository) Summary(ctx context.Context, projectID string) (map[Severity]int, error) {
	rows, err := r.db.Read.QueryContext(ctx,
		`SELECT severity, COUNT(*) FROM qc_results
		 WHERE project_id = ? AND resolved = 0 GROUP BY severity`, projectID)
	if err != nil {
		return nil, fmt.Errorf("qc: summarise findings: %w", err)
	}
	defer rows.Close()

	counts := map[Severity]int{}
	for rows.Next() {
		var severity string
		var count int
		if err := rows.Scan(&severity, &count); err != nil {
			return nil, fmt.Errorf("qc: summarise findings: %w", err)
		}
		counts[Severity(severity)] = count
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("qc: summarise findings: %w", err)
	}
	return counts, nil
}

// Resolve marks a finding as dealt with, or puts it back.
func (r *Repository) Resolve(ctx context.Context, projectID, id string, resolved bool) error {
	var resolvedAt any
	if resolved {
		resolvedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}

	result, err := r.db.Write.ExecContext(ctx,
		`UPDATE qc_results SET resolved = ?, resolved_at = ?
		 WHERE id = ? AND project_id = ?`,
		boolToInt(resolved), resolvedAt, id, projectID)
	if err != nil {
		return fmt.Errorf("qc: resolve %s: %w", id, err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("qc: resolve %s: %w", id, err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

func scanFindings(rows *sql.Rows) ([]StoredFinding, error) {
	findings := []StoredFinding{}
	for rows.Next() {
		var (
			finding    StoredFinding
			severity   string
			suggestion sql.NullString
			resolvedAt sql.NullString
			createdAt  string
		)

		err := rows.Scan(&finding.ID, &finding.SegmentID, &finding.SegmentOrdinal,
			&finding.Stage, &severity, &finding.Code, &finding.Message,
			&suggestion, &finding.Resolved, &resolvedAt, &createdAt)
		if err != nil {
			return nil, fmt.Errorf("qc: read finding: %w", err)
		}

		finding.Severity = Severity(severity)
		if suggestion.Valid {
			value := suggestion.String
			finding.Suggestion = &value
		}
		if resolvedAt.Valid {
			if ts, err := time.Parse(time.RFC3339Nano, resolvedAt.String); err == nil {
				finding.ResolvedAt = &ts
			}
		}
		if ts, err := time.Parse(time.RFC3339Nano, createdAt); err == nil {
			finding.CreatedAt = ts
		}

		findings = append(findings, finding)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("qc: read findings: %w", err)
	}
	return findings, nil
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
