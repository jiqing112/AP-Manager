// Package supervisor 通用子进程守护：hostapd / dnsmasq / wpa_supplicant / pppd 共用一套。
// 状态机 stopped → starting → running → backoff → stopped | failed；
// 指数退避重启（1s/2s/…/30s 封顶），连续 5 次失败进入 failed 不再自动拉起。
package supervisor

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ProcState 进程状态
type ProcState string

const (
	StateStopped  ProcState = "stopped"
	StateStarting ProcState = "starting"
	StateRunning  ProcState = "running"
	StateBackoff  ProcState = "backoff"
	StateFailed   ProcState = "failed"
)

// Spec 一个被托管进程的规格
type Spec struct {
	Name     string   // 展示名（hostapd / dnsmasq / wpa_supplicant / pppd）
	Argv     []string // 完整命令行（含 argv[0]）
	ReadyFn  func() error        // 启动后轮询的就绪探测（nil 则 3 秒未退出即 running）
	LogRing  *Ring                // stderr/stdout 环形缓冲（诊断面板用），可为 nil
	FailLimit int                 // 连续失败次数上限（默认 5）
}

// Proc 一个受管进程实例
type Proc struct {
	mu        sync.Mutex
	spec      *Spec
	state     ProcState
	cmd       *exec.Cmd
	starts    int       // 总启动次数
	fails     int       // 连续失败
	lastErr   string
	startedAt time.Time
	log       *slog.Logger
	cancel    context.CancelFunc
	done      chan struct{} // 当前进程退出信号
	stopping  bool          // 用户主动停止标记
	onState   func(name string, s ProcState, detail string)
}

// Supervisor 汇总视图
type Supervisor struct {
	mu        sync.Mutex
	procs     map[string]*Proc
	log       *slog.Logger
	onStateCB func(name string, st ProcState, detail string)
}

func New(log *slog.Logger) *Supervisor {
	return &Supervisor{procs: map[string]*Proc{}, log: log}
}

// OnState 注册状态变化回调（SSE 事件来源）
func (s *Supervisor) OnState(fn func(name string, st ProcState, detail string)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.procs {
		p.onState = fn
	}
	s.onStateCB = fn
}

// Start 启动（或重启）一个受管进程。已存在则先 Stop 再拉起。
func (s *Supervisor) Start(spec *Spec) error {
	s.mu.Lock()
	if old, ok := s.procs[spec.Name]; ok {
		s.mu.Unlock()
		old.Stop(10 * time.Second)
		s.mu.Lock()
	}
	p := &Proc{spec: spec, log: s.log, onState: s.onStateCB}
	if spec.FailLimit == 0 {
		spec.FailLimit = 5
	}
	s.procs[spec.Name] = p
	s.mu.Unlock()
	return p.start()
}

// Stop 停止指定进程
func (s *Supervisor) Stop(name string, timeout time.Duration) {
	s.mu.Lock()
	p := s.procs[name]
	s.mu.Unlock()
	if p != nil {
		p.Stop(timeout)
	}
}

// StopAll 逆序停止全部（main 优雅退出用）
func (s *Supervisor) StopAll(timeout time.Duration) {
	s.mu.Lock()
	procs := make([]*Proc, 0, len(s.procs))
	for _, p := range s.procs {
		procs = append(procs, p)
	}
	s.mu.Unlock()
	for _, p := range procs {
		p.Stop(timeout)
	}
}

