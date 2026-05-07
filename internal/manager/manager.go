package manager

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

type ServiceStatus string

const (
	StatusStopped  ServiceStatus = "stopped"
	StatusRunning  ServiceStatus = "running"
	StatusStarting ServiceStatus = "starting"
	StatusStopping ServiceStatus = "stopping"
	StatusFailed   ServiceStatus = "failed"
)

type ServiceConfig struct {
	Name         string            `json:"name"`
	Description  string            `json:"description"`
	Command      string            `json:"command"`
	Args         []string          `json:"args"`
	Env          map[string]string `json:"env"`
	WorkDir      string            `json:"workdir"`
	AutoRestart  bool              `json:"auto_restart"`
	AutoStart    bool              `json:"auto_start"`
	RestartDelay int               `json:"restart_delay_seconds"`
	MaxRestarts  int               `json:"max_restarts"`
}

type ServiceState struct {
	Config    ServiceConfig `json:"config"`
	Status    ServiceStatus `json:"status"`
	PID       int           `json:"pid"`
	StartedAt *time.Time    `json:"started_at"`
	StoppedAt *time.Time    `json:"stopped_at"`
	Restarts  int           `json:"restarts"`
	ExitCode  int           `json:"exit_code"`
	Logs      []LogEntry    `json:"logs"`
}

type LogEntry struct {
	Time    time.Time `json:"time"`
	Line    string    `json:"line"`
	IsError bool      `json:"is_error"`
}

type Manager struct {
	mu       sync.RWMutex
	services map[string]*serviceInstance
	dataDir  string
	events   chan Event
}

type Event struct {
	Type    string      `json:"type"`
	Service string      `json:"service"`
	Data    interface{} `json:"data"`
}

type serviceInstance struct {
	mu      sync.Mutex
	state   ServiceState
	cmd     *exec.Cmd
	stopCh  chan struct{}
	logBuf  []LogEntry
	maxLogs int
}

func New(dataDir string) (*Manager, error) {
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return nil, err
	}
	m := &Manager{
		services: make(map[string]*serviceInstance),
		dataDir:  dataDir,
		events:   make(chan Event, 256),
	}
	if err := m.loadAll(); err != nil {
		fmt.Fprintf(os.Stderr, "warn: load services: %v\n", err)
	}
	go m.startAutoStartServices()
	return m, nil
}

func (m *Manager) Events() <-chan Event {
	return m.events
}

func (m *Manager) emit(ev Event) {
	select {
	case m.events <- ev:
	default:
	}
}

// ── persistence ────────────────────────────────────────────────────────────

func (m *Manager) configPath(name string) string {
	return filepath.Join(m.dataDir, name+".json")
}

func (m *Manager) saveConfig(cfg ServiceConfig) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(m.configPath(cfg.Name), data, 0644)
}

func (m *Manager) deleteConfig(name string) error {
	return os.Remove(m.configPath(name))
}

func (m *Manager) loadAll() error {
	entries, err := os.ReadDir(m.dataDir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(m.dataDir, e.Name()))
		if err != nil {
			continue
		}
		var cfg ServiceConfig
		if err := json.Unmarshal(data, &cfg); err != nil {
			continue
		}
		inst := newInstance(cfg)
		m.services[cfg.Name] = inst
	}
	return nil
}

func (m *Manager) startAutoStartServices() {
	m.mu.RLock()
	names := make([]string, 0, len(m.services))
	for name, inst := range m.services {
		if inst.state.Config.AutoStart {
			names = append(names, name)
		}
	}
	m.mu.RUnlock()

	for _, name := range names {
		if err := m.Start(name); err != nil {
			fmt.Fprintf(os.Stderr, "warn: auto start %s: %v\n", name, err)
		}
	}
}

// ── CRUD ───────────────────────────────────────────────────────────────────

func (m *Manager) AddService(cfg ServiceConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.services[cfg.Name]; ok {
		return fmt.Errorf("service %q already exists", cfg.Name)
	}
	if err := m.saveConfig(cfg); err != nil {
		return err
	}
	m.services[cfg.Name] = newInstance(cfg)
	m.emit(Event{Type: "added", Service: cfg.Name})
	return nil
}

