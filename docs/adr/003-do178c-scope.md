# ADR-003: DO-178C Applicability Scope for mgit

**Status:** Accepted  
**Date:** 2026-03-09  
**Refs:** NFR-1.5, FR-12

---

## Context

mgit's documentation references DO-178C Level A as an applicable standard. DO-178C Level A is the highest assurance level for airborne software, requiring Modified Condition/Decision Coverage (MC/DC) — a coverage metric that Go's standard `go test -cover` does not measure.

The question is: does DO-178C apply to mgit's own binary, or to the process mgit supports?

## Decision

**mgit is a development tool, not embedded avionics software.** DO-178C applies to the *development process* that mgit supports, not to mgit's own binary.

### Classification

| Aspect | Classification | Rationale |
|--------|---------------|-----------|
| mgit binary | **Development tool** | Not embedded in aircraft, not part of airborne software load |
| Code managed by mgit | **Subject to DO-178C** | The source code in the production repo may be avionics software |
| mgit's audit trail | **DO-178C artifact** | Provides traceability evidence for certification |
| mgit's squash output | **DO-178C artifact** | The git patches produced by `mgit squash --to-git` are certification-relevant |

### What this means for mgit's own quality

mgit is not exempt from rigor. As a development tool used in DO-178C processes, mgit must:

1. **Be trustworthy** — its audit trail must be accurate and tamper-evident
2. **Be reliable** — data corruption in mgit could invalidate certification evidence
3. **Be tested** — comprehensive automated tests cover the critical paths
4. **Be documented** — traceability from requirements to tests is maintained

However, mgit does not need:

1. **MC/DC coverage** — this is a DO-178C Level A *code coverage* requirement for the avionics software itself, not for development tools
2. **DO-178C certification** — mgit itself is not certified; the software it manages is
3. **Formal verification** — mgit uses testing, not mathematical proof

### MC/DC Exception with Safety Net

While full MC/DC is not required, mgit adds **targeted MC/DC analysis** for three safety-critical functions where incorrect behavior could silently corrupt the audit trail:

| Function | Why MC/DC | Implementation |
|----------|-----------|----------------|
| `SquashService.SquashTask` | Partial squash = data corruption in production repo | Manual MC/DC analysis + exhaustive table-driven tests |
| `RollbackService.RollbackTask` | Incorrect rollback = lost audit evidence | Manual MC/DC analysis + exhaustive table-driven tests |
| `VerifyService.VerifyChain` | False positive = undetected tampering | Manual MC/DC analysis + exhaustive table-driven tests |

"Manual MC/DC analysis" means: for each conditional expression in these three functions, verify that every condition independently affects the decision outcome. Document this in inline comments.

### DO-330 Tool Qualification Level

DO-330 defines 5 Tool Qualification Levels (TQL-1 through TQL-5). mgit qualifies as **TQL-3**:

- **TQL-3 criteria**: The tool's output is verified by review before use.
- **How mgit satisfies TQL-3**: mgit produces git format-patch files (`mgit squash --to-git`) that are reviewed by humans and CI pipelines before integration into production repositories. If mgit introduces an error in the patch, it will be caught during code review.
- **TQL-3 evidence**: Tool operational requirements (REQUIREMENTS.md), tool description (README.md), and evidence that the tool works correctly (automated test suite with >85% coverage across all packages).

## Rationale

Even as a development tool, mgit produces output that may be used as certification evidence. A corrupted audit trail could invalidate work in regulated environments. The classification as a development tool does not relax mgit's own quality requirements, and the targeted MC/DC analysis on the three critical functions provides assurance where it matters most.

## Consequences

### Positive
- Avoids unnecessary certification cost and timeline
- Focuses MC/DC effort on the 3 functions that matter most
- Clear classification prevents scope creep of DO-178C requirements
- TQL-3 classification is defensible to certification authorities

### Negative
- Some auditors may challenge the "development tool" classification — this ADR provides the defense
- If mgit is ever embedded in an automated deployment pipeline without human review, the TQL classification would need re-evaluation

---

**Applies to:** QUALITY-STANDARDS.md §9 (Compliance Matrix)
