// Package controllers 中的 OAM 部分：CP 接收 OAM 静态配置的 HTTP 接口，并负责
// “事务落库 → 构建新 Snapshot → 原子替换 → 发布配置变更事件”
// 的完整链路（说明书 4 / 10.13 / 10.24）。
//
// 接口引用 OAM 既有设计：OAM 统一 POST 到 /cp/v1/common/call，
// 请求体包含 uuid、command、reqArgs。本包实现服务端一侧；
// 若 OAM 侧的响应信封另有冻结格式，只需调整 respond()。
package controllers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"

	"natcp/main/conf"
	"natcp/main/handler"
	"natcp/main/models"
	"natcp/main/webTypes"
)

// OAMServer 是 OAM 配置服务。路由注册在 server 包。
type OAMServer struct {
	repo   *models.Repository
	snap   *conf.Holder
	engine *handler.Engine
	log    *logrus.Entry

	reloadRunning atomic.Bool // 后台快照 reload 是否在跑，保证同时只有一个
}

// NewOAMServer 创建 OAM 服务。
func NewOAMServer(repo *models.Repository, snap *conf.Holder, e *handler.Engine) *OAMServer {
	return &OAMServer{repo: repo, snap: snap, engine: e, log: logrus.WithField("module", "oam")}
}

// oamRequest 是 OAM 统一调用的请求体。
type oamRequest struct {
	UUID    string          `json:"uuid"`
	Command string          `json:"command"`
	ReqArgs json.RawMessage `json:"reqArgs"`
}

func (s *OAMServer) respond(c *gin.Context, uuid string, code int, msg string, data any) {
	c.JSON(http.StatusOK, gin.H{"uuid": uuid, "code": code, "message": msg, "data": data})
}

func (s *OAMServer) HandleCall(c *gin.Context) {
	var req oamRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		s.respond(c, "", 400, "bad json: "+err.Error(), nil)
		return
	}
	oldSnap := s.snap.Load()
	ev, data, err := s.dispatch(c, req)
	if err != nil {
		s.log.WithError(err).WithField("command", req.Command).Warn("oam command failed")
		s.respond(c, req.UUID, 400, err.Error(), nil)
		return
	}
	// 只有提交成功才允许刷新内存（说明书 10.13 步骤 5）。
	version := s.repo.NextVersion()
	newSnap, err := s.buildSnapshot(version)
	if err != nil {
		// 瞬时错误（如短暂锁冲突）重试两次再放弃。
		for i := 0; i < 2 && err != nil; i++ {
			time.Sleep(100 * time.Millisecond)
			newSnap, err = s.buildSnapshot(version)
		}
	}
	if err != nil {
		s.log.WithError(err).Error("rebuild snapshot failed after commit")
		// 说明书 V1.1 3.9.13：提交成功但快照重建失败，应告警并触发全量 reload。
		s.startReloadLoop()
		s.respond(c, req.UUID, 500, "snapshot rebuild failed: "+err.Error(), nil)
		return
	}
	s.snap.Store(newSnap)
	if ev != nil {
		ev.Version = version
		ev.NewDNNs = newlyAppearedDNNs(oldSnap, newSnap)
		select {
		case s.engine.Events() <- *ev:
		default:
			// 通道满时不再丢弃事件：起 goroutine 等位投递，保证配置提交后
			// 事件必达（说明书 V1.1 3.4.2）；Go 按 FIFO 唤醒阻塞的发送方，顺序不受影响。
			s.log.Warn("config event channel full, delivering asynchronously")
			evCopy := *ev
			go func() { s.engine.Events() <- evCopy }()
		}
	}
	s.respond(c, req.UUID, 0, "OK", data)
}

