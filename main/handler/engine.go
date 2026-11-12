// Package handler 是 CP 的业务处理层（对应 jxtx handler）：把 UPF 动态会话匹配到 OAM 静态配置，
// 生成期望规则并驱动 UP 安装；同时实现说明书 V1.1 3.4.2 的运行期 DNN
// 配置新增事件驱动全量补偿与 3.5.8 的 RULE_SYNC 批量同步。
//
// 并发规则：
//   - 同一 BusinessType+SrcIP 的规则操作必须串行化（分段锁），
//     保证较新的 UE delete 不会被较旧的全量结果重新覆盖；
//   - Desired 内容不变时不递增版本、不重复下发（幂等）；
//   - ACK 只有仍对应当前 Desired 版本时才能更新 Installed 状态。
package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sirupsen/logrus"

	"natcp/main/conf"
	"natcp/main/webTypes"
	"natcp/main/up"
	"natcp/main/upf"
	"natcp/main/utils"
)

// Engine 持有全部运行态。Desired/Installed/Session 只放内存，
// 重启后由 UPF 全量会话 + UP 批量同步重建（说明书 10.22）。
type Engine struct {
	cfg    *conf.Config
	snap   *conf.Holder
	upf    *upf.Client
	up     *up.Client
	active bool

	mu           sync.Mutex
	sessions     map[string]*webTypes.UeSession   // sessionKey -> 会话
	sessionRules map[string]webTypes.RuleKey      // sessionKey -> 规则键
	desired      map[webTypes.RuleKey]*webTypes.ForwardRule
	installed    map[webTypes.RuleKey]*webTypes.InstalledState

	keyLocks [64]sync.Mutex // 按规则键分段，保证同一规则操作串行化

	compMu sync.Mutex
	comp   map[string]*compState // dnn -> 补偿状态

	upfMu  sync.Mutex
	upfDNN map[string]bool // UPF 侧智云随行 DNN 集合缓存

	linkDown atomic.Bool

	skipLogMu   sync.Mutex
	skipLogLast map[string]time.Time // sessionKey -> 上次记“无绑定跳过”日志的时间

	events chan webTypes.ConfigChangedEvent
	log    *logrus.Entry
}

type compState struct {
	running bool
	dirty   bool // 执行期间又有新配置到达，结束后需再跑一次
	pending bool // 链路/Token/DNN 不可用，待恢复后重试
}

// snapItem 是快照同步中的一条待下发规则及其发送时版本。
type snapItem struct {
	rule    *webTypes.ForwardRule
	version uint64
}

// New 创建业务引擎。
func New(cfg *conf.Config, snap *conf.Holder, upfCli *upf.Client, upCli *up.Client) *Engine {
	return &Engine{
		cfg:          cfg,
		snap:         snap,
		upf:          upfCli,
		up:           upCli,
		active:       cfg.Active,
		sessions:     make(map[string]*webTypes.UeSession),
		sessionRules: make(map[string]webTypes.RuleKey),
		desired:      make(map[webTypes.RuleKey]*webTypes.ForwardRule),
		installed:    make(map[webTypes.RuleKey]*webTypes.InstalledState),
		comp:         make(map[string]*compState),
		upfDNN:       make(map[string]bool),
		skipLogLast:  make(map[string]time.Time),
		events:       make(chan webTypes.ConfigChangedEvent, 128),
		log:          logrus.WithField("module", "engine"),
	}
}

// Events 返回配置变更事件通道，供 OAM 层投递。
func (e *Engine) Events() chan<- webTypes.ConfigChangedEvent { return e.events }

// SetLinkDown 标记 UPF 链路状态。链路异常不删除已有规则（说明书 5.10.6）。
func (e *Engine) SetLinkDown(down bool) { e.linkDown.Store(down) }

// IsLinkDown 返回 UPF 链路是否异常。
func (e *Engine) IsLinkDown() bool { return e.linkDown.Load() }

func (e *Engine) keyLock(k webTypes.RuleKey) *sync.Mutex {
	h := fnv.New32a()
	_, _ = h.Write([]byte(k.String()))
	return &e.keyLocks[h.Sum32()%uint32(len(e.keyLocks))]
}

