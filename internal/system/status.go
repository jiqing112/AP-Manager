// status.go —— 系统状态采样（CPU/内存/温度/负载/磁盘/uptime）与日志设施。
package system

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"apmanager/internal/bus"
	"apmanager/internal/supervisor"
)

// SystemStatus 一次采样
type SystemStatus struct {
	CPUPercent float64 `json:"cpuPercent"`
	MemTotalMB int     `json:"memTotalMB"`
	MemUsedMB  int     `json:"memUsedMB"`
	TempC      float64 `json:"tempC"`
	Load1      float64 `json:"load1"`
	Load5      float64 `json:"load5"`
	Load15     float64 `json:"load15"`
	UptimeSec  int64   `json:"uptimeSec"`
	DiskUsedMB int     `json:"diskUsedMB"`
	DiskTotalMB int    `json:"diskTotalMB"`
	Ts         int64   `json:"ts"`
}

// Sampler 系统状态采样器
type Sampler struct {
	bus      *bus.Bus
	log      *slog.Logger
	lastCPU  [2]uint64 // user+nice+system+idle+iotime... 用整体 total/idle 差分
	lastIdle uint64

	mu      sync.Mutex
	latest  SystemStatus
}

// NewSampler 构造
func NewSampler(b *bus.Bus, log *slog.Logger) *Sampler { return &Sampler{bus: b, log: log} }

// Latest 最近一次采样（API 轮询用；SSE 客户端拿实时流）
func (s *Sampler) Latest() SystemStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.latest
}

// Start 启动 2s 采样循环
func (s *Sampler) Start(stop <-chan struct{}) {
	s.sample() // 预热一次基线
	go func() {
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				s.bus.Publish("system", s.sample())
			}
		}
	}()
}

func (s *Sampler) sample() SystemStatus {
	st := SystemStatus{Ts: time.Now().UnixMilli()}
	st.CPUPercent = s.cpuPercent()
	st.MemTotalMB, st.MemUsedMB = memInfo()
	st.TempC = maxTemp()
	st.Load1, st.Load5, st.Load15 = loadAvg()
	st.UptimeSec = uptime()
	st.DiskUsedMB, st.DiskTotalMB = diskUsage("/var/lib")
	s.mu.Lock()
	s.latest = st
	s.mu.Unlock()
	return st
}

func readProcCPU() (total, idle uint64) {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "cpu ") {
			f := strings.Fields(line)[1:]
			var vals []uint64
			for _, v := range f {
				if n, err := strconv.ParseUint(v, 10, 64); err == nil {
					vals = append(vals, n)
				}
			}
			for _, v := range vals {
				total += v
			}
			if len(vals) >= 4 {
				idle = vals[3]
			}
			if len(vals) >= 4 { // idle + iowait
				idle += 0
			}
			return total, idle
		}
	}
	return 0, 0
}

func (s *Sampler) cpuPercent() float64 {
	total, idle := readProcCPU()
	if s.lastCPU[0] == 0 {
		s.lastCPU = [2]uint64{total, idle}
		return 0
	}
	dt := total - s.lastCPU[0]
	di := idle - s.lastIdle
	s.lastCPU = [2]uint64{total, idle}
	s.lastIdle = idle
	if dt == 0 {
		return 0
	}
	return float64(dt-di) / float64(dt) * 100
}

func memInfo() (totalMB, usedMB int) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	var total, avail uint64
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		v, _ := strconv.ParseUint(f[1], 10, 64)
		switch f[0] {
		case "MemTotal:":
			total = v
		case "MemAvailable:":
			avail = v
		}
	}
	return int(total / 1024), int((total - avail) / 1024)
}

func maxTemp() float64 {
	best := 0.0
	entries, _ := filepath.Glob("/sys/class/thermal/thermal_zone*/temp")
	for _, e := range entries {
		data, err := os.ReadFile(e)
		if err != nil {
			continue
		}
		if milli, err := strconv.ParseFloat(strings.TrimSpace(string(data)), 64); err == nil {
			if c := milli / 1000; c > best && c < 120 { // 过滤异常值
				best = c
			}
		}
	}
	return best
}

