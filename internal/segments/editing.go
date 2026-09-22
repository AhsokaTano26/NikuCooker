package segments

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/AhsokaTano26/NikuCooker/internal/subtitle"
)

// Edit is a change to one line. A nil field is left alone.
//
// Pointer fields rather than values, because "set the speaker to empty" and
// "leave the speaker as it was" are different requests and a plain string
// cannot express both.
type Edit struct {
	TranslatedText *string
	Start          *float64
	End            *float64
	Speaker        *string
	Tags           *[]string
	ReviewState    *ReviewState
	NeedsReview    *bool
}

// Apply records an edit to a line.
//
// Every change made through here marks the line as edited, which is what stops
// a re-run from overwriting it. That is the whole contract of this method: a
// human's decision outranks the pipeline's output, permanently, until they
// change it again.
func (r *Repository) Apply(ctx context.Context, projectID, id string, edit Edit) (*Record, error) {
	record, err := r.Get(ctx, projectID, id)
	if err != nil {
		return nil, err
	}

	applyEdit(record, edit)
	// Reflected on the returned record as well as in the statement below. The
	// row was read before the update, so without this the caller is handed a
	// line claiming not to have been edited — which is exactly what the client
	// stores, and what makes its copy differ from the server's.
	record.IsEdited = true

	// Re-derived rather than trusted: a change to the text or the timing
	// changes the reading speed, and a stale figure would tell a reviewer the
	// line is fine when it is not.
	segment := record.ToSegment()
	segment.RecomputeCPS()
	record.CPS = segment.CPS

	if err := record.Validate(); err != nil {
		return nil, err
	}

	words, err := json.Marshal(record.Words)
	if err != nil {
		return nil, fmt.Errorf("segments: encode words: %w", err)
	}
	tags, err := json.Marshal(orEmpty(record.Tags))
	if err != nil {
		return nil, fmt.Errorf("segments: encode tags: %w", err)
	}

	_, err = r.db.Write.ExecContext(ctx, `
		UPDATE segments SET
			translated_text = ?, start = ?, end = ?, speaker = ?, tags_json = ?,
			words_json = ?, cps = ?, needs_review = ?, review_state = ?,
			is_edited = 1, updated_at = ?
		WHERE project_id = ? AND id = ?`,
		record.TranslatedText, record.Start, record.End, record.Speaker,
		string(tags), string(words), record.CPS,
		boolToInt(record.NeedsReview), string(record.ReviewState),
		time.Now().UTC().Format(time.RFC3339Nano), projectID, id)
	if err != nil {
		return nil, fmt.Errorf("segments: save %s: %w", id, err)
	}

	return record, nil
}

// applyEdit folds an edit into a record.
func applyEdit(record *Record, edit Edit) {
	if edit.TranslatedText != nil {
		value := *edit.TranslatedText
		record.TranslatedText = &value
	}
	if edit.Start != nil {
		record.Start = *edit.Start
	}
	if edit.End != nil {
		record.End = *edit.End
	}
	if edit.Speaker != nil {
		value := *edit.Speaker
		if value == "" {
			record.Speaker = nil
		} else {
			record.Speaker = &value
		}
	}
	if edit.Tags != nil {
		record.Tags = *edit.Tags
	}
	if edit.ReviewState != nil {
		record.ReviewState = *edit.ReviewState
	}
	if edit.NeedsReview != nil {
		record.NeedsReview = *edit.NeedsReview
	}
}

