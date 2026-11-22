FROM golang:1.27.0-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -buildid=" -o /cloudflare-cache ./cmd/cloudflare-cache

FROM scratch

COPY --from=build /cloudflare-cache /cloudflare-cache
EXPOSE 8084
ENTRYPOINT ["/cloudflare-cache"]
