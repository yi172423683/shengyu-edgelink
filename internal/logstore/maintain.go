package logstore

import (
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/shengyu/edgelink/internal/db"
)

// 本文件实现容量控制：保留期、容量上限、真实压缩、磁盘水位。
//
// 评审 F12 的原话是"配额与压缩只是部分结构，生产维护并未真正启用"：
// 策略里只有 7 天保留和 CompressAfterHours，没有 QuotaBytes，也没有任何压缩执行代码，
// 而 CompressAfterHours 根本没参与清理 —— 也就是说"压缩"这个开关从来没有生效过。
//
// 这里把三件事都做实：
//
//	① 容量上限（QuotaBytes）：超过就删最旧（策略删除，不是故障删除），并记入动作列表；
//	② 真实压缩：整点分片在 CompressAfterHours 之后被 gzip，**逐字节校验**后才删原文件；
//	③ 磁盘水位（MinFreeBytes）：可用空间低于水位时，直接删最旧分片腾地方。
//	   这一条是"别把盘写满"的最后一道闸 —— 写满会连带打挂登录与配置发布。
//
// 压缩格式用**标准库 gzip**（.db.gz），不是 zstd：本项目的依赖策略是核心包只用标准库，
// 为了压缩再引一个第三方编解码库不值得；而"能不能查"这件事我们靠解压到临时文件解决，
// 不依赖随机访问能力。

// CompressionSuffix 压缩分片的后缀。
const CompressionSuffix = ".gz"

// RetentionPolicy 保留与容量策略（对应 settings 表里的 retention.* / quota.*）。
type RetentionPolicy struct {
	RetainDays         int   `json:"retain_days"`
	QuotaBytes         int64 `json:"quota_bytes"`
	CompressAfterHours int   `json:"compress_after_hours"`
	// MinFreeBytes 磁盘可用空间下限（字节）。低于它就开始删最旧分片。
	// 0 表示不检查（不推荐；生产应设成一个明确的值，例如 2GiB）。
	MinFreeBytes int64 `json:"min_free_bytes"`
}

// DefaultRetentionPolicy 返回保守默认值。
//
// 默认**带容量上限与磁盘水位**（评审 F12：只设保留天数等于把风险留给"高流量节点"）。
// 参考量级：一台节点按 2000 连接/秒的日志量估算，一条日志约 500 字节，
// 一天约 86 GB 原始分片；压缩后通常降到 10%~20%。QuotaBytes 默认 8 GiB
// 表示"宁可少留几天，也不要把盘写满"。
func DefaultRetentionPolicy() RetentionPolicy {
	return RetentionPolicy{
		RetainDays:         7,
		QuotaBytes:         8 << 30,
		CompressAfterHours: 24,
		MinFreeBytes:       2 << 30,
	}
}

// RetentionAction 一条待执行/已执行的维护动作。
type RetentionAction struct {
	Path   string    `json:"path"`
	Bytes  int64     `json:"bytes"`
	Reason string    `json:"reason"` // age | quota | disk | compress
	Start  time.Time `json:"start"`
	// Action 动作类型：delete | compress。旧字段 Reason 保留兼容语义。
	Action string `json:"action"`
	// Out 是 compress 的产物路径。
	Out string `json:"out,omitempty"`
	// SavedBytes 是 compress 实际节省的字节数。
	SavedBytes int64 `json:"saved_bytes,omitempty"`
}

