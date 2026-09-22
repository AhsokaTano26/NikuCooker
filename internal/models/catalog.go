// Package models owns the model inventory: what exists, what is downloaded,
// and how much disk it uses.
//
// The core owns this and the Python worker owns *residency* — what is loaded
// right now. A missing model is a download problem the user fixes once; a model
// that will not fit in memory is a runtime problem tied to the machine's
// current state. Conflating them makes both harder to diagnose.
package models

// Kind separates recognition models from the bundled VAD.
type Kind string

const (
	KindASR Kind = "asr"
	KindVAD Kind = "vad"
)

// Entry is a model this build knows how to obtain.
type Entry struct {
	Kind     Kind
	Name     string
	Provider string

	// Repo is the Hugging Face repository. The CTranslate2 conversions rather
	// than the original OpenAI checkpoints: faster-whisper cannot load the
	// latter, and downloading two gigabytes to discover that is a poor
	// introduction.
	Repo string

	// ApproxBytes lets the UI say what a download will cost before it starts.
	// The difference between "large-v3" and "3 GB" matters on a metered
	// connection.
	ApproxBytes int64

	// Note is shown alongside the model in the UI.
	Note string
}

// Catalog lists the models this build can download.
//
// A fixed list rather than "whatever the hub has": a user choosing between
// eight named options with descriptions is better served than one facing a
// search box, and the sizes are what make the choice informed.
var Catalog = []Entry{
	{
		Kind: KindASR, Name: "tiny", Provider: "faster-whisper",
		Repo: "Systran/faster-whisper-tiny", ApproxBytes: 78_000_000,
		Note: "Fastest and least accurate. Useful for testing the pipeline end to end.",
	},
	{
		Kind: KindASR, Name: "base", Provider: "faster-whisper",
		Repo: "Systran/faster-whisper-base", ApproxBytes: 145_000_000,
		Note: "A step up from tiny, still comfortably realtime on a laptop CPU.",
	},
	{
		Kind: KindASR, Name: "small", Provider: "faster-whisper",
		Repo: "Systran/faster-whisper-small", ApproxBytes: 486_000_000,
		Note: "The practical floor for a machine with little memory.",
	},
	{
		Kind: KindASR, Name: "medium", Provider: "faster-whisper",
		Repo: "Systran/faster-whisper-medium", ApproxBytes: 1_530_000_000,
		Note: "The default. Noticeably better than small, still usable on CPU.",
	},
	{
		Kind: KindASR, Name: "large-v3", Provider: "faster-whisper",
		Repo: "Systran/faster-whisper-large-v3", ApproxBytes: 3_090_000_000,
		Note: "Most accurate. On CPU it runs at roughly 2–3x realtime, so it is " +
			"worth it mainly with a GPU.",
	},
	{
		Kind: KindASR, Name: "distil-large-v3", Provider: "faster-whisper",
		Repo: "Systran/faster-distil-whisper-large-v3", ApproxBytes: 1_510_000_000,
		Note: "Roughly as accurate as large-v3 for transcription at about half " +
			"the cost. English-focused; weaker on other languages.",
	},
}

// Lookup finds a catalog entry.
func Lookup(kind Kind, name string) (Entry, bool) {
	for _, entry := range Catalog {
		if entry.Kind == kind && entry.Name == name {
			return entry, true
		}
	}
	return Entry{}, false
}

// ID renders a model's identifier, which is also its primary key.
func ID(kind Kind, name string) string {
	return string(kind) + ":" + name
}

// SplitID parses an identifier.
func SplitID(id string) (Kind, string, bool) {
	for i := range len(id) {
		if id[i] == ':' {
			kind := Kind(id[:i])
			if kind != KindASR && kind != KindVAD {
				return "", "", false
			}
			return kind, id[i+1:], true
		}
	}
	return "", "", false
}
