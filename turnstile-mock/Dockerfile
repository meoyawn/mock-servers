FROM golang:1.27.0-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -buildid=" -o /turnstile-mock ./cmd/turnstile-mock

FROM scratch

COPY --from=build /turnstile-mock /turnstile-mock
EXPOSE 8085
ENTRYPOINT ["/turnstile-mock"]
