// Mock 程序：本地联调用的假 UPF + 假 UP，仅用于开发测试，绝不上生产。
//
// 假 UPF（HTTP，默认 127.0.0.1:18153）：实现 token / ping / dnnlist /
// sessions / subscribe，供 CP 按说明书 5.10.1 的启动流程跑通。
// 假 UP（Unix Domain Socket，默认 /tmp/mock-up.sock）：实现 CP-UP
// NDJSON 信封协议，ADD 时从 srcNatAddr 地址段内顺序分配具体 SNAT IP
// 并在 ACK 中返回，DELETE 按 srcIp 删除，SNAPSHOT 按批次安装。
//
// 联调辅助接口（都在假 UPF 的 HTTP 服务上）：
//   POST /__mock/trigger  {action,imsi,dnn,privateIpv4}  向最新回调地址推送 UE 事件
//   GET  /__mock/state                                查看订阅与种子会话
//   GET  /__mock/up/rules                            查看 UP 已安装规则
package main

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
)

// ---------------------------------------------------------------------------
// 种子数据
// ---------------------------------------------------------------------------

type session struct {
	Action     string `json:"action"`
	IMSI       string `json:"imsi"`
	DNN        string `json:"dnn"`
	MSISDN     string `json:"msisdn"`
	UeIPv4     string `json:"ueIpv4,omitempty"`
	PrivateIPv4 string `json:"privateIpv4,omitempty"`
	PrivateIPv6 string `json:"privateIpv6,omitempty"`
}

var seedSessions = map[string][]session{
	"area1": {
		{Action: "create", IMSI: "460001111111111", DNN: "area1", MSISDN: "13800000001", UeIPv4: "10.8.1.1", PrivateIPv4: "20.20.1.1"},
	},
	"area2": {
		{Action: "create", IMSI: "460002222222222", DNN: "area2", MSISDN: "13800000002", UeIPv4: "10.8.2.1", PrivateIPv4: "20.20.2.1"},
	},
}

var seedDNNs = []map[string]any{
	{"Name": "area1", "Remark": "mock toh dnn", "Vrf": 0, "N3Vrf": 0, "S1UVrf": "", "SmartCloud": true},
	{"Name": "area2", "Remark": "mock tob dnn", "Vrf": 0, "N3Vrf": 0, "S1UVrf": "", "SmartCloud": true},
}

// ---------------------------------------------------------------------------
// 假 UP：UDS + NDJSON + CIDR 顺序分配
// ---------------------------------------------------------------------------

type upRule struct {
	BusinessType      string `json:"businessType"`
	SrcIP             string `json:"srcIp"`
	AllocatedSrcNatIP string `json:"allocatedSrcNatIp"`
	SrcNatAddr        string `json:"srcNatAddr"`
}

type mockUP struct {
	mu        sync.Mutex
	rules     map[string]*upRule // businessType|srcIp -> rule
	allocNext map[string]uint32  // cidr -> 下一个偏移
}

func newMockUP() *mockUP {
	return &mockUP{rules: map[string]*upRule{}, allocNext: map[string]uint32{}}
}

// alloc 从 CIDR 内顺序分配一个具体 IP，跳过网络地址与广播地址。
func (m *mockUP) alloc(cidr string) (string, error) {
	_, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return "", err
	}
	ip := ipnet.IP.To4()
	if ip == nil {
		return "", fmt.Errorf("only ipv4 cidr supported in mock: %s", cidr)
	}
	ones, bits := ipnet.Mask.Size()
	hostBits := bits - ones
	if hostBits < 2 {
		return "", fmt.Errorf("cidr too small: %s", cidr)
	}
	maxHosts := uint32(1<<uint(hostBits)) - 2 // 去掉网络地址与广播地址
	off := m.allocNext[cidr]
	if off >= maxHosts {
		off = 0
	}
	base := binary.BigEndian.Uint32(ip)
	candidate := base + 1 + off
	m.allocNext[cidr] = off + 1
	out := make(net.IP, 4)
	binary.BigEndian.PutUint32(out, candidate)
	return out.String(), nil
}

type envelope struct {
	RequestID string          `json:"requestId"`
	MsgType   string          `json:"msgType"`
	Operation string          `json:"operation"`
	Payload   json.RawMessage `json:"payload"`
}

type response struct {
	RequestID string `json:"requestId"`
	Code      int    `json:"code"`
	Message   string `json:"message"`
	Data      any    `json:"data,omitempty"`
}

