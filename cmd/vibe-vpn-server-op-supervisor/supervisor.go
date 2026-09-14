//go:build linux

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	productionStateRoot = "/run/vpnkit/server-operations"
	productionRunner    = "/usr/local/bin/vibe-vpn-server-op-supervisor"
	productionWorker    = "/usr/local/bin/vibe-vpn"
	productionConfig    = "/etc/vibe-vpn/config.yaml"
	defaultOutputLimit  = 1 << 20 // 1,000 display records, including escaped Unicode names
)

var (
	tokenPattern    = regexp.MustCompile(`^op_[A-Za-z0-9_-]{43}$`)
	serverIDPattern = regexp.MustCompile(`^srv_[A-Za-z0-9_-]{27}$`)
	errOpen         = errors.New("operation is still open")
)

type supervisorConfig struct {
	stateRoot          string
	runner             string
	worker             string
	workerConfig       string
	procRoot           string
	termGrace          time.Duration
	killGrace          time.Duration
	pollInterval       time.Duration
	outputLimit        int
	workerEnv          []string              // test-only injection; production leaves this nil.
	processes          processSystem         // test-only injection; production leaves this nil.
	afterFallbackProof func(operationRecord) // test-only race injection.
}

func newProductionSupervisor() *supervisor {
	return newSupervisor(supervisorConfig{
		stateRoot:    productionStateRoot,
		runner:       productionRunner,
		worker:       productionWorker,
		workerConfig: productionConfig,
		procRoot:     "/proc",
		termGrace:    500 * time.Millisecond,
		killGrace:    10 * time.Second,
		pollInterval: 10 * time.Millisecond,
		outputLimit:  defaultOutputLimit,
	})
}

type operationRequest struct {
	token     string
	operation string
	serverID  string
	targetURL string
}

func parseOperationRequest(args []string) (operationRequest, error) {
	if len(args) < 2 || len(args) > 4 || !tokenPattern.MatchString(args[0]) {
		return operationRequest{}, errors.New("invalid operation request")
	}
	request := operationRequest{token: args[0], operation: args[1]}
	switch request.operation {
	case "check-batch":
		if len(args) != 4 {
			return operationRequest{}, errors.New("invalid batch")
		}
		ids := strings.Split(args[2], ",")
		if len(ids) > 5 {
			return operationRequest{}, errors.New("invalid batch")
		}
		seen := map[string]bool{}
		for _, id := range ids {
			if !serverIDPattern.MatchString(id) || seen[id] {
				return operationRequest{}, errors.New("invalid batch id")
			}
			seen[id] = true
		}
		u, err := url.Parse(args[3])
		if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || len(args[3]) > 2048 {
			return operationRequest{}, errors.New("invalid target")
		}
		request.serverID, request.targetURL = args[2], args[3]
	case "availability":
		if len(args) != 4 || !serverIDPattern.MatchString(args[2]) {
			return operationRequest{}, errors.New("invalid request")
		}
		u, err := url.Parse(args[3])
		if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || len(args[3]) > 2048 {
			return operationRequest{}, errors.New("invalid target")
		}
		request.serverID, request.targetURL = args[2], args[3]

	case "list", "refresh", "test-all", "current":
		if len(args) != 2 {
			return operationRequest{}, errors.New("unexpected server id")
		}
	case "ping", "speed", "select":
		if len(args) != 3 || !serverIDPattern.MatchString(args[2]) {
			return operationRequest{}, errors.New("invalid server id")
		}
		request.serverID = args[2]
	default:
		return operationRequest{}, errors.New("unsupported operation")
	}
	return request, nil
}

func workerArgv(config supervisorConfig, request operationRequest) []string {
	args := []string{"--config", config.workerConfig, request.operation}
	if request.serverID != "" {
		args = append(args, "--server-id", request.serverID)
	}
	if request.targetURL != "" {
		args = append(args, "--url", request.targetURL)
	}
	return append(args, "--json")
}

