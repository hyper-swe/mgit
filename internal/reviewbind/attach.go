package reviewbind

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strconv"
	"sync"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/reviewprep"
	"github.com/hyper-swe/mgit/internal/sandboxd/egress"
	"github.com/hyper-swe/mgit/internal/sandboxd/reviewsvc"
)

const (
	guestVerb = "/sbin/mgit-guest"
	credRoot  = "/run/review-cred" //nolint:gosec // a directory path, not a credential
)

// CredentialSource fills a slot for the named credential, once, from a private
// descriptor the operator provides. It never returns the bytes in an error.
type CredentialSource func(ctx context.Context, ref string) (*Slot, error)

// Config is what the daemon gives the binding. Zero values grant nothing.
type Config struct {
	ReviewerGID int // group that may read the credential file; must match the run's
	Source      CredentialSource
	// Lookup resolves a policy's host to addresses on the host side. Nil means
	// the system resolver. The addresses are fixed at grant time: a later change
	// in DNS does not move the grant.
	Lookup func(ctx context.Context, host string) ([]netip.Addr, error)
}

// egressPort is the one port a host policy opens. The policy names no port.
const egressPort = "443"

type binding struct{ cfg Config }

// New returns the Bindings implementation: it installs the credential the
// operator's mapping names and the egress policy it names, or refuses.
func New(cfg Config) reviewsvc.Bindings {
	if cfg.ReviewerGID <= 0 {
		cfg.ReviewerGID = 65534
	}
	return &binding{cfg: cfg}
}

// grant resolves the policy's host and admits each address on port 443 only.
// The daemon revokes every grant when the guest is removed.
func (b *binding) grant(ctx context.Context, pol Policy, allow func(context.Context, string) error) error {
	lookup := b.cfg.Lookup
	if lookup == nil {
		lookup = func(c context.Context, h string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(c, "ip", h)
		}
	}
	addrs, err := lookup(ctx, pol.Host)
	if err != nil || len(addrs) == 0 {
		return unenforceable("the policy's host did not resolve to an address")
	}
	// The daemon never admits an address in a denied range, so a name that
	// resolves only to such addresses would attach and reach nothing, with no
	// word of why. It is refused here instead, naming the cause and no address.
	reachable := false
	for _, a := range addrs {
		if _, denied := egress.IsUnconditionallyDenied(a); !denied {
			reachable = true
			break
		}
	}
	if !reachable {
		return unenforceable("the policy's host resolves only to addresses a review guest may never reach")
	}
	for _, a := range addrs {
		if gerr := allow(ctx, net.JoinHostPort(a.Unmap().String(), egressPort)); gerr != nil {
			return unenforceable("the egress policy could not be applied")
		}
	}
	return nil
}

func unenforceable(detail string) error {
	return reviewprep.Refuse(reviewprep.CodePolicyUnenforcable, detail)
}

// Attach implements reviewsvc.Bindings. The release it returns is never nil and
// is safe to call more than once.
func (b *binding) Attach(ctx context.Context, in reviewsvc.AttachInput) (func(context.Context) error, error) {
	var (
		mu    sync.Mutex
		done  bool
		undo  []func(context.Context) error
		fresh = func(f func(context.Context) error) { undo = append(undo, f) }
	)
	release := func(c context.Context) error {
		mu.Lock()
		defer mu.Unlock()
		if done {
			return nil
		}
		done = true
		var first error
		for i := len(undo) - 1; i >= 0; i-- {
			if err := undo[i](c); err != nil && first == nil {
				first = err
			}
		}
		return first
	}

	pol, err := ParseEgress(in.Mapping.EgressPolicyID)
	if err != nil {
		return release, unenforceable("the mapping names an egress policy this build does not know")
	}
	ref := in.Mapping.CredentialRef
	if ref != "" && pol.Kind != EgressHost {
		return release, unenforceable("a credential needs a named host to reach")
	}
	if pol.Kind == EgressHost && in.AllowEgress == nil {
		return release, unenforceable("this guest has no egress stack, and the mapping asks for a host allowlist")
	}
	if ref != "" && (b.cfg.Source == nil || in.Exec == nil) {
		return release, unenforceable("this build has no credential source, and the mapping asks for one")
	}

	if pol.Kind == EgressHost {
		if gerr := b.grant(ctx, pol, in.AllowEgress); gerr != nil {
			return release, gerr
		}
	}
	if ref == "" {
		return release, nil
	}

	slot, serr := b.cfg.Source(ctx, ref)
	if serr != nil || slot == nil {
		_ = release(ctx)
		return release, unenforceable("the credential could not be read")
	}
	secret, terr := slot.Take()
	if terr != nil {
		_ = release(ctx)
		return release, unenforceable("the credential could not be read")
	}
	defer Wipe(secret)

	dest := credRoot + "/" + ref
	res, xerr := in.Exec(ctx, model.ExecRequest{
		Command: []string{guestVerb, "cred-install", "--dest", dest, "--gid", strconv.Itoa(b.cfg.ReviewerGID)},
		AsRoot:  true,
		Stdin:   model.Secret(secret),
	})
	if xerr != nil || res == nil || res.ExitCode != 0 {
		_ = release(ctx)
		return release, unenforceable("the credential could not be installed in the guest")
	}
	fresh(func(c context.Context) error {
		r, e := in.Exec(c, model.ExecRequest{Command: []string{guestVerb, "cred-remove", "--dest", dest}, AsRoot: true})
		if e != nil || r == nil || r.ExitCode != 0 {
			// the slot may still be in the guest: say so, without any detail that could carry bytes
			return errors.New("the credential slot could not be confirmed removed from the guest")
		}
		return nil
	})
	return release, nil
}
