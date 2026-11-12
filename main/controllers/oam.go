// Package controllers 中的 OAM 部分：CP 接收 OAM 静态配置的 HTTP 接口，并负责
// “事务落库 → 构建新 Snapshot → 原子替换 → 发布配置变更事件”
// 的完整链路（说明书 4 / 10.13 / 10.24）。
//
// 接口引用 OAM 既有设计：OAM 统一 POST 到 /cp/v1/common/call，
// 请求体包含 uuid、command、reqArgs。本包实现服务端一侧；
// 若 OAM 侧的响应信封另有冻结格式，只需调整 respond()。
package controllers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
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

// ---------------------------------------------------------------------------
// 响应信封
//
// 所有响应都是三层同构：{uuid, ret, body}，body 内再带一个 ret，
// 与外层同值。区别只在 body 的形状：
//
//	成功（查询） {"uuid":"q1","ret":0,
//	             "body":{"ret":0,"total":3,"records":[{"id":1,...}]}}
//	成功（写入） {"uuid":"a1","ret":0,"body":{"ret":0,"msg":"新增成功"}}
//	失败（任意） {"uuid":"a2","ret":-9,
//	             "body":{"ret":-9,"msg":"vxlan v1 already exists, ..."}}
//
// 关键约定：
//   - ret：0 成功，-9 失败（与 OAM 脚本约定一致）；外层与 body 内同值。
//   - 查询成功：body 为 {ret, total, records}，total/records 恒定存在，
//     空结果给 records: []，不让键消失。
//   - 写命令成功（add/mod/del/batch）：body 为 {ret, msg}，msg 是中文动作
//     描述（"新增成功"/"删除成功"）。
//   - 失败：不分命令类型，body 统一为 {ret:-9, msg:<具体原因>}，
//     不带 total/records——失败时业务数据没有意义。
//   - uuid 原样回显，供 OAM 做请求-响应匹配。
// ---------------------------------------------------------------------------

// retOK / retFail 分别是成功与失败的返回码，外层 body 内外同值。
const (
	retOK   = 0
	retFail = -9
)

// respBody 是 body 的两种形状之一，由命令类型决定：
//   - listBody：查询成功，{ret, total, records}
//   - msgBody ：增删改成功与所有失败，{ret, msg}
//
// 两者都整体放进 oamResp.Body，形状由调用方（respondList / respondOK /
// respondErr）保证，不需要各自的信封类型。
//
// listBody 字段都不带 omitempty：必须能稳定拿到 total 与 records，
// 空结果给 records: [] 而不是让键消失。
type listBody struct {
	Ret     int `json:"ret"`
	Total   int `json:"total"`
	Records any `json:"records"`
}

// msgBody 是写命令成功与所有失败的业务体。成功时 msg 是中文动作描述，
// 失败时是具体原因。
type msgBody struct {
	Ret int    `json:"ret"`
	Msg string `json:"msg"`
}

// oamResp 是所有 OAM 响应的统一信封：{uuid, ret, body}。
// Body 用 any 承载，因为 body 只有 listBody / msgBody 两种形状，
// 由三个 respond* 函数各自填，没必要为两种形状各定义一个字段完全相同的信封。
type oamResp struct {
	UUID string `json:"uuid"`
	Ret  int    `json:"ret"`
	Body any    `json:"body"`
}

// operationVerb 按命令名给出中文动作，用于拼 msg（"查询成功"/"新增成功"）。
func operationVerb(cmd string) string {
	switch {
	case strings.HasPrefix(cmd, "batch_add_"):
		return "批量新增"
	case strings.HasPrefix(cmd, "batch_del_"):
		return "批量删除"
	case strings.HasPrefix(cmd, "list_"):
		return "查询"
	case strings.HasPrefix(cmd, "add_"):
		return "新增"
	case strings.HasPrefix(cmd, "mod_"):
		return "修改"
	case strings.HasPrefix(cmd, "del_"):
		return "删除"
	default:
		return "操作"
	}
}

