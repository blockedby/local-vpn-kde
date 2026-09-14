package state

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const transactionDirectory = "transactions"

// TransactionPhase is persisted before each externally visible transaction
// step. A journal remains recoverable until CompleteTransaction removes it.
type TransactionPhase string

const (
	PhasePrepared            TransactionPhase = "prepared"
	PhaseRuntimeAcknowledged TransactionPhase = "runtime-acknowledged"
	PhaseStateCommitted      TransactionPhase = "state-committed"
)

// TransactionOperation identifies which selected-state pair should be
// recovered after a process crash. Empty is accepted for journals written by
// older callers; the command layer then uses its configured runtime.
type TransactionOperation string

const (
	TransactionApply    TransactionOperation = "apply"
	TransactionRollback TransactionOperation = "rollback"
)

// Transaction contains opaque bytes loaded from a journal's private artifact
// files. The bytes are never included in errors or logs.
type Transaction struct {
	ID                    string
	Phase                 TransactionPhase
	Operation             TransactionOperation
	Runtime               string
	ConfigPath            string
	OldRuntime            []byte
	CandidateRuntime      []byte
	CandidateRuntimeReady bool
	OldState              Snapshot
	CandidateState        Snapshot
	CreatedAt             time.Time
	UpdatedAt             time.Time
}

type transactionJournal struct {
	Version               int                  `json:"version"`
	ID                    string               `json:"id"`
	Phase                 TransactionPhase     `json:"phase"`
	Operation             TransactionOperation `json:"operation,omitempty"`
	Runtime               string               `json:"runtime,omitempty"`
	ConfigPath            string               `json:"config_path,omitempty"`
	OldRuntime            string               `json:"old_runtime"`
	CandidateRuntime      string               `json:"candidate_runtime"`
	CandidateRuntimeReady bool                 `json:"candidate_runtime_ready,omitempty"`
	OldState              string               `json:"old_state"`
	CandidateState        string               `json:"candidate_state"`
	CreatedAt             time.Time            `json:"created_at"`
	UpdatedAt             time.Time            `json:"updated_at"`
}

const (
	transactionVersion   = 1
	journalFile          = "journal.json"
	oldRuntimeFile       = "old-runtime.bin"
	candidateRuntimeFile = "candidate-runtime.bin"
	oldStateFile         = "old-state.json"
	candidateStateFile   = "candidate-state.json"
)

func TransactionsDir(stateDir string) string { return filepath.Join(stateDir, transactionDirectory) }

// BeginTransaction is the compatibility form for callers that do not need
// operation metadata. A nil candidate runtime is a prepared transaction whose
// candidate is filled by AcknowledgeTransaction after the runtime returns.
func BeginTransaction(stateDir, id string, oldRuntime, candidateRuntime []byte, oldState, candidateState Snapshot) error {
	return BeginTransactionWithMetadata(stateDir, id, "", "", "", oldRuntime, candidateRuntime, oldState, candidateState)
}

