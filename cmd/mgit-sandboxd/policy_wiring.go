package main

import (
	"github.com/hyper-swe/mgit/internal/service"
)

// egressWiring is what wiring the host egress stack produced: the capability
// escalation engine (deny->prompt->grant) and the LIVE policy enforcer.
//
// They travel together because they come from the same enforcer and both are
// optional per platform — a zero value means "this build has no host egress
// engine", and the daemon then reports those verbs unserved rather than
// answering them with a silent success. Refs: FR-17.12, MGIT-72
type egressWiring struct {
	Grants *service.CapabilityService
	Policy service.EgressPolicyController
}

// selectPolicyController picks the enforcer the LIVE policy verbs act on.
//
// The platform controller WINS when there is one: on a libkrun build the
// enforcing authorizer lives in a re-exec'd VM child, and the daemon's own
// egress runner — which may still be wired on Linux — is enforcing nothing for
// those sandboxes. Routing a revoke to it would report success while the VM
// kept the old policy, which is the worst answer this verb can give.
// Refs: MGIT-72, ADR-010, SEC-04
func selectPolicyController(platform service.EgressPolicyController, wired egressWiring) service.EgressPolicyController {
	if platform != nil {
		return platform
	}
	return wired.Policy
}
