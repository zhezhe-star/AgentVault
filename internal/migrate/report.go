package migrate

import (
	"fmt"
	"io"
)

// WriteReport renders the three-class migration status summary. Findings
// carry masks only, so the report cannot leak plaintext by construction.
func WriteReport(w io.Writer, found []Finding, importedRefs []string) {
	byClass := map[Class][]Finding{}
	for _, f := range found {
		byClass[f.Class] = append(byClass[f.Class], f)
	}
	fmt.Fprintln(w, "=== AgentVault 迁移报告 ===")
	fmt.Fprintf(w, "扫描命中 %d 处明文落盘点\n\n", len(found))

	classes := []Class{ClassProxyAttach, ClassRunInject, ClassCompliant}
	for _, c := range classes {
		fs := byClass[c]
		fmt.Fprintf(w, "[%s] %d 处\n", c, len(fs))
		for _, f := range fs {
			fmt.Fprintf(w, "  %s:%d %s (%s)\n", f.Path, f.Line, f.Var, f.MaskedValue)
		}
		fmt.Fprintln(w)
	}

	if len(importedRefs) > 0 {
		fmt.Fprintf(w, "已入钥匙链 ref：\n")
		for _, r := range importedRefs {
			fmt.Fprintf(w, "  ref:%s\n", r)
		}
		fmt.Fprintln(w)
	}

	// run-inject 类指引：一工具一专用 key 的隔离策略。
	fmt.Fprintln(w, "run-inject 类迁移指引（一工具一专用 key）：")
	fmt.Fprintln(w, "  1. 为每个工具签发独立 key（服务商控制台），最小权限、可单独吊销：")
	fmt.Fprintln(w, "     printf <new-key> 管道入 | agentvault add-key <provider> --key-id <tool-name>")
	fmt.Fprintln(w, "  2. 用 agentvault run 包装启动命令（配置文件只留 ref，不留明文）：")
	fmt.Fprintf(w, "     agentvault run --env ARK_API_KEY=ref:ark/ark_api_key -- <mcp-server 命令>\n")
	fmt.Fprintln(w, "  3. codex config.toml 的 [mcp_servers.*.env] 明文行经 import --yes 改写为占位符后，")
	fmt.Fprintln(w, "     由 wrapper 注入真实值；示例：")
	fmt.Fprintln(w, `     # ~/.codex/config.toml`)
	fmt.Fprintln(w, `     # [mcp_servers.ark.env]`)
	fmt.Fprintln(w, `     # ARK_API_KEY = "agentvault-local-placeholder"  # 真值由 agentvault run 注入`)
	fmt.Fprintln(w, "  4. 吊销演练：禁用旧 key（agentvault provider …）→ 服务商控制台吊销 → 换新 key 重录同 ref。")
}
