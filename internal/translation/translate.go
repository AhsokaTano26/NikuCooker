// Package translation turns subtitle lines into another language.
//
// The work divides into three parts that are worth keeping apart. Deciding what
// to send — which lines need translating at all, and which terms apply to them —
// is bookkeeping and is testable without a model. Asking the model and reading
// its answer is I/O, and is where nearly all the failure modes live. Recording
// the result is storage.
//
// The design assumption running through all of it is that a model is an
// unreliable narrator of its own output. It returns JSON wrapped in prose,
// drops a line from a batch, translates a context line it was told to ignore,
// and truncates a long reply mid-string. None of those is a reason to throw away
// a response that is 95% correct, and none of them is a reason to accept output
// that is silently wrong — so the reply is validated against the request, the
// recoverable part is kept, and only what is genuinely missing is asked for
// again.
package translation

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/AhsokaTano26/NikuCooker/internal/glossary"
	"github.com/AhsokaTano26/NikuCooker/internal/llmjson"
	"github.com/AhsokaTano26/NikuCooker/internal/provider"
	"github.com/AhsokaTano26/NikuCooker/internal/subtitle"
	"github.com/AhsokaTano26/NikuCooker/prompts"
)

// ErrBudgetExceeded reports that a job hit its token ceiling.
//
// It is an error rather than a silent stop because a partially translated
// project that reports success is worse than one that reports failure: the user
// would render it and only then notice the second half is in Japanese.
var ErrBudgetExceeded = errors.New("translation: the job reached its token limit")

// Options configures a translation run.
type Options struct {
	SourceLanguage string
	TargetLanguage string

	// Style is "literal", "natural" or "fansub", and selects the guidance that
	// goes into the prompt.
	Style string

	// PromptName names the template to use, e.g. "translation/ja_zh_v1".
	PromptName string

	// BatchSize is how many lines travel in one request.
	//
	// It is the most consequential setting here. One line per request costs an
	// order of magnitude more tokens and deprives the model of the cross-line
	// context that makes dialogue read as a conversation rather than as a list
	// of sentences.
	BatchSize int

	// ContextLines is how many neighbouring lines accompany each batch without
	// being translated.
	ContextLines int

	// Concurrency bounds simultaneous requests, to stay under a provider's rate
	// limit.
	Concurrency int

	Temperature float64

	// MaxTokensPerJob stops a run that is spending more than expected. Zero
	// means no ceiling.
	MaxTokensPerJob int

	// MaxAttempts is how many times one batch may be asked for, including the
	// first attempt.
	MaxAttempts int

	// Revise reprocesses lines that already carry a translation, replacing it.
	//
	// Off by default, because the ordinary run must not overwrite a user's
	// edit. It is what the polish pass turns on: its input is the existing
	// translation, and its output supersedes it.
	Revise bool

	// ContextDocument is the analysis of the work, produced by an earlier stage.
	// Empty means no context is available and the prompt says so.
	ContextDocument string

	// Neighbourhood supplies the lines around a single-line request.
	//
	// A batch carries its own neighbours, because its lines are adjacent in the
	// slice. A one-line request has none by construction, and translating a
	// line in isolation is exactly the case where the surrounding dialogue
	// matters most — it is usually a conversation being fixed, not a sentence.
	Neighbourhood *Neighbourhood

	// Glossary is every entry in scope. Only those occurring in the lines being
	// translated are sent.
	Glossary []glossary.Entry

	// Progress is called as batches finish. It may be nil.
	Progress func(done, total int)
}

// withDefaults fills in the values a caller may reasonably omit.
func (o Options) withDefaults() Options {
	if o.BatchSize <= 0 {
		o.BatchSize = 20
	}
	if o.BatchSize > maxBatchSize {
		o.BatchSize = maxBatchSize
	}
	if o.ContextLines < 0 {
		o.ContextLines = 0
	}
	if o.Concurrency <= 0 {
		o.Concurrency = 1
	}
	if o.MaxAttempts <= 0 {
		o.MaxAttempts = 3
	}
	if o.PromptName == "" {
		o.PromptName = DefaultPromptName
	}
	return o
}

