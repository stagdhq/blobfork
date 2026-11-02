# blobfork beside Azurite, in one image: Azurite listens inside on 10010,
# blobfork in front of it on 10000.
FROM golang:1.27.1-alpine AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
RUN CGO_ENABLED=0 go build -ldflags "-s -w -X main.version=${VERSION}" -o /blobfork ./cmd/blobfork

FROM mcr.microsoft.com/azure-storage/azurite:3.35.0

COPY --from=build /blobfork /usr/local/bin/blobfork
COPY image/entrypoint.sh /usr/local/bin/blobfork-entrypoint

VOLUME /data
EXPOSE 10000

ENTRYPOINT ["blobfork-entrypoint"]
