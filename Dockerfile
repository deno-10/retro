FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /retro .

FROM alpine:3.20
RUN adduser -D -u 10001 retro && mkdir -p /app/data && chown retro:retro /app/data
WORKDIR /app
COPY --from=build /retro /app/retro
USER retro
VOLUME ["/app/data"]
EXPOSE 8080
ENTRYPOINT ["/app/retro", "-addr", ":8080", "-data", "/app/data/boards.json"]