// Neighbourhood is the text around a line being translated on its own.
type Neighbourhood struct {
	Before []string
	After  []string
}

// DefaultPromptName is the template used when none is named.
const DefaultPromptName = "translation/ja_zh_v1"

// maxBatchSize bounds how many lines one request may carry.
//
// Above this the reply is long enough that truncation and dropped lines become
// likely, and the marginal context gain is spent. The ceiling exists so a
// misconfiguration degrades into more requests rather than into a broken run.
const maxBatchSize = 200

// maxSplitDepth bounds how many times a truncated batch is halved.
const maxSplitDepth = 3

// Outcome reports what a run did.
type Outcome struct {
	Translated int
	FromCache  int

	// PromptVersion identifies the template and style guidance that produced the
	// translations. It is recorded on the artifact, which is what makes "which
	// prompt produced this" answerable after the prompt has moved on.
	PromptVersion string

	// Failed lists lines the model never returned usable text for. They are not
	// a run failure: a project that is 99% translated is worth keeping, and the
	// lines are marked for review.
	Failed []string

	LLMRequests      int
	PromptTokens     int
	CompletionTokens int

	Elapsed time.Duration
}

// Translator translates segments using a provider.
type Translator struct {
	client       *provider.Client
	cache        *Cache
	providerName string
	log          *slog.Logger
}

// New builds a translator.
func New(client *provider.Client, cache *Cache, providerName string, log *slog.Logger) (*Translator, error) {
	if client == nil {
		return nil, errors.New("translation: no provider client was given")
	}
	if cache == nil {
		return nil, errors.New("translation: no cache was given")
	}
	if log == nil {
		log = slog.Default()
	}
	return &Translator{
		client:       client,
		cache:        cache,
		providerName: providerName,
		log:          log,
	}, nil
}

// line is one segment awaiting translation, with the key it is cached under.
type line struct {
	segment *subtitle.Segment

	key string

	// glossaryHash covers the terms that apply to this line alone, not the
	// terms that happened to share its batch. Keying on the batch's terms would
	// make the cache depend on the batch size, so changing an unrelated setting
	// would silently discard every cached translation.
	glossaryHash string

	// revision is the existing translation this line is being asked to improve.
	// It is part of the cache key because revising one translation and revising
	// a different one are different requests.
	revision string
}

// Translate translates the segments in place.
//
// Segments with a translated text already set are left alone: a user's edit is
// not something to overwrite, and making the caller clear the field is a much
// more visible act than relying on a flag.
func (t *Translator) Translate(
	ctx context.Context,
	segments []*subtitle.Segment,
	opts Options,
) (*Outcome, error) {
	started := time.Now()
	opts = opts.withDefaults()

	if err := t.validateOptions(opts); err != nil {
		return nil, err
	}

	template, err := prompts.Load(opts.PromptName)
	if err != nil {
		return nil, err
	}
	style, err := loadStyle(opts.Style)
	if err != nil {
		return nil, err
	}

	contextHash, err := hashString(opts.ContextDocument)
	if err != nil {
		return nil, err
	}

	lines, err := t.plan(segments, opts, template, style, contextHash)
	if err != nil {
		return nil, err
	}

	outcome := &Outcome{PromptVersion: template.Version() + "." + style.Version()}
	if len(lines) == 0 {
		outcome.Elapsed = time.Since(started)
		return outcome, nil
	}

	// Anything already translated by a previous run costs nothing.
	pending, err := t.applyCache(ctx, lines, outcome)
	if err != nil {
		return nil, err
	}

	batches := t.batch(pending, segments, opts)
	if len(batches) > 0 {
		if err := t.runBatches(ctx, batches, opts, template, style, outcome); err != nil {
			// An error here is systemic — a bad key, an unreachable host — and
			// the work already done is still on the segments. The caller
			// decides whether a partial result is worth keeping; the outcome
			// says how far it got either way.
			outcome.Elapsed = time.Since(started)
			return outcome, err
		}
	}

	outcome.Elapsed = time.Since(started)
	return outcome, nil
}

