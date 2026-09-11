// Package target 是主动推送目标的存储与管理基础设施。
//
// 分层契约：本包是基础设施，不含任何游戏或公告业务词，只认目标标识字符串
// （形如 "g:<group_openid>" 或 "u:<user_openid>"）与通用的主题键名。
// 读写分离：
// - View 暴露只读目标列表（给内核与主动推送 Feature）；
// - Manager 暴露动态启停能力（给管理员命令 Feature）。
package target

import (
	"encoding/json"
	"errors"
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
	Target    string          `json:"target"`
	EnabledAt time.Time       `json:"enabled_at"`
	Topics    map[string]bool `json:"topics,omitempty"`
}

// Store 负责维护主动推送目标，包含内存镜像与底层 store.Doc 持久化。
// 写入模式：先落盘（bbolt）后更新内存镜像，保证崩溃安全性与状态一致性；
// 读取模式：基于 RWMutex 的纯内存查读，避免热路径频繁开启 bbolt 只读事务。
type Store struct {
	doc store.Doc
	now func() time.Time

	mu      sync.RWMutex
	targets map[string]Record
	topics  map[string]Topic
	static  map[string]bool
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
		topics:  make(map[string]Topic),
		static:  make(map[string]bool),
	}

	// 1. 从持久化存储载入
	if doc != nil {
		err := doc.Scan(NS, "", func(key string, raw []byte) error {
			var rec Record
			if err := json.Unmarshal(raw, &rec); err != nil {
				rec = Record{Target: key, EnabledAt: time.Time{}}
			}
			if rec.Topics == nil {
				rec.Topics = make(map[string]bool)
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

	// 2. 载入静态目标种子（例如 -push 参数）
	for _, st := range staticTargets {
		st = strings.TrimSpace(st)
		if st == "" {
			continue
		}
		if err := Validate(st); err != nil {
			return nil, fmt.Errorf("target: 静态目标格式错误: %w", err)
		}
		s.static[st] = true
		if _, exists := s.targets[st]; !exists {
			s.targets[st] = Record{
				Target:    st,
				EnabledAt: s.now(),
				Topics:    make(map[string]bool),
			}
		}
	}

	return s, nil
}

// RegisterTopic 注册一个可用主题。
// 会将已载入的静态种子目标自动开启该主题。
func (s *Store) RegisterTopic(t Topic) error {
	if strings.TrimSpace(t.Key) == "" {
		return errors.New("target: topic key 不能为空")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.topics[t.Key] = t

	// 静态种子目标自动开启新注册的主题
	for st := range s.static {
		if rec, ok := s.targets[st]; ok {
			if rec.Topics == nil {
				rec.Topics = make(map[string]bool)
			}
			rec.Topics[t.Key] = true
			s.targets[st] = rec
		}
	}

	return nil
}

// Topics 返回当前所有已注册的主题列表，按 Key 排序保证确定性输出。
func (s *Store) Topics() []Topic {
	s.mu.RLock()
	defer s.mu.RUnlock()

	res := make([]Topic, 0, len(s.topics))
	for _, t := range s.topics {
		res = append(res, t)
	}
	sort.Slice(res, func(i, j int) bool {
		return res[i].Key < res[j].Key
	})
	return res
}

// Topic 查询指定 key 的主题元数据。
func (s *Store) Topic(key string) (Topic, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.topics[key]
	return t, ok
}

// Enable 启用目标并开启所有当前已注册的主题。
func (s *Store) Enable(target string) error {
	if err := Validate(target); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	rec := Record{
		Target:    target,
		EnabledAt: s.now(),
		Topics:    make(map[string]bool, len(s.topics)),
	}
	for k := range s.topics {
		rec.Topics[k] = true
	}

	if s.doc != nil {
		if err := s.doc.Put(NS, target, rec); err != nil {
			return fmt.Errorf("target: 启用目标落盘失败: %w", err)
		}
	}

	s.targets[target] = rec
	return nil
}

// EnableTopic 启用目标的指定主题。
func (s *Store) EnableTopic(target, topic string) error {
	if err := Validate(target); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.topics[topic]; !ok {
		return fmt.Errorf("target: 未知主题 %q", topic)
	}

	rec, exists := s.targets[target]
	if !exists {
		rec = Record{
			Target:    target,
			EnabledAt: s.now(),
			Topics:    make(map[string]bool),
		}
	}
	if rec.Topics == nil {
		rec.Topics = make(map[string]bool)
	}
	rec.Topics[topic] = true

	if s.doc != nil {
		if err := s.doc.Put(NS, target, rec); err != nil {
			return fmt.Errorf("target: 启用主题落盘失败: %w", err)
		}
	}

	s.targets[target] = rec
	return nil
}

// Disable 停用目标并清除所有主题。无论此前来自静态种子还是动态添加，均彻底移除。
func (s *Store) Disable(target string) (bool, error) {
	if err := Validate(target); err != nil {
		return false, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.static, target)

	var writeErr error
	if s.doc != nil {
		if err := s.doc.Delete(NS, target); err != nil {
			writeErr = fmt.Errorf("target: 停用目标落盘删除失败: %w", err)
		}
	}

	_, existed := s.targets[target]
	delete(s.targets, target)

	if writeErr != nil {
		return existed, writeErr
	}
	return existed, nil
}

// DisableTopic 停用目标的指定主题。若停用后目标没有任何主题，则将其彻底从存储移除。
func (s *Store) DisableTopic(target, topic string) (bool, error) {
	if err := Validate(target); err != nil {
		return false, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.topics[topic]; !ok {
		return false, fmt.Errorf("target: 未知主题 %q", topic)
	}

	rec, exists := s.targets[target]
	if !exists || !rec.Topics[topic] {
		return false, nil
	}

	delete(rec.Topics, topic)
	delete(s.static, target)

	var writeErr error
	if len(rec.Topics) == 0 {
		delete(s.targets, target)
		if s.doc != nil {
			if err := s.doc.Delete(NS, target); err != nil {
				writeErr = fmt.Errorf("target: 移除空目标落盘失败: %w", err)
			}
		}
	} else {
		s.targets[target] = rec
		if s.doc != nil {
			if err := s.doc.Put(NS, target, rec); err != nil {
				writeErr = fmt.Errorf("target: 停用主题落盘更新失败: %w", err)
			}
		}
	}

	if writeErr != nil {
		return true, writeErr
	}
	return true, nil
}

// Has 判断目标是否已启用。
func (s *Store) Has(target string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.targets[target]
	return ok
}

// HasTopic 判断目标是否启用了指定主题。
func (s *Store) HasTopic(target, topic string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rec, ok := s.targets[target]
	return ok && rec.Topics[topic]
}

// TopicsOf 返回目标当前已启用的所有主题 Key 列表（按字母序排序）。
func (s *Store) TopicsOf(target string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rec, ok := s.targets[target]
	if !ok || len(rec.Topics) == 0 {
		return nil
	}

	res := make([]string, 0, len(rec.Topics))
	for t, enabled := range rec.Topics {
		if enabled {
			res = append(res, t)
		}
	}
	sort.Strings(res)
	return res
}

// Targets 返回当前所有已启用的目标列表，按字典序排序以保证确定性输出。
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

// TargetsFor 返回开启了指定主题的目标列表，按字典序排序。
func (s *Store) TargetsFor(topic string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	res := make([]string, 0, len(s.targets))
	for t, rec := range s.targets {
		if rec.Topics[topic] {
			res = append(res, t)
		}
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
