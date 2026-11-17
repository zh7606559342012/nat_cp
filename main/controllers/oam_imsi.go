// 本文件实现 list_imsi：按 IMSI 查询维护信息（地址、DNN、转发关系）。
//
// 只读命令：不改库、不重建快照、不发配置事件，也不访问 UPF/UP 网络接口——
// 查的是 CP 内存里的静态绑定快照与运行态会话/规则，所以对端暂时不可达时
// 依然能查到。查不到（无绑定也无会话）返回 total=0 / records=[]，不是报错。
package controllers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"

	"natcp/main/conf"
	"natcp/main/handler"
	"natcp/main/webTypes"
)

// imsiInfo 是 list_imsi 的一条记录，对应一个 (IMSI, DNN, 地址)。
// 字段名与其它 list_* 一致用小驼峰；不适用的字段给零值，不省略。
type imsiInfo struct {
	IMSI         string `json:"imsi"`         // 查询的 IMSI
	DNN          string `json:"dnn"`          // 该记录所属 DNN
	Address      string `json:"address"`      // UE 地址：会话 SrcIP（privateIpv4 优先）；离线时 ""
	Online       bool   `json:"online"`       // 是否有在网会话
	BusinessType string `json:"businessType"` // 转发类型：TOH / TOB；无绑定时 ""
	VNI          uint32 `json:"vni"`          // ToH 有效；ToB 为 0
	FttrbID      string `json:"fttrbId"`      // ToB 有效；ToH 为 ""
	SrcNatAddr   string `json:"srcNatAddr"`   // 转发 NAT 地址池；无绑定时 ""
	AllocatedIP  string `json:"allocatedIp"`  // UP 已分配的 SNAT IP；未安装时 ""
	RuleStatus   string `json:"ruleStatus"`   // NONE / PENDING / INSTALLED / FAILED / DELETE_PENDING
}

// ruleStatusOf 由会话的规则状态拼出 ruleStatus：
// 无期望规则 → NONE；有期望规则但无 installed 记录 → PENDING（待安装确认，
// 含 standby 与启动全量收集阶段）；有 installed 记录 → 其 Status。
func ruleStatusOf(ent handler.IMSISession) string {
	if !ent.Desired {
		return "NONE"
	}
	if ent.Status != "" {
		return ent.Status
	}
	return "PENDING"
}

// parseIMSIArg 解析 list_imsi 的 reqArgs，接受三种写法：
//
//	"460001111111111"           裸字符串
//	["460001111111111"]         单元素数组
//	{"imsi":"460001111111111"}  对象
//
// imsi 用 flexStr 收：OAM 用 JS 拼 JSON 时 15 位数字极易退化成 number。
func parseIMSIArg(args json.RawMessage) (string, error) {
	body := bytes.TrimSpace(args)
	if len(body) == 0 || bytes.Equal(body, []byte("null")) {
		return "", fmt.Errorf(`list_imsi: reqArgs is required, expect "imsi", ["imsi"] or {"imsi":...}`)
	}
	var imsi flexStr
	switch body[0] {
	case '[':
		var arr []flexStr
		if err := json.Unmarshal(body, &arr); err != nil {
			return "", fmt.Errorf(`list_imsi: reqArgs must be an array with one imsi, got %s`, body)
		}
		if len(arr) != 1 {
			return "", fmt.Errorf("list_imsi: reqArgs must contain exactly 1 element (the imsi), got %d", len(arr))
		}
		imsi = arr[0]
	case '{':
		var obj struct {
			IMSI flexStr `json:"imsi"`
		}
		if err := json.Unmarshal(body, &obj); err != nil {
			return "", fmt.Errorf(`list_imsi: reqArgs.imsi must be a string or number, got %s`, body)
		}
		imsi = obj.IMSI
	default:
		// 裸字符串或裸数字都由 flexStr 统一收成字符串。
		if err := json.Unmarshal(body, &imsi); err != nil {
			return "", fmt.Errorf(`list_imsi: reqArgs must be "imsi", ["imsi"] or {"imsi":...}, got %s`, body)
		}
	}
	if string(imsi) == "" {
		return "", fmt.Errorf("list_imsi: imsi is empty")
	}
	return string(imsi), nil
}