// respondErr 输出失败响应：外层 ret=-9，body 内同样是 {ret:-9, msg}，
// 具体原因进 msg。查询失败与写命令失败共用这一种形状。
func (s *OAMServer) respondErr(c *gin.Context, uuid, cmd string, err error) {
	c.PureJSON(http.StatusOK, oamResp{
		UUID: uuid,
		Ret:  retFail,
		Body: msgBody{Ret: retFail, Msg: err.Error()},
	})
}

// respondOK 输出写命令的成功响应：body 为 {ret:0, msg:"新增成功"}。
func (s *OAMServer) respondOK(c *gin.Context, uuid, cmd string) {
	c.PureJSON(http.StatusOK, oamResp{
		UUID: uuid,
		Ret:  retOK,
		Body: msgBody{Ret: retOK, Msg: operationVerb(cmd) + "成功"},
	})
}

// pageResult 是 list_* 命令的返回体：items 进 records，total 为其总数。
// records 始终是数组（无数据时为 []），客户端不需要再判 null。
type pageResult struct {
	Items any
	Total int
}

// parsePage 解析 list_* 的 reqArgs 分页参数，支持三种写法：
//
//	不传 / null / {} / []          → 全量返回
//	[0, 10]                        → 下标 0 起取 10 条（OAM 侧约定写法）
//	{"offset":0,"limit":10}        → 同上，limit 也接受别名 count
//
// limit 为 0 表示不限条数（等价于不传）。
func parsePage(args json.RawMessage) (models.Page, error) {
	var p models.Page
	body := bytes.TrimSpace(args)
	if len(body) == 0 {
		return p, nil
	}
	switch body[0] {
	case 'n':
		if !bytes.Equal(body, []byte("null")) {
			return p, fmt.Errorf("invalid list args %s: expect [offset, limit]", body)
		}
		return p, nil
	case '[':
		// 空数组 [] 交给下面的 Unmarshal，长度 0 时即全量。
		var arr []int
		if err := json.Unmarshal(body, &arr); err != nil {
			return p, fmt.Errorf("invalid list args %s: expect [offset, limit]", body)
		}
		if len(arr) > 2 {
			return p, fmt.Errorf("invalid list args %s: expect [offset, limit], got %d elements", body, len(arr))
		}
		if len(arr) > 0 {
			p.Offset = arr[0]
		}
		if len(arr) > 1 {
			p.Limit = arr[1]
		}
	case '{':
		var o struct {
			Offset *int `json:"offset"`
			Limit  *int `json:"limit"`
			Count  *int `json:"count"`
		}
		if err := json.Unmarshal(body, &o); err != nil {
			return p, fmt.Errorf("invalid list args %s: expect {\"offset\":n,\"limit\":m}", body)
		}
		if o.Offset != nil {
			p.Offset = *o.Offset
		}
		if o.Limit != nil {
			p.Limit = *o.Limit
		}
		if o.Count != nil {
			p.Limit = *o.Count
		}
	default:
		return p, fmt.Errorf("invalid list args %s: expect [offset, limit]", body)
	}
	if err := p.Validate(); err != nil {
		return p, err
	}
	return p, nil
}

// parseIDArg 解析 del_* 的单个 id，两种写法都接受：
//
//	reqArgs = [11]          数组
//	reqArgs = {"id": 11}    对象
//
// id 是 OAM 主键，一定是数字，直接按 uint32 解析，不做字符串兼容。
func parseIDArg(args json.RawMessage, cmd string) (uint32, error) {
	body := bytes.TrimSpace(args)
	if len(body) == 0 || bytes.Equal(body, []byte("null")) {
		return 0, fmt.Errorf(`%s: reqArgs is required, expect [id] or {"id":...}`, cmd)
	}

	switch body[0] {
	case '[':
		var arr []uint32
		if err := json.Unmarshal(body, &arr); err != nil {
			return 0, fmt.Errorf(`%s: reqArgs must be an array with one uint32 id, got %s`, cmd, body)
		}
		if len(arr) != 1 {
			return 0, fmt.Errorf("%s: reqArgs must contain exactly 1 element (the id), got %d", cmd, len(arr))
		}
		if arr[0] == 0 {
			return 0, fmt.Errorf("%s: id must be greater than 0", cmd)
		}
		return arr[0], nil

	case '{':
		// 只取 id，多余字段忽略（OAM 可能顺带带上其它上下文）。
		var obj struct {
			ID uint32 `json:"id"`
		}
		if err := json.Unmarshal(body, &obj); err != nil {
			return 0, fmt.Errorf(`%s: reqArgs.id must be a uint32 number (0..4294967295), got %s`, cmd, body)
		}
		if obj.ID == 0 {
			return 0, fmt.Errorf(`%s: reqArgs.id is required, got %s`, cmd, body)
		}
		return obj.ID, nil

	default:
		return 0, fmt.Errorf(`%s: reqArgs must be [id] or {"id":...}, got %s`, cmd, body)
	}
}

