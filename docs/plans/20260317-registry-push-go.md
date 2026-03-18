# Rewrite push tool in Go (`registry-push`)

## Overview
- Replace the Bun/TypeScript `push/` tool with a Go binary that pushes container images to the serverless-registry
- Solves: Workers have a max request body size (~500MB), so `docker push` fails for large layers. This tool chunks uploads using the OCI chunked upload protocol. The current TS tool requires `docker save` (slow, disk-heavy) and Bun-specific workarounds
- Key improvements: direct Docker daemon access (no `docker save`), parallel gzip via pgzip, configurable chunking, multi-source support (daemon, remote registry, OCI layout), progress bars

## Context (from brainstorm)
- Files/components: flat structure in repo root, module `github.com/mkuznets/registry-push`, binary name `registry-push`
- Dependencies: `go-containerregistry`, `go-flags`, `klauspost/pgzip`, `vbauerber/mpb`
- Registry protocol: OCI Distribution Spec with chunked uploads via `PATCH` + `Content-Range`
- Existing push tool: `push/index.ts` (410 lines) — reference implementation for upload protocol

## Development Approach
- **Testing approach**: Regular (code first, then tests)
- Complete each task fully before moving to the next
- Make small, focused changes
- **CRITICAL: every task MUST include new/updated tests** for code changes in that task
- **CRITICAL: all tests must pass before starting next task**
- **CRITICAL: update this plan file when scope changes during implementation**

## Testing Strategy
- **Unit tests**: required for every task
- Focus on: source resolution, compression logic, chunked upload protocol, retry logic
- Use `github.com/stretchr/testify` for assertions
- Use `net/http/httptest` for upload protocol tests

## Progress Tracking
- Mark completed items with `[x]` immediately when done
- Add newly discovered tasks with + prefix
- Document issues/blockers with warning prefix
- Update plan if implementation deviates from original scope

## Implementation Steps

### Task 1: Project scaffold and CLI parsing

**Files:**
- Create: `main.go`
- Create: `go.mod`
- Create: `LICENSE`

- [x] Add MIT license file
- [x] Initialize Go module (`go mod init github.com/mkuznets/registry-push`)
- [x] Define `Options` struct with `go-flags` tags: `--chunk-size`, `--concurrency`, `--gzip-level`, `--recompress`, `--username`/`REGISTRY_USERNAME`, `--password`/`REGISTRY_PASSWORD`, `--insecure` (plain HTTP), positional `source` and `destination` args
- [x] Parse destination into host + repository path + tag (handle `host/path:tag` format)
- [x] Add basic `main()` that parses args, validates, and prints parsed config (stub for now)
- [x] Run `go mod tidy`, verify it builds

### Task 2: Makefile and .gitignore

**Files:**
- Create: `Makefile`
- Create/update: `.gitignore`

- [x] Add `.gitignore` with `bin/` directory and other Go artifacts
- [x] Create `Makefile` with targets:
  - `build`: compile to `./bin/registry-push`
  - `test`: run `go test ./...`
  - `lint`: run `golangci-lint run`
  - `fmt`: run `gofumpt -w .` and `goimports -w .`
  - `tidy`: run `go mod tidy`
  - `all`: run `fmt`, `lint`, `test`, `build` in sequence

### Task 3: golangci-lint configuration

**Files:**
- Create: `.golangci.yml`

- [x] Create strict `.golangci.yml` config enabling linters beyond defaults: `govet`, `errcheck`, `staticcheck`, `unused`, `gosimple`, `ineffassign`, `typecheck`, `gocritic`, `gofumpt`, `goimports`, `misspell`, `prealloc`, `revive`, `unconvert`, `unparam`, `errname`
- [x] Tune settings: set `gofumpt` as formatter, configure `revive` with sensible rules, set appropriate line length
- [x] Run `make lint` — fix any issues in existing code
- [x] Run `make test` — must pass before next task

### Task 4: GitHub Actions CI

**Files:**
- Create: `.github/workflows/ci.yml`

- [x] Create workflow triggered on pull requests (and pushes to `main`)
- [x] Job: `test` — set up Go, run `make test`
- [x] Job: `lint` — set up Go, install `golangci-lint`, run `make lint`
- [x] Use latest stable Go version, cache modules

### Task 5: Source image resolution

**Files:**
- Create: `source.go`
- Create: `source_test.go`

- [ ] Implement `resolveSource(ref string) (v1.Image, error)` that dispatches on prefix:
  - `daemon://` prefix or bare name with no dots in first segment -> Docker daemon via `daemon.Image()`
  - `oci:` prefix -> OCI layout via `layout.Image()`
  - Otherwise (contains dots, looks like a registry reference) -> `remote.Image()`
- [ ] Write tests for prefix detection and dispatch logic (mock or use `crane` helpers)
- [ ] Write tests for error cases (invalid references, missing prefix handling)
- [ ] Run tests - must pass before next task

### Task 6: Compression / passthrough layer wrapper

**Files:**
- Create: `compress.go`
- Create: `compress_test.go`

- [ ] Implement function that takes a `v1.Image` and returns a processed image:
  - If `--recompress` is false: return image as-is (layers are already compressed from source)
  - If `--recompress` is true: wrap each layer to re-compress with `pgzip` at the specified `--gzip-level`
- [ ] Use `mutate.AppendLayers` or custom `v1.Layer` implementation for re-compressed layers that computes digest/diffID/size correctly
- [ ] Write tests: passthrough preserves original layer data
- [ ] Write tests: recompression produces valid gzip at correct level
- [ ] Run tests - must pass before next task

