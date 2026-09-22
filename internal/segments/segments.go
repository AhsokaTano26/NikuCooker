// Package segments persists subtitle lines so the API and the editor can reach
// them.
//
// The pipeline's output lives in artifacts, which are content-addressed and
// immutable: excellent for caching, useless as the backing store for an editor
// that has to update one line and get it back. So a finished run writes its
// lines here as well, and this table is what the UI reads.
//
// The two are not in conflict. The artifact is the record of what a run
// produced; the table is the current state of the project, which a user may have
// since edited. When they disagree, the table is right — that is the whole point
// of having it.
package segments

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/AhsokaTano26/NikuCooker/internal/artifact"
	"github.com/AhsokaTano26/NikuCooker/internal/database"
	"github.com/AhsokaTano26/NikuCooker/internal/qc"
	"github.com/AhsokaTano26/NikuCooker/internal/subtitle"
)

// ErrNotFound reports a segment that does not exist.
var ErrNotFound = errors.New("segments: not found")

// ErrInvalid reports a request a user can fix: a split point inside a word, a
// line with no timings, an interval that ends before it starts.
//
// Distinct from a database failure, which is nobody's fault and deserves a 500.
// The API maps this one to a 400 carrying the repository's message, which is
// written to be read by the person who has to act on it.
var ErrInvalid = errors.New("segments: invalid")

// ReviewState is where a line stands in the review workflow.
type ReviewState string

const (
	ReviewNone     ReviewState = "none"
	ReviewPending  ReviewState = "pending"
	ReviewApproved ReviewState = "approved"
	ReviewRejected ReviewState = "rejected"
	ReviewEdited   ReviewState = "edited"
)

// Reviewed reports whether a state is a decision a reviewer made.
//
// "none" and "pending" are the absence of a decision, and treating either as
// one would let a client mark a line approved by sending an empty string.
func (r ReviewState) Reviewed() bool {
	return r == ReviewApproved || r == ReviewRejected
}

// Record is one stored line, shaped the way the API serves it.
type Record struct {
	ID      string `json:"id"`
	Ordinal int    `json:"ordinal"`

	Start float64 `json:"start"`
	End   float64 `json:"end"`

	Speaker *string `json:"speaker"`

	SourceLanguage string `json:"source_language"`
	TargetLanguage string `json:"target_language"`

	SourceText     string  `json:"source_text"`
	TranslatedText *string `json:"translated_text"`

	Words []subtitle.WordTimestamp `json:"words"`

	// ASRConfidence is a log probability, so negative, and higher is better.
	// It is not a 0–1 score and the UI must not present it as one.
	ASRConfidence         *float64 `json:"asr_confidence"`
	TranslationConfidence *float64 `json:"translation_confidence"`

	CPS *float64 `json:"cps"`

	NeedsReview bool        `json:"needs_review"`
	ReviewState ReviewState `json:"review_state"`

	// IsEdited records that a human changed this line, which is what stops a
	// re-run from overwriting it and what the review queue filters on.
	IsEdited bool `json:"is_edited"`

	Tags     []string       `json:"tags"`
	Metadata map[string]any `json:"metadata"`

	// QC is populated only when the caller asked for it: a page of a thousand
	// lines should not carry a thousand finding lists.
	//
	// The stored finding rather than the rule's output, because a client needs
	// an identity to resolve a finding by and a rule has none to give.
	QC []qc.StoredFinding `json:"qc,omitempty"`
}

// EffectiveText is what a viewer reads.
func (r *Record) EffectiveText() string {
	if r.TranslatedText != nil {
		return *r.TranslatedText
	}
	return r.SourceText
}

// DeriveID computes a line's stable identifier.
//
// Derived from the project and the ordinal rather than generated at random, so
// that re-running a project produces the same ids. A user who has bookmarked a
// line, or a client holding an open editor, would otherwise find that every
// re-run invalidated every id — and the review decisions attached to them.
func DeriveID(projectID string, ordinal int) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s:%d", projectID, ordinal)))
	return "seg_" + hex.EncodeToString(sum[:])[:10]
}

// ---------------------------------------------------------------------------
// Repository
// ---------------------------------------------------------------------------

// Repository reads and writes project lines.
type Repository struct {
	db *database.DB
}

// NewRepository binds a repository to a database.
func NewRepository(db *database.DB) *Repository { return &Repository{db: db} }

