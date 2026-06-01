# ── build ────────────────────────────────────────────────────────────────────
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY main.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /flight-api .

# ── runtime ──────────────────────────────────────────────────────────────────
# distroless/static já inclui ca-certificates (necessário p/ o HTTPS do adsbdb)
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /flight-api /flight-api
EXPOSE 8890
ENTRYPOINT ["/flight-api"]
