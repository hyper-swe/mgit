package model

// VMMReport is what a sandbox daemon build can say about the hypervisor it
// links, asked without starting a daemon (`mgit-sandboxd --vmm`): which VMM,
// where each library it depends on resolved in that process, and every
// condition that stops it booting a guest.
//
// It exists because "the daemon loads" and "the daemon can boot a guest" are
// different facts. libkrun loads libkrunfw (the library that carries the
// guest kernel) lazily, by name, the first time a VM context is made, so a
// daemon whose libkrunfw is missing starts, answers --version and reports
// nothing wrong until the first launch fails. Refs: MGIT-229, MGIT-206
type VMMReport struct {
	// VMM is the linked backend: BackendLibkrun, BackendKVM (firecracker),
	// BackendVZF, or "none" where the platform has no backend.
	VMM string `json:"vmm"`
	// Libraries lists what the backend depends on and where each resolved;
	// an empty Path means it was not found.
	Libraries []VMMLibrary `json:"libraries,omitempty"`
	// Problems names every condition that stops a guest booting, in words a
	// reader can act on. Empty means none was found.
	Problems []string `json:"problems,omitempty"`
}

// VMMLibrary is one dependency of the linked VMM and where it resolved.
type VMMLibrary struct {
	Name string `json:"name"`
	Path string `json:"path,omitempty"`
}

// CanBoot reports whether the build named a backend and found nothing that
// stops a guest booting. A report that names no backend never can.
func (r VMMReport) CanBoot() bool {
	return r.VMM != "" && len(r.Problems) == 0
}
