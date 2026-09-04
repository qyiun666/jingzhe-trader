package itest

// 文档与代码的契约测试（不开 JZ_ITEST 也跑，因此每次 go test ./... 都会守住）。
//
// 为什么要这一层：这轮清理里最难查的问题不在代码里，而在"文档说的接口与代码给的不是一个"
// —— README 一处写 12 个工具一处写 14 个，外部 agent 照文档调用就拿到含义不明的失败。
// 同类问题会反复出现（改代码的人不会想到去改另一份文档），所以做成断言而不是靠人记。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"jingzhe-trader/internal/app"
	"jingzhe-trader/internal/config"
	"jingzhe-trader/internal/mcp"
	"jingzhe-trader/internal/store"
)

// repoFile 读仓库根下的文档（测试工作目录是本包目录）。
func repoFile(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", name))
	if err != nil {
		t.Fatalf("读 %s 失败: %v", name, err)
	}
	return string(b)
}

// dummyRuntime 用一套假凭据装配运行时：只为拿到"代码实际注册了什么"，全程不触网。
func dummyRuntime(t *testing.T) *app.Runtime {
	t.Helper()
	t.Setenv("JZ_TUSHARE_TOKEN", "dummy-token")
	t.Setenv("JZ_SERVER_API_TOKEN", "dummy-token")
	t.Setenv("JZ_MAIL_FROM", "from@example.com")
	t.Setenv("JZ_MAIL_PASSWORD", "auth-code")
	t.Setenv("JZ_MAIL_SMTP_HOST", "smtp.example.com")
	t.Setenv("JZ_MAIL_SMTP_PORT", "465")
	t.Setenv("JZ_MAIL_ENABLED", "true")
	t.Setenv("JZ_WATCH_MAIL_TO", "me@example.com")
	st, err := store.Open(filepath.Join(t.TempDir(), "docs.db"))
	if err != nil {
		t.Fatalf("打开临时库失败: %v", err)
	}
	t.Cleanup(func() {
		if cerr := st.Close(); cerr != nil {
			t.Errorf("关闭临时库失败: %v", cerr)
		}
	})
	ctx := t.Context()
	if err := st.ConfigRepo().Set(ctx, "account.initial_capital", "20000"); err != nil {
		t.Fatalf("写本金失败: %v", err)
	}
	cfg, err := config.Load(ctx, st)
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}
	rt, err := app.BuildRuntime(ctx, st, cfg)
	if err != nil {
		t.Fatalf("装配运行时失败: %v", err)
	}
	return rt
}

// liveToolNames 从真实 MCP 服务拿 tools/list 的结果（与常驻进程用的是同一份注册表）。
func liveToolNames(t *testing.T, rt *app.Runtime) []string {
	t.Helper()
	srv, err := mcp.New(rt.MCPDeps(), "docs-token")
	if err != nil {
		t.Fatalf("构造 MCP 服务失败: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, ts.URL+"/mcp",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer docs-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("tools/list 请求失败: %v", err)
	}
	defer resp.Body.Close()
	var out struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("解析 tools/list 失败: %v", err)
	}
	names := make([]string, 0, len(out.Result.Tools))
	for _, x := range out.Result.Tools {
		names = append(names, x.Name)
	}
	sort.Strings(names)
	return names
}

// TestDocsMatchToolRegistry MCP 工具的数量与名字，代码 / README / MCP-AGENT-GUIDE 三处必须一致。
func TestDocsMatchToolRegistry(t *testing.T) {
	rt := dummyRuntime(t)
	live := liveToolNames(t, rt)
	guide := repoFile(t, "MCP-AGENT-GUIDE.md")
	readme := repoFile(t, "README.md")

	docNames := map[string]bool{}
	reLine := regexp.MustCompile(`^(读类|写类)（(\d+)）：(.+)$`)
	var wantRead, wantWrite int
	for _, line := range strings.Split(guide, "\n") {
		m := reLine.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			continue
		}
		names := strings.Fields(strings.NewReplacer("`", "", "：", " ").Replace(m[3]))
		for _, n := range names {
			docNames[n] = true
		}
		if m[1] == "读类" {
			wantRead = len(names)
		} else {
			wantWrite = len(names)
		}
	}
	if len(docNames) == 0 {
		t.Fatal("MCP-AGENT-GUIDE.md 里没解析到读类/写类清单，这条契约测试守不住任何东西")
	}
	if len(live) != len(docNames) {
		t.Errorf("服务端注册 %d 个工具，文档列了 %d 个：%v vs %v", len(live), len(docNames), live, sortedKeys(docNames))
	}
	for _, n := range live {
		if !docNames[n] {
			t.Errorf("工具 %q 代码里有、文档里没有（agent 发现不了它）", n)
		}
	}
	for n := range docNames {
		if !contains(live, n) {
			t.Errorf("工具 %q 文档里有、代码里没有（agent 照文档调用必然失败）", n)
		}
	}
	if wantRead+wantWrite != len(live) {
		t.Errorf("文档分读的 %d + 写的 %d 不等于注册数 %d", wantRead, wantWrite, len(live))
	}

	check := func(where string, got, want int) {
		if got != want {
			t.Errorf("%s 写的是 %d，实际 %d", where, got, want)
		}
	}
	if m := regexp.MustCompile(`共 (\d+) 个`).FindStringSubmatch(guide); m == nil {
		t.Error("MCP-AGENT-GUIDE.md 的工具清单标题里没有「共 N 个」")
	} else {
		check("GUIDE 清单标题", atoi(m[1]), len(live))
	}
	if m := regexp.MustCompile(`(\d+) 个 MCP 工具\*\*（(\d+) 读 \+ (\d+) 写）`).FindStringSubmatch(readme); m == nil {
		t.Error("README 里没找到「N 个 MCP 工具（X 读 + Y 写）」这句")
	} else {
		check("README 工具总数", atoi(m[1]), len(live))
		check("README 读工具数", atoi(m[2]), wantRead)
		check("README 写工具数", atoi(m[3]), wantWrite)
	}
}

