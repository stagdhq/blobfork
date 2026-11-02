# blobfork

blobfork forks a blob container: a copy-on-write branch of your Azure Blob Storage container (and later an S3 bucket), as Neon branches a Postgres database.

Your app gets a container of its own that already holds every file of the real one. It reads them, writes new ones, overwrites and deletes, and the real container never changes: what the app does stays in its fork, and goes when the fork does. Nothing is copied up front, so a fork of a container with a terabyte in it starts as fast as an empty one.

It's made for preview environments. A preview with a branch of your staging database has rows pointing at files in staging's storage: without those files its images and attachments are broken, and with staging's storage writable, the preview writes into staging. With blobfork, the preview sees staging's files and can't touch them.

> **Status:** being designed; nothing to run yet. Azure Blob Storage comes first, S3 after.

## How it works

blobfork is a small proxy in front of [Azurite](https://github.com/Azure/Azurite), the Azure Storage emulator, with two layers behind it:

- **The fork,** in Azurite: every write goes here.
- **The real container,** read-only: a read the fork can't answer falls through to it.

Your app talks to blobfork as it talks to Azure, with the Azure SDK it already uses and a connection string pointing at blobfork. Azurite does the hard parts of Azure's API (Shared Key and SAS signing, block uploads, properties and metadata), so blobfork passes most requests through unchanged and steps in only for:

- **Reads** (Get Blob, Get Blob Properties) of a file the fork hasn't got: from the real container, ranges included.
- **Deletes** of a file that's only in the real container: a marker in the fork hides it, as overlayfs does.
- **Listings** (List Blobs): both layers asked the same and merged by name, the fork's copy winning and deleted files left out, paged with a marker of blobfork's own.
- **Copies** whose source is in the real container: the file copied into the fork first.

SAS URLs your app hands a browser, for direct uploads and downloads, point at blobfork and work as they do against Azure.

The real container is read with a SAS that can only read and list, so the fork can't write to it, whatever the app asks.

## Running it

On its own, beside your app, with a read-only SAS for the container to fork:

```yaml
services:
  blobs:
    image: ghcr.io/stagdhq/blobfork
    environment:
      BLOBFORK_FROM: https://acmestaging.blob.core.windows.net/files?sv=…&sp=rl&sig=…
  app:
    build: .
    environment:
      AZURE_STORAGE_CONNECTION_STRING: DefaultEndpointsProtocol=http;AccountName=blobfork;AccountKey=…;BlobEndpoint=http://blobs:10000/blobfork
```

The container in the fork keeps the real one's name, so your app's settings work unchanged.

In a [Stagd](https://stagd.dev) preview, `stagd.yaml` asks for it, and Stagd fills in your app's connection string:

```yaml
services:
  azure-blob:
    from: [staging-files]
```

There the real container is read through Stagd, so its SAS never reaches the preview.

The settings above are the plan, and may change before the first release.

## Later

- **S3 over S3:** the same, with an S3 emulator as the fork and an S3 bucket (AWS, R2, MinIO, …) as the real one.
- **A copy of the real container at the start,** for small ones: no fall-through, and a snapshot from one moment.
