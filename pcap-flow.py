#!/usr/bin/env python3
"""pcap-flow.py：把抓到的 CP↔UPF 交互 pcap 还原成时间线。

只用 Python 标准库，不用装 tshark/Wireshark。

抓包（跑 smoke_test.sh 之前，另开一个终端先起抓包）：
    Linux： sudo tcpdump -i lo  -s 0 -w /tmp/cp-upf.pcap 'tcp port 18153 or tcp port 18080 or tcp port 10001'
    macOS： sudo tcpdump -i lo0 -s 0 -w /tmp/cp-upf.pcap 'tcp port 18153 or tcp port 18080 or tcp port 10001'
看结果：
    ./pcap-flow.py /tmp/cp-upf.pcap            # 时间线 + 按接口统计
    ./pcap-flow.py --bodies /tmp/cp-upf.pcap   # 再打印回调/OAM 请求的 body

说明：
  - 只认 HTTP 明文。mock 是 HTTP，能看到完整 URI 和状态码。
  - 真 UPF 是 HTTPS，包里只有 TLS 握手，看不到 URI：解密需在启动 CP 前设置
    SSLKEYLOGFILE=/tmp/sslkey.log，再用 tshark -o tls.keylog_file:/tmp/sslkey.log 解。
  - CP→UP 走 UDS（unix socket），不在 pcap 里：看 /__mock/up/rules 和日志。
  - token 请求 URI 里带 pwd MD5，脚本会把 pwd=... 打码；token 响应体不打印。

端口可改：UPF_PORT=18153 CB_PORT=18080 OAM_PORT=10001 ./pcap-flow.py file.pcap
"""
import os
import re
import struct
import sys
from collections import Counter

UPF_PORT = int(os.environ.get("UPF_PORT", "18153"))  # mock UPF：CP 主动调它
CB_PORT = int(os.environ.get("CB_PORT", "18080"))    # CP 回调：UPF 主动推它
OAM_PORT = int(os.environ.get("OAM_PORT", "10001"))  # OAM 配置接口

LINKTYPE_ETHERNET = 1
LINKTYPE_RAW = 12       # 裸 IP（部分抓包工具）
LINKTYPE_RAW2 = 101
LINKTYPE_LINUX_SLL = 113  # Linux cooked capture


def read_pcap(path):
    """读 pcap，返回 [(相对秒, 原始链路层帧)]。"""
    with open(path, "rb") as f:
        data = f.read()
    if len(data) < 24:
        raise ValueError("文件太小，不是 pcap")
    magic = data[:4]
    if magic == b"\xd4\xc3\xb2\xa1":      # 小端，微秒
        e, ts_frac = "<", 1_000_000
    elif magic == b"\xa1\xb2\xc3\xd4":    # 大端，微秒
        e, ts_frac = ">", 1_000_000
    elif magic == b"\x4d\x3c\xb2\xa1":    # 小端，纳秒
        e, ts_frac = "<", 1_000_000_000
    elif magic == b"\xa1\xb2\x3c\x4d":    # 大端，纳秒
        e, ts_frac = ">", 1_000_000_000
    else:
        raise ValueError("不是 pcap 文件（magic 不认识）")
    _ver_major, _ver_minor, _tz, _sig, _snap, linktype = struct.unpack(e + "HHIIII", data[4:24])
    frames = []
    off, t0 = 24, None
    while off + 16 <= len(data):
        ts_sec, ts_sub, incl, _orig = struct.unpack(e + "IIII", data[off:off + 16])
        off += 16
        frame = data[off:off + incl]
        off += incl
        if len(frame) < incl:
            break
        ts = ts_sec + ts_sub / ts_frac
        if t0 is None:
            t0 = ts
        frames.append((ts - t0, frame))
    return linktype, frames