// ---------------------------------------------------------------------------
// 规则生成：UPF 会话 + 静态快照 -> 期望规则
// ---------------------------------------------------------------------------

// buildRule 按 IMSI+DNN 查绑定，再按业务类型查 VXLAN 范围或 FTTRB 配置。
// 返回 (rule, matched, error)：matched=false 表示该会话没有业务绑定或
// 配置不完整，不下发 UP。
func (e *Engine) buildRule(snap *conf.Snapshot, sess *webTypes.UeSession) (*webTypes.ForwardRule, bool, error) {
	binding, ok := snap.LookupBinding(sess.IMSI, sess.DNN)
	if !ok {
		return nil, false, nil
	}
	rule := &webTypes.ForwardRule{
		IMSI: sess.IMSI, DNN: sess.DNN, SessionKey: sess.SessionKey(),
	}
	switch binding.BusinessType {
	case webTypes.BusinessToH:
		vc, ok := snap.LookupVxlan(binding.VNI)
		if !ok {
			return nil, false, fmt.Errorf("vni %d has no vxlan range config", binding.VNI)
		}
		rule.Key = webTypes.RuleKey{BusinessType: webTypes.BusinessToH, SrcIP: sess.SrcIP}
		rule.VNI = binding.VNI
		rule.LocalVtepIP = vc.LocalVtepIP
		rule.RemoteVtepIP = vc.RemoteVtepIP
		rule.LocalOverlayMAC = vc.LocalOverlayMAC
		rule.RemoteOverlayMAC = vc.RemoteOverlayMAC
		rule.SrcNatAddr = vc.SrcNatAddr
	case webTypes.BusinessToB:
		fc, ok := snap.LookupFttrb(binding.FttrbID)
		if !ok {
			return nil, false, fmt.Errorf("fttrb %s has no config", binding.FttrbID)
		}
		rule.Key = webTypes.RuleKey{BusinessType: webTypes.BusinessToB, SrcIP: sess.SrcIP}
		rule.SrcNatAddr = fc.SrcNatAddr
	default:
		return nil, false, fmt.Errorf("unknown business type %q", binding.BusinessType)
	}
	return rule, true, nil
}

// ---------------------------------------------------------------------------
// 会话处理：create / delete 增量与全量共用同一入口
// ---------------------------------------------------------------------------

// ApplySession 处理一条 UE 上线会话（POST create 或全量拉取）。
// 幂等：内容相同的重复 create 不产生新版本、不重复下发。
func (e *Engine) ApplySession(ctx context.Context, sess *webTypes.UeSession) error {
	return e.applySession(ctx, sess, true)
}

// applySession 是 ApplySession 的实现。upsert=false 时只生成期望规则、
// 不逐条向 UP 下发，供启动全量恢复收集用；规则随后由调用方以
// RULE_SYNC SNAPSHOT 批量下发（说明书 V1.1 3.9.23）。
func (e *Engine) applySession(ctx context.Context, sess *webTypes.UeSession, upsert bool) error {
	sess.SrcIP = sess.NormalizeSrcIP()
	if sess.SrcIP == "" {
		e.log.WithFields(logrus.Fields{"imsi": sess.IMSI, "dnn": sess.DNN}).
			Warn("session has no private ip, skip rule generation")
		return nil
	}
	snap := e.snap.Load()

	e.mu.Lock()
	e.sessions[sess.SessionKey()] = sess
	e.mu.Unlock()

	rule, matched, err := e.buildRule(snap, sess)
	if err != nil {
		e.log.WithError(err).WithFields(logrus.Fields{"imsi": sess.IMSI, "dnn": sess.DNN}).Warn("build rule failed")
		return nil
	}
	if !matched {
		// 未命中静态配置：只记录会话，不下发（说明书 5.10.5）。
		// 若该会话此前已有规则（配置刚被删除），则删除陈旧规则。
		e.mu.Lock()
		staleKey, hasStale := e.sessionRules[sess.SessionKey()]
		e.mu.Unlock()
		if hasStale {
			_ = e.deleteRuleKey(ctx, staleKey)
			e.mu.Lock()
			delete(e.sessionRules, sess.SessionKey())
			e.mu.Unlock()
		}
		e.logSkipNoBinding(sess)
		return nil
	}

	lk := e.keyLock(rule.Key)
	lk.Lock()
	defer lk.Unlock()

	e.mu.Lock()
	old, hasOld := e.desired[rule.Key]
	if hasOld && old.EqualContent(rule) {
		// 内容未变：不递增版本；若已安装成功则直接幂等返回，
		// 否则补发一次（覆盖之前失败的安装）。
		inst, ok := e.installed[rule.Key]
		e.mu.Unlock()
		if ok && inst.Status == webTypes.InstalledStatusInstalled && inst.DesiredVersion == old.DesiredVersion {
			return nil
		}
		rule.DesiredVersion = old.DesiredVersion
	} else if hasOld {
		rule.DesiredVersion = old.DesiredVersion + 1
	} else {
		rule.DesiredVersion = 1
	}
	e.desired[rule.Key] = rule
	e.sessionRules[sess.SessionKey()] = rule.Key
	e.mu.Unlock()

	if !e.active {
		// Standby 只维护状态，不向 UP 写规则（说明书 11.2）。
		return nil
	}
	if !upsert {
		// 启动全量恢复的收集模式：只生成期望规则，不逐条下发；
		// 规则由调用方随后以 RULE_SYNC SNAPSHOT 批量下发（说明书 V1.1 3.9.23）。
		return nil
	}
	return e.upsertRule(ctx, rule)
}

