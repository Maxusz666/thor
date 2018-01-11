package statedb

import (
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/vechain/thor/stackedmap"
	"github.com/vechain/thor/thor"
	"github.com/vechain/thor/vm/evm"
)

var _ evm.StateDB = (*StateDB)(nil)

// StateDB is facade for account.Manager, snapshot.Snapshot and Log.
// It implements evm.StateDB, only adapt to evm.
type StateDB struct {
	state State
	repo  *stackedmap.StackedMap
}

type suicideFlagKey common.Address
type preimageKey common.Hash
type refundKey struct{}
type logKey struct{}

// New create a statedb object.
func New(state State) *StateDB {
	getter := func(k interface{}) (interface{}, bool) {
		switch k.(type) {
		case suicideFlagKey:
			return false, true
		case refundKey:
			return &big.Int{}, true
		case preimageKey:
			return []byte(nil), true
		case logKey:
			return (*types.Log)(nil), true
		}
		panic(fmt.Sprintf("unknown type of key %+v", k))
	}

	repo := stackedmap.New(getter)
	return &StateDB{
		state,
		repo,
	}
}

// GetRefund returns total refund during VM life-cycle.
func (s *StateDB) GetRefund() *big.Int {
	v, _ := s.repo.Get(refundKey{})
	return v.(*big.Int)
}

// GetPreimages returns preimages produced by VM when evm.Config.EnablePreimageRecording turned on.
func (s *StateDB) GetPreimages(cb func(thor.Hash, []byte) bool) {
	s.repo.Journal(func(k, v interface{}) bool {
		if key, ok := k.(preimageKey); ok {
			return cb(thor.Hash(key), v.([]byte))
		}
		return true
	})
}

// GetLogs return the logs collected during VM life-cycle.
func (s *StateDB) GetLogs(cb func(*Log) bool) {
	s.repo.Journal(func(k, v interface{}) bool {
		if _, ok := k.(logKey); ok {
			return cb(v.(*Log))
		}
		return true
	})
}

// ForEachStorage see state.State.ForEachStorage.
func (s *StateDB) ForEachStorage(addr common.Address, cb func(common.Hash, common.Hash) bool) {
	s.state.ForEachStorage(thor.Address(addr), func(k thor.Hash, v thor.Hash) bool {
		return cb(common.Hash(k), common.Hash(v))
	})
}

// CreateAccount stub.
func (s *StateDB) CreateAccount(addr common.Address) {}

// GetBalance stub.
func (s *StateDB) GetBalance(addr common.Address) *big.Int {
	return s.state.GetBalance(thor.Address(addr))
}

// SubBalance stub.
func (s *StateDB) SubBalance(addr common.Address, amount *big.Int) {
	if amount.Sign() == 0 {
		return
	}
	balance := s.state.GetBalance(thor.Address(addr))
	s.state.SetBalance(thor.Address(addr), new(big.Int).Sub(balance, amount))
}

// AddBalance stub.
func (s *StateDB) AddBalance(addr common.Address, amount *big.Int) {
	if amount.Sign() == 0 {
		return
	}
	balance := s.state.GetBalance(thor.Address(addr))
	s.state.SetBalance(thor.Address(addr), new(big.Int).Add(balance, amount))
}

// GetNonce stub.
func (s *StateDB) GetNonce(addr common.Address) uint64 { return 0 }

// SetNonce stub.
func (s *StateDB) SetNonce(addr common.Address, nonce uint64) {}

// GetCodeHash stub.
func (s *StateDB) GetCodeHash(addr common.Address) common.Hash {
	return common.Hash(s.state.GetCodeHash(thor.Address(addr)))
}

// GetCode stub.
func (s *StateDB) GetCode(addr common.Address) []byte {
	return s.state.GetCode(thor.Address(addr))
}

// GetCodeSize stub.
func (s *StateDB) GetCodeSize(addr common.Address) int {
	return len(s.state.GetCode(thor.Address(addr)))
}

// SetCode stub.
func (s *StateDB) SetCode(addr common.Address, code []byte) {
	s.state.SetCode(thor.Address(addr), code)
}

// HasSuicided stub.
func (s *StateDB) HasSuicided(addr common.Address) bool {
	// only check suicide flag here
	v, _ := s.repo.Get(suicideFlagKey(addr))
	return v.(bool)
}

// Suicide stub.
// We do two things:
// 1, delete account
// 2, set suicide flag
func (s *StateDB) Suicide(addr common.Address) bool {
	if !s.state.Exists(thor.Address(addr)) {
		return false
	}
	s.state.Delete(thor.Address(addr))
	s.repo.Put(suicideFlagKey(addr), true)
	return true
}

// GetState stub.
func (s *StateDB) GetState(addr common.Address, key common.Hash) common.Hash {
	return common.Hash(s.state.GetStorage(thor.Address(addr), thor.Hash(key)))
}

// SetState stub.
func (s *StateDB) SetState(addr common.Address, key, value common.Hash) {
	s.state.SetStorage(thor.Address(addr), thor.Hash(key), thor.Hash(value))
}

// Exist stub.
func (s *StateDB) Exist(addr common.Address) bool {
	return s.state.Exists(thor.Address(addr))
}

// Empty stub.
func (s *StateDB) Empty(addr common.Address) bool {
	return !s.state.Exists(thor.Address(addr))
}

// AddRefund stub.
func (s *StateDB) AddRefund(gas *big.Int) {
	v, _ := s.repo.Get(refundKey{})
	total := new(big.Int).Add(v.(*big.Int), gas)
	s.repo.Put(refundKey{}, total)
}

// AddPreimage stub.
func (s *StateDB) AddPreimage(hash common.Hash, preimage []byte) {
	s.repo.Put(preimageKey(hash), preimage)
}

// AddLog stub.
func (s *StateDB) AddLog(vmlog *types.Log) {
	s.repo.Put(logKey{}, vmlogToLog(vmlog))
}

// Snapshot stub.
func (s *StateDB) Snapshot() int {
	s.state.NewCheckpoint()
	rev := s.repo.Push()
	return rev
}

// RevertToSnapshot stub.
func (s *StateDB) RevertToSnapshot(rev int) {
	if rev < 0 || rev > s.repo.Depth() {
		panic(fmt.Sprintf("invalid snapshot revision %d (depth:%d)", rev, s.repo.Depth()))
	}
	revertCount := s.repo.Depth() - rev
	for i := 0; i < revertCount; i++ {
		s.state.Revert()
	}
	s.repo.PopTo(rev)
}

// Log represents a contract log event. These events are generated by the LOG opcode and
// stored/indexed by the node.
type Log struct {
	// address of the contract that generated the event
	Address thor.Address
	// list of topics provided by the contract.
	Topics []thor.Hash
	// supplied by the contract, usually ABI-encoded
	Data []byte
}

func vmlogToLog(vmlog *types.Log) *Log {
	var topics []thor.Hash
	if len(vmlog.Topics) > 0 {
		for _, t := range vmlog.Topics {
			topics = append(topics, thor.Hash(t))
		}
	}
	return &Log{
		thor.Address(vmlog.Address),
		topics,
		vmlog.Data,
	}
}