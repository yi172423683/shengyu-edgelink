package store

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/shengyu/edgelink/internal/auth"
	"github.com/shengyu/edgelink/internal/model"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func mustCustomer(t *testing.T, s *Store, id, name string) *model.Customer {
	t.Helper()
	c := &model.Customer{ID: id, Name: name, Enabled: true}
	if err := s.CreateCustomer(c); err != nil {
		t.Fatalf("创建客户失败: %v", err)
	}
	return c
}

func mustNode(t *testing.T, s *Store, id, name string) *model.Node {
	t.Helper()
	n := &model.Node{ID: id, Name: name, Enabled: true, PublicIPv4: "203.0.113.10"}
	if err := s.CreateNode(n); err != nil {
		t.Fatalf("创建节点失败: %v", err)
	}
	return n
}

func TestSchemaBootstrapsAndIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "meta.db")
	for i := 0; i < 2; i++ { // 第二次打开必须幂等（生产上就是重启）
		s, err := Open(path)
		if err != nil {
			t.Fatalf("第 %d 次打开失败: %v", i+1, err)
		}
		v, err := s.SchemaVersion()
		if err != nil || v != schemaVersion {
			t.Fatalf("schema 版本异常: %q %v", v, err)
		}
		_ = s.Close()
	}
}

func TestCustomerLifecycleAndDeleteGuard(t *testing.T) {
	s := newTestStore(t)
	c := mustCustomer(t, s, "cus_1", "示例客户")

	got, err := s.GetCustomer("cus_1")
	if err != nil || got.Name != "示例客户" {
		t.Fatalf("读取客户失败: %+v %v", got, err)
	}

	// 名下有业务时禁止删客户：静默级联删除会让人误以为转发还在。
	b := &model.Business{ID: "biz_1", CustomerID: c.ID, Name: "站点A", Mode: model.ModeSNITLS, Enabled: true}
	if err := s.CreateBusiness(b); err != nil {
		t.Fatalf("创建业务失败: %v", err)
	}
	if err := s.DeleteCustomer("cus_1"); !IsConflict(err) {
		t.Fatalf("有业务时删除客户应返回冲突，got %v", err)
	}

	if err := s.DeleteBusiness("biz_1"); err != nil {
		t.Fatalf("删除业务失败: %v", err)
	}
	if err := s.DeleteCustomer("cus_1"); err != nil {
		t.Fatalf("清空后删除客户应成功，got %v", err)
	}
	if _, err := s.GetCustomer("cus_1"); !IsNotFound(err) {
		t.Fatalf("删除后应找不到，got %v", err)
	}
}

func TestTCPEntryUniquePerNode(t *testing.T) {
	s := newTestStore(t)
	mustCustomer(t, s, "cus_1", "客户")
	mustNode(t, s, "node_a", "节点A")

	b1 := &model.Business{ID: "biz_1", CustomerID: "cus_1", Name: "B1", Mode: model.ModeTCPPort, Enabled: true}
	b2 := &model.Business{ID: "biz_2", CustomerID: "cus_1", Name: "B2", Mode: model.ModeTCPPort, Enabled: true}
	for _, b := range []*model.Business{b1, b2} {
		if err := s.CreateBusiness(b); err != nil {
			t.Fatalf("创建业务失败: %v", err)
		}
	}
	r1 := &model.Route{ID: "rt_1", BusinessID: "biz_1", NodeID: "node_a", Role: model.RolePrimary,
		Mode: model.ModeTCPPort, EntryAddr: "0.0.0.0", EntryPort: 25565,
		OriginHost: "198.51.100.7", OriginPort: 25565, Enabled: true}
	if err := s.CreateRoute(r1); err != nil {
		t.Fatalf("第一条 TCP 路由应成功: %v", err)
	}
	// 同一节点同一 入口:端口 被第二条 TCP 路由抢用 —— 必须在库层面挡住，
	// 否则两条业务会抢同一个监听，HAProxy reload 直接失败。
	r2 := &model.Route{ID: "rt_2", BusinessID: "biz_2", NodeID: "node_a", Role: model.RolePrimary,
		Mode: model.ModeTCPPort, EntryAddr: "0.0.0.0", EntryPort: 25565,
		OriginHost: "198.51.100.8", OriginPort: 25565, Enabled: true}
	err := s.CreateRoute(r2)
	if !IsConflict(err) {
		t.Fatalf("端口冲突应被识别为冲突，got %v", err)
	}
	// 错误信息必须能定位到具体入口，否则运维看到 409 不知道去改哪条业务。
	if !contains(err.Error(), "25565") || !contains(err.Error(), "入口") {
		t.Fatalf("冲突错误信息应指出被占用的入口，got %v", err)
	}

	// 不同节点上同名端口互不影响
	mustNode(t, s, "node_b", "节点B")
	r2.NodeID = "node_b"
	if err := s.CreateRoute(r2); err != nil {
		t.Fatalf("另一节点上同端口应允许: %v", err)
	}

	// 换成另一种 TCP 模式时同一入口仍冲突（索引条件 mode='tcp_port'）
	r2.ID = "rt_3"
	r2.NodeID = "node_a"
	if err := s.CreateRoute(r2); !IsConflict(err) {
		t.Fatalf("同节点重复 TCP 入口应冲突，got %v", err)
	}
}