// logSkipNoBinding 记一条“会话无业务绑定、跳过生成规则”的日志。
// 测试用例 022/023/030/036 要在默认 info 级日志里看到“未生成 SNAT 表”，
// 所以这里用 Info 而不用 Debug；同一会话 60 秒内只记一次，
// 无效 DNN 的 UE 事件再多也不会刷屏。
func (e *Engine) logSkipNoBinding(sess *webTypes.UeSession) {
	key := sess.SessionKey()
	e.skipLogMu.Lock()
	if last, ok := e.skipLogLast[key]; ok && time.Since(last) < time.Minute {
		e.skipLogMu.Unlock()
		return
	}
	e.skipLogLast[key] = time.Now()
	e.skipLogMu.Unlock()
	e.log.WithFields(logrus.Fields{"imsi": sess.IMSI, "dnn": sess.DNN, "srcIp": sess.SrcIP}).
		Info("session has no business binding, skip")
}

// upsertRule 向 UP 下发一条规则并落安装状态。调用方已持有该规则键的锁。
func (e *Engine) upsertRule(ctx context.Context, rule *webTypes.ForwardRule) error {
	version := rule.DesiredVersion
	var lastErr error
	for attempt := 0; attempt <= e.cfg.UP.MaxRetry; attempt++ {
		var ack webTypes.RuleAckData
		var code int
		var err error
		reqID := utils.NewRequestID()
		if rule.Key.BusinessType == webTypes.BusinessToH {
			ack, code, err = e.up.UpsertToH(ctx, reqID, rule.ToToHRule())
		} else {
			ack, code, err = e.up.UpsertToB(ctx, reqID, rule.ToToBRule())
		}
		if err == nil {
			e.markInstalled(rule, version, ack.AllocatedSrcNatIP, reqID)
			return nil
		}
		lastErr = err
		// 1001 参数非法：不重试；2001/2002：退避重试；其他 transport 错误同样重试。
		if code == webTypes.UPCodeInvalidParam {
			break
		}
		if attempt < e.cfg.UP.MaxRetry {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(500*(1<<uint(attempt))) * time.Millisecond):
			}
		}
	}
	// 说明书 V1.1 3.9.19 注记：UP 返回失败或超时，直接删除规则。
	// 先尽力向 UP 发 DELETE 清理可能半安装的规则，再删本地期望/安装状态；
	// 会话保留在内存，后续 create/补偿/快照可重新生成规则。
	e.deleteFailedRule(ctx, rule)
	return lastErr
}

