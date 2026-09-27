# TeaStream Deluge plugin

Filmstream does no BitTorrent work itself. An unmodified Deluge 2 daemon
(libtorrent 2.0) downloads and seeds; the TeaStream core plugin in
`teastream/` gives Filmstream a small loopback JSON API to add torrents, pick
files and steer piece deadlines around the positions players read. Filmstream
then reads verified bytes straight from the shared downloads directory.

The endpoint reference is the docstring at the top of
`teastream/deluge_teastream/core.py`; `internal/torrentstream/plugin.go` is the
client.

## Build

```sh
deluge/build-egg.sh [OUTPUT_DIR]   # default deluge/dist
```

The egg is built with Docker inside the Deluge image it will run in
(`DELUGE_IMAGE`, default `lscr.io/linuxserver/deluge:latest`), because Deluge
only loads eggs tagged with its own Python version, for example
`TeaStream-1.0-py3.12.egg`.

## Install

1. Copy the egg into `<deluge config dir>/plugins/` (`/config/plugins/` in the
   linuxserver image).
2. Write a random bearer token to a file both containers can read, e.g.
   `openssl rand -hex 32 > /secrets/teastream-token`.
3. Give deluged the plugin settings through its environment:
   - `TEASTREAM_TOKEN_FILE`: the token file (required; without it every
     endpoint except `GET /v1/health` answers 503).
   - `TEASTREAM_SAVE_ROOT`: where torrents are saved, one `<root>/<info_hash>`
     directory each (default `/downloads`).
   - `TEASTREAM_BIND` / `TEASTREAM_PORT`: API address (default
     `127.0.0.1:8113`).

   The same keys (`token_file`, `save_root`, `bind`, `port`) can instead be
   set in `teastream.conf` in Deluge's config directory; the environment wins.
4. Enable the plugin once: stop deluged, set `"enabled_plugins": ["TeaStream"]`
   in `core.conf`, and start it again (or tick TeaStream under Preferences →
   Plugins in the Web UI). Deluge remembers enabled plugins across restarts.
5. Check `curl -s http://127.0.0.1:8113/v1/health`: `token_configured` must be
   true and `dht`, `lsd`, `upnp`, `natpmp` and `utpex` false.

Enabling the plugin switches off DHT, PEX, LSD, UPnP and NAT-PMP in Deluge's
config, as private trackers require. PEX stays loaded until deluged restarts
(`pex_extension_loaded` in the health response); restart deluged once after
the first enable.

It also tunes two libtorrent session settings for streaming:
`initial_picker_threshold` 0, so piece priorities (and with them the stream
windows) apply from a torrent's first piece instead of after four random ones,
and `urlseed_max_request_bytes` 2 MiB, so a web seed is never more than 2 MiB
away from serving a seek. Deluge should therefore be dedicated to Filmstream.

## Filmstream side

Filmstream and deluged must share the network namespace (the API is loopback)
and mount the downloads directory at the same path. Configure:

```json
"deluge": {
  "plugin_url": "http://127.0.0.1:8113",
  "token_file": "/secrets/teastream-token",
  "downloads_dir": "/downloads"
}
```

`--torrent-port-file` points Filmstream at a file holding the VPN's forwarded
peer port (for example gluetun's `forwarded_port`); Filmstream watches it and
pushes changes to Deluge. `--torrent-listen-port` sets a fixed port instead.

Private-tracker seed rules live on each indexer (`private`, `seed`); see the
main README.
