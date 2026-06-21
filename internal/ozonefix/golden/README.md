# Golden fixtures — O-Zone Print Server API

These are the **source of truth** for the print-server cache/proxy fidelity tests
(`docs/OZONE_PRINT_SERVER_API.md`). Each file is a representative O-Zone payload,
**valid JSON**, transcribed from the upstream worked examples — no invention.

## Provenance

| File | Source | Notes |
|---|---|---|
| `list_request.json` | `PrintServerDemo.py:323-325`, `OZone.cs:51` | the `list` command bytes |
| `list_response.json` | `PrintServerAPI.MD` §"Game List" example | 2 games, one `valid:1`, one `valid:0` with `endtime:"None"` |
| `all_request.json` | `PrintServerDemo.py:345-348`, `OZone.cs:194` | the `all` command for game 9 |
| `all_response.json` | `PrintServerAPI.MD` §"Complete Game Data Example" | game #9 "Competition Team Elimination"; the doc's `'scores',` typo is corrected to `"scores":`, Python-dict syntax converted to JSON; field names/values otherwise verbatim (incl. `omid: -1` as an integer and the `tags*` field-name variant) |
| `texts_banner.json` | `PrintServerAPI.MD` §"Scoresheet Texts" example | connect banner frame 2 |
| `event_types_banner.json` | `PrintServerAPI.MD` §"Event Types" | connect banner frame 1. **Approximate envelope** — the spec lists the id→name table but not the exact JSON wrapper O-Zone uses; both TORN and the OW2 agent *discard* this frame on connect, so only the frame count (2) and valid framing matter. Marked approximate until a real capture is available. |

`minimal`, `team`, and `player` responses are **derived** from `all_response.json`
at test time (minimal = `all` minus `events`; team/player = `game` + the named
subset), so they have no separate golden file.

## Wire framing

Golden files hold the **decoded JSON payload** only. The on-wire bytes are:

```
[ uint32 little-endian len(payload) ] [ 0x28 ] [ payload bytes ]
```

Wire bytes are produced/asserted in code (`frame(compactJSON(golden))`) rather
than committed as `.bin`, so there is a single trustworthy source and no risk of a
hand-edited binary drifting from the JSON. See the framing tests in the `results`
and `proxy` packages.

## Fidelity guarantee under test

The cache stores and replays the **exact bytes** O-Zone returned; these goldens
validate that (a) our framing is byte-correct, (b) a stored payload round-trips
byte-identically through the proxy, and (c) TORN's parser (reconstructed from
`OZone.cs`) can consume the proxy's `list`/`all` output — including the
`tagsby↔tagson` swap, the `valid>0` gate, and `omid=="-1"` fallback.
</content>
