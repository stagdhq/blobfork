# blobfork's Azure front beside Azurite, in one image: Azurite listens inside
# on 10010, blobfork in front of it on 10000.
#
#   docker build -f docker/azure/Dockerfile -t blobfork-azure .

# The build runs on the builder's platform and cross-compiles for the image's.
FROM --platform=$BUILDPLATFORM golang:1.27.1-alpine AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -ldflags "-s -w -X main.version=${VERSION}" -o /blobfork ./cmd/blobfork

FROM mcr.microsoft.com/azure-storage/azurite:3.35.0

COPY --from=build /blobfork /usr/local/bin/blobfork
COPY docker/azure/entrypoint.sh /usr/local/bin/blobfork-entrypoint
COPY LICENSE /usr/share/licenses/blobfork/LICENSE

VOLUME /data
EXPOSE 10000

# Up once blobfork serves and its emulator answers.
HEALTHCHECK --interval=5s --timeout=5s --start-period=60s --retries=3 CMD ["blobfork", "azure", "health"]

ENTRYPOINT ["blobfork-entrypoint"]