// BeginTransactionWithMetadata durably writes all opaque artifacts before
// publishing journal.json. The journal is the commit/recovery record; its
// payload contains only paths and phase metadata, never node links or config
// contents.
func BeginTransactionWithMetadata(stateDir, id string, operation TransactionOperation, runtime, configPath string, oldRuntime, candidateRuntime []byte, oldState, candidateState Snapshot) error {
	if err := validateTransactionID(id); err != nil {
		return err
	}
	if oldRuntime == nil {
		return fmt.Errorf("transaction old runtime is nil")
	}
	if pending, err := PendingTransactions(stateDir); err != nil {
		return err
	} else if len(pending) != 0 {
		return fmt.Errorf("pending transaction exists")
	}
	dir := filepath.Join(TransactionsDir(stateDir), id)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	artifacts := []struct {
		name string
		data []byte
	}{
		{oldRuntimeFile, oldRuntime},
		{candidateRuntimeFile, candidateRuntime},
		{oldStateFile, marshalSnapshot(oldState)},
		{candidateStateFile, marshalSnapshot(candidateState)},
	}
	for _, artifact := range artifacts {
		if artifact.data == nil {
			// A nil candidate is represented by an empty artifact and marked
			// not-ready in the journal. Empty runtime configs are invalid, so
			// this cannot be confused with a valid acknowledged candidate.
			if artifact.name != candidateRuntimeFile {
				return fmt.Errorf("transaction artifact is nil")
			}
			artifact.data = []byte{}
		}
		if err := writeFileAtomicDurable(filepath.Join(dir, artifact.name), artifact.data, 0600); err != nil {
			return err
		}
	}
	now := time.Now().UTC()
	j := transactionJournal{
		Version:               transactionVersion,
		ID:                    id,
		Phase:                 PhasePrepared,
		Operation:             operation,
		Runtime:               strings.TrimSpace(runtime),
		ConfigPath:            configPath,
		OldRuntime:            oldRuntimeFile,
		CandidateRuntime:      candidateRuntimeFile,
		CandidateRuntimeReady: candidateRuntime != nil,
		OldState:              oldStateFile,
		CandidateState:        candidateStateFile,
		CreatedAt:             now,
		UpdatedAt:             now,
	}
	if err := writeJSONDurable(filepath.Join(dir, journalFile), j); err != nil {
		return err
	}
	return syncDir(TransactionsDir(stateDir))
}

func LoadTransaction(stateDir, id string) (Transaction, error) {
	if err := validateTransactionID(id); err != nil {
		return Transaction{}, err
	}
	j, err := loadJournal(stateDir, id)
	if err != nil {
		return Transaction{}, err
	}
	dir := filepath.Join(TransactionsDir(stateDir), id)
	oldRuntime, err := os.ReadFile(filepath.Join(dir, j.OldRuntime))
	if err != nil {
		return Transaction{}, err
	}
	candidateRuntime, err := os.ReadFile(filepath.Join(dir, j.CandidateRuntime))
	if err != nil {
		return Transaction{}, err
	}
	oldState, err := loadSnapshotArtifact(filepath.Join(dir, j.OldState))
	if err != nil {
		return Transaction{}, err
	}
	candidateState, err := loadSnapshotArtifact(filepath.Join(dir, j.CandidateState))
	if err != nil {
		return Transaction{}, err
	}
	return Transaction{
		ID:                    j.ID,
		Phase:                 j.Phase,
		Operation:             j.Operation,
		Runtime:               j.Runtime,
		ConfigPath:            j.ConfigPath,
		OldRuntime:            oldRuntime,
		CandidateRuntime:      candidateRuntime,
		CandidateRuntimeReady: j.CandidateRuntimeReady,
		OldState:              oldState,
		CandidateState:        candidateState,
		CreatedAt:             j.CreatedAt,
		UpdatedAt:             j.UpdatedAt,
	}, nil
}

// TransactionArtifactPath returns a validated private artifact path for a
// loaded transaction. It is used by runtime packages during crash recovery.
func TransactionArtifactPath(stateDir, id, name string) (string, error) {
	if err := validateTransactionID(id); err != nil {
		return "", err
	}
	switch name {
	case oldRuntimeFile, candidateRuntimeFile, oldStateFile, candidateStateFile:
		return filepath.Join(TransactionsDir(stateDir), id, name), nil
	default:
		return "", fmt.Errorf("invalid transaction artifact")
	}
}

func TransactionOldRuntimePath(stateDir, id string) (string, error) {
	return TransactionArtifactPath(stateDir, id, oldRuntimeFile)
}

func TransactionCandidateRuntimePath(stateDir, id string) (string, error) {
	return TransactionArtifactPath(stateDir, id, candidateRuntimeFile)
}