func (m *mockUP) serve(sockPath string) error {
	// 上一个 mock 若是被强杀（taskkill / 关窗口），socket 文件会残留。
	// Windows 上残留文件删除失败通常意味着还有进程占着它，此时 bind 会报
	// "Only one usage of each socket address"，信息很难懂，所以提前讲清楚。
	if err := os.Remove(sockPath); err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "warning: 无法删除残留 socket %s: %v\n"+
			"         若随后 bind 失败，请先执行：taskkill /F /IM mock.exe\n", sockPath, err)
	}
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		return err
	}
	for {
		conn, err := ln.Accept()
		if err != nil {
			return err
		}
		go m.handleConn(conn)
	}
}

func (m *mockUP) handleConn(conn net.Conn) {
	defer conn.Close()
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return
	}
	var env envelope
	if err := json.Unmarshal(line, &env); err != nil {
		_ = json.NewEncoder(conn).Encode(response{Code: 1001, Message: "bad json"})
		return
	}
	resp := m.dispatch(env)
	_ = json.NewEncoder(conn).Encode(resp)
}

func (m *mockUP) dispatch(env envelope) response {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch env.MsgType {
	case "TOH_RULE", "TOB_RULE":
		switch env.Operation {
		case "ADD":
			var p struct {
				SrcIP      string `json:"srcIp"`
				SrcNatAddr string `json:"srcNatAddr"`
			}
			if err := json.Unmarshal(env.Payload, &p); err != nil || p.SrcIP == "" || p.SrcNatAddr == "" {
				return response{RequestID: env.RequestID, Code: 1001, Message: "invalid param"}
			}
			ip, err := m.alloc(p.SrcNatAddr)
			if err != nil {
				return response{RequestID: env.RequestID, Code: 2001, Message: err.Error()}
			}
			bt := "TOH"
			if env.MsgType == "TOB_RULE" {
				bt = "TOB"
			}
			m.rules[bt+"|"+p.SrcIP] = &upRule{BusinessType: bt, SrcIP: p.SrcIP, AllocatedSrcNatIP: ip, SrcNatAddr: p.SrcNatAddr}
			return response{RequestID: env.RequestID, Code: 0, Message: "OK",
				Data: map[string]string{"srcIp": p.SrcIP, "allocatedSrcNatIp": ip}}
		case "DELETE":
			var p struct {
				SrcIP string `json:"srcIp"`
			}
			_ = json.Unmarshal(env.Payload, &p)
			bt := "TOH"
			if env.MsgType == "TOB_RULE" {
				bt = "TOB"
			}
			if _, ok := m.rules[bt+"|"+p.SrcIP]; !ok {
				return response{RequestID: env.RequestID, Code: 1002, Message: "rule not found"}
			}
			delete(m.rules, bt+"|"+p.SrcIP)
			return response{RequestID: env.RequestID, Code: 0, Message: "OK"}
		}
	case "RULE_SYNC":
		if env.Operation == "SNAPSHOT" {
			var p struct {
				SnapshotID string `json:"snapshotId"`
				BatchNo    uint32 `json:"batchNo"`
				LastBatch  bool   `json:"lastBatch"`
				Rules      []struct {
					BusinessType string          `json:"businessType"`
					Rule         json.RawMessage `json:"rule"`
				} `json:"rules"`
			}
			if err := json.Unmarshal(env.Payload, &p); err != nil {
				return response{RequestID: env.RequestID, Code: 1001, Message: "bad snapshot payload"}
			}
			var results []map[string]any
			for _, r := range p.Rules {
				var rule struct {
					SrcIP      string `json:"srcIp"`
					SrcNatAddr string `json:"srcNatAddr"`
				}
				_ = json.Unmarshal(r.Rule, &rule)
				ip, err := m.alloc(rule.SrcNatAddr)
				if err != nil {
					results = append(results, map[string]any{"businessType": r.BusinessType, "srcIp": rule.SrcIP, "code": 2001, "message": err.Error()})
					continue
				}
				m.rules[r.BusinessType+"|"+rule.SrcIP] = &upRule{BusinessType: r.BusinessType, SrcIP: rule.SrcIP, AllocatedSrcNatIP: ip, SrcNatAddr: rule.SrcNatAddr}
				results = append(results, map[string]any{"businessType": r.BusinessType, "srcIp": rule.SrcIP, "code": 0, "allocatedSrcNatIp": ip})
			}
			return response{RequestID: env.RequestID, Code: 0, Message: "OK",
				Data: map[string]any{"snapshotId": p.SnapshotID, "batchNo": p.BatchNo,
					"successCount": len(results), "failedCount": 0, "results": results}}
		}
	}
	return response{RequestID: env.RequestID, Code: 1001, Message: "unsupported msgType/operation"}
}

