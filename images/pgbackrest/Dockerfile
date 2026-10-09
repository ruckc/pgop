# pgBackRest images used by pgop for physical backups (see docs/user-guide/backups.md).
#
# One Dockerfile, one pinned pgBackRest build, two targets:
#
#   --target pgbackrest  ghcr.io/ruckc/pgop-pgbackrest:<PGBACKREST_VERSION>
#                        Runs the backup and restore Jobs.
#   --target postgres    ghcr.io/ruckc/pgop-postgres:<PG_MAJOR>-<PGBACKREST_VERSION>
#                        The official postgres image plus the same pgBackRest
#                        binary, for Clusters with physical backups (WAL
#                        archiving and the pgBackRest TLS server).
#
# The Job and the PostgreSQL pod must run the same pgBackRest version (its
# remote protocol is version-locked), so both copy the binary built by the
# builder stage. Bump PGBACKREST_VERSION and PGBACKREST_SHA256 together, and
# update pgbackrestVersion in internal/controller/pgbackrest.go to match.

ARG PG_MAJOR=18

FROM debian:trixie-slim AS builder

ARG PGBACKREST_VERSION=2.59.3
ARG PGBACKREST_SHA256=b2a95535a68e326a0f316c2d8d3243bc30092cf1ce9de9be5d142e204d2d4263

RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      ca-certificates curl gcc meson ninja-build pkg-config \
      libbz2-dev liblz4-dev libpq-dev libssh2-1-dev libssl-dev libxml2-dev libyaml-dev libzstd-dev zlib1g-dev \
 && rm -rf /var/lib/apt/lists/*

WORKDIR /src
RUN curl -fsSL -o pgbackrest.tar.gz \
      "https://github.com/pgbackrest/pgbackrest/archive/release/${PGBACKREST_VERSION}.tar.gz" \
 && echo "${PGBACKREST_SHA256}  pgbackrest.tar.gz" | sha256sum -c - \
 && tar xzf pgbackrest.tar.gz --strip-components=1 \
 && meson setup build --buildtype=release -Dlibsystemd=disabled \
 && ninja -C build \
 && install -D -m 0755 build/src/pgbackrest /out/usr/bin/pgbackrest \
 && /out/usr/bin/pgbackrest version

# The runtime stages install the libraries pgBackRest links against (Debian
# trixie package names; the postgres image is trixie-based as well).

FROM debian:trixie-slim AS pgbackrest

ARG PGBACKREST_VERSION=2.59.3
LABEL org.opencontainers.image.title="pgop-pgbackrest" \
      org.opencontainers.image.description="pgBackRest ${PGBACKREST_VERSION} for pgop backup and restore Jobs" \
      org.opencontainers.image.source="https://github.com/ruckc/pgop" \
      org.opencontainers.image.licenses="MIT" \
      org.opencontainers.image.version="${PGBACKREST_VERSION}"

RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      ca-certificates libbz2-1.0 liblz4-1 libpq5 libssh2-1t64 libssl3t64 libxml2 libyaml-0-2 libzstd1 zlib1g \
 && rm -rf /var/lib/apt/lists/* \
 && groupadd -g 999 postgres \
 && useradd -u 999 -g 999 -d /var/lib/pgbackrest -m -s /usr/sbin/nologin postgres \
 && install -d -o 999 -g 999 -m 0750 /etc/pgbackrest /var/log/pgbackrest /var/spool/pgbackrest /tmp/pgbackrest

COPY --from=builder /out/usr/bin/pgbackrest /usr/bin/pgbackrest
RUN pgbackrest version

# Same uid/gid as the postgres user of the official postgres image, so a
# restore Job writes the data directory with the ownership PostgreSQL expects.
USER 999:999
CMD ["pgbackrest", "version"]

FROM postgres:${PG_MAJOR}-trixie AS postgres

ARG PGBACKREST_VERSION=2.59.3
LABEL org.opencontainers.image.title="pgop-postgres" \
      org.opencontainers.image.description="Official postgres image with pgBackRest ${PGBACKREST_VERSION}, for pgop Clusters with physical backups" \
      org.opencontainers.image.source="https://github.com/ruckc/pgop" \
      org.opencontainers.image.licenses="PostgreSQL AND MIT"

RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      ca-certificates libbz2-1.0 liblz4-1 libpq5 libssh2-1t64 libssl3t64 libxml2 libyaml-0-2 libzstd1 zlib1g \
 && rm -rf /var/lib/apt/lists/* \
 && install -d -o postgres -g postgres -m 0750 /etc/pgbackrest /var/log/pgbackrest /var/spool/pgbackrest /tmp/pgbackrest

COPY --from=builder /out/usr/bin/pgbackrest /usr/bin/pgbackrest
RUN pgbackrest version
