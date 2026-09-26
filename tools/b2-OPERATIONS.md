# B2 真机验收操作单（一键）

> 状态：**本文件的命令尚未执行过**。脚本已上传到测试机并在下面列出；执行后按第 4 节回帖结论。
> 在此之前，B2 **不得**被标记为"通过"，也不得写"回滚成功并验证通过"。

## 1. 前置：已经在服务器上的东西

上传目标：`root@154.9.235.57:23722`

| 服务器路径 | 是什么 | 校验 |
|---|---|---|
| `/tmp/shengyu-edgelink-v0.3.4` | 修好两个缺陷的管理面二进制（本地 `dist/shengyu-edgelink-linux-amd64`） | sha256 `82338df51320088630c1f3ec3ba82a9bb5890f7cfaab3271fd404ab4b2df0aa2`，10793112 字节 |
| `/tmp/b2-scripts/b2-00-prep.sh` | 侦察 + 全量备份 | 6787 字节 |
| `/tmp/b2-scripts/b2-01-deploy.sh` | 部署 v0.3.4（只换管理面二进制） | 3352 字节 |
| `/tmp/b2-scripts/b2-02-defect2.sh` | 缺陷二：陈旧辅助文件清理 | 10302 字节 |
| `/tmp/b2-scripts/b2-03-defect1.sh` | 缺陷一：B2（reload 只失败一次） | 10948 字节 |
| `/tmp/b2-scripts/b2-04-notverified.sh` | 中间态：已恢复但未验证 / 回滚验证失败 | 6586 字节 |
| `/tmp/b2-scripts/b2-05-cleanup.sh` | 清理临时措施 + 恢复原运行版本 | 7023 字节 |
| `/tmp/b2-scripts/b2-99-all.sh` | 上面 6 步的顺序串联 | 2009 字节 |

如果这些文件不在（机器重装过等），先在**本地** Windows 上执行：

```powershell
powershell -NoProfile -ExecutionPolicy Bypass -File C:\b2tools\go.ps1 -Action upload
```

（`C:\b2tools\go.ps1` 必须是纯 ASCII；Windows PowerShell 5.1 会把无 BOM 的 UTF-8 当 GBK 读，
中文注释会让它直接解析失败。另外 `ssh` 用小写 `-p` 传端口，`scp` 必须用大写 `-P`，
写错会看到 `stat local "23722": No such file or directory`。）

## 2. 一条命令跑完全部验收

在服务器上（root）执行：

```bash
bash /tmp/b2-scripts/b2-99-all.sh 2>&1 | tee /root/b2-accept.log
```

它会按顺序做：

1. **00 侦察 + 备份** → 新目录 `/root/b2-accept-<UTC时间戳>/`，含
   `/etc/shengyu-edgelink` 全量副本 + tar、两个 unit、drop-in 目录、polkit 规则、
   当前二进制、meta.db 副本、`facts-before.txt`。同时打印：当前 VERSION、`current/` 文件清单、
   每个版本目录的文件清单（含 manifest.json）、统计套接字的存活探测、端口、8443 握手、meta。
2. **01 部署 v0.3.4** → 校验 sha256（不一致就中止）→ 原子替换 → **只重启管理面** →
   断言 HAProxy master PID 未变 → 健康检查 / 页面 / 8443 握手 / reload 自测。
3. **02 缺陷二** → 建 9443 SNI 入口 + 测试业务 → 发布（此时 current/ 应有 `sni_allow_9443.lst`）
   → 删业务 → 改库删 9443 入口（先备份 meta.db）→ 再发布 →
   校验：`sni_allow_9443.lst` 已消失、`current/` 文件集合与目标版本目录**完全一致**、
   逐文件 sha256 相同、`current/` 里没有"不在清单内"的残留。
4. **03 缺陷一（B2）** → 装临时 reload 失败注入器 + systemd drop-in →
   先自测注入器（mode=always 必失败、mode=once 无 flag 必成功）→ arm flag → 改业务 → 发布 →
   HAProxy reload 只失败一次 → 自动回滚 → 打印**报告要求的七项**：
   nodeID 实际值 / Verify 是否实际执行（`rollback_verify_ran`）/ `rollback_verified` /
   `current/VERSION` / `current/` 文件清单 / 旧 `sni_allow_9443.lst` 是否已删除 / 8443 是否仍正常，
   外加 `rollback_outcome`、`rollback_code`、`rollback_note`、四条判据、`manual_fix` 行数。
   然后做负向验证：向**别的** nodeID 发布必须被拒（409 `remote_node_unsupported`）。
5. **04 中间态** → 4A：把回滚目标的 `stats-vN.sock` chmod 000 ⇒ 期望 `verify_failed`
   （**这是"Verify 没有被跳过"最硬的现场证据：只有它真在执行，结果才会随套接字可读性变化**）；
   4B：把回滚目标的 `state.json` 移走 ⇒ 期望 `restored_not_verified`
   （code = `rollback_verification_not_run`）。两者都必须给人工命令、且都不得出现"并验证通过"。