func (e *Engine) markInstalled(rule *webTypes.ForwardRule, version uint64, allocatedIP string, requestID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	cur, ok := e.desired[rule.Key]
	if !ok || cur.DesiredVersion != version {
		// 旧 ACK 不得覆盖新规则状态（说明书 10.18）。
		return
	}
	now := utils.NowMilli()
	e.installed[rule.Key] = &webTypes.InstalledState{
		Key: rule.Key, AllocatedSrcNatIP: allocatedIP, DesiredVersion: version,
		Status: webTypes.InstalledStatusInstalled, LastCode: 0, LastMessage: "OK",
		LastRequestID: requestID,
		InstalledAt:   now, UpdatedAt: now,
	}
	e.log.WithFields(logrus.Fields{"rule": rule.Key.String(), "allocated": allocatedIP}).Info("rule installed")
}

// deleteFailedRule 在规则安装最终失败后删除规则（说明书 V1.1 3.9.19 注记）。
// 先尽力向 UP 发一次 DELETE（失败只记日志），再删除本地期望/安装状态。
// 调用方已持有该规则键的锁；会话本身保留，后续事件可重新生成规则。
func (e *Engine) deleteFailedRule(ctx context.Context, rule *webTypes.ForwardRule) {
	reqID := utils.NewRequestID()
	var err error
	if rule.Key.BusinessType == webTypes.BusinessToH {
		_, err = e.up.DeleteToH(ctx, reqID, rule.Key.SrcIP)
	} else {
		_, err = e.up.DeleteToB(ctx, reqID, rule.Key.SrcIP)
	}
	if err != nil {
		e.log.WithError(err).WithField("rule", rule.Key.String()).Warn("delete failed rule on up failed")
	}
	e.mu.Lock()
	delete(e.desired, rule.Key)
	delete(e.installed, rule.Key)
	e.mu.Unlock()
	e.log.WithField("rule", rule.Key.String()).Warn("rule deleted after up install failed")
}

// RemoveSession 处理 UE 下线：按 IMSI+DNN+srcIp 定位，删除 UP 规则与运行态。
// srcIP 为空时删除该 IMSI+DNN 下全部会话（回调缺失 private IP 时的兜底）。
func (e *Engine) RemoveSession(ctx context.Context, imsi, dnn, srcIP string) error {
	e.mu.Lock()
	var targets []*webTypes.UeSession
	if srcIP != "" {
		if s, ok := e.sessions[imsi+"|"+dnn+"|"+srcIP]; ok {
			targets = append(targets, s)
		}
	} else {
		prefix := imsi + "|" + dnn + "|"
		for k, s := range e.sessions {
			if len(k) >= len(prefix) && k[:len(prefix)] == prefix {
				targets = append(targets, s)
			}
		}
	}
	e.mu.Unlock()

	for _, s := range targets {
		if err := e.removeOneSession(ctx, s); err != nil {
			e.log.WithError(err).WithField("session", s.SessionKey()).Error("remove session failed")
		}
	}
	return nil
}

func (e *Engine) removeOneSession(ctx context.Context, sess *webTypes.UeSession) error {
	e.mu.Lock()
	ruleKey, hasRule := e.sessionRules[sess.SessionKey()]
	delete(e.sessions, sess.SessionKey())
	delete(e.sessionRules, sess.SessionKey())
	e.mu.Unlock()
	if !hasRule {
		return nil
	}
	lk := e.keyLock(ruleKey)
	lk.Lock()
	defer lk.Unlock()

	e.mu.Lock()
	_, stillDesired := e.desired[ruleKey]
	e.mu.Unlock()
	if !stillDesired {
		return nil
	}
	if !e.active {
		e.mu.Lock()
		delete(e.desired, ruleKey)
		delete(e.installed, ruleKey)
		e.mu.Unlock()
		return nil
	}
	var lastErr error
	for attempt := 0; attempt <= e.cfg.UP.MaxRetry; attempt++ {
		var code int
		var err error
		reqID := utils.NewRequestID()
		if ruleKey.BusinessType == webTypes.BusinessToH {
			code, err = e.up.DeleteToH(ctx, reqID, ruleKey.SrcIP)
		} else {
			code, err = e.up.DeleteToB(ctx, reqID, ruleKey.SrcIP)
		}
		if err == nil {
			// 1002 也按幂等成功处理（up 层已归一化）。
			e.mu.Lock()
			delete(e.desired, ruleKey)
			delete(e.installed, ruleKey)
			e.mu.Unlock()
			e.log.WithField("rule", ruleKey.String()).Info("rule deleted")
			return nil
		}
		lastErr = err
		_ = code
		if attempt < e.cfg.UP.MaxRetry {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(500*(1<<uint(attempt))) * time.Millisecond):
			}
		}
	}
	e.mu.Lock()
	if st, ok := e.installed[ruleKey]; ok {
		st.Status = webTypes.InstalledStatusDeletePending
		st.UpdatedAt = utils.NowMilli()
	} else {
		e.installed[ruleKey] = &webTypes.InstalledState{Key: ruleKey, Status: webTypes.InstalledStatusDeletePending, UpdatedAt: utils.NowMilli()}
	}
	e.mu.Unlock()
	return lastErr
}

