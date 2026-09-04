package controlproto

import "github.com/hyper-swe/mgit/internal/model"

// KindSyncVerify ('K') asks a daemon whether a task's guest reads what was
// last delivered to it — the doctor row MGIT-164 asked for. DECLARED here
// beside the verb it serves; it is a request kind like every other one in
// controlproto.go, and TestHello_KindTagIsUnique asserts the whole set is
// distinct. Refs: MGIT-164, MGIT-192
const KindSyncVerify byte = 'K'

// SyncVerifyResult carries the guest's answer verbatim.
type SyncVerifyResult struct {
	View *model.GuestViewReport `json:"view"`
}
