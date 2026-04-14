# PACKAGE-APPROVAL-PROCESS.md
## Procedure for Adding Dependencies to mgit

**Applicable to:** All contributors and developers
**Enforcement Level:** Mandatory
**Last Updated:** 2026-03-09

---

## 1. When You Need Approval

You must request package approval in these situations:

- You need to import a package **not listed** in APPROVED-PACKAGES.md
- You want to add an external dependency to mgit
- You discover an existing approved package has a critical limitation
- You need a newer version of an approved package with breaking API changes

### Critical Rule

**STOP immediately if you identify an unapproved dependency need. Do not:**
- Add the dependency to go.mod
- Import the package in code
- Commit any changes
- Push to remote branch

Instead, follow this process document.

---

## 2. Evaluation Criteria

Your package must satisfy **ALL** of these criteria to be approved:

### 2.1 Pure Go (No CGO)

The package must compile with CGO disabled.

**Verification:**
```bash
# Clone the package repository or examine go.mod
go list -json github.com/example/pkg | jq '.CgoRequired'
# Should output: false

# Or attempt build with CGO disabled
cd /tmp/test
go mod init test
go get github.com/example/pkg@v1.0.0
CGO_ENABLED=0 go build -v ./...
# Must succeed without C compiler errors
```

**Why:** mgit requires single-binary deployment across Linux, macOS, Windows, and ARM platforms. CGO creates platform-specific binaries and introduces C library dependencies.

### 2.2 License Compliance

The package must use an open-source license. Approved licenses:
- MIT
- BSD-2-Clause
- BSD-3-Clause
- Apache-2.0
- ISC

Forbidden licenses:
- GPL (v2, v3, v3+)
- LGPL (v2, v2.1, v3)
- AGPL (any version)
- Proprietary / Closed Source
- Custom/Non-standard licenses

**Verification:**
```bash
# Check LICENSE file in package repository
# Or use tools
go-licenses report github.com/example/pkg
```

**Why:** GPL/LGPL/AGPL impose source code disclosure requirements incompatible with commercial deployment. Standardized permissive licenses ensure clarity.

### 2.3 Maintenance Status

The package must be actively maintained:
- Last commit within **last 12 months**
- Maintainer responds to issues within 30 days
- No deprecation warnings in documentation
- No "archived" or "deprecated" badges

**Verification:**
```bash
# Check GitHub repository
# - Last commit date
# - Issue response time
# - Maintainer activity
# - Release cadence

# Example: go-git
# https://github.com/go-git/go-git
# Last commit: [check date]
# Issues resolved: Yes, active
# Releases: Regular (v5.12 → v5.13 in 2026)
```

**Why:** Unmaintained packages become security risks. Active maintenance ensures bug fixes and feature updates.

### 2.4 Security (No Known CVEs)

The package must have no publicly disclosed security vulnerabilities:

**Verification:**
```bash
# Run govulncheck
go install golang.org/x/vuln/cmd/govulncheck@latest
govulncheck github.com/example/pkg@v1.0.0

# Check NVD (National Vulnerability Database)
# https://nvd.nist.gov/
# Search: "github.com/example/pkg"

# Check package GitHub repository
# - Security advisories
# - Closed security issues
```

**Why:** Safety-critical software cannot rely on packages with known exploits.

### 2.5 Minimal Transitive Dependencies

The package should not introduce large dependency trees:

**Evaluation:**
- Preferred: 0–3 transitive dependencies
- Acceptable: 4–8 transitive dependencies
- Red flag: >10 transitive dependencies

**Verification:**
```bash
go mod graph github.com/example/pkg | wc -l
# Count all direct and transitive dependencies
```

**Why:** Each dependency increases attack surface and complicates vendoring.

### 2.6 Test Coverage

The package itself should have adequate test coverage:
- **Minimum 80% code coverage** (within the package)
- Tests included in repository
- CI/CD pipeline runs tests on commits

**Verification:**
```bash
# Check GitHub repository
# - README mentions test coverage
# - GitHub Actions or CI badge
# - coverage.io badge (codecov, coveralls)
# Example: https://codecov.io/gh/go-git/go-git
```

**Why:** Well-tested packages have fewer bugs and security flaws.

### 2.7 No Functionality Overlap

The package must not duplicate capability of an already-approved package:

**Approved functionality:**
- `modernc.org/sqlite` handles all SQLite needs
- `github.com/spf13/cobra` handles all CLI needs
- `github.com/labstack/echo/v4` handles all HTTP needs
- stdlib `crypto/*` handles hashing

If an approved package already does the job, extend or configure it instead of adding another.

**Why:** Multiple packages for the same functionality create API inconsistency and increase maintenance burden.

### 2.8 Implementation Necessity

The functionality cannot be reasonably implemented in **less than 100 lines of application code**:

