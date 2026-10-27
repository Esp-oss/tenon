// board.go 实现公告板（host 的 pendingBoard，spec §3.4）的两个出口：
//
//	RepushBoard   —— 每 tick 把 pending/ 里的**活条目**幂等 upsert 上去（read-model 可重建）；
//	finishTerminal —— 归档终态的那一刻推一条同 planId 的覆盖，让页面能把卡片标成
//	                  「刚刚处理完」，而不是继续冒充「等人批」。
//
// 两者的边界写死在这里，免得后来的人把它们混成一个：
//
//	RepushBoard 只看 pending/，从不替归档件补推终态（归档件已经处理完了，重推就是推一张
//	过期的卡）；finishTerminal **只推公告板、绝不写审计**——计划级 result 段在归档之前
//	就由 resume.go 的 writePlanResult 写好了，auditId 复用挂起时那条 dispatched（I2）。
//	在这里再写一次同一个 auditId 的 result，审计链上就会多出第三条收尾记录。
package agent

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"
)

// boardExpiry 决定条目上写哪个 expiresAt。
//
// host 的 pendingBoard.List 对 expiresAt 的处理是**不对称的**（internal/controlplane/pending.go）：
// 解析成功且已过期 → 当即把这条删掉；解析失败 → 一条也不删，永久留在板上。
// 所以这里按「能不能解析」分流，而不是按「过没过期」：
//
//   - 合法值原样透传。已经过期的合法值也透传——host 会把它剔掉，这正是终态覆盖想要的落点
//     （一张过期件本就该从板上消失）；把它改写成未来时刻反而会让一张批不了的卡再挂一个窗口。
//   - 缺失/非法值换成 now+pendingTTL。推一个空串等于推一张永不消失的卡，比不推更糟。
//
// 因此正常 pending（ExpiresAt 合法且在未来）推上去的仍是它自己的时刻，每 tick 同一字节，
// upsert 幂等。
func boardExpiry(raw string, now time.Time) string {
	if _, err := time.Parse(time.RFC3339, raw); err == nil {
		return raw
	}
	return now.Add(pendingTTL).UTC().Format(time.RFC3339)
}

// boardPayloadAt 把一份 pending 映射成 host 的 pendingItem（internal/controlplane/pending.go），
// now 由调用方给（便于用例钉死时间）。
//
// 逐字段手写映射，而不是把 Pending 整个塞过去：两边的字段名与形状都不一样。
// Pending.Labels 是 []string，pendingItem.Items 是 []map[string]any——直接塞 Pending 会让
// host 的 json.Unmarshal 静默丢掉对不上的字段，页面上只剩**一张没有明细的卡**。
//
// state（awaiting_approval / done / rejected / expired / stale）是 spec §3.4 加在条目上的
// 展示字段。当前 pendingItem 还没有它，host 会静默忽略；保留它是为了页面侧接上之后不必再改
// agent 的 payload（Task 12 的文档修订也是按「条目带 state」写的）。
func boardPayloadAt(p Pending, state PendingState, now time.Time) map[string]any {
	items := make([]map[string]any, 0, len(p.Labels))
	for _, l := range p.Labels {
		items = append(items, map[string]any{"label": l})
	}
	roles := p.RequiredRoles
	if roles == nil {
		// 统一成空数组：null 与 [] 在 Go 里都读得成空切片，但页面少一件判 null 的事。
		roles = []string{}
	}
	out := map[string]any{
		"planId":        p.PlanID,
		"by":            p.By, // 只是展示文案：host 一律用 token 的 sub 覆写它（不是来源凭证）
		"summary":       p.Summary,
		"items":         items,
		"requiredRoles": roles,
		"expiresAt":     boardExpiry(p.ExpiresAt, now),
		"state":         string(state),
	}
	if p.RequestID != nil {
		out["requestId"] = *p.RequestID
	}
	return out
}

// boardPayload 是生产入口：时间取当前时刻。
func boardPayload(p Pending, state PendingState) map[string]any {
	return boardPayloadAt(p, state, time.Now().UTC())
}

