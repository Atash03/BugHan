# Build stage — static binary, no CGO.
FROM golang:1.25-alpine AS build
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/bughan ./cmd/bughan

# Run stage.
FROM alpine:3.21
RUN addgroup -S bughan && adduser -S bughan -G bughan
COPY --from=build /out/bughan /usr/local/bin/bughan
USER bughan
EXPOSE 8000
HEALTHCHECK --interval=30s --timeout=3s --retries=5 \
  CMD wget -qO- http://127.0.0.1:8000/api/health/ || exit 1
ENTRYPOINT ["bughan"]
CMD ["serve"]
