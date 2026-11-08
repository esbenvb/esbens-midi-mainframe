# Build environment for the EMM appliance image (Buildroot 2025.02.x).
# Host packages are the ones the Buildroot manual lists as required/recommended.
FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
        bash bc binutils build-essential bzip2 ca-certificates cpio diffutils file \
        findutils g++ gawk gcc git gzip libncurses-dev libssl-dev make patch perl \
        python3 rsync sed tar unzip wget xz-utils zip \
    && rm -rf /var/lib/apt/lists/*

# Buildroot refuses to build as root, so build as a user with the host's uid/gid
# (keeps files owned by you when the repository is bind-mounted).
ARG UID=1000
ARG GID=1000
RUN groupadd -g "$GID" builder 2>/dev/null || true \
    && useradd -m -u "$UID" -g "$GID" -s /bin/bash builder

USER builder
WORKDIR /work
