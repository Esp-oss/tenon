// Package audit 实现审计两段写（WAL）与 prevHash 哈希链，对应
// spec/0.1/semantics.md §10 与 spec/0.1/schemas/audit-event.schema.json。
//
// 存储形态是 append-only 的 JSONL：一行一条记录，每行都是 RFC 8785 规范化后的紧凑
// JSON。写入用 O_APPEND 一次性追加并在返回前 fsync，所以「Append 返回 nil」意味着这条
// dispatched 记录已经落盘——上层只有在这之后才允许发出真实调用（§10.2：写失败必须拒绝
// 执行）。
//
// 哈希链：hash = "sha256:" + hex(sha256(JCS(记录去掉 hash 字段)))，prevHash 指向写入
// 顺序上前一条记录的 hash，链首为 null。规范化规则见 canonicalJSON 附近的注释，第三方
// 实现可以脱离本包独立复算（§10.3 要求链的验证只依赖公开数据）。
//
// 边界：本包只管「怎么存、怎么串、怎么验」，不校验字段的取值域与内部形状——
// auditId/planId 的正则、decision/result/effects 的结构由 internal/spec 负责；本包
// 也不含闸门、重试与执行逻辑。另外，schema 里 required 的 trigger/inputHash/
// idempotencyKey/tokenUsage 本层不承载（它们要么在调用方手里，要么属后续修订），
// 落库前须先补齐。
package audit

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
)

// 审计两段（spec/0.1/schemas/audit-event.schema.json#/$defs/AuditStage）。
const (
	// StageDispatched 表示「即将发出调用」，必须在真实调用之前落盘。
	StageDispatched = "dispatched"
	// StageResult 表示「已得到结果」（含结果未定）。
	StageResult = "result"
)

// hashPrefix 是规范约定的摘要前缀，见 common.schema.json#/$defs/Sha256Hash。
const hashPrefix = "sha256:"

// AuditRecord 是 spec/0.1/schemas/audit-event.schema.json 的运行期子集。
//
// 只列宿主写链时自己要操作的字段：字段名与 schema 一致；嵌套结构（actor / decision /
// result / effects）用 json.RawMessage 原样透传，本包不校验它们的内部形状——那是
// internal/spec 的事。Hash 与 PrevHash 由 Store 维护，Append 会无条件覆盖调用方给的值。
type AuditRecord struct {
	AuditID  string          `json:"auditId"`
	Stage    string          `json:"stage"` // dispatched | result
	At       time.Time       `json:"at"`
	Actor    json.RawMessage `json:"actor"`
	Trigger  json.RawMessage `json:"trigger,omitempty"`
	PlanID   string          `json:"planId,omitempty"`
	StepID   string          `json:"stepId,omitempty"`
	Action   string          `json:"action,omitempty"`
	Decision json.RawMessage `json:"decision,omitempty"`
	Result   json.RawMessage `json:"result,omitempty"`
	Effects  json.RawMessage `json:"effects,omitempty"`
	// 以下三个字段是 spec/0.1 的 AuditEvent 必填项，之前遗漏会导致落库记录
	// 不是 schema 完整的审计事件（写进去也会被静默丢弃）。
	InputHash      string          `json:"inputHash,omitempty"`
	IdempotencyKey string          `json:"idempotencyKey,omitempty"`
	TokenUsage     json.RawMessage `json:"tokenUsage,omitempty"`
	Hash           string          `json:"hash"`
	PrevHash       *string         `json:"prevHash"`
}

// Query 是审计检索条件，零值表示「全量、不截断」。
type Query struct {
	PlanID string // 非空时只返回该计划的记录
	Since  int    // 游标：从写入顺序上第 Since 条记录（0 基）开始返回
	Limit  int    // 最多返回几条；0 表示不截断
}