// buildSnapshot 从四张静态表读取并构建新 Snapshot。
func (s *OAMServer) buildSnapshot(version uint64) (*conf.Snapshot, error) {
	vxlan, err := s.repo.ListVxlanConfigs()
	if err != nil {
		return nil, err
	}
	vb, err := s.repo.ListVxlanBindings()
	if err != nil {
		return nil, err
	}
	fttrb, err := s.repo.ListFttrbConfigs()
	if err != nil {
		return nil, err
	}
	fb, err := s.repo.ListFttrbBindings()
	if err != nil {
		return nil, err
	}
	return conf.Build(version, vxlan, vb, fttrb, fb), nil
}

// startReloadLoop 在快照重建失败后后台重试全量 reload（说明书 V1.1 3.9.13）：
// 每 5 秒从 SQLite 全量重读并重建快照，成功后原子替换并发布一次 ADD 事件，
// 让各 DNN 重新跑定向全量补偿。同一时间最多一个 reload 循环在跑。
func (s *OAMServer) startReloadLoop() {
	if !s.reloadRunning.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer s.reloadRunning.Store(false)
		for {
			time.Sleep(5 * time.Second)
			version := s.repo.NextVersion()
			snap, err := s.buildSnapshot(version)
			if err != nil {
				s.log.WithError(err).Error("snapshot reload retry failed")
				continue
			}
			s.snap.Store(snap)
			ev := &webTypes.ConfigChangedEvent{Op: "ADD", Version: version}
			for d := range snap.ConfiguredDNNs() {
				ev.DNNs = append(ev.DNNs, d)
			}
			select {
			case s.engine.Events() <- *ev:
			default:
				evCopy := *ev
				go func() { s.engine.Events() <- evCopy }()
			}
			s.log.Info("snapshot reloaded after failure")
			return
		}
	}()
}

// newlyAppearedDNNs 返回新快照中有、旧快照中没有的 DNN，
// 即“从无本机绑定变为存在本机绑定”的 DNN（说明书 5.10.2）。
func newlyAppearedDNNs(oldSnap, newSnap *conf.Snapshot) []string {
	oldDNNs := oldSnap.ConfiguredDNNs()
	var out []string
	for d := range newSnap.ConfiguredDNNs() {
		if !oldDNNs[d] {
			out = append(out, d)
		}
	}
	return out
}