// SweepOrphans 启动清扫：杀掉持有我们生成配置的残留进程（上一实例被 SIGKILL
// 时留下的孤儿 hostapd/dnsmasq/wpa_supplicant，它们占着 LAN 地址会让新实例起不来）。
// 识别方式：/proc/*/cmdline 含 runDir 路径。只杀持有我们路径的，绝不误伤系统服务。
func SweepOrphans(runDir string, log *slog.Logger) int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0
	}
	self := os.Getpid()
	killed := 0
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == self {
			continue
		}
		cmdline, err := os.ReadFile("/proc/" + e.Name() + "/cmdline")
		if err != nil {
			continue // 进程刚退出，正常
		}
		cmd := strings.ReplaceAll(string(cmdline), "\x00", " ")
		if !strings.Contains(cmd, runDir) {
			continue
		}
		log.Warn("清扫残留子进程", "pid", pid, "cmd", strings.TrimSpace(cmd))
		// SIGTERM → 1s → SIGKILL
		_ = syscall.Kill(pid, syscall.SIGTERM)
		go func(pid int) {
			time.Sleep(time.Second)
			// 进程组一起杀（防子进程再 fork）
			_ = syscall.Kill(pid, syscall.SIGKILL)
			_ = syscall.Kill(-pid, syscall.SIGKILL)
		}(pid)
		killed++
	}
	if killed > 0 {
		time.Sleep(1200 * time.Millisecond) // 等 SIGTERM 生效/残留端口释放
	}
	return killed
}

// Status 全部进程状态快照
func (s *Supervisor) Status() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []map[string]any{}
	for name, p := range s.procs {
		p.mu.Lock()
		out = append(out, map[string]any{
			"name":      name,
			"state":     string(p.state),
			"starts":    p.starts,
			"fails":     p.fails,
			"lastErr":   p.lastErr,
			"startedAt": p.startedAt.Unix(),
		})
		p.mu.Unlock()
	}
	return out
}

// GetProc 取单个进程（hostapd 事件读取等）
func (s *Supervisor) GetProc(name string) *Proc {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.procs[name]
}

// TailLog 取某进程诊断日志尾部（日志页诊断面板用）
func (s *Supervisor) TailLog(name string, n int) []RingItem {
	p := s.GetProc(name)
	if p == nil || p.spec.LogRing == nil {
		return nil
	}
	return p.spec.LogRing.Tail(n)
}

// ---- Proc ----

func (p *Proc) start() error {
	p.mu.Lock()
	if p.state == StateRunning || p.state == StateStarting {
		p.mu.Unlock()
		return errors.New("进程已在运行")
	}
	p.stopping = false
	p.state = StateStarting
	p.starts++
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	cmd := exec.CommandContext(ctx, p.spec.Argv[0], p.spec.Argv[1:]...)
	// 进程组管理，防止孤儿
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdout = p
	cmd.Stderr = p
	done := make(chan struct{})
	p.done = done
	p.cmd = cmd // Stop()/Pid() 依赖；漏赋会导致进程杀不掉
	startedAt := time.Now()
	p.startedAt = startedAt
	spec := p.spec
	p.mu.Unlock()

	if err := cmd.Start(); err != nil {
		p.mu.Lock()
		p.state = StateFailed
		p.lastErr = err.Error()
		p.mu.Unlock()
		p.emitState(StateFailed, err.Error())
		return err
	}

	go func() {
		err := cmd.Wait()
		close(done)
		p.mu.Lock()
		runtime := time.Since(startedAt)
		p.cancel = nil
		p.cmd = nil // 已退出，Stop()/Pid() 不再可用
		detail := ""
		if err != nil && !errors.Is(err, context.Canceled) {
			if ee, ok := err.(*exec.ExitError); ok {
				detail = "exit " + ee.String()
			} else {
				detail = err.Error()
			}
		}
		// 用户主动停止 → stopped；3 秒内退出视为启动失败
		switch {
		case p.stopping:
			p.state = StateStopped
			p.fails = 0
		case runtime < 3*time.Second:
			p.fails++
			p.lastErr = detail
			if p.fails >= spec.FailLimit {
				p.state = StateFailed
			} else {
				p.state = StateBackoff
			}
		default:
			// 正常退出（如配置重载）→ backoff 而非 failed
			p.fails++
			p.lastErr = detail
			if p.fails >= spec.FailLimit {
				p.state = StateFailed
			} else {
				p.state = StateBackoff
			}
		}
		state := p.state
		fails := p.fails
		p.mu.Unlock()
		p.emitState(state, detail)

		// 退避重启
		if state == StateBackoff {
			backoff := time.Duration(1) << uint(min(fails-1, 5)) * time.Second
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
			time.Sleep(backoff)
			p.mu.Lock()
			if p.state == StateBackoff && !p.stopping {
				p.mu.Unlock()
				_ = p.start()
				return
			}
			p.mu.Unlock()
		}
	}()

	// 就绪判定：ReadyFn 轮询（间隔 200ms，至多 10s）；无 ReadyFn 则 3s 未退出即 running
	go func() {
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			select {
			case <-done:
				return
			case <-time.After(200 * time.Millisecond):
			}
			if spec.ReadyFn != nil {
				if err := spec.ReadyFn(); err == nil {
					p.setStateRunning()
					return
				}
			} else if time.Since(startedAt) >= 3*time.Second {
				p.setStateRunning()
				return
			}
		}
		p.mu.Lock()
		if p.state == StateStarting {
			p.state = StateRunning // ReadyFn 一直不就绪也按 running 处理（由事件流进一步诊断）
		}
		p.mu.Unlock()
	}()
	return nil
}