func (t *Translator) validateOptions(opts Options) error {
	if opts.SourceLanguage == "" || opts.TargetLanguage == "" {
		return errors.New("translation: both a source and a target language are required")
	}
	if opts.SourceLanguage == opts.TargetLanguage {
		// Almost always a configuration mistake, and one that produces a
		// confident result which is simply the input.
		return fmt.Errorf("translation: source and target language are both %q",
			opts.SourceLanguage)
	}
	return nil
}

// loadStyle loads the guidance that goes with a style name.
func loadStyle(style string) (*prompts.Template, error) {
	if style == "" {
		style = "natural"
	}

	template, err := prompts.Load("translation/style_" + style)
	if err != nil {
		return nil, fmt.Errorf(
			"translation: %q is not a known style; use literal, natural or fansub", style)
	}
	return template, nil
}

// plan decides which segments need work and computes each one's cache key.
func (t *Translator) plan(
	segments []*subtitle.Segment,
	opts Options,
	template, style *prompts.Template,
	contextHash string,
) ([]*line, error) {
	// The prompt version covers both the template and the style guidance,
	// because both are prompt text and changing either changes the output.
	promptVersion := template.Version() + "." + style.Version()

	allTexts := make([]string, 0, len(segments))
	for _, segment := range segments {
		allTexts = append(allTexts, segment.SourceText)
	}

	// Narrow to the terms that occur anywhere in this project before doing the
	// per-line pass. A series glossary runs to hundreds of entries and the
	// per-line check is a substring search over all of them, so removing the
	// ones that appear nowhere turns a quadratic scan into a linear one.
	candidates := glossary.Match(opts.Glossary, allTexts)

	lines := make([]*line, 0, len(segments))
	for _, segment := range segments {
		if segment.SourceText == "" {
			continue
		}

		// A line the user has already translated or edited is not
		// retranslated. Re-running the stage must not undo their work.
		//
		// A revising pass is the exception, and it is explicit: it takes the
		// existing translation as its input and produces a replacement, so
		// skipping what is already translated would leave it with nothing to do.
		revision := ""
		if segment.TranslatedText != nil {
			if !opts.Revise {
				continue
			}
			revision = strings.TrimSpace(*segment.TranslatedText)
			if revision == "" {
				// Nothing to revise. The ordinary path would translate it, and
				// so does this one.
				revision = ""
			}
		}

		applicable := glossary.Match(candidates, []string{segment.SourceText})
		glossaryHash, err := glossary.Hash(applicable)
		if err != nil {
			return nil, err
		}

		key, err := DeriveKey(KeyInputs{
			Source:        normaliseSource(segment.SourceText),
			Revision:      revision,
			Style:         opts.Style,
			Provider:      t.providerName,
			Model:         t.client.Model(),
			PromptVersion: promptVersion,
			ContextHash:   contextHash,
			GlossaryHash:  glossaryHash,
		})
		if err != nil {
			return nil, err
		}

		lines = append(lines, &line{
			segment:      segment,
			key:          key,
			glossaryHash: glossaryHash,
			revision:     revision,
		})
	}

	return lines, nil
}

