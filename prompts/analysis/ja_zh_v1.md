You are preparing to translate a Japanese video into Simplified Chinese. Before
translating, read the transcript excepts below and describe the work, so that a
translator who has never seen it can translate consistently.

You are NOT translating the transcript. You are describing what a translator
would need to know about it.

## Transcript

{{.Transcript}}

## What to report

- **summary**: two or three sentences on what happens in these excerpts.
- **setting**: the world, period and situation, as far as the transcript shows.
- **tone**: the register — comedy, drama, documentary, commentary — and how
  formal the speech is.
- **characters**: everyone who speaks or is spoken to. Give the Japanese name as
  it appears and a Chinese rendering to use consistently. Say who they are.
- **terms**: recurring names, places, organisations, titles, techniques or
  honorifics whose Chinese form should stay fixed across the whole work. Include
  a short note on each where the right rendering is not obvious.
- **notes**: anything else that affects translation — dialect, wordplay, a
  running joke, speech that is deliberately archaic or childish.

Report only what the transcript supports. Do not invent plot, characters or
relationships that are not present, and do not guess at a name's spelling: if a
name is unclear from the audio, say so in `notes` rather than committing to a
rendering.

## Output

Reply with JSON only. No explanation, no prose, and no markdown code fence.

{
  "summary": "…",
  "setting": "…",
  "tone": "…",
  "characters": [{"name_ja": "…", "name_zh": "…", "role": "…"}],
  "terms": [{"source": "…", "target": "…", "type": "term|character|place|org|work|honorific", "note": "…"}],
  "notes": ["…"]
}

Use empty arrays and empty strings where there is nothing to report. Do not omit
a field.