If you can implement it locally in <100 lines (without sacrificing security or reliability), you should.

**Examples:**
- ULID generation: ~80 lines of stdlib code → **Approve external package** (complexity not worth it)
- JSON parsing: ~20 lines of stdlib code → **Use stdlib**
- Structured logging: ~50 lines of custom code → **Could go either way**, but slog is more robust

**Estimation:**
- Break down the feature into steps
- Estimate lines needed for core logic + tests + error handling
- If >100 lines, consider external package

**Why:** Reduces dependency surface; critical code stays auditable.

---

## 3. Submission Format

When you need to request approval for a new package, create a file named:

```
PACKAGE-APPROVAL-REQUEST-[PACKAGE-NAME].md
```

Example: `PACKAGE-APPROVAL-REQUEST-github-hashicorp-raft.md`

### Template

```markdown
## Package Approval Request

**Submitted by:** [Your name]
**Date:** [YYYY-MM-DD]
**Status:** Pending Review

---

### Package Information

**Package Name:** github.com/example/pkg
**Proposed Version:** v1.2.3
**Home Page:** https://github.com/example/pkg
**License:** MIT
**GitHub Stars:** [N/A]

### Purpose in mgit

[Describe what this package does for mgit and why it's needed. 2–3 sentences.]

Example:
> We need distributed ID generation for audit log entries. The package generates ULIDs (Universally Unique Lexicographically Sortable Identifiers), which are time-sortable and require no clock synchronization across agents.

### Alternatives Considered

[List other packages evaluated and explain why they were rejected.]

Example:
- `uuid`: Standard UUID v4 — not sortable, requires clock sync
- `custom ULID implementation`: Would require ~150 lines of crypto code + tests
- `github.com/segmentio/ksuid`: Requires CGO for efficient ID generation

### Evaluation Checklist

- [ ] **Pure Go (no CGO)**
  - Verification: `CGO_ENABLED=0 go build` succeeds
  - Last checked: [DATE]

- [ ] **License Approved**
  - License: [MIT/BSD-3/Apache-2.0/ISC]
  - Verified: Yes / No

- [ ] **Actively Maintained**
  - Last commit: [DATE] (within 12 months)
  - Maintainer responsive: Yes / No
  - GitHub issue response time: [N/A] days

- [ ] **Security (No CVEs)**
  - govulncheck result: Clean / [List CVEs]
  - NVD search result: No vulnerabilities found
  - Last security audit: [DATE or N/A]

- [ ] **Minimal Dependencies**
  - Direct dependencies: [COUNT]
  - Transitive dependencies: [COUNT]
  - Total: [COUNT] (acceptable if <10)

- [ ] **Test Coverage**
  - Test coverage: [PERCENTAGE]% (minimum 80%)
  - CI/CD present: Yes / No
  - Coverage badge: https://codecov.io/[...] (if available)

- [ ] **No Overlap**
  - Existing approved package covering this: [None / Package name]
  - Why not use that instead: [Explanation]

- [ ] **Implementation Necessity**
  - Estimated lines to implement without package: [ESTIMATE]
  - Complexity assessment: [Simple / Moderate / Complex]

---

### Detailed Justification

[Provide 2–3 paragraphs explaining why this package is essential for mgit.]

Example:
> The go-git library provides pure Go access to git object databases, index manipulation, and reference operations. Using an external `git` binary (via exec.Command) would introduce platform dependencies and deployment complexity. No other pure-Go git library offers comparable functionality.
>
> Alternatives like libgit2 (via git2go) require CGO, which violates mgit's single-binary deployment requirement. The net benefit of using go-git outweighs the dependency cost because mgit's core functionality depends on git manipulation.

---

### Impact Assessment

**Binary size increase:** [ESTIMATE MB]
**Startup time impact:** [ESTIMATE ms or N/A]
**Security surface:** [Assessment]
**Maintenance burden:** [Low / Medium / High]

---

### Sign-off

I certify that this package has been thoroughly evaluated against all criteria above and meets safety-critical standards for mgit.

**Requester:** [Name]
**Date:** [YYYY-MM-DD]
```

---

## 4. Approval Process

Follow these steps to request package approval:

### Step 1: Preparation
- Complete the evaluation checklist above
- Verify all 8 criteria are met
- Prepare the PACKAGE-APPROVAL-REQUEST-[NAME].md document
- Run `govulncheck` locally
- Test CGO_ENABLED=0 compilation

### Step 2: Submission
1. Create a new branch: `pkg-approval/[package-name]`
2. Add your PACKAGE-APPROVAL-REQUEST-[NAME].md file
3. Commit with message: `pkg-approval: [Package Name] - [Brief description]`
4. Push to remote
5. Open a pull request titled: `[PKG APPROVAL] Package Name - Brief description`
6. Tag it with label: `package-approval` and `safety-critical`