// dispatch 执行一条 OAM 命令的 SQLite 写入，返回待发布的配置事件。
// data 为查询类命令的返回内容。
func (s *OAMServer) dispatch(c *gin.Context, req oamRequest) (*webTypes.ConfigChangedEvent, any, error) {
	args := req.ReqArgs
	switch req.Command {
	// ---- VXLAN 范围配置 ----
	// add 与 mod 分开：add 只新增（id 已存在报错），mod 只修改且 VNI 范围不可变。
	case "add_vxlan":
		var a vxlanArgs
		if err := json.Unmarshal(args, &a); err != nil {
			return nil, nil, err
		}
		cfg := a.toModel()
		if err := s.repo.AddVxlan(cfg); err != nil {
			return nil, nil, err
		}
		return s.eventForVxlanRange("ADD", cfg.VNIBegin, cfg.VNIEnd), nil, nil
	case "mod_vxlan":
		var a vxlanArgs
		if err := json.Unmarshal(args, &a); err != nil {
			return nil, nil, err
		}
		cfg := a.toModel()
		if err := s.repo.ModifyVxlan(cfg); err != nil {
			return nil, nil, err
		}
		// 范围已由 ModifyVxlan 保证与库中一致，可直接用下发值定位受影响绑定。
		return s.eventForVxlanRange("MODIFY", cfg.VNIBegin, cfg.VNIEnd), nil, nil
	case "del_vxlan":
		var a struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(args, &a); err != nil {
			return nil, nil, err
		}
		// 范围内仍有 ToH 绑定时 DeleteVxlan 拒绝删除并给出提示；
		// 成功时返回被删配置，用其范围定位受影响会话。
		cfg, err := s.repo.DeleteVxlan(a.ID)
		if err != nil {
			return nil, nil, err
		}
		return s.eventForVxlanRange("DELETE", cfg.VNIBegin, cfg.VNIEnd), nil, nil
	case "list_vxlan":
		return nil, mustList(s.repo.ListVxlanConfigs()), nil

	// ---- ToH IMSI 绑定 ----
	case "add_vxlan_imsi", "mod_vxlan_imsi":
		var a vxlanBindingArgs
		if err := json.Unmarshal(args, &a); err != nil {
			return nil, nil, err
		}
		if err := s.repo.UpsertVxlanBinding(a.toModel()); err != nil {
			return nil, nil, err
		}
		op := "ADD"
		if req.Command == "mod_vxlan_imsi" {
			op = "MODIFY"
		}
		return &webTypes.ConfigChangedEvent{Op: op,
			Keys: []webTypes.BindingKey{{IMSI: a.IMSI, DNN: a.DNN}}, DNNs: []string{a.DNN}}, nil, nil
	case "batch_add_vxlan_imsi":
		list, err := parseList[models.VxlanImsiBinding](args, "bindings", func(a vxlanBindingArgs) models.VxlanImsiBinding {
			return a.toModel()
		})
		if err != nil {
			return nil, nil, err
		}
		if err := s.repo.BatchAddVxlanBindings(list); err != nil {
			return nil, nil, err
		}
		return eventForBindings("BATCH", listKeysVxlan(list)), nil, nil
	case "del_vxlan_imsi":
		var a vxlanBindingArgs
		if err := json.Unmarshal(args, &a); err != nil {
			return nil, nil, err
		}
		if err := s.repo.DeleteVxlanBinding(a.IMSI, a.DNN); err != nil {
			return nil, nil, err
		}
		return &webTypes.ConfigChangedEvent{Op: "DELETE",
			Keys: []webTypes.BindingKey{{IMSI: a.IMSI, DNN: a.DNN}}, DNNs: []string{a.DNN}}, nil, nil
	case "list_vxlan_imsi":
		return nil, mustList(s.repo.ListVxlanBindings()), nil

	// ---- FTTR-B 配置 ----
	case "add_fttrb", "mod_fttrb":
		var a fttrbArgs
		if err := json.Unmarshal(args, &a); err != nil {
			return nil, nil, err
		}
		cfg := a.toModel()
		if err := s.repo.UpsertFttrb(cfg); err != nil {
			return nil, nil, err
		}
		op := "ADD"
		if req.Command == "mod_fttrb" {
			op = "MODIFY"
		}
		return s.eventForFttrb(op, cfg.FttrbID), nil, nil
	case "batch_add_fttrb":
		list, err := parseList[models.FttrbConfig](args, "configs", func(a fttrbArgs) models.FttrbConfig {
			return a.toModel()
		})
		if err != nil {
			return nil, nil, err
		}
		if err := s.repo.BatchAddFttrbConfigs(list); err != nil {
			return nil, nil, err
		}
		ids := make([]string, 0, len(list))
		for _, c := range list {
			ids = append(ids, c.FttrbID)
		}
		return s.eventForFttrbIDs("BATCH", ids), nil, nil
	case "del_fttrb":
		var a struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(args, &a); err != nil {
			return nil, nil, err
		}
		var fttrbID string
		for _, fc := range mustList(s.repo.ListFttrbConfigs()) {
			if fc.ID == a.ID {
				fttrbID = fc.FttrbID
			}
		}
		if err := s.repo.DeleteFttrb(a.ID); err != nil {
			return nil, nil, err
		}
		return s.eventForFttrb("DELETE", fttrbID), nil, nil
	case "list_fttrb":
		return nil, mustList(s.repo.ListFttrbConfigs()), nil

	// ---- ToB IMSI 绑定 ----
	case "add_fttrb_imsi", "mod_fttrb_imsi":
		var a fttrbBindingArgs
		if err := json.Unmarshal(args, &a); err != nil {
			return nil, nil, err
		}
		if err := s.repo.UpsertFttrbBinding(a.toModel()); err != nil {
			return nil, nil, err
		}
		op := "ADD"
		if req.Command == "mod_fttrb_imsi" {
			op = "MODIFY"
		}
		return &webTypes.ConfigChangedEvent{Op: op,
			Keys: []webTypes.BindingKey{{IMSI: a.IMSI, DNN: a.DNN}}, DNNs: []string{a.DNN}}, nil, nil
	case "batch_add_fttrb_imsi":
		list, err := parseList[models.FttrbImsiBinding](args, "bindings", func(a fttrbBindingArgs) models.FttrbImsiBinding {
			return a.toModel()
		})
		if err != nil {
			return nil, nil, err
		}
		if err := s.repo.BatchAddFttrbBindings(list); err != nil {
			return nil, nil, err
		}
		return eventForBindings("BATCH", listKeysFttrb(list)), nil, nil
	case "del_fttrb_imsi":
		var a fttrbBindingArgs
		if err := json.Unmarshal(args, &a); err != nil {
			return nil, nil, err
		}
		if err := s.repo.DeleteFttrbBinding(a.IMSI, a.DNN); err != nil {
			return nil, nil, err
		}
		return &webTypes.ConfigChangedEvent{Op: "DELETE",
			Keys: []webTypes.BindingKey{{IMSI: a.IMSI, DNN: a.DNN}}, DNNs: []string{a.DNN}}, nil, nil
	case "list_fttrb_imsi":
		return nil, mustList(s.repo.ListFttrbBindings()), nil
	}
	return nil, nil, fmt.Errorf("unknown command %q", req.Command)
}

