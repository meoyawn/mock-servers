FROM golang:1.27.0-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -buildid=" -o /youtube-web-mock ./cmd/youtube-web-mock

# The mock probes its media with ffprobe at request time. The media itself is
# generated outside the image and mounted read-only at /fixtures.
FROM alpine:3.22

RUN apk add --no-cache ffmpeg
COPY --from=build /youtube-web-mock /usr/local/bin/youtube-web-mock
ENV YOUTUBE_WEB_MOCK_ADDRESS=0.0.0.0:3128 \
    YOUTUBE_WEB_MOCK_CA_FILE=/ca/youtube-web-mock-ca.pem \
    YOUTUBE_WEB_MOCK_FIXTURE_DIRECTORY=/fixtures/youtube
EXPOSE 3128
ENTRYPOINT ["youtube-web-mock"]
