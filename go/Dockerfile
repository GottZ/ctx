FROM golang:1.24-alpine AS build
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /ctx ./cmd/ctxd

FROM gcr.io/distroless/static-debian12
COPY --from=build /ctx /ctx
ENV GOMEMLIMIT=48GiB
ENV GOGC=100
EXPOSE 8080
ENTRYPOINT ["/ctx"]