// RecomputeBinding 在配置变更后对某 IMSI+DNN 的全部在线会话重新匹配：
// 绑定变化导致规则内容变化时 UPSERT 更新；会话不再命中时删除规则。
func (e *Engine) RecomputeBinding(ctx context.Context, imsi, dnn string) {
	e.mu.Lock()
	var sessList []*webTypes.UeSession
	prefix := imsi + "|" + dnn + "|"
	for k, s := range e.sessions {
		if len(k) >= len(prefix) && k[:len(prefix)] == prefix {
			sessList = append(sessList, s)
		}
	}
	// 找出该 IMSI+DNN 下已无会话的期望规则（孤儿规则）。
	var orphanKeys []webTypes.RuleKey
	for k, r := range e.desired {
		if r.IMSI == imsi && r.DNN == dnn {
			if _, ok := e.sessions[r.SessionKey]; !ok {
				orphanKeys = append(orphanKeys, k)
			}
		}
	}
	e.mu.Unlock()

	for _, s := range sessList {
		_ = e.ApplySession(ctx, s)
	}
	for _, k := range orphanKeys {
		// 该规则对应的会话已不在内存：按 srcIp 删除。
		_ = e.deleteRuleKey(ctx, k)
	}
}

func (e *Engine) deleteRuleKey(ctx context.Context, k webTypes.RuleKey) error {
	lk := e.keyLock(k)
	lk.Lock()
	defer lk.Unlock()
	if !e.active {
		e.mu.Lock()
		delete(e.desired, k)
		delete(e.installed, k)
		e.mu.Unlock()
		return nil
	}
	var err error
	reqID := utils.NewRequestID()
	if k.BusinessType == webTypes.BusinessToH {
		_, err = e.up.DeleteToH(ctx, reqID, k.SrcIP)
	} else {
		_, err = e.up.DeleteToB(ctx, reqID, k.SrcIP)
	}
	if err == nil {
		e.mu.Lock()
		delete(e.desired, k)
		delete(e.installed, k)
		e.mu.Unlock()
	}
	return err
}

// ---------------------------------------------------------------------------
// 全量同步：启动基线与 DNN 定向补偿共用
// ---------------------------------------------------------------------------

// RefreshUpfDNNs 刷新 UPF 侧 DNN 集合缓存。
func (e *Engine) RefreshUpfDNNs(ctx context.Context) error {
	list, err := e.upf.ListDNNs(ctx)
	if err != nil {
		return err
	}
	m := make(map[string]bool, len(list))
	for _, d := range list {
		m[d.Name] = true
	}
	e.upfMu.Lock()
	e.upfDNN = m
	e.upfMu.Unlock()
	return nil
}

// EffectiveDNNs 计算 Configured ∩ Upf（说明书 5.10.3）。
func (e *Engine) EffectiveDNNs() []string {
	configured := e.snap.Load().ConfiguredDNNs()
	e.upfMu.Lock()
	defer e.upfMu.Unlock()
	var out []string
	for d := range configured {
		if e.upfDNN[d] {
			out = append(out, d)
		}
	}
	return out
}

