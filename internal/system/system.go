// Package system 系统与安全模块：管理密码与会话（HMAC Cookie）、state.json 持久化、
// 登录限速。系统状态采样与日志环形缓冲也在本包。
package system

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// State 持久化到 /var/lib/apmanager/state.json 的运行状态
type State struct {
	PasswordHash       string             `json:"password_hash"`
	MustChangePassword bool               `json:"must_change_password"`
	SessionKey         string             `json:"session_key"` // hex，会话签名密钥
	Aliases            map[string]string  `json:"aliases"`     // MAC -> 别名（devices 模块共用）
	BlockedMACs        []string           `json:"blocked_macs"`
	Rates              map[string][2]int  `json:"rates"` // MAC -> [下行Mbps, 上行Mbps]（0=不限）
	TrafficTotal       map[string][2]uint64 `json:"traffic_total"` // MAC -> [rx,tx]（防重启丢失）
}

// Manager system 模块
type Manager struct {
	mu    sync.Mutex
	state *State
	path  string
	log   *slog.Logger

	loginMu   sync.Mutex
	loginFail map[string]*loginTracker // IP -> 失败记录（限速 5 次/分钟）
}

type loginTracker struct {
	count    int
	windowAt time.Time
}

const sessionDuration = 7 * 24 * time.Hour

// New 加载（或初始化）state.json
func New(statePath string, log *slog.Logger) (*Manager, error) {
	m := &Manager{path: statePath, log: log, loginFail: map[string]*loginTracker{}}
	data, err := os.ReadFile(statePath)
	if err == nil {
		st := &State{}
		if err := unmarshalStrict(data, st); err != nil {
			return nil, fmt.Errorf("解析 %s: %w", statePath, err)
		}
		m.state = st
		return m, nil
	}
	if !os.IsNotExist(err) {
		return nil, err
	}
	// 首跑：生成随机初始密码
	pw, err := randomPassword(12)
	if err != nil {
		return nil, err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	m.state = &State{
		PasswordHash:       string(hash),
		MustChangePassword: true,
		SessionKey:         hex.EncodeToString(key),
		Aliases:            map[string]string{},
		TrafficTotal:       map[string][2]uint64{},
	}
	if err := m.persistLocked(); err != nil {
		return nil, err
	}
	// 初始密码打印到控制台 + 写 root-only 文件
	fmt.Printf("\n==== apmanager 首次启动 ====\n初始管理密码: %s\n（首次登录后将被强制修改；也可查看 %s.initial_password）\n==============================\n\n", pw, statePath)
	if err := os.WriteFile(statePath+".initial_password", []byte(pw+"\n"), 0o600); err != nil {
		log.Warn("写初始密码文件失败", "err", err)
	}
	return m, nil
}

// unmarshalStrict 反序列化（宽松字段名）
func unmarshalStrict(data []byte, st *State) error {
	return jsonUnmarshal(data, st)
}

// persistLocked 落盘（调用方持锁）；防抖由调用方决定（别名等低频变更直接写）
func (m *Manager) persistLocked() error {
	data, err := jsonMarshal(m.state)
	if err != nil {
		return err
	}
	return atomicWrite(m.path, data, 0o600)
}

// Login 校验密码并签发会话 cookie 值；带每 IP 每分钟 5 次失败限速
func (m *Manager) Login(ip, password string) (cookie string, mustChange bool, err error) {
	// 限速检查
	m.loginMu.Lock()
	tr := m.loginFail[ip]
	if tr == nil || time.Since(tr.windowAt) > time.Minute {
		tr = &loginTracker{windowAt: time.Now()}
		m.loginFail[ip] = tr
	}
	blocked := tr.count >= 5
	m.loginMu.Unlock()
	if blocked {
		return "", false, errors.New("尝试过于频繁，请 1 分钟后再试")
	}

	m.mu.Lock()
	hash := m.state.PasswordHash
	mustChange = m.state.MustChangePassword
	m.mu.Unlock()

	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil {
		m.loginMu.Lock()
		tr.count++
		m.loginMu.Unlock()
		return "", false, errors.New("密码错误")
	}
	m.loginMu.Lock()
	tr.count = 0
	m.loginMu.Unlock()

	// 签发：exp.nonce.sig
	m.mu.Lock()
	key, _ := hex.DecodeString(m.state.SessionKey)
	m.mu.Unlock()
	cookie, err = signSession(key, time.Now().Add(sessionDuration))
	if err != nil {
		return "", false, err
	}
	m.log.Info("管理登录成功", "ip", ip)
	return cookie, mustChange, nil
}

// ValidateSession 校验会话 cookie；剩余 < 6 天时返回续期 cookie（滑动续期）
func (m *Manager) ValidateSession(cookie string) (ok bool, renewed string) {
	m.mu.Lock()
	key, _ := hex.DecodeString(m.state.SessionKey)
	m.mu.Unlock()
	exp, _, err := verifySession(key, cookie)
	if err != nil || time.Now().After(exp) {
		return false, ""
	}
	if time.Until(exp) < 6*24*time.Hour {
		if renewed, err = signSession(key, time.Now().Add(sessionDuration)); err != nil {
			renewed = ""
		}
	}
	return true, renewed
}

// SetPassword 修改密码（改完旧会话全部失效：轮换签名密钥）
func (m *Manager) SetPassword(old, new string) error {
	if len(new) < 8 || len(new) > 63 {
		return errors.New("新密码长度须 8–63")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if bcrypt.CompareHashAndPassword([]byte(m.state.PasswordHash), []byte(old)) != nil {
		return errors.New("当前密码不正确")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(new), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return err
	}
	m.state.PasswordHash = string(hash)
	m.state.MustChangePassword = false
	m.state.SessionKey = hex.EncodeToString(key) // 旧会话全部失效
	return m.persistLocked()
}

// State 只读快照（内部模块用）
func (m *Manager) Snapshot() State {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *m.state
	return cp
}

// Mutate 带锁修改状态并落盘
func (m *Manager) Mutate(fn func(*State)) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	fn(m.state)
	return m.persistLocked()
}

// ---- 会话签名 ----

func signSession(key []byte, exp time.Time) (string, error) {
	nonce := make([]byte, 8)
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	body := fmt.Sprintf("%d.%s", exp.Unix(), hex.EncodeToString(nonce))
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(body))
	return body + "." + hex.EncodeToString(mac.Sum(nil)), nil
}

func verifySession(key []byte, cookie string) (exp time.Time, nonce string, err error) {
	parts := strings.Split(cookie, ".")
	if len(parts) != 3 {
		return time.Time{}, "", errors.New("会话格式非法")
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(parts[0] + "." + parts[1]))
	if !hmac.Equal([]byte(hex.EncodeToString(mac.Sum(nil))), []byte(parts[2])) {
		return time.Time{}, "", errors.New("会话签名不符")
	}
	var expUnix int64
	fmt.Sscanf(parts[0], "%d", &expUnix)
	return time.Unix(expUnix, 0), parts[1], nil
}

// randomPassword 生成易抄写的随机密码（去掉 0/O/1/I 混淆字符）
func randomPassword(n int) (string, error) {
	const alphabet = "abcdefghjkmnpqrstuvwxyz23456789"
	var b strings.Builder
	for i := 0; i < n; i++ {
		idx, err := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		if err != nil {
			return "", err
		}
		b.WriteByte(alphabet[idx.Int64()])
	}
	return b.String(), nil
}
