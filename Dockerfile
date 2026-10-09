# review-panel under the ReviewBench agent contract:
# https://github.com/review-bench/ReviewBench/blob/main/AGENT_CONTRACT.md
#
# Credentials come from the run, never the image: OPENAI_API_KEY for the
# default openai provider, or the chosen Goose provider's own variables.
# Egress: the model host (RB_MODEL_BASE_URL, default api.openai.com).
# Configuration (RB_CONFIG_*): PROVIDER, MODEL, EFFORT, ROLE_MODELS,
# ROLE_EFFORTS, GATE (on|off).
FROM golang:1.26 AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /review-panel ./cmd/review-panel

FROM debian:bookworm-slim
ARG GOOSE_VERSION=1.43.0
ARG GOOSE_SHA256=a9a96f559a8b5f20b11597b78e4aa5bb0b9b29796ec4f808ca466a3f59a5ec20
RUN apt-get update \
    && apt-get install -y --no-install-recommends git ca-certificates curl ripgrep libstdc++6 \
    && curl -fsSL -o /tmp/goose.tgz "https://github.com/aaif-goose/goose/releases/download/v${GOOSE_VERSION}/goose-x86_64-unknown-linux-gnu.tar.gz" \
    && echo "${GOOSE_SHA256}  /tmp/goose.tgz" | sha256sum -c - \
    && tar -xzf /tmp/goose.tgz -C /usr/local/bin ./goose \
    && rm /tmp/goose.tgz \
    && apt-get purge -y curl && apt-get autoremove -y && rm -rf /var/lib/apt/lists/*
COPY --from=build /review-panel /usr/local/bin/review-panel
ENTRYPOINT ["review-panel", "reviewbench"]
