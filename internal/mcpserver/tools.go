package mcpserver

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// toolHandler 是工具实现：返回一段给人/模型读的正文，以及「这是不是一个失败」。
//
// 业务失败用 isError=true 表达（例如「这个动作不是只读的」「没有 token」），
// 但不等于协议错误——模型需要读到原因，而不是看到一个 -32603。
type toolHandler func(s *Server, args json.RawMessage) (string, bool)

type toolSpec struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

func toolSpecs() []toolSpec {
	empty := map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{}}
	return []toolSpec{
		{
			Name: "registry_list",
			Description: "列出向注册中心自注册且仍然存活的模块，包含心跳年龄、声明能力与每个动作的 kind。" +
				"数据与宿主 GET /tenon/modules 同源。",
			InputSchema: empty,
		},
		{
			Name: "action_invoke",
			Description: "调用一个**只读**（kind=read）动作并返回其结果。写动作一律拒绝——" +
				"写只能通过计划评审并由宿主签票执行。",
			InputSchema: map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"required":             []string{"actionId"},
				"properties": map[string]any{
					"actionId": map[string]any{"type": "string", "description": "形如 inventory.GetStock"},
					"args":     map[string]any{"type": "object", "description": "动作入参"},
				},
			},
		},
		{
			Name: "plan_preview",
			Description: "对一份计划做闸门预检：策略、授权、预演。只给结论，不落卡、不执行。" +
				"真正的待批卡由 agent 在收到人的请求后生成。",
			InputSchema: map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"required":             []string{"steps"},
				"properties": map[string]any{
					"triggerEventId": map[string]any{
						"type":        "string",
						"description": "可选。宿主签发的 syntheticEventId；带上才会被判成 origin=request。",
					},
					"steps": map[string]any{
						"type": "array",
						"items": map[string]any{
							"type":     "object",
							"required": []string{"action"},
							"properties": map[string]any{
								"action": map[string]any{"type": "string"},
								"args":   map[string]any{"type": "object"},
							},
						},
					},
				},
			},
		},
		{
			Name: "request_submit",
			Description: "把用户的**原话**交给 agent，让它自己规划、过三道闸门、必要时落一张待批卡。" +
				"（需要人工 token。）本工具不执行任何动作、不决定计划内容、也不造卡——" +
				"计划的所有权在 agent 手里，这样审计里记的才是「谁的计划」。" +
				"用户明确要求「去做某件事」时才用；只是问问题、查数据不要用它。",
			InputSchema: map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"required":             []string{"text"},
				"properties": map[string]any{
					"text": map[string]any{
						"type":        "string",
						"description": "用户的原话，不要改写、不要补充他没说过的事实",
					},
				},
			},
		},
		{
			Name: "pending_list",
			Description: "列出待批与已处理的计划条目（需要人工 token）。" +
				"列完后**必须**用 ask_user_question 让用户逐张选「允许执行」或「驳回」，" +
				"再按用户的选择调用 pending_decide —— 不要自己替用户决定。",
			InputSchema: empty,
		},
		{
			Name: "pending_decide",
			Description: "以人的身份批准或驳回一张待批卡（需要人工 token，且角色需匹配）。" +
				"批准后由 agent 恢复执行，本工具不执行任何动作。" +
				"调用前必须先用 ask_user_question 把这张卡摆给用户选「允许执行 / 驳回」，" +
				"decision 只能来自用户的这次选择。",
			InputSchema: map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"required":             []string{"planId", "decision"},
				"properties": map[string]any{
					"planId":   map[string]any{"type": "string"},
					"decision": map[string]any{"type": "string", "enum": []string{"approve", "reject"}},
				},
			},
		},
		{
			Name:        "audit_query",
			Description: "按 planId 查宿主审计记录（dispatched / result）。",
			InputSchema: map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"required":             []string{"planId"},
				"properties": map[string]any{
					"planId": map[string]any{"type": "string"},
					"since":  map[string]any{"type": "string", "description": "可选，RFC3339 或序号"},
				},
			},
		},
	}
}