6. **05 清理 + 恢复** → 撤 drop-in、撤注入器、删 flag、`daemon-reload` 并确认 `ExecReload`
   回到 `/bin/kill -USR2 $MAINPID`；删验收造的客户/业务/9443 入口；
   回滚到验收前的版本；**逐字节**比对 `current/` 与验收前备份（文件清单 + 每个文件 sha256）；
   最后 `haproxy -c`、两个 unit、端口表、8443 握手、9443 应已关闭、页面与 meta。

## 3. 中途出问题怎么办

每个脚本都是 `set -u` 而不是 `set -e`，所以某个检查失败不会中断后续步骤，日志里会留下证据。
如果卡住不动（例如某个步骤在等 30 秒就绪超时），按顺序看：

```bash
tail -50 /root/b2-accept.log
systemctl status shengyu-edgelink-server shengyu-edgelink-haproxy --no-pager
cat /etc/shengyu-edgelink/haproxy/current/VERSION
ls -la /etc/shengyu-edgelink/haproxy/current/
```

**无论走到哪一步，收尾必须把下面两件事做掉**（05 会做，但手工中断时请自己确认）：

```bash
rm -f /etc/systemd/system/shengyu-edgelink-haproxy.service.d/10-b2-reload-inject.conf
rmdir /etc/systemd/system/shengyu-edgelink-haproxy.service.d 2>/dev/null
rm -f /usr/local/bin/rbui-reload-inject.sh /run/shengyu-edgelink/rbui-reload-* 
systemctl daemon-reload
systemctl show -p ExecReload --value shengyu-edgelink-haproxy   # 必须是 /bin/kill -USR2 $MAINPID
```

## 4. 需要回帖的内容

跑完后，把这一段贴回来即可（结论行 + 03 阶段的原始字段）：

```bash
grep -E 'B2_VERDICT|SCENARIO_4A|SCENARIO_4B|CHECK_9443|CHECK_FILESET|CHECK_VERSION_NOT_ADVANCED|BYTE_IDENTICAL|FILESET_SAME|DEPLOY_DONE|CLEANUP_DONE' /root/b2-accept.log
sed -n '/B2：arm flag/,/B2_STEP_DONE/p' /root/b2-accept.log
```

如果日志太大，也可以直接把 `/root/b2-accept.log` 传回本地，我来逐条核对。

## 5. 报告里会被写死的事实（供你核对口径）

- `nodeID 实际值`：取自 `/api/nodes` 的第一条，03 阶段会原样打印。
- `Verify 是否实际执行`：以 `rollback_verify_ran` 为准，并用 4A 场景（socket 不可读 → 结论翻成
  `verify_failed`）作为**行为层面的反证**。
- `rollback_verified`：只有四条判据（文件集合 / 版本标记 / 统计套接字 / 监听归属）全成立才为 true。
- `current/VERSION`：失败版本**不得**被留在 current/ 上（必须等于失败前的版本号）。
- `current/ 文件清单`：`ls -la current/` + `current/manifest.json` 的 `files` 字段 + 与版本目录的
  `diff -r` 与逐文件 sha256。
- `旧 sni_allow_9443.lst 是否已删除`：以 `ls` 为准，不以 JSON 为准。
- `8443 业务是否仍正常`：`openssl s_client -connect 127.0.0.1:8443 -servername verify.example.com -brief`
  且 HAProxy master PID 全程未变。

## 6. 已知偏差（必须进报告，不许省略）

1. **SNI 入口没有删除接口**：9443 入口是**改库**删的（先备份 meta.db）。这也是本轮验收会顺带
   核实的事实之一 —— 产品侧缺这个接口。
2. **SNI 入口的创建时间无法证明"验收前就存在"**：`sni_entries` 表没有 `updated_at`，
   上一轮验收留下的时间线证据只能说明"8443 入口早于既有业务创建"，不能反推它一定先于本轮验收。
3. 上一轮（v0.3.1）的真机结论**不适用于本轮**：v0.3.1 的 B2 是"报成功但没验证"，
   本轮要用 v0.3.4 重跑，以新日志为准。

---

# 第二轮（v0.3.5）：针对第一轮真机暴露的问题

第一轮（v0.3.4）已经跑过，结果在 `docs/08-acceptance-linux.md` 第 6、7 节。
第二轮只验新修的两处 + 把"五种回滚结论"补齐。

## 已在服务器上

| 路径 | 说明 | 校验 |
|---|---|---|
| `/tmp/shengyu-edgelink-v0.3.5` | 修好"空入口假失败"与"回滚失败告警"的二进制 | sha256 `fec0c54f19d3f08606cc26679592a8e0c962989d7d9334b190a7eadc28c45dc3` |
| `/tmp/b2-scripts/b2-10-deploy35.sh` | 部署 v0.3.5（只换管理面二进制） | |
| `/tmp/b2-scripts/b2-11-fixes.sh` | S1 空 SNI 入口 + S2 缺陷二复验 | 会先抓"改动前快照" |
| `/tmp/b2-scripts/b2-12-rollback.sh` | S3–S8 五种回滚结论 | S4 会等满 30 秒 |
| `/tmp/b2-scripts/b2-13-cleanup.sh` | 清理临时措施 + 恢复改动前版本（逐字节比对） | |
| `/tmp/b2-scripts/b2-99c-second.sh` | 上面四步的顺序串联 | |

