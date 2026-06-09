# E2B rootfs example

This directory contains an example rootfs Dockerfile for running an E2B-style
environment under Conch.

## Build

```bash
buildctl build \
  --frontend dockerfile.v0 \
  --local context=. \
  --local dockerfile=. \
  --output type=image,name=localhost:5000/conch/e2b-rootfs:debug,push=true,registry.insecure=true
```

## Debug SSH

To inject a debug SSH public key at build time:

```bash
buildctl build \
  --frontend dockerfile.v0 \
  --local context=. \
  --local dockerfile=. \
  --opt build-arg:DEBUG_SSH_AUTHORIZED_KEY="$(cat ~/.ssh/id_ed25519.pub)" \
  --output type=image,name=localhost:5000/conch/e2b-rootfs:debug,push=true,registry.insecure=true
```