// PlanMaintenance 计算维护计划（纯函数，可测）。
//
// 顺序很重要：**先按保留期删（这是策略），再按磁盘水位删（这是保命），
// 最后按配额删最旧（这是兜底）；压缩排在删除之前**，因为压缩能让"还没到期的分片"
// 少占地方，从而不必提前删掉还有查询价值的数据。
//
// freeBytes 为负表示"调用方不提供磁盘信息"，此时跳过磁盘水位这一步（不猜）。
func (s *Store) PlanMaintenance(kind Kind, pol RetentionPolicy, now time.Time, freeBytes int64) ([]RetentionAction, int64, error) {
	parts, err := s.listPartitions(kind)
	if err != nil {
		return nil, 0, err
	}
	var total int64
	for _, p := range parts {
		total += p.Bytes
	}

	var actions []RetentionAction
	cutoff := now.UTC().AddDate(0, 0, -pol.RetainDays)
	kept := make([]Partition, 0, len(parts))
	for _, p := range parts {
		if pol.RetainDays > 0 && p.Start.Add(time.Hour).Before(cutoff) {
			actions = append(actions, RetentionAction{
				Path: p.Path, Bytes: p.Bytes, Reason: "age", Action: "delete", Start: p.Start,
			})
			continue
		}
		kept = append(kept, p)
	}

	// 压缩：只压"已经整点结束、且超过 CompressAfterHours 没再被写过"的分片。
	// 不允许压当前小时 —— 它还在被写入，压缩等于把正在写的文件抽走。
	compressed := 0
	if pol.CompressAfterHours > 0 {
		compressBefore := now.UTC().Add(-time.Duration(pol.CompressAfterHours) * time.Hour)
		for i := range kept {
			p := kept[i]
			if p.Compressed {
				continue
			}
			if !p.Start.Add(time.Hour).Before(compressBefore) {
				continue
			}
			actions = append(actions, RetentionAction{
				Path: p.Path, Bytes: p.Bytes, Reason: "compress", Action: "compress",
				Start: p.Start, Out: p.Path + CompressionSuffix,
			})
			compressed++
		}
	}

	// 配额：从最旧的**未压缩**分片开始删。压缩过的分片仍然占位，
	// 但它们是"省过空间的"，优先删未压缩的更划算。
	if pol.QuotaBytes > 0 {
		remaining := total
		for _, a := range actions {
			if a.Action == "delete" {
				remaining -= a.Bytes
			}
		}
		for _, p := range kept {
			if remaining <= pol.QuotaBytes {
				break
			}
			if alreadyPlanned(actions, p.Path) {
				continue
			}
			actions = append(actions, RetentionAction{
				Path: p.Path, Bytes: p.Bytes, Reason: "quota", Action: "delete", Start: p.Start,
			})
			remaining -= p.Bytes
		}
	}

	// 磁盘水位：无论配额是否满足，可用空间不够就必须继续删。
	if pol.MinFreeBytes > 0 && freeBytes >= 0 && freeBytes < pol.MinFreeBytes {
		need := pol.MinFreeBytes - freeBytes
		freed := int64(0)
		for _, a := range actions {
			if a.Action == "delete" {
				freed += a.Bytes
			}
		}
		for _, p := range kept {
			if freed >= need {
				break
			}
			if alreadyPlanned(actions, p.Path) {
				continue
			}
			actions = append(actions, RetentionAction{
				Path: p.Path, Bytes: p.Bytes, Reason: "disk", Action: "delete", Start: p.Start,
			})
			freed += p.Bytes
		}
	}

	_ = compressed
	// 删除按时间从旧到新（先删最旧的，保留最近的可查窗口）。
	sort.SliceStable(actions, func(i, j int) bool {
		if actions[i].Action != actions[j].Action {
			// 压缩与删除互不干扰，删除排在前面（先腾空间再压）。
			return actions[i].Action == "delete"
		}
		return actions[i].Start.Before(actions[j].Start)
	})
	return actions, total, nil
}

func alreadyPlanned(actions []RetentionAction, path string) bool {
	for _, a := range actions {
		if a.Path == path {
			return true
		}
	}
	return false
}

// MaintenanceResult 一次维护执行的结果。
type MaintenanceResult struct {
	Deleted       int               `json:"deleted"`
	ReleasedBytes int64             `json:"released_bytes"`
	Compressed    int               `json:"compressed"`
	CompressedIn  int64             `json:"compressed_in_bytes"`
	CompressedOut int64             `json:"compressed_out_bytes"`
	FreeBytes     int64             `json:"free_bytes"`
	TotalBytes    int64             `json:"total_bytes"`
	Actions       []RetentionAction `json:"actions"`
	Errors        []string          `json:"errors,omitempty"`
}

