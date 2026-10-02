# ursa-bifrost hosted MCP server (bifrost serve) for Cloud Run.
# Static Go binary on a distroless base: no shell, no package manager.
FROM golang:1.26.8 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X github.com/UCR-Research-Computing/ursa-bifrost/internal/version.Version=${VERSION}" \
      -o /out/bifrost ./cmd/bifrost

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/bifrost /bifrost
USER nonroot:nonroot
ENV PORT=8080
EXPOSE 8080
ENTRYPOINT ["/bifrost", "serve", "--config", "/config/config.yaml"]