func (m *Manager) UpdateService(cfg ServiceConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	inst, ok := m.services[cfg.Name]
	if !ok {
		return fmt.Errorf("service %q not found", cfg.Name)
	}
	inst.mu.Lock()
	inst.state.Config = cfg
	inst.mu.Unlock()
	if err := m.saveConfig(cfg); err != nil {
		return err
	}
	m.emit(Event{Type: "updated", Service: cfg.Name})
	return nil
}

func (m *Manager) RemoveService(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	inst, ok := m.services[name]
	if !ok {
		return fmt.Errorf("service %q not found", name)
	}
	inst.mu.Lock()
	if inst.state.Status == StatusRunning || inst.state.Status == StatusStarting {
		inst.mu.Unlock()
		return fmt.Errorf("stop service %q before removing", name)
	}
	inst.mu.Unlock()
	delete(m.services, name)
	_ = m.deleteConfig(name)
	m.emit(Event{Type: "removed", Service: name})
	return nil
}

// ── lifecycle ──────────────────────────────────────────────────────────────

func (m *Manager) Start(name string) error {
	m.mu.RLock()
	inst, ok := m.services[name]
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("service %q not found", name)
	}
	return m.startInstance(inst)
}

func (m *Manager) Stop(name string) error {
	m.mu.RLock()
	inst, ok := m.services[name]
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("service %q not found", name)
	}
	return m.stopInstance(inst)
}

func (m *Manager) Restart(name string) error {
	_ = m.Stop(name)
	time.Sleep(300 * time.Millisecond)
	return m.Start(name)
}

func (m *Manager) startInstance(inst *serviceInstance) error {
	inst.mu.Lock()
	defer inst.mu.Unlock()

	if inst.state.Status == StatusRunning || inst.state.Status == StatusStarting {
		return fmt.Errorf("already running")
	}

	cfg := inst.state.Config
	inst.state.Status = StatusStarting
	inst.stopCh = make(chan struct{})
	inst.state.Restarts = 0

	m.emit(Event{Type: "status", Service: cfg.Name, Data: StatusStarting})

	go func() {
		restarts := 0
		for {
			exitCode, err := runProcess(inst, m)
			if err != nil {
				inst.mu.Lock()
				inst.appendLog(fmt.Sprintf("[procman] error: %v", err), true)
				inst.mu.Unlock()
			}

			inst.mu.Lock()
			// check stop signal
			select {
			case <-inst.stopCh:
				inst.state.Status = StatusStopped
				now := time.Now()
				inst.state.StoppedAt = &now
				inst.state.ExitCode = exitCode
				inst.mu.Unlock()
				m.emit(Event{Type: "status", Service: cfg.Name, Data: StatusStopped})
				return
			default:
			}

			maxR := cfg.MaxRestarts
			if maxR == 0 {
				maxR = 10
			}
			if !cfg.AutoRestart || restarts >= maxR {
				inst.state.Status = StatusFailed
				now := time.Now()
				inst.state.StoppedAt = &now
				inst.state.ExitCode = exitCode
				inst.mu.Unlock()
				m.emit(Event{Type: "status", Service: cfg.Name, Data: StatusFailed})
				return
			}
			restarts++
			inst.state.Restarts = restarts
			delay := cfg.RestartDelay
			if delay == 0 {
				delay = 3
			}
			inst.appendLog(fmt.Sprintf("[procman] restarting in %ds (attempt %d/%d)", delay, restarts, maxR), false)
			inst.mu.Unlock()
			m.emit(Event{Type: "status", Service: cfg.Name, Data: StatusStarting})

			select {
			case <-inst.stopCh:
				inst.mu.Lock()
				inst.state.Status = StatusStopped
				inst.mu.Unlock()
				m.emit(Event{Type: "status", Service: cfg.Name, Data: StatusStopped})
				return
			case <-time.After(time.Duration(delay) * time.Second):
			}
		}
	}()
	return nil
}

