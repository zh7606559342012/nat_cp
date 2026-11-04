// Package snapshot 维护 CP 的不可变静态配置快照。
//
// 约定（说明书 9.3 / 10.13）：配置模块（工程师 A）是静态配置的唯一
// 写入者；SQLite 提交成功后构建新 Snapshot 并通过 atomic.Value 一次性
// 替换旧 Snapshot；业务模块（工程师 B）只读 Snapshot，绝不直接改 map。
// 因此 Snapshot 一经发布即不可变，热路径查询无需加锁。
package conf

import (
	"sort"
	"sync/atomic"

	"natcp/main/models"
	"natcp/main/webTypes"
)

// Snapshot 是 OAM 静态配置的内存视图。
type Snapshot struct {
	Version  uint64
	Bindings map[webTypes.BindingKey]webTypes.BusinessBinding
	// VxlanRanges 按 VNIBegin 排序，供 VNI 范围二分命中。
	VxlanRanges []models.VxlanConfig
	// Fttrb 按 FttrbID 索引。
	Fttrb map[string]models.FttrbConfig
}

// LookupBinding 按 IMSI+DNN 查统一业务绑定（O(1)）。
func (s *Snapshot) LookupBinding(imsi, dnn string) (webTypes.BusinessBinding, bool) {
	b, ok := s.Bindings[webTypes.BindingKey{IMSI: imsi, DNN: dnn}]
	return b, ok
}

// LookupVxlan 按 VNI 命中 VXLAN 范围配置。范围配置不展开，
// 用二分查找定位候选区间，再校验 vniBegin <= vni <= vniEnd。
func (s *Snapshot) LookupVxlan(vni uint32) (models.VxlanConfig, bool) {
	i := sort.Search(len(s.VxlanRanges), func(i int) bool {
		return s.VxlanRanges[i].VNIBegin > vni
	})
	if i == 0 {
		return models.VxlanConfig{}, false
	}
	cand := s.VxlanRanges[i-1]
	if vni >= cand.VNIBegin && vni <= cand.VNIEnd {
		return cand, true
	}
	return models.VxlanConfig{}, false
}

// LookupFttrb 按 FTTRB ID 查配置（O(1)）。
func (s *Snapshot) LookupFttrb(fttrbID string) (models.FttrbConfig, bool) {
	c, ok := s.Fttrb[fttrbID]
	return c, ok
}

// ConfiguredDNNs 返回当前静态绑定中出现过的 DNN 集合。
func (s *Snapshot) ConfiguredDNNs() map[string]bool {
	out := make(map[string]bool)
	for k := range s.Bindings {
		out[k.DNN] = true
	}
	return out
}

// Build 从四张静态表构建新 Snapshot。调用方保证入参在事务提交后读取。
func Build(version uint64, vxlan []models.VxlanConfig, vxlanBindings []models.VxlanImsiBinding,
	fttrb []models.FttrbConfig, fttrbBindings []models.FttrbImsiBinding) *Snapshot {
	s := &Snapshot{
		Version:  version,
		Bindings: make(map[webTypes.BindingKey]webTypes.BusinessBinding, len(vxlanBindings)+len(fttrbBindings)),
		Fttrb:    make(map[string]models.FttrbConfig, len(fttrb)),
	}
	for _, b := range vxlanBindings {
		s.Bindings[webTypes.BindingKey{IMSI: b.IMSI, DNN: b.DNN}] = webTypes.BusinessBinding{
			BusinessType: webTypes.BusinessToH, VNI: b.VNI,
		}
	}
	for _, b := range fttrbBindings {
		s.Bindings[webTypes.BindingKey{IMSI: b.IMSI, DNN: b.DNN}] = webTypes.BusinessBinding{
			BusinessType: webTypes.BusinessToB, FttrbID: b.FttrbID,
		}
	}
	s.VxlanRanges = append(s.VxlanRanges, vxlan...)
	sort.Slice(s.VxlanRanges, func(i, j int) bool {
		return s.VxlanRanges[i].VNIBegin < s.VxlanRanges[j].VNIBegin
	})
	for _, c := range fttrb {
		s.Fttrb[c.FttrbID] = c
	}
	return s
}

// Holder 以 atomic.Value 持有当前 Snapshot，替换与读取均为原子操作。
type Holder struct {
	v atomic.Value // *Snapshot
}

// NewHolder 创建持有初始 Snapshot 的 Holder。
func NewHolder(s *Snapshot) *Holder {
	h := &Holder{}
	h.Store(s)
	return h
}

// Load 返回当前 Snapshot（调用方不得修改其内部 map）。
func (h *Holder) Load() *Snapshot {
	if v := h.v.Load(); v != nil {
		return v.(*Snapshot)
	}
	return &Snapshot{Bindings: map[webTypes.BindingKey]webTypes.BusinessBinding{}, Fttrb: map[string]models.FttrbConfig{}}
}

// Store 原子替换 Snapshot。
func (h *Holder) Store(s *Snapshot) { h.v.Store(s) }
