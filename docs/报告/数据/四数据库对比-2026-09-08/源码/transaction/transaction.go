package transaction

import "gbaselite/storage"

type State uint8

const (
	Active State = iota
	Committed
	RolledBack
)

type Transaction struct {
	Snapshot storage.StoreSnapshot
	State    State
}

func Begin(store *storage.Store) *Transaction {
	return &Transaction{Snapshot: store.Snapshot(), State: Active}
}
func (t *Transaction) Commit() {
	if t.State == Active {
		t.State = Committed
	}
}
func (t *Transaction) Rollback(store *storage.Store) error {
	if t.State != Active {
		return nil
	}
	if err := store.Replace(t.Snapshot); err != nil {
		return err
	}
	t.State = RolledBack
	return nil
}
