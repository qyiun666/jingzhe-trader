// jingzhe db 子命令：库结构维护（清点遗留对象、重建 daily_bar）。
//
// 与 serve 共用同一个 store.Open：清点永远在"建表之后"跑，看到的才是真实差异。
//
// 用法:
//
//	jingzhe db audit          库结构与现役 schema 的差异清点（只读）
//	jingzhe db backup [-to PATH]  给现役库拍一致性快照并校验副本（部署前用）
//	jingzhe db rebuild-bar    把 daily_bar 重建为 WITHOUT ROWID（离线执行）
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"jingzhe-trader/internal/store"
)

// runDB 处理 `jingzhe db <audit|backup|rebuild-bar>`。
func runDB(ctx context.Context, st *store.Store, args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "用法: jingzhe db <audit|backup|rebuild-bar>")
		os.Exit(2)
	}
	fs := flag.NewFlagSet("db", flag.ExitOnError)
	backupTo := fs.String("to", "", "db backup 的快照目标路径；留空则落 <库所在目录>/backup/jingzhe.pre-deploy-<时间戳>.db")
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

	case "backup":
		// 默认目标由库路径运行时推导（命令行里不出现库目录外的绝对路径），
		// 这样 IM 绑定的运维会话也能守住"部署前先有快照"这一步。
		res, err := st.BackupSnapshot(ctx, strings.TrimSpace(*backupTo))
		if err != nil {
			fmt.Fprintf(os.Stderr, "库快照失败: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("库快照完成: %s\n  体积=%d 字节 integrity=%s 用户表数=%d\n",
			res.Path, res.Bytes, res.Integrity, res.Tables)
		fmt.Printf("  回退方式: 停服 → 用该副本替换 %s（替换前先把它另存一份）→ 起服 → db audit + get_brief 核对\n", st.Path())

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
		fmt.Fprintf(os.Stderr, "未知 db 子命令: %s（可选 audit|backup|rebuild-bar）\n", args[0])
		os.Exit(2)
	}
}
