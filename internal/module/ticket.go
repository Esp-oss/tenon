package module

import (
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"tenon.dev/internal/protocol"
)

// 票据强制模式。enforce 是唯一允许上生产的取值；warn 只用于本机联调。
const (
	TicketModeEnforce = "enforce"
	TicketModeWarn    = "warn"
)

// ticketRejection 是拒票的原因与错误码，便于日志与测试共享同一套判据。
type ticketRejection struct {
	code   string
	reason string
}

// checkTicket 按 design §5.3 的 9 步顺序校验票据，返回 nil 表示放行。
//
// 顺序不是随意的：先判"有没有/是不是真的"，再判"是不是这张票"，
// 最后才是"用过没有"。把重用检查提前会让一张伪造票也报 TICKET_REUSED，
// 排查时会把"有人在重放"和"签名不对"混成一件事。
func (m *Module) checkTicket(r *http.Request, body []byte, action string, now time.Time) (*protocol.Ticket, *ticketRejection) {
	if len(m.TrustedKeys) == 0 {
		// 没配信任集就不该假装验过了：enforce 下必须拒绝，warn 下记一条日志放行。
		return nil, &ticketRejection{code: protocol.CodeTicketUnknownKey,
			reason: "模块未配置 TENON_TRUSTED_KEYS，无法验票"}
	}

	raw := strings.TrimSpace(r.Header.Get(protocol.HeaderTicket))
	if raw == "" {
		return nil, &ticketRejection{code: protocol.CodeTicketMissing,
			reason: "缺少 " + protocol.HeaderTicket + " 请求头：没有宿主签发的票就不产生副作用"}
	}

	t, err := protocol.VerifyTicket(m.TrustedKeys, raw, now, protocol.SignatureTolerance)
	if err != nil {
		switch {
		case isErr(err, protocol.ErrTicketUnknownKey):
			return nil, &ticketRejection{code: protocol.CodeTicketUnknownKey, reason: err.Error()}
		case isErr(err, protocol.ErrTicketExpired), isErr(err, protocol.ErrTicketDuration):
			return nil, &ticketRejection{code: protocol.CodeTicketExpired, reason: err.Error()}
		default:
			return nil, &ticketRejection{code: protocol.CodeTicketInvalid, reason: err.Error()}
		}
	}

	if t.Module != m.ID {
		return nil, &ticketRejection{code: protocol.CodeTicketWrongModule,
			reason: fmt.Sprintf("票据目标模块是 %q，本模块是 %q", t.Module, m.ID)}
	}
	if t.Action != action {
		return nil, &ticketRejection{code: protocol.CodeTicketActionMismatch,
			reason: fmt.Sprintf("票据动作为 %q，本次请求是 %q", t.Action, action)}
	}
	if want := protocol.ArgsHash(body); t.ArgsHash != want {
		return nil, &ticketRejection{code: protocol.CodeTicketArgsMismatch,
			reason: fmt.Sprintf("入参摘要不符：票内 %s，实收 %s（参数被改过）", t.ArgsHash, want)}
	}

	// 一次性：同一 tid 只服务同一个幂等键。
	// 同键重发是传输层重试（下面命中幂等缓存返回首次结果），换键重发才是重放。
	if m.ticketUses == nil {
		m.ticketUses = make(map[string]string)
	}
	reqIdem := strings.TrimSpace(r.Header.Get(protocol.HeaderIdempotencyKey))
	if prevIdem, used := m.ticketUses[t.TicketID]; used && prevIdem != reqIdem {
		return nil, &ticketRejection{code: protocol.CodeTicketReused,
			reason: fmt.Sprintf("票据 %s 已用于幂等键 %q，本次是 %q", t.TicketID, prevIdem, reqIdem)}
	}
	if t.IdempotencyKey != reqIdem {
		return nil, &ticketRejection{code: protocol.CodeTicketArgsMismatch,
			reason: fmt.Sprintf("票据绑定的幂等键是 %q，请求头是 %q", t.IdempotencyKey, reqIdem)}
	}
	m.ticketUses[t.TicketID] = reqIdem
	return &t, nil
}

func isErr(err, target error) bool {
	return err != nil && target != nil && strings.Contains(err.Error(), target.Error())
}

// rejectTicket 写出拒票响应。warn 模式下只记日志不拦，**仅限本机联调**。
func (h *handler) rejectTicket(w http.ResponseWriter, rej *ticketRejection, action string) bool {
	if h.m.TicketMode == TicketModeWarn {
		log.Printf("module: warn 模式放行无票/坏票请求 action=%s code=%s：%s",
			action, rej.code, rej.reason)
		return false
	}
	e := protocol.NewTicketError(rej.code, rej.reason)
	writeTenonError(w, e)
	return true
}
