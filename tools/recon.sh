#!/usr/bin/env bash
# read-only recon
echo "===== 1. units ====="
systemctl is-active shengyu-edgelink-server shengyu-edgelink-haproxy 2>&1
systemctl is-enabled shengyu-edgelink-server shengyu-edgelink-haproxy 2>&1
echo "----- server unit -----"
cat /etc/systemd/system/shengyu-edgelink-server.service 2>&1
echo "----- haproxy unit -----"
cat /etc/systemd/system/shengyu-edgelink-haproxy.service 2>&1
echo "===== 2. drop-ins ====="
ls -la /etc/systemd/system/shengyu-edgelink-haproxy.service.d/ 2>&1
ls -la /etc/systemd/system/shengyu-edgelink-server.service.d/ 2>&1
echo "===== 3. haproxy runtime ====="
haproxy -vv 2>&1 | head -3
ps -o user=,pid=,ppid=,args= -C haproxy 2>&1
echo "===== 4. tree ====="
ls -la /etc/shengyu-edgelink/haproxy/ 2>&1
echo "----- current -----"
ls -la /etc/shengyu-edgelink/haproxy/current/ 2>&1
echo "VERSION=$(cat /etc/shengyu-edgelink/haproxy/current/VERSION 2>&1)"
sha256sum /etc/shengyu-edgelink/haproxy/current/haproxy.cfg 2>&1
echo "----- versions -----"
find /etc/shengyu-edgelink/haproxy/versions -maxdepth 3 2>&1 | sort
echo "===== 5. ports ====="
ss -lntp 2>&1 | sort -k4
echo "===== 6. proc net tcp (non-loopback listen) ====="
cat /proc/net/tcp | awk 'NR==1 || $4=="0A"' 2>&1
echo "----- tcp6 -----"
cat /proc/net/tcp6 | awk 'NR==1 || $4=="0A"' 2>&1
echo "===== 7. binary ====="
ls -la /usr/local/bin/shengyu-edgelink 2>&1
/usr/local/bin/shengyu-edgelink -version 2>&1
echo "===== 8. health/meta ====="
curl -s -m 5 -o /tmp/r_health.json -w 'health_http=%{http_code}\n' http://127.0.0.1:8081/api/health
cat /tmp/r_health.json; echo
curl -s -m 5 -o /tmp/r_meta.json -w 'meta_http=%{http_code}\n' http://127.0.0.1:8081/api/meta
cat /tmp/r_meta.json; echo
echo "===== 9. install log tail ====="
ls -la /root/shengyu-install.log 2>&1
tail -5 /root/shengyu-install.log 2>&1
echo "===== 10. current cfg ====="
cat /etc/shengyu-edgelink/haproxy/current/haproxy.cfg 2>&1
echo "===== 11. resources ====="
nproc; free -m; df -h / /var/lib/shengyu-edgelink 2>&1
echo "===== RECON_DONE ====="
