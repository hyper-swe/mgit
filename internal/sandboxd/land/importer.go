package land

import (
	"context"
	"fmt"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/hyper-swe/mgit/internal/model"
	gitstore "github.com/hyper-swe/mgit/internal/store/git"
)

// StoreImporter is the concrete ObjectImporter: it writes verified land
// objects (commits, trees, blobs) into the host shared .mgit object store.
// It is the only writer of the shared store on the land path and runs
// only AFTER VerifyBinding has bound each object's bytes to its claimed
// identity (SEC-06) — so it imports content-addressed bytes the host has
// already proven. Imports are idempotent (the store is content-addressed),
// so a re-run or an aborted land leaves harmless orphans, never
// corruption. Refs: FR-17.5, FR-17.24
type StoreImporter struct {
	repo *gitstore.Repository
}

// NewStoreImporter wires the importer to a host repository's object store.
func NewStoreImporter(repo *gitstore.Repository) *StoreImporter {
	return &StoreImporter{repo: repo}
}

// ImportObjects writes each object into the host store under the git
// object type its frame tag declares. An unknown tag is a schema
// violation (it should never reach here — DecodeObjects already rejects
// unknown tags — but the importer fails closed regardless). Refs: FR-17.5
func (s *StoreImporter) ImportObjects(_ context.Context, objs []Object) error {
	for _, obj := range objs {
		typ, err := gitObjectType(obj.Type)
		if err != nil {
			return err
		}
		if _, err := s.repo.WriteRawObject(typ, obj.Data); err != nil {
			return fmt.Errorf("land import: %w", err)
		}
	}
	return nil
}

// gitObjectType maps a land frame tag to its go-git object type.
func gitObjectType(t byte) (plumbing.ObjectType, error) {
	switch t {
	case ObjCommit:
		return plumbing.CommitObject, nil
	case ObjTree:
		return plumbing.TreeObject, nil
	case ObjBlob:
		return plumbing.BlobObject, nil
	default:
		return plumbing.InvalidObject,
			fmt.Errorf("%w: cannot import unknown object type %#x", model.ErrLandVerificationFailed, t)
	}
}
