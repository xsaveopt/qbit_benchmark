# qbit_benchmark

Generate an arbitrarily large test torrent and benchmark a qBittorrent client's upload, download, and disk throughput against a tracker and peer you run yourself.

Every byte in the torrent comes from a 16 byte seed stored in the metadata, derived on demand as peers ask for it.
Nothing is written to disk and no copy of the payload is held in memory, so a 1 TB torrent costs the same to serve as a 1 GiB one.

## Contents

- [Installation](#installation)
- [Benchmarking a download](#benchmarking-a-download)
- [Benchmarking an upload](#benchmarking-an-upload)
- [Addressing](#addressing)
- [Sizes and piece lengths](#sizes-and-piece-lengths)
- [Metrics](#metrics)
- [docker-compose](#docker-compose)
- [Image tags](#image-tags)

## Installation

Download the latest static Linux binary:

```sh
curl -fL -o qbit_benchmark https://github.com/xsaveopt/qbit_benchmark/releases/latest/download/qbit_benchmark_linux_amd64
chmod +x qbit_benchmark
```

A container image is published as well, covered under docker-compose below.

## Benchmarking a download

Start the tracker and seeder on the machine you want qBittorrent to pull from, setting the announce host to an address the qBittorrent box can actually reach on the network.

```sh
qbit_benchmark serve -size 4GiB -piece 1MiB -announce http://LANIP:6969/announce
```

That writes qbench.torrent, prints its infohash, and begins reporting served bytes per second.
Copy the file to the qBittorrent machine and add it there, and the tracker will hand qBittorrent the seeder's address so the transfer starts on its own.
Watch the rate in qBittorrent, in the serve output, or on the metrics endpoint.

If you only want the file, gen writes one and exits.

```sh
qbit_benchmark gen -size 4GiB -piece 1MiB -announce http://LANIP:6969/announce -o qbench.torrent
```

Pass an existing file to serve with -torrent to seed it again later, and the announce URL and seed are read back out of it.

## Benchmarking an upload

Once qBittorrent holds a complete copy from the download run, leave it seeding and pull from it.
Its listen port is under Options, Connection, Port used for incoming connections.

```sh
qbit_benchmark leech -torrent qbench.torrent -addr QBITIP:PORT -n 1
```

Results print per connection and as an aggregate, then the run exits.
The same qbench.torrent works for as many leech runs as you like, so this part is repeatable once qBittorrent has the data.

Most clients, qBittorrent among them, accept a single connection per remote IP, and the extra connections are closed as they arrive.
Keep -n at 1 against qBittorrent, and raise it only when the far side allows several connections from one address.

## Addressing

Everything the tracker deals in is IPv4.
Announces are answered with a compact peer list, which carries four byte addresses, so the peers it hands out and the seeder it advertises are IPv4 addresses.
Give -announce a host that resolves to one, and serve will say on startup which address and port it is advertising the seeder under.

A client that announces from an IPv6 address is logged with a warning and left out of the peer list handed to others.
It still receives the seeder's IPv4 address, so a dual stack qBittorrent connects and benchmarks normally, while an IPv6 only client cannot take part.

## Sizes and piece lengths

Sizes accept KiB, MiB and GiB along with their decimal KB, MB and GB forms, and fractions such as 1.5GiB are fine.
Defaults are a 1 GiB torrent with 256 KiB pieces, a tracker on :6969, and a seeder on :6881.

Piece length must be a multiple of 16 KiB.
Larger pieces mean fewer hashes and lower per-piece overhead, while smaller pieces let a transfer reach full speed sooner.

Generating a torrent is the one step whose cost scales with the total size, because the metadata carries a SHA-1 for every piece and each piece has to be produced once to hash it.
Hashing runs across all cores and holds only a piece per core at a time, so 8 GiB takes a few seconds and 1 TB takes several minutes.
At terabyte scale the piece length matters more than the total: 1 TB of 1 MiB pieces is a million hashes and a 20 MB torrent file that some clients struggle with, where 8 MiB or 16 MiB pieces bring it down to a couple of MB and cut the hashing time to match.

To measure disk rather than page cache, make the torrent comfortably larger than the RAM on the qBittorrent box, so 32 GiB or more on a 16 GB machine.

## Metrics

Prometheus metrics are served by serve at /metrics on the tracker's HTTP port.

Reported are bytes and piece blocks served, piece requests received, open peer connections, tracker announces, and peers currently in the swarm.
A leech run prints its result to stdout and exits, so its numbers come out there rather than through a scrape.

## docker-compose

```yaml
services:
  qbit_benchmark:
    image: ghcr.io/xsaveopt/qbit_benchmark:latest
    container_name: qbit_benchmark
    restart: unless-stopped
    command: serve -size 4GiB -piece 1MiB -announce http://YOUR_HOST:6969/announce -o /out/qbench.torrent
    read_only: true
    volumes:
      - ./out:/out
    ports:
      - "6969:6969"
      - "6881:6881"
```

Set YOUR_HOST to the host's LAN address, since the announce URL has to resolve to something the qBittorrent box can reach.
The container generates its torrent on startup and writes it into the mounted out directory, and that copy is the one to add to qBittorrent.

```sh
docker compose up -d
```

Keep the ports the same on both sides.
The tracker advertises the port it listens on inside the container, so 7881:6881 would point qBittorrent at the wrong one.

## Image tags

Stable releases are tracked by latest, and 1, 1.2 and 1.2.3 pin to a major, minor, or patch line.
The dev tag follows the tip of main, rebuilt on every commit, and is the easiest way to try an unreleased change.
Images are published to ghcr.io/xsaveopt/qbit_benchmark for linux/amd64.