// flexStr 是业务标识（imsi / fttrbId）的入参类型。
//
// 这两个字段 OAM 可能发字符串也可能发数字（15 位 imsi 用 JS 拼 JSON 极易退化成
// number），统一收成字符串后再入模型。主键 id 不走这里——它是 uint32。
type flexStr string

func (f *flexStr) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || bytes.Equal(b, []byte("null")) {
		*f = ""
		return nil
	}
	if b[0] == '"' {
		var v string
		if err := json.Unmarshal(b, &v); err != nil {
			return fmt.Errorf("expect string or number, got %s", b)
		}
		*f = flexStr(v)
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(b, &n); err != nil {
		return fmt.Errorf("expect string or number, got %s", b)
	}
	*f = flexStr(n.String())
	return nil
}

// paged 是 list_* 分支的公共实现：解析分页参数 → 查询 → 组装 pageResult。
func paged[T any](args json.RawMessage, fn func(models.Page) ([]T, int, error)) (any, error) {
	p, err := parsePage(args)
	if err != nil {
		return nil, err
	}
	items, total, err := fn(p)
	if err != nil {
		return nil, err
	}
	// nil slice 直接序列化会变成 null；records 语义是数组，统一成 []。
	if items == nil {
		items = make([]T, 0)
	}
	return pageResult{Items: items, Total: total}, nil
}

func (s *OAMServer) HandleCall(c *gin.Context) {
	var req oamRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		s.respondErr(c, "", req.Command, fmt.Errorf("bad json: %w", err))
		return
	}
	oldSnap := s.snap.Load()
	ev, data, err := s.dispatch(c, req)
	if err != nil {
		s.log.WithError(err).WithField("command", req.Command).Warn("oam command failed")
		s.respondErr(c, req.UUID, req.Command, err)
		return
	}
	// 查询类命令（list_*）不改库：不递增版本、不重建快照，直接回数据。
	if ev == nil {
		s.respondList(c, req.UUID, req.Command, data)
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
		s.respondErr(c, req.UUID, req.Command, fmt.Errorf("snapshot rebuild failed: %w", err))
		return
	}
	s.snap.Store(newSnap)
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
	s.respondOK(c, req.UUID, req.Command)
}

