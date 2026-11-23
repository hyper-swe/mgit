package reviewbind

import (
	"context"
	"strconv"
	"sync"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/reviewprep"
	"github.com/hyper-swe/mgit/internal/sandboxd/reviewsvc"
)

const (
	guestVerb = "/sbin/mgit-guest"
	credRoot  = "/run/review-cred" //nolint:gosec // a directory path, not a credential
)

// CredentialSource fills a slot for the named credential, once, from a private
// descriptor the operator provides. It never returns the bytes in an error.
type CredentialSource func(ctx context.Context, ref string) (*Slot, error)

// EgressApplier enforces a host-allowlist policy for one guest and returns the
// function that lifts it. Without one, a policy naming a host is refused.
type EgressApplier interface {
	Apply(ctx context.Context, guestID string, p Policy) (lift func(context.Context) error, err error)
}

// Config is what the daemon gives the binding. Zero values grant nothing.
type Config struct {
	ReviewerGID int // group that may read the credential file; must match the run's
	Source      CredentialSource
	Egress      EgressApplier
}

type binding struct{ cfg Config }

// New returns the Bindings implementation: it installs the credential the
// operator's mapping names and the egress policy it names, or refuses.
func New(cfg Config) reviewsvc.Bindings {
	if cfg.ReviewerGID <= 0 {
		cfg.ReviewerGID = 65534
	}
	return &binding{cfg: cfg}
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
	if pol.Kind == EgressHost && b.cfg.Egress == nil {
		return release, unenforceable("this build cannot enforce a host allowlist, and the mapping asks for one")
	}
	if ref != "" && (b.cfg.Source == nil || in.Exec == nil) {
		return release, unenforceable("this build has no credential source, and the mapping asks for one")
	}

	if pol.Kind == EgressHost {
		lift, aerr := b.cfg.Egress.Apply(ctx, in.GuestID, pol)
		if aerr != nil {
			return release, unenforceable("the egress policy could not be applied")
		}
		fresh(lift)
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
		_, e := in.Exec(c, model.ExecRequest{Command: []string{guestVerb, "cred-remove", "--dest", dest}, AsRoot: true})
		return e
	})
	return release, nil
}
