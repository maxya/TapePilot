FROM golang:1.26.11-alpine@sha256:ecb67f987aa1a57794c91d19ade7ae0209f2f0f5496aaef9e6ae85ddf92d02ab AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download && go mod verify
COPY internal internal
COPY cmd cmd
RUN CGO_ENABLED=0 go build -trimpath -o /tapepilot ./cmd/tapepilot
RUN mkdir -p /state && chmod 0700 /state
FROM debian:bookworm-slim@sha256:e67eeead736ff7bbe258ef088bff683635c561b1fcfefad0806c1df4ab49ce0e
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
    tar=1.34+dfsg-1.2+deb12u1 \
    mbuffer=20230301+ds1-1 \
    mt-st=1.7-1 \
    lsscsi=0.31-1+b1 \
    ca-certificates=20250419~deb12u2 \
 && rm -rf /var/lib/apt/lists/*
COPY --from=build --chown=65532:65532 /state /var/lib/tapepilot
RUN chmod 0700 /var/lib/tapepilot
COPY --from=build /tapepilot /tapepilot
COPY config/tapepilot.example.yaml /etc/tapepilot/config.yaml
LABEL org.opencontainers.image.title="TapePilot backend" \
      org.opencontainers.image.source="https://github.com/maxya/TapePilot"
USER 65532:65532
ENTRYPOINT ["/tapepilot"]
CMD ["-container","-config","/etc/tapepilot/config.yaml","serve"]
