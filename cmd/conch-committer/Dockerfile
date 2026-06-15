# conch-committer: drop-in replacement for OpenSandbox's image-committer.
# Built so the binary lives at /usr/local/bin/image-committer — the path the
# OpenSandbox SandboxSnapshotController hardcodes as the Job Command. Deploy via:
#   --image-committer-image=<this image>
#   --containerd-socket-path=<conchd's unix socket path>   (mounted into the Job)
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/image-committer ./cmd/conch-committer

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/image-committer /usr/local/bin/image-committer
ENTRYPOINT ["/usr/local/bin/image-committer"]