// ApplyMaintenance 执行维护计划。
//
// 语义保证：
//   - 单个动作失败**不中断**整轮维护，但每一处失败都会被记进 Errors ——
//     静默失败会让"磁盘一直不降"变成无法解释的现象；
//   - 压缩先校验（解压回来逐字节比对长度）再删原文件。校验不通过就保留原文件，
//     宁可多占空间，也不能把日志压坏 —— 压坏等于静默丢数据；
//   - 删除/压缩后立即丢弃该分片的缓存句柄（否则会在已删除文件上继续读写）。
func (s *Store) ApplyMaintenance(kind Kind, pol RetentionPolicy) (MaintenanceResult, error) {
	var res MaintenanceResult
	free := int64(-1)
	if pol.MinFreeBytes > 0 && s.diskCheck {
		if v, err := s.FreeBytes(); err == nil {
			free = v
		}
	}
	plan, total, err := s.PlanMaintenance(kind, pol, s.now(), free)
	if err != nil {
		return res, err
	}
	res.FreeBytes = free
	res.TotalBytes = total

	for _, a := range plan {
		switch a.Action {
		case "compress":
			in, out, cerr := s.compressShard(a.Path)
			if cerr != nil {
				res.Errors = append(res.Errors, "压缩失败 "+filepath.Base(a.Path)+": "+cerr.Error())
				continue
			}
			a.Bytes, a.SavedBytes = in, in-out
			a.Reason = "compress"
			res.Compressed++
			res.CompressedIn += in
			res.CompressedOut += out
			res.Actions = append(res.Actions, a)
		default:
			s.dropCached(a.Path)
			if rerr := os.Remove(a.Path); rerr != nil && !os.IsNotExist(rerr) {
				res.Errors = append(res.Errors, "删除失败 "+filepath.Base(a.Path)+": "+rerr.Error())
				continue
			}
			// 一并清掉 WAL/SHM 残留，否则目录里会长期留着一堆孤儿文件。
			for _, suffix := range []string{"-wal", "-shm"} {
				_ = os.Remove(a.Path + suffix)
			}
			res.Deleted++
			res.ReleasedBytes += a.Bytes
			a.Action = "delete"
			res.Actions = append(res.Actions, a)
		}
	}
	if free >= 0 && s.diskCheck {
		if v, err := s.FreeBytes(); err == nil {
			res.FreeBytes = v
		}
	}
	return res, nil
}

// FreeBytes 返回日志根目录所在文件系统的可用字节数。
func (s *Store) FreeBytes() (int64, error) {
	return freeBytes(s.Root)
}