// eventForVxlanRange 找出 VNI 落在该范围内的全部绑定，作为受影响键。
func (s *OAMServer) eventForVxlanRange(op string, begin, end uint32) *webTypes.ConfigChangedEvent {
	ev := &webTypes.ConfigChangedEvent{Op: op}
	seen := map[string]bool{}
	for _, b := range mustList(s.repo.ListVxlanBindings()) {
		if b.VNI >= begin && b.VNI <= end {
			ev.Keys = append(ev.Keys, webTypes.BindingKey{IMSI: b.IMSI, DNN: b.DNN})
			if !seen[b.DNN] {
				seen[b.DNN] = true
				ev.DNNs = append(ev.DNNs, b.DNN)
			}
		}
	}
	return ev
}

// eventForFttrb 找出引用该 FTTRB ID 的全部绑定，作为受影响键。
func (s *OAMServer) eventForFttrb(op, fttrbID string) *webTypes.ConfigChangedEvent {
	return s.eventForFttrbIDs(op, []string{fttrbID})
}

// eventForFttrbIDs 一次扫描绑定表，取出引用这批 FTTRB ID 的全部绑定。
// 批量命令共用一次 ListFttrbBindings，避免每条配置扫一遍库。
func (s *OAMServer) eventForFttrbIDs(op string, fttrbIDs []string) *webTypes.ConfigChangedEvent {
	want := make(map[string]bool, len(fttrbIDs))
	for _, id := range fttrbIDs {
		want[id] = true
	}
	ev := &webTypes.ConfigChangedEvent{Op: op}
	seen := map[string]bool{}
	for _, b := range mustList(s.repo.ListFttrbBindings()) {
		if want[b.FttrbID] {
			ev.Keys = append(ev.Keys, webTypes.BindingKey{IMSI: b.IMSI, DNN: b.DNN})
			if !seen[b.DNN] {
				seen[b.DNN] = true
				ev.DNNs = append(ev.DNNs, b.DNN)
			}
		}
	}
	return ev
}

func eventForBindings(op string, keys []webTypes.BindingKey) *webTypes.ConfigChangedEvent {
	ev := &webTypes.ConfigChangedEvent{Op: op, Keys: keys}
	seen := map[string]bool{}
	for _, k := range keys {
		if !seen[k.DNN] {
			seen[k.DNN] = true
			ev.DNNs = append(ev.DNNs, k.DNN)
		}
	}
	return ev
}

func listKeysVxlan(list []models.VxlanImsiBinding) []webTypes.BindingKey {
	var out []webTypes.BindingKey
	for _, b := range list {
		out = append(out, webTypes.BindingKey{IMSI: b.IMSI, DNN: b.DNN})
	}
	return out
}