type operationRecord struct {
	TargetURL   string `json:"target_url,omitempty"`
	Version     int    `json:"version"`
	Token       string `json:"token"`
	Operation   string `json:"operation"`
	ServerID    string `json:"server_id,omitempty"`
	Status      string `json:"status"`
	RunnerPID   int    `json:"runner_pid,omitempty"`
	RunnerStart uint64 `json:"runner_start,omitempty"`
	WorkerPID   int    `json:"worker_pid,omitempty"`
	WorkerStart uint64 `json:"worker_start,omitempty"`
	PGID        int    `json:"pgid,omitempty"`
	Session     int    `json:"session,omitempty"`
}

func (record operationRecord) request() operationRequest {
	return operationRequest{token: record.Token, operation: record.Operation, serverID: record.ServerID, targetURL: record.TargetURL}
}

func (record operationRecord) sameProcess(other operationRecord) bool {
	return record.Token == other.Token &&
		record.Operation == other.Operation &&
		record.ServerID == other.ServerID && record.TargetURL == other.TargetURL &&
		record.RunnerPID == other.RunnerPID &&
		record.RunnerStart == other.RunnerStart &&
		record.WorkerPID == other.WorkerPID &&
		record.WorkerStart == other.WorkerStart &&
		record.PGID == other.PGID &&
		record.Session == other.Session
}

type supervisor struct {
	config    supervisorConfig
	processes processSystem
}

func newSupervisor(config supervisorConfig) *supervisor {
	if config.procRoot == "" {
		config.procRoot = "/proc"
	}
	if config.runner == "" {
		config.runner = productionRunner
	}
	if config.termGrace <= 0 {
		config.termGrace = 500 * time.Millisecond
	}
	if config.killGrace <= 0 {
		config.killGrace = 10 * time.Second
	}
	if config.pollInterval <= 0 {
		config.pollInterval = 10 * time.Millisecond
	}
	if config.outputLimit <= 0 {
		config.outputLimit = defaultOutputLimit
	}
	processes := config.processes
	if processes == nil {
		processes = &linuxProcessSystem{procRoot: config.procRoot}
	}
	return &supervisor{config: config, processes: processes}
}

type stateLock struct {
	file *os.File
}

func (lock *stateLock) close() {
	if lock == nil || lock.file == nil {
		return
	}
	_ = syscall.Flock(int(lock.file.Fd()), syscall.LOCK_UN)
	_ = lock.file.Close()
}

func (supervisor *supervisor) lockState() (*stateLock, error) {
	if supervisor.config.stateRoot == "" {
		return nil, errors.New("state root is empty")
	}
	if err := os.MkdirAll(supervisor.config.stateRoot, 0o700); err != nil {
		return nil, err
	}
	rootInfo, err := os.Lstat(supervisor.config.stateRoot)
	if err != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("unsafe state root")
	}
	if rootInfo.Mode().Perm() != 0o700 {
		return nil, errors.New("state root is not private")
	}
	lockPath := filepath.Join(supervisor.config.stateRoot, ".lock")
	fd, err := syscall.Open(lockPath, syscall.O_CREAT|syscall.O_RDWR|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), lockPath)
	if file == nil {
		_ = syscall.Close(fd)
		return nil, errors.New("could not open state lock")
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || fileNlink(info) != 1 {
		_ = file.Close()
		return nil, errors.New("unsafe state lock")
	}
	if err := syscall.Flock(fd, syscall.LOCK_EX); err != nil {
		_ = file.Close()
		return nil, err
	}
	return &stateLock{file: file}, nil
}

func fileNlink(info os.FileInfo) uint64 {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0
	}
	return uint64(stat.Nlink)
}

func (supervisor *supervisor) tokenDir(token string) string {
	return filepath.Join(supervisor.config.stateRoot, token)
}

func (supervisor *supervisor) recordPath(token string) string {
	return filepath.Join(supervisor.tokenDir(token), "record.json")
}

func (supervisor *supervisor) validateTokenDir(token string) error {
	info, err := os.Lstat(supervisor.tokenDir(token))
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 {
		return errors.New("unsafe token state directory")
	}
	return nil
}

