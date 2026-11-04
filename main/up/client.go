// Package up 实现 CP 作为客户端访问 UP 的 socket 接口。
//
// 接口来源：《Fusion Access GW CP 接口与数据结构设计说明书》第 6 章：
//   TOH_RULE ADD/DELETE、TOB_RULE ADD/DELETE、RULE_SYNC SNAPSHOT，
//   统一消息信封（requestId/msgType/operation/payload），
//   ACK 中的 allocatedSrcNatIp 由 UP 从 srcNatAddr 地址段内分配后返回。
//
// 帧格式约定（联调约定，待 Q1 冻结）：NDJSON——CP 与 UP 通过 Unix Domain
// Socket 交互，每条消息为一行 JSON，CP 写一行、UP 回一行。
// 若联调冻结为 TCP 或其他帧格式，只需替换本包的 dial/read/write 部分。
package up

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"time"

	"natcp/main/webTypes"
)

// Client 是 UP socket 客户端。
type Client struct {
	socket   string // 如 unix:///var/run/fusion-access-gw/up.sock
	timeout  time.Duration
	maxRetry int
}

// New 创建 UP 客户端。socket 支持 unix:// 前缀或裸路径。
func New(socket string, timeout time.Duration, maxRetry int) *Client {
	return &Client{socket: socket, timeout: timeout, maxRetry: maxRetry}
}

func (c *Client) dialPath() string {
	return strings.TrimPrefix(c.socket, "unix://")
}

// call 发送一条信封并读取一行 ACK。transport 错误会按调用方策略重试，
// 业务错误码由调用方按说明书 6.8 处理。
func (c *Client) call(ctx context.Context, env webTypes.MessageEnvelope) (*webTypes.ResponseEnvelope, error) {
	data, err := json.Marshal(env)
	if err != nil {
		return nil, err
	}
	var lastErr error
	for attempt := 0; attempt <= c.maxRetry; attempt++ {
		resp, err := c.callOnce(ctx, data)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff(attempt)):
		}
	}
	return nil, fmt.Errorf("up call %s %s: %w", env.MsgType, env.Operation, lastErr)
}

func backoff(attempt int) time.Duration {
	// 500ms、1s、2s……指数退避，上限 5s。
	d := 500 * time.Millisecond * time.Duration(1<<uint(attempt))
	if d > 5*time.Second {
		d = 5 * time.Second
	}
	return d
}

func (c *Client) callOnce(ctx context.Context, data []byte) (*webTypes.ResponseEnvelope, error) {
	d := net.Dialer{Timeout: c.timeout}
	conn, err := d.DialContext(ctx, "unix", c.dialPath())
	if err != nil {
		return nil, fmt.Errorf("dial up socket: %w", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(c.timeout))
	if _, err := conn.Write(append(data, '\n')); err != nil {
		return nil, fmt.Errorf("write to up: %w", err)
	}
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return nil, fmt.Errorf("read up ack: %w", err)
	}
	var resp webTypes.ResponseEnvelope
	if err := json.Unmarshal(line, &resp); err != nil {
		return nil, fmt.Errorf("decode up ack: %w", err)
	}
	return &resp, nil
}

// parseAckData 解析 UPSERT 成功 ACK 的 data。
func parseAckData(resp *webTypes.ResponseEnvelope) (webTypes.RuleAckData, error) {
	var data webTypes.RuleAckData
	if len(resp.Data) == 0 {
		return data, fmt.Errorf("up ack missing data")
	}
	if err := json.Unmarshal(resp.Data, &data); err != nil {
		return data, fmt.Errorf("decode up ack data: %w", err)
	}
	return data, nil
}

// UpsertToH 下发 ToH 规则新增/更新（说明书 6.2，operation=ADD）。
// 返回 UP 分配的具体 SNAT IP 与 UP 响应码；1001 不重试、2001/2002
// 由 call 的 transport 重试之外的业务层重试处理（见 engine）。
func (c *Client) UpsertToH(ctx context.Context, requestID string, rule webTypes.ToHRule) (webTypes.RuleAckData, int, error) {
	payload, _ := json.Marshal(rule)
	env := webTypes.MessageEnvelope{RequestID: requestID, MsgType: "TOH_RULE", Operation: "ADD", Payload: payload}
	resp, err := c.call(ctx, env)
	if err != nil {
		return webTypes.RuleAckData{}, 0, err
	}
	if resp.Code != webTypes.UPCodeOK {
		return webTypes.RuleAckData{}, resp.Code, fmt.Errorf("up toh upsert failed code=%d msg=%s", resp.Code, resp.Message)
	}
	data, err := parseAckData(resp)
	return data, resp.Code, err
}

