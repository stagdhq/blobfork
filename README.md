# blobfork

blobfork forks a blob container: a copy-on-write branch of your Azure Blob Storage container (and later an S3 bucket), as Neon branches a Postgres database.

Your app gets a container of its own that already holds every file of the real one. It reads them, writes new ones, overwrites and deletes, and the real container never changes: what the app does stays in its fork, and goes when the fork does. Nothing is copied up front, so a fork of a container with a terabyte in it starts as fast as an empty one.

It's made for preview environments. A preview with a branch of your staging database has rows pointing at files in staging's storage: without those files its images and attachments are broken, and with staging's storage writable, the preview writes into staging. With blobfork, the preview sees staging's files and can't touch them.

> **Status:** a first working version, for Azure Blob Storage; no release or published image yet. S3 comes after.

## How it works

blobfork is a small proxy in front of [Azurite](https://github.com/Azure/Azurite), the Azure Storage emulator, with two layers behind it:

- **The fork,** in Azurite: every write goes here.
- **The real container,** read-only: a read the fork can't answer falls through to it.

Your app talks to blobfork as it talks to Azure, with the Azure SDK it already uses and a connection string pointing at blobfork. Azurite does the hard parts of Azure's API (Shared Key and SAS signing, block uploads, properties and metadata), so blobfork passes most requests through unchanged and steps in only for:

- **Reads** (Get Blob, Get Blob Properties) of a file the fork hasn't got: from the real container, ranges included.
- **Deletes** of a file that's only in the real container: a marker in the fork hides it, as overlayfs does.
- **Listings** (List Blobs): both layers asked the same and merged by name, the fork's copy winning and deleted files left out, paged with a marker of blobfork's own.
- **Copies** whose source is in the real container (Copy Blob, Put Blob and Put Block From URL): the file copied into the fork first.
- **Writes that change a file in place** (metadata, properties, tags, tier, leases, snapshots) of a file only in the real container: the file copied into the fork first, too.

SAS URLs your app hands a browser, for direct uploads and downloads, point at blobfork and work as they do against Azure.

The real container is read with a SAS that can only read and list, so the fork can't write to it, whatever the app asks.

Delete markers are empty blobs in a container of the fork's own, `blobfork-deleted`, which blobfork leaves out of List Containers; they last as long as Azurite's data does.

## Running it

On its own, beside your app, with a read-only SAS for the container to fork. Until the image is published, build it with `docker build -t blobfork .` and use `image: blobfork`.

```yaml
services:
  blobs:
    image: ghcr.io/stagdhq/blobfork
    environment:
      BLOBFORK_FROM: https://acmestaging.blob.core.windows.net/files?sv=…&sp=rl&sig=…
  app:
    build: .
    environment:
      AZURE_STORAGE_CONNECTION_STRING: DefaultEndpointsProtocol=http;AccountName=blobfork;AccountKey=Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw==;BlobEndpoint=http://blobs:10000/blobfork
```

The container in the fork keeps the real one's name, so your app's settings work unchanged. The account key is Azurite's well-known one unless you set `BLOBFORK_KEY`: the fork holds only what the app writes, and the real container's SAS never leaves blobfork. In `docker compose`, write each `$` in the SAS as `$$`.

The image runs Azurite inside, on 10010, with blobfork in front of it on 10000; Azurite keeps its data in `/data`.

### Settings

`blobfork serve` takes each as a flag or from its variable:

| Flag | Variable | Default | |
|---|---|---|---|
| `--from` | `BLOBFORK_FROM` | | The real container's URL with a SAS that can read and list (`sp=rl`). Required. |
| `--account` | `BLOBFORK_ACCOUNT` | `blobfork` | The account the app uses, `AccountName` in its connection string. |
| `--key` | `BLOBFORK_KEY` | Azurite's well-known key | Its key, `AccountKey`. |
| `--container` | `BLOBFORK_CONTAINER` | the real one's name | The forked container's name. |
| `--listen` | `BLOBFORK_LISTEN` | `:10000` | Where the app reaches blobfork. |
| `--azurite` | `BLOBFORK_AZURITE` | `http://127.0.0.1:10010` | Azurite's blob endpoint, without the account. |
| `--log-level` | `BLOBFORK_LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error`. |

`blobfork check --from <url>` lists the real container and reads a blob of it, and warns if the SAS could write. `blobfork version` prints the version.

Run on its own, outside the image, blobfork needs an Azurite started with the account (`AZURITE_ACCOUNTS=blobfork:<key>`), `--skipApiVersionCheck` (blobfork passes the app's API version on) and `--disableProductStyleUrl` (so a host like `blob--pr-1.example.com` isn't read as an account).

In a [Stagd](https://stagd.dev) preview, `stagd.yaml` asks for it, and Stagd fills in your app's connection string:

```yaml
services:
  azure-blob:
    from: [staging-files]
```

There the real container is read through Stagd, so its SAS never reaches the preview.

The settings above may change before the first release.

## What it leaves out, for now

- **Snapshots, versions and soft-deleted blobs** of the real container: a listing that asks for them (`include=snapshots,versions,deleted`) gets the current blobs only, from either layer. Reads by `snapshot` or `versionid` go to the fork alone.
- **Page and append blobs** of the real container are read and listed, but not copied up: copies of them, and writes to them in place, answer 404.
- **A Put Block List that keeps the real blob's committed blocks:** the blocks aren't there to keep. A list of new blocks works.
- **Blob batches** (`comp=batch`): a delete in a batch doesn't leave a marker.
- **Conditions on a delete** (`If-Match` and the rest) of a file only in the real container aren't checked, and `x-ms-delete-snapshots` is taken as a plain delete.
- **Get Block List and Get Blob Tags** of a file only in the real container answer 404.
- **Put Block From URL:** blobfork copies the source up, but Azurite (3.35) doesn't do the rest and answers 501.
- **Copy-ups and the copy-up check** happen before Azurite checks the request's key: a request without one can't read anything, but can have a file of the real container copied into the fork.
- **Names compare by byte:** for names outside the Basic Multilingual Plane, Azurite's order may differ, and a listing of such names may page out of order.
- **Fall through every time:** a read the fork can't answer goes to the real container each time, rather than copying the file up on the first read.

## Later

- **S3 over S3:** the same, with an S3 emulator as the fork and an S3 bucket (AWS, R2, MinIO, …) as the real one.
- **A copy of the real container at the start,** for small ones: no fall-through, and a snapshot from one moment.
- **The real container read through Stagd,** in a preview, so its SAS stays in Stagd: another lower layer (`azure.Layer`), on a Stagd API to come.
- **A front over another kind** (S3 over Azure, Azure over S3): possible with a lower layer that translates, but not planned; each front sits over its own kind, so nothing is translated.

## Developing

```bash
go test ./...
```

The tests run the Azure SDK against blobfork in front of Azurite, which they start with Docker (`mcr.microsoft.com/azure-storage/azurite:3.35.0`), with a second account in it holding the real container. `BLOBFORK_TEST_AZURITE` points them at a running Azurite instead.
