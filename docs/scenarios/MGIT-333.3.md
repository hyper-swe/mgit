11. What a review needs beyond the sealed bundle (the binding map, a network policy, a credential) is installed behind
    one interface, `reviewsvc.Bindings`, called once per run after the guest is sealed and before the reviewer starts, and
    released whatever happens. It receives identifiers only. The default installs nothing and grants nothing: a mapping
    that names a credential or any network policy other than none is refused with POLICY_UNENFORCEABLE, so a review is
    never run without what its mapping asked for.