func (p *Proc) setStateRunning() {
	p.mu.Lock()
	if p.state == StateStarting {
		p.state = StateRunning
		p.fails = 0 // 成功运行重置连续失败计数
	}
	p.mu.Unlock()
}

// Stop 优雅停止：SIGTERM(进程组) → 超时 SIGKILL
func (p *Proc) Stop(timeout time.Duration) {
	p.mu.Lock()
	if p.state == StateStopped || p.state == StateFailed || p.cmd == nil {
		p.mu.Unlock()
		return
	}
	p.stopping = true
	cmd := p.cmd
	done := p.done
	p.mu.Unlock()

	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	}
	select {
	case <-done:
	case <-time.After(timeout):
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		select {
		case <-done:
		case <-time.After(3 * time.Second):
		}
	}
	p.mu.Lock()
	p.state = StateStopped
	p.cmd = nil
	p.mu.Unlock()
	p.emitState(StateStopped, "")
}

// State 当前状态
func (p *Proc) State() ProcState {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.state
}

// Pid 进程号（未运行为 -1）
func (p *Proc) Pid() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cmd != nil && p.cmd.Process != nil {
		return p.cmd.Process.Pid
	}
	return -1
}

func (p *Proc) emitState(st ProcState, detail string) {
	p.log.Info("supervisor", "proc", p.spec.Name, "state", string(st), "detail", detail)
	if p.onState != nil {
		p.onState(p.spec.Name, st, detail)
	}
}

// Write 实现 io.Writer：子进程 stdout/stderr 进环形缓冲与日志
func (p *Proc) Write(b []byte) (int, error) {
	line := strings.TrimRight(string(b), "\r\n")
	if line != "" {
		p.log.Info("["+p.spec.Name+"]", "out", line)
		if p.spec.LogRing != nil {
			p.spec.LogRing.Push(time.Now(), line)
		}
	}
	return len(b), nil
}

// Ring 定长环形缓冲（子进程诊断日志 / 应用日志页共用）
type Ring struct {
	mu    sync.Mutex
	items []RingItem
	max   int
}

// RingItem 一条带时间的记录
type RingItem struct {
	Ts   time.Time `json:"ts"`
	Line string    `json:"line"`
}

func NewRing(max int) *Ring { return &Ring{max: max} }

// Push 追加一行（超出容量丢最旧）
func (r *Ring) Push(ts time.Time, line string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.items = append(r.items, RingItem{Ts: ts, Line: line})
	if len(r.items) > r.max {
		r.items = r.items[len(r.items)-r.max:]
	}
}

// Tail 取最近 n 条
func (r *Ring) Tail(n int) []RingItem {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n > len(r.items) {
		n = len(r.items)
	}
	out := make([]RingItem, n)
	copy(out, r.items[len(r.items)-n:])
	return out
}

// 环境辅助：确保 PATH 覆盖 /usr/sbin（cron/systemd 环境常缺）
func init() {
	if !strings.Contains(os.Getenv("PATH"), "/usr/sbin") {
		os.Setenv("PATH", os.Getenv("PATH")+":/usr/sbin:/sbin")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
