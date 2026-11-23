# FEAT-3.347.1.7 — installing and showing a review binding map

An operator installs the binding map a review is checked against, and a caller reads back which map is the default.

## Scenarios

1. `mgit sandbox review map install --file map.json [--default]` checks the record (schema `mgit.review-map/v1`, a valid id,
   an egress policy of `none` or one named host, a credential reference only with a named host), creates the host root,
   `review` and `review/maps` as owner-only directories, and writes `<id>.json` and `<id>.sha256` (the canonical hash) at
   mode 0600. It loads the result back and removes it if it does not load. An id that is already installed is refused and
   never overwritten.
2. `--default` also writes a pinned pointer, `_default`, naming the id. `_default` is not a valid map id, so no map can take
   its name. An edited pointer is refused.
3. `mgit sandbox review map show [--id X] [--json]` verifies the map and prints its id, `binding_sha256` (the digest the
   record carries), `map_sha256` (the pinned hash of the whole record; a different value) and the egress policy id. It never
   prints a credential or its reference. Without `--id` it describes the default map.
4. With no default installed, `show` says: `mgit sandbox review map install --file <map.json> --default`. Nothing picks a map
   on its own.
5. The host root is the one the sandbox review commands use; `--host-root` overrides it. The image comes separately, from the
   release pin.

The JSON is `mgit.review-map-info/v1`: `schema`, `id`, `binding_sha256`, `map_sha256`, `egress_policy_id`.
