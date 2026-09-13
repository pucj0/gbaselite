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
	// Re-issuing a name moves the restore point to the current position; the older
	// layer becomes an anonymous boundary, matching the legacy replacement rule.
	for index := range session.savepoints {
		if session.savepoints[index].name != "" && strings.EqualFold(session.savepoints[index].name, name) {
			session.savepoints[index].name = ""
		}
	}
	active := 0
	for _, layer := range session.savepoints {
		if layer.name != "" {
			active++
		}
	}
	if active >= maxMVCCSavepoints {
		return nil, fmt.Errorf("%w: at most %d savepoints per transaction", errSavepointResource, maxMVCCSavepoints)
	}
	parent := session.transaction
	child, err := parent.Child()
	if err != nil {
		return nil, err
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
	if err := layer.tx.Rollback(); err != nil {
		return nil, err
	}
	// Savepoints above the target are discarded, and the target keeps its name on a
	// fresh child of the same parent so the transaction can continue.
	fresh, err := layer.parent.Child()
	if err != nil {
		return nil, err
	}
	session.savepoints = append(session.savepoints[:index], savepointLayer{name: layer.name, tx: fresh, parent: layer.parent})
	session.transaction = fresh
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