### Step 3: Review
The maintainer review team will:
1. Verify all criteria compliance
2. Run independent security checks (govulncheck, CVE database)
3. Evaluate architectural fit
4. Request modifications if needed

### Step 4: Decision
One of three outcomes:

#### APPROVED
- Your PACKAGE-APPROVAL-REQUEST file is moved to `/pkg-approvals/approved/`
- The package is added to APPROVED-PACKAGES.md with full entry
- You are notified in the PR
- You may now add the dependency to go.mod

#### NEEDS REVISION
- Specific issues are listed in the review
- You revise the PACKAGE-APPROVAL-REQUEST document
- Resubmit for review
- Iterate until approved

#### REJECTED
- Detailed rationale provided
- Alternatives suggested
- Discussion open for appeals
- You must implement the feature using approved packages or stdlib

### Step 5: Implementation (Upon Approval)
Once approved:
1. Update go.mod: `go get github.com/example/pkg@v1.2.3`
2. Update go.sum
3. Commit both go.mod and go.sum
4. Update APPROVED-PACKAGES.md table (add new row)
5. Create a new branch for actual code changes
6. Submit feature PR with dependency already approved

---

## 5. Emergency Exception Policy

**There is no emergency exception.**

Safety-critical software does not take shortcuts with dependencies. Period.

If you face a deadline conflict:
1. Implement the feature using approved packages (even if less elegant)
2. File the PACKAGE-APPROVAL-REQUEST through normal channels
3. Continue development on other features while approval is pending
4. Refactor to use the approved package once it's added

Time pressure is not a valid reason to bypass safety-critical review.

---

## 6. Revision of Previously Approved Packages

If you discover an approved package has a critical limitation or requires an update:

### Minor Version Update (e.g., 1.2.3 → 1.2.4)
- Usually safe; review release notes
- Update go.mod directly
- Run tests and govulncheck
- No additional approval needed

### Major Version Update (e.g., 1.2.3 → 2.0.0)
- May introduce breaking API changes
- Follow this entire approval process
- Create PACKAGE-APPROVAL-UPDATE-[NAME]-v2.md
- Justification should explain migration plan

### Deprecation or Removal
- If you want to remove an approved package, document rationale
- Follow reverse approval process
- Ensure all imports removed
- Update APPROVED-PACKAGES.md to mark as deprecated

---

## 7. Appeals Process

If you disagree with a rejection decision:

1. Request clarification in the PR comments
2. Propose alternative approaches
3. If unresolved after discussion, escalate to project lead
4. Project lead makes final decision
5. Decision is binding; no further appeals

---

## 8. Common Rejection Reasons

| Reason | How to Address |
|--------|----------------|
| CGO required | Choose pure-Go alternative or implement locally |
| GPL/AGPL license | Choose alternative with permissive license |
| Unmaintained (>12 months) | Fork and maintain yourself, or choose active alternative |
| Known CVEs | Wait for security patch or choose alternative |
| Too many dependencies | Reduce scope or implement simpler feature |
| Overlaps with approved package | Extend or configure existing package instead |
| Can be implemented in <100 lines | Implement locally in application code |
| Insufficient test coverage (<80%) | Request maintainer add tests, or choose alternative |

---

## 9. Frequently Asked Questions

**Q: Can I use an approved package in a way not mentioned in APPROVED-PACKAGES.md?**
A: Yes, as long as the usage is reasonable and doesn't introduce new dependencies. If you need to import a subpackage that's not already used, you don't need re-approval.

**Q: What if I need a package urgently for a critical bug fix?**
A: Implement a workaround using approved packages first. Then submit the approval request through normal channels. Safety-critical cannot mean "quick" — it means "correct."

**Q: Can I use a fork of an approved package?**
A: Only if the fork is in mgit's organization and actively maintained. External forks require approval as a new package.

**Q: What's the typical approval turnaround time?**
A: 3–7 business days for complete requests. Incomplete requests may wait longer.

**Q: Can I temporarily add a dependency and remove it later?**
A: No. All dependencies in go.mod must be approved. If you want to experiment, use a separate branch with explicit "WIP" status.

---

## 10. Enforcement

Violations of this policy will result in:

1. **First violation:** Code review rejection + explanation of policy
2. **Second violation:** Pull request blocked + notification to project lead
3. **Third violation:** Review of commit privileges

The CI/CD pipeline enforces this with dependency scanning:
```bash
# Automated check (part of pre-merge CI)
./scripts/check-approved-packages.sh
```

This script fails the build if:
- Unknown packages in go.mod
- Versions don't match APPROVED-PACKAGES.md pins
- Unapproved transitive dependencies detected

---

## Footer

**Remember:** Every dependency is a liability. We approve only what's necessary.

Questions? Contact the maintainer.
