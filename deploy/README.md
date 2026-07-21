# Deploy — Etronium MVP

Single-machine dev/demo deploy via docker-compose.

Production multi-node deploy is intentionally **out of scope** for this
version — users run their own single-machine instance, and we route to
their lords.

## Quick Start (docker-compose)

```bash
# Build runtime image (etronium + schedulerd + BPF .o baked in)
./scripts/mvp/build-image.sh

# Bring up testbed
./scripts/mvp/up.sh -d

# Run E2E BPF test
./scripts/mvp/e2e-bpf.sh

# Observe kernel scx state + scheduler maps
docker exec mvp-frontend /usr/local/bin/scheduler stats

# Live migration via scheduler CLI
docker exec mvp-frontend \
    /usr/local/bin/scheduler migrate --hostname lord-edge-X --shares 4

# Tear down
./scripts/mvp/down.sh
```
