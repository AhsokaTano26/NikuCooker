package main

// starterConfig is the file `nikucooker config init` writes.
//
// Everything is commented out, so the file starts out meaning exactly what no
// file at all means: it is a list of the settings worth knowing about, not a
// second set of defaults that could drift from the real ones. The values shown
// are the defaults, so uncommenting a line and restarting is a no-op until it
// is edited — which is what makes it safe to experiment in.
//
// Settings not listed here still work; they are omitted because changing them
// is rare enough that the full list would bury the ones that are not.
const starterConfig = `# NikuCooker configuration.
#
# Every setting here is optional: the defaults are a complete, working setup,
# and this file only needs to exist to change one of them. Delete it and the
# program keeps working.
#
# Read this file's location with ` + "`nikucooker config path`" + `.
# Precedence, lowest first:
#   defaults -> this file -> environment -> project overlay -> command line

server:
  host: 127.0.0.1
  port: 8080

  # allow_path_source lets a project be created from any path this process can
  # read — the whole disk, not just the video you meant. It is off by default
  # for that reason, and turning it on is what the web interface's "new project"
  # page needs to accept a server-side path.
  #
  # Only enable it where everyone who can reach the port is someone you would
  # hand your filesystem to. The server has no authentication, so "the port is
  # reachable" is the whole of the access check.
  # allow_path_source: false

storage:
  # Where projects, artifacts and the database live. Relative paths resolve
  # against the working directory.
  # data_dir: ./data
  # model_dir: ./models

translation:
  # The one thing that must be configured: speech recognition runs locally, but
  # translation calls an LLM. Any OpenAI-compatible /chat/completions endpoint
  # works — OpenAI, DeepSeek, Moonshot, Groq, OpenRouter, Ollama, vLLM, LM Studio.
  #
  # Can also be set in the web interface under "翻译服务", which stores the key
  # in the database and never returns it to the browser.
  # base_url: https://api.example.com/v1
  # api_key: sk-...
  # model: some-model

  # literal  keeps Japanese structure and honorifics — good for study
  # natural  rewrites into Chinese word order
  # fansub  keeps honorifics, prefers brevity (default)
  # style: fansub

  # Lines per request, and how many lines of surrounding context to attach.
  # Larger batches cost fewer requests but make a failed batch more expensive.
  # batch_size: 20
  # context_lines: 2
  # concurrency: 2

  # Hard ceiling on tokens for one run. Reaching it fails the run rather than
  # truncating it: a project that silently translated half the lines looks
  # finished until someone watches it.
  # max_tokens_per_job: 0

asr:
  # tiny (74 MB) is enough to try the pipeline out; medium (1.4 GB) is the
  # default; large-v3 (2.9 GB) is the most accurate and the slowest.
  # model: medium

  # auto | cpu | cuda. CUDA needs an NVIDIA GPU.
  # device: auto

  # Word timestamps are what let the editor split a line between words. Turning
  # them off makes splitting unavailable rather than imprecise.
  # word_timestamps: true

subtitle:
  # Both are written. SRT plays anywhere; ASS carries styling and is what the
  # render stage burns in or muxes.
  # formats: [srt, ass]

  # Original above, translation below.
  # bilingual: false

  # Reading speed ceiling, in characters per second. Lines above this are
  # flagged in the review queue rather than cut.
  # max_cps: 18

  # ASS style: fansub | default | broadcast
  # preset: fansub

pipeline:
  # Stages the pipeline skips. polish re-runs every line through the model to
  # improve wording, roughly doubling translation cost.
  # disabled: [polish]

render:
  # Which versions to produce. Listing both produces both from one run.
  #
  #   soft  muxes the subtitles in as a separate track, copying the video
  #         stream. Seconds, lossless, and the viewer can turn them off.
  #   hard  burns them into the picture. A full re-encode, slow, irreversible,
  #         and needs an FFmpeg built with libass. Run "nikucooker doctor" to
  #         see whether this machine has one.
  #
  # The hard version is named ...nikucooker.hardsub.mkv so the two are not
  # confused once they are sitting in the same folder.
  # modes: [soft]
  # modes: [soft, hard]

log:
  # debug | info | warn | error
  # level: info
`