func (supervisor *supervisor) readRecord(token string) (operationRecord, error) {
	var record operationRecord
	if !tokenPattern.MatchString(token) {
		return record, errors.New("invalid token")
	}
	if err := supervisor.validateTokenDir(token); err != nil {
		return record, err
	}
	path := supervisor.recordPath(token)
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return record, err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = syscall.Close(fd)
		return record, errors.New("could not open operation record")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || fileNlink(info) != 1 {
		return record, errors.New("unsafe operation record")
	}
	decoder := json.NewDecoder(io.LimitReader(file, 4097))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil {
		return operationRecord{}, errors.New("malformed operation record")
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return operationRecord{}, errors.New("operation record has trailing data")
	}
	request, err := parseOperationRequest(recordArgs(record))
	if err != nil || request.token != token || record.Version != 1 {
		return operationRecord{}, errors.New("operation record identity mismatch")
	}
	switch record.Status {
	case "prepared":
		if record.RunnerPID != 0 || record.WorkerPID != 0 {
			return operationRecord{}, errors.New("prepared record contains process identity")
		}
	case "running", "cancelling", "closed":
		if record.WorkerPID != 0 && !validPublishedIdentity(record) {
			return operationRecord{}, errors.New("invalid published process identity")
		}
	default:
		return operationRecord{}, errors.New("invalid operation status")
	}
	return record, nil
}

func recordArgs(record operationRecord) []string {
	args := []string{record.Token, record.Operation}
	if record.ServerID != "" {
		args = append(args, record.ServerID)
	}
	if record.TargetURL != "" {
		args = append(args, record.TargetURL)
	}
	return args
}

func validPublishedIdentity(record operationRecord) bool {
	return record.RunnerPID > 1 && record.RunnerStart > 0 &&
		record.WorkerPID > 1 && record.WorkerStart > 0 &&
		record.PGID == record.WorkerPID && record.Session == record.WorkerPID
}

func (supervisor *supervisor) writeRecord(record operationRecord) error {
	if !tokenPattern.MatchString(record.Token) {
		return errors.New("invalid record token")
	}
	if err := supervisor.validateTokenDir(record.Token); err != nil {
		return err
	}
	encoded, err := json.Marshal(record)
	if err != nil || len(encoded) > 4096 {
		return errors.New("could not encode operation record")
	}
	// CreateTemp uses O_EXCL, so the private token directory cannot redirect
	// this atomic stage through a pre-existing link after validation.
	temporary, err := os.CreateTemp(supervisor.tokenDir(record.Token), ".record-")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	cleanup := true
	defer func() {
		_ = temporary.Close()
		if cleanup {
			_ = os.Remove(temporaryPath)
		}
	}()
	if _, err := temporary.Write(encoded); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, supervisor.recordPath(record.Token)); err != nil {
		return err
	}
	cleanup = false
	return nil
}