// ChainError 说明哈希链断在第几条记录上。
type ChainError struct {
	Index  int    // 记录序号，1 基，按写入顺序（文件里若有空行不算一条记录）
	Reason string // 中文原因，指出是 prevHash 断链还是记录内容被改
}

// Error 输出「第几条、为什么断」。
func (e *ChainError) Error() string {
	return fmt.Sprintf("审计链在第 %d 条记录断裂：%s", e.Index, e.Reason)
}

// Store 是一条 append-only 审计日志。
//
// 进程内用 Mutex 串行化写入与读取，因此同一个 *Store 上的 Append 是全序的；多进程同时
// 写同一文件不在支持范围内（链尾会分叉，各自写出自洽但整体断链的日志）。
type Store struct {
	mu       sync.Mutex
	path     string
	f        *os.File
	tail     *string // 链尾 hash；nil 表示还没有任何记录（下一条的 prevHash 为 null）
	failNext error   // 测试注入：下一次 Append 直接返回该错误
	broken   error   // 真实写入失败后置位：内存链尾可能与磁盘不一致，拒绝继续追加
}

// NewAuditStore 打开或创建 path 上的 JSONL 审计日志，并把链尾接到既有记录之后。
//
// 打开时会先读一遍全量记录并校验整条链：已损坏的日志不允许继续追加（fail-closed），
// 否则「链从某处断了」会被后续写入掩盖。代价是启动 O(n)，对 M0 的量级足够。
func NewAuditStore(path string) (*Store, error) {
	if path == "" {
		return nil, fmt.Errorf("审计日志路径不能为空")
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("打开审计日志失败：%w", err)
	}
	s := &Store{path: path, f: f}
	recs, err := s.readAll()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if err := verifyRecords(recs); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("既有审计日志未通过链校验，拒绝接链：%w", err)
	}
	if n := len(recs); n > 0 {
		last := recs[n-1]
		if last.Hash == "" {
			_ = f.Close()
			return nil, fmt.Errorf("审计日志第 %d 条记录缺少 hash，拒绝在其后追加", n)
		}
		s.tail = &last.Hash
	}
	return s, nil
}

// Close 关闭底层文件句柄；之后再 Append 会报错，Query 与 VerifyChain 仍然可读。
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		return nil
	}
	err := s.f.Close()
	s.f = nil
	return err
}

// FailNextAppend 让下一次 Append 直接返回 err，既不写盘也不改动链尾。
//
// 仅供测试与故障演练：spec/0.1/semantics.md §10.2 要求「注入 dispatched 写失败，必须
// 观察到调用未发出」，上层用例靠这个钩子制造那一幕。传 nil 等同取消注入。生产代码保留
// 此钩子，但除演练脚本外不应调用。
func (s *Store) FailNextAppend(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failNext = err
}

// Append 把一条审计记录追加到链尾，返回 nil 表示已经落盘。
//
// 三件事由 Store 负责而不是调用方：At 为零值时补当前时刻；Hash 按 §10.3 重算；PrevHash
// 一律指向当前链尾（链首为 null），调用方给的值会被覆盖——链的权威只能有一个。
func (s *Store) Append(rec AuditRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.f == nil {
		return fmt.Errorf("审计存储已关闭，拒绝追加")
	}
	if s.broken != nil {
		return fmt.Errorf("审计存储已失效（上次写入失败：%v），须重新打开并校验链", s.broken)
	}
	if s.failNext != nil {
		err := s.failNext
		s.failNext = nil
		return err
	}
	if err := checkRecord(rec); err != nil {
		return err
	}

	if rec.At.IsZero() {
		rec.At = time.Now().UTC()
	}
	rec.Hash = ""
	rec.PrevHash = s.tail

	obj, err := recordTree(rec)
	if err != nil {
		return fmt.Errorf("审计记录无法规范化（auditId=%s）：%w", rec.AuditID, err)
	}
	hash, err := hashOfTree(obj)
	if err != nil {
		return fmt.Errorf("审计记录无法规范化（auditId=%s）：%w", rec.AuditID, err)
	}
	rec.Hash = hash
	obj["hash"] = hash
	line, err := encodeCanonical(obj)
	if err != nil {
		return fmt.Errorf("审计记录无法规范化（auditId=%s）：%w", rec.AuditID, err)
	}

	// 单次 Write 追加整行：O_APPEND 下这段字节要么整行落到文件尾，要么报错，
	// 不会插入到别的记录中间。
	buf := make([]byte, 0, len(line)+1)
	buf = append(buf, line...)
	buf = append(buf, '\n')
	if _, err := s.f.Write(buf); err != nil {
		s.broken = err
		return fmt.Errorf("写入审计记录失败（auditId=%s）：%w", rec.AuditID, err)
	}
	// dispatched 段必须先落盘才允许执行，所以这里必须 fsync，不能只依赖页缓存。
	if err := s.f.Sync(); err != nil {
		s.broken = err
		return fmt.Errorf("审计记录落盘失败（auditId=%s，fsync）：%w", rec.AuditID, err)
	}
	s.tail = &rec.Hash
	return nil
}