def strip_l2(linktype, frame):
    """去链路层头，返回 IP 包；认不出的返回 None。"""
    if linktype == LINKTYPE_ETHERNET:
        if len(frame) < 14 or frame[12:14] != b"\x08\x00":
            return None
        return frame[14:]
    if linktype in (LINKTYPE_RAW, LINKTYPE_RAW2):
        return frame
    if linktype == LINKTYPE_LINUX_SLL:
        if len(frame) < 16 or frame[14:16] != b"\x08\x00":
            return None
        return frame[16:]
    return None


def parse_tcp_payloads(linktype, frames):
    """按 TCP 四元组重组出每条单向流的字节流。

    返回 [(首包相对秒, src_ip, src_port, dst_ip, dst_port, 字节流)]。
    只拼 payload；重传按 seq 去重；有空洞就截断（并记一笔）。
    """
    streams = {}
    for ts, frame in frames:
        ip = strip_l2(linktype, frame)
        if ip is None or len(ip) < 20:
            continue
        version = ip[0] >> 4
        if version != 4:
            continue  # 只处理 IPv4（mock/联调都是 v4）
        ihl = (ip[0] & 0x0F) * 4
        proto = ip[9]
        if proto != 6 or len(ip) < ihl + 20:
            continue
        src = ".".join(str(b) for b in ip[12:16])
        dst = ".".join(str(b) for b in ip[16:20])
        tcp = ip[ihl:]
        sport, dport, seq = struct.unpack(">HHI", tcp[:8])
        doff = (tcp[12] >> 4) * 4
        payload = tcp[doff:]
        if not payload:
            continue
        key = (src, sport, dst, dport)
        streams.setdefault(key, {"ts": ts, "segs": {}})
        streams[key]["segs"].setdefault(seq, (payload, ts))
    out = []
    for (src, sport, dst, dport), info in streams.items():
        buf = b""
        seg_map = []  # (buf_start, buf_end, 包时间），用来给每条消息定时间戳
        expect = None
        gap = False
        for seq in sorted(info["segs"]):
            payload, ts = info["segs"][seq]
            if expect is not None and seq > expect:
                gap = True
                break
            if expect is None:
                buf += payload
                seg_map.append((0, len(payload), ts))
                expect = seq + len(payload)
            elif seq + len(payload) > expect:
                start = len(buf)
                buf += payload[expect - seq:]
                seg_map.append((start, len(buf), ts))
                expect = seq + len(payload)
        out.append((info["ts"], src, sport, dst, dport, buf, seg_map, gap))
    out.sort(key=lambda x: x[0])
    return out


def ts_at(seg_map, off):
    """字节偏移 off 所在段的包时间；找不到就用第一段的时间。"""
    for start, end, ts in seg_map:
        if start <= off < end:
            return ts
    return seg_map[0][2] if seg_map else 0.0


def parse_http_messages(buf, seg_map):
    """从一条 TCP 流里切出 HTTP 消息，返回 [(时间, 方法或状态码, URI或'', body)]。"""
    msgs = []
    off = 0
    while off < len(buf):
        msg_ts = ts_at(seg_map, off)
        end = buf.find(b"\r\n\r\n", off)
        if end == -1:
            break
        head = buf[off:end].decode("latin-1")
        lines = head.split("\r\n")
        first = lines[0]
        headers = {}
        for ln in lines[1:]:
            if ":" in ln:
                k, v = ln.split(":", 1)
                headers[k.strip().lower()] = v.strip()
        body_start = end + 4
        if first.startswith("HTTP/"):
            kind = first.split(" ", 2)[1]  # 状态码
            uri = ""
        else:
            parts = first.split(" ", 2)
            kind = parts[0] if parts else ""  # 方法
            uri = parts[1] if len(parts) > 1 else ""
        if headers.get("transfer-encoding") == "chunked":
            body, consumed = b"", body_start
            while True:
                eol = buf.find(b"\r\n", consumed)
                if eol == -1:
                    break
                try:
                    n = int(buf[consumed:eol].decode("latin-1").strip(), 16)
                except ValueError:
                    break
                consumed = eol + 2
                if n == 0:
                    consumed += 2
                    break
                body += buf[consumed:consumed + n]
                consumed += n + 2
            msgs.append((msg_ts, kind, uri, body))
            off = consumed
            continue
        n = int(headers.get("content-length", "0") or "0")
        body = buf[body_start:body_start + n]
        msgs.append((msg_ts, kind, uri, body))
        off = body_start + n
    return msgs


