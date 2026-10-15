# pixeld

The on-device daemon for a **Govee H8630 Gaming Pixel Light** (32×32 LED matrix, Allwinner T113) running
custom firmware in "native" mode: none of Govee's processes run, and `pixeld` drives the panel and talks
to Home Assistant over the LAN. No cloud.

It is the device side of the [Govee Pixel Light](https://github.com/dtbell91/ha-govee-pixel-light) Home
Assistant integration (its `pixeld` connection type).

## What it does

- **Panel:** a bit-exact framebuffer encoder for the panel (`panel`), a playlist/compositor that rotates
  content on the light and resumes after interruptions (`player`), and cards drawn on the light, e.g. a
  clock (`cards`).
- **Home Assistant over MQTT** (`px/*` topics): state, playlist, show, content delivered by SHA-256 and
  cached on the light, telemetry, buttons, headphone jack.
- **Voice satellite** over the ESPHome native API (`:6053`), added by Home Assistant's ESPHome
  integration: on-device wake word (microWakeWord via an external `mww` binary), announcements, and timers
  shown on the panel. The speaker is a media player too (e.g. for Music Assistant): music streams as
  48 kHz WAV (Home Assistant converts it), with replies, chimes and alarms mixed over it and the music
  ducked while the assistant listens or speaks.
- **DDP** live pixels on UDP 4048 (LedFx, xLights, WLED-style senders), and a Pac-Man game with a phone
  controller page on `:8080`.

## Build

Static armv7 binary:

```sh
GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" ./cmd/pixeld
```

`go test ./...` runs anywhere. The panel encoder's bit-exact comparison against Govee's own renderer
(`TestMatchesGovee`) runs only with `qemu-arm`, a reference renderer built against Govee's
`libgovee_display.so`, and the stock rootfs it runs in (`PIXELD_ORACLE`, `PIXELD_SYSROOT`); otherwise it is
skipped.

Tagged releases (`v*`) are built by GitHub Actions and attached to the release as
`pixeld-linux-armv7` with its SHA-256.

`MQTT_HOST=<broker> go run ./cmd/pixeld -sim -data /tmp/pixeld` runs on a PC without hardware and shows
the panel in a browser at http://localhost:8099/.

## Running it

`pixeld` expects the custom firmware's native mode (WiFi up, MQTT settings in
`/mnt/UDISK/mqtt/bridge.env`, data in `/mnt/UDISK/pixeld`); see `pixeld -h`. It is not useful on stock
Govee firmware.

## Notes

`panel/data` holds the panel's LED map, constant driver template and Govee's gamma tables, as used by
Govee's own display library, so the output matches the stock firmware exactly. Not affiliated with
Govee. MIT licensed.