// Query 按写入顺序返回满足条件的记录：先按 Since 跳过前若干条，再按 PlanID 过滤，
// 最后用 Limit 截断。Since 超过记录条数时返回空结果（游标走到末尾不是错误）。
func (s *Store) Query(q Query) ([]AuditRecord, error) {
	if q.Since < 0 || q.Limit < 0 {
		return nil, fmt.Errorf("审计检索条件非法：since=%d limit=%d，都不允许为负", q.Since, q.Limit)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	recs, err := s.readAll()
	if err != nil {
		return nil, err
	}
	if q.Since > len(recs) {
		q.Since = len(recs)
	}
	out := make([]AuditRecord, 0, len(recs)-q.Since)
	for _, rec := range recs[q.Since:] {
		if q.PlanID != "" && rec.PlanID != q.PlanID {
			continue
		}
		if q.Limit > 0 && len(out) >= q.Limit {
			break
		}
		out = append(out, rec)
	}
	return out, nil
}

// VerifyChain 按写入顺序重算全链并逐条比对，返回 *ChainError 指出断在第几条。
//
// 两种断裂都会被抓到：记录内容被改（重算 hash 与记录里的 hash 不符）与记录被删/被插
// （prevHash 接不上前一条）。
func (s *Store) VerifyChain() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	recs, err := s.readAll()
	if err != nil {
		return err
	}
	return verifyRecords(recs)
}

// readAll 读全量记录。空行被跳过：正常写入总以换行结尾，末尾的空串是切分产物。
// 半行/坏行一律报错，不静默跳过——否则「少了一条」与「从没写过」无法区分。
func (s *Store) readAll() ([]AuditRecord, error) {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return nil, fmt.Errorf("读取审计日志失败：%w", err)
	}
	lines := bytes.Split(raw, []byte("\n"))
	recs := make([]AuditRecord, 0, len(lines))
	for i, line := range lines {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var rec AuditRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			return nil, fmt.Errorf("审计日志第 %d 行无法解析：%w", i+1, err)
		}
		recs = append(recs, rec)
	}
	return recs, nil
}

// verifyRecords 按写入顺序重算全链；Index 为 1 基记录序号。
func verifyRecords(recs []AuditRecord) error {
	var prev *string
	for i := range recs {
		rec := recs[i]
		idx := i + 1
		if !sameHash(rec.PrevHash, prev) {
			return &ChainError{
				Index:  idx,
				Reason: fmt.Sprintf("prevHash 未接上一条记录（期望 %s，实际 %s）", describeHash(prev), describeHash(rec.PrevHash)),
			}
		}
		want, err := hashOfRecord(rec)
		if err != nil {
			return &ChainError{Index: idx, Reason: "记录无法重新规范化：" + err.Error()}
		}
		if rec.Hash != want {
			return &ChainError{
				Index:  idx,
				Reason: fmt.Sprintf("记录内容与自身 hash 不符（重算 %s，记录里是 %s）", want, describeHash(&rec.Hash)),
			}
		}
		h := rec.Hash
		prev = &h
	}
	return nil
}

