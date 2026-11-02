# blobfork

blobfork forks a blob container: a copy-on-write branch of your Azure Blob Storage container or S3 bucket, as Neon branches a Postgres database.

Your app gets a container of its own that already holds every file of the real one. It reads them, writes new ones, overwrites and deletes, and the real container never changes: what the app does stays in its fork, and goes when the fork does. Nothing is copied up front, so a fork of a container with a terabyte in it starts as fast as an empty one.

It's made for preview environments. A preview with a branch of your staging database has rows pointing at files in staging's storage: without those files its images and attachments are broken, and with staging's storage writable, the preview writes into staging. With blobfork, the preview sees staging's files and can't touch them.

> **Status:** a first working version, for Azure Blob Storage and S3; no release or published image yet.

## How it works

blobfork is a small proxy in front of an emulator, with two layers behind it:

- **The fork,** in the emulator: every write goes here. [Azurite](https://github.com/Azure/Azurite) for Azure, [Floci](https://floci.io) for S3.
- **The real container or bucket,** read-only: a read the fork can't answer falls through to it.

Each front sits over its own kind: an Azure container under the Azure front, an S3 bucket (AWS, R2, MinIO, …) under the S3 one. Nothing is translated.

Your app talks to blobfork as it talks to Azure or S3, with the SDK it already uses pointed at blobfork. The emulator does the hard parts of the API (signatures, block and multipart uploads, properties and metadata), so blobfork passes most requests through unchanged and steps in only for:

- **Reads** (Get Blob and Get Blob Properties; GetObject and HeadObject) of a file the fork hasn't got: from the real one, ranges included.
- **Deletes** of a file that's only in the real one (DeleteObjects too): a marker in the fork hides it, as overlayfs does.
- **Listings** (List Blobs; ListObjects and ListObjectsV2): both layers asked the same and merged by name, the fork's copy winning and deleted files left out, paged with a marker of blobfork's own.
- **Copies** whose source is in the real one (Copy Blob, Put Blob and Put Block From URL; CopyObject and UploadPartCopy): the file copied into the fork first.
- **Writes that change a file in place** of a file only in the real one (metadata, properties, tags, tier, leases and snapshots; tagging, ACLs, retention and legal hold): the file copied into the fork first, too.

SAS URLs and presigned URLs your app hands a browser, for direct uploads and downloads, point at blobfork and work as they do against Azure and S3.

The real container is read with a SAS that can only read and list, and the real bucket with a key that should only read and list, so the fork can't write to it, whatever the app asks.

blobfork answers from the real one only for a request the emulator has checked: the emulator's 404 means the app's key or signature was good. Floci takes unsigned requests, so blobfork refuses them before they reach it.

Delete markers are empty files in a container or bucket of the fork's own, `blobfork-deleted`, which blobfork leaves out of List Containers and ListBuckets; they last as long as the emulator's data does.

## Running it

Each front has an image with its emulator inside. Until they're published, build them from this repository:

```bash
docker build -f docker/azure/Dockerfile -t blobfork-azure .
```

```bash
docker build -f docker/s3/Dockerfile -t blobfork-s3 .
```

### Azure

On its own, beside your app, with a read-only SAS for the container to fork:

```yaml
services:
  blobs:
    image: ghcr.io/stagdhq/blobfork-azure
    environment:
      BLOBFORK_FROM: https://acmestaging.blob.core.windows.net/files?sv=…&sp=rl&sig=…
  app:
    build: .
    environment:
      AZURE_STORAGE_CONNECTION_STRING: DefaultEndpointsProtocol=http;AccountName=blobfork;AccountKey=Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw==;BlobEndpoint=http://blobs:10000/blobfork
```

The container in the fork keeps the real one's name, so your app's settings work unchanged. The account key is Azurite's well-known one unless you set `BLOBFORK_KEY`: the fork holds only what the app writes, and the real container's SAS never leaves blobfork. In `docker compose`, write each `$` in the SAS as `$$`.

The image runs Azurite inside, on 10010, with blobfork in front of it on 10000; Azurite keeps its data in `/data`.

`blobfork azure serve` takes each setting as a flag or from its variable:

| Flag | Variable | Default | |
|---|---|---|---|
| `--from` | `BLOBFORK_FROM` | | The real container's URL with a SAS that can read and list (`sp=rl`). Required. |
| `--account` | `BLOBFORK_ACCOUNT` | `blobfork` | The account the app uses, `AccountName` in its connection string. |
| `--key` | `BLOBFORK_KEY` | Azurite's well-known key | Its key, `AccountKey`. |
| `--container` | `BLOBFORK_CONTAINER` | the real one's name | The forked container's name. |
| `--listen` | `BLOBFORK_LISTEN` | `:10000` | Where the app reaches blobfork. |
| `--azurite` | `BLOBFORK_AZURITE` | `http://127.0.0.1:10010` | Azurite's blob endpoint, without the account. |
| `--log-level` | `BLOBFORK_LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error`. |

`blobfork azure check --from <url>` lists the real container and reads a blob of it, and warns if the SAS could write.

Run outside the image, blobfork needs an Azurite started with the account (`AZURITE_ACCOUNTS=blobfork:<key>`), `--skipApiVersionCheck` (blobfork passes the app's API version on) and `--disableProductStyleUrl` (so a host like `blob--pr-1.example.com` isn't read as an account).

### S3

On its own, beside your app, with a key that can only read and list the bucket to fork (`s3:GetObject`, `s3:ListBucket`):

```yaml
services:
  blobs:
    image: ghcr.io/stagdhq/blobfork-s3
    environment:
      BLOBFORK_FROM: s3://acme-staging-uploads
      BLOBFORK_FROM_REGION: eu-north-1
      BLOBFORK_FROM_ACCESS_KEY_ID: AKIA…
      BLOBFORK_FROM_SECRET_ACCESS_KEY: …
  app:
    build: .
    environment:
      AWS_ENDPOINT_URL_S3: http://blobs:4566
      AWS_ACCESS_KEY_ID: test
      AWS_SECRET_ACCESS_KEY: test
      AWS_REGION: us-east-1
```

The bucket in the fork keeps the real one's name. The app signs with Floci's keys, `test` and `test`, and addresses the bucket in the path (`forcePathStyle: true`, `UsePathStyle`, `s3ForcePathStyle` or the like in its SDK): blobfork answers at one host, not one per bucket. The real bucket's key never leaves blobfork.

The image runs Floci inside, on 4567 and checking signatures, with blobfork in front of it on 4566. Floci keeps its data in memory unless you set `FLOCI_STORAGE_MODE` (`persistent` keeps it in `/app/data`).

`blobfork s3 serve` takes each setting as a flag or from its variable:

| Flag | Variable | Default | |
|---|---|---|---|
| `--from` | `BLOBFORK_FROM` | | The real bucket, `s3://<bucket>`. Required. |
| `--from-endpoint` | `BLOBFORK_FROM_ENDPOINT` | AWS's | Its S3 endpoint, for R2, MinIO and the like. |
| `--from-region` | `BLOBFORK_FROM_REGION` | `us-east-1` | Its region. |
| `--from-path-style` | `BLOBFORK_FROM_PATH_STYLE` | with `--from-endpoint` | Address it in the path, not the host. |
| `--from-access-key-id` | `BLOBFORK_FROM_ACCESS_KEY_ID` | | A key that can read and list it, and nothing more. Required. |
| `--from-secret-access-key` | `BLOBFORK_FROM_SECRET_ACCESS_KEY` | | Its secret. Required. |
| `--from-session-token` | `BLOBFORK_FROM_SESSION_TOKEN` | | Its session token, for temporary credentials. |
| `--bucket` | `BLOBFORK_BUCKET` | the real one's name | The forked bucket's name. |
| `--access-key-id` | `BLOBFORK_ACCESS_KEY_ID` | `test` | The key the app signs with, Floci's. |
| `--secret-access-key` | `BLOBFORK_SECRET_ACCESS_KEY` | `test` | Its secret. |
| `--region` | `BLOBFORK_REGION` | `us-east-1` | The region the app signs for. |
| `--listen` | `BLOBFORK_LISTEN` | `:4566` | Where the app reaches blobfork. |
| `--floci` | `BLOBFORK_FLOCI` | `http://127.0.0.1:4567` | Floci's endpoint. |
| `--log-level` | `BLOBFORK_LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error`. |

`blobfork s3 check` takes the `--from` flags, lists the real bucket and reads an object of it. It can't tell whether the key could also write: that's for the key's policy.

Run outside the image, blobfork needs a Floci that checks signatures (`FLOCI_AUTH_VALIDATE_SIGNATURES=true`) and that only blobfork can reach.

### In a Stagd preview

`stagd.yaml` asks for it, and Stagd fills in your app's settings:

```yaml
services:
  azure-blob:
    from: [staging-files]
  s3:
    from: [staging-uploads]
```

There the real container or bucket is read through Stagd, so its SAS or key never reaches the preview.

The settings above may change before the first release. `blobfork version` prints the version.

## What it leaves out, for now

Both fronts:

- **Copy-ups and the copy-up check** happen before the emulator checks the request's key: a request without a good one can't read anything, but can have a file of the real one copied into the fork.
- **Names compare by byte:** for names outside the Basic Multilingual Plane, the emulator's order may differ, and a listing of such names may page out of order.
- **Fall through every time:** a read the fork can't answer goes to the real one each time, rather than copying the file up on the first read.

Azure:

- **Snapshots, versions and soft-deleted blobs** of the real container: a listing that asks for them (`include=snapshots,versions,deleted`) gets the current blobs only, from either layer. Reads by `snapshot` or `versionid` go to the fork alone.
- **Page and append blobs** of the real container are read and listed, but not copied up: copies of them, and writes to them in place, answer 404.
- **A Put Block List that keeps the real blob's committed blocks:** the blocks aren't there to keep. A list of new blocks works.
- **Blob batches** (`comp=batch`): a delete in a batch doesn't leave a marker.
- **Conditions on a delete** (`If-Match` and the rest) of a file only in the real container aren't checked, and `x-ms-delete-snapshots` is taken as a plain delete.
- **Get Block List and Get Blob Tags** of a file only in the real container answer 404.
- **Put Block From URL:** blobfork copies the source up, but Azurite (3.35) doesn't do the rest and answers 501.

S3:

- **Virtual-hosted-style requests** (`<bucket>.<host>`) to blobfork: path style only.
- **Unsigned requests** are refused, so a public-read bucket policy in the fork does nothing; a browser's CORS preflight and a POST form upload pass. Signature version 2 is refused too.
- **Versions of the real bucket:** reads by `versionId` and ListObjectVersions go to the fork alone, as does ListMultipartUploads.
- **GetObjectTagging, GetObjectAcl and GetObjectAttributes** of an object only in the real bucket answer 404.
- **Copy-ups keep** the object's body, content headers and metadata, not its tags, storage class or encryption settings, and are one PUT: an object over 5 GB isn't copied up.
- **Conditions on a delete** of an object only in the real bucket aren't checked.

## Later

- **A copy of the real container at the start,** for small ones: no fall-through, and a snapshot from one moment.
- **The real container or bucket read through Stagd,** in a preview, so its SAS or key stays in Stagd: another lower layer (`azure.Layer`, `s3.Layer`), on a Stagd API to come.
- **A front over another kind** (S3 over Azure, Azure over S3): possible with a lower layer that translates, but not planned.

## Developing

```bash
go test ./...
```

The tests run the Azure SDK and the AWS SDK against blobfork in front of the emulators, which they start with Docker: one Azurite (`mcr.microsoft.com/azure-storage/azurite:3.35.0`) with a second account in it holding the real container, and two Flocis (`floci/floci:2.4.0`), one holding the real bucket. `BLOBFORK_TEST_AZURITE`, `BLOBFORK_TEST_FLOCI` and `BLOBFORK_TEST_FLOCI_LAB` point them at running ones instead.

## License

blobfork is licensed under the [Apache License 2.0](LICENSE). The images also hold Azurite and Floci, each under its own MIT license: Azurite's in `/opt/azurite/LICENSE`, Floci's in `/usr/share/licenses/floci/LICENSE`.