// TestDocsMatchTaskNames README 列出的任务名必须与调度器注册表一模一样。
func TestDocsMatchTaskNames(t *testing.T) {
	live := dummyRuntime(t).TaskNames()
	sort.Strings(live)
	for _, line := range strings.Split(repoFile(t, "README.md"), "\n") {
		if !strings.Contains(line, "手工执行单个任务") {
			continue
		}
		// 只取形如 xxx_yyy 的反引号词：同一句里还反引号了 serve 这类非任务词
		var doc []string
		for _, tok := range regexp.MustCompile("`([a-z]+_[a-z_]+)`").FindAllStringSubmatch(line, -1) {
			doc = append(doc, tok[1])
		}
		sort.Strings(doc)
		if strings.Join(doc, ",") != strings.Join(live, ",") {
			t.Errorf("README 的任务清单 %v 与注册表 %v 不一致（agent 照文档 run 会拿到未知任务）", doc, live)
		}
		return
	}
	t.Error("README 里没找到「手工执行单个任务」那一行，任务名契约没被守住")
}

// TestDocsPathsExist README / MCP-AGENT-GUIDE / docs/API.md 里点名的仓库路径必须真实存在。
//
// 重写过程删了不少文件与整目录（scripts/ 就是其一），文字里留下的引用没人回头改，
// 照着操作的人只会撞到 No such file。
func TestDocsPathsExist(t *testing.T) {
	// 前缀清单含 scripts/：本次 launchd 残留就是因为它指向 scripts/*.sh，而那个整目录已删
	re := regexp.MustCompile("`((?:cmd|internal|deploy|docs|itest|scripts)/[A-Za-z0-9_./-]+)`")
	for _, name := range []string{"README.md", "MCP-AGENT-GUIDE.md", "docs/API.md"} {
		seen := map[string]bool{}
		for _, m := range re.FindAllStringSubmatch(repoFile(t, name), -1) {
			p := m[1]
			if strings.Contains(p, "*") || seen[p] {
				continue
			}
			seen[p] = true
			if _, err := os.Stat(filepath.Join("..", p)); err != nil {
				t.Errorf("%s 引用了不存在的路径 %s（删掉的东西要连引用一起删）", name, p)
			}
		}
	}
}

// TestAPIDocToolCounts docs/API.md 的工具总数与读/写分组数必须与注册表一致。
//
// 这条以前只覆盖 README 与 GUIDE，于是 API.md 可以整节过期而全绿——
// 实测发现它的 trigger_task 参数、get_logs 返回键、§5 守护进程三处全是错的。
func TestAPIDocToolCounts(t *testing.T) {
	rt := dummyRuntime(t)
	live := liveToolNames(t, rt)
	api := repoFile(t, "docs/API.md")

	if m := regexp.MustCompile(`## 3\. 工具清单（(\d+) 个）`).FindStringSubmatch(api); m == nil {
		t.Error("docs/API.md 的 §3 标题里没有「工具清单（N 个）」")
	} else if atoi(m[1]) != len(live) {
		t.Errorf("docs/API.md §3 写的是 %s 个，实际注册 %d 个", m[1], len(live))
	}
	for _, c := range []struct {
		kind string
		want int
	}{{"读类", 5}, {"写类", 7}} {
		m := regexp.MustCompile(`### ` + c.kind + `（(\d+)）`).FindStringSubmatch(api)
		if m == nil {
			t.Errorf("docs/API.md 里没有「### %s（N）」这一节", c.kind)
			continue
		}
		if atoi(m[1]) != c.want {
			t.Errorf("docs/API.md 的 %s 写了 %s 个，实际 %d 个", c.kind, m[1], c.want)
		}
	}
	// 文档表格里点名的工具，注册表里必须真有；反过来也不能漏。
	// 只在 §3 这一节里扫：§2 的 `initialize`/`tools/list` 是 JSON-RPC 方法名，不是工具。
	toolSection := api
	if s := strings.Index(api, "### 读类"); s >= 0 {
		end := strings.Index(api[s:], "## 4.")
		if end > 0 {
			toolSection = api[s : s+end]
		}
	}
	docNames := map[string]bool{}
	reRow := regexp.MustCompile("(?m)^\\| `([a-z_]+)` \\|")
	for _, m := range reRow.FindAllStringSubmatch(toolSection, -1) {
		docNames[m[1]] = true
	}
	for _, n := range live {
		if !docNames[n] {
			t.Errorf("工具 %q 代码里有、docs/API.md 表格里没有", n)
		}
	}
	for n := range docNames {
		if !contains(live, n) {
			t.Errorf("工具 %q 写在 docs/API.md 里但代码没注册（照着调用必失败）", n)
		}
	}
}

