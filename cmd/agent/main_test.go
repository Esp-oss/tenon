package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"tenon.dev/internal/spec"
)

// clearAgentEnv 清空本进程读取的全部环境变量，让每个用例从同一个起点开始。
func clearAgentEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		envHost, envSkillDir, envAgentID, envOnBehalfOf, envPollInterval,
		envOnce, envCursorFile, envSpecDir,
	} {
		t.Setenv(name, "")
	}
}

func TestAgentRefusesToStartWithoutLLMKey(t *testing.T) {
	// 缺 key 必须启动即失败，不得静默降级（设计 §5）。
	// 两个来源都清空：文件优先于环境变量，只清一个测不出东西。
	t.Setenv("TENON_LLM_API_KEY", "")
	t.Setenv("TENON_LLM_API_KEY_FILE", "")

	if err := run(context.Background()); err == nil {
		t.Fatal("缺 key 必须启动即失败，不得静默降级")
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	clearAgentEnv(t)
	t.Setenv("TENON_LLM_API_KEY", "sk-test-not-used")
	t.Setenv(envOnBehalfOf, "user:alice")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("缺省配置应当可用，得到错误：%v", err)
	}
	if cfg.host != defaultHost {
		t.Fatalf("host 缺省值不对：%q", cfg.host)
	}
	if cfg.skillDir != defaultSkillDir {
		t.Fatalf("skill 目录缺省值不对：%q", cfg.skillDir)
	}
	if cfg.agentID != defaultAgentID {
		t.Fatalf("agent id 缺省值不对：%q", cfg.agentID)
	}
	if cfg.pollInterval != defaultPollInterval {
		t.Fatalf("轮询间隔缺省值不对：%v", cfg.pollInterval)
	}
	if cfg.once {
		t.Fatal("TENON_ONCE 缺省必须是假")
	}
	if cfg.specDir != spec.DefaultDir {
		t.Fatalf("schema 目录缺省值不对：%q", cfg.specDir)
	}
}

func TestLoadConfigRequiresOnBehalfOf(t *testing.T) {
	clearAgentEnv(t)
	t.Setenv("TENON_LLM_API_KEY", "sk-test-not-used")

	// 不提供默认代行身份：编一个身份等于让审计记录归属于不存在的人。
	if _, err := loadConfig(); err == nil {
		t.Fatal("缺 TENON_ON_BEHALF_OF 必须启动即失败")
	}
}

func TestLoadConfigRejectsBadValues(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
	}{
		{name: "host 不是 URL", env: map[string]string{envHost: "not a url"}},
		{name: "host scheme 不对", env: map[string]string{envHost: "ftp://127.0.0.1:8080"}},
		{name: "轮询间隔非法", env: map[string]string{envPollInterval: "一秒"}},
		{name: "轮询间隔为零", env: map[string]string{envPollInterval: "0s"}},
		{name: "once 取值无法识别", env: map[string]string{envOnce: "maybe"}},
		{name: "agent id 词法非法", env: map[string]string{envAgentID: "9 bad id"}},
		{name: "委托人词法非法", env: map[string]string{envOnBehalfOf: "alice"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearAgentEnv(t)
			t.Setenv("TENON_LLM_API_KEY", "sk-test-not-used")
			t.Setenv(envOnBehalfOf, "user:alice")
			for name, value := range tc.env {
				t.Setenv(name, value)
			}
			if _, err := loadConfig(); err == nil {
				t.Fatalf("%s：必须报错", tc.name)
			}
		})
	}
}

func TestCursorStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cursors.json")

	store, err := loadCursors(path)
	if err != nil {
		t.Fatalf("文件不存在应当是空游标，得到错误：%v", err)
	}
	if got := store.get("inventory"); got != "" {
		t.Fatalf("空游标应当返回空串，得到 %q", got)
	}

	store.set("inventory", "cur_out_3")
	store.set("quote", "cur_out_7")
	store.set("empty", "")
	if err := store.save(path); err != nil {
		t.Fatalf("写游标失败：%v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("游标文件没有落盘：%v", err)
	}

	again, err := loadCursors(path)
	if err != nil {
		t.Fatalf("重新加载游标失败：%v", err)
	}
	if again.get("inventory") != "cur_out_3" || again.get("quote") != "cur_out_7" {
		t.Fatalf("游标往返不一致：%+v", again.entries)
	}
	if _, ok := again.entries["empty"]; ok {
		t.Fatal("空游标不该写盘（回显空串等于从头读）")
	}
}

func TestPollIntervalIsHonoured(t *testing.T) {
	clearAgentEnv(t)
	t.Setenv("TENON_LLM_API_KEY", "sk-test-not-used")
	t.Setenv(envOnBehalfOf, "user:alice")
	t.Setenv(envPollInterval, "250ms")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig 失败：%v", err)
	}
	if cfg.pollInterval != 250*time.Millisecond {
		t.Fatalf("TENON_POLL_INTERVAL 没有生效：%v", cfg.pollInterval)
	}
}
