# Laya "System 1" decision model as a sidecar for BabelGate's smart router.
# CPU-only torch keeps the image at ~2 GB instead of ~8 GB with CUDA; Laya
# answers in tens to a few hundred milliseconds on a few CPU cores.
FROM python:3.12-slim

ARG LAYA_VERSION=0.3.21
RUN pip install --no-cache-dir torch --index-url https://download.pytorch.org/whl/cpu \
 && pip install --no-cache-dir "laya[serve]==${LAYA_VERSION}"

# Checkpoints are downloaded from Hugging Face on first start; mount a volume
# here so restarts do not download them again.
ENV HF_HOME=/models \
    LAYA_PORT=8000 \
    LAYA_MODELS=english \
    LAYA_MAX_LOADED=1
VOLUME /models
EXPOSE 8000

HEALTHCHECK --interval=30s --timeout=5s --start-period=180s \
  CMD python -c "import urllib.request; urllib.request.urlopen('http://127.0.0.1:8000/health')"

CMD ["laya-serve"]
