# MGIT-333.4: the seam for what a review needs beyond the sealed bundle

## Today: the interface and its default
1. What a review needs beyond the sealed bundle (the binding map, a network policy, a credential slot) is to be
   installed behind one interface, `reviewsvc.Bindings`. Nothing calls it yet: the host verb that runs a review is a
   later change, and until it lands this is an interface, a default and a way to replace the map loader.
2. `Attach` is given identifiers and the guest's exec; it returns a release function and an error. The release
   function is never nil, on any return path including an error, and calling it twice is safe.
3. The default, `NoBindings`, installs nothing and grants nothing. A mapping that names a credential, or a network
   policy other than none, is refused with POLICY_UNENFORCEABLE, and the refusal says nothing of the credential
   reference. A mapping that asks for neither is accepted.
4. The service reads the operator's mapping through a loader it can be given (`WithMapLoader`). The mapping id is
   checked before any loader runs, so a replacement loader is only ever handed an id of letters, digits, dot, dash
   and underscore, starting with a letter or digit, at most 64 characters.

## The caller contract (for the run verb, a later change)
5. The run calls `Attach` once, after the guest is provisioned and its seal verified and before the reviewer starts.
6. The run always calls the release function it got back, after an error as well, without checking it, and does so
   whatever happened to the reviewer.
7. An error from `Attach` is a refusal and the reviewer is not started.
8. A secret does not travel in the arguments or environment of the guest exec `Attach` is given: they are visible to
   the guest's other processes.
