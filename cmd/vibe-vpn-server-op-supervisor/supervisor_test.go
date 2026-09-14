//go:build linux

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

const (
	testToken      = "op_1234567890123456789012345678901234567890123"
	secondToken    = "op_abcdefghijklmnopqrstuvwxyzABCDEFGH012345678"
	testServerID   = "srv_123456789012345678901234567"
	workerModeEnv  = "VPNKIT_OP_SUPERVISOR_TEST_WORKER"
	workerChildEnv = "VPNKIT_OP_SUPERVISOR_TEST_CHILD"
)

func TestMain(m *testing.M) {
	if os.Getenv(workerModeEnv) != "" {
		fakeWorker()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func fakeWorker() {
	mode := os.Getenv(workerModeEnv)
	if logPath := os.Getenv("VPNKIT_OP_SUPERVISOR_TEST_ARGV"); logPath != "" {
		encoded, _ := json.Marshal(os.Args[1:])
		_ = os.WriteFile(logPath, encoded, 0o600)
	}
	_, _ = io.WriteString(os.Stderr, "private-worker-stderr-marker\n")
	if os.Getenv(workerChildEnv) == "1" {
		signal.Ignore(syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
		if ready := os.Getenv("VPNKIT_OP_SUPERVISOR_TEST_CHILD_READY"); ready != "" {
			_ = os.WriteFile(ready, []byte(strconv.Itoa(os.Getpid())), 0o600)
		}
		time.Sleep(700 * time.Millisecond)
		if survivor := os.Getenv("VPNKIT_OP_SUPERVISOR_TEST_SURVIVOR"); survivor != "" {
			_ = os.WriteFile(survivor, []byte("survived"), 0o600)
		}
		for {
			time.Sleep(time.Second)
		}
	}
	switch mode {
	case "normal":
		_, _ = io.WriteString(os.Stdout, `{"schema":"vibe-vpn.server-browser.v2","status":"ok","generation":1,"servers":[]}`+"\n")
	case "nonzero":
		_, _ = io.WriteString(os.Stdout, `{"schema":"vibe-vpn.server-browser.v2","status":"unavailable","generation":0}`+"\n")
		os.Exit(9)
	case "catalog-sized":
		_, _ = io.WriteString(os.Stdout, strings.Repeat("x", 700000))
	case "oversized":
		_, _ = io.WriteString(os.Stdout, strings.Repeat("x", defaultOutputLimit+4096))
	case "exit-with-inherited-output":
		child := exec.Command(os.Args[0], "fixed-inherited-output-descendant")
		child.Env = append(os.Environ(), workerChildEnv+"=1")
		child.Stdin = nil
		child.Stdout = os.Stdout
		child.Stderr = io.Discard
		if err := child.Start(); err != nil {
			os.Exit(93)
		}
		if ready := os.Getenv("VPNKIT_OP_SUPERVISOR_TEST_CHILD_READY"); ready != "" {
			deadline := time.Now().Add(500 * time.Millisecond)
			for time.Now().Before(deadline) {
				if _, err := os.Stat(ready); err == nil {
					break
				}
				time.Sleep(time.Millisecond)
			}
		}
		_, _ = io.WriteString(os.Stdout, `{"schema":"vibe-vpn.server-browser.v2","status":"ok","generation":1,"servers":[]}`+"\n")
	case "ignore-term-tree":
		signal.Ignore(syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
		child := exec.Command(os.Args[0], "fixed-descendant")
		child.Env = append(os.Environ(), workerChildEnv+"=1")
		child.Stdin = nil
		child.Stdout = io.Discard
		child.Stderr = io.Discard
		if err := child.Start(); err != nil {
			os.Exit(91)
		}
		if ready := os.Getenv("VPNKIT_OP_SUPERVISOR_TEST_PARENT_READY"); ready != "" {
			_ = os.WriteFile(ready, []byte(strconv.Itoa(os.Getpid())), 0o600)
		}
		for {
			time.Sleep(time.Second)
		}
	default:
		os.Exit(92)
	}
}

func newTestSupervisor(t *testing.T, mode string, extraEnv ...string) *supervisor {
	t.Helper()
	root := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	environment := []string{workerModeEnv + "=" + mode}
	environment = append(environment, extraEnv...)
	return newSupervisor(supervisorConfig{
		stateRoot:    root,
		runner:       os.Args[0],
		worker:       os.Args[0],
		workerConfig: "/fixed/vibe-vpn/config.yaml",
		procRoot:     "/proc",
		termGrace:    50 * time.Millisecond,
		killGrace:    3 * time.Second,
		pollInterval: 5 * time.Millisecond,
		outputLimit:  defaultOutputLimit,
		workerEnv:    environment,
	})
}

func requestArgs(token, operation, serverID string) []string {
	args := []string{token, operation}
	if serverID != "" {
		args = append(args, serverID)
	}
	return args
}

func waitForRecord(t *testing.T, supervisor *supervisor, token, status string) operationRecord {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		lock, err := supervisor.lockState()
		if err == nil {
			record, readErr := supervisor.readRecord(token)
			lock.close()
			if readErr == nil && record.Status == status {
				return record
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("operation %s did not reach %s", token, status)
	return operationRecord{}
}

func waitForPath(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", filepath.Base(path))
}

func processCanMutate(pid int) bool {
	stat, err := readProcStat("/proc", pid)
	return err == nil && !processExitedState(stat.state)
}

type fakeProcessSystem struct {
	mutex       sync.Mutex
	stats       map[int]procStat
	cmdlines    map[int][]string
	waitExited  bool
	waitCalls   int
	signals     []syscall.Signal
	afterPIDs   func()
	afterSignal func(syscall.Signal)
}

func newFakeProcessSystem() *fakeProcessSystem {
	return &fakeProcessSystem{
		stats:    make(map[int]procStat),
		cmdlines: make(map[int][]string),
	}
}

func (system *fakeProcessSystem) stat(pid int) (procStat, error) {
	system.mutex.Lock()
	defer system.mutex.Unlock()
	stat, ok := system.stats[pid]
	if !ok {
		return procStat{}, os.ErrNotExist
	}
	return stat, nil
}

func (system *fakeProcessSystem) cmdline(pid int) ([]string, error) {
	system.mutex.Lock()
	defer system.mutex.Unlock()
	if _, ok := system.stats[pid]; !ok {
		return nil, os.ErrNotExist
	}
	return append([]string(nil), system.cmdlines[pid]...), nil
}

func (system *fakeProcessSystem) pids() ([]int, error) {
	system.mutex.Lock()
	pids := make([]int, 0, len(system.stats))
	for pid := range system.stats {
		pids = append(pids, pid)
	}
	sort.Ints(pids)
	after := system.afterPIDs
	system.afterPIDs = nil
	system.mutex.Unlock()
	if after != nil {
		after()
	}
	return pids, nil
}

func (system *fakeProcessSystem) childExitedNoReap(int) (bool, error) {
	system.mutex.Lock()
	defer system.mutex.Unlock()
	system.waitCalls++
	return system.waitExited, nil
}

func (system *fakeProcessSystem) signalGroup(_ int, signal syscall.Signal) error {
	system.mutex.Lock()
	system.signals = append(system.signals, signal)
	after := system.afterSignal
	system.mutex.Unlock()
	if after != nil {
		after(signal)
	}
	return nil
}

func (system *fakeProcessSystem) setProcess(pid int, stat procStat, cmdline []string) {
	system.mutex.Lock()
	defer system.mutex.Unlock()
	system.stats[pid] = stat
	system.cmdlines[pid] = append([]string(nil), cmdline...)
}

func (system *fakeProcessSystem) removeProcess(pid int) {
	system.mutex.Lock()
	defer system.mutex.Unlock()
	delete(system.stats, pid)
	delete(system.cmdlines, pid)
}

func (system *fakeProcessSystem) signalSnapshot() []syscall.Signal {
	system.mutex.Lock()
	defer system.mutex.Unlock()
	return append([]syscall.Signal(nil), system.signals...)
}

func (system *fakeProcessSystem) waitCallCount() int {
	system.mutex.Lock()
	defer system.mutex.Unlock()
	return system.waitCalls
}

const (
	fakeRunnerPID   = 424100
	fakeWorkerPID   = 424200
	fakeChildPID    = 424201
	fakeRunnerStart = 1001
	fakeWorkerStart = 2001
)

func newFakeSupervisor(t *testing.T, processes *fakeProcessSystem) *supervisor {
	t.Helper()
	root := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	return newSupervisor(supervisorConfig{
		stateRoot:    root,
		runner:       "/fixed/supervisor",
		worker:       "/fixed/worker",
		workerConfig: "/fixed/config.yaml",
		termGrace:    2 * time.Millisecond,
		killGrace:    50 * time.Millisecond,
		pollInterval: time.Millisecond,
		processes:    processes,
	})
}

func publishFakeRecord(t *testing.T, supervisor *supervisor, token string) operationRecord {
	t.Helper()
	args := requestArgs(token, "list", "")
	if err := supervisor.prepare(args); err != nil {
		t.Fatal(err)
	}
	lock, err := supervisor.lockState()
	if err != nil {
		t.Fatal(err)
	}
	defer lock.close()
	record, err := supervisor.readRecord(token)
	if err != nil {
		t.Fatal(err)
	}
	record.Status = "running"
	record.RunnerPID = fakeRunnerPID
	record.RunnerStart = fakeRunnerStart
	record.WorkerPID = fakeWorkerPID
	record.WorkerStart = fakeWorkerStart
	record.PGID = fakeWorkerPID
	record.Session = fakeWorkerPID
	if err := supervisor.writeRecord(record); err != nil {
		t.Fatal(err)
	}
	return record
}

func seedExactFakeProcesses(supervisor *supervisor, processes *fakeProcessSystem, record operationRecord) {
	processes.setProcess(record.RunnerPID, procStat{state: 'S', startTime: record.RunnerStart}, []string{supervisor.config.runner, "run"})
	processes.setProcess(
		record.WorkerPID,
		procStat{state: 'S', pgrp: record.PGID, session: record.Session, startTime: record.WorkerStart},
		append([]string{supervisor.config.worker}, workerArgv(supervisor.config, record.request())...),
	)
}

func readFakeRecord(t *testing.T, supervisor *supervisor, token string) operationRecord {
	t.Helper()
	lock, err := supervisor.lockState()
	if err != nil {
		t.Fatal(err)
	}
	defer lock.close()
	record, err := supervisor.readRecord(token)
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func TestFixedArgvPrivateIdentityAndNormalCleanup(t *testing.T) {
	argvLog := filepath.Join(t.TempDir(), "argv.json")
	supervisor := newTestSupervisor(t, "normal", "VPNKIT_OP_SUPERVISOR_TEST_ARGV="+argvLog)
	args := requestArgs(testToken, "ping", testServerID)
	if err := supervisor.prepare(args); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(supervisor.tokenDir(testToken))
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("token state is not private: info=%v err=%v", info, err)
	}
	recordInfo, err := os.Stat(supervisor.recordPath(testToken))
	if err != nil || recordInfo.Mode().Perm() != 0o600 || fileNlink(recordInfo) != 1 {
		t.Fatalf("record is not a private single-link file: info=%v err=%v", recordInfo, err)
	}

	result, err := supervisor.run(args)
	if err != nil {
		t.Fatal(err)
	}
	if result.exitCode != 0 || !bytes.Contains(result.output, []byte(`"schema":"vibe-vpn.server-browser.v2"`)) {
		t.Fatalf("unexpected result: code=%d output=%q", result.exitCode, result.output)
	}
	logged, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatal(err)
	}
	var actual []string
	if err := json.Unmarshal(logged, &actual); err != nil {
		t.Fatal(err)
	}
	expected := []string{"--config", "/fixed/vibe-vpn/config.yaml", "ping", "--server-id", testServerID, "--json"}
	if fmt.Sprint(actual) != fmt.Sprint(expected) {
		t.Fatalf("argv=%v want=%v", actual, expected)
	}
	if _, err := os.Stat(supervisor.tokenDir(testToken)); !os.IsNotExist(err) {
		t.Fatalf("normal operation state was not cleaned: %v", err)
	}

	for _, hostile := range [][]string{
		{testToken, "exec", "docker"},
		{testToken, "list", "extra"},
		{testToken, "ping", "../../private"},
		{testToken + ";", "list"},
	} {
		if _, err := parseOperationRequest(hostile); err == nil {
			t.Fatalf("accepted hostile request: %v", hostile)
		}
	}
}

func TestPrepareCancellationClosesPrepublicationRaceWithoutSpawn(t *testing.T) {
	argvLog := filepath.Join(t.TempDir(), "argv.json")
	supervisor := newTestSupervisor(t, "normal", "VPNKIT_OP_SUPERVISOR_TEST_ARGV="+argvLog)
	args := requestArgs(testToken, "list", "")
	if err := supervisor.prepare(args); err != nil {
		t.Fatal(err)
	}
	if err := supervisor.cancel(testToken); err != nil {
		t.Fatal(err)
	}
	if _, err := supervisor.run(args); err == nil {
		t.Fatal("canceled prepared operation spawned")
	}
	if _, err := os.Stat(argvLog); !os.IsNotExist(err) {
		t.Fatalf("worker unexpectedly ran: %v", err)
	}
	if err := supervisor.verify(testToken); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(supervisor.tokenDir(testToken)); !os.IsNotExist(err) {
		t.Fatalf("verified canceled state was not cleaned: %v", err)
	}
}

func TestWaitidNowaitObservesExitWithoutReapingChild(t *testing.T) {
	command := exec.Command(os.Args[0], "waitid-nowait-worker")
	command.Env = append(os.Environ(), workerModeEnv+"=normal")
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	reaped := false
	defer func() {
		if !reaped {
			_, _ = command.Process.Wait()
		}
	}()

	processes := &linuxProcessSystem{procRoot: "/proc"}
	deadline := time.Now().Add(3 * time.Second)
	exited := false
	for time.Now().Before(deadline) {
		var err error
		exited, err = processes.childExitedNoReap(command.Process.Pid)
		if err != nil {
			t.Fatal(err)
		}
		if exited {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !exited {
		t.Fatal("waitid(WNOWAIT) did not observe child exit")
	}
	state, err := command.Process.Wait()
	reaped = true
	if err != nil {
		t.Fatalf("WNOWAIT unexpectedly reaped child: %v", err)
	}
	if !state.Success() {
		t.Fatalf("unexpected child status after final reap: %v", state)
	}
}

func TestExternalCancellationOnlyMarksAndRunnerPollObservesRequest(t *testing.T) {
	processes := newFakeProcessSystem()
	supervisor := newFakeSupervisor(t, processes)
	record := publishFakeRecord(t, supervisor, testToken)
	seedExactFakeProcesses(supervisor, processes, record)

	awaited := make(chan struct {
		cancelled bool
		err       error
	}, 1)
	go func() {
		cancelled, _, err := supervisor.awaitWorker(record, make(chan os.Signal))
		awaited <- struct {
			cancelled bool
			err       error
		}{cancelled: cancelled, err: err}
	}()
	deadline := time.Now().Add(time.Second)
	for processes.waitCallCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if processes.waitCallCount() == 0 {
		t.Fatal("runner did not begin waitid polling")
	}

	if err := supervisor.cancel(testToken); err != nil {
		t.Fatal(err)
	}
	if signals := processes.signalSnapshot(); len(signals) != 0 {
		t.Fatalf("external cancellation signaled instead of only marking: %v", signals)
	}
	if status := readFakeRecord(t, supervisor, testToken).Status; status != "cancelling" {
		t.Fatalf("status=%q want cancelling", status)
	}
	select {
	case result := <-awaited:
		if result.err != nil || !result.cancelled {
			t.Fatalf("runner poll result: cancelled=%v err=%v", result.cancelled, result.err)
		}
	case <-time.After(time.Second):
		t.Fatal("runner did not observe cancellation record")
	}
}

func TestRunnerGoneFallbackNeverSignalsReplacementAfterProof(t *testing.T) {
	processes := newFakeProcessSystem()
	supervisor := newFakeSupervisor(t, processes)
	record := publishFakeRecord(t, supervisor, testToken)
	seedExactFakeProcesses(supervisor, processes, record)
	processes.removeProcess(record.RunnerPID)

	// The fallback has no ownership anchor after the exact identity proof. A
	// replacement is installed at the recorded PID immediately after that
	// proof returns, exactly where an unsafe implementation would next issue
	// its numeric group signal.
	replacementInstalled := false
	supervisor.config.afterFallbackProof = func(operationRecord) {
		replacementInstalled = true
		processes.setProcess(
			record.WorkerPID,
			procStat{state: 'S', pgrp: record.PGID, session: record.Session, startTime: record.WorkerStart + 1},
			[]string{"/unrelated/replacement"},
		)
	}

	// The worker proof returned exact-live, but fallback must remain open and
	// must never signal the replacement.
	err := supervisor.cancel(testToken)
	if !replacementInstalled {
		t.Fatal("replacement was not installed after the exact worker proof")
	}
	if !errors.Is(err, errOpen) {
		t.Fatalf("replacement race did not fail closed: %v", err)
	}
	if signals := processes.signalSnapshot(); len(signals) != 0 {
		t.Fatalf("replacement group received signals: %v", signals)
	}
	if status := readFakeRecord(t, supervisor, testToken).Status; status != "cancelling" {
		t.Fatalf("status=%q want cancelling", status)
	}
}

func TestRunnerGoneFallbackRejectsReplacementAfterEmptyGroupProof(t *testing.T) {
	processes := newFakeProcessSystem()
	supervisor := newFakeSupervisor(t, processes)
	record := publishFakeRecord(t, supervisor, testToken)
	processes.removeProcess(record.RunnerPID)
	// The exact worker is already gone and the initial process list is empty.
	// Replace its PID after that list is captured, before fallback could close
	// the record or (in the old design) issue a numeric group signal.
	replacementInstalled := false
	processes.afterPIDs = func() {
		replacementInstalled = true
		processes.setProcess(
			record.WorkerPID,
			procStat{state: 'S', pgrp: record.PGID, session: record.Session, startTime: record.WorkerStart + 1},
			[]string{"/unrelated/replacement"},
		)
	}

	if err := supervisor.cancel(testToken); !errors.Is(err, errOpen) {
		t.Fatalf("replacement after empty-group proof was not kept open: %v", err)
	}
	if !replacementInstalled {
		t.Fatal("replacement was not installed after the empty-group proof")
	}
	if signals := processes.signalSnapshot(); len(signals) != 0 {
		t.Fatalf("replacement group received signals: %v", signals)
	}
	if status := readFakeRecord(t, supervisor, testToken).Status; status != "cancelling" {
		t.Fatalf("status=%q want cancelling", status)
	}
}

func TestRunnerZombieAnchorSurvivesTermKillAndDrain(t *testing.T) {
	processes := newFakeProcessSystem()
	supervisor := newFakeSupervisor(t, processes)
	record := publishFakeRecord(t, supervisor, testToken)
	processes.setProcess(
		record.WorkerPID,
		procStat{state: 'Z', pgrp: record.PGID, session: record.Session, startTime: record.WorkerStart},
		nil,
	)
	processes.setProcess(
		fakeChildPID,
		procStat{state: 'S', pgrp: record.PGID, session: record.Session, startTime: 3001},
		[]string{"/fixed/daemonized-descendant"},
	)
	processes.afterSignal = func(signal syscall.Signal) {
		if signal == syscall.SIGKILL {
			processes.setProcess(
				fakeChildPID,
				procStat{state: 'Z', pgrp: record.PGID, session: record.Session, startTime: 3001},
				nil,
			)
		}
	}

	if err := supervisor.stopGroup(record, newRunnerChildAnchor(record)); err != nil {
		t.Fatal(err)
	}
	if got := processes.signalSnapshot(); fmt.Sprint(got) != fmt.Sprint([]syscall.Signal{syscall.SIGTERM, syscall.SIGKILL}) {
		t.Fatalf("signals=%v want TERM,KILL", got)
	}
	state, err := supervisor.workerIdentity(record)
	if err != nil || state != identityZombie {
		t.Fatalf("leader anchor was not retained as zombie: state=%v err=%v", state, err)
	}
}

func TestRunnerGoneFallbackNeverSignalsAndClosesOnlyAfterDrain(t *testing.T) {
	processes := newFakeProcessSystem()
	supervisor := newFakeSupervisor(t, processes)
	record := publishFakeRecord(t, supervisor, testToken)
	seedExactFakeProcesses(supervisor, processes, record)
	processes.removeProcess(record.RunnerPID)
	processes.setProcess(
		fakeChildPID,
		procStat{state: 'S', pgrp: record.PGID, session: record.Session, startTime: 3001},
		[]string{"/fixed/term-resistant-descendant"},
	)

	if err := supervisor.cancel(testToken); !errors.Is(err, errOpen) {
		t.Fatalf("runner-gone fallback must fail closed while worker tree lives: %v", err)
	}
	if signals := processes.signalSnapshot(); len(signals) != 0 {
		t.Fatalf("runner-gone fallback signaled without an ownership anchor: %v", signals)
	}
	if status := readFakeRecord(t, supervisor, testToken).Status; status != "cancelling" {
		t.Fatalf("status=%q want cancelling", status)
	}
	if err := supervisor.verify(testToken); !errors.Is(err, errOpen) {
		t.Fatalf("verify falsely claimed runner-gone tree drained: %v", err)
	}

	processes.removeProcess(record.WorkerPID)
	processes.removeProcess(fakeChildPID)
	if err := supervisor.cancel(testToken); err != nil {
		t.Fatalf("runner-gone fallback did not close an exact-gone empty group: %v", err)
	}
	if status := readFakeRecord(t, supervisor, testToken).Status; status != "closed" {
		t.Fatalf("status=%q want closed after exact worker/group drain", status)
	}
	if err := supervisor.verify(testToken); err != nil {
		t.Fatalf("verify did not remove drained runner-crash fallback: %v", err)
	}
}

func TestRunnerGoneLeaderGoneDescendantsRemainFailsClosed(t *testing.T) {
	processes := newFakeProcessSystem()
	supervisor := newFakeSupervisor(t, processes)
	record := publishFakeRecord(t, supervisor, testToken)
	processes.setProcess(
		fakeChildPID,
		procStat{state: 'S', pgrp: record.PGID, session: record.Session, startTime: 3001},
		[]string{"/possibly-unrelated-group"},
	)

	if err := supervisor.cancel(testToken); !errors.Is(err, errOpen) {
		t.Fatalf("unanchored descendants did not fail closed: %v", err)
	}
	if signals := processes.signalSnapshot(); len(signals) != 0 {
		t.Fatalf("unanchored numeric group received signals: %v", signals)
	}
	if status := readFakeRecord(t, supervisor, testToken).Status; status != "cancelling" {
		t.Fatalf("status=%q want cancelling", status)
	}
}

func TestCancellationRequestWakesRunnerWhichEscalatesAndDrains(t *testing.T) {
	fixture := t.TempDir()
	parentReady := filepath.Join(fixture, "parent-ready")
	childReady := filepath.Join(fixture, "child-ready")
	survivor := filepath.Join(fixture, "survivor")
	supervisor := newTestSupervisor(
		t,
		"ignore-term-tree",
		"VPNKIT_OP_SUPERVISOR_TEST_PARENT_READY="+parentReady,
		"VPNKIT_OP_SUPERVISOR_TEST_CHILD_READY="+childReady,
		"VPNKIT_OP_SUPERVISOR_TEST_SURVIVOR="+survivor,
	)
	args := requestArgs(testToken, "select", testServerID)
	if err := supervisor.prepare(args); err != nil {
		t.Fatal(err)
	}
	runDone := make(chan error, 1)
	go func() {
		_, err := supervisor.run(args)
		runDone <- err
	}()
	record := waitForRecord(t, supervisor, testToken, "running")
	waitForPath(t, parentReady)
	waitForPath(t, childReady)
	childPIDBytes, err := os.ReadFile(childReady)
	if err != nil {
		t.Fatal(err)
	}
	childPID, err := strconv.Atoi(string(childPIDBytes))
	if err != nil {
		t.Fatal(err)
	}

	started := time.Now()
	if err := supervisor.cancel(testToken); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("runner did not poll, escalate, and close after cancellation request")
	}
	if time.Since(started) < supervisor.config.termGrace {
		t.Fatal("runner closed before TERM grace/escalation")
	}
	if processCanMutate(record.WorkerPID) || processCanMutate(childPID) {
		t.Fatalf("worker tree still mutates after runner drain: parent=%v child=%v", processCanMutate(record.WorkerPID), processCanMutate(childPID))
	}
	time.Sleep(750 * time.Millisecond)
	if _, err := os.Stat(survivor); !os.IsNotExist(err) {
		t.Fatal("TERM-resistant descendant survived and mutated")
	}
	if err := supervisor.verify(testToken); err != nil {
		t.Fatal(err)
	}
}

func TestStaleTokenNeverTargetsActiveWorker(t *testing.T) {
	fixture := t.TempDir()
	parentReady := filepath.Join(fixture, "parent-ready")
	childReady := filepath.Join(fixture, "child-ready")
	supervisor := newTestSupervisor(
		t,
		"ignore-term-tree",
		"VPNKIT_OP_SUPERVISOR_TEST_PARENT_READY="+parentReady,
		"VPNKIT_OP_SUPERVISOR_TEST_CHILD_READY="+childReady,
	)
	args := requestArgs(secondToken, "current", "")
	if err := supervisor.prepare(args); err != nil {
		t.Fatal(err)
	}
	runDone := make(chan error, 1)
	go func() {
		_, err := supervisor.run(args)
		runDone <- err
	}()
	record := waitForRecord(t, supervisor, secondToken, "running")
	waitForPath(t, childReady)

	if err := supervisor.cancel(testToken); err != nil {
		t.Fatalf("absent stale token should be isolated: %v", err)
	}
	if !processCanMutate(record.WorkerPID) {
		t.Fatal("stale token affected newer worker")
	}

	if err := supervisor.cancel(secondToken); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("restored exact worker did not drain")
	}
}

func TestNormalExitDrainsInheritedOutputDescendantBeforeReap(t *testing.T) {
	fixture := t.TempDir()
	childReady := filepath.Join(fixture, "child-ready")
	supervisor := newTestSupervisor(
		t,
		"exit-with-inherited-output",
		"VPNKIT_OP_SUPERVISOR_TEST_CHILD_READY="+childReady,
	)
	args := requestArgs(testToken, "list", "")
	if err := supervisor.prepare(args); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct {
		result runResult
		err    error
	}, 1)
	go func() {
		result, err := supervisor.run(args)
		done <- struct {
			result runResult
			err    error
		}{result: result, err: err}
	}()
	waitForPath(t, childReady)
	childPIDBytes, err := os.ReadFile(childReady)
	if err != nil {
		t.Fatal(err)
	}
	childPID, err := strconv.Atoi(string(childPIDBytes))
	if err != nil {
		t.Fatal(err)
	}

	select {
	case outcome := <-done:
		if outcome.err != nil {
			t.Fatal(outcome.err)
		}
		if outcome.result.exitCode != 0 || !bytes.Contains(outcome.result.output, []byte(`"status":"ok"`)) {
			t.Fatalf("unexpected result: code=%d output=%q", outcome.result.exitCode, outcome.result.output)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("run deadlocked on stdout inherited by daemonized descendant")
	}
	if processCanMutate(childPID) {
		t.Fatal("normal completion reaped leader before descendant closure")
	}
}

func TestDispatchBoundsOutputDiscardsStderrAndUsesFixedExitCodes(t *testing.T) {
	normal := newTestSupervisor(t, "normal")
	args := requestArgs(testToken, "list", "")
	if code := dispatch(append([]string{"prepare"}, args...), normal, io.Discard); code != 0 {
		t.Fatalf("prepare exit=%d", code)
	}
	var output bytes.Buffer
	if code := dispatch(append([]string{"run"}, args...), normal, &output); code != 0 {
		t.Fatalf("run exit=%d", code)
	}
	if strings.Contains(output.String(), "private-worker-stderr-marker") {
		t.Fatal("private stderr crossed supervisor output")
	}
	if !strings.Contains(output.String(), "server-browser.v2") {
		t.Fatalf("safe stdout missing: %q", output.String())
	}

	nonzero := newTestSupervisor(t, "nonzero")
	if err := nonzero.prepare(args); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	if code := dispatch(append([]string{"run"}, args...), nonzero, &output); code != 9 {
		t.Fatalf("worker exit was not preserved: %d", code)
	}
	if !strings.Contains(output.String(), `"status":"unavailable"`) {
		t.Fatalf("safe nonzero response missing: %q", output.String())
	}

	large := newTestSupervisor(t, "catalog-sized")
	if err := large.prepare(args); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	if code := dispatch(append([]string{"run"}, args...), large, &output); code != 0 || output.Len() != 700000 {
		t.Fatalf("supported catalog output rejected: exit=%d bytes=%d", code, output.Len())
	}

	oversized := newTestSupervisor(t, "oversized")
	if err := oversized.prepare(args); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	if code := dispatch(append([]string{"run"}, args...), oversized, &output); code != exitFailure {
		t.Fatalf("oversized output exit=%d", code)
	}
	if output.Len() != 0 {
		t.Fatal("partial oversized output escaped")
	}
	if code := dispatch([]string{"run", testToken, "list", "extra", "passthrough"}, normal, &output); code != exitUsage {
		t.Fatalf("raw passthrough was not rejected: %d", code)
	}
}

func TestVerifyReportsOpenWithoutSignaling(t *testing.T) {
	fixture := t.TempDir()
	supervisor := newTestSupervisor(
		t,
		"ignore-term-tree",
		"VPNKIT_OP_SUPERVISOR_TEST_PARENT_READY="+filepath.Join(fixture, "parent"),
		"VPNKIT_OP_SUPERVISOR_TEST_CHILD_READY="+filepath.Join(fixture, "child"),
	)
	args := requestArgs(testToken, "test-all", "")
	if err := supervisor.prepare(args); err != nil {
		t.Fatal(err)
	}
	runDone := make(chan error, 1)
	go func() {
		_, err := supervisor.run(args)
		runDone <- err
	}()
	record := waitForRecord(t, supervisor, testToken, "running")
	if err := supervisor.verify(testToken); !errors.Is(err, errOpen) {
		t.Fatalf("verify did not report open: %v", err)
	}
	if !processCanMutate(record.WorkerPID) {
		t.Fatal("verify signaled worker")
	}
	if err := supervisor.cancel(testToken); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("worker did not close")
	}
	if err := supervisor.verify(testToken); err != nil {
		t.Fatal(err)
	}
}

func TestAvailabilityTargetIsBoundToOperationIdentity(t *testing.T) {
	target := "https://www.youtube.com/"
	args := []string{testToken, "availability", testServerID, target}
	request, err := parseOperationRequest(args)
	if err != nil {
		t.Fatal(err)
	}
	supervisor := newTestSupervisor(t, "normal")
	if err := supervisor.prepare(args); err != nil {
		t.Fatal(err)
	}
	record, err := supervisor.readRecord(testToken)
	if err != nil {
		t.Fatal(err)
	}
	if record.TargetURL != target || record.request() != request {
		t.Fatal("target not retained in operation identity")
	}
	changed := record
	changed.TargetURL = "https://example.com/"
	if record.sameProcess(changed) {
		t.Fatal("different targets compare equal")
	}
	for _, bad := range []string{"http://example.com/", "https://user:password@example.com/", "--raw", "https://"} {
		if _, err := parseOperationRequest([]string{testToken, "availability", testServerID, bad}); err == nil {
			t.Fatal("unsafe target accepted")
		}
	}
	if got := strings.Join(workerArgv(supervisor.config, request), " "); !strings.Contains(got, "--url "+target) {
		t.Fatal("target missing from worker argv")
	}
}

func TestCheckBatchRequestBoundsAndIdentity(t *testing.T) {
	ids := make([]string, 6)
	for i := range ids {
		ids[i] = fmt.Sprintf("srv_%027d", i)
	}
	good := []string{testToken, "check-batch", strings.Join(ids[:5], ","), "https://example.com/"}
	if _, err := parseOperationRequest(good); err != nil {
		t.Fatal(err)
	}
	for _, csv := range []string{strings.Join(ids, ","), ids[0] + "," + ids[0], ids[0] + ",", "$(id)"} {
		if _, err := parseOperationRequest([]string{testToken, "check-batch", csv, good[3]}); err == nil {
			t.Fatal("invalid batch accepted")
		}
	}
	if _, err := parseOperationRequest([]string{testToken, "check-batch", ids[0], "http://example.com/"}); err == nil {
		t.Fatal("insecure target accepted")
	}
}
