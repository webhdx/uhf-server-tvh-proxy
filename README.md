# UHF Server → TVHeadend DVR proxy

<p align="center">
  <img src="logo.png" alt="UHF Server to TVHeadend DVR proxy logo" width="240">
</p>

A minimal UHF Server replacement that accepts recording requests from the UHF
app and creates DVR timers in TVHeadend. The proxy does not download or record
streams itself.

## How it works

1. UHF signs in through the compatible `POST /auth/login` endpoint.
2. `POST /dvr/recordings` is translated to
   `POST /api/dvr/entry/create` in TVHeadend.
3. The TVHeadend DVR UUID is exposed directly as the UHF recording ID.
4. Recording listing, status, cancellation, and deletion are read from or
   written to the TVHeadend DVR API without local persistence.
5. `/dvr/recordings/{id}/stream` proxies `/dvrfile/{uuid}` from TVHeadend,
   including HTTP Range support.

The original UHF Server is neither started nor required.

## TVHeadend requirements

Create a dedicated API user in TVHeadend. Its access entry needs at least:

- Web interface access (`ACCESS_WEB_INTERFACE`), which is required by the JSON
  API;
- Recorder / Basic permission;
- access to the channels that should be recorded.

The channel name exposed to UHF should match the corresponding channel name in
TVHeadend.

## Quick start with Docker

The published image supports `linux/amd64` and `linux/arm64`.

```sh
docker run -d \
  --name uhf-server-tvh-proxy \
  --restart unless-stopped \
  -p 8000:8000 \
  -e TVH_URL=http://192.168.1.20:9981 \
  -e TVH_USERNAME=uhf \
  -e TVH_PASSWORD=change-me \
  -e SERVER_PASSWORD=change-me \
  ghcr.io/webhdx/uhf-server-tvh-proxy:1.0.0
```

Replace the TVHeadend address and credentials before starting the container.
`TVH_URL` must be reachable from inside the container.

Check that the proxy is running:

```sh
curl http://127.0.0.1:8000/server/stats
```

## Running with Docker Compose

```sh
git clone https://github.com/webhdx/uhf-server-tvh-proxy.git
cd uhf-server-tvh-proxy
cp .env.example .env
```

Set the TVHeadend URL, API credentials, and optional server password in `.env`,
then run:

```sh
docker compose pull
docker compose up -d
```

Set `IMAGE_TAG=1.0.0` in `.env` to pin a specific release. The default is
`latest`. Upgrade the container with the same `pull` and `up` commands.

If TVHeadend is outside the same Docker network, `TVH_URL` must point to an
address reachable from the container, for example
`http://192.168.1.20:9981`.

Add the server manually in the UHF app:

- host: the address of the machine running the proxy;
- port: `8000`, or the configured `PORT` value;
- server password: the `SERVER_PASSWORD` value, if configured.

Automatic mDNS discovery is not implemented yet.

Useful operational commands:

```sh
docker compose logs -f
docker compose restart
docker compose down
```

## Configuration

| Variable | Default | Description |
| --- | --- | --- |
| `PORT` | `8000` | Port of the UHF-compatible API |
| `TVH_URL` | `http://tvheadend:9981` | TVHeadend base URL, optionally including `http_root` |
| `TVH_USERNAME` | empty | TVHeadend API username |
| `TVH_PASSWORD` | empty | TVHeadend API password |
| `SERVER_PASSWORD` | empty | Optional password entered when adding the server in UHF |

The UHF account password submitted during login is not verified with Firebase,
stored, or logged. It is accepted only to preserve the UHF client contract. If
the proxy is not restricted to a trusted local network, configure
`SERVER_PASSWORD` and place it behind TLS or a reverse proxy.

## Channel mapping

The proxy selects the DVR channel in this order:

1. `/stream/channel/<32-character-uuid>` URL → TVHeadend `channel` field;
2. `/stream/channelname/<name>` URL → TVHeadend `channelname` field;
3. UHF request `description` field → TVHeadend `channelname` field.

The third rule supports standard TVHeadend playlists, which generate
`/stream/channelid/<short-id>` URLs. UHF includes the channel name in the
request's `description` field.