const columns = `id, ordinal, start, end, speaker, source_language, target_language,
	source_text, translated_text, words_json, asr_confidence, translation_confidence,
	cps, needs_review, review_state, is_edited, tags_json, metadata_json`

// ReplaceFromArtifacts writes a run's output as the project's current lines.
//
// Existing rows are matched by ordinal and updated in place, so that review
// state a user has already set survives a re-run, and an edited line is never
// overwritten by the pipeline that produced it. Rows beyond the new line count
// are deleted: the transcript got shorter, and leaving orphans behind would show
// a user lines that no longer exist.
func (r *Repository) ReplaceFromArtifacts(
	ctx context.Context,
	projectID string,
	set *subtitle.Set,
	lineage Lineage,
) error {
	if set == nil {
		return nil
	}

	tx, err := r.db.Write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("segments: store lines: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	statement, err := tx.PrepareContext(ctx, `
		INSERT INTO segments (
			id, project_id, ordinal, start, end, speaker,
			source_language, target_language, source_text, translated_text,
			words_json, asr_confidence, translation_confidence, cps,
			needs_review, review_state, is_edited, tags_json, metadata_json,
			seg_artifact_id, trans_artifact_id, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'none', 0, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			ordinal = excluded.ordinal,
			start = excluded.start, end = excluded.end,
			speaker = excluded.speaker,
			source_text = excluded.source_text,
			words_json = excluded.words_json,
			asr_confidence = excluded.asr_confidence,
			cps = excluded.cps,
			needs_review = excluded.needs_review,
			tags_json = excluded.tags_json,
			metadata_json = excluded.metadata_json,
			seg_artifact_id = excluded.seg_artifact_id,
			updated_at = excluded.updated_at,
			-- A line a human edited keeps its text. The pipeline that produced
			-- the previous version has no authority over the correction, and a
			-- re-run silently reverting it is the single most destructive thing
			-- this function could do.
			translated_text = CASE WHEN segments.is_edited = 1
				THEN segments.translated_text ELSE excluded.translated_text END,
			trans_artifact_id = CASE WHEN segments.is_edited = 1
				THEN segments.trans_artifact_id ELSE excluded.trans_artifact_id END`)
	if err != nil {
		return fmt.Errorf("segments: prepare: %w", err)
	}
	defer func() { _ = statement.Close() }()

	now := time.Now().UTC().Format(time.RFC3339Nano)

	for i, segment := range set.Segments {
		ordinal := i + 1
		id := DeriveID(projectID, ordinal)

		words, err := json.Marshal(segment.Words)
		if err != nil {
			return fmt.Errorf("segments: encode words of %s: %w", segment.ID, err)
		}
		tags, err := json.Marshal(orEmpty(segment.Tags))
		if err != nil {
			return fmt.Errorf("segments: encode tags of %s: %w", segment.ID, err)
		}
		metadata, err := json.Marshal(orEmptyMap(segment.Metadata))
		if err != nil {
			return fmt.Errorf("segments: encode metadata of %s: %w", segment.ID, err)
		}

		if _, err := statement.ExecContext(ctx,
			id, projectID, ordinal, segment.Start, segment.End, segment.Speaker,
			segment.SourceLanguage, segment.TargetLanguage,
			segment.SourceText, segment.TranslatedText,
			string(words), segment.ASRConfidence, segment.TranslationConfidence, segment.CPS,
			boolToInt(segment.NeedsReview), string(tags), string(metadata),
			nullable(lineage.SegmentationArtifactID), nullable(lineage.TranslationArtifactID),
			now, now,
		); err != nil {
			// The schema's CHECK constraints reject an inverted or negative
			// interval, so a segmentation bug surfaces here rather than as a
			// broken subtitle file a user discovers on playback.
			return fmt.Errorf("segments: store line %d: %w", ordinal, err)
		}
	}

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM segments WHERE project_id = ? AND ordinal > ?`,
		projectID, len(set.Segments)); err != nil {
		return fmt.Errorf("segments: remove stale lines: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("segments: store lines: %w", err)
	}
	return nil
}

// Lineage records which artifacts a set of lines came from.
type Lineage struct {
	SegmentationArtifactID string
	TranslationArtifactID  string
}

// SaveFindings replaces the unresolved findings a stage reported.
//
// Resolved findings are kept. A user who worked through a warning should not
// find it back on the list — and re-raising a problem someone has already judged
// is how a review queue trains people to ignore it.
func (r *Repository) SaveFindings(ctx context.Context, projectID, stage string, findings []qc.Finding, ordinalToID map[string]string) error {
	tx, err := r.db.Write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("segments: store findings: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM qc_results WHERE project_id = ? AND stage = ? AND resolved = 0`,
		projectID, stage); err != nil {
		return fmt.Errorf("segments: clear old findings: %w", err)
	}

	statement, err := tx.PrepareContext(ctx, `
		INSERT INTO qc_results (
			id, project_id, segment_id, stage, severity, code, message, suggestion,
			resolved, artifact_id, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0, NULL, ?)`)
	if err != nil {
		return fmt.Errorf("segments: prepare findings: %w", err)
	}
	defer func() { _ = statement.Close() }()

	now := time.Now().UTC().Format(time.RFC3339Nano)

	for i, finding := range findings {
		// The finding names the line by the id the pipeline used. That id is
		// positional and belongs to the artifact; the table's id is derived.
		// Translating here rather than storing the artifact id keeps the two
		// identities from leaking into each other.
		var segmentID any
		if finding.SegmentID != "" {
			mapped, ok := ordinalToID[finding.SegmentID]
			if !ok {
				// A finding about a line that no longer exists — the transcript
				// changed between the check and the write. Dropping it is right:
				// it describes nothing the user can act on.
				continue
			}
			segmentID = mapped
		}

		id := fmt.Sprintf("qcr_%d_%d", time.Now().UnixNano(), i)
		if _, err := statement.ExecContext(ctx,
			id, projectID, segmentID, finding.Stage, string(finding.Severity),
			finding.Code, finding.Message, nullable(finding.Suggestion), now,
		); err != nil {
			return fmt.Errorf("segments: store finding %s: %w", finding.Code, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("segments: store findings: %w", err)
	}
	return nil
}

// OrdinalMap builds the artifact-id to table-id mapping a run needs.
func OrdinalMap(projectID string, set *subtitle.Set) map[string]string {
	if set == nil {
		return nil
	}
	out := make(map[string]string, len(set.Segments))
	for i, segment := range set.Segments {
		out[segment.ID] = DeriveID(projectID, i+1)
	}
	return out
}

// ---------------------------------------------------------------------------
// Reading
// ---------------------------------------------------------------------------

// ListQuery filters a page of lines.
type ListQuery struct {
	Limit  int
	Offset int

	// NeedsReview restricts to lines flagged for a human.
	NeedsReview bool

	// Search matches the source or translated text.
	Search string

	// Order is "ordinal", "start", "cps" or "duration".
	Order string
	Desc  bool
}

// List returns a page of lines and the total matching count.
func (r *Repository) List(ctx context.Context, projectID string, query ListQuery) ([]Record, int, error) {
	if query.Limit <= 0 || query.Limit > 500 {
		query.Limit = 100
	}

	where := []string{"project_id = ?"}
	args := []any{projectID}

	if query.NeedsReview {
		where = append(where, "needs_review = 1")
	}
	if query.Search != "" {
		where = append(where,
			"(source_text LIKE ? OR COALESCE(translated_text, '') LIKE ?)")
		pattern := "%" + query.Search + "%"
		args = append(args, pattern, pattern)
	}

	condition := strings.Join(where, " AND ")

	var total int
	if err := r.db.Read.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM segments WHERE `+condition, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("segments: count: %w", err)
	}

	// A fixed set rather than interpolating the caller's string: the column
	// name goes into the SQL, and a whitelist is the only safe way to do that.
	order := "ordinal"
	switch query.Order {
	case "start":
		order = "start"
	case "cps":
		order = "COALESCE(cps, 0)"
	case "duration":
		order = "(end - start)"
	case "ordinal", "":
	}
	if query.Desc {
		order += " DESC"
	}

	rows, err := r.db.Read.QueryContext(ctx,
		`SELECT `+columns+` FROM segments WHERE `+condition+
			` ORDER BY `+order+` LIMIT ? OFFSET ?`,
		append(args, query.Limit, query.Offset)...)
	if err != nil {
		return nil, 0, fmt.Errorf("segments: list: %w", err)
	}
	defer rows.Close()

	records, err := scanRecords(rows)
	if err != nil {
		return nil, 0, err
	}
	return records, total, nil
}

// Get returns one line.
func (r *Repository) Get(ctx context.Context, projectID, id string) (*Record, error) {
	row := r.db.Read.QueryRowContext(ctx,
		`SELECT `+columns+` FROM segments WHERE project_id = ? AND id = ?`, projectID, id)

	record, err := scanRecord(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return record, err
}

// Counts reports the totals a dashboard shows.
func (r *Repository) Counts(ctx context.Context, projectID string) (total, needsReview int, err error) {
	err = r.db.Read.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(needs_review), 0) FROM segments WHERE project_id = ?`,
		projectID).Scan(&total, &needsReview)
	if err != nil {
		return 0, 0, fmt.Errorf("segments: count for %s: %w", projectID, err)
	}
	return total, needsReview, nil
}

// ---------------------------------------------------------------------------
// Scanning
// ---------------------------------------------------------------------------

type scanner interface {
	Scan(dest ...any) error
}

func scanRecords(rows *sql.Rows) ([]Record, error) {
	records := []Record{}
	for rows.Next() {
		record, err := scanRecord(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, *record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("segments: read: %w", err)
	}
	return records, nil
}

func scanRecord(row scanner) (*Record, error) {
	var (
		record          Record
		speaker         sql.NullString
		translated      sql.NullString
		wordsJSON       string
		asrConfidence   sql.NullFloat64
		transConfidence sql.NullFloat64
		cps             sql.NullFloat64
		needsReview     int
		reviewState     string
		isEdited        int
		tagsJSON        string
		metadataJSON    string
	)

	err := row.Scan(
		&record.ID, &record.Ordinal, &record.Start, &record.End, &speaker,
		&record.SourceLanguage, &record.TargetLanguage,
		&record.SourceText, &translated, &wordsJSON,
		&asrConfidence, &transConfidence, &cps,
		&needsReview, &reviewState, &isEdited, &tagsJSON, &metadataJSON,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		return nil, fmt.Errorf("segments: read: %w", err)
	}

	if speaker.Valid {
		value := speaker.String
		record.Speaker = &value
	}
	if translated.Valid {
		value := translated.String
		record.TranslatedText = &value
	}
	if asrConfidence.Valid {
		value := asrConfidence.Float64
		record.ASRConfidence = &value
	}
	if transConfidence.Valid {
		value := transConfidence.Float64
		record.TranslationConfidence = &value
	}
	if cps.Valid {
		value := cps.Float64
		record.CPS = &value
	}

	record.NeedsReview = needsReview != 0
	record.ReviewState = ReviewState(reviewState)
	record.IsEdited = isEdited != 0

	record.Words = []subtitle.WordTimestamp{}
	if wordsJSON != "" {
		// A malformed row yields no words rather than an error. The timings are
		// diagnostic; refusing to show the line because its word list is
		// unreadable would be the wrong trade.
		_ = json.Unmarshal([]byte(wordsJSON), &record.Words)
	}

	record.Tags = []string{}
	if tagsJSON != "" {
		_ = json.Unmarshal([]byte(tagsJSON), &record.Tags)
	}

	if metadataJSON != "" && metadataJSON != "{}" {
		_ = json.Unmarshal([]byte(metadataJSON), &record.Metadata)
	}

	return &record, nil
}

func nullable(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func orEmpty(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

func orEmptyMap(values map[string]any) map[string]any {
	if values == nil {
		return map[string]any{}
	}
	return values
}

// ArtifactIDOf reports the artifacts a project's lines came from, for the
// provenance the API exposes.
func (r *Repository) ArtifactIDOf(ctx context.Context, projectID string) (Lineage, error) {
	var seg, trans sql.NullString
	err := r.db.Read.QueryRowContext(ctx, `
		SELECT
			(SELECT seg_artifact_id FROM segments WHERE project_id = ? AND seg_artifact_id IS NOT NULL LIMIT 1),
			(SELECT trans_artifact_id FROM segments WHERE project_id = ? AND trans_artifact_id IS NOT NULL LIMIT 1)`,
		projectID, projectID).Scan(&seg, &trans)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Lineage{}, fmt.Errorf("segments: read lineage: %w", err)
	}
	return Lineage{
		SegmentationArtifactID: seg.String,
		TranslationArtifactID:  trans.String,
	}, nil
}

// DurationOf reports the project's media length, taken from its last line.
//
// Read from the lines rather than from the probe artifact: the artifact holds
// the accurate figure, but decoding a file to display a number on a project list
// is a file read per project per request. The end of the last subtitle is the
// same answer for anything that has been through segmentation, and zero means
// "not known yet" — which the API renders as null rather than as a length of
// nothing.
func (r *Repository) DurationOf(ctx context.Context, projectID string) (float64, error) {
	var duration sql.NullFloat64
	err := r.db.Read.QueryRowContext(ctx,
		`SELECT MAX(end) FROM segments WHERE project_id = ?`, projectID).Scan(&duration)
	if err != nil {
		return 0, fmt.Errorf("segments: read duration: %w", err)
	}
	return duration.Float64, nil
}

// CurrentLines returns the project's lines as a subtitle set.
//
// The table is the project's current state, which is what an editor changes and
// what a render should use. The artifact is the record of what a particular run
// produced, which is a different question — and the one that would silently drop
// a user's corrections if the output stages read it instead.
func (r *Repository) CurrentLines(ctx context.Context, projectID string) (*subtitle.Set, error) {
	records, _, err := r.List(ctx, projectID, ListQuery{Limit: 500})
	if err != nil {
		return nil, err
	}
	if len(records) == 0 {
		return nil, nil
	}

	// Paged, because the repository caps a page at 500 and a feature-length
	// work is several thousand lines. Reading only the first page would render
	// a truncated film.
	for offset := 500; ; offset += 500 {
		page, total, err := r.List(ctx, projectID, ListQuery{Limit: 500, Offset: offset})
		if err != nil {
			return nil, err
		}
		if len(page) == 0 || len(records) >= total {
			break
		}
		records = append(records, page...)
	}

	set := &subtitle.Set{
		SourceLanguage: records[0].SourceLanguage,
		TargetLanguage: records[0].TargetLanguage,
		Segments:       make([]*subtitle.Segment, 0, len(records)),
	}

	for _, record := range records {
		segment := &subtitle.Segment{
			ID:                    record.ID,
			Start:                 record.Start,
			End:                   record.End,
			Speaker:               record.Speaker,
			SourceLanguage:        record.SourceLanguage,
			TargetLanguage:        record.TargetLanguage,
			SourceText:            record.SourceText,
			TranslatedText:        record.TranslatedText,
			Words:                 record.Words,
			ASRConfidence:         record.ASRConfidence,
			TranslationConfidence: record.TranslationConfidence,
			CPS:                   record.CPS,
			NeedsReview:           record.NeedsReview,
			Tags:                  record.Tags,
			Metadata:              record.Metadata,
		}
		if segment.Tags == nil {
			segment.Tags = []string{}
		}
		set.Segments = append(set.Segments, segment)
	}

	return set, nil
}

// LinesHash digests the project's current lines.
//
// It is what makes an edit invalidate the stages that produce output from them.
// Without it, changing a line would leave the subtitle and render artifacts
// describing the previous text, and a re-run would serve them from cache — a
// video with the correction missing and nothing to say why.
//
// Only what a render depends on is hashed. A review decision or a resolved
// finding does not change a single character of the output, and folding those in
// would re-render a film because someone ticked a checkbox.
func (r *Repository) LinesHash(ctx context.Context, projectID string) (string, error) {
	records, _, err := r.List(ctx, projectID, ListQuery{Limit: 500})
	if err != nil {
		return "", err
	}
	if len(records) == 0 {
		return "", nil
	}

	for offset := 500; ; offset += 500 {
		page, total, err := r.List(ctx, projectID, ListQuery{Limit: 500, Offset: offset})
		if err != nil {
			return "", err
		}
		if len(page) == 0 || len(records) >= total {
			break
		}
		records = append(records, page...)
	}

	var b strings.Builder
	for _, record := range records {
		text := record.SourceText
		if record.TranslatedText != nil {
			text = *record.TranslatedText
		}
		fmt.Fprintf(&b, "%d\x1f%.3f\x1f%.3f\x1f%s\x1e", record.Ordinal, record.Start, record.End, text)
	}

	digest, err := artifact.FingerprintBytes("lines", b.String())
	if err != nil {
		return "", err
	}
	return digest, nil
}