func listKeysFttrb(list []models.FttrbImsiBinding) []webTypes.BindingKey {
	var out []webTypes.BindingKey
	for _, b := range list {
		out = append(out, webTypes.BindingKey{IMSI: b.IMSI, DNN: b.DNN})
	}
	return out
}

// parseList 解析批量参数：既接受 {"<field>":[...]}，也接受裸数组 [...]。
// field 是包装字段名，绑定类是 "bindings"，配置类是 "configs"。
func parseList[T any, A any](args json.RawMessage, field string, conv func(A) T) ([]T, error) {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(args, &probe); err == nil {
		if raw, ok := probe[field]; ok {
			var arr []A
			if err := json.Unmarshal(raw, &arr); err != nil {
				return nil, fmt.Errorf("invalid %q array: %w", field, err)
			}
			return convertList(arr, conv), nil
		}
	}
	var arr []A
	if err := json.Unmarshal(args, &arr); err != nil {
		return nil, fmt.Errorf("invalid batch list (expect an array or {\"%s\":[...]}): %w", field, err)
	}
	return convertList(arr, conv), nil
}

func convertList[T any, A any](arr []A, conv func(A) T) []T {
	out := make([]T, 0, len(arr))
	for _, a := range arr {
		out = append(out, conv(a))
	}
	return out
}

func mustList[T any](list []T, err error) []T {
	if err != nil {
		return nil
	}
	return list
}

// ---------------------------------------------------------------------------
// reqArgs 参数结构（字段名与说明书第 4 章一致）
// ---------------------------------------------------------------------------

type vxlanArgs struct {
	ID               string `json:"id"`
	VNIBegin         uint32 `json:"vniBegin"`
	VNIEnd           uint32 `json:"vniEnd"`
	LocalVtepIP      string `json:"localVtepIp"`
	RemoteVtepIP     string `json:"remoteVtepIp"`
	LocalOverlayMAC  string `json:"localOverlayMac"`
	RemoteOverlayMAC string `json:"remoteOverlayMac"`
	SrcNatAddr       string `json:"srcNatAddr"`
}

func (a vxlanArgs) toModel() models.VxlanConfig {
	return models.VxlanConfig{
		ID: a.ID, VNIBegin: a.VNIBegin, VNIEnd: a.VNIEnd,
		LocalVtepIP: a.LocalVtepIP, RemoteVtepIP: a.RemoteVtepIP,
		LocalOverlayMAC: a.LocalOverlayMAC, RemoteOverlayMAC: a.RemoteOverlayMAC,
		SrcNatAddr: a.SrcNatAddr,
	}
}

type vxlanBindingArgs struct {
	ID   string `json:"id"`
	IMSI string `json:"imsi"`
	DNN  string `json:"dnn"`
	VNI  uint32 `json:"vni"`
}

func (a vxlanBindingArgs) toModel() models.VxlanImsiBinding {
	return models.VxlanImsiBinding{ID: a.ID, IMSI: a.IMSI, DNN: a.DNN, VNI: a.VNI}
}

type fttrbArgs struct {
	ID         string `json:"id"`
	FttrbID    string `json:"fttrbId"`
	SrcNatAddr string `json:"srcNatAddr"`
}

func (a fttrbArgs) toModel() models.FttrbConfig {
	return models.FttrbConfig{ID: a.ID, FttrbID: a.FttrbID, SrcNatAddr: a.SrcNatAddr}
}

type fttrbBindingArgs struct {
	ID      string `json:"id"`
	IMSI    string `json:"imsi"`
	DNN     string `json:"dnn"`
	FttrbID string `json:"fttrbId"`
}

func (a fttrbBindingArgs) toModel() models.FttrbImsiBinding {
	return models.FttrbImsiBinding{ID: a.ID, IMSI: a.IMSI, DNN: a.DNN, FttrbID: a.FttrbID}
}
