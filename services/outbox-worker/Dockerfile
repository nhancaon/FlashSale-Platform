# Build context: services/outbox-worker
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/app ./cmd/outbox-worker

FROM alpine:3.24
# Non-root user; busybox wget is used by the container healthcheck.
RUN adduser -D -u 10001 app
COPY --from=build /out/app /app
USER 10001
ENV PORT=8080
EXPOSE 8080
ENTRYPOINT ["/app"]
