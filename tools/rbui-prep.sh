#!/usr/bin/env bash
# 回滚 UI 验收 - 第 0 步：只读备份。不修改任何东西。
set -u
TS=$(date +%Y%m%d-%H%M%S)
B=/root/rollback-ui-accept-$TS
mkdir -p "$B"
echo "BACKUP_DIR=$B"

# 1) 配置目录整体（版本目录 + current）
tar -C /etc/shengyu-edgelink -czf "$B/etc-shengyu-edgelink.tar.gz" haproxy 2>&1
# 2) 状态目录（元数据库 + 日志分片）
tar -C /var/lib -czf "$B/var-lib-shengyu-edgelink.tar.gz" shengyu-edgelink 2>&1
# 3) systemd 与 polkit
cp -a /etc/systemd/system/shengyu-edgelink-server.service "$B/" 2>&1
cp -a /etc/systemd/system/shengyu-edgelink-haproxy.service "$B/" 2>&1
cp -a /etc/polkit-1/rules.d/50-shengyu-edgelink.rules "$B/" 2>&1
# 4) 二进制
cp -a /usr/local/bin/shengyu-edgelink "$B/shengyu-edgelink.v0.3.1.bin" 2>&1
# 5) 事实快照
{
  echo "### date"; date -u
  echo "### VERSION"; cat /etc/shengyu-edgelink/haproxy/current/VERSION
  echo "### sha256 current/haproxy.cfg"; sha256sum /etc/shengyu-edgelink/haproxy/current/haproxy.cfg
  echo "### sha256 current/*"; sha256sum /etc/shengyu-edgelink/haproxy/current/*
  echo "### versions tree"; find /etc/shengyu-edgelink/haproxy/versions -maxdepth 3 | sort
  echo "### binary version"; /usr/local/bin/shengyu-edgelink -version
  echo "### ps haproxy"; ps -o user=,pid=,ppid=,args= -C haproxy
  echo "### systemctl is-active"; systemctl is-active shengyu-edgelink-server shengyu-edgelink-haproxy
  echo "### ExecReload (effective)"; systemctl show -p ExecReload --value shengyu-edgelink-haproxy
  echo "### MainPID"; systemctl show -p MainPID --value shengyu-edgelink-haproxy
  echo "### drop-in dirs"; ls -la /etc/systemd/system/shengyu-edgelink-haproxy.service.d/ 2>&1
  echo "### stats sockets"; ls -la /run/shengyu-edgelink/
  echo "### listen ports"; ss -lntp | sort -k4
  echo "### free"; free -m
} > "$B/facts-before.txt" 2>&1

echo "---- facts-before.txt ----"
cat "$B/facts-before.txt"
echo "---- backup listing ----"
ls -la "$B"
echo "BACKUP_DONE"