func loadAvg() (l1, l5, l15 float64) {
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0, 0, 0
	}
	fmt.Sscanf(string(data), "%f %f %f", &l1, &l5, &l15)
	return
}

func uptime() int64 {
	data, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0
	}
	var up float64
	fmt.Sscanf(string(data), "%f", &up)
	return int64(up)
}

func diskUsage(path string) (usedMB, totalMB int) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0
	}
	total := st.Blocks * uint64(st.Bsize)
	free := st.Bfree * uint64(st.Bsize)
	return int((total - free) / 1024 / 1024), int(total / 1024 / 1024)
}

// ---- 日志设施 ----

// AppRing 应用日志环形缓冲（日志页 /api/logs 的数据源）
type AppRing struct {
	*supervisor.Ring
}

// NewAppRing 最近 2000 条
func NewAppRing() *AppRing { return &AppRing{supervisor.NewRing(2000)} }

// rotatingWriter 10MB 轮转留 3 份的文件 writer
type rotatingWriter struct {
	mu      sync.Mutex
	path    string
	f       *os.File
	size    int64
	maxSize int64
	keep    int
}

// NewRotatingWriter 打开（必要时创建）日志文件
func NewRotatingWriter(path string) (*rotatingWriter, error) {
	w := &rotatingWriter{path: path, maxSize: 10 * 1024 * 1024, keep: 3}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
	if err != nil {
		return nil, err
	}
	if st, err := f.Stat(); err == nil {
		w.size = st.Size()
	}
	w.f = f
	return w, nil
}

func (w *rotatingWriter) Write(b []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.size+int64(len(b)) > w.maxSize {
		w.rotate()
	}
	n, err := w.f.Write(b)
	w.size += int64(n)
	return n, err
}

func (w *rotatingWriter) rotate() {
	w.f.Close()
	for i := w.keep - 1; i >= 1; i-- {
		os.Rename(fmt.Sprintf("%s.%d", w.path, i), fmt.Sprintf("%s.%d", w.path, i+1))
	}
	os.Rename(w.path, w.path+".1")
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
	if err == nil {
		w.f = f
		w.size = 0
	}
}

// ringWriter 把 slog 输出同喂环形缓冲（日志页实时流）
type ringWriter struct{ ring *AppRing }

func (r *ringWriter) Write(b []byte) (int, error) {
	r.ring.Push(time.Now(), strings.TrimRight(string(b), "\n"))
	return len(b), nil
}

// SetupLogging 组装日志链：控制台 + 轮转文件 + 环形缓冲；返回环形缓冲与 slog.Logger
func SetupLogging(logPath string) (*AppRing, *slog.Logger, error) {
	ring := NewAppRing()
	rw, err := NewRotatingWriter(logPath)
	if err != nil {
		return nil, nil, err
	}
	var multi *slog.Logger
	if isSystemd() {
		// systemd 下 stdout 已进 journald，文件照写
		multi = slog.New(slog.NewTextHandler(ioMulti(os.Stdout, rw, &ringWriter{ring}), nil))
	} else {
		multi = slog.New(slog.NewTextHandler(ioMulti(os.Stdout, rw, &ringWriter{ring}), nil))
	}
	slog.SetDefault(multi)
	return ring, multi, nil
}

func isSystemd() bool {
	_, err := os.Stat("/run/systemd/system")
	return err == nil
}

type multiWriter struct{ writers []*bytes.Buffer }

func ioMulti(writers ...interface{ Write([]byte) (int, error) }) *mw { return &mw{ws: writers} }

type mw struct{ ws []interface{ Write([]byte) (int, error) } }

func (m *mw) Write(b []byte) (int, error) {
	for _, w := range m.ws {
		_, _ = w.Write(b)
	}
	return len(b), nil
}

// jsonMarshal/jsonUnmarshal/atomicWrite 小助手（避免多处引相同包名）
func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }
func jsonUnmarshal(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	return dec.Decode(v)
}
func atomicWrite(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