// normaliseSource prepares a line for hashing.
//
// Whitespace is collapsed because the same line arrives with different spacing
// depending on how the recogniser tokenised it, and a cache that misses on a
// double space pays for the same translation twice.
func normaliseSource(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

// applyCache fills in lines that have been translated before.
func (t *Translator) applyCache(ctx context.Context, lines []*line, outcome *Outcome) ([]*line, error) {
	keys := make([]string, 0, len(lines))
	for _, item := range lines {
		keys = append(keys, item.key)
	}

	cached, err := t.cache.Get(ctx, keys)
	if err != nil {
		return nil, err
	}

	pending := make([]*line, 0, len(lines))
	for _, item := range lines {
		if translated, ok := cached[item.key]; ok {
			item.segment.SetTranslation(translated)
			outcome.FromCache++
			continue
		}
		pending = append(pending, item)
	}
	return pending, nil
}

// batch groups lines into requests, keeping consecutive lines together.
//
// Adjacency matters more than packing. Two lines of a conversation translated in
// one request read as a conversation; the same two lines split across requests
// read as two independent sentences, and the model has no way to recover what it
// was not shown. So a run of uncached lines stays intact and is only cut where
// the run itself is longer than a batch.
type batch struct {
	lines []*line

	before []*subtitle.Segment
	after  []*subtitle.Segment
}

func (t *Translator) batch(pending []*line, segments []*subtitle.Segment, opts Options) []batch {
	if len(pending) == 0 {
		return nil
	}

	// Which position each segment holds, so a line's neighbours can be found.
	position := make(map[string]int, len(segments))
	for i, segment := range segments {
		position[segment.ID] = i
	}

	var batches []batch
	var run []*line

	flush := func() {
		for start := 0; start < len(run); start += opts.BatchSize {
			end := min(start+opts.BatchSize, len(run))
			chunk := run[start:end]

			current := batch{lines: chunk}
			if at, ok := position[chunk[0].segment.ID]; ok && opts.ContextLines > 0 {
				low := max(0, at-opts.ContextLines)
				current.before = segments[low:at]

				high := min(len(segments), at+len(chunk)+opts.ContextLines)
				if at+len(chunk) < high {
					current.after = segments[at+len(chunk) : high]
				}
			}
			batches = append(batches, current)
		}
		run = nil
	}

	for i, item := range pending {
		if i > 0 && !adjacent(pending[i-1], item, position) {
			flush()
		}
		run = append(run, item)
	}
	flush()

	return batches
}

// adjacent reports whether two lines were neighbours in the original segment
// list, so that breaking between them breaks a conversation.
func adjacent(previous, current *line, position map[string]int) bool {
	previousAt, ok := position[previous.segment.ID]
	if !ok {
		return false
	}
	currentAt, ok := position[current.segment.ID]
	if !ok {
		return false
	}
	return currentAt == previousAt+1
}

// runBatches translates the batches, bounded by the concurrency setting.
func (t *Translator) runBatches(
	ctx context.Context,
	batches []batch,
	opts Options,
	template, style *prompts.Template,
	outcome *Outcome,
) error {
	budget := &tokenBudget{limit: opts.MaxTokensPerJob}

	// The first systemic error stops the run. Continuing would produce a
	// hundred identical failures and a hundred identical log lines.
	var (
		work   = make(chan batch)
		wg     sync.WaitGroup
		mu     sync.Mutex
		done   int
		failed []string
		fatal  error
	)

	workers := min(opts.Concurrency, len(batches))

	workerCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for current := range work {
				if workerCtx.Err() != nil {
					return
				}
				if budget.exhausted() {
					mu.Lock()
					if fatal == nil {
						fatal = fmt.Errorf("%w (%d tokens)", ErrBudgetExceeded, budget.spentTotal())
					}
					mu.Unlock()
					cancel()
					return
				}

				translations, missing, err := t.translateBatch(workerCtx, current, opts, budget, template, style, 0)

				mu.Lock()
				for id, text := range translations {
					if segment := findSegment(current.lines, id); segment != nil {
						segment.SetTranslation(text)
						outcome.Translated++
					}
				}
				failed = append(failed, missing...)
				done++

				budget.record(outcome)

				if err != nil {
					if fatal == nil {
						fatal = err
					}
					mu.Unlock()
					cancel()
					return
				}
				mu.Unlock()

				if opts.Progress != nil {
					opts.Progress(done, len(batches))
				}
			}
		}()
	}

	for _, current := range batches {
		select {
		case work <- current:
		case <-workerCtx.Done():
		}
		if workerCtx.Err() != nil {
			break
		}
	}
	close(work)
	wg.Wait()

	sort.Strings(failed)
	outcome.Failed = failed

	if fatal != nil {
		return fatal
	}
	return nil
}