// UpsertToB 下发 ToB 规则新增/更新（说明书 6.4）。
func (c *Client) UpsertToB(ctx context.Context, requestID string, rule webTypes.ToBRule) (webTypes.RuleAckData, int, error) {
	payload, _ := json.Marshal(rule)
	env := webTypes.MessageEnvelope{RequestID: requestID, MsgType: "TOB_RULE", Operation: "ADD", Payload: payload}
	resp, err := c.call(ctx, env)
	if err != nil {
		return webTypes.RuleAckData{}, 0, err
	}
	if resp.Code != webTypes.UPCodeOK {
		return webTypes.RuleAckData{}, resp.Code, fmt.Errorf("up tob upsert failed code=%d msg=%s", resp.Code, resp.Message)
	}
	data, err := parseAckData(resp)
	return data, resp.Code, err
}

// DeleteToH 删除 ToH 规则（说明书 6.3，只带 srcIp）。
// 1002（规则不存在）按幂等成功处理。
func (c *Client) DeleteToH(ctx context.Context, requestID, srcIP string) (int, error) {
	payload, _ := json.Marshal(webTypes.RuleDelete{SrcIP: srcIP})
	env := webTypes.MessageEnvelope{RequestID: requestID, MsgType: "TOH_RULE", Operation: "DELETE", Payload: payload}
	resp, err := c.call(ctx, env)
	if err != nil {
		return 0, err
	}
	if resp.Code != webTypes.UPCodeOK && resp.Code != webTypes.UPCodeRuleNotFound {
		return resp.Code, fmt.Errorf("up toh delete failed code=%d msg=%s", resp.Code, resp.Message)
	}
	return resp.Code, nil
}

// DeleteToB 删除 ToB 规则（说明书 6.5）。
func (c *Client) DeleteToB(ctx context.Context, requestID, srcIP string) (int, error) {
	payload, _ := json.Marshal(webTypes.RuleDelete{SrcIP: srcIP})
	env := webTypes.MessageEnvelope{RequestID: requestID, MsgType: "TOB_RULE", Operation: "DELETE", Payload: payload}
	resp, err := c.call(ctx, env)
	if err != nil {
		return 0, err
	}
	if resp.Code != webTypes.UPCodeOK && resp.Code != webTypes.UPCodeRuleNotFound {
		return resp.Code, fmt.Errorf("up tob delete failed code=%d msg=%s", resp.Code, resp.Message)
	}
	return resp.Code, nil
}

// Snapshot 发送一批 RULE_SYNC SNAPSHOT（说明书 6.7）。
// 返回批次 ACK；逐规则结果由调用方落入 Installed State。
func (c *Client) Snapshot(ctx context.Context, requestID, snapshotID string, batchNo uint32, lastBatch bool, rules []webTypes.SnapshotRule) (*webTypes.SnapshotAckData, error) {
	payload, _ := json.Marshal(webTypes.SnapshotPayload{
		SnapshotID: snapshotID, BatchNo: batchNo, LastBatch: lastBatch, Rules: rules,
	})
	env := webTypes.MessageEnvelope{RequestID: requestID, MsgType: "RULE_SYNC", Operation: "SNAPSHOT", Payload: payload}
	resp, err := c.call(ctx, env)
	if err != nil {
		return nil, err
	}
	if resp.Code != webTypes.UPCodeOK {
		return nil, fmt.Errorf("up snapshot batch %d failed code=%d msg=%s", batchNo, resp.Code, resp.Message)
	}
	var ack webTypes.SnapshotAckData
	if err := json.Unmarshal(resp.Data, &ack); err != nil {
		return nil, fmt.Errorf("decode snapshot ack: %w", err)
	}
	return &ack, nil
}