// RepushBoard 把 pending/ 里现存的**活条目**幂等 upsert 到公告板，返回推送条数。
//
// 三类条目会被跳过，理由各不相同：
//
//  1. archived/ 里已有同名件的。Archive 是「写归档件 → 删 pending 件」两步，中间失败
//     （进程被杀、磁盘写失败）会留下同名双份，而 List() 只读 pending/，照样把它交出来。
//     照它重推会让板上挂一张**早已处理完的卡**，直到 expiresAt 才消失。所以这里跳过，
//     并顺手删掉残留的 pending 件（它就是那份删了一半的垃圾）——归档件一个字都不动。
//  2. state=resuming 的。那是「这条 confirm 已被接受过、进程崩在执行中途」的标记：
//     按待批重推等于让审批人**再批一次同一份计划**，重批是二次副作用最便宜的入口。
//  3. 其余非 awaiting_approval 的（pending/ 里出现终态说明有人手工放错了文件）：
//     不猜、不推。
//
// 公告板是 read-model，推不上去只影响展示；但错误仍然返回给调用方决定要不要记日志、
// 下个 tick 再来——吞掉会让「推不上去」变成永远没人知道的事。
func RepushBoard(ctx context.Context, client *HostClient, store *StateStore) (int, error) {
	if store == nil {
		// 「没有挂起通道」是装配问题，不是「零条可推」：静默返回 0 会让配置错误永远看不见。
		return 0, fmt.Errorf("agent: 没有挂起通道（StateStore 为 nil），公告板无可重推")
	}
	if client == nil {
		return 0, fmt.Errorf("agent: 没有公告板通道（HostClient 为 nil）")
	}
	items, err := store.List()
	if err != nil {
		return 0, err
	}
	now := time.Now().UTC() // 一批条目共用同一时刻：同一 tick 推出的 expiresAt 不会互相漂移
	pushed := 0
	for _, p := range items {
		archived, found, aerr := readPending(store.archivedPath(p.PlanID))
		if aerr != nil {
			// 归档件坏了：既不能确定它已完成，也不能确定它没完成。这轮不推
			// （板上保持上一轮的样子），也不推一张可能早已完成的待批卡。
			log.Printf("agent: 公告板重推跳过 %s：归档件不可解析（%v）", p.PlanID, aerr)
			continue
		}
		if found {
			// 走到这里就是「同名双份」的残留现场（正常归档会删掉 pending 件）。
			if !isTerminalState(archived.State) {
				log.Printf("agent: 公告板重推跳过 %s：归档件状态 %q 不是终态（不该出现在 archived/）",
					p.PlanID, archived.State)
			}
			if rerr := os.Remove(store.pendingPath(p.PlanID)); rerr != nil {
				log.Printf("agent: 清掉已归档 %s 的残留 pending 失败：%v", p.PlanID, rerr)
			} else {
				log.Printf("agent: 清掉已归档 %s 的残留 pending（归档状态 %s）：这一轮不重推",
					p.PlanID, archived.State)
			}
			continue
		}
		if p.State != StateAwaitingApproval {
			log.Printf("agent: 公告板重推跳过 %s（state=%s）：只有待批件才按 awaiting 重推",
				p.PlanID, p.State)
			continue
		}
		if perr := client.PushPending(ctx, boardPayloadAt(p, p.State, now)); perr != nil {
			return pushed, perr
		}
		pushed++
	}
	return pushed, nil
}

// finishTerminal 推一条终态公告板覆盖（state = done / rejected / expired / stale）。
//
// **它只推公告板，绝不写 result 段**：计划级 result 在归档之前就由 resume.go 的
// writePlanResult 写好了（auditId 复用挂起时那条 dispatched，approvedBy 记审批人）。
// 同一个 auditId 再写一条 result，计划级就从两段变三段，而且两条 result 迟早给出
// 互相矛盾的结论（一条 ok 一条 error）。计划正文里那句「写计划级 result 段并推终态覆盖」
// 是 Task 8 落地前的旧注释，已作废。
//
// 返回错误由调用方决定要不要记日志：归档本身已经成功，推送失败不该把归档回滚成
// 「没归档」——那会让同一份 confirm 被重放去执行第二次（幂等键挡得住副作用，挡不住
// 重复审计与状态漂移）。
func finishTerminal(ctx context.Context, client *HostClient, p Pending, state PendingState) error {
	if client == nil {
		return fmt.Errorf("agent: 没有公告板通道（HostClient 为 nil），无法推终态覆盖 %s", p.PlanID)
	}
	return client.PushPending(ctx, boardPayload(p, state))
}

// PushTerminal 是 finishTerminal 的导出包装：cmd/agent 的恢复段在 Resume 成功返回后，
// 用 store.LoadArchived(planId) 取回归档件再调它推一条终态覆盖。
//
// 之所以要这个出口：Resume 的入参里只有宿主地址、没有带凭证的 HostClient（签名不改），
// 所以恢复路径只归档、把「推终态」留给调用方；而 finishTerminal 未导出，调用方够不着。
//
// 语义一个字没改，边界仍是 finishTerminal 那两条：
//   - 只推公告板，绝不写 result 段（计划级 result 在归档之前就由 resume.go 写过）；
//   - 推失败**不要回滚归档**——归档已成功，回滚会让同一份 confirm 被重放执行第二次。
//     错误只该记日志；重放时归档件已在，再推一次是幂等 upsert。
func PushTerminal(ctx context.Context, client *HostClient, p Pending, state PendingState) error {
	return finishTerminal(ctx, client, p, state)
}

// isTerminalState 判断是不是终态：pending/ 只该有 awaiting_approval 与 resuming，
// archived/ 只该有这四个。
func isTerminalState(s PendingState) bool {
	switch s {
	case StateDone, StateRejected, StateExpired, StateStale:
		return true
	}
	return false
}