// PendingTransactions lists valid journal directories in deterministic ID
// order. A directory without a journal is an orphan artifact and is left for
// PruneOrphanTransactionArtifacts; it is never treated as a transaction.
func PendingTransactions(stateDir string) ([]Transaction, error) {
	entries, err := os.ReadDir(TransactionsDir(stateDir))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() && validateTransactionID(entry.Name()) == nil {
			if _, statErr := os.Stat(filepath.Join(TransactionsDir(stateDir), entry.Name(), journalFile)); os.IsNotExist(statErr) {
				continue
			} else if statErr != nil {
				return nil, statErr
			}
			ids = append(ids, entry.Name())
		}
	}
	sort.Strings(ids)
	out := make([]Transaction, 0, len(ids))
	for _, id := range ids {
		tx, err := LoadTransaction(stateDir, id)
		if err != nil {
			// Runtime/state bytes are required to re-prove the selected pair.
			// Even a state-committed journal is retained when an artifact is
			// missing; silently removing it would turn cleanup durability into a
			// false health acknowledgement.
			return nil, err
		}
		out = append(out, tx)
	}
	return out, nil
}

// AcknowledgeTransaction publishes the candidate runtime bytes and the
// runtime-acknowledged phase as one ordered durable transition. The candidate
// artifact is synced before the journal phase, so a crash between those writes
// safely re-enters PhasePrepared and restores the old pair.
func AcknowledgeTransaction(stateDir, id string, candidateRuntime []byte) error {
	if candidateRuntime == nil {
		return fmt.Errorf("transaction candidate runtime is nil")
	}
	j, err := loadJournal(stateDir, id)
	if err != nil {
		return err
	}
	if j.Phase != PhasePrepared {
		return fmt.Errorf("invalid transaction phase transition")
	}
	path, err := TransactionCandidateRuntimePath(stateDir, id)
	if err != nil {
		return err
	}
	if err := writeFileAtomicDurable(path, candidateRuntime, 0600); err != nil {
		return err
	}
	j.CandidateRuntimeReady = true
	j.Phase = PhaseRuntimeAcknowledged
	j.UpdatedAt = time.Now().UTC()
	if err := writeJSONDurable(filepath.Join(TransactionsDir(stateDir), id, journalFile), j); err != nil {
		return err
	}
	return syncDir(filepath.Join(TransactionsDir(stateDir), id))
}

func UpdateTransactionPhase(stateDir, id string, phase TransactionPhase) error {
	if err := validateTransactionID(id); err != nil {
		return err
	}
	if !validPhase(phase) {
		return fmt.Errorf("invalid transaction phase")
	}
	j, err := loadJournal(stateDir, id)
	if err != nil {
		return err
	}
	if phaseRank(phase) != phaseRank(j.Phase)+1 {
		return fmt.Errorf("invalid transaction phase transition")
	}
	if phase == PhaseStateCommitted && !j.CandidateRuntimeReady {
		return fmt.Errorf("transaction candidate runtime is not acknowledged")
	}
	j.Phase, j.UpdatedAt = phase, time.Now().UTC()
	if err := writeJSONDurable(filepath.Join(TransactionsDir(stateDir), id, journalFile), j); err != nil {
		return err
	}
	return syncDir(filepath.Join(TransactionsDir(stateDir), id))
}

func CompleteTransaction(stateDir, id string) error {
	if err := validateTransactionID(id); err != nil {
		return err
	}
	j, err := loadJournal(stateDir, id)
	if err != nil {
		return err
	}
	if j.Phase != PhaseStateCommitted {
		return fmt.Errorf("transaction is not state-committed")
	}
	if err := os.RemoveAll(filepath.Join(TransactionsDir(stateDir), id)); err != nil {
		return err
	}
	return syncDir(TransactionsDir(stateDir))
}

// PruneOrphanTransactionArtifacts removes only entries that cannot be an
// active journal. Valid journals, including prepared journals from a crash,
// are retained for recovery. Call it under the state-dir lock.
func PruneOrphanTransactionArtifacts(stateDir string) error {
	root := TransactionsDir(stateDir)
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		path := filepath.Join(root, entry.Name())
		if !entry.IsDir() {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return err
			}
			continue
		}
		if validateTransactionID(entry.Name()) != nil {
			if err := os.RemoveAll(path); err != nil {
				return err
			}
			continue
		}
		if _, err := os.Stat(filepath.Join(path, journalFile)); os.IsNotExist(err) {
			if err := os.RemoveAll(path); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
	}
	return syncDir(root)
}

