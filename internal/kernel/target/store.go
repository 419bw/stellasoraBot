// Package target 是主动推送目标的存储与管理基础设施。
//
// 分层契约：本包是基础设施，不含任何游戏或公告业务词，只认目标标识字符串
// （形如 "g:<group_openid>" 或 "u:<user_openid>"）。
// 读写分离：
// - View 暴露只读目标列表（给内核与主动推送 Feature）；
// - Manager 暴露动态启停能力（给管理员命令 Feature）。
package target

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"xingta/internal/store"
)

// NS 是主动推送目标在 store.Doc 中占用的命名空间。
const NS = "targets"

// Record 记录一个目标的持久化信息。
type Record struct {
	Target    string    `json:"target"`
	EnabledAt time.Time `json:"enabled_at"`
}

// Store 负责维护主动推送目标，包含内存镜像与底层 store.Doc 持久化。
// 写入模式：先落盘（bbolt）后更新内存镜像，保证崩溃安全性与状态一致性；
// 读取模式：基于 RWMutex 的纯内存查读，避免热路径频繁开启 bbolt 只读事务。
type Store struct {
	doc store.Doc
	now func() time.Time

	mu      sync.RWMutex
	targets map[string]Record
}

// Validate 校验目标格式：必须以 "g:"（群）或 "u:"（用户）为前缀，且 openid 不能为空且不含空格。
func Validate(target string) error {
	if !strings.HasPrefix(target, "g:") && !strings.HasPrefix(target, "u:") {
		return fmt.Errorf("目标 %q 缺少前缀，应写成 g:<群 openid> 或 u:<用户 openid>", target)
	}
	id := target[2:]
	if id == "" {
		return fmt.Errorf("目标 %q 的前缀后面没有 openid", target)
	}
	if strings.TrimSpace(id) != id {
		return fmt.Errorf("目标 %q 的 openid 包含空白字符", target)
	}
	return nil
}

// NewStore 创建目标存储。
// doc 可为空（测试或无持久化场景）；
// staticTargets 是启动时注入的静态种子目标（如命令行 -push），会经过校验并载入内存。
func NewStore(doc store.Doc, staticTargets []string) (*Store, error) {
	s := &Store{
		doc:     doc,
		now:     time.Now,
		targets: make(map[string]Record),
	}

	// 1. 从持久化存储载入
	if doc != nil {
		err := doc.Scan(NS, "", func(key string, raw []byte) error {
			var rec Record
			if err := json.Unmarshal(raw, &rec); err != nil {
				// 解不开则容错：按已启用处理，键就是目标
				rec = Record{Target: key, EnabledAt: time.Time{}}
			}
			if err := Validate(rec.Target); err == nil {
				s.targets[rec.Target] = rec
			}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("target: 读取持久化记录失败: %w", err)
		}
	}

	// 2. 合并静态目标种子（例如 -push 参数）
	for _, st := range staticTargets {
		st = strings.TrimSpace(st)
		if st == "" {
			continue
		}
		if err := Validate(st); err != nil {
			return nil, fmt.Errorf("target: 静态目标格式错误: %w", err)
		}
		if _, exists := s.targets[st]; !exists {
			s.targets[st] = Record{
				Target:    st,
				EnabledAt: s.now(),
			}
		}
	}

	return s, nil
}

// Enable 启用目标并写盘。若已启用则刷新启用时间。
func (s *Store) Enable(target string) error {
	if err := Validate(target); err != nil {
		return err
	}

	rec := Record{
		Target:    target,
		EnabledAt: s.now(),
	}

	// 先落盘，确保写入成功
	if s.doc != nil {
		if err := s.doc.Put(NS, target, rec); err != nil {
			return fmt.Errorf("target: 启用目标落盘失败: %w", err)
		}
	}

	s.mu.Lock()
	s.targets[target] = rec
	s.mu.Unlock()

	return nil
}

// Disable 停用目标。无论此前来自静态种子还是动态添加，均从持久化与内存中同时移除。
// 返回值表示被移除前是否已存在。
func (s *Store) Disable(target string) (bool, error) {
	if err := Validate(target); err != nil {
		return false, err
	}

	var writeErr error
	if s.doc != nil {
		if err := s.doc.Delete(NS, target); err != nil {
			writeErr = fmt.Errorf("target: 停用目标落盘删除失败: %w", err)
		}
	}

	s.mu.Lock()
	_, existed := s.targets[target]
	delete(s.targets, target)
	s.mu.Unlock()

	if writeErr != nil {
		return existed, writeErr
	}
	return existed, nil
}

// Has 判断目标是否已启用。
func (s *Store) Has(target string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.targets[target]
	return ok
}

// Targets 返回当前所有启用的目标列表，按字典序排序以保证确定性输出。
func (s *Store) Targets() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	res := make([]string, 0, len(s.targets))
	for t := range s.targets {
		res = append(res, t)
	}
	sort.Strings(res)
	return res
}

// List 与 Targets 同义，给管理端展示。
func (s *Store) List() []string {
	return s.Targets()
}

// SetNow 允许单测注入时钟。
func (s *Store) SetNow(now func() time.Time) {
	if now != nil {
		s.now = now
	}
}

var (
	_ View    = (*Store)(nil)
	_ Manager = (*Store)(nil)
)
