// snapshot.go —— 变更前的接口/地址/桥成员快照与恢复（自动回滚的依据）。
package ethport

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"apmanager/internal/config"
	"apmanager/internal/priv"
)

// IfaceSnap 一个接口的地址与桥成员关系
type IfaceSnap struct {
	Name    string
	Addrs   []string
	Master  string
	Up      bool
	Existed bool
}

// Snapshot 一次应用前的网络面快照
type Snapshot struct {
	TakenAt time.Time
	Ifaces  []IfaceSnap
	NftRuleset string // 变更前的 nft 表导出（自家表）
}

// takeSnapshot 收集将要触碰的接口的当前状态（候选里出现的口 + 相关桥）
func takeSnapshot(cur *config.Config, cand Candidate) (*Snapshot, error) {
	s := &Snapshot{TakenAt: time.Now()}
	interesting := map[string]bool{}
	for _, p := range cand.Ports {
		interesting[p.Name] = true
	}
	for _, p := range cur.Ports {
		interesting[p.Name] = true
	}
	for _, seg := range cand.LAN {
		if seg.Bridge != "" {
			interesting[seg.Bridge] = true
		}
	}
	for _, seg := range cur.LAN {
		if seg.Bridge != "" {
			interesting[seg.Bridge] = true
		}
	}
	links, err := priv.AllLinks()
	if err != nil {
		return nil, err
	}
	for _, li := range links {
		if !interesting[li.Name] {
			continue
		}
		s.Ifaces = append(s.Ifaces, IfaceSnap{
			Name: li.Name, Addrs: append([]string{}, li.Addrs...),
			Master: li.Master, Up: li.OperState == "up", Existed: true,
		})
	}
	// 候选中要新建的桥：现在不存在 → 回滚时删除
	for name := range interesting {
		found := false
		for _, li := range links {
			if li.Name == name {
				found = true
				break
			}
		}
		if !found {
			s.Ifaces = append(s.Ifaces, IfaceSnap{Name: name, Existed: false})
		}
	}
	s.NftRuleset = priv.NftExport()
	return s, nil
}

// restore 恢复快照：地址、桥成员、桥存在性、nft 表
func (s *Snapshot) restore() error {
	var firstErr error
	keepErr := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	for _, is := range s.Ifaces {
		if !is.Existed {
			// 变更中新建的接口（桥）：回滚 = 删除
			keepErr(priv.BridgeDelete(is.Name))
			continue
		}
		// 恢复桥成员关系
		if is.Master != "" {
			keepErr(priv.BridgeAddPort(is.Master, is.Name))
		} else {
			keepErr(priv.BridgeDelPort(is.Name))
		}
		// 恢复地址（先清再加）
		cur, _ := priv.LinkByName(is.Name)
		for _, a := range cur.Addrs {
			if !strings.Contains(a, "fe80::") {
				keepErr(priv.AddrFlush(is.Name))
				break
			}
		}
		for _, a := range is.Addrs {
			if strings.Contains(a, "fe80::") {
				continue // 链路本地自动生成
			}
			if err := addrAdd(is.Name, a); err != nil {
				keepErr(err)
			}
		}
		if is.Up {
			keepErr(priv.SetUp(is.Name))
		}
	}
	// nft 表恢复（导出文本原样重放；"(表不存在)" 占位则删除）
	if strings.Contains(s.NftRuleset, "table inet apmanager") {
		keepErr(priv.NftApply(normalizeNftDump(s.NftRuleset)))
	} else {
		priv.NftDelete()
	}
	return firstErr
}

// addrAdd 幂等加地址（已存在同地址则跳过）
func addrAdd(name, cidr string) error {
	li, err := priv.LinkByName(name)
	if err != nil {
		return err
	}
	for _, a := range li.Addrs {
		if a == cidr {
			return nil
		}
	}
	return priv.AddrReplace(name, cidr)
}

// normalizeNftDump 把 nft list 输出裁成可重放的表定义
// （导出文本带缩进与注释，nft -f 可直接吃；去掉头部可能的 banner 行）
func normalizeNftDump(dump string) string {
	var b strings.Builder
	inTable := false
	for _, line := range strings.Split(dump, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "table inet apmanager") {
			inTable = true
		}
		if inTable {
			b.WriteString(line + "\n")
		}
	}
	return b.String()
}

// readFileAll/readDirAll 小助手（探测 ifupdown 托管用）
func readFileAll(path string) (string, error) {
	data, err := os.ReadFile(path)
	return string(data), err
}

func readDirAll(dir string) (string, error) {
	var b strings.Builder
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err == nil {
			b.WriteString(string(data) + "\n")
		}
	}
	return b.String(), nil
}

var _ = fmt.Sprintf