// FullSyncDNN 对单个 DNN 分页拉取全量会话并应用，返回处理条数。
// 结束条件：某页返回数量 < step。
// upsert=true 时逐条向 UP 下发规则（运行期补偿用）；upsert=false 时只收集
// 期望规则，由调用方随后以 RULE_SYNC SNAPSHOT 批量下发（启动恢复用，说明书 V1.1 3.9.23）。
func (e *Engine) FullSyncDNN(ctx context.Context, dnn string, upsert bool) (int, error) {
	step := e.cfg.Sessions.PageStep
	total := 0
	for page := 1; ; page++ {
		pg, err := e.upf.ListSessions(ctx, page, step, dnn)
		if err != nil {
			return total, err
		}
		for i := range pg.UeSessions {
			s := pg.UeSessions[i]
			if err := e.applySession(ctx, &s, upsert); err != nil {
				e.log.WithError(err).WithField("session", s.SessionKey()).Warn("apply full-sync session failed")
			}
			total++
		}
		if len(pg.UeSessions) < step {
			break
		}
	}
	e.log.WithFields(logrus.Fields{"dnn": dnn, "sessions": total}).Info("full sync dnn done")
	return total, nil
}

// ---------------------------------------------------------------------------
// 运行期 DNN 配置新增事件驱动全量补偿（说明书 5.10.2）
// ---------------------------------------------------------------------------

// TriggerCompensation 为某 DNN 触发定向全量补偿。同一 DNN 在一次补偿
// 执行期间的多次配置事件只保留一个 running + 一个 dirty 标记；
// 不同 DNN 可受控并发（各跑一个 goroutine）。
func (e *Engine) TriggerCompensation(dnn string) {
	e.compMu.Lock()
	st, ok := e.comp[dnn]
	if !ok {
		st = &compState{}
		e.comp[dnn] = st
	}
	if st.running {
		st.dirty = true
		e.compMu.Unlock()
		return
	}
	st.running = true
	st.pending = false
	e.compMu.Unlock()
	go e.runCompensation(dnn)
}

func (e *Engine) runCompensation(dnn string) {
	ctx := context.Background()
	for {
		err := e.compensateOnce(ctx, dnn)
		e.compMu.Lock()
		st := e.comp[dnn]
		if err != nil {
			// 链路/Token/DNN 不可用：标记 pending，待链路恢复后由
			// 维护循环 RetryPending 再次触发。
			st.pending = true
			st.running = false
			e.compMu.Unlock()
			e.log.WithError(err).WithField("dnn", dnn).Warn("dnn compensation pending")
			return
		}
		st.pending = false
		if st.dirty {
			st.dirty = false
			e.compMu.Unlock()
			continue // 基于最新 BindingIndex 再补跑一次
		}
		st.running = false
		e.compMu.Unlock()
		return
	}
}

// compensateOnce 执行一次定向全量补偿：只针对该 DNN，不扫描其他 DNN。
// 补偿期间不重新注册订阅，后续增量仍由 POST create/delete 维护。
func (e *Engine) compensateOnce(ctx context.Context, dnn string) error {
	if !e.active {
		return fmt.Errorf("standby cp does not run compensation")
	}
	if e.IsLinkDown() {
		return fmt.Errorf("upf link down")
	}
	// 确认 dnn 属于 UpfDNNSet；缓存中没有则先刷新一次 DNN List。
	e.upfMu.Lock()
	_, inUpf := e.upfDNN[dnn]
	e.upfMu.Unlock()
	if !inUpf {
		if err := e.RefreshUpfDNNs(ctx); err != nil {
			return fmt.Errorf("refresh dnn list: %w", err)
		}
		e.upfMu.Lock()
		_, inUpf = e.upfDNN[dnn]
		e.upfMu.Unlock()
		if !inUpf {
			return fmt.Errorf("dnn %s not in upf dnn list, keep pending", dnn)
		}
	}
	// 该 DNN 当前是否仍有本机绑定（配置可能在排队期间又被删除）。
	if !e.snap.Load().ConfiguredDNNs()[dnn] {
		return nil
	}
	_, err := e.FullSyncDNN(ctx, dnn, true)
	return err
}

// RetryPending 由维护循环定期调用：重试所有 pending 的 DNN 补偿。
func (e *Engine) RetryPending() {
	e.compMu.Lock()
	var pending []string
	for dnn, st := range e.comp {
		if st.pending && !st.running {
			pending = append(pending, dnn)
		}
	}
	e.compMu.Unlock()
	for _, dnn := range pending {
		e.TriggerCompensation(dnn)
	}
}