// TestAPIDocTaskNames docs/API.md 列出的 trigger_task 任务名必须与调度器注册表一模一样。
//
// 这张表曾经写着 `freshness`/`screener`/`signal`/`t1_settle` —— 四个名字没有一个是真的，
// 照文档调用的 agent 每次都拿"未知任务"，而从测试输出上看不出接口有任何问题。
func TestAPIDocTaskNames(t *testing.T) {
	live := dummyRuntime(t).TaskNames()
	sort.Strings(live)
	api := repoFile(t, "docs/API.md")
	m := regexp.MustCompile("(?s)`trigger_task`.*?```\\n([^`]+?)\\n```").FindStringSubmatch(api)
	if m == nil {
		t.Fatal("docs/API.md 里没找到 trigger_task 那一节末尾用代码块列出的任务名清单")
	}
	doc := strings.Fields(m[1])
	sort.Strings(doc)
	if strings.Join(doc, ",") != strings.Join(live, ",") {
		t.Errorf("docs/API.md 的任务名 %v 与注册表 %v 不一致（照文档调用必拿未知任务）", doc, live)
	}
}

// TestAPIDocReadToolKeys docs/API.md 读类表格里点名的返回字段，必须是工具真的返回的那些顶层键。
//
// 实测抓到过两处：文档写 `traces[]` 而实际键是单数 `trace`；文档写 `candidates`/`signals`
// 而 get_brief 根本不返回它们（选股中间产物只在内存里传）。
// 这类偏差靠人不改文档就一定复发，所以按"实际调用一次、比对顶层键集合"来守。
func TestAPIDocReadToolKeys(t *testing.T) {
	rt := dummyRuntime(t)
	srv, err := mcp.New(rt.MCPDeps(), "docs-token")
	if err != nil {
		t.Fatalf("构造 MCP 服务失败: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	api := repoFile(t, "docs/API.md")
	// 只比对那些确实在库里造得出数据的只读工具
	for _, tool := range []string{"get_brief", "get_tickets", "get_positions", "get_portfolio", "get_logs"} {
		row := regexp.MustCompile("(?m)^\\| `" + tool + "` \\|[^|]*\\|([^|]*)\\|").FindStringSubmatch(api)
		if row == nil {
			t.Errorf("docs/API.md 里找不到 %s 这一行", tool)
			continue
		}
		actual := map[string]bool{}
		for _, k := range toolTopKeys(t, ts, tool) {
			actual[k] = true
		}
		mentioned := map[string]bool{}
		// 文档习惯把多个键塞进同一个反引号区间（`{trade_date, tickets[]}`），
		// 所以先拆区间、再从区间内提取标识符，否则会把真实字段判成"没提"。
		for _, span := range regexp.MustCompile("`([^`]*)`").FindAllStringSubmatch(row[1], -1) {
			for _, id := range regexp.MustCompile(`[a-z_]+`).FindAllString(span[1], -1) {
				mentioned[id] = true
			}
		}
		for k := range mentioned {
			if !actual[k] {
				t.Errorf("docs/API.md 说 %s 返回 `%s`，实际顶层键是 %v（文档编了一个不存在的字段）",
					tool, k, sortedKeys(actual))
			}
		}
		for k := range actual {
			if !mentioned[k] && k != "trade_date" {
				t.Errorf("%s 实际返回 `%s`，docs/API.md 那行没提（agent 发现不了这个字段）", tool, k)
			}
		}
	}
}

// toolTopKeys 真实调用一次只读工具，返回它业务 JSON 的顶层键集合。
func toolTopKeys(t *testing.T, ts *httptest.Server, tool string) []string {
	t.Helper()
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + tool + `","arguments":{}}}`
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, ts.URL+"/mcp", strings.NewReader(body))
	if err != nil {
		t.Fatalf("构造 %s 请求失败: %v", tool, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer docs-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s 调用失败: %v", tool, err)
	}
	defer resp.Body.Close()
	var out struct {
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("解析 %s 响应失败: %v", tool, err)
	}
	if out.Result.IsError || len(out.Result.Content) == 0 {
		t.Fatalf("%s 调用返回错误: %s", tool, out.Result.Content)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out.Result.Content[0].Text), &m); err != nil {
		t.Fatalf("%s 的业务 JSON 解析失败: %v", tool, err)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

func atoi(s string) int {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			continue
		}
		n = n*10 + int(r-'0')
	}
	return n
}
