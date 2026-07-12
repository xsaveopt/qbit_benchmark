# qbit_benchmark

Generate an arbitrarily large test torrent and benchmark a qBittorrent client's upload, download, and disk throughput against a tracker and peer you run yourself.

> ⚠️ This runs an open tracker and a peer that serves data to anyone who connects, and it pushes as much traffic as the link allows.
> Run it on networks you control, against your own qBittorrent, not on the public internet.

## Contents

- [Installation](#installation)
- [Usage](#usage)
- [Metrics](#metrics)
- [docker-compose](#docker-compose)
- [Image tags](#image-tags)

## Installation

Download the latest static Linux binary:

```sh
curl -fL -o qbit_benchmark https://github.com/xsaveopt/qbit_benchmark/releases/latest/download/qbit_benchmark_linux_amd64
chmod +x qbit_benchmark
```

A container image is also published, covered under [docker-compose](#docker-compose) below.

## Usage

There are three subcommands.

`gen` writes a `.torrent` and prints its infohash without starting anything.

```sh
qbit_benchmark gen -size 4GiB -piece 1MiB -announce http://HOST:6969/announce -o qbench.torrent
```

`serve` runs the tracker and the seeder, generating the `.torrent` first if you do not pass one.
This is the download benchmark: add the printed `.torrent` to qBittorrent and watch it pull.

```sh
qbit_benchmark serve -size 4GiB -piece 1MiB -announce http://HOST:6969/announce
```

`leech` pulls from a peer that is already seeding the `.torrent`, with `-n` parallel connections.
This is the upload benchmark: have qBittorrent seed the `.torrent`, then point `leech` at its listen address.

```sh
qbit_benchmark leech -torrent qbench.torrent -addr HOST:PORT -n 8
```

Sizes accept `KiB`, `MiB`, `GiB` and their decimal `KB`, `MB`, `GB` forms.
Piece length must be a multiple of 16 KiB; larger pieces mean fewer hashes and lower per-piece overhead, smaller pieces ramp up faster.
The defaults are a 1 GiB torrent with 256 KiB pieces, a tracker on `:6969`, and a seeder on `:6881`.

## Metrics

`serve` exposes Prometheus metrics at `/metrics` on the tracker's HTTP port.

It reports bytes and piece blocks served, piece requests received, open peer connections, tracker announces, and peers currently in the swarm.
`leech` is a one-shot run that prints its result and exits, so it is not scraped; its numbers come out on stdout.

## docker-compose

```yaml
services:
  qbit_benchmark:
    image: ghcr.io/xsaveopt/qbit_benchmark:latest
    container_name: qbit_benchmark
    restart: unless-stopped
    command: serve -size 4GiB -piece 1MiB -announce http://YOUR_HOST:6969/announce
    ports:
      - "6969:6969"
      - "6881:6881"
```

The announce URL must be an address the qBittorrent box can actually reach, so set `YOUR_HOST` to the host's LAN address.

```sh
docker compose up -d
```

## Image tags

`latest` tracks the most recent stable release, and `1`, `1.2`, `1.2.3` pin to a major, minor, or patch line.
`dev` tracks the tip of `main`, rebuilt on every commit, and is the easiest tag for trying an unreleased change.
Images are published to `ghcr.io/xsaveopt/qbit_benchmark` for `linux/amd64`.
