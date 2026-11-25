FROM golang:1.26.11-alpine@sha256:ecb67f987aa1a57794c91d19ade7ae0209f2f0f5496aaef9e6ae85ddf92d02ab AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY internal internal
COPY cmd cmd
RUN CGO_ENABLED=0 go build -trimpath -o /tapepilot ./cmd/tapepilot
RUN mkdir -p /state && chmod 0700 /state
FROM debian:bookworm-slim@sha256:e67eeead736ff7bbe258ef088bff683635c561b1fcfefad0806c1df4ab49ce0e
RUN apt-get update \
 && apt-get install -y --no-install-recommends mbuffer=20230301+ds1-1 \
 && rm -rf /var/lib/apt/lists/*
COPY --from=build --chown=65532:65532 /state /var/lib/tapepilot
RUN chmod 0700 /var/lib/tapepilot
COPY --from=build /tapepilot /tapepilot
COPY config/tapepilot.example.yaml /etc/tapepilot/config.yaml
USER 65532:65532
ENTRYPOINT ["/tapepilot"]
CMD ["-container","-config","/etc/tapepilot/config.yaml","serve"]