func toolHandlers() map[string]toolHandler {
	return map[string]toolHandler{
		"registry_list":  toolRegistryList,
		"action_invoke":  toolActionInvoke,
		"plan_preview":   toolPlanPreview,
		"request_submit": toolRequestSubmit,
		"pending_list":   toolPendingList,
		"pending_decide": toolPendingDecide,
		"audit_query":    toolAuditQuery,
	}
}

// toolRequestSubmit 把人的原话投进入站队列（POST /tenon/requests）。
//
// 为什么这条不能省：入站帧是**唯一**能开回合的入口。没有它，对话层只能预检与批准，
// 却没有任何东西可批——把页面上的输入框摘掉之后，系统就再没有人说话的地方了。
//
// 为什么它不违反「对话层不造卡」：它只搬运原话。计划由 agent 生成，卡由 agent 落，
// 审计里的 plan 归属不变；本工具拿的是 human token，连 tickets:issue 都调不到。
func toolRequestSubmit(s *Server, args json.RawMessage) (string, bool) {
	var in struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "参数需要 text（用户的原话）", true
	}
	text := strings.TrimSpace(in.Text)
	if text == "" {
		return "text 不能为空：这里要放用户说过的话，不是摘要", true
	}
	// 宿主侧上限 2000 字符；先在这里挡住，比让宿主回 400 更好读。
	if len([]rune(text)) > 2000 {
		return fmt.Sprintf("原话 %d 字符，超过宿主 2000 字符上限——拆成两条说", len([]rune(text))), true
	}
	if s.humanToken() == "" {
		// 没有身份就不投递：请求帧会带着某个人名进审计，名字不能是猜的。
		return "没有可用的人工身份（TENON_MCP_HUMAN_TOKEN 或 TENON_MCP_USER/PASSWORD），拒绝提交请求。", true
	}

	// by 不填：宿主用 token 的 sub 覆写。填了也只是被判为无效并丢弃。
	frame := map[string]any{"type": "tenon.request", "text": text}
	body, _ := json.Marshal(frame)
	status, raw, err := s.doAuthed(http.MethodPost, s.Host+"/tenon/requests", body)
	if err != nil {
		return "提交请求失败：" + err.Error(), true
	}
	if status < 200 || status > 299 {
		return fmt.Sprintf("请求被拒（HTTP %d）：%s", status, truncate(string(raw), 300)), true
	}

	var resp struct {
		Seq              int    `json:"seq"`
		Durable          bool   `json:"durable"`
		RequestID        string `json:"requestId"`
		SyntheticEventID string `json:"syntheticEventId"`
	}
	_ = json.Unmarshal(raw, &resp)

	note := "已交给 agent（seq " + strconv.Itoa(resp.Seq) + "）。"
	if resp.RequestID != "" {
		note += " requestId=" + resp.RequestID + "；"
	}
	note += "它现在会自己读注册表、规划、过策略/授权/预演三道闸门。"
	note += "要批准的计划会出现在 pending_list 里——先问用户，再调 pending_decide。"
	if !resp.Durable {
		note += "（注意：宿主回的 durable=false，这帧只进了内存，重启会丢；不要重试，重试会造出第二条请求。）"
	}
	return note, false
}

// ---------- 注册中心 ----------

type registryEntry struct {
	ID       string `json:"id"`
	BaseURL  string `json:"baseURL"`
	State    string `json:"state"`
	Enabled  bool   `json:"enabled"`
	LastSeen string `json:"lastSeen"`
	Manifest struct {
		Module struct {
			Title string `json:"title"`
		} `json:"module"`
		Actions []struct {
			ID   string `json:"id"`
			Kind string `json:"kind"`
		} `json:"actions"`
		Capabilities []string `json:"capabilities"`
	} `json:"manifest"`
}