// lookupIMSI 按 IMSI 查维护信息。查不到返回 total=0 / records=[]，不是报错。
func (s *OAMServer) lookupIMSI(args json.RawMessage) (pageResult, error) {
	imsi, err := parseIMSIArg(args)
	if err != nil {
		return pageResult{}, err
	}
	records := buildIMSIInfo(imsi, s.snap.Load(), s.engine.LookupIMSI(imsi))
	return pageResult{Items: records, Total: len(records)}, nil
}

// buildIMSIInfo 把静态绑定与在网会话拼成 list_imsi 的记录。
// sessions 是 engine 的只读快照（值副本）。
//
// 一条记录对应一个 (IMSI, DNN, 地址)：
//   - 有会话的 DNN：每个会话地址各出一行，online=true；
//   - 有绑定、无会话的 DNN：补一行离线记录（address 为空、online=false），
//     让 OAM 能区分“没配绑定”和“配了但 UE 不在线”；
//   - 有会话、无绑定的 DNN：也出一行（业务字段为空、ruleStatus=NONE），
//     即此前只记“session has no business binding, skip”的那类会话。
func buildIMSIInfo(imsi string, snap *conf.Snapshot, sessions []handler.IMSISession) []imsiInfo {
	// 静态绑定按 DNN 归集，同时取到转发 NAT 地址池。
	type binding struct {
		businessType string
		vni          uint32
		fttrbID      string
		srcNatAddr   string
	}
	binds := map[string]binding{}
	for k, b := range snap.Bindings {
		if k.IMSI != imsi {
			continue
		}
		bb := binding{businessType: b.BusinessType, vni: b.VNI, fttrbID: b.FttrbID}
		switch b.BusinessType {
		case webTypes.BusinessToH:
			if cfg, ok := snap.LookupVxlan(b.VNI); ok {
				bb.srcNatAddr = cfg.SrcNatAddr
			}
		case webTypes.BusinessToB:
			if cfg, ok := snap.LookupFttrb(b.FttrbID); ok {
				bb.srcNatAddr = cfg.SrcNatAddr
			}
		}
		binds[k.DNN] = bb
	}

	records := make([]imsiInfo, 0, len(sessions)+len(binds))
	seenDNN := map[string]bool{}
	for _, ent := range sessions {
		sess := ent.Session
		rec := imsiInfo{
			IMSI:        imsi,
			DNN:         sess.DNN,
			Address:     sess.SrcIP,
			Online:      true,
			RuleStatus:  ruleStatusOf(ent),
			AllocatedIP: ent.Allocated,
		}
		if bb, ok := binds[sess.DNN]; ok {
			rec.BusinessType = bb.businessType
			rec.VNI = bb.vni
			rec.FttrbID = bb.fttrbID
			rec.SrcNatAddr = bb.srcNatAddr
		}
		records = append(records, rec)
		seenDNN[sess.DNN] = true
	}
	for dnn, bb := range binds {
		if seenDNN[dnn] {
			continue
		}
		records = append(records, imsiInfo{
			IMSI:         imsi,
			DNN:          dnn,
			Online:       false,
			BusinessType: bb.businessType,
			VNI:          bb.vni,
			FttrbID:      bb.fttrbID,
			SrcNatAddr:   bb.srcNatAddr,
			RuleStatus:   "NONE",
		})
	}
	sort.Slice(records, func(i, j int) bool {
		if records[i].DNN != records[j].DNN {
			return records[i].DNN < records[j].DNN
		}
		return records[i].Address < records[j].Address
	})
	return records
}
