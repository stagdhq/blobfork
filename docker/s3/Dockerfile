# blobfork's S3 front beside Floci, in one image: Floci listens inside on
# 4567, checking signatures, and blobfork in front of it on 4566.
#
#   docker build -f docker/s3/Dockerfile -t blobfork-s3 .

# The build runs on the builder's platform and cross-compiles for the image's.
FROM --platform=$BUILDPLATFORM golang:1.27.1-alpine AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -ldflags "-s -w -X main.version=${VERSION}" -o /blobfork ./cmd/blobfork

FROM floci/floci:2.4.0

COPY --from=build /blobfork /usr/local/bin/blobfork
COPY docker/s3/entrypoint.sh /usr/local/bin/blobfork-entrypoint
COPY LICENSE /usr/share/licenses/blobfork/LICENSE
COPY docker/licenses/floci.txt /usr/share/licenses/floci/LICENSE

EXPOSE 4566

# Up once blobfork serves and its emulator answers.
HEALTHCHECK --interval=5s --timeout=5s --start-period=60s --retries=3 CMD ["blobfork", "s3", "health"]

ENTRYPOINT ["blobfork-entrypoint"]
CMD []
