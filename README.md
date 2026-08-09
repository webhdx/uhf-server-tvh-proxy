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
3. The proxy stores the mapping between the UHF recording ID and the
   TVHeadend DVR UUID locally.
4. Recording listing, cancellation, and deletion use the TVHeadend DVR API.
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

## Running with Docker Compose

```sh
cp .env.example .env
```

Set the TVHeadend URL and API credentials in `.env`, then run:

```sh
docker compose up -d --build
```

If TVHeadend is outside the same Docker network, `TVH_URL` must point to an
address reachable from the container, for example
`http://192.168.1.20:9981`.

Add the server manually in the UHF app:

- host: the address of the machine running the proxy;
- port: `8000`, or the configured `PORT` value;
- server password: the `SERVER_PASSWORD` value, if configured.

Automatic mDNS discovery is not implemented yet.

## Configuration

| Variable | Default | Description |
| --- | --- | --- |
| `PORT` | `8000` | Port of the UHF-compatible API |
| `TVH_URL` | `http://tvheadend:9981` | TVHeadend base URL, optionally including `http_root` |
| `TVH_USERNAME` | empty | TVHeadend API username |
| `TVH_PASSWORD` | empty | TVHeadend API password |
| `SERVER_PASSWORD` | empty | Optional password entered when adding the server in UHF |
| `STATE_PATH` | `/data/state.json` | Persistent UHF ↔ TVHeadend mapping file |

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

## UHF 1.6.0 API compatibility

The following table covers every endpoint exposed by the UHF Server 1.6.0
OpenAPI document.

| Method and endpoint | Status | Current behavior |
| --- | --- | --- |
| `POST /auth/login` | Partial | Preserves the UHF response format and issues a local token. It validates the optional `SERVER_PASSWORD`, but does not authenticate the account with Firebase. Credentials are not stored. |
| `GET /dvr/recordings` | Supported | Reads DVR state from TVHeadend and returns recordings created through this proxy. Existing TVHeadend recordings created elsewhere are not imported. |
| `POST /dvr/recordings` | Partial | Creates a single timer through `dvr/entry/create`. Channel UUID and channel-name mapping are supported. Requests containing `recurrence_days` return HTTP 501. |
| `GET /dvr/recordings/{recording_id}` | Supported | Returns a recording with its current status derived from TVHeadend. |
| `DELETE /dvr/recordings/{recording_id}` | Supported | Cancels an upcoming or active recording, or removes a finished recording and its file through TVHeadend. |
| `GET /dvr/recordings/{recording_id}/metadata` | Local support | Returns metadata stored in the proxy state file. |
| `PATCH /dvr/recordings/{recording_id}/metadata` | Local support | Merges metadata locally; a `null` value removes a key. Changes are not written to TVHeadend DVR metadata. |
| `GET /dvr/recordings/{recording_id}/stream` | Supported | Proxies TVHeadend `/dvrfile/{uuid}` and forwards `Range`, `If-Range`, `If-None-Match`, and `If-Modified-Since`. |
| `GET /dvr/recordings/{recording_id}/thumbnail` | Not implemented | The route validates authentication but always returns HTTP 404. |
| `GET /dvr/recordings/{recording_id}/commercials` | Stub | Always returns an empty commercial list and `total_segments: 0`. |
| `PATCH /dvr/recordings/{recording_id}/cancel` | Supported | Calls TVHeadend `dvr/entry/cancel` and stores the local `cancelled` status. |
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
- Importing TVHeadend recordings that were not created through this proxy.
- Actual TVHeadend host storage, CPU, and memory statistics.
- Firebase account verification.
- Automatic discovery through `_uhf-server._tcp.local.` mDNS.

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
