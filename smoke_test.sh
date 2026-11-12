#!/bin/bash
# nat_cp 本地联调冒烟脚本：mock UPF/UP + CP 全流程验证
set -u
cd "$(dirname "$0")"
export PATH=$HOME/go-sdk/bin:$PATH
go build -o /tmp/mock ./main/mock
go build -o /tmp/cp ./main
rm -f /tmp/cp-test.db* /tmp/mock-up.sock

/tmp/mock > /tmp/mock.log 2>&1 &
MOCK_PID=$!
/tmp/cp -config main/conf/cp-test.yaml > /tmp/cp.log 2>&1 &
CP_PID=$!
trap 'kill $MOCK_PID $CP_PID 2>/dev/null; echo "--- cp-test.log tail (CP 文件日志) ---"; tail -30 /tmp/cp-test.log' EXIT
sleep 3

oam() { curl -s -X POST http://127.0.0.1:10001/cp/v1/common/call -d "$1"; echo; }
uprules() { curl -s http://127.0.0.1:18153/__mock/up/rules; echo; }

echo "== 1. OAM 下发 ToH 配置（area1），触发定向全量补偿 =="
oam '{"uuid":"t1","command":"add_vxlan","reqArgs":{"id":1,"vniBegin":100,"vniEnd":199,"localVtepIp":"172.16.123.100","remoteVtepIp":"10.88.123.100","localOverlayMac":"20:88:10:91:15:22","remoteOverlayMac":"86:1f:ac:b1:79:8a","srcNatAddr":"10.10.0.0/24"}}'
oam '{"uuid":"t2","command":"add_vxlan_imsi","reqArgs":{"id":2,"imsi":"460001111111111","dnn":"area1","vni":100}}'
sleep 3
echo "-- UP rules after area1 compensation (expect 20.20.1.1 TOH):"
uprules

echo "== 2. OAM 下发 ToB 配置（area2），触发定向全量补偿 =="
oam '{"uuid":"t3","command":"add_fttrb","reqArgs":{"id":3,"fttrbId":"FTTRB-1","srcNatAddr":"10.10.1.0/24"}}'
oam '{"uuid":"t4","command":"add_fttrb_imsi","reqArgs":{"id":4,"imsi":"460002222222222","dnn":"area2","fttrbId":"FTTRB-1"}}'
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

echo "== 5. 重启 CP：启动应以 RULE_SYNC SNAPSHOT 恢复规则 =="
kill $CP_PID; sleep 1
/tmp/cp -config main/conf/cp-test.yaml > /tmp/cp.log 2>&1 &
CP_PID=$!
sleep 4
echo "-- state after restart (expect active:true, desired=2, installed=2):"
curl -s http://127.0.0.1:18080/debug/state; echo
echo "-- UP rules after restart (expect 20.20.1.1 TOH + 20.20.2.1 TOB):"
uprules

echo "== 6. 新 OAM 接口：分页查询 + 按 id 删除 =="
echo "-- list_vxlan (expect total 1, record id 1):"
oam '{"uuid":"q1","command":"list_vxlan","reqArgs":[0,10]}'
echo "-- list_vxlan_imsi (expect total 1, record id 2):"
oam '{"uuid":"q2","command":"list_vxlan_imsi","reqArgs":{"offset":0,"limit":10}}'
echo "-- del_vxlan_imsi [2] (expect 删除成功):"
oam '{"uuid":"d1","command":"del_vxlan_imsi","reqArgs":[2]}'
sleep 2
echo "-- UP rules after del binding (expect only 20.20.2.1 TOB):"
uprules