// ---------------------------------------------------------------------------
// 假 UPF：HTTP
// ---------------------------------------------------------------------------

type mockUPF struct {
	mu   sync.Mutex
	subs []map[string]string // 按注册顺序保存；最新一条为推送目标
	up   *mockUP
}

func (m *mockUPF) requireToken(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("X-Custom-Key") != "mock-token" {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("unauthorized"))
		return false
	}
	return true
}

func (m *mockUPF) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/upf/v2/user/token", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode("mock-token")
	})
	mux.HandleFunc("/api/upf/v2/ping", func(w http.ResponseWriter, r *http.Request) {
		if !m.requireToken(w, r) {
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/api/upf/v2/conf/dnnlist", func(w http.ResponseWriter, r *http.Request) {
		if !m.requireToken(w, r) {
			return
		}
		_ = json.NewEncoder(w).Encode(seedDNNs)
	})
	mux.HandleFunc("/api/upf/v2/ue/sessions", func(w http.ResponseWriter, r *http.Request) {
		if !m.requireToken(w, r) {
			return
		}
		q := r.URL.Query()
		page, _ := strconv.Atoi(q.Get("page"))
		step, _ := strconv.Atoi(q.Get("step"))
		if page < 1 {
			page = 1
		}
		if step < 1 {
			step = 10
		}
		all := seedSessions[q.Get("dnn")]
		start := (page - 1) * step
		var pageItems []session
		if start < len(all) {
			end := start + step
			if end > len(all) {
				end = len(all)
			}
			pageItems = all[start:end]
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"page": page, "sessionNum": len(all), "ueSessions": pageItems,
		})
	})
	mux.HandleFunc("/api/upf/v2/ue/subscribe", func(w http.ResponseWriter, r *http.Request) {
		if !m.requireToken(w, r) {
			return
		}
		switch r.Method {
		case http.MethodPost:
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			m.mu.Lock()
			m.subs = append(m.subs, body)
			m.mu.Unlock()
			w.WriteHeader(http.StatusOK)
		case http.MethodGet:
			m.mu.Lock()
			defer m.mu.Unlock()
			_ = json.NewEncoder(w).Encode(m.subs)
		case http.MethodDelete:
			target := r.URL.Query().Get("url")
			m.mu.Lock()
			var kept []map[string]string
			for _, s := range m.subs {
				if s["url"] != target {
					kept = append(kept, s)
				}
			}
			m.subs = kept
			m.mu.Unlock()
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
	// 向最新回调地址推送 UE 事件。
	mux.HandleFunc("/__mock/trigger", func(w http.ResponseWriter, r *http.Request) {
		var ev session
		if err := json.NewDecoder(r.Body).Decode(&ev); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		m.mu.Lock()
		if len(m.subs) == 0 {
			m.mu.Unlock()
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte("no subscriber"))
			return
		}
		target := m.subs[len(m.subs)-1]["url"]
		m.mu.Unlock()
		body, _ := json.Marshal(ev)
		resp, err := http.Post(target, "application/json", strings.NewReader(string(body)))
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(err.Error()))
			return
		}
		defer resp.Body.Close()
		w.WriteHeader(resp.StatusCode)
	})
	mux.HandleFunc("/__mock/state", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"subs": m.subs, "seedSessions": seedSessions})
	})
	mux.HandleFunc("/__mock/up/rules", func(w http.ResponseWriter, r *http.Request) {
		m.up.mu.Lock()
		defer m.up.mu.Unlock()
		var out []*upRule
		for _, v := range m.up.rules {
			out = append(out, v)
		}
		_ = json.NewEncoder(w).Encode(out)
	})
	return mux
}

func main() {
	upfListen := flag.String("upf-listen", "127.0.0.1:18153", "mock upf http listen addr")
	upSock := flag.String("up-sock", "/tmp/mock-up.sock", "mock up unix socket path")
	flag.Parse()

	up := newMockUP()
	upf := &mockUPF{up: up}
	go func() {
		if err := up.serve(*upSock); err != nil {
			fmt.Println("mock up exited:", err)
			os.Exit(1)
		}
	}()
	fmt.Println("mock upf listening on", *upfListen, "| mock up socket", *upSock)
	if err := http.ListenAndServe(*upfListen, upf.routes()); err != nil {
		fmt.Println("mock upf exited:", err)
		os.Exit(1)
	}
}
