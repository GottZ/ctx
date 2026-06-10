# Stage 0: Frontend (Svelte 5 + TS + Vite, bun) — produces web/dist for go:embed.
FROM oven/bun:1.3-alpine AS frontend
WORKDIR /web
# Dependency layer (cache cut like go.mod/go.sum below):
COPY web/package.json web/bun.lock ./
RUN bun install --frozen-lockfile
COPY web/ .
RUN bun run check && bun run build

FROM golang:1.26-alpine AS build
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Real assets ON TOP of the committed placeholder — must happen BEFORE go
# build, go:embed resolves at compile time (web/web.go: //go:embed all:dist).
COPY --from=frontend /web/dist ./web/dist
RUN CGO_ENABLED=0 go build -o /ctx ./cmd/ctxd

FROM gcr.io/distroless/static-debian12
COPY --from=build /ctx /ctx
ENV GOMEMLIMIT=200MiB
ENV GOGC=100
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/ctx"]
