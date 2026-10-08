// nft.go —— nftables 下发：只操作自家 table inet apmanager，整表生成后 `nft -f` 近原子替换。
// 绝不 flush 全局规则；失败时保留旧表并返回 stderr。
package priv

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// NftTable 我们唯一管理的表名
const NftTable = "table inet apmanager"

// NftApply 把整段表定义原子下发（nft -f 临时文件）
func NftApply(ruleset string) error {
	if Mode == "dev" {
		audit("nft(dev)", "ruleset", ruleset)
		return nil
	}
	if DryRun {
		audit("nft dry-run", "ruleset", ruleset)
		return nil
	}
	dir := os.TempDir()
	tmp, err := os.CreateTemp(dir, "apmanager-nft-*.nft")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(ruleset); err != nil {
		tmp.Close()
		return err
	}
	tmp.Close()
	// 先清理旧表（不存在时忽略报错），再整表载入 —— 保证幂等
	_ = runNft("delete table inet apmanager")
	r := runNftFile(tmp.Name())
	if r.Err != nil {
		return fmt.Errorf("nft 下发失败: %s", oneLine(r.Stderr))
	}
	audit("nft applied", "bytes", len(ruleset))
	return nil
}

// NftExport 导出当前自家表（UI 诊断/调试显示）
func NftExport() string {
	if Mode == "dev" || DryRun {
		return "(dev/dry-run 模式无实际规则)"
	}
	r := runNft("list table inet apmanager")
	if r.Err != nil {
		return "(表不存在)"
	}
	return r.Stdout
}

// NftDelete 删除自家表（停服/清场用；不影响其他表）
func NftDelete() {
	if Mode == "dev" || DryRun {
		return
	}
	_ = runNft("delete table inet apmanager")
}

func runNft(args ...string) CmdResult {
	return Run(10*time.Second, "nft", args...)
}

func runNftFile(path string) CmdResult {
	return Run(10*time.Second, "nft", "-f", path)
}

func oneLine(s string) string {
	var parts []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			parts = append(parts, l)
		}
	}
	return strings.Join(parts, " | ")
}

// WriteFileAtomic 原子写文件（配置/状态落盘共用）
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
