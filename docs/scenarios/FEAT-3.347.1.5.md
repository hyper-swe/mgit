# FEAT-3.347.1.5: the operator's binding map, the egress policy and the credential slot

1. A review reads its binding map only from the operator's install directory (`review/maps/<id>.json`), never from the
   bundle or the project. `reviewbind.LoadMap` accepts a map only when it is a plain file, not a link and not writable by
   group or others, and only when a pinned `<id>.sha256` holds the record's canonical hash (keys sorted, no spacing)
   and the record still matches it. A missing pin, an edit after the pin, a record that names another id, a link, a
   writable file or an id that leaves the directory is refused with MAPPING_MISMATCH.
2. The egress policy id is `none` (or empty), or `host:<one fully qualified name>`. A wildcard, an address, a list, a
   port, `localhost` or any other word is refused. `none` allows nothing; a host policy allows that name and no other,
   whatever the letter case.
3. A map that names a credential must also name a host policy; a credential with no network is refused, and so is a
   credential reference that is not a plain token.
4. The credential slot is filled once from an inherited descriptor, taken once and wiped after. It is never in argv or
   the environment, and neither its rendering (`%v`, `%+v`, `%#v`, `%s`, `%q`, `%x`, JSON) nor an error carries its bytes.
   The tests use a made-up key only; no real credential reaches a guest.
