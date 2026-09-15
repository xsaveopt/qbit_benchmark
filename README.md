# qbit_benchmark

qbit_benchmark generates an arbitrarily large test torrent and benchmarks a qBittorrent client's upload, download and disk throughput against a tracker and seeder that it runs itself.
Every byte of the payload is derived on demand from a 16 byte seed stored in the torrent's metadata, so the data never touches disk and a 1 TB torrent costs the same to serve as a 1 GiB one.

## Installation

Static Linux binaries for amd64 and arm64 are attached to every release.

```sh
curl -fL -o qbit_benchmark https://github.com/xsaveopt/qbit_benchmark/releases/latest/download/qbit_benchmark_linux_amd64
chmod +x qbit_benchmark
```

A linux/amd64 container image is published to ghcr.io/xsaveopt/qbit_benchmark as well, and running it is covered under Docker below.

## Benchmarking a download

Start the tracker and seeder on the machine qBittorrent should pull from, with an announce host that the qBittorrent box can reach over the network.

```sh
qbit_benchmark serve -size 4GiB -piece 1MiB -announce http://{host}:6969/announce
```

That writes qbench.torrent, prints its infohash and the address the seeder is advertised under, and then reports served bytes per second.
Once the file is added in qBittorrent, the tracker hands it the seeder's address and the transfer starts by itself.
The rate shows up in qBittorrent, in the serve output, and as Prometheus metrics at /metrics on the tracker's HTTP port, which cover bytes and piece blocks served, piece requests received, open peer connections, tracker announces and peers currently in the swarm.

The gen command writes the same torrent and exits, taking -o for the output path.
Handing an existing file to serve with -torrent seeds it again from the seed stored inside it.
The advertised seeder address always comes from -announce, which falls back to 127.0.0.1 on the tracker port when it is left out, so pass the same URL the file was generated with.

Announces are answered with a compact peer list, which only carries IPv4 addresses, so the announce host has to resolve to one.
A client that announces over IPv6 is logged with a warning and left out of the list handed to others, although it still receives the seeder's IPv4 address.
A dual stack qBittorrent therefore benchmarks normally, while an IPv6 only client cannot take part.

## Benchmarking an upload

Once qBittorrent holds a complete copy from a download run, leave it seeding and pull from it with leech.
Its listen port is under Options, Connection, Port used for incoming connections.

```sh
qbit_benchmark leech -torrent qbench.torrent -addr {qbittorrent-ip}:{port} -n 1
```

Results print per connection and as an aggregate before the run exits, and the same qbench.torrent works for as many runs as you like.
Without -n, leech opens four parallel connections, but qBittorrent accepts a single connection per remote IP like most clients and closes the others as they arrive.

## Sizes and pieces

Sizes take KiB, MiB and GiB, their decimal KB, MB and GB forms, or a plain byte count, and fractions such as 1.5GiB work too.
There is no terabyte unit, so a 1 TB torrent is written as 1024GiB or 1000GB.
Piece length must be a multiple of 16 KiB, and larger pieces mean fewer hashes and less overhead per piece while smaller ones let a transfer reach full speed sooner.

Generating the torrent is the one step whose cost grows with the total size, because the metadata carries a SHA-1 for every piece and each piece has to be produced once to hash it.
Hashing runs on every core and holds one piece per core in memory, so 8 GiB takes a few seconds and 1 TB takes several minutes.
At that scale the piece length matters more than the size, since 1 TB of 1 MiB pieces is a million hashes and a 20 MB torrent file that some clients struggle with, while 8 MiB or 16 MiB pieces bring the file down to a couple of MB and cut the hashing time to match.

A torrent comfortably larger than the RAM on the qBittorrent box, such as 32 GiB on a 16 GB machine, measures the disk rather than the page cache.

## Docker

The docker-compose.yml in the repo runs serve with a 4 GiB torrent written into ./out, which is the copy to add in qBittorrent.
Replace YOUR_HOST in its announce URL with the host's LAN address and start it with docker compose up -d.
The seeder is advertised under the port it listens on inside the container, so each published port has to be the same on both sides, and a mapping like 7881:6881 would send qBittorrent to the wrong one.

Stable releases are tagged latest, with 1, 1.2 and 1.2.3 pinning a major, minor or patch line.
The dev tag follows the tip of main and is rebuilt on every commit.

## License

qbit_benchmark is released under the GNU General Public License v2.0, found in LICENSE.
