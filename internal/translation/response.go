package translation

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/AhsokaTano26/NikuCooker/internal/llmjson"
)

// translationItem is one entry of a model's reply.
//
// The field names vary between models and between prompt versions: some reply
// with `text`, some with `translation`, some with `target`. All are accepted
// rather than retried, because a retry costs money and the meaning is not in
// doubt.
type translationItem struct {
	ID          string `json:"id"`
	Text        string `json:"text"`
	Translation string `json:"translation"`
	Target      string `json:"target"`
	Source      string `json:"source"`
}

func (item translationItem) value() string {
	for _, candidate := range []string{item.Text, item.Translation, item.Target} {
		if strings.TrimSpace(candidate) != "" {
			return candidate
		}
	}
	return ""
}

// ParseTranslations decodes a reply into a map from segment id to translation.
//
// Three shapes are accepted, because all three are things a competent model
// produces when asked for "JSON only": the documented object with a
// `translations` array, a bare array, and an object keyed by id. Rejecting the
// last two would mean paying for a correct answer and throwing it away.
func ParseTranslations(raw []byte) (map[string]string, error) {
	var envelope struct {
		Translations []translationItem `json:"translations"`
		Lines        []translationItem `json:"lines"`
		Results      []translationItem `json:"results"`
	}
	if err := json.Unmarshal(raw, &envelope); err == nil {
		for _, items := range [][]translationItem{envelope.Translations, envelope.Lines, envelope.Results} {
			if len(items) > 0 {
				return collect(items), nil
			}
		}
	}

	var list []translationItem
	if err := json.Unmarshal(raw, &list); err == nil && len(list) > 0 {
		return collect(list), nil
	}

	// An object keyed by id: {"seg_0001": "…", "seg_0002": "…"}.
	var byID map[string]json.RawMessage
	if err := json.Unmarshal(raw, &byID); err == nil {
		out := make(map[string]string, len(byID))
		for id, value := range byID {
			var text string
			if err := json.Unmarshal(value, &text); err != nil {
				// A nested object under the id key, as in
				// {"seg_0001": {"text": "…"}}.
				var item translationItem
				if err := json.Unmarshal(value, &item); err != nil {
					continue
				}
				text = item.value()
			}
			if strings.TrimSpace(text) != "" {
				out[id] = text
			}
		}
		if len(out) > 0 {
			return out, nil
		}
	}

	return nil, fmt.Errorf("translation: the model's reply was not a translation list: %s",
		llmjson.Truncate(string(raw), 200))
}

func collect(items []translationItem) map[string]string {
	out := make(map[string]string, len(items))
	for _, item := range items {
		// An entry with no id cannot be attributed to a line. Guessing by
		// position would silently mis-assign every line after a dropped entry,
		// which is worse than reporting the batch as incomplete.
		if item.ID == "" {
			continue
		}
		value := normaliseTranslation(item.value())
		if value == "" {
			continue
		}
		out[item.ID] = value
	}
	return out
}

// normaliseTranslation tidies a single translation.
func normaliseTranslation(text string) string {
	text = strings.TrimSpace(text)

	// A model occasionally quotes the whole line, or keeps the 「」 from the
	// Japanense. Quotes around a translation are almost never intentional, but
	// quotation marks inside dialogue are, so only a matched pair wrapping the
	// entire string is removed.
	if len(text) >= 2 {
		pairs := [][2]string{{`"`, `"`}, {`'`, `'`}, {"“", "”"}}
		for _, pair := range pairs {
			if strings.HasPrefix(text, pair[0]) && strings.HasSuffix(text, pair[1]) &&
				len(text) > len(pair[0])+len(pair[1]) {
				inner := text[len(pair[0]) : len(text)-len(pair[1])]
				if !strings.Contains(inner, pair[0]) {
					text = strings.TrimSpace(inner)
				}
			}
		}
	}

	// A model asked for one line sometimes prefixes its own labelling.
	for _, prefix := range []string{"译文：", "翻译：", "Translation:"} {
		if strings.HasPrefix(text, prefix) {
			text = strings.TrimSpace(strings.TrimPrefix(text, prefix))
		}
	}

	return text
}

// BatchValidation describes how a reply fell short of what was asked for.
type BatchValidation struct {
	// Missing are ids that were requested and not returned.
	Missing []string

	// Extra are ids that were returned and not requested. Not an error on its
	// own — it usually means the model translated a context line — but it
	// indicates the model is not following the instructions, which is worth
	// knowing before the missing ones are attributed to carelessness.
	Extra []string

	// Empty are ids returned with an empty translation.
	Empty []string
}

// Complete reports whether the reply covered exactly what was asked.
func (v BatchValidation) Complete() bool {
	return len(v.Missing) == 0 && len(v.Empty) == 0
}

// Summary renders the shortfall for a log line or a retry note.
func (v BatchValidation) Summary() string {
	var parts []string
	if len(v.Missing) > 0 {
		parts = append(parts, fmt.Sprintf("missing %s", joinIDs(v.Missing)))
	}
	if len(v.Empty) > 0 {
		parts = append(parts, fmt.Sprintf("empty %s", joinIDs(v.Empty)))
	}
	if len(v.Extra) > 0 {
		parts = append(parts, fmt.Sprintf("unexpected %s", joinIDs(v.Extra)))
	}
	return strings.Join(parts, "; ")
}

// Validate compares a reply against what was requested.
func Validate(requested []string, got map[string]string) BatchValidation {
	wanted := make(map[string]bool, len(requested))
	var validation BatchValidation

	for _, id := range requested {
		wanted[id] = true
		value, ok := got[id]
		switch {
		case !ok:
			validation.Missing = append(validation.Missing, id)
		case strings.TrimSpace(value) == "":
			validation.Empty = append(validation.Empty, id)
		}
	}

	for id := range got {
		if !wanted[id] {
			validation.Extra = append(validation.Extra, id)
		}
	}

	// Sorted so that the retry note and the log line are stable between runs of
	// the same failing batch. Map iteration order is not.
	sort.Strings(validation.Missing)
	sort.Strings(validation.Empty)
	sort.Strings(validation.Extra)

	return validation
}

func joinIDs(ids []string) string {
	const show = 5
	if len(ids) <= show {
		return strings.Join(ids, ", ")
	}
	return strings.Join(ids[:show], ", ") + fmt.Sprintf(" and %d more", len(ids)-show)
}
