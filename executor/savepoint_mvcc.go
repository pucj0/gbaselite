package executor

import (
	"errors"
	"fmt"
	"gbaselite/storageengine"
	"strings"
)

// MVCC savepoints are nested layers of the session transaction: SAVEPOINT opens a
// child write set, ROLLBACK TO discards that layer and re-opens a fresh child of
// its parent, and RELEASE only drops the name so later savepoints stay valid.
// Every layer commits into its parent, so COMMIT merges the whole chain into the
// single outermost transaction; nothing opens an extra top-level transaction.
//
// maxMVCCSavepoints bounds the live child layers of one user transaction, not the
// distinct savepoint names. A released or replaced name leaves its layer behind as an
// anonymous boundary, and B01 does not compact those, so the layer count is the
// resource that must stay bounded.
const maxMVCCSavepoints = 32

var (
	errMVCCSavepointNotFound = errors.New("SAVEPOINT does not exist")
	errSavepointResource     = errors.New("resource limit exceeded")
)

type savepointLayer struct {
	name   string
	tx     storageengine.Txn
	parent storageengine.Txn
}

func (e *Engine) savepointIndex(session *Session, name string) int {
	for index := len(session.savepoints) - 1; index >= 0; index-- {
		if session.savepoints[index].name != "" && strings.EqualFold(session.savepoints[index].name, name) {
			return index
		}
	}
	return -1
}

// outermostTransaction returns the user transaction that owns every savepoint
// layer (the session transaction when no savepoint is active).
func outermostTransaction(session *Session) storageengine.Txn {
	if len(session.savepoints) > 0 {
		return session.savepoints[0].parent
	}
	return session.transaction
}

func (e *Engine) createSavepoint(session *Session, name string) (*Result, error) {
	if session.transaction == nil {
		return &Result{Message: "no active transaction"}, nil
	}
	// The budget bounds the live MVCC savepoint layers, not the names. RELEASE and a
	// same-name replacement only anonymize a layer, so the child transaction it holds
	// keeps occupying a slot: counting only named savepoints would let
	// "SAVEPOINT x; RELEASE x" -- or a repeated "SAVEPOINT x" -- grow the layer chain
	// without bound. The check runs before any mutation, so a refused SAVEPOINT cannot
	// anonymize an existing name as a side effect.
	if len(session.savepoints) >= maxMVCCSavepoints {
		return nil, fmt.Errorf("%w: at most %d savepoint layers per transaction", errSavepointResource, maxMVCCSavepoints)
	}
	// Below the ceiling, re-issuing a name still moves the restore point to the current
	// position; the older layer becomes an anonymous boundary, matching the legacy
	// replacement rule. An anonymous boundary is not compacted, so it keeps counting
	// against the layer budget above.
	//
	// The child is created before any name is touched: if that fails (a closed or
	// generation-invalidated parent), the statement must not have dropped a savepoint
	// name as a side effect.
	parent := session.transaction
	child, err := parent.Child()
	if err != nil {
		return nil, err
	}
	for index := range session.savepoints {
		if session.savepoints[index].name != "" && strings.EqualFold(session.savepoints[index].name, name) {
			session.savepoints[index].name = ""
		}
	}
	session.savepoints = append(session.savepoints, savepointLayer{name: name, tx: child, parent: parent})
	session.transaction = child
	return &Result{Message: "savepoint created"}, nil
}

func (e *Engine) releaseSavepoint(session *Session, name string) (*Result, error) {
	index := e.savepointIndex(session, name)
	if index < 0 {
		return nil, fmt.Errorf("%w: %s", errMVCCSavepointNotFound, name)
	}
	session.savepoints[index].name = ""
	return &Result{Message: "savepoint released"}, nil
}

func (e *Engine) rollbackToSavepoint(session *Session, name string) (*Result, error) {
	index := e.savepointIndex(session, name)
	if index < 0 || session.transaction == nil {
		return nil, fmt.Errorf("%w: %s", errMVCCSavepointNotFound, name)
	}
	layer := session.savepoints[index]
	// The target and every layer above it are logically discarded, so each one must end
	// its own lifecycle and release its own resources. Dropping them from the registry is
	// not enough: the registry owns diagnostics state, while storageengine.Txn cleanup
	// (closing the staging database and removing its file) only happens when the child
	// transaction itself is rolled back. Innermost first, and every layer is attempted
	// even if one cleanup fails, so a single failure cannot leave the rest of the
	// discarded subtree holding staging handles.
	discarded := session.savepoints[index:]
	var cleanupErr error
	for i := len(discarded) - 1; i >= 0; i-- {
		if err := discarded[i].tx.Rollback(); err != nil {
			cleanupErr = errors.Join(cleanupErr, err)
		}
	}
	// Shortening the slice does not drop the backing array's references to the discarded
	// layers, and an abandoned layer would keep its staging handle alive, so the dropped
	// range is cleared explicitly.
	clear(discarded)
	session.savepoints = session.savepoints[:index]
	// Fallback invariant: the session must always reference a live, reachable
	// transaction that can still be cleaned up. Point it at the surviving parent before
	// the fresh child is attempted, because that attempt can fail (a closed or
	// generation-invalidated parent). Without this, a failure at index 0 would leave the
	// session holding only the discarded, now closed layer, so the root would be
	// unreachable and its staging handle and file could never be released by ROLLBACK,
	// CloseSession or Store.Close.
	session.transaction = layer.parent
	// The target keeps its name on a fresh child of the same parent so the transaction
	// can continue. That parent is outside the discarded range by construction, so the
	// live layer count never exceeds the ceiling: the fresh child is created only after
	// the discarded range is gone.
	fresh, err := layer.parent.Child()
	if err != nil {
		return nil, errors.Join(cleanupErr, err)
	}
	session.savepoints = append(session.savepoints, savepointLayer{name: layer.name, tx: fresh, parent: layer.parent})
	session.transaction = fresh
	if cleanupErr != nil {
		// The discarded writes are gone and the chain was rebuilt, so the logical outcome
		// is decided; only a resource release failed, and that error is reported.
		return nil, cleanupErr
	}
	return &Result{Message: "rolled back to savepoint"}, nil
}

// commitSessionTransaction commits every savepoint layer into its parent and then
// commits the outermost transaction.
func (e *Engine) commitSessionTransaction(session *Session) error {
	if session.transaction == nil {
		return nil
	}
	for index := len(session.savepoints) - 1; index >= 0; index-- {
		if _, err := session.savepoints[index].tx.Commit(operatorContext(session)); err != nil {
			// Roll back every remaining layer and the outermost transaction so no
			// orphan transaction or savepoint state survives a failed merge.
			rollbackSessionTransaction(session)
			return err
		}
	}
	outermost := outermostTransaction(session)
	if _, err := outermost.Commit(operatorContext(session)); err != nil {
		rollbackSessionTransaction(session)
		return err
	}
	session.savepoints = nil
	session.transaction = nil
	return nil
}

// rollbackSessionTransaction discards every layer, including the outermost one.
func rollbackSessionTransaction(session *Session) {
	if session.transaction == nil {
		return
	}
	for index := len(session.savepoints) - 1; index >= 0; index-- {
		_ = session.savepoints[index].tx.Rollback()
	}
	outermost := outermostTransaction(session)
	session.savepoints = nil
	session.transaction = nil
	if outermost != nil {
		_ = outermost.Rollback()
	}
}
