FROM golang:1.27.4-alpine3.24@sha256:0a5f7fe796599ff487605987432e7619ae45beeb2fba4c2ce0b907879932aebf AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /retro .

FROM alpine:3.24.4@sha256:f59abe656b692d901e184be3445e084786602108599d2a51d229c0f5a48c71bf
RUN apk add --no-cache ca-certificates && adduser -D -u 10001 retro && mkdir -p /app/data && chown retro:retro /app/data
WORKDIR /app
COPY --from=build /retro /app/retro
USER retro
VOLUME ["/app/data"]
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 CMD ["/app/retro", "healthcheck"]
ENTRYPOINT ["/app/retro"]
CMD ["-addr", ":8080", "-data", "/app/data/retro.db", "-log-json"]