// respondList 输出 list_* 的成功响应：{uuid, ret, body:{ret,total,records}}。
func (s *OAMServer) respondList(c *gin.Context, uuid, cmd string, data any) {
	pr, ok := data.(pageResult)
	if !ok {
		// 理论上不会发生：list_* 分支都返回 pageResult。
		s.respondErr(c, uuid, cmd, fmt.Errorf("internal error: list command returned unexpected payload"))
		return
	}
	c.PureJSON(http.StatusOK, oamResp{
		UUID: uuid,
		Ret:  retOK,
		Body: listBody{Ret: retOK, Total: pr.Total, Records: pr.Items},
	})
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
		// OAM 的 mod 报文不带 vniBegin/vniEnd，ModifyVxlan 会用库中范围补齐后返回，
		// 事件影响面必须按补齐后的范围算，否则会漏掉该范围内的绑定。
		eff, err := s.repo.ModifyVxlan(cfg)
		if err != nil {
			return nil, nil, err
		}
		return s.eventForVxlanRange("MODIFY", eff.VNIBegin, eff.VNIEnd), nil, nil
	case "del_vxlan":
		id, err := parseIDArg(args, "del_vxlan")
		if err != nil {
			return nil, nil, err
		}
		// 范围内仍有 ToH 绑定时 DeleteVxlan 拒绝删除并给出提示；
		// 成功时返回被删配置，用其范围定位受影响会话。
		cfg, err := s.repo.DeleteVxlan(id)
		if err != nil {
			return nil, nil, err
		}
		return s.eventForVxlanRange("DELETE", cfg.VNIBegin, cfg.VNIEnd), nil, nil
	case "list_vxlan":
		// 分页写法：reqArgs = [offset, limit]
		data, err := paged(args, s.repo.ListVxlanConfigsPage)
		return nil, data, err

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
			Keys: []webTypes.BindingKey{{IMSI: string(a.IMSI), DNN: a.DNN}}, DNNs: []string{a.DNN}}, nil, nil
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
		id, err := parseIDArg(args, "del_vxlan_imsi")
		if err != nil {
			return nil, nil, err
		}
		b, err := s.repo.DeleteVxlanBindingByID(id)
		if err != nil {
			return nil, nil, err
		}
		return &webTypes.ConfigChangedEvent{Op: "DELETE",
			Keys: []webTypes.BindingKey{{IMSI: b.IMSI, DNN: b.DNN}}, DNNs: []string{b.DNN}}, nil, nil
	case "list_vxlan_imsi":
		data, err := paged(args, s.repo.ListVxlanBindingsPage)
		return nil, data, err

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
		id, err := parseIDArg(args, "del_fttrb")
		if err != nil {
			return nil, nil, err
		}
		var fttrbID string
		for _, fc := range mustList(s.repo.ListFttrbConfigs()) {
			if fc.ID == id {
				fttrbID = fc.FttrbID
			}
		}
		if err := s.repo.DeleteFttrb(id); err != nil {
			return nil, nil, err
		}
		return s.eventForFttrb("DELETE", fttrbID), nil, nil
	case "list_fttrb":
		data, err := paged(args, s.repo.ListFttrbConfigsPage)
		return nil, data, err

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
			Keys: []webTypes.BindingKey{{IMSI: string(a.IMSI), DNN: a.DNN}}, DNNs: []string{a.DNN}}, nil, nil
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
		id, err := parseIDArg(args, "del_fttrb_imsi")
		if err != nil {
			return nil, nil, err
		}
		b, err := s.repo.DeleteFttrbBindingByID(id)
		if err != nil {
			return nil, nil, err
		}
		return &webTypes.ConfigChangedEvent{Op: "DELETE",
			Keys: []webTypes.BindingKey{{IMSI: b.IMSI, DNN: b.DNN}}, DNNs: []string{b.DNN}}, nil, nil
	case "list_fttrb_imsi":
		data, err := paged(args, s.repo.ListFttrbBindingsPage)
		return nil, data, err
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
	ID               uint32 `json:"id"`
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
	ID   uint32  `json:"id"`
	IMSI flexStr `json:"imsi"`
	DNN  string  `json:"dnn"`
	VNI  uint32  `json:"vni"`
}

func (a vxlanBindingArgs) toModel() models.VxlanImsiBinding {
	return models.VxlanImsiBinding{
		ID: a.ID, IMSI: string(a.IMSI), DNN: a.DNN, VNI: a.VNI,
	}
}

type fttrbArgs struct {
	ID         uint32  `json:"id"`
	FttrbID    flexStr `json:"fttrbId"`
	SrcNatAddr string  `json:"srcNatAddr"`
}

func (a fttrbArgs) toModel() models.FttrbConfig {
	return models.FttrbConfig{
		ID: a.ID, FttrbID: string(a.FttrbID), SrcNatAddr: a.SrcNatAddr,
	}
}

type fttrbBindingArgs struct {
	ID      uint32  `json:"id"`
	IMSI    flexStr `json:"imsi"`
	DNN     string  `json:"dnn"`
	FttrbID flexStr `json:"fttrbId"`
}

func (a fttrbBindingArgs) toModel() models.FttrbImsiBinding {
	return models.FttrbImsiBinding{
		ID: a.ID, IMSI: string(a.IMSI), DNN: a.DNN, FttrbID: string(a.FttrbID),
	}
}