### Task 7: Chunked upload protocol

**Files:**
- Create: `upload.go`
- Create: `upload_test.go`

- [ ] Implement `pushLayer(ctx, client, baseURL, cred, digest, reader, totalSize, chunkSize, bar)`:
  - `HEAD` to check if blob exists (skip if 200)
  - `POST` to initiate upload, read `docker-upload-uuid` and `oci-chunk-max-length` headers
  - Compute effective chunk size: `min(clientChunkSize, registryMax)` (use registry max if client didn't set one)
  - `PATCH` loop: send chunks with `Content-Range` header, follow `Location` header for next URL
  - `PUT` to finalize with `?digest=` parameter
- [ ] Wrap reader with counting writer that updates `mpb` progress bar
- [ ] Implement retry logic: up to 3 attempts with exponential backoff (1s, 2s, 4s) on 5xx/network errors, immediate fail on 4xx
- [ ] Write tests using `httptest.Server` simulating the full upload flow (HEAD -> POST -> PATCH -> PUT)
- [ ] Write tests for retry on 5xx, immediate fail on 4xx, skip on existing blob
- [ ] Run tests - must pass before next task

### Task 8: Push orchestration with progress bars

**Files:**
- Modify: `main.go`
- Modify: `upload.go`

- [ ] Wire everything together in `main()`: resolve source -> process layers -> push concurrently -> push manifest
- [ ] Set up `mpb.Progress` container with per-layer bars showing: short digest, progress percentage, bytes transferred/total, speed
- [ ] Use `errgroup.Group` with `SetLimit(concurrency)` for parallel layer+config uploads
- [ ] Build OCI manifest after all layers uploaded (schema version 2, `application/vnd.oci.image.manifest.v1+json`)
- [ ] `PUT` manifest to `/v2/<repo>/manifests/<tag>`
- [ ] Test full push flow end-to-end with `httptest.Server` (at least one integration-style test)
- [ ] Run tests - must pass before next task

### Task 9: Verify acceptance criteria

- [ ] Verify: source resolution works for daemon, remote, and OCI sources
- [ ] Verify: passthrough mode skips compression, `--recompress` re-compresses at specified level
- [ ] Verify: `--chunk-size` correctly caps chunk size
- [ ] Verify: `--concurrency` controls parallelism
- [ ] Verify: progress bars show per-layer progress with speed
- [ ] Verify: retries work on transient failures
- [ ] Run full test suite: `go test ./...`

### Task 10: GoReleaser and release workflow

**Files:**
- Create: `.goreleaser.yml`
- Create: `.github/workflows/release.yml`

- [ ] Create `.goreleaser.yml` config:
  - Build `registry-push` binary for linux/darwin × amd64/arm64
  - Produce tar.gz archives with binary, LICENSE, README
  - Generate checksums file
- [ ] Create `.github/workflows/release.yml`:
  - Trigger on tag push (`v*`)
  - Use `goreleaser/goreleaser-action` to build and publish GitHub Release
- [ ] Add `dist/` to `.gitignore`

### Task 11: README and documentation

**Files:**
- Create: `README.md`

- [ ] Write README explaining:
  - **Why this tool exists**: Workers have a max request body size, so `docker push` fails for large layers. This tool implements OCI chunked uploads to work around that limitation. Replaces the original TypeScript push tool with a faster, more reliable Go implementation
  - **Installation**: `go build` or `go install`
  - **Usage examples**: daemon source, remote registry source, OCI layout source, with flags
  - **Configuration**: all flags and env vars
  - **How it works**: brief description of the chunked upload flow
- [ ] Move this plan to `docs/plans/completed/`

## Technical Details

### CLI Options struct
```go
type Options struct {
    ChunkSize   int    `long:"chunk-size" description:"Max chunk size in bytes (0 = use registry default)" default:"0"`
    Concurrency int    `long:"concurrency" description:"Parallel layer operations" default:"5"`
    GzipLevel   int    `long:"gzip-level" description:"Gzip level 1-9, used with --recompress" default:"9"`
    Recompress  bool   `long:"recompress" description:"Re-compress layers with pgzip"`
    Insecure    bool   `long:"insecure" description:"Use plain HTTP"`
    Username    string `long:"username" env:"REGISTRY_USERNAME" required:"true"`
    Password    string `long:"password" env:"REGISTRY_PASSWORD" required:"true"`
    Args        struct {
        Source      string `positional-arg-name:"source" required:"true"`
        Destination string `positional-arg-name:"destination" required:"true"`
    } `positional-args:"yes" required:"yes"`
}
```

### Upload protocol flow
```
HEAD /v2/<repo>/blobs/<digest>          -> 200 (skip) or 404 (continue)
POST /v2/<repo>/blobs/uploads/          -> 202 + docker-upload-uuid + oci-chunk-max-length
PATCH <location>                        -> 202 + range + location (repeat for each chunk)
PUT <location>?digest=sha256:<hex>      -> 201 (done)
```

### Source resolution rules
| Input | Source |
|-------|--------|
| `myimage:latest` | Docker daemon |
| `daemon://myimage:v2` | Docker daemon |
| `docker.io/library/nginx:1.27` | Remote registry |
| `ghcr.io/org/repo:tag` | Remote registry |
| `oci:./my-layout` | OCI layout directory |

## Post-Completion

**Manual verification:**
- Push a real multi-layer image to the serverless-registry and verify it pulls correctly with `docker pull`
- Test with large layers (>500MB) to verify chunking works end-to-end
- Compare push speed with the old TypeScript tool