func (supervisor *supervisor) removeRecord(record operationRecord) error {
	current, err := supervisor.readRecord(record.Token)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if !record.sameProcess(current) && !(record.Status == "prepared" && current.Status == "closed") {
		return errors.New("operation identity changed before cleanup")
	}
	if err := os.Remove(supervisor.recordPath(record.Token)); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Remove(supervisor.tokenDir(record.Token)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (supervisor *supervisor) prepare(args []string) error {
	request, err := parseOperationRequest(args)
	if err != nil {
		return err
	}
	lock, err := supervisor.lockState()
	if err != nil {
		return err
	}
	defer lock.close()
	if _, err := os.Lstat(supervisor.tokenDir(request.token)); err == nil {
		return errors.New("operation token already exists")
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.Mkdir(supervisor.tokenDir(request.token), 0o700); err != nil {
		return err
	}
	record := operationRecord{
		Version:   1,
		Token:     request.token,
		Operation: request.operation,
		ServerID:  request.serverID,
		TargetURL: request.targetURL,
		Status:    "prepared",
	}
	if err := supervisor.writeRecord(record); err != nil {
		_ = os.Remove(supervisor.tokenDir(request.token))
		return err
	}
	return nil
}

type boundedBuffer struct {
	mutex    sync.Mutex
	buffer   bytes.Buffer
	limit    int
	overflow bool
}

func (buffer *boundedBuffer) Write(value []byte) (int, error) {
	buffer.mutex.Lock()
	defer buffer.mutex.Unlock()
	original := len(value)
	remaining := buffer.limit - buffer.buffer.Len()
	if remaining < len(value) {
		buffer.overflow = true
		if remaining < 0 {
			remaining = 0
		}
		value = value[:remaining]
	}
	_, _ = buffer.buffer.Write(value)
	return original, nil
}

func (buffer *boundedBuffer) result() ([]byte, bool) {
	buffer.mutex.Lock()
	defer buffer.mutex.Unlock()
	return append([]byte(nil), buffer.buffer.Bytes()...), buffer.overflow
}

type runResult struct {
	output   []byte
	exitCode int
}

func (supervisor *supervisor) run(args []string) (runResult, error) {
	request, err := parseOperationRequest(args)
	if err != nil {
		return runResult{}, err
	}

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(signals)

	lock, err := supervisor.lockState()
	if err != nil {
		return runResult{}, err
	}
	record, err := supervisor.readRecord(request.token)
	if err != nil || record.Status != "prepared" || record.request() != request {
		lock.close()
		return runResult{}, errors.New("operation is not prepared for this request")
	}

	captured := &boundedBuffer{limit: supervisor.config.outputLimit}
	command := exec.Command(supervisor.config.worker, workerArgv(supervisor.config, request)...)
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	command.Stdout = captured
	command.Stderr = io.Discard
	if supervisor.config.workerEnv != nil {
		command.Env = append(os.Environ(), supervisor.config.workerEnv...)
	}
	if err := command.Start(); err != nil {
		lock.close()
		return runResult{}, err
	}

	// Start made this helper the worker's parent. Nothing calls Wait until every
	// group drain below is complete, so a worker that exits remains an unreaped
	// child (and therefore a non-reusable PID/PGID/session ownership anchor).
	// Use that same anchor even for pre-publication cleanup; a proc check alone
	// is never used to authorize a numeric group signal.
	startedRecord := record
	startedRecord.Status = "running"
	startedRecord.RunnerPID = os.Getpid()
	startedRecord.WorkerPID = command.Process.Pid
	startedRecord.PGID = command.Process.Pid
	startedRecord.Session = command.Process.Pid
	runnerStat, runnerErr := supervisor.processes.stat(os.Getpid())
	workerStat, workerErr := supervisor.processes.stat(command.Process.Pid)
	if runnerErr == nil {
		startedRecord.RunnerStart = runnerStat.startTime
	}
	if workerErr == nil {
		startedRecord.WorkerStart = workerStat.startTime
	}
	if runnerErr != nil || workerErr != nil || workerStat.pgrp != command.Process.Pid || workerStat.session != command.Process.Pid {
		supervisor.killStartedWorker(command, startedRecord)
		lock.close()
		return runResult{}, errors.New("could not prove launched worker identity")
	}
	record.Status = "running"
	record.RunnerPID = os.Getpid()
	record.RunnerStart = runnerStat.startTime
	record.WorkerPID = command.Process.Pid
	record.WorkerStart = workerStat.startTime
	record.PGID = workerStat.pgrp
	record.Session = workerStat.session
	if err := supervisor.writeRecord(record); err != nil {
		supervisor.killStartedWorker(command, record)
		lock.close()
		return runResult{}, err
	}
	lock.close()

	cancelled, interrupted, err := supervisor.awaitWorker(record, signals)
	if err != nil {
		return runResult{}, err
	}

	// waitid(WNOWAIT) observes normal exit without reaping. On cancellation the
	// child is likewise still live or will become our zombie. Either way the
	// runner-owned anchor remains non-reusable through TERM, KILL, and drain.
	anchor := newRunnerChildAnchor(record)
	if err := supervisor.stopGroup(record, anchor); err != nil {
		return runResult{}, err
	}
	waitErr := command.Wait()
	anchor.release()

	lock, err = supervisor.lockState()
	if err != nil {
		return runResult{}, err
	}
	current, readErr := supervisor.readRecord(request.token)
	if readErr == nil {
		if !record.sameProcess(current) {
			lock.close()
			return runResult{}, errors.New("operation identity changed during run")
		}
		cancelled = cancelled || current.Status == "cancelling" || current.Status == "closed"
		if cancelled || interrupted {
			current.Status = "closed"
			err = supervisor.writeRecord(current)
		} else {
			err = supervisor.removeRecord(current)
		}
	} else if !os.IsNotExist(readErr) {
		err = readErr
	}
	lock.close()
	if err != nil {
		return runResult{}, err
	}
	if cancelled || interrupted {
		return runResult{exitCode: 125}, nil
	}

	exitCode := 0
	if waitErr != nil {
		var exitError *exec.ExitError
		if errors.As(waitErr, &exitError) {
			exitCode = exitError.ExitCode()
		} else {
			return runResult{}, waitErr
		}
	}
	output, overflow := captured.result()
	if overflow {
		return runResult{}, errors.New("worker output exceeded fixed limit")
	}
	return runResult{output: output, exitCode: exitCode}, nil
}

// killStartedWorker performs pre-publication cleanup while command.Process is
// still an unreaped child of this runner. The child relationship is the
// non-reusable ownership anchor for the numeric group signal.
func (supervisor *supervisor) killStartedWorker(command *exec.Cmd, record operationRecord) {
	anchor := newRunnerChildAnchor(record)
	_ = supervisor.signalGroup(record, syscall.SIGKILL, anchor)
	_, _ = command.Process.Wait()
	anchor.release()
}

// awaitWorker polls the authenticated private record for cancellation and uses
// waitid(WNOWAIT) to observe child exit without allowing PID/PGID reuse. It
// deliberately never calls Process.Wait.
func (supervisor *supervisor) awaitWorker(record operationRecord, signals <-chan os.Signal) (cancelled bool, interrupted bool, err error) {
	ticker := time.NewTicker(supervisor.config.pollInterval)
	defer ticker.Stop()
	for {
		status, statusErr := supervisor.operationStatus(record)
		if statusErr != nil {
			return false, interrupted, statusErr
		}
		if status == "cancelling" || status == "closed" {
			return true, interrupted, nil
		}
		exited, waitErr := supervisor.processes.childExitedNoReap(record.WorkerPID)
		if waitErr != nil {
			return false, interrupted, waitErr
		}
		if exited {
			return false, interrupted, nil
		}

		select {
		case <-signals:
			interrupted = true
			if err := supervisor.markCancellation(record); err != nil {
				return false, interrupted, err
			}
		case <-ticker.C:
		}
	}
}

func (supervisor *supervisor) operationStatus(record operationRecord) (string, error) {
	lock, err := supervisor.lockState()
	if err != nil {
		return "", err
	}
	defer lock.close()
	current, err := supervisor.readRecord(record.Token)
	if err != nil {
		return "", err
	}
	if !record.sameProcess(current) {
		return "", errors.New("operation identity changed while awaiting worker")
	}
	return current.Status, nil
}

func (supervisor *supervisor) markCancellation(record operationRecord) error {
	lock, err := supervisor.lockState()
	if err != nil {
		return err
	}
	defer lock.close()
	current, err := supervisor.readRecord(record.Token)
	if err != nil {
		return err
	}
	if !record.sameProcess(current) {
		return errors.New("operation identity changed before cancellation request")
	}
	if current.Status == "running" {
		current.Status = "cancelling"
		return supervisor.writeRecord(current)
	}
	if current.Status != "cancelling" && current.Status != "closed" {
		return errors.New("operation cannot be cancelled")
	}
	return nil
}

func (supervisor *supervisor) cancel(token string) error {
	if !tokenPattern.MatchString(token) {
		return errors.New("invalid token")
	}
	lock, err := supervisor.lockState()
	if err != nil {
		return err
	}
	record, err := supervisor.readRecord(token)
	if os.IsNotExist(err) {
		lock.close()
		return nil
	}
	if err != nil {
		lock.close()
		return err
	}
	if record.Status == "prepared" {
		record.Status = "closed"
		err = supervisor.writeRecord(record)
		lock.close()
		return err
	}
	if record.Status == "closed" {
		live, liveErr := supervisor.groupLive(record)
		lock.close()
		if liveErr != nil {
			return liveErr
		}
		if live {
			return errOpen
		}
		return nil
	}

	// The external helper is only a cancellation requester while the exact
	// runner is alive. Publishing this state under the private-record lock lets
	// that parent retain and use its unreaped child anchor for every group stop.
	if record.Status == "running" {
		record.Status = "cancelling"
		if err := supervisor.writeRecord(record); err != nil {
			lock.close()
			return err
		}
	}
	runnerState, identityErr := supervisor.runnerIdentity(record)
	if identityErr != nil {
		lock.close()
		return identityErr
	}
	if runnerState == identityLive {
		lock.close()
		return nil
	}

	// A runner-gone fallback has no non-reusable ownership anchor. Never
	// signal its numeric worker PID or process group: proc identity checks can
	// race both worker exit/reap and PGID reuse. The only safe fallback outcome
	// is closure after the exact recorded worker is gone and the group is empty.
	workerState, identityErr := supervisor.workerIdentity(record)
	if identityErr != nil {
		lock.close()
		return identityErr
	}
	if workerState == identityLive && supervisor.config.afterFallbackProof != nil {
		// Test-only hook: this is the exact gap an unsafe fallback would have
		// between its final identity proof and numeric group signal.
		supervisor.config.afterFallbackProof(record)
	}
	if workerState != identityGone {
		lock.close()
		return errOpen
	}
	live, liveErr := supervisor.groupLive(record)
	if liveErr != nil {
		lock.close()
		return liveErr
	}
	if live {
		lock.close()
		return errOpen
	}
	// Re-check after group enumeration. This still does not form an ownership
	// anchor and authorizes no signal; it only prevents a PID replacement that
	// appeared during the empty-group proof from being mistaken for a drain.
	workerState, identityErr = supervisor.workerIdentity(record)
	if identityErr != nil {
		lock.close()
		return identityErr
	}
	if workerState != identityGone {
		lock.close()
		return errOpen
	}
	live, liveErr = supervisor.groupLive(record)
	if liveErr != nil {
		lock.close()
		return liveErr
	}
	if live {
		lock.close()
		return errOpen
	}
	record.Status = "closed"
	err = supervisor.writeRecord(record)
	lock.close()
	return err
}

func (supervisor *supervisor) verify(token string) error {
	if !tokenPattern.MatchString(token) {
		return errors.New("invalid token")
	}
	lock, err := supervisor.lockState()
	if err != nil {
		return err
	}
	defer lock.close()
	record, err := supervisor.readRecord(token)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if record.Status == "prepared" {
		return errOpen
	}
	live, err := supervisor.groupLive(record)
	if err != nil {
		return err
	}
	if live {
		state, identityErr := supervisor.workerIdentity(record)
		if identityErr != nil {
			return identityErr
		}
		if state == identityMismatch {
			return errors.New("worker identity mismatch")
		}
		return errOpen
	}
	if record.Status == "running" || record.Status == "cancelling" {
		state, identityErr := supervisor.workerIdentity(record)
		if identityErr != nil || state != identityGone {
			return errOpen
		}
		record.Status = "closed"
		if err := supervisor.writeRecord(record); err != nil {
			return err
		}
	}
	return supervisor.removeRecord(record)
}

type procStat struct {
	state     byte
	pgrp      int
	session   int
	startTime uint64
}

type processSystem interface {
	stat(pid int) (procStat, error)
	cmdline(pid int) ([]string, error)
	pids() ([]int, error)
	childExitedNoReap(pid int) (bool, error)
	signalGroup(pgid int, signal syscall.Signal) error
}

type linuxProcessSystem struct {
	procRoot string
}

func (system *linuxProcessSystem) stat(pid int) (procStat, error) {
	return readProcStat(system.procRoot, pid)
}

func (system *linuxProcessSystem) cmdline(pid int) ([]string, error) {
	return readProcCmdline(system.procRoot, pid)
}

func (system *linuxProcessSystem) pids() ([]int, error) {
	entries, err := os.ReadDir(system.procRoot)
	if err != nil {
		return nil, err
	}
	pids := make([]int, 0, len(entries))
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err == nil && pid > 1 {
			pids = append(pids, pid)
		}
	}
	return pids, nil
}

func (system *linuxProcessSystem) childExitedNoReap(pid int) (bool, error) {
	var info unix.Siginfo
	err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOHANG|unix.WNOWAIT, nil)
	if err != nil {
		return false, err
	}
	return info.Signo != 0, nil
}

func (system *linuxProcessSystem) signalGroup(pgid int, signal syscall.Signal) error {
	return syscall.Kill(-pgid, signal)
}

func readProcStat(procRoot string, pid int) (procStat, error) {
	content, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "stat"))
	if err != nil {
		return procStat{}, err
	}
	closing := bytes.LastIndex(content, []byte(") "))
	if closing < 0 {
		return procStat{}, errors.New("malformed proc stat")
	}
	fields := strings.Fields(string(content[closing+2:]))
	if len(fields) < 20 || len(fields[0]) != 1 {
		return procStat{}, errors.New("malformed proc stat fields")
	}
	pgrp, err := strconv.Atoi(fields[2])
	if err != nil {
		return procStat{}, err
	}
	sessionID, err := strconv.Atoi(fields[3])
	if err != nil {
		return procStat{}, err
	}
	startTime, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return procStat{}, err
	}
	return procStat{state: fields[0][0], pgrp: pgrp, session: sessionID, startTime: startTime}, nil
}

