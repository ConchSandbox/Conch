# E2B rootfs example

This directory contains an example rootfs Dockerfile for running an E2B-style
environment under Conch.

## Build

From the repository root, enter this directory and run the build. It requires a
running BuildKit daemon, the `buildctl` client, and an insecure registry
listening on `localhost:5000`:

```bash
cd examples/e2b-rootfs
buildctl build \
  --frontend dockerfile.v0 \
  --local context=. \
  --local dockerfile=. \
  --output type=image,name=localhost:5000/conch/e2b-rootfs:debug,push=true,registry.insecure=true
```

## Debug SSH

To inject a debug SSH public key at build time, run this standalone command from
the repository root:

```bash
cd examples/e2b-rootfs
buildctl build \
  --frontend dockerfile.v0 \
  --local context=. \
  --local dockerfile=. \
  --opt build-arg:DEBUG_SSH_AUTHORIZED_KEY="$(cat ~/.ssh/id_ed25519.pub)" \
  --output type=image,name=localhost:5000/conch/e2b-rootfs:debug,push=true,registry.insecure=true
```

The key becomes part of the built image. Use a dedicated, short-lived debug key,
do not publish this debug image to an untrusted registry, and rebuild without the
key before production use. Injecting the key only configures guest SSH access;
the operator must separately provide network reachability to the Sandbox IP and
the SSH port.
