# pixeld

The on-device daemon for a **Govee H8630 Gaming Pixel Light** (32×32 LED matrix, Allwinner T113) running
custom firmware in "native" mode: none of Govee's processes run, and `pixeld` drives the panel and talks
to Home Assistant over the LAN. No cloud.

It is the device side of the [Govee Pixel Light](https://github.com/dtbell91/ha-govee-pixel-light) Home
Assistant integration (its `pixeld` connection type).

## What it does

- **Panel:** a bit-exact framebuffer encoder for the panel (`panel`), a playlist/compositor that rotates
  content on the light and resumes after interruptions (`player`), and cards drawn on the light (`cards`):
  a clock, and live weather and forecast scenes drawn from the weather data Home Assistant sends (see below).
- **Home Assistant over MQTT** (`px/*` topics): state, playlist (next / previous / go to an item / hold it), show, content (PNG, full-colour APNG,
  JPEG or GIF) delivered by SHA-256 and cached on the light, telemetry, buttons, headphone jack.
- **Voice satellite** over the ESPHome native API (`:6053`), added by Home Assistant's ESPHome
  integration: on-device wake word (microWakeWord via an external `mww` binary), announcements, and timers
  shown on the panel. The speaker is a media player too (e.g. for Music Assistant): music streams as
  48 kHz WAV (Home Assistant converts it), with replies, chimes and alarms mixed over it and the music
  ducked while the assistant listens or speaks.
- **Sendspin player** (`:8928/sendspin`, advertised as `_sendspin._tcp`): Music Assistant's synchronised
  multi-room protocol, so the light can be grouped with other Sendspin speakers. End-to-end encrypted
  (Noise KKpsk2); paired by a 6-digit code shown on the panel, or by token (`pixeld -sendspin-token`).
  Audio is scheduled against the server's clock straight through ALSA (no `aplay`), so the light knows
  to the sample when each block is heard. Same speaker, volume and mute as the media player.
- **Bluetooth proxy** for Home Assistant over the same ESPHome API connection: passive BLE scanning
  through a raw HCI socket on the RTL8723DS (pixeld powers it on via rfkill and attaches it with
  `rtk_hciattach`; no BlueZ daemon), advertisements relayed raw in batches, as ESPHome proxies do.
  Scans only while Home Assistant is subscribed, 30 ms in every 320 ms to leave the radio to WiFi.
  `-bluetooth=false` turns it off.
- **DDP** live pixels on UDP 4048 (LedFx, xLights, WLED-style senders), and a Pac-Man game with a phone
  controller page on `:8080`.

## Weather cards

`{"type": "weather", ...}` and `{"type": "forecast", ...}` playlist cards are drawn by the light at 20 fps from
the data in the card, so Home Assistant only sends new numbers when the weather changes. The weather card is a
window onto the sky:

- The sun and moon cross the sky where they really are, from the light's clock and the card's `lat`/`lon`
  (facing the equator: rising on the left in the northern hemisphere, the right in the southern), the moon at
  its actual phase. The sky goes through dawn, day, sunset and night colours, with twinkling stars and the
  odd shooting star on clear nights.
- `cloud` (%) sets how many clouds there are; they drift with `wind` / `bearing`, and are lit pink at sunrise
  and sunset. Rain, snow, sleet and hail fall harder with `rate` (expected in the next hour) and slant with the
  wind; storms flash with lightning at random. Streaks of wind from 22 km/h; leaves (petals in spring) in a
  gale. `uv` grows the sun's halo and rays. Hot days shimmer near the ground, frosty ground glints, wet
  ground has puddles.
- Under the sky, in turn: the temperature (with a bar along the bottom showing it between today's `low` and
  `high`), high/low, feels like (`apparent`, when it differs by 3°), chance and amount of rain, wind speed and
  direction, UV index and its level, the fire danger rating (`fire`, Australian ratings, as a four-step gauge in
  `fire_color`), weather warnings in force (`warnings`), and the next sunrise or sunset.

How the strip is shown: `pages` (which, in what order: `temperature`, `warnings`, `high_low`, `feels_like`, `rain`,
`wind`, `uv`, `fire`, `sun`; default all), `temp_seconds` (6), `page_seconds` (4), `transition` (0.35 s slide). Each
time the card comes back on screen it shows the first page and then carries on where it left off, so a short
playlist turn still gets through every page over a few turns (`restart_pages: true` always starts again).

A `clock` card with a `weather` spec draws the same live scene behind the time: sky and ground on top, the time
under them, the date over the horizon.

The forecast card shows up to three `days` (condition, high, low, rain chance as a bar, day name), with the
icons animating live and today's icon turning to night after sunset. `page_seconds` (4) sets how long the highs and lows, then the days
and rain chances, show.

`go run ./cmd/cardpreview -frames 6 -step 1s '{"type":"weather",...}'` draws a card over time to a PNG (or
`-gif out.gif` as an animation).

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