func TestSNIRoutesShare443(t *testing.T) {
	s := newTestStore(t)
	mustCustomer(t, s, "cus_1", "客户")
	mustNode(t, s, "node_a", "节点A")
	if err := s.CreateSNIEntry(&model.SNIEntry{ID: "sni_1", NodeID: "node_a", BindAddr: "0.0.0.0", BindPort: 443, Enabled: true}); err != nil {
		t.Fatalf("创建 SNI 入口失败: %v", err)
	}
	for i, id := range []string{"biz_a", "biz_b"} {
		if err := s.CreateBusiness(&model.Business{ID: id, CustomerID: "cus_1", Name: id,
			Mode: model.ModeSNITLS, Enabled: true, Domains: []string{id + ".example.com"}}); err != nil {
			t.Fatalf("创建业务失败: %v", err)
		}
		r := &model.Route{ID: "rt_" + id, BusinessID: id, NodeID: "node_a", Role: model.RolePrimary,
			Mode: model.ModeSNITLS, SNIEntryID: "sni_1",
			OriginHost: "198.51.100." + string(rune('1'+i)), OriginPort: 443, Enabled: true}
		if err := s.CreateRoute(r); err != nil {
			t.Fatalf("SNI 路由应共享 443，第 %d 条失败: %v", i+1, err)
		}
	}
	// 同一业务在同一节点只能有一条路由
	err := s.CreateRoute(&model.Route{ID: "rt_dup", BusinessID: "biz_a", NodeID: "node_a",
		Role: model.RolePrimary, Mode: model.ModeSNITLS, SNIEntryID: "sni_1",
		OriginHost: "198.51.100.1", OriginPort: 443, Enabled: true})
	if !IsConflict(err) {
		t.Fatalf("同业务同节点重复路由应冲突，got %v", err)
	}
}

func TestVersionNumberIsMonotonicAndHashReusable(t *testing.T) {
	s := newTestStore(t)
	mustNode(t, s, "node_a", "节点A")

	v, err := s.NextVersion("node_a")
	if err != nil || v != 1 {
		t.Fatalf("首次版本号应为 1，got %d %v", v, err)
	}
	cv := &ConfigVersion{ID: "cv_1", NodeID: "node_a", Version: 1, ContentHash: "hash-a",
		Status: CfgActive, DirPath: "/etc/shengyu-edgelink/haproxy/v1", RouteCount: 2}
	if err := s.CreateConfigVersion(cv); err != nil {
		t.Fatalf("创建配置版本失败: %v", err)
	}
	v2, _ := s.NextVersion("node_a")
	if v2 != 2 {
		t.Fatalf("第二个版本号应为 2，got %d", v2)
	}

	// 内容 hash 相同 → 应被识别为"无需重新发布"
	found, err := s.FindConfigVersionByHash("node_a", "hash-a")
	if err != nil || found.Version != 1 {
		t.Fatalf("按 hash 查找失败: %+v %v", found, err)
	}
	if _, err := s.FindConfigVersionByHash("node_a", "hash-unknown"); !IsNotFound(err) {
		t.Fatalf("未知 hash 应返回 NotFound，got %v", err)
	}

	// 同节点同版本号不可重复插入
	if err := s.CreateConfigVersion(&ConfigVersion{ID: "cv_dup", NodeID: "node_a", Version: 1,
		ContentHash: "hash-b", Status: CfgCandidate}); !IsConflict(err) {
		t.Fatalf("重复版本号应冲突，got %v", err)
	}
}

