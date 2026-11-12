// Package upf 实现 CP 作为客户端访问 UPF 北向 REST 接口。
//
// 接口来源：《智云随行 restapi 接口说明文档 V1.1》与 api 接口表格：
//   GET  /api/upf/v2/user/token?name=&pwd=      获取 token（不带鉴权头）
//   POST /api/upf/v2/ue/subscribe               新增 UE 上下线订阅
//   GET  /api/upf/v2/ue/subscribe               查询订阅列表
//   DELETE /api/upf/v2/ue/subscribe?url=        删除订阅
//   GET  /api/upf/v2/ping                       心跳检查
//   GET  /api/upf/v2/ue/sessions?page&step&dnn  全量 UE 会话（分页）
//   GET  /api/upf/v2/conf/dnnlist?SmartCloud=true  智云随行 DNN 列表
// 除 token 接口外，其余接口 HTTP Header 携带 X-Custom-Key: <token>。
// token 默认有效期 30 分钟；401 或临近过期时自动刷新一次后重试。
package upf

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"

	"natcp/main/webTypes"
)

// StringOrNumber 兼容 UPF 文档中类型不一致的字段（如 S1UVrf 在表格中为
// string、示例中为数字）。统一按字符串处理。
type StringOrNumber string

// UnmarshalJSON 接受 JSON 字符串或数字。
func (s *StringOrNumber) UnmarshalJSON(data []byte) error {
	var str string
	if err := json.Unmarshal(data, &str); err == nil {
		*s = StringOrNumber(str)
		return nil
	}
	var num json.Number
	if err := json.Unmarshal(data, &num); err == nil {
		*s = StringOrNumber(num.String())
		return nil
	}
	return fmt.Errorf("invalid string-or-number %s", string(data))
}

// DnnEntry 是 /conf/dnnlist 返回的一条 DNN。
type DnnEntry struct {
	Name       string         `json:"Name"`
	Remark     string         `json:"Remark"`
	Vrf        int            `json:"Vrf"`
	N3Vrf      int            `json:"N3Vrf"`
	S1UVrf     StringOrNumber `json:"S1UVrf"`
	SmartCloud bool           `json:"SmartCloud"`
}

// UeSubscribe 是订阅记录。
type UeSubscribe struct {
	URL  string `json:"url"`
	Desc string `json:"desc"`
}

// SessionsPage 是 /ue/sessions 一页的返回。
type SessionsPage struct {
	Page       int                 `json:"page"`
	SessionNum int                 `json:"sessionNum"`
	UeSessions []webTypes.UeSession   `json:"ueSessions"`
}

// Client 是 UPF 客户端，内部维护 token 与过期时间。
type Client struct {
	baseURL   string
	user      string
	pwdMD5    string
	ttl       time.Duration
	refreshBefore time.Duration
	http      *http.Client

	mu     sync.Mutex
	token  string
	expiry time.Time

	log *logrus.Entry
}

// New 创建 UPF 客户端。
func New(baseURL, user, pwdMD5 string, timeout time.Duration, ttlMin, refreshBeforeSec int) *Client {
	return &Client{
		baseURL:   strings.TrimRight(baseURL, "/"),
		user:      user,
		pwdMD5:    pwdMD5,
		ttl:       time.Duration(ttlMin) * time.Minute,
		refreshBefore: time.Duration(refreshBeforeSec) * time.Second,
		http:      &http.Client{Timeout: timeout},
		log:           logrus.WithField("module", "upf"),
	}
}

// GetToken 调用 token 接口。响应体可能是带引号的 JSON 字符串或裸字符串，
// 两种形式都兼容。
func (c *Client) GetToken(ctx context.Context) (string, error) {
	u := fmt.Sprintf("%s/api/upf/v2/user/token?name=%s&pwd=%s",
		c.baseURL, url.QueryEscape(c.user), url.QueryEscape(c.pwdMD5))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("upf get token: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("upf get token: http %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var token string
	if err := json.Unmarshal(body, &token); err != nil {
		// 裸字符串形式
		token = strings.Trim(string(body), " \t\r\n\"")
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return "", fmt.Errorf("upf get token: empty token")
	}
	c.mu.Lock()
	c.token = token
	c.expiry = time.Now().Add(c.ttl)
	c.mu.Unlock()
	// 只记“拿到 token”这一事实，不记 token 本身的值。
	c.log.Info("upf token obtained")
	return token, nil
}

// ensureToken 在 token 为空或临近过期时刷新。
func (c *Client) ensureToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	token, expiry := c.token, c.expiry
	c.mu.Unlock()
	if token == "" || time.Until(expiry) < c.refreshBefore {
		return c.GetToken(ctx)
	}
	return token, nil
}

