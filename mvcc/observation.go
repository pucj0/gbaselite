package mvcc

import (
	"bytes"
	"sync/atomic"
)

// observation.go holds the transaction-diagnostics accounting: bounded counters
// for read observations, the write set, and declared dependencies.
//
// Nothing here retains keys. A transaction's ordinary reads are summarised as
// counts, so a large SELECT costs a fixed amount of diagnostics state no matter
// how many rows it read. The write-set counters do track logical keys, but only
// through the write set the transaction already holds (the bounded buffer and
// optional staging database), never through a second collection.
//
// The counters themselves live in a transactionCounters block owned by
// TransactionManager, so diagnostics can read them without locking the commit path.

// counterDecrement is -1 in two's complement, for atomic.Uint64.Add.
const counterDecrement = ^uint64(0)

// transactionObservation accumulates one read's rows and bytes locally, then
// publishes them to the counters in a single step. A scan never updates shared
// state per row.
type transactionObservation struct {
	rows  uint64
	bytes uint64
}

// add records one observed row: the logical key and value bytes the read returned.
func (o *transactionObservation) add(key, value []byte) {
	o.rows++
	o.bytes += uint64(len(key) + len(value))
}

// stats returns this transaction's counters block, creating a detached one only if
// a transaction was built without registration.
func (t *Tx) stats() *transactionCounters {
	if t.counters == nil {
		t.counters = newTransactionCounters()
	}
	return t.counters
}

// observePointRead records one Get. Only counters change: a transaction never keeps
// the keys it read.
func (t *Tx) observePointRead(observation transactionObservation) {
	counters := t.stats()
	counters.pointReads.Add(1)
	if observation.rows != 0 {
		counters.rowsObserved.Add(observation.rows)
		counters.bytesObserved.Add(observation.bytes)
	}
}

// observeRangeRead records one Scan or ScanRange call.
func (t *Tx) observeRangeRead(observation transactionObservation) {
	counters := t.stats()
	counters.rangeReads.Add(1)
	if observation.rows != 0 {
		counters.rowsObserved.Add(observation.rows)
		counters.bytesObserved.Add(observation.bytes)
	}
}

// stagedOpKind classifies one logical key of the write set for diagnostics.
type stagedOpKind uint8

const (
	stagedOpNone stagedOpKind = iota
	stagedOpWrite
	stagedOpPointDependency
	stagedOpRangeDependency
)

// stagedOpKindOf classifies an encoded staged op from its flag byte and namespace,
// without decoding the payload. Missing input is stagedOpNone, so the first write
// to a key and a replacement are handled by the same move.
func stagedOpKindOf(k, encoded []byte) stagedOpKind {
	if len(encoded) < 2 {
		return stagedOpNone
	}
	if encoded[1]&2 == 0 {
		return stagedOpWrite
	}
	if bytes.HasPrefix(k, rangeGuardPrefix) {
		return stagedOpRangeDependency
	}
	return stagedOpPointDependency
}

// stagedOpByteCost is the diagnostics byte cost of one staged op, in the same unit
// the write-set budget uses.
func stagedOpByteCost(k, encoded []byte) int64 {
	return int64(len(k) + len(encoded))
}

// trackStagedOp reclassifies one logical key after a write.
//
// The write-set counters describe the set as it stands now: a replacement removes
// the previous contribution and adds the new one, so "Put A=1; Put A=2" reports one
// write, not two. A guard over a key that is already written never reaches here,
// because bufferWrite returns early for it.
func (t *Tx) trackStagedOp(k, previous, current []byte) {
	counters := t.stats()
	previousKind, currentKind := stagedOpKindOf(k, previous), stagedOpKindOf(k, current)
	moveCounter(&counters.writes, previousKind == stagedOpWrite, currentKind == stagedOpWrite)
	moveCounter(&counters.pointDependencies, previousKind == stagedOpPointDependency, currentKind == stagedOpPointDependency)
	moveCounter(&counters.rangeDependencies, previousKind == stagedOpRangeDependency, currentKind == stagedOpRangeDependency)
	counters.writeBytes.Add(writeByteCost(k, currentKind, current) - writeByteCost(k, previousKind, previous))
}

// writeByteCost reports how many write-set bytes one staged op contributes. A
// guard is a dependency, not a write, so it contributes none.
func writeByteCost(k []byte, kind stagedOpKind, encoded []byte) int64 {
	if kind != stagedOpWrite {
		return 0
	}
	return stagedOpByteCost(k, encoded)
}

// moveCounter applies a +1/-1 move from one classification of the same logical key
// to another.
func moveCounter(counter *atomic.Uint64, was, is bool) {
	switch {
	case is && !was:
		counter.Add(1)
	case was && !is:
		counter.Add(counterDecrement)
	}
}