If the channel cannot be identified, the proxy returns an error and does not
create a timer.

## UHF 2.0.0 API compatibility

The following table covers every endpoint exposed by the UHF Server 2.0.0
OpenAPI document.

Proxy releases use their own semantic version independently of the emulated
UHF Server version. For example, proxy `1.0.0` emulates the UHF Server `2.0.0`
API. `/server/stats` reports the emulated server version expected by the UHF
client.

| Method and endpoint | Status | Current behavior |
| --- | --- | --- |
| `POST /auth/login` | Partial | Preserves the UHF response format and issues a stateless, signed token. It validates the optional `SERVER_PASSWORD`, but does not authenticate the account with Firebase. Credentials are not stored. |
| `GET /dvr/recordings` | Supported | Reads and returns all DVR entries directly from TVHeadend, including recordings created elsewhere. |
| `POST /dvr/recordings` | Partial | Creates a single timer through `dvr/entry/create`. Channel UUID and channel-name mapping are supported. Requests containing `recurrence_days` return HTTP 501. |
| `GET /dvr/recordings/{recording_id}` | Supported | Returns a recording with its current status derived from TVHeadend. |
| `DELETE /dvr/recordings/{recording_id}` | Supported | Cancels an upcoming or active recording, or removes a finished recording and its file through TVHeadend. |
| `GET /dvr/recordings/{recording_id}/metadata` | Partial | Returns a TVHeadend `metadata` object when present, otherwise an empty object. |
| `PATCH /dvr/recordings/{recording_id}/metadata` | No-op | Validates the request and returns the current recording with HTTP 200, but only logs the update because arbitrary UHF metadata cannot be persisted in TVHeadend. |
| `GET /dvr/recordings/{recording_id}/stream` | Supported | Proxies TVHeadend `/dvrfile/{uuid}` and forwards `Range`, `If-Range`, `If-None-Match`, and `If-Modified-Since`. |
| `GET /dvr/recordings/{recording_id}/hls/{name}` | Not available | Preserves the UHF 2.0 route and authentication contract, but returns HTTP 404 because TVHeadend does not expose DVR recordings as UHF-style HLS assets. Use `/stream` instead. |
| `GET /dvr/recordings/{recording_id}/thumbnail` | Not implemented | The route validates authentication but always returns HTTP 404. |
| `GET /dvr/recordings/{recording_id}/commercials` | Stub | Always returns an empty commercial list and `total_segments: 0`. |
| `PATCH /dvr/recordings/{recording_id}/cancel` | Supported | Calls TVHeadend `dvr/entry/cancel`; subsequent status is read from TVHeadend. |
| `PATCH /dvr/recordings/{recording_id}/cancel-recurrence` | Not implemented | Returns HTTP 400 because recurring recordings cannot currently be created. |
| `GET /server/stats` | Partial | Checks TVHeadend availability through `api/serverinfo` and returns the expected UHF schema. CPU, memory, and disk values describe the proxy or are placeholders rather than TVHeadend host statistics. |

No OpenAPI route is entirely absent from the HTTP router. The complete
single-recording workflow—create, retrieve, list, cancel, delete, and
playback—is implemented.

## Missing functionality

- Recurring recordings and `cancel-recurrence`. These require mapping UHF
  recurrence settings to TVHeadend `timerec` or `autorec` rules.
- Thumbnail generation or forwarding.
- Commercial detection data.
- Updating arbitrary UHF metadata, because TVHeadend has no equivalent field.
- Actual TVHeadend host storage, CPU, and memory statistics.
- Firebase account verification.
- Automatic discovery through `_uhf-server._tcp.local.` mDNS.

## Building locally

Build and run the container from the current checkout:

```sh
docker build -t uhf-server-tvh-proxy:local .
docker run --rm -p 8000:8000 --env-file .env uhf-server-tvh-proxy:local
```

## Running without Docker

Go 1.26 or newer is required.

```sh
go test ./...
go run .
```

## Disclaimer

This project was vibecoded. It is provided as-is, without any warranty, and the
author accepts no responsibility or liability for whether or how it works, or
for any damage, data loss, failed recordings, or other consequences resulting
from its use.

## License

This project is licensed under the [MIT License](LICENSE).
