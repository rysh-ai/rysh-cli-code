# Rysh CLI — try-before-install container image (design 011, D3).
#
# rysh-cli is excluded from go.work and replaces github.com/rysh-ai/rysh-shared
# with ../rysh-shared, so the build context must include BOTH module dirs. Build
# from the MONOREPO ROOT:
#
#   docker build -f rysh-cli/Dockerfile -t rysh/rysh .
#   docker run -it --rm rysh/rysh
#
# Work on a repo without installing:
#   docker run -it --rm -v "$PWD:/work" -w /work rysh/rysh
#
# ---- build stage -----------------------------------------------------------
FROM golang:1.25-alpine AS build
RUN apk add --no-cache git
WORKDIR /src
# Copy both modules; rysh-cli's `replace ../rysh-shared` resolves inside /src.
COPY rysh-shared/ ./rysh-shared/
COPY rysh-cli/ ./rysh-cli/
WORKDIR /src/rysh-cli
ENV GOWORK=off CGO_ENABLED=0
RUN go build -ldflags "-s -w" -o /out/rysh ./cmd/rysh

# ---- runtime stage ---------------------------------------------------------
FROM alpine:3.20
# rysh PTY-spawns a shell per pane and embeds a NATS server; ca-certificates for
# provider TLS, git for the agentic git tooling, bash as the default shell.
RUN apk add --no-cache ca-certificates git bash && \
    adduser -D -h /home/rysh rysh
COPY --from=build /out/rysh /usr/local/bin/rysh
USER rysh
WORKDIR /home/rysh
ENV TERM=xterm-256color SHELL=/bin/bash
ENTRYPOINT ["/usr/local/bin/rysh"]
