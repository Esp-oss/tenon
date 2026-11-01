package module

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// genesisKind 是链的创世行标记。
//
// 升级前的日志是无链的，直接校验会让每个升级过的部署第一次启动就判定"链断"。
// 所以首次以新版本写入时先落一行创世，记下此前有多少行历史；那些行标 legacy，
// 只参与按幂等键的对账，不参与链校验（design §8.3）。
const genesisKind = "genesis"

// chainInput 把一条记录摊平成稳定的哈希输入。
//
// 不用 json.Marshal：字段顺序、omitempty、HTML 转义都会让同一条记录在不同版本里
// 算出不同哈希，而这种"升级即断链"的坑在审计里是不可接受的。
func chainInput(prev string, r auditRecord) string {
	parts := []string{
		prev, r.Module, r.Kind, r.Action, r.IdempotencyKey, r.ArgsHash,
		r.Result, r.ErrorCode, r.EventID, r.AuditID, r.PlanID, r.StepID,
		r.ApprovedBy, r.TicketID, r.At,
	}
	return strings.Join(parts, "\x1f")
}

func chainHash(prev string, r auditRecord) string {
	sum := sha256.Sum256([]byte(chainInput(prev, r)))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// ensureChain 在第一次追加前把链状态准备好：有历史就先写创世行。
// 调用方必须持有 auditMu。
func (m *Module) ensureChain() error {
	if m.auditReady {
		return nil
	}
	legacy, err := countLines(m.auditPath())
	if err != nil {
		return err
	}
	m.auditSeq = int64(legacy)
	if legacy > 0 {
		g := auditRecord{
			At:          time.Now().UTC().Format(time.RFC3339),
			Module:      m.id(),
			Kind:        genesisKind,
			Result:      "ok",
			LegacyCount: legacy,
			Seq:         int64(legacy),
		}
		head := chainHash("", g)
		g.Hash = head
		if err := m.writeAuditLine(g); err != nil {
			return err
		}
		m.auditHead = head
		m.auditSeq++
	}
	m.auditReady = true
	return nil
}

func countLines(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	defer func() { _ = f.Close() }()
	n := 0
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for s.Scan() {
		if strings.TrimSpace(s.Text()) != "" {
			n++
		}
	}
	return n, s.Err()
}

// writeAuditLine 只负责追加一行，不参与链计算。
func (m *Module) writeAuditLine(rec auditRecord) error {
	line, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	f, err := os.OpenFile(m.auditPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(line); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// chainStatus 是 GET /tenon/audit/local 的响应。
type chainStatus struct {
	Module    string        `json:"module"`
	HeadSeq   int64         `json:"headSeq"`
	HeadHash  string        `json:"headHash"`
	Legacy    int           `json:"legacyRecords"`
	Records   []auditRecord `json:"records"`
	Truncated bool          `json:"truncated,omitempty"`
}

// readChain 读出 since 之后最多 limit 行，并顺带校验链。
//
// 校验与读取放在一起：对账方拿到 headHash 时，必须同时知道"这份 head 是自洽的"，
// 否则一个被改过的日志也能给出一个看起来很正常的 head。
func (m *Module) readChain(since int64, limit int) (chainStatus, error) {
	out := chainStatus{Module: m.id(), Records: []auditRecord{}}
	f, err := os.Open(m.auditPath())
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return out, err
	}
	defer func() { _ = f.Close() }()

	prev := ""
	seq := int64(0)
	legacy := 0
	started := false
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" {
			continue
		}
		var rec auditRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			return out, fmt.Errorf("第 %d 行不是合法 JSON：%w", seq+1, err)
		}
		seq++
		if rec.Kind == genesisKind {
			started = true
			prev = rec.Hash
		} else if !started {
			if rec.Hash == "" {
				// 创世行之前的历史（没有 hash 字段）：不校验，只计数。
				legacy++
			} else {
				// 文件里没有创世行，但第一条就带 hash：链从第一行起算。
				started = true
				prev = rec.PrevHash
			}
		}
		if started && rec.Kind != genesisKind {
			if rec.PrevHash != prev {
				return out, fmt.Errorf("链断在第 %d 行：prevHash 不接", seq)
			}
			if rec.Hash != chainHash(prev, rec) {
				return out, fmt.Errorf("第 %d 行内容与哈希不符（被改过）", seq)
			}
			prev = rec.Hash
		}
		if rec.Seq >= since && len(out.Records) < limit {
			out.Records = append(out.Records, rec)
		} else if rec.Seq >= since {
			out.Truncated = true
		}
	}
	if err := s.Err(); err != nil {
		return out, err
	}
	out.HeadSeq = seq
	out.HeadHash = prev
	out.Legacy = legacy
	return out, nil
}

// parseSince 解析 ?since=，非法值按 0 处理（读全量），不因参数错而拒绝对账。
func parseSince(raw string) int64 {
	if raw == "" {
		return 0
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || v < 0 {
		return 0
	}
	return v
}
