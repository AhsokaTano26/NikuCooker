package protocol

// Shared data models.
//
// These are the structures that cross the language boundary as payloads. They
// mirror the core's internal models in internal/subtitle and
// internal/data-model; the wire form is the subset both languages agree on.
//
// Optional numerics are pointers on purpose. A confidence of 0.0 and "the model
// reported no confidence" are different facts, and collapsing them loses
// information that QC and the review UI both need.

// WordTimestamp is one word with its timing.
type WordTimestamp struct {
	Start       float64  `json:"start"`
	End         float64  `json:"end"`
	Text        string   `json:"text"`
	Probability *float64 `json:"probability,omitempty"`
}

// ASRSegment is one recognition segment as the model produced it.
//
// These are inputs to segmentation, never final subtitle lines: a Whisper
// segment is typically several seconds of continuous speech and is not a
// readable subtitle.
type ASRSegment struct {
	Start        float64         `json:"start"`
	End          float64         `json:"end"`
	Text         string          `json:"text"`
	AvgLogprob   *float64        `json:"avg_logprob,omitempty"`
	NoSpeechProb *float64        `json:"no_speech_prob,omitempty"`
	Words        []WordTimestamp `json:"words"`
}

// ASRResult is the full transcription payload.
//
// It is what the worker writes to result_path when one is supplied, and what it
// returns inline in the result event when one is not.
type ASRResult struct {
	Language            string       `json:"language"`
	LanguageProbability *float64     `json:"language_probability,omitempty"`
	Duration            float64      `json:"duration"`
	Segments            []ASRSegment `json:"segments"`
}

// VADRegion is one detected speech interval.
//
// Regions are ordered, non-overlapping, and each satisfies Start < End. The
// core validates that on receipt rather than trusting it: a violated invariant
// here propagates into impossible subtitle timings much later, where the cause
// is no longer visible.
type VADRegion struct {
	Start float64 `json:"start"`
	End   float64 `json:"end"`
}

// VADResult is the speech detection output.
type VADResult struct {
	Regions      []VADRegion `json:"regions"`
	TotalSpeechS float64     `json:"total_speech_s"`
	TotalAudioS  float64     `json:"total_audio_s"`
}

// LoadedModel describes a model currently resident in the worker.
//
// Device is what the model is *actually* running on, which may differ from what
// was requested when device was "auto" and the requested accelerator was
// unavailable.
type LoadedModel struct {
	Kind        string `json:"kind"`
	Name        string `json:"name"`
	Device      string `json:"device"`
	ComputeType string `json:"compute_type,omitempty"`
	MemoryMB    int64  `json:"memory_mb,omitempty"`
}
