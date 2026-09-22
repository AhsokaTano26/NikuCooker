# syntax=docker/dockerfile:1.7
#
# Two runtime images, one Go binary.
#
# The Go binary is CGO-free and carries the web application, so it is identical
# between the CPU and CUDA images. Only the Python environment and the base
# image differ — which means one `go build` and two thin wrappers, rather than
# two build paths that can drift.
#
#   docker build --target runtime-cpu  -t nikucooker:cpu  .
#   docker build --target runtime-cuda -t nikucooker:cuda .
#
# See docs/deployment.md.

# ---------------------------------------------------------------------------
# Stage 1: web application
# ---------------------------------------------------------------------------

FROM node:24-alpine AS web-builder

WORKDIR /src
RUN corepack enable

# Copied first so this layer caches until the lockfile actually changes.
COPY web/package.json web/pnpm-lock.yaml ./
RUN pnpm install --frozen-lockfile

COPY web/ ./
RUN pnpm build

# ---------------------------------------------------------------------------
# Stage 2: Go binary
# ---------------------------------------------------------------------------

FROM golang:1.26-alpine AS go-builder

ARG VERSION=dev
ARG COMMIT=unknown
ARG CODE_REVISION=unknown
ARG BUILD_DATE=unknown

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

# The //go:embed directive needs the built assets present before compilation,
# and it needs dist/ to exist either way — hence the .gitkeep fallback.
COPY --from=web-builder /src/dist ./web/dist

# CGO_ENABLED=0 is what makes the release matrix a loop rather than a
# per-target toolchain problem. -trimpath keeps build paths out of the binary.
RUN CGO_ENABLED=0 go build \
        -trimpath \
        -ldflags "-s -w \
            -X main.version=${VERSION} \
            -X main.commit=${COMMIT} \
            -X main.codeRevision=${CODE_REVISION} \
            -X main.buildDate=${BUILD_DATE}" \
        -o /out/nikucooker ./cmd/nikucooker

# ---------------------------------------------------------------------------
# Stage 3a: CPU runtime
# ---------------------------------------------------------------------------

FROM python:3.12-slim AS runtime-cpu

ARG AI_EXTRAS=cpu

# fonts-noto-cjk is what the subtitle styles name, and without it a burned-in
# render produces a screen of empty boxes: FFmpeg and libass succeed, the file
# plays, and the only way to find out is to watch it. Nothing else here pulls
# in a CJK font — fontconfig ships with ffmpeg but its font sets do not — so it
# is a dependency rather than a nicety.
RUN apt-get update && apt-get install -y --no-install-recommends \
        ffmpeg \
        ca-certificates \
        tini \
        fonts-noto-cjk \
    && rm -rf /var/lib/apt/lists/*

COPY --from=ghcr.io/astral-sh/uv:latest /uv /usr/local/bin/uv

WORKDIR /app/ai
# README.md is copied too: pyproject.toml declares it, and uv builds the
# project itself (an editable install) before syncing dependencies. Without
# it, hatchling fails with "Readme file does not exist".
COPY ai/pyproject.toml ai/uv.lock ai/README.md ./
RUN uv sync --frozen --no-dev --extra ${AI_EXTRAS} \
    && rm -rf /root/.cache/uv
COPY ai/ ./

COPY --from=go-builder /out/nikucooker /usr/local/bin/nikucooker

# The protocol fixtures live in the Go module and are read across the language
# boundary. The worker derives its schema digest from them, so they have to be
# present here or the handshake falls back to the manifest's weaker recorded
# value. The path matters: schema.py resolves it as parents[3] of its own file,
# which is /app, so no environment variable is needed.
COPY pkg/protocol/testdata /app/pkg/protocol/testdata

ENV NIKUCOOKER_DATA_DIR=/data \
    NIKUCOOKER_MODEL_DIR=/models \
    NIKUCOOKER_CONFIG=/config/config.yaml \
    NIKUCOOKER_PYTHON=/app/ai/.venv/bin/python \
    HF_HOME=/models/.hf

RUN mkdir -p /data /models /config
VOLUME ["/data", "/models", "/config"]
EXPOSE 8080

# tini is PID 1 so that the Python child is reaped and signals reach the whole
# process group. Without it, `docker stop` can leave an orphaned worker holding
# several gigabytes of model memory.
ENTRYPOINT ["/usr/bin/tini", "--", "nikucooker"]
CMD ["serve", "--host", "0.0.0.0", "--port", "8080"]

# ---------------------------------------------------------------------------
# Stage 3b: CUDA runtime
# ---------------------------------------------------------------------------

# The plain runtime variant, not cudnn-runtime: CTranslate2 4.8.2 wheels are
# built with -DWITH_CUDNN=OFF and contain no cuDNN symbols. The vendor's own
# installation page still says "cuDNN 8" and is stale — see
# docs/dependency-audit.md §1.
FROM nvidia/cuda:12.8.0-runtime-ubuntu22.04 AS runtime-cuda

ARG AI_EXTRAS=cuda
ARG PYTHON_VERSION=3.12

# fonts-noto-cjk for the same reason as the CPU image: the subtitle styles name
# it, and a render without it is a screen of boxes.
RUN apt-get update && apt-get install -y --no-install-recommends \
        ffmpeg \
        ca-certificates \
        tini \
        curl \
        fonts-noto-cjk \
    && rm -rf /var/lib/apt/lists/*

COPY --from=ghcr.io/astral-sh/uv:latest /uv /usr/local/bin/uv

# uv installs the interpreter itself, so no deadsnakes PPA is needed and the
# base image's Python version is irrelevant.
ENV UV_PYTHON_INSTALL_DIR=/opt/python
RUN uv python install ${PYTHON_VERSION}

WORKDIR /app/ai
# README.md is copied too: pyproject.toml declares it, and uv builds the
# project itself (an editable install) before syncing dependencies. Without
# it, hatchling fails with "Readme file does not exist".
COPY ai/pyproject.toml ai/uv.lock ai/README.md ./
RUN uv sync --frozen --no-dev --extra ${AI_EXTRAS} \
    && rm -rf /root/.cache/uv
COPY ai/ ./

COPY --from=go-builder /out/nikucooker /usr/local/bin/nikucooker

# The protocol fixtures live in the Go module and are read across the language
# boundary. The worker derives its schema digest from them, so they have to be
# present here or the handshake falls back to the manifest's weaker recorded
# value. The path matters: schema.py resolves it as parents[3] of its own file,
# which is /app, so no environment variable is needed.
COPY pkg/protocol/testdata /app/pkg/protocol/testdata

ENV NIKUCOOKER_DATA_DIR=/data \
    NIKUCOOKER_MODEL_DIR=/models \
    NIKUCOOKER_CONFIG=/config/config.yaml \
    NIKUCOOKER_PYTHON=/app/ai/.venv/bin/python \
    HF_HOME=/models/.hf

RUN mkdir -p /data /models /config
VOLUME ["/data", "/models", "/config"]
EXPOSE 8080

ENTRYPOINT ["/usr/bin/tini", "--", "nikucooker"]
CMD ["serve", "--host", "0.0.0.0", "--port", "8080"]