// ---------------------------------------------------------------------------
// 配置变更事件消费
// ---------------------------------------------------------------------------

// StartEventLoop 消费配置变更事件（异步，不阻塞 OAM 接口）。
func (e *Engine) StartEventLoop(ctx context.Context) {
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case ev := <-e.events:
				e.onConfigChanged(ctx, ev)
			}
		}
	}()
}

func (e *Engine) onConfigChanged(ctx context.Context, ev webTypes.ConfigChangedEvent) {
	e.log.WithFields(logrus.Fields{"op": ev.Op, "dnns": ev.DNNs}).Info("config changed event")
	switch ev.Op {
	case "DELETE":
		// 删除配置不触发 UPF 全量查询：直接对受影响会话重算（删规则）。
		for _, k := range ev.Keys {
			e.RecomputeBinding(ctx, k.IMSI, k.DNN)
		}
	case "ADD", "BATCH", "BATCH_ADD":
		// 先用内存会话重算，再触发定向全量补偿补齐配置前已在线 UE。
		for _, k := range ev.Keys {
			e.RecomputeBinding(ctx, k.IMSI, k.DNN)
		}
		for _, dnn := range ev.DNNs {
			e.TriggerCompensation(dnn)
		}
	case "MODIFY":
		for _, k := range ev.Keys {
			e.RecomputeBinding(ctx, k.IMSI, k.DNN)
		}
		// 只有首次出现绑定的 DNN 才触发定向全量补偿。
		for _, dnn := range ev.NewDNNs {
			e.TriggerCompensation(dnn)
		}
	}
}

// ---------------------------------------------------------------------------
// RULE_SYNC 批量同步（说明书 6.7 / 10.23）
// ---------------------------------------------------------------------------

// SnapshotSync 把当前全部期望规则按批次下发给 UP，用于 CP/UP 重启、
// 主备切换后的规则恢复（说明书 V1.1 3.5.8 / 3.9.23）。
// 一次发送全部批次：batchNo 从 1 顺序递增，最后一批 lastBatch=true；
// 逐规则失败只重试失败项，走单条 UPSERT；整批调用失败（UP 不可达）的
// 规则不单条重试，保留在期望集等下次快照再推。
// 实时事件优先级高于快照：发送前与 ACK 落态时都以 Desired 版本为准，版本已变的跳过。
func (e *Engine) SnapshotSync(ctx context.Context) error {
	e.mu.Lock()
	var items []snapItem
	for _, r := range e.desired {
		items = append(items, snapItem{rule: r, version: r.DesiredVersion})
	}
	e.mu.Unlock()
	if len(items) == 0 {
		return nil
	}
	if !e.active {
		return nil
	}
	snapshotID := fmt.Sprintf("snap-%d", utils.NowMilli())
	batchSize := e.cfg.UP.SnapshotBatchSize
	if batchSize <= 0 {
		batchSize = 500
	}
	var ruleFailed, batchFailed []snapItem
	for start := 0; start < len(items); start += batchSize {
		end := start + batchSize
		if end > len(items) {
			end = len(items)
		}
		batch := items[start:end]
		lastBatch := end == len(items)
		failed, err := e.sendSnapshotBatch(ctx, snapshotID, uint32(start/batchSize+1), lastBatch, batch)
		if err != nil {
			e.log.WithError(err).WithField("batch", start/batchSize+1).Error("snapshot batch failed")
			batchFailed = append(batchFailed, batch...)
			continue
		}
		ruleFailed = append(ruleFailed, failed...)
	}
	// 逐规则失败只重试失败项（说明书 V1.1 3.5.8.2），走单条 UPSERT；
	// 重试耗尽后 upsertRule 按 3.9.19 删除规则，不再计入待处理。
	failed := ruleFailed
	for round := 0; round < e.cfg.UP.MaxRetry && len(failed) > 0; round++ {
		var still []snapItem
		for _, it := range failed {
			lk := e.keyLock(it.rule.Key)
			lk.Lock()
			e.mu.Lock()
			cur, ok := e.desired[it.rule.Key]
			e.mu.Unlock()
			if !ok || cur.DesiredVersion != it.version {
				lk.Unlock()
				continue
			}
			err := e.upsertRule(ctx, it.rule)
			lk.Unlock()
			if err != nil {
				e.mu.Lock()
				_, stillDesired := e.desired[it.rule.Key]
				e.mu.Unlock()
				if stillDesired {
					still = append(still, it)
				}
			}
		}
		failed = still
	}
	if len(batchFailed) > 0 || len(failed) > 0 {
		return fmt.Errorf("snapshot sync: %d batch-failed and %d rule-failed rules", len(batchFailed), len(failed))
	}
	return nil
}