// findSegment locates a line by segment id.
func findSegment(lines []*line, id string) *subtitle.Segment {
	for _, item := range lines {
		if item.segment.ID == id {
			return item.segment
		}
	}
	return nil
}

// translateBatch asks for a set of lines, retrying what comes back incomplete.
//
// It returns whatever it managed to obtain along with the ids it could not, and
// an error only when the cause was systemic. A model that drops one line out of
// twenty is a normal Tuesday, not a reason to fail a job.
func (t *Translator) translateBatch(
	ctx context.Context,
	current batch,
	opts Options,
	budget *tokenBudget,
	template, style *prompts.Template,
	depth int,
) (map[string]string, []string, error) {
	translations := make(map[string]string, len(current.lines))
	pending := current.lines

	var lastValidation BatchValidation

	for attempt := 1; attempt <= opts.MaxAttempts && len(pending) > 0; attempt++ {
		if err := ctx.Err(); err != nil {
			return translations, idsOf(pending), err
		}

		user, err := t.buildPrompt(current, pending, opts, template, style, attempt, lastValidation)
		if err != nil {
			return translations, idsOf(pending), err
		}

		response, err := t.client.Chat(ctx, provider.ChatRequest{
			System:      "",
			User:        user,
			Temperature: opts.Temperature,
			// The reply is a JSON object per line, so a generous bound keeps a
			// large batch from being cut off before it is finished.
			MaxTokens: maxTokensFor(len(pending)),
			JSON:      true,
		})

		if err != nil {
			// A truncated reply means the batch was too large, and the answer
			// is to ask for less rather than to ask again: the same request
			// truncates in the same place.
			if errors.Is(err, provider.ErrTruncated) {
				if depth < maxSplitDepth && len(pending) > 1 {
					t.log.Warn("the reply was cut off; splitting the batch",
						"lines", len(pending), "depth", depth+1)
					return t.splitAndTranslate(ctx, current, pending, opts, budget, template, style, depth, translations)
				}
				return translations, idsOf(pending), fmt.Errorf(
					"translation: the reply for %d line(s) was cut off by the token limit; raise max_tokens",
					len(pending))
			}

			// Retry a transient failure rather than failing the run. The
			// attempt counter is shared with the validation retries, which is
			// what bounds a provider that is failing every request.
			if wait, ok := retryable(err); ok && attempt < opts.MaxAttempts {
				t.log.Warn("retrying a failed translation request",
					"attempt", attempt, "wait", wait.Round(time.Millisecond), "error", err)
				if err := sleepCtx(ctx, wait, attempt); err != nil {
					return translations, idsOf(pending), err
				}
				continue
			}
			return translations, idsOf(pending), err
		}

		if response.PromptTokens > 0 || response.CompletionTokens > 0 {
			budget.add(response.PromptTokens, response.CompletionTokens)
		}
		budget.requests()

		raw, err := llmjson.Extract(response.Content)
		if err != nil {
			lastValidation = BatchValidation{Missing: idsOf(pending)}
			t.log.Warn("the model's reply was not JSON", "lines", len(pending), "error", err)
			continue
		}

		got, err := ParseTranslations(raw)
		if err != nil {
			lastValidation = BatchValidation{Missing: idsOf(pending)}
			t.log.Warn("the model's reply was not a translation list",
				"lines", len(pending), "error", err)
			continue
		}

		// Only ids this request asked for are accepted. A model that also
		// translated the context lines would otherwise write those translations
		// into segments this batch was not responsible for.
		wanted := map[string]bool{}
		for _, item := range pending {
			wanted[item.segment.ID] = true
		}
		for id := range got {
			if !wanted[id] {
				delete(got, id)
			}
		}

		validation := Validate(idsOf(pending), got)
		if !validation.Complete() {
			lastValidation = validation
			t.log.Warn("the model's reply was incomplete",
				"lines", len(pending), "detail", validation.Summary())
		}

		remaining := make([]*line, 0, len(pending))
		for _, item := range pending {
			if text, ok := got[item.segment.ID]; ok && strings.TrimSpace(text) != "" {
				translations[item.segment.ID] = text
				continue
			}
			remaining = append(remaining, item)
		}
		pending = remaining
	}

	failed := idsOf(pending)
	if len(failed) > 0 {
		t.log.Warn("giving up on lines the model would not translate",
			"count", len(failed), "detail", lastValidation.Summary())
	}

	if err := t.store(ctx, current, translations, opts, template, style); err != nil {
		// A cache write failure must not discard translations that are already
		// correct in memory. It costs money next run, which is worth a warning.
		t.log.Warn("could not store translations in the cache", "error", err)
	}

	return translations, failed, nil
}