## 一条命令

```bash
bash /tmp/b2-scripts/b2-99c-second.sh 2>&1 | tee /root/b2-accept2.log
```

## 需要回帖的内容

```bash
grep -E '^(S[3-8]=|CHECK_|FILESET_SAME|BYTE_IDENTICAL|VERSION_SAME|DEPLOY35_DONE|CLEANUP2_DONE)' /root/b2-accept2.log
sed -n '/S4：reload 返回 0/,/S5：/p' /root/b2-accept2.log
sed -n '/S7：回滚目标目录不完整/,/S8：/p' /root/b2-accept2.log
```

## 记住两件事

1. **S4 会长**：它故意让 reload 变成"返回 0 但什么都不做"，`waitVerify` 要轮询等满 30 秒
   才判失败，之后才走真正的回滚。看到卡住别打断。
2. **S7 会故意把 `current/` 留在未获批准的新版本上**（这就是它要验的东西：告警有没有出现）。
   `b2-13-cleanup.sh` 会把它清干净，**但如果你中途 Ctrl-C，请手工补跑一次 `b2-13-cleanup.sh`**。

---

# 第五轮（v0.3.7）：清单外文件的处理边界

**已执行完毕**（真机 154.9.235.57:23722），结果见 `docs/08-acceptance-linux.md` §10.4，日志 `/root/b2-accept5.log`。

一条命令（若需重跑）：

```bash
bash /tmp/b2-scripts/b2-99g-final.sh 2>&1 | tee /root/b2-accept5.log
```

| 场景 | 构造方式 | 期望 |
|---|---|---|
| S1 | 用 `install -d -o shengyu -g shengyu` 预置 `v<NEXT>/haproxy.cfg.orig`，再改业务发布 | 发布**成功** + `warnings` 点名该文件 + 审计有 `config.version_dir_extra_files` |
| S1c | `GET /` | 页面含「存在未纳入清单的额外文件」 |
| S4 | 比对 `current/` 与目标版本清单 | 完全一致，多余文件**不在** `current/` 里 |
| S2a | 移走**被 cfg 引用**的 `sni_allow_*.lst` | 被拒（HAProxy 语法检查先拦，属"第一道防线"） |
| S2b | 移走**不被引用**的 `state.json` | 被拒，原因必须提到「版本目录不完整」+ 点名 `state.json` |
| S3 | 给基线版本 cfg 注入指向 `extra_allow.lst` 的 `-f` 引用（该文件不进清单） | 被拒，原因点名该文件 + 「不在清单」 |

`S3` 之前有一条**单独验证** `haproxy -c` 的命令输出 `cfg_syntax_after_inject=VALID` ——
它是用来排除"拒绝其实是语法错造成的"这种假阳性，别删。

**重跑注意**：脚本会临时改坏基线版本的 `haproxy.cfg`（先备份、收尾逐字节还原），
并在版本目录里临时移动文件；中途中断请手工核对 `CFG_RESTORED` 与 `find $CR/versions -name '*.orig'`。

---

# 第六轮（v0.4.0 正式版）：删除接口 / 悬空引用 / socket 清理 / 摄入可见

**已执行完毕**（真机 154.9.235.57:23722），结果见 `docs/08-acceptance-linux.md` §11，日志 `/root/b2-accept6.log`。

```bash
bash /tmp/b2-scripts/b2-99h-final.sh 2>&1 | tee /root/b2-accept6.log
```

| 场景 | 构造方式 | 期望 |
|---|---|---|
| S3 | 建 9443 入口 + 建业务挂上去，然后删入口 | **409 `sni_entry_in_use`**，报错点名业务，入口原样保留 |
| S5 | 删业务（级联删路由）后再删入口 | 200 + `next_step` 要求发布 + 审计 `sni_entry.delete` |
| S6a/b/c | 先查诊断（干净）→ 改库删入口（造悬空）→ 查诊断 → 删业务 → 再查诊断 | 干净时不报；造出后报 `dangling_reference`(error)；修好后消失 |
| S7 | 改业务端口后发布 | 成功，`stats_socket_prune` 显示 removed/kept，审计 `config.prune_stats_sockets` |
| S8 | 把业务端口改回 | 成功 |
| S9 | `GET /api/overview` | 含 `log_ingest`（ingested/dropped/…） |

**S1–S6 全程不发布**，只在期望状态里建/删入口与业务 ⇒ 9443 端口**从未被真正绑定**，对线上 8443 零影响。
唯一会发布的是 S7/S8（改业务端口再改回），收尾再回滚到基线版本。

**注意**：S6b 会**故意改库**造一条悬空引用 —— 这是为了验证诊断页能发现"绕过守卫的写操作"留下的脏数据。
它必须在 S6c 被清掉（删业务即级联删路由），否则后续发布会一直被 `validate` 拒成 422。
若中途中断，请手工核对：`select count(*) from routes r left join sni_entries e on e.id=r.sni_entry_id where r.sni_entry_id<>'' and e.id is null` 应为 0。
