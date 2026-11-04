#!/bin/bash
# nat_cp 本地联调冒烟脚本：mock UPF/UP + CP 全流程验证
set -u
cd ~/workspace/nat_cp
export PATH=$HOME/go-sdk/bin:$PATH
go build -o /tmp/mock ./cmd/mock
go build -o /tmp/cp ./cmd/cp
rm -f /tmp/cp-test.db* /tmp/mock-up.sock

/tmp/mock > /tmp/mock.log 2>&1 &
MOCK_PID=$!
/tmp/cp -config configs/cp-test.yaml > /tmp/cp.log 2>&1 &
CP_PID=$!
trap 'kill $MOCK_PID $CP_PID 2>/dev/null; echo "--- cp.log tail ---"; tail -30 /tmp/cp.log' EXIT
sleep 3

oam() { curl -s -X POST http://127.0.0.1:10001/cp/v1/common/call -d "$1"; echo; }
uprules() { curl -s http://127.0.0.1:18153/__mock/up/rules; echo; }

echo "== 1. OAM 下发 ToH 配置（area1），触发定向全量补偿 =="
oam '{"uuid":"t1","command":"add_vxlan","reqArgs":{"id":"vx1","vniBegin":100,"vniEnd":199,"localVtepIp":"172.16.123.100","remoteVtepIp":"10.88.123.100","localOverlayMac":"20:88:10:91:15:22","remoteOverlayMac":"86:1f:ac:b1:79:8a","srcNatAddr":"10.10.0.0/24"}}'
oam '{"uuid":"t2","command":"add_vxlan_imsi","reqArgs":{"id":"b1","imsi":"460001111111111","dnn":"area1","vni":100}}'
sleep 3
echo "-- UP rules after area1 compensation (expect 20.20.1.1 TOH):"
uprules

echo "== 2. OAM 下发 ToB 配置（area2），触发定向全量补偿 =="
oam '{"uuid":"t3","command":"add_fttrb","reqArgs":{"id":"f1","fttrbId":"FTTRB-1","srcNatAddr":"10.10.1.0/24"}}'
oam '{"uuid":"t4","command":"add_fttrb_imsi","reqArgs":{"id":"b2","imsi":"460002222222222","dnn":"area2","fttrbId":"FTTRB-1"}}'
sleep 3
echo "-- UP rules after area2 compensation (expect 20.20.1.1 + 20.20.2.1):"
uprules

echo "== 3. 模拟 UPF 推送 area1 UE 下线 =="
curl -s -X POST http://127.0.0.1:18153/__mock/trigger -d \
 '{"action":"delete","imsi":"460001111111111","dnn":"area1","msisdn":"13800000001","privateIpv4":"20.20.1.1"}'; echo
sleep 2
echo "-- UP rules after delete (expect only 20.20.2.1 TOB):"
uprules

echo "== 4. CP 运行态 + 手动批量同步 =="
curl -s http://127.0.0.1:18080/debug/state; echo
curl -s -X POST http://127.0.0.1:18080/debug/snapshot-sync; echo
sleep 1
echo "-- UP rules after snapshot-sync (expect 20.20.2.1 TOB):"
uprules