func (s *Server) fetchRegistry() ([]registryEntry, error) {
	status, raw, err := s.do(http.MethodGet, s.Host+"/tenon/modules", nil, nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("GET /tenon/modules 返回 %d：%s", status, truncate(string(raw), 200))
	}
	var body struct {
		Modules []registryEntry `json:"modules"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, fmt.Errorf("注册中心响应不是 JSON：%w", err)
	}
	return body.Modules, nil
}

func toolRegistryList(s *Server, _ json.RawMessage) (string, bool) {
	mods, err := s.fetchRegistry()
	if err != nil {
		return "读注册中心失败：" + err.Error(), true
	}
	if len(mods) == 0 {
		return "没有任何模块注册（或全部掉线）。", false
	}
	sort.Slice(mods, func(i, j int) bool { return mods[i].ID < mods[j].ID })

	var b strings.Builder
	fmt.Fprintf(&b, "已注册模块 %d 个：\n", len(mods))
	for _, m := range mods {
		name := m.Manifest.Module.Title
		if name == "" {
			name = m.ID
		}
		fmt.Fprintf(&b, "\n- %s（%s）state=%s 心跳=%s\n", name, m.ID, m.State, ageText(m.LastSeen))
		if len(m.Manifest.Capabilities) > 0 {
			fmt.Fprintf(&b, "  能力：%s\n", strings.Join(m.Manifest.Capabilities, ", "))
		}
		for _, a := range m.Manifest.Actions {
			write := ""
			if a.Kind != "read" {
				write = "（写，需计划与票据）"
			}
			fmt.Fprintf(&b, "  · %s [%s]%s\n", a.ID, a.Kind, write)
		}
	}
	b.WriteString("\n注：只列出 enabled && alive 的模块；掉线的会从列表里消失。")
	return b.String(), false
}

// ---------- 只读动作 ----------

func toolActionInvoke(s *Server, args json.RawMessage) (string, bool) {
	var in struct {
		ActionID string          `json:"actionId"`
		Args     json.RawMessage `json:"args"`
	}
	if err := json.Unmarshal(args, &in); err != nil || in.ActionID == "" {
		return "参数需要 actionId（以及可选的 args）", true
	}
	if len(in.Args) == 0 {
		in.Args = json.RawMessage(`{}`)
	}

	mods, err := s.fetchRegistry()
	if err != nil {
		return "读注册中心失败：" + err.Error(), true
	}
	entry, kind := lookupAction(mods, in.ActionID)
	if entry == nil {
		return "注册中心里没有这个动作：" + in.ActionID +
			"（先 registry_list 看清单；不在清单里的动作不能凭空调用）", true
	}
	if kind != "read" {
		return fmt.Sprintf("%s 是 %s 动作，对话层不能直接调用。"+
			"写动作只能变成待批卡：用 plan_preview 看预检结论，再让人发请求、由 agent 落卡。",
			in.ActionID, kind), true
	}

	status, raw, err := s.do(http.MethodPost, entry.BaseURL+"/tenon/actions/"+in.ActionID, in.Args,
		map[string]string{"Tenon-Version": "0.1", "Content-Type": "application/json"})
	if err != nil {
		return "调用失败：" + err.Error(), true
	}
	if status < 200 || status > 299 {
		return fmt.Sprintf("%s 返回 %d：%s", in.ActionID, status, truncate(string(raw), 400)), true
	}
	return fmt.Sprintf("%s → %s", in.ActionID, string(raw)), false
}

func lookupAction(mods []registryEntry, actionID string) (*registryEntry, string) {
	for i := range mods {
		for _, a := range mods[i].Manifest.Actions {
			if a.ID == actionID {
				return &mods[i], a.Kind
			}
		}
	}
	return nil, ""
}

// ---------- 计划预检 ----------

func toolPlanPreview(s *Server, args json.RawMessage) (string, bool) {
	var in struct {
		TriggerEventID string `json:"triggerEventId"`
		Steps          []struct {
			Action string          `json:"action"`
			Args   json.RawMessage `json:"args"`
		} `json:"steps"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "参数坏：" + err.Error(), true
	}
	if len(in.Steps) == 0 {
		return "steps 不能为空：零步计划没有可执行语义，闸门会直接判 deny。", true
	}

	pln := fmt.Sprintf("pln_mcp_preview_%d", time.Now().UnixNano())
	steps := make([]map[string]any, 0, len(in.Steps))
	for i, st := range in.Steps {
		var parsed any = map[string]any{}
		if len(st.Args) > 0 {
			if err := json.Unmarshal(st.Args, &parsed); err != nil {
				return fmt.Sprintf("第 %d 步 args 不是 JSON 对象：%v", i+1, err), true
			}
		}
		steps = append(steps, map[string]any{
			"stepId": fmt.Sprintf("s%d", i+1),
			"action": st.Action,
			"args":   parsed,
		})
	}

	var b strings.Builder
	fmt.Fprintf(&b, "预检计划 %s（%d 步）——只给结论，不会落卡、不会执行\n", pln, len(steps))

	// 1) 策略：整份计划一次评估（宿主按 deny > approve > auto 合并）。
	policyBody := map[string]any{
		"plan": map[string]any{"planId": pln, "steps": steps},
	}
	if in.TriggerEventID != "" {
		policyBody["triggerEventId"] = in.TriggerEventID
	}
	var policy struct {
		Outcome   string   `json:"outcome"`
		Approvers []string `json:"approvers"`
		Reasons   []string `json:"reasons"`
	}
	if err := s.postJSON(s.Host+"/tenon/policies:evaluate", policyBody, &policy); err != nil {
		return "策略评估失败：" + err.Error(), true
	}
	fmt.Fprintf(&b, "\n策略：%s", policy.Outcome)
	if len(policy.Approvers) > 0 {
		fmt.Fprintf(&b, "（审批人：%s）", strings.Join(policy.Approvers, ", "))
	}
	if len(policy.Reasons) > 0 {
		fmt.Fprintf(&b, " 理由：%s", strings.Join(policy.Reasons, ", "))
	}
	b.WriteString("\n")

	verdict := true
	if policy.Outcome == "deny" {
		verdict = false
	}

	// 2) 授权：逐 step（宿主按动作计配额，预检必须 charge:false 否则每预检一次都扣账）。
	for i, st := range steps {
		grantBody := map[string]any{
			"agent":      s.AgentID,
			"onBehalfOf": s.OnBehalfOf,
			"action":     st["action"],
			"args":       st["args"],
			"charge":     false,
		}
		var grant struct {
			Allowed bool   `json:"allowed"`
			Reason  string `json:"reason"`
		}
		if err := s.postJSON(s.Host+"/tenon/grants:evaluate", grantBody, &grant); err != nil {
			return "授权评估失败：" + err.Error(), true
		}
		if !grant.Allowed {
			verdict = false
			fmt.Fprintf(&b, "授权：第 %d 步（%v）拒绝：%s\n", i+1, st["action"], grant.Reason)
		}
	}
	if verdict {
		b.WriteString("授权：全部允许\n")
	}

	// 3) 预演：只对写动作（读动作按协议不预演）。
	mods, err := s.fetchRegistry()
	if err != nil {
		return b.String() + "\n预演：读注册中心失败：" + err.Error(), true
	}
	for i, st := range steps {
		action, _ := st["action"].(string)
		entry, kind := lookupAction(mods, action)
		if entry == nil {
			verdict = false
			fmt.Fprintf(&b, "预演：第 %d 步 %s 不在注册表里\n", i+1, action)
			continue
		}
		if kind != "write" {
			continue
		}
		argsRaw, _ := json.Marshal(st["args"])
		status, raw, err := s.do(http.MethodPost,
			entry.BaseURL+"/tenon/actions/"+action+":dry-run", argsRaw,
			map[string]string{"Tenon-Version": "0.1", "Content-Type": "application/json"})
		if err != nil {
			return b.String() + "\n预演失败：" + err.Error(), true
		}
		if status == http.StatusNotFound {
			// 404 = 模块没实现预演。这是可判定信号，按协议必须当 unsupported 并强制人工。
			verdict = false
			fmt.Fprintf(&b, "预演：第 %d 步 %s 返回 404（模块未实现预演）→ 必须人工\n", i+1, action)
			continue
		}
		var wrap struct {
			DryRun struct {
				Status string `json:"status"`
			} `json:"dryRun"`
		}
		if err := json.Unmarshal(raw, &wrap); err != nil {
			return b.String() + fmt.Sprintf("\n预演响应不是 JSON（HTTP %d）：%s", status, truncate(string(raw), 200)), true
		}
		fmt.Fprintf(&b, "预演：第 %d 步 %s → %s\n", i+1, action, wrap.DryRun.Status)
		if wrap.DryRun.Status != "verified" {
			verdict = false
		}
	}

	if verdict {
		b.WriteString("\n结论：预检通过（策略放行、授权允许、预演 verified）。")
	} else {
		b.WriteString("\n结论：预检未通过，见上文逐项结论。")
	}
	b.WriteString("\n注意：预检不是授权凭证。真正执行要由人发请求 → agent 落卡 → 人批准 → 宿主签票。")
	return b.String(), false
}

