// cmd/config 应用配置管理工具
//
// 配置源已从 config/*.yaml 迁到 SQLite 的 config_kv 单行 JSON 文档, 本工具负责:
//
//	import  一次性把旧 YAML 的现值搬进库 (只搬显式配置项, 不固化默认值)
//	dump    打印合并默认值后的生效配置 (凭据默认掩码)
//	get/set 读改单个配置项
//	path    打印解析出的数据库路径
package main

import (
	"flag"
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"

	"github.com/jmoiron/sqlx"

	"jingzhe-trader/internal/appcfg"
	"jingzhe-trader/internal/config"
	"jingzhe-trader/internal/store"
)

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		usage()
		os.Exit(2)
	}

	var err error
	switch args[0] {
	case "import":
		err = runImport(args[1:])
	case "dump":
		err = runDump(args[1:])
	case "get":
		err = runGet(args[1:])
	case "set":
		err = runSet(args[1:])
	case "path":
		err = runPath(args[1:])
	case "-h", "--help", "help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "未知子命令: %s\n\n", args[0])
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "配置操作失败: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `用法: config <子命令> [参数]

  import -f <旧配置文件> [-force]   把 YAML 现值搬进数据库 (已存在需 -force)
  dump   [-show-secrets]            打印生效配置 (默认值已合并, 凭据掩码)
  get    <key> [-show-secrets]      打印单个配置项, key 为点路径 (如 screener.max_pe)
  set    <key> <value>              修改单个配置项 (值按 JSON 解析, 支持数字/布尔/数组)
  path                              打印解析出的数据库路径

通用参数: -db <路径>    未指定时依次取 %s 环境变量、%s
`, appcfg.EnvDBPath, config.DefaultDBPath())
}

// openDBWith 解析通用参数并按统一优先级打开数据库, 返回句柄、实际路径与释放函数
func openDBWith(fs *flag.FlagSet, args []string) (*sqlx.DB, string, func(), error) {
	dbPath := fs.String("db", "", "数据库路径")
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return nil, "", nil, err
	}
	path := appcfg.ResolveDBPath(*dbPath)
	db, err := appcfg.Open(path)
	if err != nil {
		return nil, "", nil, err
	}
	return db, path, func() { _ = db.Close() }, nil
}

// reorderFlags 把散落在位置参数之后的开关提到前面。
// flag 包遇到第一个非开关参数即停止解析, 而 "config get <key> -show-secrets" 是自然会写的顺序。
func reorderFlags(fs *flag.FlagSet, args []string) []string {
	var flagArgs, posArgs []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		// 负数取值 (如 stop_loss_pct 的 -0.005) 形状像开关, 必须先按取值处理
		if _, isNum := strconv.ParseFloat(arg, 64); isNum == nil {
			posArgs = append(posArgs, arg)
			continue
		}
		if !strings.HasPrefix(arg, "-") {
			posArgs = append(posArgs, arg)
			continue
		}
		flagArgs = append(flagArgs, arg)

		name := strings.TrimLeft(arg, "-")
		if strings.Contains(name, "=") {
			continue // -k=v 自带值
		}
		f := fs.Lookup(name)
		if f == nil {
			continue // 未注册的开关, 交给 flag 包报错, 不猜它的值
		}
		if flagTakesValue(f) && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
			i++
			flagArgs = append(flagArgs, args[i])
		}
	}
	return append(flagArgs, posArgs...)
}

// flagTakesValue 布尔开关不消耗下一个 token 作值, 其余开关都消耗
func flagTakesValue(f *flag.Flag) bool {
	return reflect.ValueOf(f.Value).Elem().Kind() != reflect.Bool
}

func runImport(args []string) error {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	file := fs.String("f", "", "待搬运的旧配置文件路径")
	force := fs.Bool("force", false, "库内已有配置时仍覆盖")
	db, path, closeDB, err := openDBWith(fs, args)
	if err != nil {
		return err
	}
	defer closeDB()

	if *file == "" {
		return fmt.Errorf("必须用 -f 指定待搬运的配置文件")
	}
	repo := store.NewConfigRepo(db)
	if _, found, err := repo.Get(); err != nil {
		return err
	} else if found && !*force {
		return fmt.Errorf("库内已有配置文档, 直接覆盖会丢掉搬运后的改动; 确认无误再加 -force")
	}

	doc, err := config.RawFileJSON(*file)
	if err != nil {
		return err
	}
	if err := repo.Put(doc); err != nil {
		return err
	}
	fmt.Printf("已搬运 %s → %s\n下一步: config dump 与旧文件逐段比对, 确认无丢项后再删 YAML\n", *file, path)
	return nil
}

func runDump(args []string) error {
	fs := flag.NewFlagSet("dump", flag.ContinueOnError)
	showSecrets := fs.Bool("show-secrets", false, "输出凭据明文 (默认掩码)")
	db, _, closeDB, err := openDBWith(fs, args)
	if err != nil {
		return err
	}
	defer closeDB()

	doc, _, err := store.NewConfigRepo(db).Get()
	if err != nil {
		return err
	}
	if *showSecrets {
		fmt.Println(string(doc))
		return nil
	}
	out, err := config.EffectiveJSON(doc)
	if err != nil {
		return err
	}
	fmt.Println(string(out))
	return nil
}

func runGet(args []string) error {
	fs := flag.NewFlagSet("get", flag.ContinueOnError)
	showSecrets := fs.Bool("show-secrets", false, "输出凭据明文 (默认掩码)")
	db, _, closeDB, err := openDBWith(fs, args)
	if err != nil {
		return err
	}
	defer closeDB()

	if fs.NArg() != 1 {
		return fmt.Errorf("用法: config get <key>")
	}
	key := fs.Arg(0)
	doc, _, err := store.NewConfigRepo(db).Get()
	if err != nil {
		return err
	}
	if config.IsSecretPath(key) && !*showSecrets {
		fmt.Printf("*** (%s 属于凭据, 需要明文再加 -show-secrets)\n", key)
		return nil
	}
	// 默认走脱敏读取: 父路径取值会连带返回凭据子项 (get mail 含 password), 只判断叶子路径挡不住
	val, err := config.DocGetMasked(doc, key)
	if *showSecrets {
		val, err = config.DocGet(doc, key)
	}
	if err != nil {
		return err
	}
	fmt.Printf("%v\n", val)
	return nil
}

func runSet(args []string) error {
	fs := flag.NewFlagSet("set", flag.ContinueOnError)
	db, _, closeDB, err := openDBWith(fs, args)
	if err != nil {
		return err
	}
	defer closeDB()

	if fs.NArg() != 2 {
		return fmt.Errorf("用法: config set <key> <value>")
	}
	key, value := fs.Arg(0), fs.Arg(1)

	repo := store.NewConfigRepo(db)
	doc, _, err := repo.Get()
	if err != nil {
		return err
	}
	next, err := config.DocSet(doc, key, value)
	if err != nil {
		return err
	}
	if err := repo.Put(next); err != nil {
		return err
	}

	// 回读生效值, 让操作者立刻看到写入是否真的生效
	got, err := config.DocGet(next, key)
	if err != nil {
		return err
	}
	if config.IsSecretPath(key) {
		fmt.Printf("%s 已更新 (凭据不回显)\n", key)
		return nil
	}
	fmt.Printf("%s = %v\n", key, got)
	return nil
}

func runPath(args []string) error {
	fs := flag.NewFlagSet("path", flag.ContinueOnError)
	dbPath := fs.String("db", "", "数据库路径")
	if err := fs.Parse(args); err != nil {
		return err
	}
	fmt.Println(appcfg.ResolveDBPath(*dbPath))
	return nil
}