func readProcCmdline(procRoot string, pid int) ([]string, error) {
	content, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return nil, err
	}
	content = bytes.TrimSuffix(content, []byte{0})
	if len(content) == 0 {
		return nil, nil
	}
	parts := bytes.Split(content, []byte{0})
	result := make([]string, len(parts))
	for index, part := range parts {
		result[index] = string(part)
	}
	return result, nil
}

func processExitedState(state byte) bool {
	return state == 'Z' || state == 'X' || state == 'x'
}

type identityState uint8

const (
	identityGone identityState = iota
	identityMismatch
	identityLive
	identityZombie
)

func cmdlineEqual(actual, expected []string) bool {
	if len(actual) != len(expected) {
		return false
	}
	for index := range expected {
		if actual[index] != expected[index] {
			return false
		}
	}
	return true
}

func (supervisor *supervisor) workerIdentity(record operationRecord) (identityState, error) {
	stat, err := supervisor.processes.stat(record.WorkerPID)
	if err != nil {
		if os.IsNotExist(err) {
			return identityGone, nil
		}
		return identityMismatch, err
	}
	if stat.startTime != record.WorkerStart || stat.pgrp != record.PGID || stat.session != record.Session {
		return identityMismatch, nil
	}
	if processExitedState(stat.state) {
		return identityZombie, nil
	}
	cmdline, err := supervisor.processes.cmdline(record.WorkerPID)
	if err == nil {
		expected := append([]string{supervisor.config.worker}, workerArgv(supervisor.config, record.request())...)
		if cmdlineEqual(cmdline, expected) {
			return identityLive, nil
		}
	} else if !os.IsNotExist(err) {
		return identityMismatch, err
	}

	// A live worker can become our zombie between stat and cmdline. Re-read the
	// stable identity before classifying an empty zombie cmdline as a mismatch.
	latest, latestErr := supervisor.processes.stat(record.WorkerPID)
	if latestErr != nil {
		if os.IsNotExist(latestErr) {
			return identityGone, nil
		}
		return identityMismatch, latestErr
	}
	if latest.startTime == record.WorkerStart && latest.pgrp == record.PGID && latest.session == record.Session && processExitedState(latest.state) {
		return identityZombie, nil
	}
	return identityMismatch, nil
}