// SetReviewState records a review decision without marking the line edited.
//
// Approving a line is not changing it. Conflating the two would mean a reviewer
// ticking through a queue marked every line as hand-edited, and a re-run would
// then refuse to improve any of them.
func (r *Repository) SetReviewState(ctx context.Context, projectID, id string, state ReviewState, needsReview bool) (*Record, error) {
	if !validReviewState(state) {
		return nil, fmt.Errorf("%w: %q is not a review state", ErrInvalid, state)
	}

	result, err := r.db.Write.ExecContext(ctx, `
		UPDATE segments SET review_state = ?, needs_review = ?, updated_at = ?
		WHERE project_id = ? AND id = ?`,
		string(state), boolToInt(needsReview),
		time.Now().UTC().Format(time.RFC3339Nano), projectID, id)
	if err != nil {
		return nil, fmt.Errorf("segments: set the review state of %s: %w", id, err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("segments: set the review state of %s: %w", id, err)
	}
	if affected == 0 {
		return nil, ErrNotFound
	}

	return r.Get(ctx, projectID, id)
}

func validReviewState(state ReviewState) bool {
	switch state {
	case ReviewNone, ReviewPending, ReviewApproved, ReviewRejected, ReviewEdited:
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// Split and merge
// ---------------------------------------------------------------------------

// Split divides a line at a time.
//
// The split point must fall between two words. Dividing text that has no word
// timings would mean guessing which characters belong to which half, and a
// wrong guess produces two lines whose text and timing disagree — which is worse
// than refusing and telling the user to fix the timing instead.
func (r *Repository) Split(ctx context.Context, projectID, id string, at float64) (*Record, *Record, error) {
	record, err := r.Get(ctx, projectID, id)
	if err != nil {
		return nil, nil, err
	}

	if at <= record.Start || at >= record.End {
		return nil, nil, fmt.Errorf("%w: %.3fs is not inside the line [%.3f, %.3f]",
			ErrInvalid, at, record.Start, record.End)
	}
	if len(record.Words) < 2 {
		return nil, nil, fmt.Errorf("%w: this line has no word timings, so there is no way "+
			"to tell which text belongs to which half; re-run recognition with word timestamps enabled",
			ErrInvalid)
	}

	// The first word that starts at or after the cut belongs to the second half.
	cut := -1
	for i, word := range record.Words {
		if word.Start >= at {
			cut = i
			break
		}
	}
	if cut <= 0 || cut >= len(record.Words) {
		return nil, nil, fmt.Errorf("%w: %.3fs does not fall between two words", ErrInvalid, at)
	}

	head := *record
	head.End = record.Words[cut].Start
	head.Words = append([]subtitle.WordTimestamp(nil), record.Words[:cut]...)
	head.SourceText = subtitle.TextOf(head.Words)
	head.TranslatedText = nil
	// Both halves need a fresh translation, and neither is a user's edit yet.
	head.IsEdited = false

	tail := *record
	tail.Start = record.Words[cut].Start
	tail.Words = append([]subtitle.WordTimestamp(nil), record.Words[cut:]...)
	tail.SourceText = subtitle.TextOf(tail.Words)
	tail.TranslatedText = nil
	tail.IsEdited = false

	// The translation is dropped rather than divided, because there is no
	// honest way to divide it: the Chinese for a phrase does not split where
	// the Japanese word boundary is. Both halves are marked for review so the
	// gap is visible rather than silent.
	head.NeedsReview = true
	tail.NeedsReview = true
	head.AddTag(subtitle.TagUntranslated)
	tail.AddTag(subtitle.TagUntranslated)

	for _, derived := range []*Record{&head, &tail} {
		segment := derived.ToSegment()
		segment.RecomputeCPS()
		derived.CPS = segment.CPS
		if err := derived.Validate(); err != nil {
			return nil, nil, err
		}
	}

	// Rewritten in one transaction, and the ordinals after this line shift by
	// one. Everything downstream is keyed by ordinal, so a partial success here
	// would leave the project's lines describing a transcript that never
	// existed.
	if err := r.rewriteFrom(ctx, projectID, record.Ordinal, []Record{head, tail}); err != nil {
		return nil, nil, err
	}

	created, err := r.byOrdinal(ctx, projectID, record.Ordinal)
	if err != nil {
		return nil, nil, err
	}
	second, err := r.byOrdinal(ctx, projectID, record.Ordinal+1)
	if err != nil {
		return nil, nil, err
	}
	return created, second, nil
}

// Merge combines a line with the one after it.
func (r *Repository) Merge(ctx context.Context, projectID, id string, withNext bool) (*Record, *Record, error) {
	// The API takes "merge with the next line"; merging with the previous one
	// is the same operation seen from the other side, so it is expressed by
	// moving to that line first rather than by a second code path that would
	// have to get the same edge cases right.
	if !withNext {
		previous, err := r.relative(ctx, projectID, id, -1)
		if err != nil {
			return nil, nil, err
		}
		return r.mergePair(ctx, projectID, previous.ID, id)
	}
	return r.mergePair(ctx, projectID, id, "")
}

// mergePair merges two adjacent lines.
func (r *Repository) mergePair(ctx context.Context, projectID, firstID, secondID string) (*Record, *Record, error) {
	first, err := r.Get(ctx, projectID, firstID)
	if err != nil {
		return nil, nil, err
	}

	var second *Record
	if secondID == "" {
		second, err = r.next(ctx, projectID, first.Ordinal)
	} else {
		second, err = r.Get(ctx, projectID, secondID)
	}
	if err != nil {
		return nil, nil, err
	}

	merged := *first
	merged.End = second.End

	// Word timings concatenate, which is what makes the result splittable
	// again. A line merged without them could never be split back.
	merged.Words = append(append([]subtitle.WordTimestamp(nil), first.Words...), second.Words...)
	merged.SourceText = subtitle.TextOf(merged.Words)
	if merged.SourceText == "" {
		// Neither half had timings, so the text is joined directly. Not
		// ideal, but better than losing one of the two.
		merged.SourceText = strings.TrimSpace(first.SourceText + second.SourceText)
	}

	merged.TranslatedText = joinTranslations(first.TranslatedText, second.TranslatedText)
	merged.NeedsReview = first.NeedsReview || second.NeedsReview
	merged.Tags = unionTags(first.Tags, second.Tags)
	merged.IsEdited = true

	segment := merged.ToSegment()
	segment.RecomputeCPS()
	merged.CPS = segment.CPS

	if err := merged.Validate(); err != nil {
		return nil, nil, err
	}

	// The merged line replaces the first and the second is removed, so the
	// ordinals after it shift back by one.
	if err := r.replaceRange(ctx, projectID, first.Ordinal, []Record{merged}); err != nil {
		return nil, nil, err
	}

	result, err := r.byOrdinal(ctx, projectID, first.Ordinal)
	if err != nil {
		return nil, nil, err
	}
	return result, second, nil
}

// joinTranslations combines two translations into one line.
//
// A space only between two Latin texts, because Japanese and Chinese do not
// separate words and inserting one produces a line with a visible gap in the
// middle of a sentence.
func joinTranslations(first, second *string) *string {
	if first == nil && second == nil {
		return nil
	}
	if first == nil {
		return second
	}
	if second == nil {
		return first
	}

	left := strings.TrimSpace(*first)
	right := strings.TrimSpace(*second)

	joined := left + right
	if needsSeparator(left, right) {
		joined = left + " " + right
	}
	return &joined
}

func needsSeparator(left, right string) bool {
	if left == "" || right == "" {
		return false
	}
	return subtitle.UsesSpaces(lastRune(left)) && subtitle.UsesSpaces(firstRune(right))
}

func unionTags(first, second []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(first)+len(second))

	for _, tags := range [][]string{first, second} {
		for _, tag := range tags {
			if !seen[tag] {
				seen[tag] = true
				out = append(out, tag)
			}
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Splicing
// ---------------------------------------------------------------------------

// rewriteFrom replaces one line at an ordinal with a list.
func (r *Repository) rewriteFrom(ctx context.Context, projectID string, ordinal int, replacement []Record) error {
	return r.splice(ctx, projectID, ordinal, 1, replacement)
}

// replaceRange replaces two lines at an ordinal with one.
func (r *Repository) replaceRange(ctx context.Context, projectID string, ordinal int, replacement []Record) error {
	return r.splice(ctx, projectID, ordinal, 2, replacement)
}

// splice replaces everything from an ordinal onward.
//
// Every line after an insertion or a removal shifts, and a line's identity is
// derived from its ordinal — so the tail is renumbered rather than moved. That
// is inherent to inserting a line in the middle of a transcript, and doing it
// any other way would leave two lines claiming the same ordinal at some point
// during the transaction.
//
// The whole thing runs in one transaction, because a partial splice leaves the
// project describing a transcript that never existed.
func (r *Repository) splice(ctx context.Context, projectID string, ordinal, replaced int, replacement []Record) error {
	tx, err := r.db.Write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("segments: splice lines: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// The tail is read first, with its content intact, so that an edited line
	// keeps its correction when it shifts. It starts after the lines being
	// replaced — not after the ones replacing them, which is a different
	// position whenever the count changes.
	tail, err := r.tailFrom(ctx, tx, projectID, ordinal+replaced)
	if err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM segments WHERE project_id = ? AND ordinal >= ?`, projectID, ordinal); err != nil {
		return fmt.Errorf("segments: splice lines: %w", err)
	}

	statement, err := tx.PrepareContext(ctx, insertSQL)
	if err != nil {
		return fmt.Errorf("segments: prepare splice: %w", err)
	}
	defer func() { _ = statement.Close() }()

	now := time.Now().UTC().Format(time.RFC3339Nano)

	next := ordinal
	for _, record := range append(append([]Record(nil), replacement...), tail...) {
		if err := insertRecord(ctx, statement, projectID, next, record, now); err != nil {
			return err
		}
		next++
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("segments: splice lines: %w", err)
	}
	return nil
}

// tailFrom reads the lines at and after an ordinal.
func (r *Repository) tailFrom(ctx context.Context, tx *sql.Tx, projectID string, ordinal int) ([]Record, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT `+columns+` FROM segments
		 WHERE project_id = ? AND ordinal >= ? ORDER BY ordinal`, projectID, ordinal)
	if err != nil {
		return nil, fmt.Errorf("segments: read the tail: %w", err)
	}
	defer rows.Close()

	records := []Record{}
	for rows.Next() {
		record, err := scanRecord(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, *record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("segments: read the tail: %w", err)
	}
	return records, nil
}

// insertSQL is shared between the initial write and every splice, so the two
// cannot drift into storing different things.
const insertSQL = `
	INSERT INTO segments (
		id, project_id, ordinal, start, end, speaker,
		source_language, target_language, source_text, translated_text,
		words_json, asr_confidence, translation_confidence, cps,
		needs_review, review_state, is_edited, tags_json, metadata_json,
		seg_artifact_id, trans_artifact_id, created_at, updated_at)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, NULL, ?, ?)`

// insertRecord writes one line at an ordinal, preserving its review state.
func insertRecord(ctx context.Context, statement *sql.Stmt, projectID string, ordinal int, record Record, now string) error {
	words, err := json.Marshal(record.Words)
	if err != nil {
		return fmt.Errorf("segments: encode words: %w", err)
	}
	tags, err := json.Marshal(orEmpty(record.Tags))
	if err != nil {
		return fmt.Errorf("segments: encode tags: %w", err)
	}
	metadata, err := json.Marshal(orEmptyMap(record.Metadata))
	if err != nil {
		return fmt.Errorf("segments: encode metadata: %w", err)
	}

	reviewState := record.ReviewState
	if reviewState == "" {
		reviewState = ReviewNone
	}

	if _, err := statement.ExecContext(ctx,
		DeriveID(projectID, ordinal), projectID, ordinal, record.Start, record.End, record.Speaker,
		record.SourceLanguage, record.TargetLanguage, record.SourceText, record.TranslatedText,
		string(words), record.ASRConfidence, record.TranslationConfidence, record.CPS,
		boolToInt(record.NeedsReview), string(reviewState), boolToInt(record.IsEdited),
		string(tags), string(metadata), now, now,
	); err != nil {
		return fmt.Errorf("segments: insert line %d: %w", ordinal, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Lookups
// ---------------------------------------------------------------------------

// byOrdinal returns the line at an ordinal.
func (r *Repository) byOrdinal(ctx context.Context, projectID string, ordinal int) (*Record, error) {
	row := r.db.Read.QueryRowContext(ctx,
		`SELECT `+columns+` FROM segments WHERE project_id = ? AND ordinal = ?`,
		projectID, ordinal)

	record, err := scanRecord(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return record, err
}

// next returns the line after an ordinal.
func (r *Repository) next(ctx context.Context, projectID string, ordinal int) (*Record, error) {
	record, err := r.byOrdinal(ctx, projectID, ordinal+1)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, fmt.Errorf("%w: there is no line after this one to merge with", ErrInvalid)
		}
		return nil, err
	}
	return record, nil
}

// relative returns the line an offset away from a given line.
func (r *Repository) relative(ctx context.Context, projectID, id string, offset int) (*Record, error) {
	record, err := r.Get(ctx, projectID, id)
	if err != nil {
		return nil, err
	}

	neighbour, err := r.byOrdinal(ctx, projectID, record.Ordinal+offset)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, fmt.Errorf("%w: %s has no line %d away from it", ErrInvalid, id, offset)
		}
		return nil, err
	}
	return neighbour, nil
}

func lastRune(s string) rune {
	runes := []rune(s)
	if len(runes) == 0 {
		return 0
	}
	return runes[len(runes)-1]
}

func firstRune(s string) rune {
	runes := []rune(s)
	if len(runes) == 0 {
		return 0
	}
	return runes[0]
}

// ---------------------------------------------------------------------------
// Conversion to the shared model
// ---------------------------------------------------------------------------

// toSegment converts a record into the shared subtitle model.
//
// The conversion exists so that the timing and readability rules live in one
// place. Reimplementing "is this line too fast to read" here would be a second
// answer to a question the subtitle package already answers.
func (r *Record) ToSegment() *subtitle.Segment {
	return &subtitle.Segment{
		ID:                    r.ID,
		Start:                 r.Start,
		End:                   r.End,
		Speaker:               r.Speaker,
		SourceLanguage:        r.SourceLanguage,
		TargetLanguage:        r.TargetLanguage,
		SourceText:            r.SourceText,
		TranslatedText:        r.TranslatedText,
		Words:                 r.Words,
		ASRConfidence:         r.ASRConfidence,
		TranslationConfidence: r.TranslationConfidence,
		CPS:                   r.CPS,
		NeedsReview:           r.NeedsReview,
		Tags:                  r.Tags,
		Metadata:              r.Metadata,
	}
}

// Validate checks the invariants the rest of the system relies on.
func (r *Record) Validate() error {
	if r.Start < 0 {
		return fmt.Errorf("%w: a line cannot start before zero", ErrInvalid)
	}
	if r.End <= r.Start {
		return fmt.Errorf("%w: a line must end after it starts, got [%.3f, %.3f]",
			ErrInvalid, r.Start, r.End)
	}
	if strings.TrimSpace(r.SourceText) == "" && r.TranslatedText == nil {
		return fmt.Errorf("%w: a line cannot be empty", ErrInvalid)
	}
	// Word timings must nest inside their line. A word outside it becomes an
	// impossible subtitle boundary later, where the cause is invisible.
	previousEnd := r.Start
	for i, word := range r.Words {
		if word.End < word.Start {
			return fmt.Errorf("%w: word %d has an inverted duration", ErrInvalid, i)
		}
		if word.Start < r.Start-0.05 || word.End > r.End+0.05 {
			return fmt.Errorf("%w: word %d [%.3f, %.3f] falls outside [%.3f, %.3f]",
				ErrInvalid, i, word.Start, word.End, r.Start, r.End)
		}
		if word.Start < previousEnd-0.05 {
			return fmt.Errorf("%w: word %d overlaps its predecessor", ErrInvalid, i)
		}
		previousEnd = word.End
	}
	return nil
}

// AddTag records a tag once.
func (r *Record) AddTag(tag string) {
	for _, existing := range r.Tags {
		if existing == tag {
			return
		}
	}
	r.Tags = append(r.Tags, tag)
}

// RemoveTag clears a tag.
func (r *Record) RemoveTag(tag string) {
	kept := r.Tags[:0]
	for _, existing := range r.Tags {
		if existing != tag {
			kept = append(kept, existing)
		}
	}
	r.Tags = kept
}