// ---------- 待批 ----------

func toolPendingList(s *Server, _ json.RawMessage) (string, bool) {
	status, raw, err := s.doAuthed(http.MethodGet, s.Host+"/tenon/pending", nil)
	if err != nil && status == 0 {
		return "没有可用的工人身份（TENON_MCP_HUMAN_TOKEN 或 TENON_MCP_USER/PASSWORD 都没配）。" +
			"这不是故障：待批条目属于个人视图，必须挂在某个具体人名下。", true
	}
	if err != nil {
		return "读取失败：" + err.Error(), true
	}
	if status != http.StatusOK {
		return fmt.Sprintf("GET /tenon/pending 返回 %d：%s", status, truncate(string(raw), 300)), true
	}

	var body struct {
		Items []struct {
			PlanID        string   `json:"planId"`
			State         string   `json:"state"`
			Summary       string   `json:"summary"`
			RequiredRoles []string `json:"requiredRoles"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return "响应不是 JSON：" + err.Error(), true
	}
	if len(body.Items) == 0 {
		return "当前没有待批或已处理条目。\n" +
			"（待批列表为空有三种原因：agent 还在规划；计划判 auto 已直接执行；或卡片已过期。）", false
	}
	var b strings.Builder
	fmt.Fprintf(&b, "共 %d 条：\n", len(body.Items))
	awaiting := 0
	for _, it := range body.Items {
		fmt.Fprintf(&b, "\n- %s  state=%s\n  %s\n", it.PlanID, it.State, it.Summary)
		if len(it.RequiredRoles) > 0 {
			fmt.Fprintf(&b, "  需要角色：%s\n", strings.Join(it.RequiredRoles, ", "))
		}
		if it.State == "awaiting_approval" || it.State == "" {
			awaiting++
			// 每张卡都带一句「怎么问」：模型读到条目后最想做的事是立刻调 pending_decide，
			// 而它手里的服务账号口令真能批准。把选项卡片写成下一步，比在系统提示里写纪律有用。
			fmt.Fprintf(&b, "  下一步：ask_user_question 让用户选「允许执行」/「驳回」（planId=%s）\n", it.PlanID)
		}
	}
	if awaiting > 0 {
		// 这段是**行为指令**，不是说明文字：没有它，模型会把「列出待批」直接接成「批准」。
		b.WriteString("\n下一步（必做）：对上面每张 state=awaiting_approval 的卡，先调用 ask_user_question，")
		b.WriteString("选项写「允许执行」(推荐) 与「驳回」，question 里带上 planId 与摘要；")
		b.WriteString("拿到用户的选择后，才调用 pending_decide（decision=approve|reject）。\n")
		b.WriteString("不要替用户选：这张卡存在的理由就是有人要看一眼。\n")
	}
	return b.String(), false
}

func toolPendingDecide(s *Server, args json.RawMessage) (string, bool) {
	var in struct {
		PlanID   string `json:"planId"`
		Decision string `json:"decision"`
	}
	if err := json.Unmarshal(args, &in); err != nil || in.PlanID == "" {
		return "参数需要 planId 与 decision（approve|reject）", true
	}
	if in.Decision != "approve" && in.Decision != "reject" {
		return "decision 只能是 approve 或 reject", true
	}
	if s.humanToken() == "" {
		// 没有身份就什么都不做：这条分支必须早于任何副作用。
		return "没有可用的人工身份（TENON_MCP_HUMAN_TOKEN 或 TENON_MCP_USER/PASSWORD），拒绝执行审批。", true
	}

	frame := map[string]any{
		"type":      "tenon.confirm",
		"confirmId": fmt.Sprintf("cfm_mcp_%d", time.Now().UnixNano()),
		"planId":    in.PlanID,
		"decision":  in.Decision,
		"patch":     nil,
		"at":        time.Now().UTC().Format(time.RFC3339),
	}
	body, _ := json.Marshal(frame)
	status, raw, err := s.doAuthed(http.MethodPost, s.Host+"/tenon/requests", body)
	if err != nil {
		return "提交审批失败：" + err.Error(), true
	}
	if status < 200 || status > 299 {
		return fmt.Sprintf("审批被拒（HTTP %d）：%s", status, truncate(string(raw), 300)), true
	}
	verb := "已批准"
	if in.Decision == "reject" {
		verb = "已驳回"
	}
	return fmt.Sprintf("%s %s。批准后由 agent 恢复执行并由宿主签票；本工具本身不执行动作。", verb, in.PlanID), false
}

// ---------- 审计 ----------

func toolAuditQuery(s *Server, args json.RawMessage) (string, bool) {
	var in struct {
		PlanID string `json:"planId"`
		Since  string `json:"since"`
	}
	if err := json.Unmarshal(args, &in); err != nil || in.PlanID == "" {
		return "参数需要 planId", true
	}
	url := s.Host + "/tenon/audit?planId=" + in.PlanID
	if in.Since != "" {
		url += "&since=" + in.Since
	}
	status, raw, err := s.do(http.MethodGet, url, nil, nil)
	if err != nil {
		return "查询失败：" + err.Error(), true
	}
	if status != http.StatusOK {
		return fmt.Sprintf("GET /tenon/audit 返回 %d：%s", status, truncate(string(raw), 300)), true
	}
	return string(raw), false
}

// ---------- 小工具 ----------

func (s *Server) do(method, url string, body []byte, headers map[string]string) (int, []byte, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		return 0, nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := s.client().Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, raw, nil
}

func (s *Server) postJSON(url string, payload any, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	status, raw, err := s.do(http.MethodPost, url, body, map[string]string{
		"Content-Type":  "application/json",
		"Tenon-Version": "0.1",
	})
	if err != nil {
		return err
	}
	if status < 200 || status > 299 {
		return fmt.Errorf("HTTP %d：%s", status, truncate(string(raw), 300))
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("响应不是 JSON：%w", err)
		}
	}
	return nil
}

func ageText(iso string) string {
	t, err := time.Parse(time.RFC3339Nano, iso)
	if err != nil {
		return "未知"
	}
	d := time.Since(t)
	if d < 0 {
		d = 0
	}
	sec := int(d.Seconds())
	switch {
	case sec < 60:
		return fmt.Sprintf("%d 秒前", sec)
	case sec < 3600:
		return fmt.Sprintf("%d 分 %d 秒前", sec/60, sec%60)
	default:
		return fmt.Sprintf("%d 小时前", sec/3600)
	}
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// doAuthed 发一个带人工身份的请求，并在 401 时换一次 token 重试。
//
// 为什么要重试一次：token 会过期，而 dsh 里的 MCP 进程可能活很久。401 之后不重登，
// 症状是「一开始能批，过一会儿就不能批」——这类间歇失败最难归因。
func (s *Server) doAuthed(method, url string, body []byte) (int, []byte, error) {
	token := s.humanToken()
	if token == "" {
		return 0, nil, errNoHumanIdentity
	}
	status, raw, err := s.do(method, url, body, map[string]string{
		"Authorization": "Bearer " + token,
		"Content-Type":  "application/json",
	})
	if err != nil || status != http.StatusUnauthorized {
		return status, raw, err
	}

	s.mu.Lock()
	s.token = ""
	s.mu.Unlock()

	token = s.humanToken()
	if token == "" {
		return status, raw, nil
	}
	return s.do(method, url, body, map[string]string{
		"Authorization": "Bearer " + token,
		"Content-Type":  "application/json",
	})
}

var errNoHumanIdentity = fmt.Errorf("没有可用的人工身份")
