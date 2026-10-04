# Build context: services/ (the gateway imports the limiter library of ratelimiter-go through a go.mod replace).
#   docker build -f gateway/Dockerfile services
FROM golang:1.27-alpine AS build
WORKDIR /src
# Dependencies first, so they are cached until a go.mod changes.
COPY ratelimiter-go/go.mod ratelimiter-go/go.sum ratelimiter-go/
COPY gateway/go.mod gateway/go.sum gateway/
WORKDIR /src/gateway
RUN go mod download
WORKDIR /src
COPY ratelimiter-go/ ratelimiter-go/
COPY gateway/ gateway/
WORKDIR /src/gateway
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/app ./cmd/gateway

FROM alpine:3.22
# Non-root user; busybox wget is used by the container healthcheck.
RUN adduser -D -u 10001 app
COPY --from=build /out/app /app
USER 10001
ENV PORT=8080
EXPOSE 8080
ENTRYPOINT ["/app"]