// MigrateLegacyTransactionArtifacts creates the transaction namespace and
// removes no valid journal. The old flat namespace is intentionally handled
// conservatively: only clearly orphaned temporary files are moved into the
// prune path; no guessed journal is synthesized from incomplete data.
func MigrateLegacyTransactionArtifacts(stateDir string) error {
	root := TransactionsDir(stateDir)
	if err := os.MkdirAll(root, 0700); err != nil {
		return err
	}
	legacy := filepath.Join(stateDir, "transaction-artifacts")
	entries, err := os.ReadDir(legacy)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	orphan := filepath.Join(root, ".legacy-orphans")
	if err := os.MkdirAll(orphan, 0700); err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		src := filepath.Join(legacy, entry.Name())
		dst := filepath.Join(orphan, entry.Name())
		if _, statErr := os.Stat(dst); statErr == nil {
			if err := compareFiles(src, dst); err != nil {
				return err
			}
			if err := os.Remove(src); err != nil {
				return err
			}
			continue
		} else if !os.IsNotExist(statErr) {
			return statErr
		}
		if err := os.Rename(src, dst); err != nil {
			return err
		}
	}
	return syncDir(root)
}

func validateTransactionID(id string) error {
	if id == "" || id == "." || id == ".." || len(id) > 128 || filepath.Base(id) != id || filepath.IsAbs(id) {
		return fmt.Errorf("invalid transaction id")
	}
	for _, r := range id {
		if !(r == '-' || r == '_' || r == '.' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z') {
			return fmt.Errorf("invalid transaction id")
		}
	}
	return nil
}

func validPhase(p TransactionPhase) bool {
	return p == PhasePrepared || p == PhaseRuntimeAcknowledged || p == PhaseStateCommitted
}

func phaseRank(p TransactionPhase) int {
	switch p {
	case PhasePrepared:
		return 1
	case PhaseRuntimeAcknowledged:
		return 2
	case PhaseStateCommitted:
		return 3
	}
	return 0
}

func loadJournal(stateDir, id string) (transactionJournal, error) {
	var j transactionJournal
	b, err := os.ReadFile(filepath.Join(TransactionsDir(stateDir), id, journalFile))
	if err != nil {
		return j, err
	}
	if err := json.Unmarshal(b, &j); err != nil {
		return j, err
	}
	if j.Version != transactionVersion || j.ID != id || !validPhase(j.Phase) || j.OldRuntime != oldRuntimeFile || j.CandidateRuntime != candidateRuntimeFile || j.OldState != oldStateFile || j.CandidateState != candidateStateFile {
		return j, fmt.Errorf("invalid transaction journal")
	}
	if j.Phase != PhasePrepared && !j.CandidateRuntimeReady {
		return j, fmt.Errorf("invalid transaction journal candidate state")
	}
	return j, nil
}

func marshalSnapshot(s Snapshot) []byte {
	b, _ := json.Marshal(s.persisted())
	return append(b, '\n')
}

func loadSnapshotArtifact(path string) (Snapshot, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Snapshot{}, err
	}
	var p persistedSnapshot
	if err := json.Unmarshal(b, &p); err != nil {
		return Snapshot{}, err
	}
	return p.snapshot(), nil
}

func writeJSONDurable(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomicDurable(path, append(b, '\n'), 0600)
}

func writeFileAtomicDurable(path string, b []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".txn-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Chmod(perm); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return syncDir(dir)
}

func syncDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func compareFiles(a, b string) error {
	x, err := os.ReadFile(a)
	if err != nil {
		return err
	}
	y, err := os.ReadFile(b)
	if err != nil {
		return err
	}
	if !bytes.Equal(x, y) {
		return fmt.Errorf("transaction artifact migration conflict")
	}
	return nil
}