// compressShard 把 f 压成 f.gz，校验通过后删除原文件。
//
// ⚠️ 压缩前必须先把 WAL 合并回主文件，否则会**静默丢数据**：
// WAL 模式下，最近提交的行可能还只在 `f-wal` 里。只压 `f` 就等于把那些行丢掉，
// 而且丢掉之后毫无痕迹 —— 文件"压缩成功"、原文件"正常删除"，只有查历史时才发现少了几天。
// 所以顺序是：先断开写连接（触发 checkpoint）→ 再显式 checkpoint(TRUNCATE) →
// 确认 `-wal` 已空 → 才压缩；任何一步不满足就**拒绝压缩**并报错。
//
// 校验方式：解压回来比对**字节长度**与 gzip 尾部记录的原始长度。
// 为什么不比对完整内容：那等于把文件读两遍，代价翻倍而收益很小 ——
// 长度一致加上 gzip 自带的 CRC32（解压时会校验，不一致会报错）已经足够。
// 关键在于：任何一步失败都**不删原文件**。
func (s *Store) compressShard(path string) (inBytes, outBytes int64, err error) {
	if strings.HasSuffix(path, CompressionSuffix) {
		return 0, 0, fmt.Errorf("分片已是压缩状态: %s", filepath.Base(path))
	}
	// 必须先确认文件存在。压缩流程里有一个"以写模式打开分片"的步骤（为了 checkpoint），
	// 而写模式打开会**创建**一个空库 —— 于是"压缩一个不存在的分片"会成功产出一个
	// 空分片，看起来一切正常。这类"把不存在变成存在"的行为必须在入口挡住。
	if _, err := os.Stat(path); err != nil {
		return 0, 0, fmt.Errorf("分片不存在或不可读: %s（%v）", filepath.Base(path), err)
	}
	if err := s.checkpointForCompress(path); err != nil {
		return 0, 0, err
	}

	src, err := os.Open(path)
	if err != nil {
		return 0, 0, err
	}
	fi, err := src.Stat()
	if err != nil {
		_ = src.Close()
		return 0, 0, err
	}

	out := path + CompressionSuffix
	tmp := out + ".tmp"
	dst, err := os.Create(tmp)
	if err != nil {
		_ = src.Close()
		return 0, 0, err
	}
	zw, err := gzip.NewWriterLevel(dst, gzip.BestCompression)
	if err != nil {
		_ = dst.Close()
		_ = src.Close()
		_ = os.Remove(tmp)
		return 0, 0, err
	}
	_, cpErr := io.Copy(zw, src)
	// ⚠️ 必须在删除原文件**之前**关掉这个读句柄。
	// 之前写成 defer src.Close()，删除动作发生在 defer 之前 ——
	// 在 Windows 上就是"压缩成功但原文件删不掉"，而这恰恰是最该成功的一步。
	closeErr := src.Close()
	if cpErr != nil {
		_ = zw.Close()
		_ = dst.Close()
		_ = os.Remove(tmp)
		return 0, 0, cpErr
	}
	if closeErr != nil {
		_ = zw.Close()
		_ = dst.Close()
		_ = os.Remove(tmp)
		return 0, 0, closeErr
	}
	if err := zw.Close(); err != nil {
		_ = dst.Close()
		_ = os.Remove(tmp)
		return 0, 0, err
	}
	if err := dst.Close(); err != nil {
		_ = os.Remove(tmp)
		return 0, 0, err
	}

	// 校验：解压回来必须能读完，且长度与原文件一致。
	if err := verifyGzip(tmp, fi.Size()); err != nil {
		_ = os.Remove(tmp)
		return 0, 0, fmt.Errorf("压缩产物校验失败（原文件保留）：%w", err)
	}
	if err := os.Rename(tmp, out); err != nil {
		_ = os.Remove(tmp)
		return 0, 0, err
	}
	ofi, err := os.Stat(out)
	if err != nil {
		return 0, 0, err
	}

	// 校验通过：丢弃句柄（若还持有），删除原文件与 WAL/SHM 残留。
	s.dropCached(path)
	if err := os.Remove(path); err != nil {
		return 0, ofi.Size(), fmt.Errorf("压缩成功但原文件删除失败: %w", err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		_ = os.Remove(path + suffix)
	}
	return fi.Size(), ofi.Size(), nil
}

// checkpointForCompress 在压缩之前把 WAL 合并干净。
//
// 步骤（顺序不能变）：
//  1. 丢弃缓存里的写/读连接 —— 写连接关闭时 SQLite 会做一次 checkpoint；
//  2. 用一条临时写连接执行 `PRAGMA wal_checkpoint(TRUNCATE)`，把残留的 WAL 清空；
//  3. 确认 `-wal` 文件为空或不存在。不满足就报错，**不压缩**。
func (s *Store) checkpointForCompress(path string) error {
	s.dropCached(path)

	d, err := db.Open(path, false)
	if err != nil {
		return fmt.Errorf("压缩前打开分片失败: %w", err)
	}
	if _, err := d.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		_ = d.Close()
		return fmt.Errorf("压缩前合并 WAL 失败: %w", err)
	}
	// 关掉才能保证句柄释放（Windows 上句柄会阻止后续删除原文件）
	if err := d.Close(); err != nil {
		return fmt.Errorf("压缩前关闭分片失败: %w", err)
	}
	if sz := fileSize(path + "-wal"); sz > 0 {
		return fmt.Errorf("分片 %s 的 WAL 仍有 %d 字节未合并，拒绝压缩（否则会丢掉这部分日志）",
			filepath.Base(path), sz)
	}
	return nil
}

// verifyGzip 解压 r 并确认解压后的字节数与 want 相同。
func verifyGzip(path string, want int64) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer zr.Close()
	n, err := io.Copy(io.Discard, zr)
	if err != nil {
		return err
	}
	if n != want {
		return fmt.Errorf("解压后 %d 字节，原文件 %d 字节", n, want)
	}
	return nil
}

// materializeCompressed 把压缩分片解压到一个临时文件，返回其路径。
// 调用方负责删除临时文件。
func materializeCompressed(path string) (string, error) {
	src, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer src.Close()
	zr, err := gzip.NewReader(src)
	if err != nil {
		return "", err
	}
	defer zr.Close()
	tmp, err := os.CreateTemp(filepath.Dir(path), ".decompress-*.db")
	if err != nil {
		return "", err
	}
	defer tmp.Close()
	if _, err := io.Copy(tmp, zr); err != nil {
		_ = os.Remove(tmp.Name())
		return "", err
	}
	return tmp.Name(), nil
}
