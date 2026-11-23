// 本文件实现 list_upf：查询 CP 到 UPF 的链路状态。
//
// 只读命令：不改库、不重建快照、不发配置事件，也不访问 UPF 网络接口——
// 所以 UPF 暂时不可达时依然能查到链路信息，OAM 侧不会因为对端抖动而查不到配置。
package controllers

import (
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"natcp/main/models"
)

// upfLink 是 list_upf 的一条记录，字段名与其它 list_* 一致用小驼峰。
type upfLink struct {
	// ID 是记录序号，从 1 开始，按整张列表里的位置编号（不受 offset 影响），
	// 只是给 OAM 一个稳定可引用的下标，不落库、不是主键。
	ID        int      `json:"id"`
	LocalAddr string   `json:"localAddr"` // 本端地址：CP 访问 UPF 时的出口 IP
	UPFAddr   string   `json:"upfAddr"`   // UPF 地址：upf.baseURL 里的 host
	Port      int      `json:"port"`      // 端口号：upf.baseURL 里的端口
	DNNs      []string `json:"dnns"`      // DNN 列表：当前静态绑定中出现过的 DNN
}

// upfLinkPage 按分页参数返回 UPF 链路记录，语义与其它 list_* 完全一致：
// reqArgs 是 [pageIndex, pageSize] 或 {"pageIndex":n,"pageSize":m}，页码从 0 开始
// （pageIndex=0 拿第一页），total 是总条数（不受分页参数影响）。
//
// 一条 CP 只对接一个 UPF，所以这里最多只有 1 条记录：OAM 传 pageIndex=0 得到
// 1 条，pageIndex=1 得到空 records 但 total 仍为 1。
func (s *OAMServer) upfLinkPage(p models.Page) ([]upfLink, int, error) {
	all := s.upfLinks()
	total := len(all)
	if p.Offset >= total {
		// 下标越界不算错误：与 list_* 一致，返回空数组 + 真实总数。
		return []upfLink{}, total, nil
	}
	rest := all[p.Offset:]
	if p.Limit > 0 && p.Limit < len(rest) {
		rest = rest[:p.Limit]
	}
	return rest, total, nil
}

// upfLinks 组装 UPF 链路记录。
//
// 没配 UPF（upf.baseURL 为空）或地址解析不出 host 时返回空切片，
// 而不是报错——OAM 侧按“没有 UPF”处理即可。
func (s *OAMServer) upfLinks() []upfLink {
	host, port, ok := parseUpfBaseURL(s.upfBaseURL)
	if !ok {
		return []upfLink{}
	}
	links := []upfLink{{
		LocalAddr: localAddrToward(host, port),
		UPFAddr:   host,
		Port:      port,
		DNNs:      s.configuredDNNs(),
	}}
	// 序号按整张列表的位置从 1 开始编，翻页时同一行的 id 不变。
	for i := range links {
		links[i].ID = i + 1
	}
	return links
}

// parseUpfBaseURL 把 upf.baseURL 拆成 host 与 port。
// 支持 "https://10.216.4.160:8153"、"10.216.4.160:8153" 两种写法；
// 没写端口时按协议补默认值（http→80，https→443）。
func parseUpfBaseURL(base string) (host string, port int, ok bool) {
	base = strings.TrimSpace(base)
	if base == "" {
		return "", 0, false
	}
	// 不带协议时补 "//" 让 url.Parse 把首段当 host，而不是当成 scheme。
	if !strings.Contains(base, "://") {
		base = "//" + base
	}
	u, err := url.Parse(base)
	if err != nil {
		return "", 0, false
	}
	host = u.Hostname()
	if host == "" {
		return "", 0, false
	}
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil {
			return "", 0, false
		}
		port = n
	}
	if port == 0 {
		switch strings.ToLower(u.Scheme) {
		case "http":
			port = 80
		default: // https 或未写协议
			port = 443
		}
	}
	return host, port, true
}

// localAddrToward 取 CP 到 UPF 这条链路的本端出口 IP。
//
// 做法是一次 UDP "connect"：内核选路后就能读到本地地址，而 UDP 不会真的发包，
// 因此不会给 UPF 造成任何负担，也不需要 UPF 在线。取不到（无路由、域名解析失败）
// 时返回空串，由调用方原样吐给 OAM。
func localAddrToward(host string, port int) string {
	if host == "" {
		return ""
	}
	addr := host
	if port > 0 {
		addr = net.JoinHostPort(host, strconv.Itoa(port))
	}
	conn, err := net.DialTimeout("udp", addr, 500*time.Millisecond)
	if err != nil {
		// UPF 侧地址不可路由：退回 0.0.0.0 路由查询，拿默认出口网卡地址。
		if conn, err = net.DialTimeout("udp", "8.8.8.8:53", 500*time.Millisecond); err != nil {
			return ""
		}
	}
	defer conn.Close()
	if ua, ok := conn.LocalAddr().(*net.UDPAddr); ok && ua.IP != nil && !ua.IP.IsUnspecified() {
		return ua.IP.String()
	}
	return ""
}

// configuredDNNs 返回当前静态绑定里出现过的 DNN，去空、排序后输出。
// 无绑定时返回空切片（JSON 里是 []），不返回 nil（否则会序列化成 null）。
func (s *OAMServer) configuredDNNs() []string {
	seen := s.snap.Load().ConfiguredDNNs()
	out := make([]string, 0, len(seen))
	for d := range seen {
		if d != "" {
			out = append(out, d)
		}
	}
	sort.Strings(out)
	return out
}
