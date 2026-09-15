// jingzhe db 子命令：库结构维护（清点遗留对象、重建 daily_bar）。
//
// 与 serve 共用同一个 store.Open：清点永远在"建表之后"跑，看到的才是真实差异。
//
// 用法:
//
//	jingzhe -db data/jingzhe.db db audit          库结构与现役 schema 的差异清点（只读）
//	jingzhe -db data/jingzhe.db db rebuild-bar    把 daily_bar 重建为 WITHOUT ROWID（离线执行）
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"jingzhe-trader/internal/store"
)

// runDB 处理 `jingzhe db <audit|rebuild-bar>`。
func runDB(ctx context.Context, st *store.Store, args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "用法: jingzhe db <audit|rebuild-bar>")
		os.Exit(2)
	}
	fs := flag.NewFlagSet("db", flag.ExitOnError)
	if err := fs.Parse(args[1:]); err != nil {
		os.Exit(2)
	}

	switch args[0] {
	case "audit":
		a, err := st.AuditSchema(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "库结构清点失败: %v\n", err)
			os.Exit(1)
		}
		if a.Clean() {
			fmt.Println("库结构与现役 schema 一致")
			return
		}
		fmt.Print(a.String())
		os.Exit(3) // 有差异：非零退出便于脚本/agent 判读

	case "rebuild-bar":
		moved, err := st.RebuildDailyBar(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "重建 daily_bar 失败: %v\n", err)
			os.Exit(1)
		}
		if moved == 0 {
			fmt.Println("daily_bar 已是 WITHOUT ROWID，无需重建")
			return
		}
		fmt.Printf("daily_bar 已重建为 WITHOUT ROWID，搬迁 %d 行（请核对库体积已下降）\n", moved)

	default:
		fmt.Fprintf(os.Stderr, "未知 db 子命令: %s（可选 audit|rebuild-bar）\n", args[0])
		os.Exit(2)
	}
}