func runProcess(inst *serviceInstance, m *Manager) (int, error) {
	inst.mu.Lock()
	cfg := inst.state.Config
	stopCh := inst.stopCh
	inst.mu.Unlock()

	args := cfg.Args
	cmd := exec.Command(cfg.Command, args...)

	if cfg.WorkDir != "" {
		cmd.Dir = cfg.WorkDir
	}
	env := os.Environ()
	for k, v := range cfg.Env {
		env = append(env, k+"="+v)
	}
	cmd.Env = env

	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()

	if err := cmd.Start(); err != nil {
		return -1, err
	}

	inst.mu.Lock()
	inst.cmd = cmd
	now := time.Now()
	inst.state.StartedAt = &now
	inst.state.Status = StatusRunning
	inst.state.PID = cmd.Process.Pid
	inst.mu.Unlock()
	m.emit(Event{Type: "status", Service: cfg.Name, Data: StatusRunning})

	// stream logs
	go streamLog(stdout, inst, m, cfg.Name, false)
	go streamLog(stderr, inst, m, cfg.Name, true)

	// wait for stop signal
	done := make(chan int, 1)
	go func() {
		err := cmd.Wait()
		code := 0
		if err != nil {
			if ee, ok := err.(*exec.ExitError); ok {
				code = ee.ExitCode()
			}
		}
		done <- code
	}()

	select {
	case code := <-done:
		return code, nil
	case <-stopCh:
		_ = cmd.Process.Signal(os.Interrupt)
		select {
		case code := <-done:
			return code, nil
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			return -1, nil
		}
	}
}

func streamLog(r io.Reader, inst *serviceInstance, m *Manager, name string, isErr bool) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := scanner.Text()
		inst.mu.Lock()
		inst.appendLog(line, isErr)
		inst.mu.Unlock()
		m.emit(Event{Type: "log", Service: name, Data: LogEntry{Time: time.Now(), Line: line, IsError: isErr}})
	}
}

func (m *Manager) stopInstance(inst *serviceInstance) error {
	inst.mu.Lock()
	defer inst.mu.Unlock()
	if inst.state.Status != StatusRunning && inst.state.Status != StatusStarting {
		return fmt.Errorf("not running")
	}
	inst.state.Status = StatusStopping
	close(inst.stopCh)
	m.emit(Event{Type: "status", Service: inst.state.Config.Name, Data: StatusStopping})
	return nil
}

// ── query ──────────────────────────────────────────────────────────────────

func (m *Manager) List() []ServiceState {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]ServiceState, 0, len(m.services))
	for _, inst := range m.services {
		inst.mu.Lock()
		out = append(out, inst.state)
		inst.mu.Unlock()
	}
	return out
}

func (m *Manager) Get(name string) (ServiceState, error) {
	m.mu.RLock()
	inst, ok := m.services[name]
	m.mu.RUnlock()
	if !ok {
		return ServiceState{}, fmt.Errorf("service %q not found", name)
	}
	inst.mu.Lock()
	defer inst.mu.Unlock()
	return inst.state, nil
}

func (m *Manager) GetLogs(name string) ([]LogEntry, error) {
	m.mu.RLock()
	inst, ok := m.services[name]
	m.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("service %q not found", name)
	}
	inst.mu.Lock()
	defer inst.mu.Unlock()
	cp := make([]LogEntry, len(inst.logBuf))
	copy(cp, inst.logBuf)
	return cp, nil
}

// ── helpers ────────────────────────────────────────────────────────────────

func newInstance(cfg ServiceConfig) *serviceInstance {
	return &serviceInstance{
		state:   ServiceState{Config: cfg, Status: StatusStopped},
		maxLogs: 500,
	}
}

func (inst *serviceInstance) appendLog(line string, isErr bool) {
	entry := LogEntry{Time: time.Now(), Line: line, IsError: isErr}
	inst.logBuf = append(inst.logBuf, entry)
	if len(inst.logBuf) > inst.maxLogs {
		inst.logBuf = inst.logBuf[len(inst.logBuf)-inst.maxLogs:]
	}
}
