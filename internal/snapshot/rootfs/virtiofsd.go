package rootfs

import (
	"github.com/openeuler/Conch/internal/snapshot/common"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

type State struct {
	Key       string    // key
	Rootfs    string    // which is share dir
	Pid       int       // save current pid
	StartedAt time.Time // save start time
}

// manager process struce
type Instance struct {
	State

	conf  *Config
	cmd   *exec.Cmd
	mutex sync.RWMutex
}

type Config struct {
	Key       string
	Rootfs    string // which is share dir
	ExtraArgs []string
}

type Manager struct {
	instances map[string]*Instance
	mutex     sync.RWMutex
}

var gWorkDir = "/var/run/conch/virtiofsd"

const (
	SocketName = "virtiofsd.sock"
	StateFile  = "state.json"
)

func NewManager() *Manager {
	return &Manager{
		instances: make(map[string]*Instance),
	}
}

func (m *Manager) NewInstance(config *Config) (*Instance, error) {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	if _, exists := m.instances[config.Key]; exists {
		return nil, fmt.Errorf("instance %s has exist", config.Key)
	}
	if err := os.MkdirAll(filepath.Join(gWorkDir, config.Key), common.DirMode); err != nil {
		return nil, err
	}

	instance := &Instance{
		conf: config,
	}

	m.instances[config.Key] = instance
	return instance, nil
}

func (m *Manager) GetInstance(id string) (*Instance, bool) {
	m.mutex.RLock()
	defer m.mutex.RUnlock()

	instance, exists := m.instances[id]
	return instance, exists
}

func (m *Manager) LoadInstances() error {
	return filepath.WalkDir(gWorkDir, func(path string, d fs.DirEntry, err error) error {
		if path == gWorkDir {
			return nil
		}
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		instance := &Instance{}
		if terr := instance.Load(d.Name()); terr != nil {
			return terr
		}
		// TODO: check instance pid is running, if not, just ignore it
		m.instances[d.Name()] = instance
		fmt.Printf("get instance: %v\n", instance.State)
		return nil
	})
}

func (m *Manager) RemoveInstance(id string) error {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	instance, exists := m.instances[id]
	if !exists {
		return fmt.Errorf("no found instance: %s", id)
	}

	if instance.IsRunning() {
		return errors.New("instance is running, please stop first")
	}

	delete(m.instances, id)
	return os.RemoveAll(filepath.Join(gWorkDir, instance.Key))
}

func (i *Instance) Start() error {
	i.mutex.Lock()
	defer i.mutex.Unlock()

	if i.Pid != 0 {
		return errors.New("instance is running")
	}

	args := buildArgs(i.conf)
	cmd := exec.Command("virtiofsd", args...)

	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true, // create new process group
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("process startup failed: %w", err)
	}
	go cmd.Wait()

	i.Key = i.conf.Key
	i.Rootfs = i.conf.Rootfs
	i.Pid = cmd.Process.Pid
	i.StartedAt = time.Now()

	return i.Save()
}

func (i *Instance) GetRootfsSock() string {
	i.mutex.RLock()
	defer i.mutex.RUnlock()

	if i.Pid == 0 {
		return ""
	}
	return filepath.Join(gWorkDir, i.Key, SocketName)
}

func (i *Instance) Stop() error {
	i.mutex.Lock()
	defer i.mutex.Unlock()

	if i.Pid == 0 {
		return errors.New("进程未在运行")
	}

	// send signal to process group
	if err := syscall.Kill(-i.Pid, syscall.SIGTERM); err != nil {
		_ = syscall.Kill(-i.Pid, syscall.SIGKILL)
		if !errors.Is(err, syscall.ESRCH) {
			return fmt.Errorf("send signal failed: %v", err)
		}
	}

	i.Pid = 0

	os.Remove(filepath.Join(gWorkDir, i.Key, SocketName))
	return i.Save()
}

func (i *Instance) Load(key string) error {
	fname := filepath.Join(gWorkDir, key, StateFile)
	file, err := os.OpenFile(fname, os.O_RDONLY, common.FileMode)
	if err != nil {
		return err
	}
	defer file.Close()
	de := json.NewDecoder(file)
	return de.Decode(&i.State)
}

func (i *Instance) Save() error {
	fname := filepath.Join(gWorkDir, i.Key, StateFile)

	file, err := os.OpenFile(fname, os.O_CREATE|os.O_RDWR, common.FileMode)
	if err != nil {
		return err
	}
	defer file.Close()
	en := json.NewEncoder(file)
	return en.Encode(i.State)
}

func (i *Instance) IsRunning() bool {
	i.mutex.Lock()
	defer i.mutex.Unlock()

	if i.Pid == 0 {
		return false
	}

	// send signal to process group
	// to check exist or not
	err := syscall.Kill(-i.Pid, 0)
	return err == nil
}

func buildArgs(config *Config) []string {
	var args []string

	args = append(args, fmt.Sprintf("--socket-path=%s", filepath.Join(gWorkDir, config.Key, SocketName)))
	args = append(args, fmt.Sprintf("--shared-dir=%s", config.Rootfs))
	args = append(args, config.ExtraArgs...)
	return args
}
