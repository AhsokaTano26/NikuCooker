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

ARG CODE_REVISION=unknown

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
        -ldflags "-s -w -X main.codeRevision=${CODE_REVISION}" \
        -o /out/nikucooker ./cmd/nikucooker

# ---------------------------------------------------------------------------
# Stage 3a: CPU runtime
# ---------------------------------------------------------------------------

FROM python:3.12-slim AS runtime-cpu

ARG AI_EXTRAS=cpu

RUN apt-get update && apt-get install -y --no-install-recommends \
        ffmpeg \
        ca-certificates \
        tini \
    && rm -rf /var/lib/apt/lists/*

COPY --from=ghcr.io/astral-sh/uv:latest /uv /usr/local/bin/uv

WORKDIR /app/ai
COPY ai/pyproject.toml ai/uv.lock ./
RUN uv sync --frozen --no-dev --extra ${AI_EXTRAS} \
    && rm -rf /root/.cache/uv
COPY ai/ ./

COPY --from=go-builder /out/nikucooker /usr/local/bin/nikucooker

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

RUN apt-get update && apt-get install -y --no-install-recommends \
        ffmpeg \
        ca-certificates \
        tini \
        curl \
    && rm -rf /var/lib/apt/lists/*

COPY --from=ghcr.io/astral-sh/uv:latest /uv /usr/local/bin/uv

# uv installs the interpreter itself, so no deadsnakes PPA is needed and the
# base image's Python version is irrelevant.
ENV UV_PYTHON_INSTALL_DIR=/opt/python
RUN uv python install ${PYTHON_VERSION}

WORKDIR /app/ai
COPY ai/pyproject.toml ai/uv.lock ./
RUN uv sync --frozen --no-dev --extra ${AI_EXTRAS} \
    && rm -rf /root/.cache/uv
COPY ai/ ./

COPY --from=go-builder /out/nikucooker /usr/local/bin/nikucooker

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