// do 执行一次带鉴权头的 UPF 请求；401 时刷新 token 后重试一次。
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body any) (int, []byte, error) {
	var bodyBytes []byte
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		bodyBytes = b
	}
	call := func(token string) (int, []byte, error) {
		u := c.baseURL + path
		if len(query) > 0 {
			u += "?" + query.Encode()
		}
		var rdr io.Reader
		if bodyBytes != nil {
			rdr = bytes.NewReader(bodyBytes)
		}
		req, err := http.NewRequestWithContext(ctx, method, u, rdr)
		if err != nil {
			return 0, nil, err
		}
		req.Header.Set("Accept", "application/json")
		if bodyBytes != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if token != "" {
			req.Header.Set("X-Custom-Key", token)
		}
		resp, err := c.http.Do(req)
		if err != nil {
			return 0, nil, fmt.Errorf("upf %s %s: %w", method, path, err)
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			return 0, nil, err
		}
		return resp.StatusCode, b, nil
	}

	token, err := c.ensureToken(ctx)
	if err != nil {
		return 0, nil, err
	}
	status, respBody, err := call(token)
	if err != nil {
		return 0, nil, err
	}
	if status == http.StatusUnauthorized {
		// token 可能已过期：刷新一次后重试。
		token, err = c.GetToken(ctx)
		if err != nil {
			return 0, nil, err
		}
		status, respBody, err = call(token)
		if err != nil {
			return 0, nil, err
		}
	}
	return status, respBody, nil
}

// Ping 心跳检查：HTTP 200 表示网元正常。
func (c *Client) Ping(ctx context.Context) error {
	status, body, err := c.do(ctx, http.MethodGet, "/api/upf/v2/ping", nil, nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("upf ping: http %d: %s", status, strings.TrimSpace(string(body)))
	}
	return nil
}

// ListDNNs 获取智云随行 DNN 列表（SmartCloud=true 必填）。
func (c *Client) ListDNNs(ctx context.Context) ([]DnnEntry, error) {
	q := url.Values{}
	q.Set("SmartCloud", "true")
	status, body, err := c.do(ctx, http.MethodGet, "/api/upf/v2/conf/dnnlist", q, nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("upf dnnlist: http %d: %s", status, strings.TrimSpace(string(body)))
	}
	var list []DnnEntry
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("upf dnnlist: decode: %w", err)
	}
	return list, nil
}

// ListSessions 分页拉取某 DNN 的全量 UE 会话。
// 结束条件（V1.1）：某页返回数量 < step 时该 DNN 已取完。
func (c *Client) ListSessions(ctx context.Context, page, step int, dnn string) (*SessionsPage, error) {
	q := url.Values{}
	q.Set("page", fmt.Sprintf("%d", page))
	q.Set("step", fmt.Sprintf("%d", step))
	q.Set("dnn", dnn)
	status, body, err := c.do(ctx, http.MethodGet, "/api/upf/v2/ue/sessions", q, nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("upf sessions dnn=%s page=%d: http %d: %s", dnn, page, status, strings.TrimSpace(string(body)))
	}
	var pg SessionsPage
	if err := json.Unmarshal(body, &pg); err != nil {
		return nil, fmt.Errorf("upf sessions: decode: %w", err)
	}
	for i := range pg.UeSessions {
		pg.UeSessions[i].SrcIP = pg.UeSessions[i].NormalizeSrcIP()
		pg.UeSessions[i].Source = "FULL_SYNC"
	}
	return &pg, nil
}

// Subscribe 新增 UE 上下线订阅。注意 UPF 只向最新注册的回调地址推送，
// 删除最新后使用次新地址；主备部署时仅 Active CP 注册。
func (c *Client) Subscribe(ctx context.Context, callbackURL, desc string) error {
	status, body, err := c.do(ctx, http.MethodPost, "/api/upf/v2/ue/subscribe", nil,
		map[string]string{"url": callbackURL, "desc": desc})
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("upf subscribe: http %d: %s", status, strings.TrimSpace(string(body)))
	}
	return nil
}

// Unsubscribe 删除 UE 上下线订阅（Query 参数 url）。
func (c *Client) Unsubscribe(ctx context.Context, callbackURL string) error {
	q := url.Values{}
	q.Set("url", callbackURL)
	status, body, err := c.do(ctx, http.MethodDelete, "/api/upf/v2/ue/subscribe", q, nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("upf unsubscribe: http %d: %s", status, strings.TrimSpace(string(body)))
	}
	return nil
}

// ListSubscribes 查询订阅列表。
func (c *Client) ListSubscribes(ctx context.Context) ([]UeSubscribe, error) {
	status, body, err := c.do(ctx, http.MethodGet, "/api/upf/v2/ue/subscribe", nil, nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("upf list subscribes: http %d: %s", status, strings.TrimSpace(string(body)))
	}
	var list []UeSubscribe
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("upf list subscribes: decode: %w", err)
	}
	return list, nil
}