def redact(uri):
    return re.sub(r"pwd=[^&]*", "pwd=***", uri)


def main(argv):
    show_bodies = "--bodies" in argv
    args = [a for a in argv if not a.startswith("-")]
    if len(args) != 1:
        print("用法：pcap-flow.py [--bodies] <pcap 文件>", file=sys.stderr)
        return 1
    path = args[0]
    if not os.path.isfile(path):
        print(f"找不到文件：{path}", file=sys.stderr)
        return 1
    try:
        linktype, frames = read_pcap(path)
    except ValueError as e:
        print(f"读 pcap 失败：{e}", file=sys.stderr)
        return 1
    if linktype not in (LINKTYPE_ETHERNET, LINKTYPE_RAW, LINKTYPE_RAW2, LINKTYPE_LINUX_SLL):
        print(f"不支持的链路层类型：{linktype}（只支持 Ethernet/Raw/Linux SLL）", file=sys.stderr)
        return 1

    events = []  # (时间, 方向, 方法/状态码, URI, body)
    counts = Counter()
    gaps = 0
    for ts, src, sport, dst, dport, buf, seg_map, gap in parse_tcp_payloads(linktype, frames):
        if gap:
            gaps += 1
        for msg_ts, kind, uri, body in parse_http_messages(buf, seg_map):
            if uri:  # 请求
                if dport == UPF_PORT:
                    direction, label = "CP  -> UPF", kind
                elif dport == CB_PORT:
                    direction, label = "UPF -> CP  (callback)", kind
                elif dport == OAM_PORT:
                    direction, label = "OAM -> CP", kind
                else:
                    direction, label = "?   -> ?", kind
                events.append((msg_ts, direction, label, redact(uri), body))
                counts[uri.split("?", 1)[0]] += 1
            else:  # 响应
                if sport == UPF_PORT:
                    direction = "UPF -> CP"
                elif sport == CB_PORT:
                    direction = "CP  -> UPF  (callback reply)"
                elif sport == OAM_PORT:
                    direction = "CP  -> OAM"
                else:
                    direction = "?   -> ?"
                events.append((msg_ts, direction, kind, "", b""))

    events.sort(key=lambda e: e[0])
    if not events:
        print("这个 pcap 里没有 HTTP 包。")
        print("可能原因：1) 抓包时端口不对；2) 走的是 HTTPS（真 UPF），包是加密的——")
        print("   启动 CP 前设置 SSLKEYLOGFILE=/tmp/sslkey.log，再用 tshark -o tls.keylog_file:/tmp/sslkey.log 解。")
        return 0
    for i, (ts, direction, label, uri, body) in enumerate(events, 1):
        if uri:
            print(f"[{ts:7.3f}s] #{i} {direction}  {label} {uri}")
            if show_bodies and body and direction.startswith(("UPF -> CP  (callback)", "OAM -> CP")):
                print(f"           body: {body[:300].decode('utf-8', 'replace')}")
        else:
            print(f"[{ts:7.3f}s] #{i} {direction}  {label}")
    print()
    print("--- 按接口统计（请求）---")
    for p, c in sorted(counts.items()):
        print(f"  {p:<40} {c} 次")
    print(f"  共 {len(events)} 个 HTTP 消息")
    if gaps:
        print(f"  注意：{gaps} 条流有 TCP 空洞（丢包），时间线可能不完整")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