// splitAndTranslate halves an oversized batch after a truncation.
//
// Asking for less is the only fix that works. A request whose reply was cut off
// at the token limit will be cut off at the same place if it is repeated
// verbatim, so retrying the batch unchanged would burn the whole attempt budget
// and fail anyway.
func (t *Translator) splitAndTranslate(
	ctx context.Context,
	current batch,
	pending []*line,
	opts Options,
	budget *tokenBudget,
	template, style *prompts.Template,
	depth int,
	translations map[string]string,
) (map[string]string, []string, error) {
	half := len(pending) / 2

	var failed []string
	for _, part := range [][]*line{pending[:half], pending[half:]} {
		partBatch := batch{lines: part, before: current.before, after: current.after}

		got, partFailed, err := t.translateBatch(ctx, partBatch, opts, budget, template, style, depth+1)
		for id, text := range got {
			translations[id] = text
		}
		failed = append(failed, partFailed...)

		if err != nil {
			return translations, failed, err
		}
	}

	return translations, failed, nil
}

// store writes newly obtained translations to the cache.
func (t *Translator) store(
	ctx context.Context,
	current batch,
	translations map[string]string,
	opts Options,
	template, style *prompts.Template,
) error {
	if len(translations) == 0 {
		return nil
	}

	promptVersion := template.Version() + "." + style.Version()

	entries := make([]CacheEntry, 0, len(translations))
	for _, item := range current.lines {
		text, ok := translations[item.segment.ID]
		if !ok {
			continue
		}
		entries = append(entries, CacheEntry{
			Key:        item.key,
			SourceText: normaliseSource(item.segment.SourceText),
			Translated: text,
			Provider:   t.providerName,
			Model:      t.client.Model(),
			PromptVers: promptVersion,
			Style:      opts.Style,
		})
	}

	return t.cache.Put(ctx, entries)
}

// buildPrompt renders the request for a batch.
func (t *Translator) buildPrompt(
	current batch,
	pending []*line,
	opts Options,
	template, style *prompts.Template,
	attempt int,
	validation BatchValidation,
) (string, error) {
	lines, err := renderLines(pending, opts.SourceLanguage, opts.TargetLanguage)
	if err != nil {
		return "", err
	}

	guidance, err := style.Render(nil)
	if err != nil {
		return "", err
	}

	// Only the terms that occur in the lines still to be translated. A term
	// sent for a line it does not apply to is an instruction the model will try
	// to honour somewhere it should not.
	pendingTexts := make([]string, 0, len(pending))
	for _, item := range pending {
		pendingTexts = append(pendingTexts, item.segment.SourceText)
	}
	applicable := glossary.Match(opts.Glossary, pendingTexts)

	before := renderContextLines(current.before)
	after := renderContextLines(current.after)

	// A single-line request has no batch to take neighbours from, so the
	// caller's are used instead. The batch's win when both exist: they are
	// adjacent to the lines being translated, which the caller's may not be.
	if opts.Neighbourhood != nil {
		if before == "" {
			before = strings.Join(opts.Neighbourhood.Before, "\n")
		}
		if after == "" {
			after = strings.Join(opts.Neighbourhood.After, "\n")
		}
	}

	user, err := template.Render(promptData{
		Context:       opts.ContextDocument,
		Glossary:      glossary.Block(applicable),
		StyleGuidance: strings.TrimSpace(guidance),
		ContextBefore: before,
		ContextAfter:  after,
		Lines:         lines,
	})
	if err != nil {
		return "", err
	}

	if attempt > 1 && !validation.Complete() {
		user += correctionNote(validation)
	}
	return user, nil
}