// checkRecord 只拦住「写进去就没有审计价值」的记录：缺 id，或 stage 不是那两态。
// 字段格式（aud_/pln_ 前缀、时间带时区等）留给 internal/spec。
func checkRecord(rec AuditRecord) error {
	if rec.AuditID == "" {
		return fmt.Errorf("审计记录缺少 auditId")
	}
	switch rec.Stage {
	case StageDispatched, StageResult:
	default:
		return fmt.Errorf("审计记录 %s 的 stage=%q 非法，只能是 %s 或 %s", rec.AuditID, rec.Stage, StageDispatched, StageResult)
	}
	return nil
}

func sameHash(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func describeHash(h *string) string {
	switch {
	case h == nil:
		return "null"
	case *h == "":
		return "空"
	default:
		return *h
	}
}

// recordTree 把记录编成 JSON 树（map[string]any）并去掉 hash 字段：hash 由链自己算，
// 不进摘要输入，否则自指。嵌套的 RawMessage 在这里被解析后重新规范化，因此 hash 只取决于
// 语义内容，与调用方当初的排版（空白、键序、数字写法）无关。
func recordTree(rec AuditRecord) (map[string]any, error) {
	raw, err := json.Marshal(rec)
	if err != nil {
		return nil, err
	}
	v, err := decodeJSON(raw)
	if err != nil {
		return nil, err
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("审计记录不是 JSON 对象")
	}
	delete(obj, "hash")
	return obj, nil
}

// hashOfRecord 重算一条记录的 hash（输入不含它自己的 hash 字段）。
func hashOfRecord(rec AuditRecord) (string, error) {
	obj, err := recordTree(rec)
	if err != nil {
		return "", err
	}
	return hashOfTree(obj)
}

// hashOfTree 对规范化后的 JSON 取 SHA-256，写成 sha256:<hex>。
func hashOfTree(v any) (string, error) {
	canon, err := encodeCanonical(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canon)
	return hashPrefix + hex.EncodeToString(sum[:]), nil
}

// decodeJSON 解析任意一段 JSON 文本，数字保留字面量（避免先转 float64 再转回去时丢精度
// 或改变写法）。尾部多余内容视为错误：一行只允许一个 JSON 值。
func decodeJSON(raw []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("JSON 之后还有多余内容")
	}
	return v, nil
}

