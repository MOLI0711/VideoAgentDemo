ARG GO_IMAGE=golang:1.27.1-bookworm
ARG RUNTIME_IMAGE=debian:bookworm-slim
FROM ${GO_IMAGE} AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY migrations ./migrations
RUN --mount=type=cache,target=/root/.cache/go-build CGO_ENABLED=0 go build -p 2 -trimpath -o /out/api ./cmd/api && \
    CGO_ENABLED=0 go build -p 2 -trimpath -o /out/worker ./cmd/worker && \
    CGO_ENABLED=0 go build -p 2 -trimpath -o /out/coordinator ./cmd/coordinator && \
    CGO_ENABLED=0 go build -p 2 -trimpath -o /out/devtool ./cmd/devtool
FROM ${RUNTIME_IMAGE}
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates ffmpeg tesseract-ocr tesseract-ocr-chi-sim && \
    rm -rf /var/lib/apt/lists/* && useradd --create-home --uid 10001 app
COPY --from=build /out/ /usr/local/bin/
COPY LICENSE /usr/share/doc/vedioagent/LICENSE
WORKDIR /app
RUN mkdir -p /app/.runtime/media && chown -R app:app /app
USER app
ENTRYPOINT ["api"]
CMD ["-config", "/app/configs/local.yaml"]