// maxTokensFor estimates a response budget for a batch.
//
// Chinese is compact but a reasoning model spends tokens before it emits
// anything, so the per-line allowance is far above the output itself. The floor
// keeps a short batch from being cut off before the model has finished thinking.
func maxTokensFor(lines int) int {
	const perLine = 220
	const floor = 1024
	const ceiling = 32000

	budget := lines * perLine
	if budget < floor {
		return floor
	}
	if budget > ceiling {
		return ceiling
	}
	return budget
}

// idsOf returns the segment ids of a line list.
func idsOf(lines []*line) []string {
	ids := make([]string, 0, len(lines))
	for _, item := range lines {
		ids = append(ids, item.segment.ID)
	}
	sort.Strings(ids)
	return ids
}

// retryable reports whether an error may succeed on a second attempt, and how
// long to wait first.
func retryable(err error) (time.Duration, bool) {
	var apiErr *provider.APIError
	if errors.As(err, &apiErr) {
		// The server's own guidance beats any backoff we would invent.
		return apiErr.RetryAfter, apiErr.Retryable()
	}

	var transportErr *provider.TransportError
	if errors.As(err, &transportErr) {
		return 0, true
	}

	// A context deadline is the request timing out, which is worth one retry: a
	// batch that took six minutes may well finish in five on a second attempt
	// against a less loaded server.
	if errors.Is(err, context.DeadlineExceeded) {
		return 0, true
	}

	return 0, false
}

// sleepCtx waits, returning early if the context is cancelled.
func sleepCtx(ctx context.Context, suggested time.Duration, attempt int) error {
	wait := suggested
	if wait <= 0 {
		// Exponential backoff with a cap. A floor of one second gives a rate
		// limiter time to recover without stalling a run that only hit one
		// transient failure.
		wait = time.Duration(math.Pow(2, float64(attempt-1))) * time.Second
	}
	if wait > maxBackoff {
		wait = maxBackoff
	}

	timer := time.NewTimer(wait)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// maxBackoff caps how long one retry waits.
//
// A server asking for longer than this is telling us it is unavailable, and a
// user watching a progress bar is better served by an error than by a run that
// appears to have hung.
const maxBackoff = 60 * time.Second

// ---------------------------------------------------------------------------
// Token budget
// ---------------------------------------------------------------------------

// tokenBudget tracks spend across concurrent batches.
type tokenBudget struct {
	mu        sync.Mutex
	limit     int
	prompt    int
	completed int
	requested int
}

// add records the tokens one request consumed.
func (b *tokenBudget) add(promptTokens, completionTokens int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.prompt += promptTokens + completionTokens
}

// requests records that a request was made.
func (b *tokenBudget) requests() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.requested++
}

// exhausted reports whether the ceiling has been reached.
func (b *tokenBudget) exhausted() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.limit > 0 && b.prompt >= b.limit
}

func (b *tokenBudget) spentTotal() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.prompt
}

// record copies a batch's consumption into the run's outcome.
func (b *tokenBudget) record(outcome *Outcome) {
	b.mu.Lock()
	defer b.mu.Unlock()
	outcome.PromptTokens = b.prompt
	outcome.LLMRequests = b.requested
}