// encodeCanonical 输出 RFC 8785（JCS）形式的紧凑 JSON：键按 UTF-16 码元序排序、无任何
// 多余空白、字符串只做必需转义、数字按 ECMAScript Number::toString 书写。同一份语义内容
// 只有一种字节形态，所以 hash 可以被任何第三方独立复算。
func encodeCanonical(v any) ([]byte, error) {
	var buf bytes.Buffer
	if err := writeCanonical(&buf, v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeCanonical(buf *bytes.Buffer, v any) error {
	switch x := v.(type) {
	case nil:
		buf.WriteString("null")
	case bool:
		if x {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case string:
		writeCanonicalString(buf, x)
	case json.Number:
		num, err := canonicalNumber(x)
		if err != nil {
			return err
		}
		buf.WriteString(num)
	case []any:
		buf.WriteByte('[')
		for i, elem := range x {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeCanonical(buf, elem); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool { return utf16Less(keys[i], keys[j]) })
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			writeCanonicalString(buf, k)
			buf.WriteByte(':')
			if err := writeCanonical(buf, x[k]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	default:
		return fmt.Errorf("JSON 规范化：不支持的取值类型 %T", v)
	}
	return nil
}

// writeCanonicalString 按 RFC 8785 §3.2.2.2 写字符串：只做必需转义，控制字符用最短形式
// （\b \t \n \f \r，其余 \u00xx 小写十六进制），非 ASCII 字符按 UTF-8 原样输出。
func writeCanonicalString(buf *bytes.Buffer, s string) {
	const hexDigits = "0123456789abcdef"
	buf.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			buf.WriteString(`\"`)
		case '\\':
			buf.WriteString(`\\`)
		case '\b':
			buf.WriteString(`\b`)
		case '\t':
			buf.WriteString(`\t`)
		case '\n':
			buf.WriteString(`\n`)
		case '\f':
			buf.WriteString(`\f`)
		case '\r':
			buf.WriteString(`\r`)
		default:
			if r < 0x20 {
				buf.WriteString(`\u00`)
				buf.WriteByte(hexDigits[(r>>4)&0xf])
				buf.WriteByte(hexDigits[r&0xf])
				continue
			}
			buf.WriteRune(r)
		}
	}
	buf.WriteByte('"')
}

// utf16Less 按 UTF-16 码元序列比较字符串，这是 RFC 8785 §3.2.3 规定的键序；它与按 UTF-8
// 字节序只在「BMP 末尾字符（U+E000..U+FFFF）与代理对（非 BMP 字符）混排」时分歧。
func utf16Less(a, b string) bool {
	au, bu := utf16.Encode([]rune(a)), utf16.Encode([]rune(b))
	for i := 0; i < len(au) && i < len(bu); i++ {
		if au[i] != bu[i] {
			return au[i] < bu[i]
		}
	}
	return len(au) < len(bu)
}

// canonicalNumber 按 ECMAScript Number::toString（RFC 8785 §3.2.2.3）写数字：同一个数值
// 无论原文写成 1e3、1000 还是 1000.0，规范化后都是 1000；-0 写成 0。
//
// 解析不成 float64 或超出其表示范围（含 ±Inf）一律报错，不静默截断：静默截断会让两条
// 不同的记录算出同一个摘要，链就失去了「改一处即断」的性质。
func canonicalNumber(n json.Number) (string, error) {
	f, err := strconv.ParseFloat(n.String(), 64)
	if err != nil {
		return "", fmt.Errorf("数字 %q 无法表示为 float64：%w", n.String(), err)
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return "", fmt.Errorf("数字 %q 超出 float64 表示范围", n.String())
	}
	if f == 0 {
		return "0", nil
	}
	neg := f < 0
	if neg {
		f = -f
	}
	// 'e' 格式给出最短往返有效数字，形如 d[.ddd]e±XX，满足 d.ddd × 10^exp = f。
	sci := strconv.FormatFloat(f, 'e', -1, 64)
	mant, expText, _ := strings.Cut(sci, "e")
	exp, err := strconv.Atoi(expText)
	if err != nil {
		return "", fmt.Errorf("数字 %q 的指数部分无法解析：%w", n.String(), err)
	}
	digits := strings.Replace(mant, ".", "", 1)
	k := len(digits)
	// 与 ECMA-262 的 (s, k, n) 同构：值 = 0.<digits> × 10^n。
	n10 := exp + 1
	var out string
	switch {
	case k <= n10 && n10 <= 21:
		out = digits + strings.Repeat("0", n10-k)
	case 0 < n10 && n10 <= 21:
		out = digits[:n10] + "." + digits[n10:]
	case -6 < n10 && n10 <= 0:
		out = "0." + strings.Repeat("0", -n10) + digits
	default:
		e := n10 - 1
		sign := "+"
		if e < 0 {
			sign, e = "-", -e
		}
		if k == 1 {
			out = digits + "e" + sign + strconv.Itoa(e)
		} else {
			out = digits[:1] + "." + digits[1:] + "e" + sign + strconv.Itoa(e)
		}
	}
	if neg {
		out = "-" + out
	}
	return out, nil
}