func (supervisor *supervisor) runnerIdentity(record operationRecord) (identityState, error) {
	stat, err := supervisor.processes.stat(record.RunnerPID)
	if err != nil {
		if os.IsNotExist(err) {
			return identityGone, nil
		}
		return identityMismatch, err
	}
	if stat.startTime != record.RunnerStart {
		return identityMismatch, nil
	}
	if processExitedState(stat.state) {
		return identityZombie, nil
	}
	cmdline, err := supervisor.processes.cmdline(record.RunnerPID)
	if err != nil {
		if os.IsNotExist(err) {
			return identityGone, nil
		}
		return identityMismatch, err
	}
	if len(cmdline) == 0 || cmdline[0] != supervisor.config.runner {
		return identityMismatch, nil
	}
	return identityLive, nil
}

func (supervisor *supervisor) groupLive(record operationRecord) (bool, error) {
	if record.PGID <= 1 || record.Session <= 1 {
		return false, nil
	}
	pids, err := supervisor.processes.pids()
	if err != nil {
		return false, err
	}
	for _, pid := range pids {
		stat, err := supervisor.processes.stat(pid)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return false, err
		}
		if stat.pgrp == record.PGID && stat.session == record.Session && !processExitedState(stat.state) {
			return true, nil
		}
	}
	return false, nil
}

