## 本地预推送检查（CI 的替代品）

`ci.yml` 需要一个带 `workflow` scope 的凭据才推得上去。在拿到之前，用这条钩子拿到**等效防线**：

```bash
git config core.hooksPath .githooks     # 一次性，本地配置、不入库
```

之后每次 `git push` 会先跑 `gofmt -l .` / `go vet ./...` / `go test ./... -count=1`，任一失败即拒绝推送（完整测试输出写到 `.git/pre-push-test.log`）。

跳过（明确知道为什么才用）：

```bash
SKIP_PRE_PUSH=1 git push
```

**为什么需要它**：P1 时期一个「会过期的时间夹具」在特性分支上躺了一整天——当天上午绿、下午红，而没人 push、CI 也从未真正跑过。钩子拦的正是这一类：**不是"代码写错了"，而是"时间过去了"**。