func TestDesiredStateAssembly(t *testing.T) {
	s := newTestStore(t)
	mustCustomer(t, s, "cus_1", "客户")
	mustNode(t, s, "node_a", "节点A")
	if err := s.CreateSNIEntry(&model.SNIEntry{ID: "sni_1", NodeID: "node_a", BindAddr: "0.0.0.0", BindPort: 443, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	// 业务超时/限制 0 表示继承业务默认，路由级非 0 优先 —— 这条继承规则必须可测。
	b := &model.Business{ID: "biz_1", CustomerID: "cus_1", Name: "站点A", Mode: model.ModeSNITLS,
		Enabled: true, Domains: []string{"a.example.com"}, ConnectTimeoutMS: 3000, MaxConn: 100}
	if err := s.CreateBusiness(b); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateRoute(&model.Route{ID: "rt_1", BusinessID: "biz_1", NodeID: "node_a",
		Role: model.RolePrimary, Mode: model.ModeSNITLS, SNIEntryID: "sni_1",
		OriginHost: "198.51.100.1", OriginPort: 8443, MaxConn: 0, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	// 未启用的业务不应进入期望状态
	b2 := &model.Business{ID: "biz_2", CustomerID: "cus_1", Name: "站点B", Mode: model.ModeSNITLS,
		Enabled: false, Domains: []string{"b.example.com"}}
	if err := s.CreateBusiness(b2); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateRoute(&model.Route{ID: "rt_2", BusinessID: "biz_2", NodeID: "node_a",
		Role: model.RolePrimary, Mode: model.ModeSNITLS, SNIEntryID: "sni_1",
		OriginHost: "198.51.100.2", OriginPort: 8443, Enabled: true}); err != nil {
		t.Fatal(err)
	}

	st, err := s.DesiredStateForNode("node_a", 7)
	if err != nil {
		t.Fatalf("组装期望状态失败: %v", err)
	}
	if st.Version != 7 || len(st.Routes) != 2 || len(st.SNIEntries) != 1 {
		t.Fatalf("期望状态规模不符: ver=%d routes=%d entries=%d", st.Version, len(st.Routes), len(st.SNIEntries))
	}
	byID := map[string]model.RouteView{}
	for _, r := range st.Routes {
		byID[r.Route.ID] = r
	}
	if got := byID["rt_1"]; got.EffectiveMaxConn != 100 || got.ConnectTimeoutMS != 3000 || !got.Enabled {
		t.Fatalf("rt_1 继承字段不对: %+v", got)
	}
	if got := byID["rt_2"]; got.Enabled {
		t.Fatal("业务未启用时路由必须视为未启用（否则停用业务还会继续转发）")
	}
}

func TestAgentTokenOneTimeUse(t *testing.T) {
	s := newTestStore(t)
	mustNode(t, s, "node_a", "节点A")
	tok, err := s.CreateAgentToken("node_a", "首次注册", time.Hour)
	if err != nil {
		t.Fatalf("创建注册令牌失败: %v", err)
	}
	nodeID, err := s.ConsumeAgentToken(tok.Token)
	if err != nil || nodeID != "node_a" {
		t.Fatalf("首次消费应成功: %q %v", nodeID, err)
	}
	if _, err := s.ConsumeAgentToken(tok.Token); err == nil {
		t.Fatal("注册令牌必须一次性，第二次消费应失败")
	}

	// 并发消费：只能有一个赢
	tok2, _ := s.CreateAgentToken("node_a", "并发测试", time.Hour)
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.ConsumeAgentToken(tok2.Token); err == nil {
				mu.Lock()
				ok++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if ok != 1 {
		t.Fatalf("并发消费应恰好成功 1 次，实际 %d 次", ok)
	}

	// 过期令牌
	exp, _ := s.CreateAgentToken("node_a", "过期", -time.Second)
	if _, err := s.ConsumeAgentToken(exp.Token); err == nil {
		t.Fatal("过期令牌应被拒绝")
	}
	// 撤销令牌
	rev, _ := s.CreateAgentToken("node_a", "撤销", time.Hour)
	if err := s.RevokeAgentToken(rev.ID); err != nil {
		t.Fatalf("撤销失败: %v", err)
	}
	if _, err := s.ConsumeAgentToken(rev.Token); err == nil {
		t.Fatal("已撤销令牌应被拒绝")
	}
}

func TestSessionExpiryAndPasswordChangeRevokes(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC()
	clock := now
	s.SetClock(func() time.Time { return clock })

	u := &User{ID: "usr_1", Username: "admin", Enabled: true}
	if err := s.CreateUser(u, "S3cure-Passw0rd!", 500); err != nil {
		t.Fatalf("创建账号失败: %v", err)
	}
	// 账号枚举防护：不存在的用户与错口令返回同一错误
	if _, err := s.Authenticate("admin", "wrong"); err != auth.ErrMismatch {
		t.Fatalf("错误口令应返回 ErrMismatch，got %v", err)
	}
	if _, err := s.Authenticate("nobody", "whatever"); err != auth.ErrMismatch {
		t.Fatalf("不存在的用户也应返回 ErrMismatch，got %v", err)
	}
	if _, err := s.Authenticate("admin", "S3cure-Passw0rd!"); err != nil {
		t.Fatalf("正确口令应通过: %v", err)
	}

	sess, err := s.CreateSession("usr_1", "10.0.0.9", "test-agent", 2*time.Hour)
	if err != nil {
		t.Fatalf("创建会话失败: %v", err)
	}
	if _, err := s.LookupSession(sess.Token); err != nil {
		t.Fatalf("会话应有效: %v", err)
	}

	// 时间推进到过期之后
	clock = now.Add(3 * time.Hour)
	if _, err := s.LookupSession(sess.Token); !IsNotFound(err) {
		t.Fatalf("过期会话应无效，got %v", err)
	}

	// 改密必须吊销全部会话
	clock = now
	sess2, _ := s.CreateSession("usr_1", "10.0.0.9", "ua", time.Hour)
	if err := s.UpdateUserPassword("usr_1", "New-Passw0rd!", 500); err != nil {
		t.Fatalf("改密失败: %v", err)
	}
	if _, err := s.LookupSession(sess2.Token); !IsNotFound(err) {
		t.Fatal("改密后旧会话必须失效")
	}
	if _, err := s.Authenticate("admin", "New-Passw0rd!"); err != nil {
		t.Fatalf("新口令应可登录: %v", err)
	}
}

func TestAuditWriteAndPurge(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC()
	clock := now
	s.SetClock(func() time.Time { return clock })

	for i := 0; i < 5; i++ {
		if err := s.WriteAudit(&AuditEntry{ID: "a" + string(rune('0'+i)), Action: "business.update",
			Actor: "admin", TargetType: "business", TargetID: "biz_1", Summary: "改源站"}); err != nil {
			t.Fatalf("写审计失败: %v", err)
		}
	}
	clock = now.Add(100 * 24 * time.Hour)
	if err := s.WriteAudit(&AuditEntry{ID: "a_new", Action: "login", Actor: "admin"}); err != nil {
		t.Fatalf("写审计失败: %v", err)
	}
	rows, total, err := s.ListAudit(AuditFilter{})
	if err != nil || total != 6 || len(rows) != 6 {
		t.Fatalf("审计列表异常: total=%d len=%d err=%v", total, len(rows), err)
	}
	// 90 天保留期：只应清掉旧的那 5 条
	n, err := s.PurgeAuditBefore(clock.Add(-90 * 24 * time.Hour))
	if err != nil || n != 5 {
		t.Fatalf("清理审计应删 5 条，实际 %d，err=%v", n, err)
	}
	rows, total, _ = s.ListAudit(AuditFilter{})
	if total != 1 || rows[0].ID != "a_new" {
		t.Fatalf("清理后应只剩 1 条新记录，实际 total=%d", total)
	}
	// 按 action 过滤
	if _, total, _ := s.ListAudit(AuditFilter{Action: "login"}); total != 1 {
		t.Fatalf("按 action 过滤应命中 1 条，实际 %d", total)
	}
}

func TestMarkStaleNodes(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC()
	clock := now
	s.SetClock(func() time.Time { return clock })
	mustNode(t, s, "node_a", "节点A")

	if err := s.RecordHeartbeat(HeartbeatInput{NodeID: "node_a", AgentVersion: "0.1.0",
		AppliedVersion: 3, DataplaneOK: true}); err != nil {
		t.Fatalf("心跳失败: %v", err)
	}
	n, _ := s.GetNode("node_a")
	if n.Health != model.NodeOnline || n.AppliedVersion != 3 {
		t.Fatalf("心跳后状态不对: %+v", n)
	}

	// 心跳不该篡改期望版本 —— agent 无权自证"我就是你期望的那版"
	if n.ExpectedVersion != 0 {
		t.Fatalf("期望版本不应被心跳改写，got %d", n.ExpectedVersion)
	}

	// 数据面不健康 → 降级，而不是离线
	if err := s.RecordHeartbeat(HeartbeatInput{NodeID: "node_a", DataplaneOK: false}); err != nil {
		t.Fatal(err)
	}
	n, _ = s.GetNode("node_a")
	if n.Health != model.NodeDegraded {
		t.Fatalf("数据面异常应为 degraded，got %s", n.Health)
	}

	clock = now.Add(10 * time.Minute)
	cnt, err := s.MarkStaleNodes(3 * time.Minute)
	if err != nil || cnt != 1 {
		t.Fatalf("应标记 1 台离线，实际 %d err=%v", cnt, err)
	}
	n, _ = s.GetNode("node_a")
	if n.Health != model.NodeOffline {
		t.Fatalf("应变为 offline，got %s", n.Health)
	}
}

func TestReleaseLifecycle(t *testing.T) {
	s := newTestStore(t)
	mustNode(t, s, "node_a", "节点A")
	rel := &Release{ID: "rel_1", NodeID: "node_a", Action: ActionPublish,
		FromVersion: 1, ToVersion: 2, Status: ReleaseRunning, Actor: "admin"}
	if err := s.CreateRelease(rel); err != nil {
		t.Fatalf("创建发布记录失败: %v", err)
	}
	if err := s.UpdateReleasePhase("rel_1", "reload"); err != nil {
		t.Fatalf("更新阶段失败: %v", err)
	}
	if err := s.FinishRelease("rel_1", ReleaseAppliedUnhealthy, "verify",
		"配置已生效，但源站不可达", "2 条路由中 1 条源站连接超时"); err != nil {
		t.Fatalf("结束发布失败: %v", err)
	}
	got, err := s.LastRelease("node_a")
	if err != nil {
		t.Fatalf("读取最近发布失败: %v", err)
	}
	// "配置发布成功但源站不可用"必须能单独表达 —— 这正是需求 §五 要求区分的情况
	if got.Status != ReleaseAppliedUnhealthy || got.ToVersion != 2 {
		t.Fatalf("发布记录不符: %+v", got)
	}
	if got.FinishedAt.IsZero() {
		t.Fatal("结束时间未写入")
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