type runnerChildAnchor struct {
	record   operationRecord
	retained bool
}

func newRunnerChildAnchor(record operationRecord) *runnerChildAnchor {
	return &runnerChildAnchor{record: record, retained: true}
}

func (anchor *runnerChildAnchor) assertRetained(record operationRecord) error {
	if anchor == nil || !anchor.retained || !anchor.record.sameProcess(record) {
		return errors.New("runner no longer retains exact worker anchor")
	}
	return nil
}

func (anchor *runnerChildAnchor) release() {
	anchor.retained = false
}

func (supervisor *supervisor) waitGroupClosed(record operationRecord, timeout time.Duration, anchor *runnerChildAnchor) (bool, error) {
	deadline := time.Now().Add(timeout)
	for {
		if err := anchor.assertRetained(record); err != nil {
			return false, err
		}
		live, err := supervisor.groupLive(record)
		if err != nil {
			return false, err
		}
		if !live {
			if err := anchor.assertRetained(record); err != nil {
				return false, err
			}
			return true, nil
		}
		if time.Now().After(deadline) {
			return false, nil
		}
		time.Sleep(supervisor.config.pollInterval)
	}
}

func (supervisor *supervisor) signalGroup(record operationRecord, signal syscall.Signal, anchor *runnerChildAnchor) error {
	if record.PGID <= 1 || record.PGID == syscall.Getpgrp() || record.Session != record.PGID {
		return errors.New("refusing unsafe process group")
	}
	// This method is intentionally restricted to the runner-owned path. The
	// runner keeps its child unreaped from WNOWAIT observation through this
	// syscall and the complete drain, so the child PID/PGID cannot be reused.
	// There is no equivalent fallback anchor after the runner is gone.
	if err := anchor.assertRetained(record); err != nil {
		return err
	}
	if err := supervisor.processes.signalGroup(record.PGID, signal); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}

func (supervisor *supervisor) stopGroup(record operationRecord, anchor *runnerChildAnchor) error {
	if err := anchor.assertRetained(record); err != nil {
		return err
	}
	live, err := supervisor.groupLive(record)
	if err != nil || !live {
		return err
	}
	if err := supervisor.signalGroup(record, syscall.SIGTERM, anchor); err != nil {
		return err
	}
	closed, err := supervisor.waitGroupClosed(record, supervisor.config.termGrace, anchor)
	if err != nil || closed {
		return err
	}
	if err := supervisor.signalGroup(record, syscall.SIGKILL, anchor); err != nil {
		return err
	}
	closed, err = supervisor.waitGroupClosed(record, supervisor.config.killGrace, anchor)
	if err != nil {
		return err
	}
	if !closed {
		return fmt.Errorf("process group did not drain after SIGKILL")
	}
	return nil
}