func (e *Engine) sendSnapshotBatch(ctx context.Context, snapshotID string, batchNo uint32, lastBatch bool, batch []snapItem) ([]snapItem, error) {
	var rules []webTypes.SnapshotRule
	var live []snapItem
	for _, it := range batch {
		// 发送前再校验版本：实时事件已更新的规则跳过，等待下一次同步。
		e.mu.Lock()
		cur, ok := e.desired[it.rule.Key]
		e.mu.Unlock()
		if !ok || cur.DesiredVersion != it.version {
			continue
		}
		var raw json.RawMessage
		var err error
		if it.rule.Key.BusinessType == webTypes.BusinessToH {
			raw, err = json.Marshal(it.rule.ToToHRule())
		} else {
			raw, err = json.Marshal(it.rule.ToToBRule())
		}
		if err != nil {
			return nil, err
		}
		rules = append(rules, webTypes.SnapshotRule{BusinessType: it.rule.Key.BusinessType, Rule: raw})
		live = append(live, it)
	}
	if len(rules) == 0 {
		return nil, nil
	}
	reqID := utils.NewRequestID()
	ack, err := e.up.Snapshot(ctx, reqID, snapshotID, batchNo, lastBatch, rules)
	if err != nil {
		return nil, err
	}
	byKey := make(map[string]webTypes.SnapshotRuleResult)
	for _, r := range ack.Results {
		byKey[r.BusinessType+"|"+r.SrcIP] = r
	}
	var failed []snapItem
	now := utils.NowMilli()
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, it := range live {
		res, ok := byKey[it.rule.Key.String()]
		if !ok {
			failed = append(failed, it)
			continue
		}
		cur, still := e.desired[it.rule.Key]
		if !still || cur.DesiredVersion != it.version {
			continue // 实时事件已更新，旧 ACK 不覆盖
		}
		if res.Code == webTypes.UPCodeOK {
			e.installed[it.rule.Key] = &webTypes.InstalledState{
				Key: it.rule.Key, AllocatedSrcNatIP: res.AllocatedSrcNatIP,
				DesiredVersion: it.version, Status: webTypes.InstalledStatusInstalled,
				LastCode: 0, LastMessage: "OK", LastRequestID: reqID,
				InstalledAt: now, UpdatedAt: now,
			}
		} else {
			// 失败项不落 FAILED 状态，交给 SnapshotSync 的单条重试；
			// 重试耗尽后由 upsertRule 按说明书 V1.1 3.9.19 删除规则。
			failed = append(failed, it)
		}
	}
	return failed, nil
}

// ---------------------------------------------------------------------------
// 运维查询
// ---------------------------------------------------------------------------

// Stats 返回运行态计数与安装结果，供 /debug/state 使用。
type Stats struct {
	Sessions  int                            `json:"sessions"`
	Desired   int                            `json:"desired"`
	Installed []webTypes.InstalledState         `json:"installed"`
	LinkDown  bool                           `json:"linkDown"`
	Active    bool                           `json:"active"`
	UpfDNNs   []string                       `json:"upfDnns"`
}

// Stats 快照运行态。
func (e *Engine) Stats() Stats {
	e.mu.Lock()
	defer e.mu.Unlock()
	st := Stats{
		Sessions: len(e.sessions),
		Desired:  len(e.desired),
		LinkDown: e.IsLinkDown(),
		Active:   e.active,
	}
	for _, v := range e.installed {
		st.Installed = append(st.Installed, *v)
	}
	e.upfMu.Lock()
	for d := range e.upfDNN {
		st.UpfDNNs = append(st.UpfDNNs, d)
	}
	e.upfMu.Unlock()
	return st
}
