FROM golang:1.24-bookworm AS go-build
WORKDIR /src
COPY backend/go.mod backend/go.sum ./
RUN go mod download
COPY backend/ ./
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w' -o /out/musicd ./cmd/musicd

FROM node:22.14.0-bookworm-slim AS frontend-build
WORKDIR /build
COPY frontend/package*.json ./
RUN npm ci
COPY frontend/ ./
RUN npm run build

# Python is retained only for the transcription pipeline (models, music21, exports).
FROM python:3.11.14-slim-bookworm AS worker-base
ENV PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1 PYTHONPATH=/app \
    DATA_DIR=/data OMP_NUM_THREADS=2 OPENBLAS_NUM_THREADS=2 MKL_NUM_THREADS=2
RUN apt-get update && apt-get install -y --no-install-recommends ffmpeg fluidsynth musescore3 \
    xvfb xauth fonts-dejavu-core timgm6mb-soundfont libsndfile1 ca-certificates \
    && mkdir -p /soundfonts && cp /usr/share/sounds/sf2/TimGM6mb.sf2 /soundfonts/default.sf2 \
    && cp /usr/share/doc/timgm6mb-soundfont/copyright /soundfonts/LICENSE \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY backend/requirements.txt ./requirements.txt
RUN pip install --no-cache-dir -r requirements.txt
COPY backend/pipeline ./pipeline
COPY --from=go-build /out/musicd /usr/local/bin/musicd
RUN groupadd --gid 10001 music && useradd --uid 10001 --gid music --create-home music \
    && mkdir -p /data /models && chown -R music:music /data /models /app
ENV SOUNDFONT_PATH=/soundfonts/default.sf2 MUSESCORE_BIN=musescore3 PIPELINE_PYTHON=/usr/local/bin/python
USER music
ENTRYPOINT ["/usr/local/bin/musicd"]
HEALTHCHECK --interval=30s --timeout=10s CMD ["musicd", "health", "--role", "api"]

# API/migrate/dispatcher are Go-only: no Torch, model venv or Python runtime.
FROM debian:bookworm-slim AS go-runtime
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --gid 10001 music && useradd --uid 10001 --gid music --create-home music \
    && mkdir -p /data && chown music:music /data
COPY --from=go-build /out/musicd /usr/local/bin/musicd
ENV DATA_DIR=/data COOKIE_SECURE=true
USER music
ENTRYPOINT ["/usr/local/bin/musicd"]
CMD ["api"]

FROM python:3.11.14-slim-bookworm AS cpu-models
RUN python -m venv /opt/models-venv \
    && /opt/models-venv/bin/pip install --no-cache-dir torch==2.5.1 torchaudio==2.5.1 --index-url https://download.pytorch.org/whl/cpu
COPY backend/requirements-models.txt /tmp/requirements-models.txt
RUN /opt/models-venv/bin/pip install --no-cache-dir -r /tmp/requirements-models.txt \
    && /opt/models-venv/bin/pip install --no-cache-dir --no-deps basic-pitch==0.4.0

FROM worker-base AS worker-cpu
COPY --from=cpu-models /opt/models-venv /opt/models-venv
ENV MODEL_PYTHON=/opt/models-venv/bin/python INFERENCE_DEVICE=cpu \
    DEMUCS_REPO=/models/demucs BASIC_PITCH_MODEL_PATH=/models/basic-pitch/nmp.onnx HF_HUB_OFFLINE=1
USER music
HEALTHCHECK --interval=30s --timeout=10s CMD ["musicd", "health", "--role", "worker"]
CMD ["worker"]

FROM go-runtime AS runtime

FROM python:3.11.14-slim-bookworm AS gpu-models
RUN python -m venv /opt/models-venv \
    && /opt/models-venv/bin/pip install --no-cache-dir torch==2.5.1 torchaudio==2.5.1 --index-url https://download.pytorch.org/whl/cu118
COPY backend/requirements-models.txt /tmp/requirements-models.txt
RUN /opt/models-venv/bin/pip install --no-cache-dir -r /tmp/requirements-models.txt \
    && /opt/models-venv/bin/pip install --no-cache-dir --no-deps basic-pitch==0.4.0

FROM worker-base AS worker-gpu
COPY --from=gpu-models /opt/models-venv /opt/models-venv
ENV MODEL_PYTHON=/opt/models-venv/bin/python INFERENCE_DEVICE=cuda \
    DEMUCS_REPO=/models/demucs BASIC_PITCH_MODEL_PATH=/models/basic-pitch/nmp.onnx HF_HUB_OFFLINE=1
USER music
HEALTHCHECK --interval=30s --timeout=10s CMD ["musicd", "health", "--role", "worker"]
CMD ["worker"]

FROM nginxinc/nginx-unprivileged:1.27.5-alpine AS web
COPY deploy/nginx.conf /etc/nginx/conf.d/default.conf
COPY --from=frontend-build /build/dist /usr/share/nginx/html
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=3s CMD wget -q -O /dev/null http://127.0.0.1:8080/healthz || exit 1
