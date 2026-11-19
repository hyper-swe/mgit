# MGIT-329.2: land after mgit rewrote its own block in AGENTS.md or CLAUDE.md

1. A repository tracks AGENTS.md and CLAUDE.md. `mgit work --sandbox` and the sandbox launch rewrite the
   block mgit generates in them.
2. A guest commit is made and `mgit sandbox land --task-id <id>` runs.
3. Before: land refused with "native projection would overwrite host changes at CLAUDE.md", though nothing
   but mgit's own block had changed.
4. After: a doc recorded as generated lands when host, baseline and guest tip are identical outside the block.
   The host doc is left exactly as it was.
5. An edit outside the block, in any of the three versions, still refuses, and a path that was never recorded
   as generated behaves as before.
