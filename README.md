# registry-push

A Go CLI tool that pushes container images to OCI-compatible registries using chunked uploads.

## Why

[cloudflare/serverless-registry](https://github.com/cloudflare/serverless-registry) enforce a maximum request body
size (~500 MB), which causes `docker push` to fail for large layers. This tool implements the OCI chunked upload
protocol, splitting layers into configurable chunks and uploading them sequentially. It replaces the original
TypeScript [push](https://github.com/cloudflare/serverless-registry/tree/main/push) tool with a faster, more portable Go
implementation.

## Installation

### From source

```
go install github.com/mkuznets/registry-push@latest
```

### From release binaries

Download a pre-built binary from the [GitHub Releases](https://github.com/mkuznets/registry-push/releases) page.
Binaries are available for Linux and macOS on both amd64 and arm64.

### Build locally

```
git clone https://github.com/mkuznets/registry-push.git
cd registry-push
make build
# Binary is at ./bin/registry-push
```

## Usage

```
registry-push [OPTIONS] <source> <destination>
```

### Source formats

Images are always resolved locally — either from the Docker daemon or an OCI layout directory on disk.

| Input                          | Source               |
|--------------------------------|----------------------|
| `myimage:latest`               | Docker daemon        |
| `docker.io/library/nginx:1.27` | Docker daemon        |
| `ghcr.io/org/repo:tag`         | Docker daemon        |
| `oci:./my-layout`              | OCI layout directory |

The `oci:` prefix selects an OCI layout directory. Everything else is resolved from the local Docker daemon.

### Destination format

```
host/repository[:tag]
```

Tag defaults to `latest` if omitted.

### Examples

Push from local Docker daemon:

```
registry-push --username user --password pass myimage:latest registry.example.com/repo/myimage:v1
```

Push from an OCI layout directory:

```
registry-push --username user --password pass oci:./my-layout registry.example.com/repo/myimage:v1
```

Custom gzip level (default is 9):

```
registry-push --gzip-level 6 --username user --password pass myimage:latest registry.example.com/repo/myimage:v1
```

Skip recompression (use original layer compression):

```
registry-push --no-recompress --username user --password pass myimage:latest registry.example.com/repo/myimage:v1
```

## Configuration

### Flags

| Flag            | Default    | Description                                                                                   |
|-----------------|------------|-----------------------------------------------------------------------------------------------|
| `--chunk-size`  | `0`        | Max chunk size in bytes. `0` means use the registry's advertised maximum (or 50 MB fallback). |
| `--concurrency` | `5`        | Number of parallel layer uploads.                                                             |
| `--gzip-level`  | `9`        | Gzip compression level (1-9) for layers.                                                      |
| `--insecure`    | `false`    | Use plain HTTP instead of HTTPS.                                                              |
| `--username`    | (required) | Registry username.                                                                            |
| `--password`    | (required) | Registry password.                                                                            |

### Environment variables

| Variable            | Equivalent flag |
|---------------------|-----------------|
| `REGISTRY_USERNAME` | `--username`    |
| `REGISTRY_PASSWORD` | `--password`    |

## How it works

For each layer in the image:

1. `HEAD /v2/<repo>/blobs/<digest>` -- check if the blob already exists (skip if 200).
2. `POST /v2/<repo>/blobs/uploads/` -- initiate an upload session. The registry returns a `docker-upload-uuid` and
   optionally an `OCI-Chunk-Max-Length` header advertising its maximum chunk size.
3. `PATCH <location>` -- upload chunks sequentially with `Content-Range` headers. Each response returns a `Location` for
   the next request.
4. `PUT <location>?digest=sha256:<hex>` -- finalize the upload.

Layers and the config blob are uploaded concurrently (controlled by `--concurrency`), each with its own progress bar.
After all blobs are uploaded, the OCI manifest is assembled and pushed via `PUT /v2/<repo>/manifests/<tag>`.

Failed requests to the registry are retried up to 3 times with exponential backoff (1s, 2s, 4s) on 5xx/network errors.
Client errors (4xx) fail immediately.

## License

[MIT](LICENSE)
