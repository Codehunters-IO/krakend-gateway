#!/usr/bin/env bash
# Fails when docker-compose.yml publishes a gateway port that nothing inside the
# container serves.
#
# KrakenD listens on ONE port -- service.json's `port` -- and enabling TLS makes
# that same port serve HTTPS rather than opening a second one. Metrics are the
# exception: metrics.json has its own listener.
#
# Compose published `8443:8443` for months with nothing behind it. Measured
# 2026-10-11: the running container had :8090 and :9090 open and nothing on
# :8443, so the README's own verification step, `curl -k
# https://localhost:8443/__health`, could never have worked, and the ports table
# described 8443 as "API Gateway (HTTPS)". A published port that answers nothing
# is worse than an undocumented one: it is read as the way in, and the operator
# who follows it concludes the gateway is down.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

python3 - "$ROOT_DIR" <<'PY'
import json, re, sys, pathlib

root = pathlib.Path(sys.argv[1])
compose = (root / "docker-compose.yml").read_text(encoding="utf-8")
settings = root / "config" / "settings"

service_port = json.loads((settings / "service.json").read_text(encoding="utf-8"))["port"]

# metrics.json carries its own listener, as ":9090" or "0.0.0.0:9090".
metrics = json.loads((settings / "metrics.json").read_text(encoding="utf-8"))
listen = str(metrics.get("listen_address", ""))
metrics_port = int(listen.rsplit(":", 1)[-1]) if ":" in listen else None

served = {int(service_port)}
if metrics_port:
    served.add(metrics_port)

# The krakend service's ports block: `- "HOST:CONTAINER"` until the next key.
block = re.search(r"\n  krakend:\n(.*?)(?=\n  [a-z_-]+:\n|\Z)", compose, re.S)
if not block:
    sys.exit("check-published-ports: no krakend service found in docker-compose.yml")

ports = re.search(r"\n    ports:\n((?:\s*(?:#.*|-.*)\n)+)", block.group(1))
published = []
if ports:
    for line in ports.group(1).splitlines():
        m = re.match(r'\s*-\s*"?(\d+):(\d+)"?\s*$', line)
        if m:
            published.append((int(m.group(1)), int(m.group(2))))

dead = [(h, c) for h, c in published if c not in served]
if dead:
    print("check-published-ports: FAILED", file=sys.stderr)
    for host, container in dead:
        print("    %d:%d — nothing in the container serves %d" % (host, container, container),
              file=sys.stderr)
    print("  KrakenD serves one port (service.json -> port = %d)%s." % (
        service_port, ", plus metrics on %d" % metrics_port if metrics_port else ""), file=sys.stderr)
    print("  A published port that answers nothing reads as the way in. Map the", file=sys.stderr)
    print("  host port to %d, or drop the mapping." % service_port, file=sys.stderr)
    sys.exit(1)

if int(service_port) not in [c for _, c in published]:
    print("check-published-ports: FAILED", file=sys.stderr)
    print("  docker-compose.yml publishes no mapping to the service port %d," % service_port,
          file=sys.stderr)
    print("  so the gateway is unreachable from the host.", file=sys.stderr)
    sys.exit(1)

print("check-published-ports: OK (%d published, every one served; service port %d reachable)"
      % (len(published), service_port))
PY
